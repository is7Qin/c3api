// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package httpx

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 本文件是 spec-upstream-h2-sharding §7 门禁 (a)~(f)、(h)~(j) 的分片侧实现。
// 每条断言都对应一个"改哪行会红"，负例写在函数注释里。
//
// 两条纪律（评审要求）：
//  1. 预热与测量分开记录——否则冷启动拨号竞态（E9）会替被测实现多开连接，
//     让"恒取一个分片"的坏实现也假绿。
//  2. 任何请求失败都必须让门禁红——失败的请求返回得快，墙钟会"变漂亮"。

// connRecorder 按 (path, remote addr) 去重记录连接。不包装 listener：包装后
// httptest 在 Close 时会 panic。
type connRecorder struct {
	mu     sync.Mutex
	byPath map[string]map[string]struct{}
}

func (c *connRecorder) observe(path, addr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byPath == nil {
		c.byPath = make(map[string]map[string]struct{})
	}
	if c.byPath[path] == nil {
		c.byPath[path] = make(map[string]struct{})
	}
	c.byPath[path][addr] = struct{}{}
}

func (c *connRecorder) count(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.byPath[path])
}

// newH2Server 起一个 h2 TLS 测试服务端：按路径记录连接，并把 remote addr 回写到
// X-Remote 头（供"这次请求走了哪条连接"的白盒断言）。
func newH2Server(t *testing.T, h http.HandlerFunc) (*httptest.Server, *connRecorder) {
	t.Helper()
	rec := &connRecorder{}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.observe(r.URL.Path, r.RemoteAddr)
		w.Header().Set("X-Remote", r.RemoteAddr)
		h(w, r)
	}))
	ts.Config.HTTP2 = &http.HTTP2Config{MaxConcurrentStreams: 100} // M=100：不会自动增连
	ts.EnableHTTP2 = true
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts, rec
}

// h2Base 是与生产同形态的底座：ForceAttemptHTTP2 + 自定义 TLSClientConfig。
func h2Base(t *testing.T, ts *httptest.Server) *http.Transport {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	return &http.Transport{
		ForceAttemptHTTP2:   true,
		TLSClientConfig:     &tls.Config{RootCAs: pool},
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     time.Minute,
	}
}

// doPost 发一个 POST 并读完 body；读失败或 Close 失败都算失败（不能把截断当成功）。
func doPost(ctx context.Context, rt http.RoundTripper, target string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		_ = resp.Body.Close()
		return err
	}
	return resp.Body.Close()
}

// postProto 发一次 POST 并返回协商到的协议主版本（2 = h2）。
func postProto(ctx context.Context, rt http.RoundTripper, target string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader([]byte("hi")))
	if err != nil {
		return 0, err
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if err := resp.Body.Close(); err != nil {
		return 0, err
	}
	return resp.ProtoMajor, nil
}

func sumInflight(st *ShardedTransport) int64 {
	var sum int64
	for i := range st.shards {
		sum += st.shards[i].inflight.Load()
	}
	return sum
}

// warmEachShard 逐片直接预热（每片一条连接），不经过 pick ⇒ 不会只热到 0 号分片，
// 也不会污染对 pick 的观测。
func warmEachShard(t *testing.T, st *ShardedTransport, target string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for i := range st.shards {
		require.NoErrorf(t, doPost(ctx, st.shards[i].tr, target, []byte("hi")), "预热分片 %d", i)
	}
}

// (a) 真并发铺满：N 个**同时在飞**的请求必须落在 N 条独立连接上。
// 服务端 handler 卡住直到 N 个请求全部到齐，保证它们真正重叠。
// 负例：选择规则改成恒取 0 号分片 ⇒ 只有 1 条连接，红。
func TestShardSpreadsTrulyConcurrentRequests(t *testing.T) {
	const n = 4
	release := make(chan struct{})
	arrived := make(chan struct{}, n)
	ts, rec := newH2Server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/burst" {
			return
		}
		arrived <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	})
	st := NewShardedTransport(h2Base(t, ts), n)
	require.NotNil(t, st)
	warmEachShard(t, st, ts.URL+"/warm") // 预热记录在 /warm 下，不干扰 /burst 计数

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := doPost(ctx, st, ts.URL+"/burst", []byte("hi")); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	for range n {
		select {
		case <-arrived:
		case <-ctx.Done():
			close(release)
			t.Fatalf("只有 %d/%d 个请求到齐（分片没把并发摊开）", len(arrived), n)
		}
	}
	close(release)
	wg.Wait()
	require.Empty(t, errs)
	require.GreaterOrEqual(t, rec.count("/burst"), n, "%d 个真并发请求必须落在 %d 条独立连接上", n, n)
}

// (a) 对照：单 transport 下同样 4 个真并发请求必须复用**一条**连接（h2 M=100）。
// 串行预热保证起点只有 1 条；负例：换成 4 个独立 transport ⇒ 4 条，红。
func TestSingleTransportReusesOneConnection(t *testing.T) {
	const n = 4
	release := make(chan struct{})
	arrived := make(chan struct{}, n)
	ts, rec := newH2Server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/burst" {
			return
		}
		arrived <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	})
	base := h2Base(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	require.NoError(t, doPost(ctx, base, ts.URL+"/warm", []byte("hi"))) // 串行钉住 1 条

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := doPost(ctx, base, ts.URL+"/burst", []byte("hi")); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	for range n {
		select {
		case <-arrived:
		case <-ctx.Done():
			close(release)
			t.Fatalf("只有 %d/%d 个请求到齐", len(arrived), n)
		}
	}
	close(release)
	wg.Wait()
	require.Empty(t, errs)
	require.Equal(t, 1, rec.count("/burst"), "单 transport 下 %d 个真并发请求必须复用同一条连接", n)
}

// (b) 绕开在飞最多的分片：0 号被一条长流占住（body 未关）时，下一个请求必须换连接。
// 负例：pick 恒取 0 号 ⇒ 两次请求同一条连接，红。
func TestShardAvoidsBusyShard(t *testing.T) {
	hold := make(chan struct{})
	release := sync.OnceFunc(func() { close(hold) })
	ts, _ := newH2Server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			w.WriteHeader(http.StatusOK)
			_ = http.NewResponseController(w).Flush() // 先送出 header，再卡住 body
			<-hold
		}
	})
	// 清理顺序：服务器 Close 先注册 ⇒ 后执行；release 必须最后注册才会**最先**执行，
	// 否则关服务器时 handler 还卡在 hold 上。
	t.Cleanup(release)
	st := NewShardedTransport(h2Base(t, ts), 4)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/hold", nil)
	require.NoError(t, err)
	held, err := st.RoundTrip(req)
	require.NoError(t, err)
	addrHeld := held.Header.Get("X-Remote")
	require.Equal(t, int64(1), sumInflight(st), "长流必须被计为在飞（header 回来不算完）")

	addrNext, err := remoteOf(ctx, st, ts.URL+"/short")
	require.NoError(t, err)
	require.NotEqual(t, addrHeld, addrNext, "新请求必须绕开在飞的分片")

	release()
	require.NoError(t, held.Body.Close())
	require.Equal(t, int64(0), sumInflight(st), "关闭 body 后计数必须归还")
}

// (b) 精确落点：在飞分布不均时必须落在 argmin 的下标；平局取最小下标。
// 负例：改成最大下标/随机/轮询 ⇒ 命中位不等于期望，红。
func TestPickLandsOnLeastInflightExact(t *testing.T) {
	st := NewShardedTransport(&http.Transport{}, 4)
	st.shards[0].inflight.Store(3)
	st.shards[1].inflight.Store(1)
	st.shards[2].inflight.Store(2)
	st.shards[3].inflight.Store(4)
	require.Equal(t, 1, st.pick(), "必须落在在飞最少的分片")
	require.Equal(t, int64(2), st.shards[1].inflight.Load(), "选中后必须预留 +1")

	st.shards[0].inflight.Store(0)
	st.shards[1].inflight.Store(0)
	st.shards[2].inflight.Store(0)
	st.shards[3].inflight.Store(2)
	require.Equal(t, 0, st.pick(), "平局必须取最小下标")
	require.Equal(t, 1, st.pick(), "0 号被预留后顺延到 1 号")
}

func remoteOf(ctx context.Context, rt http.RoundTripper, target string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		_ = resp.Body.Close()
		return "", err
	}
	if err := resp.Body.Close(); err != nil {
		return "", err
	}
	return resp.Header.Get("X-Remote"), nil
}

// (c) 串行不铺满：8 分片、串行 6 个请求 ⇒ 只建 1 条连接。
// 负例：tie-break 改轮转 ⇒ 6 条连接，红。
func TestShardSerialRequestsReuseOneConnection(t *testing.T) {
	const n = 8
	ts, rec := newH2Server(t, func(w http.ResponseWriter, r *http.Request) {})
	st := NewShardedTransport(h2Base(t, ts), n)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for range 6 {
		require.NoError(t, doPost(ctx, st, ts.URL+"/x", nil))
	}
	require.Equal(t, 1, rec.count("/x"), "串行 6 个请求只应建立 1 条连接")
	require.Equal(t, int64(0), sumInflight(st))
}

// (d) 配额守恒：三个连接配额字段都要分摊，Σ 必须**严格**等于配置值。
// 负例：shareConns 原样返回 ⇒ Σ=512，红；余数不摊 ⇒ 100/8 得 96，红。
func TestShareConnsKeepsTotalBudget(t *testing.T) {
	for _, c := range []struct {
		total, n, i, want int
	}{
		{64, 8, 0, 8},
		{64, 8, 7, 8},
		{8192, 8, 3, 1024},
		{100, 8, 0, 13}, // 余数 4：前 4 片多 1
		{100, 8, 3, 13},
		{100, 8, 4, 12},
		{100, 8, 7, 12},
		{16, 8, 0, 2},
		{4, 8, 0, 2}, // total<2n：每片 2（下限优先）
		{7, 8, 6, 2},
		{0, 8, 0, 0}, // 零值原样保留（语义按字段不同）
	} {
		require.Equal(t, c.want, shareConns(c.total, c.n, c.i),
			"shareConns(%d,%d,%d)", c.total, c.n, c.i)
	}

	// total=0（"不限"/Go 默认）也要断言字段值全 0：这一档没有守恒可言，但"被摊成非零"
	// 或"被摊成 0 后当成禁用"都是错的方向，所以字段值必须原样是 0。
	for _, total := range []int{0, 64, 100, 8192} {
		base := &http.Transport{MaxIdleConns: total, MaxIdleConnsPerHost: total, MaxConnsPerHost: total}
		st := NewShardedTransport(base, 8)
		var sumIdle, sumPerHost, sumMax int
		for i := range st.shards {
			sumIdle += st.shards[i].tr.MaxIdleConns
			sumPerHost += st.shards[i].tr.MaxIdleConnsPerHost
			sumMax += st.shards[i].tr.MaxConnsPerHost
		}
		require.Equal(t, total, sumIdle, "Σ max_idle_conns(%d)", total)
		require.Equal(t, total, sumPerHost, "Σ max_idle_conns_per_host(%d)", total)
		require.Equal(t, total, sumMax, "Σ max_conns_per_host(%d)：上限被放大 = 每 host 连接数 ×N", total)
		require.Equal(t, total, base.MaxIdleConns, "底座参数不得被分摊污染")
		require.Equal(t, total, base.MaxIdleConnsPerHost, "底座参数不得被分摊污染")
		require.Equal(t, total, base.MaxConnsPerHost, "底座参数不得被分摊污染")
	}
}

// (e) 计数归还：正常关闭 / 双 Close / RoundTrip 报错 / 读 body 出错后 Close
// 四条路径都必须回到 0 且永不为负。
// 负例：删掉 countedBody 的 once ⇒ 双 Close 后 -1，红；删掉错误路径归还 ⇒ 红。
func TestCountedBodyReturnsExactlyOnce(t *testing.T) {
	ts, _ := newH2Server(t, func(w http.ResponseWriter, r *http.Request) {})
	st := NewShardedTransport(h2Base(t, ts), 4)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/x", nil)
	require.NoError(t, err)
	resp, err := st.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, int64(0), sumInflight(st))
	require.NoError(t, resp.Body.Close(), "重复 Close 不得报错")
	require.Equal(t, int64(0), sumInflight(st), "重复 Close 不得把计数减成负数")

	// RoundTrip 报错路径：拨号直接失败
	broken := h2Base(t, ts)
	broken.DialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("boom")
	}
	st2 := NewShardedTransport(broken, 2)
	req2, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://upstream.invalid/x", nil)
	require.NoError(t, err)
	_, err = st2.RoundTrip(req2)
	require.Error(t, err)
	require.Equal(t, int64(0), sumInflight(st2), "报错路径必须归还在飞计数")

	// 读 body 出错后 Close：服务端声明 100 字节只给 10 字节就断连。
	truncated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, brw, herr := http.NewResponseController(w).Hijack()
		if herr != nil {
			return
		}
		_, _ = brw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n0123456789")
		_ = brw.Flush()
		_ = conn.Close()
	}))
	defer truncated.Close()
	st3 := NewShardedTransport(&http.Transport{MaxIdleConnsPerHost: 2}, 2)
	req3, err := http.NewRequestWithContext(ctx, http.MethodGet, truncated.URL, nil)
	require.NoError(t, err)
	resp3, err := st3.RoundTrip(req3)
	require.NoError(t, err)
	_, err = io.ReadAll(resp3.Body)
	require.Error(t, err, "截断的响应必须读出错误")
	_ = resp3.Body.Close()
	_ = resp3.Body.Close()
	require.Equal(t, int64(0), sumInflight(st3), "读出错后 Close 也必须归还且只归还一次")
}

// (f) 决策矩阵：h2 + N>1 ⇒ 分片；N<=1（含 0/负数）⇒ 单 transport 且逐值字段相等；
// 关 h2 ⇒ 单 transport 且 ForceAttemptHTTP2=false。
// 负例：无条件分片 ⇒ 关 h2 时仍是 ShardedTransport，红。
func TestNewClientShardDecision(t *testing.T) {
	on := TransportConfig{
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 16,
		MaxConnsPerHost:     4,
		IdleConnTimeout:     30 * time.Second,
		DialTimeout:         time.Second,
		ForceHTTP2:          true,
		HTTP2Shards:         8,
	}
	c := NewClient(on)
	st, ok := c.Transport.(*ShardedTransport)
	require.True(t, ok, "h2 + shards=8 必须是分片")
	require.Equal(t, 8, st.ShardCount())

	want := NewTransport(on)
	for _, shards := range []int{1, 0, -1} {
		cfg := on
		cfg.HTTP2Shards = shards
		tr, ok := NewClient(cfg).Transport.(*http.Transport)
		require.Truef(t, ok, "shards=%d 必须是单 transport", shards)
		require.NotNil(t, tr)
		require.Equal(t, want.MaxIdleConns, tr.MaxIdleConns)
		require.Equal(t, want.MaxIdleConnsPerHost, tr.MaxIdleConnsPerHost)
		require.Equal(t, want.MaxConnsPerHost, tr.MaxConnsPerHost)
		require.Equal(t, want.IdleConnTimeout, tr.IdleConnTimeout)
		require.Equal(t, want.TLSHandshakeTimeout, tr.TLSHandshakeTimeout)
		require.Equal(t, want.ExpectContinueTimeout, tr.ExpectContinueTimeout)
		require.Equal(t, want.ForceAttemptHTTP2, tr.ForceAttemptHTTP2)
		require.Nil(t, tr.Proxy, "Proxy 仍需是显式直连")
	}

	off := on
	off.ForceHTTP2 = false
	trOff, ok := NewClient(off).Transport.(*http.Transport)
	require.True(t, ok, "关 h2 时即便 shards>1 也不得分片")
	require.False(t, trOff.ForceAttemptHTTP2)
}

// (g-part1) I4 连接池不复用：底座、原始分片、两个不同代理的克隆分片，
// 所有 *http.Transport 必须非 nil 且两两不等；Proxy 逐片生效且底座不被污染。
// 负例：Clone 里直接返回 s（共享连接池）⇒ 指针集合出现重复，红；漏覆盖 Proxy ⇒ 红。
func TestShardCloneKeepsPoolsIndependent(t *testing.T) {
	base := &http.Transport{ForceAttemptHTTP2: true}
	st := NewShardedTransport(base, 3)
	proxyA, err := url.Parse("http://127.0.0.1:3128")
	require.NoError(t, err)
	proxyB, err := url.Parse("socks5://127.0.0.1:1080")
	require.NoError(t, err)
	clA, ok := st.CloneWithProxy(func(*http.Request) (*url.URL, error) { return proxyA, nil }).(*ShardedTransport)
	require.True(t, ok, "CloneWithProxy 必须返回分片副本")
	clB, ok := st.CloneWithProxy(func(*http.Request) (*url.URL, error) { return proxyB, nil }).(*ShardedTransport)
	require.True(t, ok, "CloneWithProxy 必须返回分片副本")
	require.Equal(t, 3, clA.ShardCount())
	require.Equal(t, 3, clB.ShardCount())

	seen := make(map[*http.Transport]string)
	add := func(tr *http.Transport, what string) {
		require.NotNil(t, tr, what)
		prev, dup := seen[tr]
		require.Falsef(t, dup, "%s 与 %s 共享同一个 *http.Transport（连接池必须独立）", what, prev)
		seen[tr] = what
	}
	add(base, "base") // 底座本身也必须与所有分片不同（否则分片复用了底座连接池）
	for i := range st.shards {
		add(st.shards[i].tr, "orig")
		add(clA.shards[i].tr, "cloneA")
		add(clB.shards[i].tr, "cloneB")
	}
	require.Len(t, seen, 10, "底座 + 3 片 × 3 个实例必须是 10 个独立连接池")

	for _, cl := range []struct {
		st  *ShardedTransport
		url string
	}{{clA, proxyA.String()}, {clB, proxyB.String()}} {
		for i := range cl.st.shards {
			require.NotNil(t, cl.st.shards[i].tr.Proxy, "每片都必须装配代理")
			req, err := http.NewRequest(http.MethodGet, "http://up.example/", nil)
			require.NoError(t, err)
			got, err := cl.st.shards[i].tr.Proxy(req)
			require.NoError(t, err)
			require.Equal(t, cl.url, got.String())
		}
	}
	require.Nil(t, base.Proxy, "底座不得被克隆污染")
	for i := range st.shards {
		require.Nilf(t, st.shards[i].tr.Proxy, "原始分片 %d 不得被克隆污染", i)
	}
}

// (h) 价值门禁：注入 RTT 后，N 分片的并发上传聚合吞吐必须显著高于单连接。
//
// 为什么必须用延迟代理（而不是"慢读服务端"）：慢读让**流窗**成为约束，而流窗与连接窗
// 同量级时分片分摊不到任何东西——实测慢读版单连接臂只 102ms、比值≈1，门禁失效。
// 真正的约束是「连接窗 ÷ RTT」：8 条独立连接各有一份窗口。
//
// 两臂都必须**先锁定连接条件**再量：单连接臂串行预热 1 条、分片臂逐片预热 8 条；
// 服务端必须完整收到每个测量请求的 64KiB（截断上传会让"吞吐"凭空变高）。
// 负例：全落同一分片（或分片退化成单 transport）⇒ 比值≈1，红。
func TestShardRaisesUploadThroughputUnderRTT(t *testing.T) {
	const (
		shards    = 8
		bodyBytes = 64 << 10
	)
	sizes := make(chan int64, shards*2)
	var upMu sync.Mutex
	upRemotes := make(map[string]struct{})
	resetRemotes := func() {
		upMu.Lock()
		defer upMu.Unlock()
		upRemotes = make(map[string]struct{})
	}
	remotesSeen := func() int {
		upMu.Lock()
		defer upMu.Unlock()
		return len(upRemotes)
	}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/up" {
			upMu.Lock()
			upRemotes[r.RemoteAddr] = struct{}{}
			upMu.Unlock()
			sizes <- n
		}
		w.WriteHeader(http.StatusOK)
	}))
	ts.Config.HTTP2 = &http.HTTP2Config{
		MaxConcurrentStreams:          100,
		MaxReceiveBufferPerConnection: 64 << 10,
		MaxReceiveBufferPerStream:     64 << 10,
	}
	ts.EnableHTTP2 = true
	ts.StartTLS()
	t.Cleanup(ts.Close)

	// 预热、协议探测、测量**必须同走延迟代理**：连接池按 (scheme, host) 分键，
	// 直连服务器预热出来的连接在测量目标（代理端口）上一条都用不上——那样两臂都在
	// 冷启动，等于把 E9 的拨号竞态重新引进门禁（这正是本门禁要排除的变量）。
	proxyOrigin := "https://" + startDelayProxy(t, ts.Listener.Addr().String(), 20*time.Millisecond)
	target := proxyOrigin + "/up"
	warmTarget := proxyOrigin + "/warm"

	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	newBase := func() *http.Transport {
		return &http.Transport{
			ForceAttemptHTTP2:   true,
			TLSClientConfig:     &tls.Config{RootCAs: pool},
			MaxIdleConnsPerHost: 64,
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	measure := func(rt http.RoundTripper) (time.Duration, []error) {
		var wg sync.WaitGroup
		var mu sync.Mutex
		var errs []error
		start := time.Now()
		for range shards {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := doPost(ctx, rt, target, make([]byte, bodyBytes)); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		return time.Since(start), errs
	}

	// 服务端必须完整收到**每臂**的每个测量请求 body（截断上传会让"吞吐"凭空变高）。
	drainSizes := func() {
		require.Len(t, sizes, shards, "测量请求必须全部到达服务端")
		for range shards {
			require.Equal(t, int64(bodyBytes), <-sizes, "服务端必须完整收到 64KiB")
		}
	}

	// 单连接臂：串行预热正好一条连接（并发预热会因 E9 拨出多条）。
	single := newBase()
	require.NoError(t, doPost(ctx, single, warmTarget, []byte("hi")))
	major, err := postProto(ctx, single, warmTarget)
	require.NoError(t, err)
	require.Equal(t, 2, major, "单连接臂必须是 h2")
	resetRemotes()
	singleTime, errs := measure(single)
	require.Empty(t, errs, "单连接臂有失败请求：墙钟不可信")
	drainSizes()
	require.Equal(t, 1, remotesSeen(), "单连接臂的 8 次测量必须复用**同一条**连接")

	// 分片臂：逐片预热，确保 8 条连接都在。
	st := NewShardedTransport(newBase(), shards)
	require.NotNil(t, st)
	warmEachShard(t, st, warmTarget)
	major, err = postProto(ctx, st, warmTarget)
	require.NoError(t, err)
	require.Equal(t, 2, major, "分片臂必须是 h2")
	resetRemotes()
	shardedTime, errs := measure(st)
	require.Empty(t, errs, "分片臂有失败请求：墙钟不可信")
	drainSizes()
	require.Equal(t, shards, remotesSeen(), "分片臂的 8 次测量必须落在 8 条独立连接上")

	t.Logf("单连接 %v vs %d 分片 %v", singleTime, shards, shardedTime)
	// 前置：延迟代理与小窗必须真的生效（8×64KB 串行窗口至少 8×40ms），否则门禁等于没测。
	require.GreaterOrEqual(t, singleTime, 150*time.Millisecond,
		"延迟代理/小窗没生效（单连接 %v），门禁无效", singleTime)
	require.LessOrEqual(t, shardedTime*2, singleTime,
		"分片没带来上行吞吐提升：单连接 %v vs %d 分片 %v", singleTime, shards, shardedTime)
}

// startDelayProxy 在 127.0.0.1 起一个纯 TCP 转发代理，每方向每块延迟 d（近似 RTT=2d）。
func startDelayProxy(t *testing.T, target string, d time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				u, err := net.Dial("tcp", target)
				if err != nil {
					_ = c.Close()
					return
				}
				go delayedPipe(c, u, d)
				go delayedPipe(u, c, d)
			}()
		}
	}()
	return ln.Addr().String()
}

func delayedPipe(dst, src net.Conn, d time.Duration) {
	buf := make([]byte, 64<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			time.Sleep(d)
			if _, werr := dst.Write(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	_ = dst.Close()
	_ = src.Close()
}

// (i) WS 升级门禁：101 响应体必须**保持 io.ReadWriteCloser**（不得被计数包装），
// 升级后能真收发字节，且 Close 前后计数都为零。
// 负例：去掉 RoundTrip 的 101 分支 ⇒ 客户端库断言 "response body is not a
// io.ReadWriteCloser" ⇒ codex WS 全线握手失败，本用例红。
func TestShardLeavesUpgradeBodyUnwrapped(t *testing.T) {
	const shards = 8
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = brw.Flush()
		buf := make([]byte, 4)
		if _, err := io.ReadFull(brw, buf); err == nil {
			_, _ = brw.Write(buf) // echo
			_ = brw.Flush()
		}
	}))
	defer ts.Close()

	st := NewShardedTransport(&http.Transport{MaxIdleConnsPerHost: 4}, shards)
	require.Equal(t, shards, st.ShardCount())
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/ws", nil)
	require.NoError(t, err)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")

	resp, err := st.RoundTrip(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	rw, ok := resp.Body.(io.ReadWriteCloser)
	require.True(t, ok, "101 的 body 必须是 io.ReadWriteCloser（coder/websocket 硬断言该类型）")
	require.Equal(t, int64(0), sumInflight(st), "101 不占分片在飞计数")

	_, err = rw.Write([]byte("ping"))
	require.NoError(t, err)
	got := make([]byte, 4)
	_, err = io.ReadFull(rw, got)
	require.NoError(t, err)
	require.Equal(t, []byte("ping"), got, "升级后必须能真收发字节")
	require.NoError(t, rw.Close())
	require.Equal(t, int64(0), sumInflight(st), "升级连接关闭后计数仍为零")
}

// (j) 选路零分配：argmin + CAS 不得产生堆分配（countedBody 的那 1 次分配不计入选路）。
func TestPickIsAllocationFree(t *testing.T) {
	st := NewShardedTransport(&http.Transport{}, 8)
	allocs := testing.AllocsPerRun(200, func() {
		i := st.pick()
		st.shards[i].inflight.Add(-1)
	})
	require.Zero(t, allocs, "选路必须零分配（否则每请求多一次 GC 压力）")
}
