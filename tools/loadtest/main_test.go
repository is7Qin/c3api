// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ---- 采样直方图（无锁固定桶数组）行为固定 ----

func TestP99LatencyUsesDedicatedHistogram(t *testing.T) {
	m := &metrics{}
	m.latencySamples[1].Store(2)
	m.latencySamples[5].Store(1)
	require.Equal(t, int64(10), p99Latency(m))
}

func TestP99Empty(t *testing.T) {
	m := &metrics{}
	require.Equal(t, int64(-1), p99(m))
	require.Equal(t, int64(-1), p99Latency(m))
}

func TestP99BucketsCrossing(t *testing.T) {
	// 100 个 500ms + 100 个 900ms：99% 分位 = 900ms 桶（90*10）
	m := &metrics{}
	for i := 0; i < 100; i++ {
		storeSample(m, 500)
		storeSample(m, 900)
	}
	require.Equal(t, int64(900), p99(m))
}

func TestP99BoundaryBucketClampsOversize(t *testing.T) {
	// ≥10.24s 延迟进边界桶 1023，p99 返回 1023*10 = 10230（超界不精确，结构兼容）
	m := &metrics{}
	for i := 0; i < 100; i++ {
		storeSample(m, 20000) // 20s，超出 0-10.24s 覆盖区间
	}
	require.Equal(t, int64(10230), p99(m))
}

// ---- SSE 行读取（零分配 drainSSE）行为固定 ----

func TestDrainSSEStopsAtDone(t *testing.T) {
	src := "data: a\n\ndata: b\n\ndata: [DONE]\n\ndata: 不应读到\n"
	require.NoError(t, drainSSE(bufio.NewReader(bytes.NewBufferString(src))))
}

func TestDrainSSEReadsToEOFWithoutDone(t *testing.T) {
	src := "data: a\n\ndata: b\n\n"
	require.True(t, errors.Is(drainSSE(bufio.NewReader(bytes.NewBufferString(src))), io.EOF))
}

func TestDrainSSELongLine(t *testing.T) {
	// 行超过 bufio 默认缓冲（4096B），ReadSlice 分段 + ErrBufferFull 累积
	long := bytes.Repeat([]byte("x"), 8000)
	src := append(long, []byte("\ndata: [DONE]\n\n")...)
	require.NoError(t, drainSSE(bufio.NewReader(bytes.NewBuffer(src))))
}

func TestDrainSSEDoneInsideLongLine(t *testing.T) {
	line := append(bytes.Repeat([]byte("x"), 5000), []byte("[DONE] 后缀")...)
	src := append(line, []byte("\n\n")...)
	require.NoError(t, drainSSE(bufio.NewReader(bytes.NewBuffer(src))))
}

func TestDrainSSEFinalLineWithoutNewline(t *testing.T) {
	// 流在 [DONE] 行后无 \n 直接 EOF：仍按 [DONE] 终止
	src := "data: x\ndata: [DONE]"
	require.NoError(t, drainSSE(bufio.NewReader(bytes.NewBufferString(src))))
}

// ---- 请求构造（模板格式）行为固定 ----

func TestRequestTemplateMatchesNewRequest(t *testing.T) {
	buildReqTemplate()
	got := newRequestFromTemplate("ck-test", -1)
	want := newLoadtestRequest(*addr, "ck-test", *mode, "")
	require.Equal(t, want.Method, got.Method)
	require.Equal(t, want.URL.String(), got.URL.String())
	require.Equal(t, "application/json", got.Header.Get("Content-Type"))
	require.Equal(t, "Bearer ck-test", got.Header.Get("Authorization"))
	require.Equal(t, int64(len(tmplBody)), got.ContentLength)
	b, _ := io.ReadAll(got.Body)
	require.JSONEq(t, `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`, string(b))
}

// ---- [DONE] 后响应体尾部排空（连接复用）行为固定 ----

func TestDrainTailReadsToEOF(t *testing.T) {
	// chunked 尾部（空行 + 结束块）立刻 EOF：排空成功，连接可复用
	body := io.NopCloser(bytes.NewBufferString("\n0\r\n\r\n"))
	require.NoError(t, drainTail(body))
}

func TestDrainTailBoundedWhenNoEOF(t *testing.T) {
	// 服务端不结束流：50ms 超时放弃，绝不挂死
	start := time.Now()
	err := drainTail(newBlockingBody())
	require.ErrorIs(t, err, errDrainTimeout)
	require.Less(t, time.Since(start), 2*time.Second)
}

// blockingBody 永不返回数据的响应体；Close 解除阻塞（模拟服务端不主动结束的流）。
type blockingBody struct{ done chan struct{} }

func newBlockingBody() *blockingBody { return &blockingBody{done: make(chan struct{})} }

func (b *blockingBody) Read([]byte) (int, error) { <-b.done; return 0, io.EOF }

func (b *blockingBody) Close() error { close(b.done); return nil }

func TestChatRequestBodyOmitsStream(t *testing.T) {
	req := newLoadtestRequest("http://example.test", "ck-test", "chat", "")
	var body map[string]any
	require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
	_, ok := body["stream"]
	require.False(t, ok)
	require.Equal(t, "Bearer ck-test", req.Header.Get("Authorization"))
}

func TestStreamRequestBodyEnablesStream(t *testing.T) {
	req := newLoadtestRequest("http://example.test", "ck-test", "stream", "")
	var body map[string]any
	require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
	require.Equal(t, true, body["stream"])
}

// TestImagesStreamRequestBody images 双形态：-mode stream → JSON 带 stream:true
// （网关 scanKeys 探测后走 SSE 透传）；-mode chat → 非流式 JSON 原样（无 stream
// 键，data 数组计图计费路径不变）。
func TestImagesStreamRequestBody(t *testing.T) {
	prevFmt := *format
	t.Cleanup(func() { *format = prevFmt })
	*format = "images"

	req := newLoadtestRequest("http://example.test", "ck-test", "stream", "")
	require.Equal(t, "http://example.test/v1/images/generations", req.URL.String())
	var body map[string]any
	require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
	require.Equal(t, true, body["stream"], "stream 模式必须发 stream:true")

	req = newLoadtestRequest("http://example.test", "ck-test", "chat", "")
	body = map[string]any{} // 重置：Decode 进非 nil map 是合并语义
	require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
	_, ok := body["stream"]
	require.False(t, ok, "非流式 images 保持原 JSON 契约（不带 stream）")
}

// TestDoRequestImagesStreamDrainsTerminal 压测端对 Images SSE 终态的排空：
// completed 帧 + [DONE] 终帧的流正常计成功（首字节采样入直方图、零错误）。
func TestDoRequestImagesStreamDrainsTerminal(t *testing.T) {
	prevMode, prevFmt := *mode, *format
	*mode, *format = "stream", "images"
	t.Cleanup(func() { *mode, *format = prevMode, prevFmt })
	srv, _ := connCountingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "data: {\"type\":\"image_generation.completed\",\"data\":[{\"b64_json\":\"QUJD\"}]}\n\n")
		fl.Flush()
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	})
	useAddr(t, srv.URL) // 改 addr 后重建模板（模板按 flags 构建）
	m := &metrics{errDetail: make(map[string]int64)}
	doRequest(&http.Client{Timeout: 5 * time.Second}, m, rand.New(rand.NewPCG(1, 1)), true)
	require.Equal(t, int64(1), m.total.Load())
	require.Equal(t, int64(0), m.errs.Load())
	require.GreaterOrEqual(t, p99(m), int64(0), "成功流必须入首字节直方图")
}

// TestAffinityInjectsPromptCacheKey 软亲和注入：chat/responses 格式带
// prompt_cache_key（affinity 非空），anthropic 忽略（不吃该字段）。
func TestAffinityInjectsPromptCacheKey(t *testing.T) {
	prevFmt := *format
	t.Cleanup(func() { *format = prevFmt })

	*format = "chat"
	req := newLoadtestRequest("http://example.test", "ck-test", "chat", "affinity-7")
	var body map[string]any
	require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
	require.Equal(t, "affinity-7", body["prompt_cache_key"], "chat 注入显式亲和键")

	*format = "anthropic"
	req = newLoadtestRequest("http://example.test", "ck-test", "chat", "affinity-7")
	require.NotContains(t, readAll(t, req), "prompt_cache_key", "anthropic 不注入（格式白名单外）")
}

// TestAffinityTemplateRotation -affinity-keys N 预构建 N 个亲和模板，
// newRequestFromTemplate 按 idx 轮转取对应 prompt_cache_key。
func TestAffinityTemplateRotation(t *testing.T) {
	prevKeys, prevMode, prevFmt := *affinityKeys, *mode, *format
	t.Cleanup(func() {
		*affinityKeys, *mode, *format = prevKeys, prevMode, prevFmt
		buildReqTemplate()
	})
	*affinityKeys, *mode, *format = 3, "chat", "chat"
	buildReqTemplate()
	require.Len(t, affinityTmpls, 3)
	for i := 0; i < 3; i++ {
		req := newRequestFromTemplate("ck-test", i)
		b := readAll(t, req)
		require.Contains(t, b, fmt.Sprintf("affinity-%d", i), "轮转下标 %d 命中对应亲和域", i)
	}
	// idx 超界取模回绕；idx<0 走无亲和单模板。
	require.Contains(t, readAll(t, newRequestFromTemplate("ck-test", 7)), "affinity-1")
	require.NotContains(t, readAll(t, newRequestFromTemplate("ck-test", -1)), "prompt_cache_key")
}

// readAll 读尽请求体（工具侧小辅助）。
func readAll(t *testing.T, req *http.Request) string {
	t.Helper()
	if req.Body == nil {
		return ""
	}
	b, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	return string(b)
}

// ---- fill 模式（管理面填充压测）----

// fillFlag 测试辅助：临时改全局 flag 值，defer 恢复（flag 包全局单例，
// 其余测试依赖默认 mode=stream）。
func fillFlag(t *testing.T, m, ft, tok string) {
	t.Helper()
	prevMode, prevType, prevTok := *mode, *fillType, *adminToken
	*mode, *fillType, *adminToken = m, ft, tok
	t.Cleanup(func() { *mode, *fillType, *adminToken = prevMode, prevType, prevTok })
}

func TestFillTypeMixedCycles(t *testing.T) {
	want := []string{"users", "keys", "accounts", "groups", "templates", "pricing"}
	for i, w := range want {
		require.Equal(t, w, fillTypeFor(int64(i+1)))
	}
	require.Equal(t, "users", fillTypeFor(7)) // 7 = 回到首轮
}

func TestFillRequestUsersBody(t *testing.T) {
	fillFlag(t, "fill", "users", "tok-test")
	fillProc = 42
	fillSeq.Store(0)
	req, preErr := newFillRequest(&http.Client{Timeout: time.Second}, rand.New(rand.NewPCG(1, 1)))
	require.Empty(t, preErr)
	require.Equal(t, "Bearer tok-test", req.Header.Get("Authorization"))
	require.Equal(t, "http://127.0.0.1:8080/api/admin/users", req.URL.String())
	var body map[string]any
	require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
	require.Equal(t, "fill-42-1@loadtest.test", body["email"])
	require.Equal(t, float64(100), body["balance"])
	require.Equal(t, float64(8), body["max_concurrency"])
}

func TestFillRequestNamesUniquePerRequest(t *testing.T) {
	fillFlag(t, "fill", "users", "tok")
	fillProc = 42
	fillSeq.Store(0)
	rng := rand.New(rand.NewPCG(1, 1))
	// 序号推进：同一进程内实体名必不重复（重复创建 409 归类错误明细的前提）
	names := map[string]bool{}
	for i := 0; i < 100; i++ {
		req, preErr := newFillRequest(&http.Client{Timeout: time.Second}, rng)
		require.Empty(t, preErr)
		var body map[string]any
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		names[body["email"].(string)] = true
	}
	require.Len(t, names, 100)
}

func TestPickFillUser(t *testing.T) {
	prevUser, prevPool := *fillUser, fillUserPool
	*fillUser = "a@b.com:pw1"
	fillUserPool = []string{"c@d.com:pw2", "e@f.com:pw3"}
	t.Cleanup(func() { *fillUser, fillUserPool = prevUser, prevPool })
	rng := rand.New(rand.NewPCG(1, 1))
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		email, pw := pickFillUser(rng)
		seen[email+":"+pw] = true
	}
	require.Len(t, seen, 2)
	require.Contains(t, seen, "c@d.com:pw2")
	require.Contains(t, seen, "e@f.com:pw3")
	// 空池 → -fill-user 兜底
	fillUserPool = nil
	email, pw := pickFillUser(rng)
	require.Equal(t, "a@b.com", email)
	require.Equal(t, "pw1", pw)
}

func TestFillRequestSuccessRecordsLatency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, `{"ID":1}`)
	}))
	defer srv.Close()
	prevAddr := *addr
	*addr = srv.URL
	t.Cleanup(func() { *addr = prevAddr })
	fillFlag(t, "fill", "users", "tok")
	m := &metrics{errDetail: make(map[string]int64)}
	doFillRequest(&http.Client{Timeout: 5 * time.Second}, m, rand.New(rand.NewPCG(1, 1)), true)
	require.Equal(t, int64(1), m.total.Load())
	require.Equal(t, int64(0), m.errs.Load())
	require.GreaterOrEqual(t, p99Latency(m), int64(0)) // 成功 → latency 直方图有采样
}

func TestFillRequestNon200CountsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = fmt.Fprintln(w, `{"error":"dup"}`)
	}))
	defer srv.Close()
	prevAddr := *addr
	*addr = srv.URL
	t.Cleanup(func() { *addr = prevAddr })
	fillFlag(t, "fill", "users", "tok")
	m := &metrics{errDetail: make(map[string]int64)}
	doFillRequest(&http.Client{Timeout: 5 * time.Second}, m, rand.New(rand.NewPCG(1, 1)), true)
	require.Equal(t, int64(1), m.total.Load())
	require.Equal(t, int64(1), m.errs.Load())
	m.mu.Lock()
	require.Equal(t, int64(1), m.errDetail["status:409"])
	m.mu.Unlock()
}

// ---- doRequest 非 200：响应体排空（连接回池复用）行为固定 ----

// connCountingServer 统计 TCP 层新连接数（StateNew ≈ Accept）的 httptest
// server：断言连接复用行为的直接观测面。
func connCountingServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var conns atomic.Int64
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, &conns
}

// useAddr 临时改 -addr 并重建请求模板（模板按 flags 在 main 启动时构建一次，
// 测试改全局 flag 后必须重建）；cleanup 恢复并重建。
func useAddr(t *testing.T, u string) {
	t.Helper()
	prev := *addr
	*addr = u
	buildReqTemplate()
	t.Cleanup(func() { *addr = prev; buildReqTemplate() })
}

func TestDoRequestNon200CountsError(t *testing.T) {
	srv, _ := connCountingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprintln(w, `{"error":"rate limited"}`)
	})
	useAddr(t, srv.URL)
	m := &metrics{errDetail: make(map[string]int64)}
	doRequest(&http.Client{Timeout: 5 * time.Second}, m, rand.New(rand.NewPCG(1, 1)), true)
	require.Equal(t, int64(1), m.total.Load())
	require.Equal(t, int64(1), m.errs.Load())
	m.mu.Lock()
	require.Equal(t, int64(1), m.errDetail["status:429"])
	m.mu.Unlock()
}

func TestDoRequestNon200DrainsBodyForReuse(t *testing.T) {
	for _, testMode := range []string{"stream", "chat"} {
		t.Run(testMode, func(t *testing.T) {
			prevMode := *mode
			*mode = testMode
			t.Cleanup(func() { *mode = prevMode; buildReqTemplate() })

			const n = 160
			srv, conns := connCountingServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write(bytes.Repeat([]byte("rate limited\n"), 128)) // ~2KB body，确认有可排空内容
			})
			useAddr(t, srv.URL)
			m := &metrics{errDetail: make(map[string]int64)}
			client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
				MaxIdleConns:        n,
				MaxIdleConnsPerHost: n,
				IdleConnTimeout:     90 * time.Second,
			}}
			// 单 worker 顺序发 160 个 429：排空 body → 首请求拨号后连接回池，
			// 后续请求复用，连接数期望 = 1。transport 在空闲池为空时会并行起
			// 拨号与 readLoop 异步回池竞速（getConn：queueForIdleConn 未命中
			// 即 queueForDial，Go transport.go），单 worker 下该竞态最坏仅
			// +1 条连接（此后池恒温不再拨号，上界绝对）——并发多 worker 时
			// 窗口被交错放大，精确断言会 flake（评审 1M），故用顺序形态。
			// 不排空则每请求重拨 160 条 = 压测实证的 429 拨号风暴。
			rng := rand.New(rand.NewPCG(1, 1))
			for i := 0; i < n; i++ {
				doRequest(client, m, rng, true)
			}
			require.LessOrEqual(t, conns.Load(), int64(2))
			require.Equal(t, int64(n), m.total.Load())
			require.Equal(t, int64(n), m.errs.Load())
			m.mu.Lock()
			require.Equal(t, int64(n), m.errDetail["status:429"])
			m.mu.Unlock()
		})
	}
}

// TestDoRequestRecordsDialAndConnWait 建连观测：首请求真实拨号记 dial，
// 次请求复用 keep-alive 只记 conn_wait；两请求都必须有 conn_wait 样本。
// dial 由 newLoadTransport 的自持 DialContext 记录（确定性；httptrace 的
// Connect* 回调在拨号 goroutine 上偶发丢失，已弃用）。
func TestDoRequestRecordsDialAndConnWait(t *testing.T) {
	srv, _ := connCountingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "data: {\"ok\":true}\n\n")
		fl.Flush()
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	})
	useAddr(t, srv.URL)
	m := &metrics{errDetail: make(map[string]int64)}
	client := &http.Client{Timeout: 5 * time.Second, Transport: newLoadTransport(m)}
	rng := rand.New(rand.NewPCG(1, 1))
	doRequest(client, m, rng, true)
	doRequest(client, m, rng, true)
	require.Equal(t, int64(2), m.total.Load())
	require.Equal(t, int64(2), m.connWaitN.Load(), "每次取连接都要有 conn_wait 样本")
	require.GreaterOrEqual(t, m.dialN.Load(), int64(1), "首请求真实拨号必须记录")
	require.LessOrEqual(t, m.dialN.Load(), int64(2), "复用后不应再拨号")
}
