// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	codexsdk "github.com/is7Qin/codex-sdk"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

const codexInvalidBodyWant = `{"error":{"message":"invalid request body","type":"invalid_request_error"}}`

// assertCodexLocalRejectLog 本地拒绝的记录面：恰一行 400/Err4xx（固定文案）、
// commit_state=not_sent；MarkResult 0 次（规则事件提交数相对 rulesBefore 差值为 0
// ——默认规则表对 4xx 无动作，只看健康投递会漏掉 MarkResult(Kind4xx)）、无健康
// 投递、账号仍可用、并发槽已释放。
func assertCodexLocalRejectLog(t *testing.T, p *Proxy, store *captureLogStore, rulesBefore uint64, format domain.RequestFormat, accounts ...int64) {
	t.Helper()
	waitStoreLogs(t, store, 1)
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	require.Len(t, store.logs, 1, "本地拒绝恰一行记录（无故障转移）")
	lg := store.logs[0]
	store.mu.Unlock()
	require.Equal(t, http.StatusBadRequest, lg.StatusCode)
	require.Equal(t, domain.Err4xx, lg.ErrorType)
	require.Equal(t, format, lg.Format)
	require.NotNil(t, lg.ErrorMessage)
	require.Equal(t, codexInvalidBodyMsg, *lg.ErrorMessage)
	require.NotNil(t, lg.CommitState, "出网前拒绝：交付事实确定")
	require.Equal(t, CommitNotSent.String(), *lg.CommitState)
	require.Equal(t, rulesBefore, p.sched.RuleEventsSubmitted(), "本地拒绝 MarkResult 0 次（零规则事件）")
	p.sched.FlushRules()
	require.Empty(t, testHealthSink.throttles(), "本地拒绝不 Classify/MarkResult（零健康投递）")
	for _, id := range accounts {
		ri, ok := p.sched.Runtime(id)
		require.True(t, ok)
		require.Equal(t, domain.StatusActive, ri.Status, "账号健康不变")
		require.Zero(t, ri.Concurrency, "并发槽已释放")
	}
}

// TestCodexInvalidBodyIsClientError C3-8：SDK 本地拒绝（ErrInvalidRequestBody）
// 四个落点——HTTP 非流式/流式（含转换车道）、WS 拨号（经 translateDialError 包装
// 后仍识别）、WS 首帧、WS 后续帧——客户端看到 400 / 错误帧，记录 400，零健康投递、
// 无故障转移、上游零接触；后续帧拒绝后会话继续；conv_local_rejects 递增。
func TestCodexInvalidBodyIsClientError(t *testing.T) {
	twoPAT := func() map[int64]*domain.AccountExt {
		return map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-10"), 20: codexPATExt(20, "pat-20")}
	}

	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("http_direct_stream_%v", stream), func(t *testing.T) {
			testHealthSink.reset()
			up, upc := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{t6RespCreated, t6RespDone}})
			defer up.Close()
			store := &captureLogStore{}
			p, failures := newTestCodexRespProxy(t, credential.TypeCodexPAT, twoPAT(), up.URL, nil, nil, store)
			srv := httptest.NewServer(AIRouter(p))
			defer srv.Close()

			before := convLocalRejects.Load()
			rulesBefore := p.sched.RuleEventsSubmitted()
			// input 为数字：SDK 整形拒绝（网关入口不校验 input 形态）。
			resp := postResponses(t, srv, fmt.Sprintf(`{"model":"gpt-4o","stream":%v,"input":42}`, stream))
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			require.Equal(t, http.StatusBadRequest, resp.StatusCode, "body=%s", b)
			require.Equal(t, "application/json", resp.Header.Get("Content-Type"))
			require.JSONEq(t, codexInvalidBodyWant, string(b))
			require.Equal(t, before+1, convLocalRejects.Load(), "conv_local_rejects 递增")
			require.Zero(t, upc.callsN(), "未出网、不转移")
			require.Zero(t, recorderCalls(failures), "不上报失效")
			assertCodexLocalRejectLog(t, p, store, rulesBefore, domain.FormatOpenAIResponses, 10, 20)
		})
	}

	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("http_converted_chat_stream_%v", stream), func(t *testing.T) {
			testHealthSink.reset()
			up, upc := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{t6RespCreated, t6RespDone}})
			defer up.Close()
			store := &captureLogStore{}
			// 映射出的模型名含控制字节：转换后的体经 setModel 写入，SDK 拒绝。
			bad := map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "gpt\x01bad", Mode: domain.ModelMappingModeExplicit}}
			p, _ := newConvertedCodexTestProxy(t, credential.TypeCodexPAT, twoPAT(), up.URL, bad,
				[]domain.ProtocolConvert{domain.ProtocolConvertChatToResp}, store)

			before := convLocalRejects.Load()
			rulesBefore := p.sched.RuleEventsSubmitted()
			rec := postChatConv(t, p, fmt.Sprintf(`{"model":"gpt-4o","stream":%v,"messages":[{"role":"user","content":"hi"}]}`, stream), nil)
			require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
			require.JSONEq(t, codexInvalidBodyWant, rec.Body.String(), "转换车道同样是 OpenAI 错误形态")
			require.Equal(t, before+1, convLocalRejects.Load())
			require.Zero(t, upc.callsN())
			assertCodexLocalRejectLog(t, p, store, rulesBefore, domain.FormatOpenAIChat, 10, 20)
		})
	}

	t.Run("ws_dial", func(t *testing.T) {
		testHealthSink.reset()
		up, hooks := newCodexWSUpstream(t, []int{200}, 1)
		defer up.Close()
		store := &captureLogStore{}
		p, failures := newTestCodexWSProxy(t, credential.TypeCodexPAT, twoPAT(), up.URL, nil, store)
		srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
		defer srv.Close()
		c := dialResponsesWS(t, srv)
		defer c.CloseNow()

		before := convLocalRejects.Load()
		rulesBefore := p.sched.RuleEventsSubmitted()
		require.NoError(t, c.Write(context.Background(), websocket.MessageText,
			[]byte(`{"type":"response.create","model":"gpt-4o","input":42}`)))
		require.JSONEq(t, string(codexInvalidBodyFrame), string(readResponsesWSFrame(t, c)), "拨号阶段本地拒绝 → 错误帧")
		readResponsesWSClose(t, c, websocket.StatusNormalClosure)
		require.Equal(t, before+1, convLocalRejects.Load())
		require.Zero(t, hooks.upgradesN(), "Dial 未拨号即返回、不转移")
		require.Zero(t, recorderCalls(failures))
		assertCodexLocalRejectLog(t, p, store, rulesBefore, domain.FormatOpenAIResponsesWS, 10, 20)
	})

	t.Run("ws_dial_error_chain", func(t *testing.T) {
		// translateDialError 对 *DialError 包成 EnvelopeError：Unwrap 链保留，仍识别。
		wrapped := fmt.Errorf("dial: %w", &codexsdk.DialError{Err: codexsdk.ErrInvalidRequestBody})
		require.True(t, isCodexInvalidBody(wrapped))
		require.False(t, isCodexInvalidBody(errors.New("invalid request body")), "只认错误链，不按文本匹配")
		require.False(t, isCodexInvalidBody(nil))
	})

	t.Run("ws_first_frame", func(t *testing.T) {
		// 当前 SDK 在 Dial 阶段已按同一份首帧整形并拒绝，真实首帧 Send 不会再报
		// ErrInvalidRequestBody；经 codexWSTransportWrap 注入首帧失败，走完整
		// HandleResponsesWS → wsAttempt.call → relayWS（保留错误链）→
		// codexFirstFrameFailed 调用点。
		testHealthSink.reset()
		up, hooks := newCodexWSUpstream(t, []int{200}, 1)
		defer up.Close()
		store := &captureLogStore{}
		p, failures := newTestCodexWSProxy(t, credential.TypeCodexPAT, twoPAT(), up.URL, nil, store)
		var wrapped *firstWriteFailTransport
		p.codexWSTransportWrap = func(tr wsRelayTransport) wsRelayTransport {
			wrapped = &firstWriteFailTransport{wsRelayTransport: tr, err: fmt.Errorf("send: %w", codexsdk.ErrInvalidRequestBody)}
			return wrapped
		}
		srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
		defer srv.Close()
		before := convLocalRejects.Load()
		rulesBefore := p.sched.RuleEventsSubmitted()
		c := dialResponsesWS(t, srv)
		defer c.CloseNow()

		require.NoError(t, c.Write(context.Background(), websocket.MessageText,
			[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
		require.JSONEq(t, string(codexInvalidBodyFrame), string(readResponsesWSFrame(t, c)), "首帧本地拒绝 → 错误帧")
		readResponsesWSClose(t, c, websocket.StatusNormalClosure)
		require.NotNil(t, wrapped, "经 wsAttempt codex 分支")
		require.True(t, wrapped.closedNow.Load(), "上游连接已直拆")
		require.Equal(t, 1, hooks.upgradesN(), "不转移：第二账号不拨号")
		require.Empty(t, hooks.framesSnapshot(), "被拒首帧不出网")
		require.Zero(t, recorderCalls(failures))
		require.Equal(t, before+1, convLocalRejects.Load())
		assertCodexLocalRejectLog(t, p, store, rulesBefore, domain.FormatOpenAIResponsesWS, 10, 20)
	})

	t.Run("ws_first_frame_other_error_still_retryable", func(t *testing.T) {
		p, _ := newTestCodexWSProxy(t, credential.TypeCodexPAT, twoPAT(), "http://127.0.0.1:1", nil, &captureLogStore{})
		code, body, _, handled, callErr := p.codexFirstFrameFailed(nil, "", 0, time.Time{}, nil, "", nil, errors.New("broken pipe"))
		require.False(t, handled, "其余首帧失败照旧交循环（not-sent 可重试）")
		require.Zero(t, code)
		require.Equal(t, "broken pipe", string(body))
		require.EqualError(t, callErr, "broken pipe")
	})

	t.Run("ws_followup_frame_session_continues", func(t *testing.T) {
		testHealthSink.reset()
		up, hooks := newCodexWSUpstream(t, []int{200}, 2)
		defer up.Close()
		store := &captureLogStore{}
		p, _ := newTestCodexWSProxy(t, credential.TypeCodexPAT, twoPAT(), up.URL, nil, store)
		srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
		defer srv.Close()
		c := dialResponsesWS(t, srv)
		defer c.CloseNow()
		rulesBefore := p.sched.RuleEventsSubmitted()

		// 首帧 → 事件流 3 帧 + 回声；不带 model 的帧本地拒绝；下一帧照常回声，
		// 上游第 2 帧后 1000 关闭 → 会话按成功收尾（不写错误槽、不改结束分类）。
		got := codexWSSessionWithLocalReject(t, c, `{"type":"response.create","model":"gpt-4o","input":"hi"}`, 4,
			[]string{`{"type":"response.create","model":"gpt-4o","input":"again"}`}, 1)
		require.Contains(t, got[4], `"type":"echo"`, "拒绝后会话继续")
		readResponsesWSClose(t, c, websocket.StatusNormalClosure)
		hooks.mu.Lock()
		require.Len(t, hooks.frames, 2, "被拒帧不出网")
		require.Equal(t, 1, hooks.upgrades, "不重拨")
		hooks.mu.Unlock()
		require.NoError(t, p.rec.Close(context.Background()))
		waitStoreLogs(t, store, 1)
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 1)
		require.Equal(t, domain.ErrNone, store.logs[0].ErrorType, "结束分类不变（成功）")
		require.Equal(t, http.StatusOK, store.logs[0].StatusCode)
		require.Equal(t, rulesBefore, p.sched.RuleEventsSubmitted(), "中间帧本地拒绝 MarkResult 0 次（成功收尾的 ok 事件在无 ok 规则时短路）")
		require.Empty(t, testHealthSink.throttles())
	})
}

// firstWriteFailTransport 首次 Write（= relayWS 写首帧）返回 err，其余委托内层。
type firstWriteFailTransport struct {
	wsRelayTransport
	err       error
	failed    atomic.Bool
	closedNow atomic.Bool
}

func (f *firstWriteFailTransport) Write(ctx context.Context, typ websocket.MessageType, frame []byte) error {
	if !f.failed.Swap(true) {
		return f.err
	}
	return f.wsRelayTransport.Write(ctx, typ, frame)
}

func (f *firstWriteFailTransport) CloseNow() {
	f.closedNow.Store(true)
	f.wsRelayTransport.CloseNow()
}
