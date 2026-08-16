// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// --- relayWS 骨架与传输解耦单测（fakeTransport 可编程上游） ---
// 端到端双路径（aiclient/codex 真实传输）由 caller_responses_ws_test.go /
// codex_responses_ws_test.go 既有用例覆盖（零回归）；本文件用假传输直测合一
// 骨架的编排面——首帧转发失败 / 分类三路径 / 记录先行 / frameHook 调用序，
// 与传输具体类型解耦。

// fakeTransport 可编程 wsRelayTransport（relayWS 骨架单测上游面）。
// 行为：Write 记录帧（writeErr 非 nil = 恒失败——首帧转发失败路径）；Read 按
// 队列弹出（耗尽且无 readBlock → io.EOF 收尾；readBlock 非 nil → 阻塞至
// ctx 取消或闸释放——客户端首退场景防止 up-loop 抢跑）；Close 记录关闭码
// （closeCalled/closeBlock 非 nil → 进入信号 + 阻塞闸——记录先行断言）。
// 注意：CloseError 必须按**值**（非指针）返回——与 coder/websocket 真实错误
// 链同形（"received close frame: %w" 包值类型 CloseError），errors.As 按目标
// 元素类型匹配，指针形态会漏匹配（recordClose/wsCloseStatus 走 1011 兜底）。
type fakeTransport struct {
	mu        sync.Mutex
	writeErr  error
	writes    [][]byte
	writeTyp  []websocket.MessageType
	readQueue []fakeRead
	readBlock chan struct{}
	pingErr   error

	closeCalled chan struct{}
	closeBlock  chan struct{}
	closeCode   websocket.StatusCode
	closeNow    atomic.Bool
}

type fakeRead struct {
	typ   websocket.MessageType
	frame []byte
	err   error
}

func (f *fakeTransport) Write(_ context.Context, typ websocket.MessageType, frame []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, append([]byte(nil), frame...))
	f.writeTyp = append(f.writeTyp, typ)
	return f.writeErr
}

func (f *fakeTransport) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	f.mu.Lock()
	if len(f.readQueue) > 0 {
		r := f.readQueue[0]
		f.readQueue = f.readQueue[1:]
		f.mu.Unlock()
		return r.typ, r.frame, r.err
	}
	block := f.readBlock
	f.mu.Unlock()
	if block == nil {
		return 0, nil, io.EOF // 队列耗尽 = 上游流结束（客户端断开语义不抢跑）
	}
	select {
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case <-block:
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.readQueue) > 0 {
			r := f.readQueue[0]
			f.readQueue = f.readQueue[1:]
			return r.typ, r.frame, r.err
		}
		return 0, nil, io.EOF
	}
}

func (f *fakeTransport) Ping(context.Context) error { return f.pingErr }

func (f *fakeTransport) Close(code websocket.StatusCode, _ string) error {
	if f.closeCalled != nil {
		close(f.closeCalled)
	}
	if f.closeBlock != nil {
		<-f.closeBlock
	}
	f.mu.Lock()
	f.closeCode = code
	f.mu.Unlock()
	return nil
}

func (f *fakeTransport) CloseNow() { f.closeNow.Store(true) }

func (f *fakeTransport) closeCodeN() websocket.StatusCode {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeCode
}

// relayWSTestEnv relayWS 骨架单测环境：真实 client WS 对（服务端侧进骨架）+ 假
// 上游 + 测试代理。选号经 p.sched.Select 真抢并发槽（骨架 finish 的 Release 与
// 之平衡——Concurrency 断言可用）；out 收到骨架返回。
type relayWSTestEnv struct {
	p     *Proxy
	store *captureLogStore
	ft    *fakeTransport
	out   chan relayOutcome
}

type relayOutcome struct {
	handled bool
	fwMsg   string
}

func newRelayWSTest(t *testing.T, ft *fakeTransport, frameHook func([]byte), first []byte) (*relayWSTestEnv, *websocket.Conn, *httptest.Server) {
	t.Helper()
	store := &captureLogStore{}
	p := newTestProxyFormatLogs(t, "http://127.0.0.1:1", domain.FormatOpenAIResponsesWS, store)
	sel, err := p.sched.Select(10, domain.FormatOpenAIResponsesWS, "gpt-4o")
	require.NoError(t, err, "Select 必须成功（真实抢槽——finish 的 Release 与之平衡）")
	reqID := newReqID()
	start := time.Now()
	out := make(chan relayOutcome, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			CompressionMode: websocket.CompressionContextTakeover,
		})
		if err != nil {
			out <- relayOutcome{}
			return
		}
		defer client.CloseNow()
		handled, fwMsg := p.relayWS(client, ft, frameHook, r, reqID, 10, start, sel, "gpt-4o", websocket.MessageText, first)
		out <- relayOutcome{handled: handled, fwMsg: fwMsg}
	}))
	t.Cleanup(srv.Close)
	c := dialResponsesWS(t, srv)
	t.Cleanup(func() { c.CloseNow() })
	return &relayWSTestEnv{p: p, store: store, ft: ft, out: out}, c, srv
}

// TestRelayWSFirstFrameWriteFail 首帧转发失败 → (false, fwMsg) + CloseNow 直拆
// 上游（上游未消费请求，调用方按连接级错误转移）；不产生任何记录。
func TestRelayWSFirstFrameWriteFail(t *testing.T) {
	env, c, _ := newRelayWSTest(t, &fakeTransport{writeErr: errors.New("upstream write failed")}, nil,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`))
	defer c.CloseNow()

	r := <-env.out
	require.False(t, r.handled, "首帧转发失败 = 未处理（调用方转移）")
	require.Equal(t, "upstream write failed", r.fwMsg, "fwMsg = 截断错误文本")
	require.True(t, env.ft.closeNow.Load(), "首帧转发失败必须 CloseNow 直拆上游")
	require.NoError(t, env.p.rec.Close(context.Background()))
	require.Zero(t, env.p.rec.Pending(), "首帧转发失败路径不产生记录（上游未消费请求）")
}

// TestRelayWSUpstreamNormalClose 分类路径①：上游正常关闭（1000）→ 成功——
// completed 帧原样透传客户端 + 1000 关闭帧；ErrNone 200 记录（5 计数 usage）+
// ResultOK（不冷却）+ 并发槽释放；传输 Close 完成关闭握手（同码）。
func TestRelayWSUpstreamNormalClose(t *testing.T) {
	ft := &fakeTransport{readQueue: []fakeRead{
		{typ: websocket.MessageText, frame: []byte(responsesWSCompletedFrame)},
		{typ: 0, err: websocket.CloseError{Code: websocket.StatusNormalClosure}},
	}}
	env, c, _ := newRelayWSTest(t, ft, nil,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`))

	require.Equal(t, responsesWSCompletedFrame, string(readResponsesWSFrame(t, c)), "completed 帧原样透传")
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)
	r := <-env.out
	require.True(t, r.handled)
	require.Equal(t, websocket.StatusNormalClosure, ft.closeCodeN(), "上游正常关闭 → up.Close 完成关闭握手")

	require.NoError(t, env.p.rec.Close(context.Background()))
	env.store.mu.Lock()
	require.Len(t, env.store.logs, 1)
	lg := env.store.logs[0]
	env.store.mu.Unlock()
	require.Equal(t, domain.ErrNone, lg.ErrorType)
	require.Equal(t, http.StatusOK, lg.StatusCode)
	require.Equal(t, int64(3), lg.InputTokens)
	require.Equal(t, int64(5), lg.OutputTokens)
	require.Equal(t, int64(8), lg.TotalTokens)
	require.Equal(t, int64(1), lg.CacheReadTokens)
	require.Equal(t, int64(3), lg.CacheCreationTokens)
	env.p.sched.FlushRules() // MarkResult 异步投递：断言前排空
	ri, ok := env.p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusActive, ri.Status, "成功路径不冷却")
	require.Zero(t, ri.Concurrency, "成功路径必须释放并发槽")
}

// TestRelayWSClientAbort 分类路径②：客户端主动关闭（1000）→ abort——向上游
// 传播同码关闭帧；200 ErrAbort 记录（断前已嗅探 usage 不丢）；不 MarkResult
// （不冷却无辜账号）+ 并发槽释放。
func TestRelayWSClientAbort(t *testing.T) {
	ft := &fakeTransport{
		readQueue: []fakeRead{{typ: websocket.MessageText, frame: []byte(responsesWSCompletedFrame)}},
		readBlock: make(chan struct{}), // 队列耗尽后阻塞：up-loop 不抢首退（客户端退出先发生，分类确定）
	}
	env, c, _ := newRelayWSTest(t, ft, nil,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`))

	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	// 先读完 completed 帧（up-loop 的 client.Write 确定成功——客户端关闭在
	// 帧消费之后，clientErr 恒为关闭帧错误而非写失败竞态），再主动关闭
	require.Equal(t, responsesWSCompletedFrame, string(readResponsesWSFrame(t, c)))
	require.NoError(t, c.Close(websocket.StatusNormalClosure, "")) // 客户端主动结束会话
	r := <-env.out
	require.True(t, r.handled)
	require.Equal(t, websocket.StatusNormalClosure, ft.closeCodeN(), "客户端正常关闭 → 向上游传播同码")

	// abort 双轨：usage_logs（放行路径 abort）+ err_logs（豁免队列）各一行
	require.NoError(t, env.p.rec.Close(context.Background()))
	require.NoError(t, env.p.errlog.Close(context.Background()))
	env.store.mu.Lock()
	require.Len(t, env.store.logs, 2)
	var lg *domain.UsageLog
	for _, l := range env.store.logs {
		if l.ErrorType == domain.ErrAbort {
			lg = l
		}
	}
	env.store.mu.Unlock()
	require.NotNil(t, lg, "abort 记录必须存在")
	require.Equal(t, http.StatusOK, lg.StatusCode)
	require.Equal(t, int64(3), lg.InputTokens, "断开前已嗅探的 usage 不丢")
	require.Equal(t, int64(5), lg.OutputTokens)
	env.p.sched.FlushRules()
	ri, ok := env.p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusActive, ri.Status, "客户端断开不 MarkResult（不冷却无辜账号）")
	require.Zero(t, ri.Concurrency, "并发槽必须释放")
}

// TestRelayWSUpstreamError 分类路径③：上游网络错误（无关闭帧）→ 错误——记录
// 先行（ErrAbort + 断前 usage）+ ResultError 冷却（StatusUnhealthy）+ 客户端
// 1011 关闭 + 传输 CloseNow 直拆（上游已死免握手）。
func TestRelayWSUpstreamError(t *testing.T) {
	ft := &fakeTransport{readQueue: []fakeRead{
		{typ: websocket.MessageText, frame: []byte(responsesWSCompletedFrame)},
		{typ: 0, err: errors.New("upstream network error")},
	}}
	env, c, _ := newRelayWSTest(t, ft, nil,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`))

	require.Equal(t, responsesWSCompletedFrame, string(readResponsesWSFrame(t, c)), "completed 帧原样透传")
	readResponsesWSClose(t, c, websocket.StatusInternalError) // 1011 内部错误传播
	r := <-env.out
	require.True(t, r.handled)
	require.True(t, ft.closeNow.Load(), "上游错误 → CloseNow 直拆（免握手等待）")

	require.NoError(t, env.p.rec.Close(context.Background()))
	require.NoError(t, env.p.errlog.Close(context.Background()))
	env.store.mu.Lock()
	var lg *domain.UsageLog
	for _, l := range env.store.logs {
		if l.ErrorType == domain.ErrAbort {
			lg = l
		}
	}
	env.store.mu.Unlock()
	require.NotNil(t, lg, "recordStreamAbort 记录必须存在（ErrAbort——现状语义）")
	require.Equal(t, int64(3), lg.InputTokens, "断前已嗅探 usage 照常计费")
	require.Equal(t, int64(5), lg.OutputTokens)
	env.p.sched.FlushRules()
	ri, ok := env.p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusUnhealthy, ri.Status, "上游错误 → ResultError 冷却")
	require.Zero(t, ri.Concurrency, "并发槽必须释放")
}

// TestRelayWSRecordFirst 记录先行语义：客户端断开分类路径中，用量记录入队
// （p.finish）必须先于关闭传播（up.Close）——传输 Close 阻塞期间记录已在
// pending（客户端"感知会话结束"与"记录入队"之间无竞态窗口，记录不丢）。
func TestRelayWSRecordFirst(t *testing.T) {
	ft := &fakeTransport{
		readQueue:   []fakeRead{{typ: websocket.MessageText, frame: []byte(responsesWSCompletedFrame)}},
		readBlock:   make(chan struct{}), // 同上：客户端退出先发生
		closeCalled: make(chan struct{}, 1),
		closeBlock:  make(chan struct{}), // 关闭传播阻塞闸——观测记录先行
	}
	env, c, _ := newRelayWSTest(t, ft, nil,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`))

	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	require.Equal(t, responsesWSCompletedFrame, string(readResponsesWSFrame(t, c))) // 帧消费后关闭（clientErr 恒为关闭帧错误）
	require.NoError(t, c.Close(websocket.StatusNormalClosure, ""))
	<-ft.closeCalled // 传输 Close 已进入（= 分类已完成、p.finish 已先行执行）
	require.Equal(t, 1, env.p.rec.Pending(), "记录先行：关闭传播（up.Close）发出前用量已入队")
	close(ft.closeBlock) // 放行关闭传播
	r := <-env.out
	require.True(t, r.handled)
	require.NoError(t, env.p.rec.Close(context.Background()))
	require.NoError(t, env.p.errlog.Close(context.Background()))
	env.store.mu.Lock()
	require.NotEmpty(t, env.store.logs, "记录先行收尾：abort 记录不丢")
	env.store.mu.Unlock()
}

// TestRelayWSFrameHookEveryFrame frameHook 每帧调用：上游帧（读帧成功后）原样
// 到达钩子，与透传客户端帧字节一致。
func TestRelayWSFrameHookEveryFrame(t *testing.T) {
	var hookMu sync.Mutex
	var hooked [][]byte
	ft := &fakeTransport{readQueue: []fakeRead{
		{typ: websocket.MessageText, frame: []byte(responsesWSCompletedFrame)},
		{typ: 0, err: websocket.CloseError{Code: websocket.StatusNormalClosure}},
	}}
	env, c, _ := newRelayWSTest(t, ft, func(f []byte) {
		hookMu.Lock()
		hooked = append(hooked, append([]byte(nil), f...))
		hookMu.Unlock()
	}, []byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`))

	require.Equal(t, responsesWSCompletedFrame, string(readResponsesWSFrame(t, c)))
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)
	r := <-env.out
	require.True(t, r.handled)
	hookMu.Lock()
	defer hookMu.Unlock()
	require.Len(t, hooked, 1, "hook 每帧一次（读帧成功后）")
	require.Equal(t, responsesWSCompletedFrame, string(hooked[0]), "hook 收到与透传一致的帧字节")
	require.NoError(t, env.p.rec.Close(context.Background()))
}

// TestRelayWSFrameHookBeforeClientWrite 调用序：hook 在 client.Write 之前——
// 客户端连接已拆（client.Write 必失败）时 hook 仍触发（判死帧不因客户端写失
// 败漏检——FatalAuth 双源去重前提；与现状 codex_responses_ws.go:339-341 先于
// 354 的调用序一致）。
func TestRelayWSFrameHookBeforeClientWrite(t *testing.T) {
	deathFrame := []byte(`{"type":"error","error":{"code":"token_invalidated","message":"x"}}`)
	hookCalled := make(chan []byte, 1)
	ft := &fakeTransport{readQueue: []fakeRead{{typ: websocket.MessageText, frame: deathFrame}}}
	env, c, _ := newRelayWSTest(t, ft, func(f []byte) {
		hookCalled <- append([]byte(nil), f...)
	}, []byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`))

	c.CloseNow() // 客户端连接立即拆——client.Write 必失败
	select {
	case f := <-hookCalled:
		require.Equal(t, string(deathFrame), string(f), "hook 先于 client.Write 调用（写失败不阻断判死嗅探）")
	case <-time.After(3 * time.Second):
		t.Fatal("frameHook 未触发")
	}
	r := <-env.out
	require.True(t, r.handled, "客户端断开 → abort 收尾")
	require.NoError(t, env.p.rec.Close(context.Background()))
}
