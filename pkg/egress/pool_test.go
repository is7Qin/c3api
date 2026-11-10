// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package egress

import (
	"bufio"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/is7qin/c3api/pkg/httpx"
	"github.com/stretchr/testify/require"
)

// splitHostPort 把 httptest 的 `127.0.0.1:port` 拆成规格的三元组。
func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)
	return host, port
}

// TestPoolForReusesSameClient 钉住"按摘要复用同一 *Client 指针"——这是连接池
// 跨快照重建保留的唯一依据：若每次 For 都建新 client，代理连接池随每次 reload
// 重建，持久复用就是空话。
func TestPoolForReusesSameClient(t *testing.T) {
	pool := NewPool(nil)
	spec := New("Socks5", "10.0.0.1", 1080, "u", "p")

	first := pool.For(spec)
	require.NotNil(t, first)
	require.Equal(t, spec, first.Spec)
	// 同一份规格的不同构造（大小写/方括号写法）必须命中同一 client。
	second := pool.For(New("socks5", "[10.0.0.1]", 1080, "u", "p"))
	require.Same(t, first, second, "同键必须复用同一 *Client（含大小写与 IPv6 方括号归一）")
	require.Same(t, first, pool.For(spec))
	require.Equal(t, 1, pool.Size())

	// 摘要的任一维变化 ⇒ 新 client。
	other := pool.For(New("socks5", "10.0.0.1", 1080, "u", "p2"))
	require.NotSame(t, first, other, "改凭据 = 换一条出口线")
	require.Equal(t, 2, pool.Size())
}

// TestPoolDirectSingleton 直连出口是构造期单例：零值 spec 与 For 的零值入参都
// 返回同一个指针（未绑定账号的 Egress 指针同一性由这个单例保证）。
func TestPoolDirectSingleton(t *testing.T) {
	pool := NewPool(nil)
	direct := pool.Direct()
	require.NotNil(t, direct)
	require.NotNil(t, direct.HTTP)
	require.Equal(t, Spec{}, direct.Spec, "直连 = 零值 Spec")
	require.Same(t, direct, pool.Direct())
	require.Same(t, direct, pool.For(Spec{}), "For 零值 spec ⇒ Direct()")
	require.Same(t, http.DefaultClient, direct.HTTP, "base == nil ⇒ http.DefaultClient")
	require.Equal(t, 0, pool.Size(), "直连不占注册表条目")
}

// TestPoolNilBaseDoesNotPanic：base == nil 是测试路径常态，必须可用（不 panic、
// 且仍能按规格建 client）。
func TestPoolNilBaseDoesNotPanic(t *testing.T) {
	pool := NewPool(nil)
	require.NotPanics(t, func() {
		c := pool.For(New("http", "127.0.0.1", 3128, "", ""))
		require.NotNil(t, c)
		require.NotNil(t, c.HTTP)
		require.NotNil(t, c.HTTP.Transport)
	})
}

// TestPoolSpecsGetDistinctTransports：每条规格必须有**自己的** Transport（连接池
// 不可共享），且直连与代理不是同一个 Transport。
func TestPoolSpecsGetDistinctTransports(t *testing.T) {
	// 底座显式直连（与 main 的 httpx.NewClient{Proxy: nil} 同形态），便于断言
	// 「底座没被代理装配污染」。
	base := &http.Client{Transport: &http.Transport{Proxy: nil}}
	pool := NewPool(base)

	a := pool.For(New("http", "127.0.0.1", 3128, "", ""))
	b := pool.For(New("socks5", "127.0.0.1", 1080, "", ""))
	require.NotSame(t, a.HTTP.Transport, b.HTTP.Transport)

	tr, ok := a.HTTP.Transport.(*http.Transport)
	require.True(t, ok)
	require.NotNil(t, tr.Proxy, "代理 client 必须装配 Transport.Proxy")
	req, err := http.NewRequest(http.MethodGet, "http://upstream.example/", nil)
	require.NoError(t, err)
	proxyURL, err := tr.Proxy(req)
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:3128", proxyURL.String())

	// 克隆底座不得污染底座本身（底座的连接池仍归直连用）。
	baseTr, ok := base.Transport.(*http.Transport)
	require.True(t, ok)
	require.Nil(t, baseTr.Proxy, "底座 Transport 不得被代理装配改写")
	// 直连 client 的 Transport 就是底座本身（零改造）。
	require.Same(t, base.Transport, pool.Direct().HTTP.Transport)
}

// TestPoolDetachKeepsActiveAndRemovesRest 钉住 Detach 的取舍与幂等：活跃项保留
// （同一指针），其余摘出并**返回**（之后再 For 必然新建），重复调用不 panic。
func TestPoolDetachKeepsActiveAndRemovesRest(t *testing.T) {
	pool := NewPool(nil)
	keepSpec := New("socks5", "10.0.0.1", 1080, "", "")
	dropSpec := New("http", "10.0.0.2", 3128, "", "")
	keep := pool.For(keepSpec)
	drop := pool.For(dropSpec)
	require.Equal(t, 2, pool.Size())

	require.NotPanics(t, func() { CloseClients(nil) }, "nil 安全")
	CloseClients(pool.Detach(map[[16]byte]struct{}{keepSpec.Key(): {}}))
	require.Equal(t, 1, pool.Size())
	require.Same(t, keep, pool.For(keepSpec), "活跃规格必须保留同一 client")
	require.NotSame(t, drop, pool.For(dropSpec), "被移除的规格必须重建（旧 client 已关闭）")
	require.Equal(t, 2, pool.Size())

	require.NotPanics(t, func() {
		CloseClients(pool.Detach(nil))
		CloseClients(pool.Detach(nil))
	})
	require.Equal(t, 0, pool.Size(), "空 active 集 ⇒ 清空注册表")
}

// TestPoolDetachClosesIdleConnections 证明 Detach 返回的 client 被调用方关闭后
// **连接真的被关**（不只是丢引用）：经代理建立一条 keep-alive 连接后摘除并锁外
// 关闭，代理侧必须观测到连接关闭。
func TestPoolDetachClosesIdleConnections(t *testing.T) {
	var closed atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	proxy.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateClosed || st == http.StateHijacked {
			closed.Add(1)
		}
	}
	defer proxy.Close()
	host, port := splitHostPort(t, proxy.Listener.Addr().String())

	pool := NewPool(nil)
	spec := New("http", host, port, "", "")
	c := pool.For(spec)
	resp, err := c.HTTP.Get("http://upstream.invalid/x")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	detached := pool.Detach(nil)
	require.Len(t, detached, 1, "被摘出的正是刚才那条出口 client")
	CloseClients(detached)
	require.Eventually(t, func() bool { return closed.Load() > 0 }, 2*time.Second, 10*time.Millisecond,
		"Detach + CloseClients 必须关掉空闲连接（代理侧应观测到连接关闭）")
	require.Equal(t, 0, pool.Size())
}

// TestPoolForRoutesThroughProxy：For 建出的 client 真的把请求交给代理（不是直连
// 目标），且认证头按规格生成。出口是否"生效"的最短可观测形态。
func TestPoolForRoutesThroughProxy(t *testing.T) {
	type seen struct {
		uri  string
		auth string
	}
	got := make(chan seen, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- seen{uri: r.RequestURI, auth: r.Header.Get("Proxy-Authorization")}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()
	host, port := splitHostPort(t, proxy.Listener.Addr().String())

	pool := NewPool(nil)
	c := pool.For(New("http", host, port, "alice", "s3cret"))
	resp, err := c.HTTP.Get("http://upstream.invalid/hello")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	select {
	case s := <-got:
		require.Equal(t, "http://upstream.invalid/hello", s.uri, "代理必须收到绝对 URI（说明请求确实经代理）")
		require.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("alice:s3cret")), s.auth)
	case <-time.After(2 * time.Second):
		t.Fatal("代理没有收到请求：出口 client 没有走代理")
	}
}

// TestPoolClonesShardedTransport 门禁 (g)：分片底座 ⇒ 出口 client **仍是分片**，
// 分片数不变、连接池独立、Proxy 真的装上（经真实代理发一次请求验证）、
// Timeout/CheckRedirect 沿用底座。
//
// 负例：cloneTransport 漏掉 *httpx.ShardedTransport 分支 ⇒ 走 default 退回单
// transport，分片数变 1，红；Clone 里忘了覆盖 Proxy ⇒ 请求不会经过代理，红。
func TestPoolClonesShardedTransport(t *testing.T) {
	proxyAddr, seen := startRecordingProxy(t)

	baseTr := httpx.NewShardedTransport(&http.Transport{ForceAttemptHTTP2: true, MaxIdleConns: 64}, 4)
	require.Equal(t, 4, baseTr.ShardCount())
	base := &http.Client{
		Transport: baseTr,
		Timeout:   42 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	pool := NewPool(base)
	host, port := splitHostPort(t, proxyAddr)
	c := pool.For(New("http", host, port, "", ""))

	tr, ok := c.HTTP.Transport.(*httpx.ShardedTransport)
	require.True(t, ok, "分片底座经出口池后必须仍是分片（否则经代理的账号退回单连接）")
	require.Equal(t, 4, tr.ShardCount())
	require.NotSame(t, base.Transport, c.HTTP.Transport, "出口必须有自己的连接池")
	require.Equal(t, 42*time.Second, c.HTTP.Timeout, "Timeout 必须沿用底座")
	// CheckRedirect 必须沿用底座的**行为**，不只是"非 nil"（被换成别的非 nil 函数也要红）。
	require.NotNil(t, c.HTTP.CheckRedirect, "CheckRedirect 必须沿用底座")
	require.ErrorIs(t, c.HTTP.CheckRedirect(
		&http.Request{URL: &url.URL{Scheme: "http", Host: "upstream.example"}}, nil),
		http.ErrUseLastResponse, "克隆后必须仍是底座那个拒绝跟随重定向的回调")

	resp, err := c.HTTP.Get("http://upstream.example/echo")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Len(t, seen(), 1, "请求必须经过代理一次")
	require.Contains(t, seen()[0], "http://upstream.example/echo",
		"代理看到的必须是绝对 URI（证明 Transport.Proxy 被覆盖到每个分片）")
}

// startRecordingProxy 起一个玩具 HTTP 代理：记录请求行，回一个固定 200。
func startRecordingProxy(t *testing.T) (string, func() []string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	var mu sync.Mutex
	var lines []string
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				line, err := bufio.NewReader(conn).ReadString('\n')
				if err != nil {
					return
				}
				mu.Lock()
				lines = append(lines, line)
				mu.Unlock()
				_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
			}()
		}
	}()
	return ln.Addr().String(), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
}

// TestPoolDetachClosesIdleConnectionsOnShardedBase 门禁 (g)/B9：底座是分片时，
// 摘除后的关闭必须**关到每个分片**的空闲连接。
//
// `http.Client.CloseIdleConnections` 靠类型断言
// `interface{ CloseIdleConnections() }`；ShardedTransport 少写这个方法 ⇒ 关闭
// 静默不再关连接，而用 nil 底座的既有用例不会红。负例：删
// ShardedTransport.CloseIdleConnections ⇒ 本用例超时，红。
func TestPoolDetachClosesIdleConnectionsOnShardedBase(t *testing.T) {
	const shards = 3
	release := make(chan struct{})
	arrived := make(chan struct{}, shards)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// barrier：三个请求必须同时在飞，否则后续请求会复用第 0 片的连接，
		// "每片都有一条空闲连接"这个前提就不成立（门禁会变成看调度运气）。
		arrived <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	rls := sync.OnceFunc(func() { close(release) })
	t.Cleanup(rls)
	var closed atomic.Int32
	proxy.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateClosed || st == http.StateHijacked {
			closed.Add(1)
		}
	}
	defer proxy.Close()
	host, port := splitHostPort(t, proxy.Listener.Addr().String())

	base := &http.Client{Transport: httpx.NewShardedTransport(&http.Transport{MaxIdleConns: 32}, shards)}
	require.NotNil(t, base.Transport)
	pool := NewPool(base)
	c := pool.For(New("http", host, port, "", ""))

	// 先让**每个分片**各自建立一条连接（并发 ⇒ 最少在飞把请求摊到 3 个分片），
	// 这样"关闭只到一个分片"会被抓出来；只发一个请求的话断言 ≥1 是空转。
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for range shards {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := c.HTTP.Get("http://upstream.invalid/x")
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
			_ = resp.Body.Close()
		}()
	}
	for range shards {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			rls()
			t.Fatalf("只有 %d/%d 个请求同时在飞", len(arrived), shards)
		}
	}
	rls()
	wg.Wait()
	require.Empty(t, errs, "铺连接阶段有失败请求，关闭断言会失真")

	before := closed.Load()
	require.Zero(t, before, "摘除/关闭之前不应有连接被关闭")
	CloseClients(pool.Detach(nil))
	require.Eventually(t, func() bool { return closed.Load()-before >= shards }, 2*time.Second, 10*time.Millisecond,
		"摘除后的关闭必须把**每个**分片的空闲连接都关掉（漏 CloseIdleConnections ⇒ 连接泄漏）")
}

// foreignProxyTransport 是 pkg/egress **从未听说过**的 transport 类型，只实现
// proxyCloner 这个能力（CloneWithProxy）。它证明池子认的是能力而不是具体类型：
// 若有人把 cloneTransport 改回 `case *httpx.ShardedTransport` 之类的具体类型
// switch，stub 会掉进 DefaultTransport 兜底 ⇒ 请求打不到 stub 上、Proxy 也不生效。
type foreignProxyTransport struct {
	proxy func(*http.Request) (*url.URL, error)
	hits  *atomic.Int32
}

func (f *foreignProxyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.hits.Add(1)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("stub")),
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
	}, nil
}

func (f *foreignProxyTransport) CloneWithProxy(proxy func(*http.Request) (*url.URL, error)) http.RoundTripper {
	return &foreignProxyTransport{proxy: proxy, hits: f.hits}
}

// TestPoolHonorsAnyProxyCloner 门禁：任何实现 proxyCloner 的 transport 都必须被
// 池子认领（独立副本 + Proxy 覆盖），而不是只看具体类型。
// 负例：cloneTransport 退回具体类型判断 ⇒ 本用例红。
func TestPoolHonorsAnyProxyCloner(t *testing.T) {
	var hits atomic.Int32
	base := &http.Client{Transport: &foreignProxyTransport{hits: &hits}}
	pool := NewPool(base)
	c := pool.For(New("http", "127.0.0.1", 3128, "", ""))

	tr, ok := c.HTTP.Transport.(*foreignProxyTransport)
	require.True(t, ok, "实现了 CloneWithProxy 的 transport 必须被认领（池子只认能力）")
	require.NotSame(t, base.Transport, c.HTTP.Transport, "出口必须是独立副本")
	require.NotNil(t, tr.proxy, "Proxy 必须被覆盖")

	req, err := http.NewRequest(http.MethodGet, "http://upstream.invalid/x", nil)
	require.NoError(t, err)
	u, err := tr.proxy(req)
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:3128", u.String())

	resp, err := c.HTTP.Get("http://upstream.invalid/x")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.EqualValues(t, 1, hits.Load(), "请求必须走 stub 本身（不是 DefaultTransport 兜底）")
	require.Same(t, base.Transport, pool.Direct().HTTP.Transport, "直连仍是底座本身，不被克隆")
}
