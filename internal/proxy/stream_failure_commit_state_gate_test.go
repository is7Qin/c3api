// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/sserelay"
)

// --- 交付事实门禁 H1–H7（fix-brief r1 §5） ---
//
// 唯一真值表（commitStateFor）：
//
//	framesSent=true,  writeFailed=true             ⇒ sent_ambiguous
//	framesSent=true,  writeFailed=false, cancel    ⇒ response_started
//	framesSent=true,  writeFailed=false, !cancel   ⇒ sent_ambiguous
//	framesSent=false, clientCancel                 ⇒ not_sent
//	framesSent=false, !clientCancel                ⇒ upstream_responded
//
// 读回协议：异步一律先 p.rec.Close / p.errlog.Close（或 errlog worker 的
// waitStoreLogs）再断言；禁 time.Sleep；只用 require；t.Parallel() 保持 0。

// h6CodexErrorEvent codex HTTP 上游的流内错误事件（type 精确匹配 sserelay 判据）。
const h6CodexErrorEvent = `{"type":"error","error":{"type":"server_error","message":"` + inbandOverloadMsg + `"}}`

// requireCommitState 断言行落盘交付态并返回该值。
func requireCommitState(t *testing.T, l *domain.UsageLog, want CommitState) {
	t.Helper()
	require.NotNil(t, l.CommitState, "交付态必须落盘（nil = 未知，不能用于计费）")
	require.Equal(t, want.String(), *l.CommitState)
}

// H1：入站 WS 上游错误帧、零业务帧 ⇒ 持久化 upstream_responded；
// usage_logs 0 行、err_logs 1 行（用户裁定：零业务帧不可计费）。
func TestStreamFailureH1InboundWSUpstreamErrorZeroFramesIsUpstreamResponded(t *testing.T) {
	testHealthSink.reset()
	up, _ := fakeResponsesWSScript(t, `{"type":"error","error":{"type":"server_error","message":"`+inbandOverloadMsg+`"}}`)
	store := &captureLogStore{}
	p, srv := wsHealthProxy(t, up.URL, store)

	c := wsDialInbound(t, srv)
	defer c.CloseNow()
	_ = wsReadFrames(t, c, 1)
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	raw := drainLogs(t, p, store)
	require.Equal(t, logTracks{usage: 0, errs: 1, abort: 0}, inbandTracks(raw), "rows=%+v", raw)
	row := failureRow(t, raw)
	require.Equal(t, domain.Err5xx, row.ErrorType, "序 1：上游已表态 ⇒ 5xx")
	requireCommitState(t, row, CommitUpstreamResponded)
	require.False(t, commitStateBillable(row.CommitState), "upstream_responded 不可计费")
}

// H2：入站 WS 上游错误帧、已下发 1 帧业务帧 ⇒ sent_ambiguous；双轨（usage+err
// 各 1 行）。错误帧本身不计业务交付。
func TestStreamFailureH2InboundWSUpstreamErrorAfterOneFrameIsSentAmbiguous(t *testing.T) {
	testHealthSink.reset()
	up, _ := fakeResponsesWSScript(t,
		`{"type":"response.output_text.delta","delta":"hi"}`,
		`{"type":"error","error":{"type":"server_error","message":"`+inbandOverloadMsg+`"}}`)
	store := &captureLogStore{}
	p, srv := wsHealthProxy(t, up.URL, store)

	c := wsDialInbound(t, srv)
	defer c.CloseNow()
	_ = wsReadFrames(t, c, 2)
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	raw := drainLogs(t, p, store)
	require.Equal(t, logTracks{usage: 1, errs: 1, abort: 0}, inbandTracks(raw), "rows=%+v", raw)
	row := failureRow(t, raw)
	require.Equal(t, domain.Err5xx, row.ErrorType)
	requireCommitState(t, row, CommitSentAmbiguous)
	require.True(t, commitStateBillable(row.CommitState), "已写业务帧 ⇒ 可计费")
}

// H3：客户端取消、零业务帧 ⇒ not_sent；usage_logs 0 行。
func TestStreamFailureH3ClientCancelZeroFramesIsNotSent(t *testing.T) {
	testHealthSink.reset()
	started := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // 排空请求体：服务端才能感知客户端断开
		close(started)                     // 请求已到上游、尚未写任何帧
		<-r.Context().Done()
	}))
	defer up.Close()
	store := &captureLogStore{}
	p := newInbandChatProxy(t, up.URL, 0, store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.HandleChat(rec, req)
	}()
	<-started
	cancel()
	<-done

	raw := drainLogs(t, p, store)
	require.Equal(t, logTracks{usage: 0, errs: 1, abort: 1}, inbandTracks(raw), "rows=%+v", raw)
	row := failureRow(t, raw)
	require.Equal(t, domain.ErrAbort, row.ErrorType, "客户端侧结束 ⇒ abort")
	requireCommitState(t, row, CommitNotSent)
	require.False(t, commitStateBillable(row.CommitState), "零业务帧不可计费")
}

// fakeResponsesWSHoldOpen 入站 WS 假上游：读到首个客户端帧后按脚本下发，随后
// 保持连接（读到网关关闭帧为止）。与 fakeResponsesWSScript 的差别只在“不主动
// 1000 关闭”——H4 要由测试客户端触发客户端侧结束。返回的 done 在上游 handler
// 退出时关闭（网关的关闭传播发生在 reportWSOutcome 之后，可作读回同步点）。
func fakeResponsesWSHoldOpen(t *testing.T, frames ...string) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			CompressionMode: websocket.CompressionContextTakeover,
		})
		if err != nil {
			return
		}
		defer c.CloseNow()
		typ, _, err := c.Read(context.Background())
		if err != nil {
			return
		}
		for _, f := range frames {
			if err := c.Write(context.Background(), typ, []byte(f)); err != nil {
				return
			}
		}
		_, _, _ = c.Read(context.Background()) // 等网关关闭帧
	}))
	t.Cleanup(srv.Close)
	return srv, done
}

// H4：客户端取消、已下发 1 帧业务帧（链路未断）⇒ response_started；双轨、按已
// 采 usage 计费（usage_logs 成员资格 = 计费资格；金额口径由 G6 覆盖）。
func TestStreamFailureH4ClientCancelAfterOneFrameIsResponseStarted(t *testing.T) {
	testHealthSink.reset()
	up, done := fakeResponsesWSHoldOpen(t, responsesWSCompletedFrame)
	store := &captureLogStore{}
	p, srv := wsHealthProxy(t, up.URL, store)

	c := wsDialInbound(t, srv)
	got := wsReadFrames(t, c, 1)
	require.Contains(t, got[0], `"type":"response.completed"`, "业务帧确已送达客户端")
	c.CloseNow() // 客户端侧结束
	<-done       // 上游连接已被网关取消（coder/websocket 在 ctx 取消时直拆）
	// 收尾在服务端 goroutine：等可计费行真正入队后再排空（done 不是记录完成的信号）。
	require.Eventually(t, func() bool { return p.rec.Pending() >= 1 }, 3*time.Second, time.Millisecond,
		"收尾必须把 response_started 行入队")

	raw := drainLogs(t, p, store)
	require.Equal(t, logTracks{usage: 1, errs: 1, abort: 1}, inbandTracks(raw), "rows=%+v", raw)
	row := failureRow(t, raw)
	require.Equal(t, domain.ErrAbort, row.ErrorType, "客户端侧结束 ⇒ abort")
	requireCommitState(t, row, CommitResponseStarted)
	require.True(t, commitStateBillable(row.CommitState))
	require.Equal(t, http.StatusOK, row.StatusCode, "已交付的取消记 200（既有语义）")
	require.Equal(t, int64(2), row.InputTokens, "已采 usage 不丢（6 gross − cached 1 − cache_write 3）")
	require.Equal(t, int64(5), row.OutputTokens)
}

// H5：下游写失败 + 已下发过业务帧 ⇒ sent_ambiguous；双轨。
// 复现：codex HTTP 在 [DONE] 帧写出时失败（此前已写 response.created 等业务帧）。
func TestStreamFailureH5DownstreamWriteFailureAfterFramesIsSentAmbiguous(t *testing.T) {
	testHealthSink.reset()
	up, _ := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{t6RespCreated, t6RespItemEv, t6RespDone}})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-1")}, up.URL, nil, nil, store)
	sel := selectCodexAccount(t, p, 10)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	w := &frameFailWriter{header: make(http.Header), failContains: []byte("[DONE]")}

	code, _, handled, err := p.callCodexResponses(req.Context(), w, req, "req-1", 10, time.Now(), sel,
		[]byte(`{"model":"gpt-4o","stream":true}`), true)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, 0, code)
	require.NotContains(t, w.body.String(), "[DONE]", "[DONE] 未送达（写失败）")

	require.NoError(t, p.rec.Close(context.Background()))
	waitStoreLogs(t, store, 2)
	raw := storeRows(store)
	require.Equal(t, logTracks{usage: 1, errs: 1, abort: 1}, inbandTracks(raw), "rows=%+v", raw)
	row := failureRow(t, raw)
	require.Equal(t, domain.ErrAbort, row.ErrorType, "序 0c：客户端侧结束")
	requireCommitState(t, row, CommitSentAmbiguous)
	require.True(t, commitStateBillable(row.CommitState), "写过业务帧 ⇒ 可计费")
}

// H6：跨出口一致性——同一事实（已下发 1 帧 + 上游错误）经 codex HTTP / chat /
// images / 入站 WS 必须落**同一个** commit_state（sent_ambiguous），error_type
// 可以是 5xx/network 但不得是 abort。
func TestStreamFailureH6CrossExitDeliveryFactsAgree(t *testing.T) {
	t.Run("codex HTTP", func(t *testing.T) {
		testHealthSink.reset()
		up, _ := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{t6RespCreated, h6CodexErrorEvent}})
		defer up.Close()
		store := &captureLogStore{}
		p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
			map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-1")}, up.URL, nil, nil, store)
		sel := selectCodexAccount(t, p, 10)
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		rec := httptest.NewRecorder()
		code, _, handled, err := p.callCodexResponses(req.Context(), rec, req, "req-1", 10, time.Now(), sel,
			[]byte(`{"model":"gpt-4o","stream":true}`), true)
		require.NoError(t, err)
		require.True(t, handled)
		require.Equal(t, 0, code)
		require.NoError(t, p.rec.Close(context.Background()))
		waitStoreLogs(t, store, 2)
		requireH6Row(t, storeRows(store))
	})

	t.Run("chat", func(t *testing.T) {
		testHealthSink.reset()
		up, _ := newInbandSSEUpstream(t, "/v1/chat/completions", "Authorization", "Bearer sk-upstream",
			chatNormalFrame, chatErrFrame)
		store := &captureLogStore{}
		p := newInbandChatProxy(t, up.URL, 0, store)
		rec := postChatStream(t, p)
		require.Equal(t, http.StatusOK, rec.Code)
		requireH6Row(t, drainLogs(t, p, store))
	})

	t.Run("images", func(t *testing.T) {
		testHealthSink.reset()
		p, store := newImageStreamTestProxy(t, nil)
		r, rec := streamImageReq(t, nil)
		b64a := "aGVsbG8="
		events := []domain.ImageStreamEvent{
			{Type: domain.ImageStreamEventKeepalive},
			{Type: domain.ImageStreamEventCompleted, B64JSON: &b64a},
		}
		envErr := &fakeEnvelope{status: http.StatusInternalServerError, body: `{"error":{"message":"upstream exploded"}}`}
		code, _, handled, err := p.streamImageGeneration(context.Background(), rec, r, "req-1", 10, time.Now(),
			streamImageSel(), "gpt-image-2", streamImageCred(), streamImageParams(),
			fakeStreamGen(events, envErr, nil))
		require.NoError(t, err)
		require.Equal(t, 0, code)
		require.True(t, handled)
		requireH6Row(t, drainLogs(t, p, store))
	})

	t.Run("inbound WS", func(t *testing.T) {
		testHealthSink.reset()
		up, _ := fakeResponsesWSScript(t,
			`{"type":"response.output_text.delta","delta":"hi"}`,
			`{"type":"error","error":{"type":"server_error","message":"`+inbandOverloadMsg+`"}}`)
		store := &captureLogStore{}
		p, srv := wsHealthProxy(t, up.URL, store)
		c := wsDialInbound(t, srv)
		defer c.CloseNow()
		_ = wsReadFrames(t, c, 2)
		readResponsesWSClose(t, c, websocket.StatusNormalClosure)
		requireH6Row(t, drainLogs(t, p, store))
	})
}

// requireH6Row 跨出口判据：唯一失败行的交付态必须是 sent_ambiguous，且不得把
// 上游失败误标成 abort。
func requireH6Row(t *testing.T, raw []*domain.UsageLog) {
	t.Helper()
	require.Equal(t, logTracks{usage: 1, errs: 1, abort: 0}, inbandTracks(raw), "rows=%+v", raw)
	row := failureRow(t, raw)
	require.NotEqual(t, domain.ErrAbort, row.ErrorType, "上游失败不是客户端侧结束")
	requireCommitState(t, row, CommitSentAmbiguous)
	require.True(t, commitStateBillable(row.CommitState))
}

// H7：每个已提交失败出口产出的 AttemptOutcome 必须能通过 Validate()
// （零帧失败的「类型层占位编码」不得再制造非法 outcome 被 pipeline_observer 丢弃）。
func TestStreamFailureH7DispatchedFailureOutcomesValidate(t *testing.T) {
	t.Run("chat", func(t *testing.T) {
		testHealthSink.reset()
		up, _ := newInbandSSEUpstream(t, "/v1/chat/completions", "Authorization", "Bearer sk-upstream",
			chatNormalFrame, chatErrFrame)
		store := &captureLogStore{}
		p := newInbandChatProxy(t, up.URL, 0, store)
		_, fc := wireObserverHarness(t, p)
		_ = postChatStream(t, p)
		requireValidOutcomes(t, fc.snapshot())
	})

	t.Run("codex HTTP 零帧写失败（网关超时）", func(t *testing.T) {
		// 回归点：codex_responses_http.go 曾把 out.BusinessFrameSent() 直接写进
		// outcome，零帧时 sent_ambiguous+false 过不了 Validate 并被静默丢弃。
		// 现场：首帧写被阻塞到请求 ctx deadline（序 0b：网关超时、零业务帧）。
		testHealthSink.reset()
		up, _ := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{t6RespCreated}})
		defer up.Close()
		store := &captureLogStore{}
		p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
			map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-1")}, up.URL, nil, nil, store)
		_, fc := wireObserverHarness(t, p)
		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		defer cancel()
		w := &h7DeadlineWriter{header: make(http.Header), ctx: ctx, first: make(chan struct{})}
		req := httptest.NewRequest(http.MethodPost, "/v1/responses",
			strings.NewReader(`{"model":"gpt-4o","stream":true}`)).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer ck-1")
		req.Header.Set("Content-Type", "application/json")
		p.HandleResponses(w, req)
		select {
		case <-w.first:
		default:
			t.Fatal("首帧写未发生——零帧写失败场景未建立")
		}

		outcomes := fc.snapshot()
		requireValidOutcomes(t, outcomes)
		require.True(t, outcomes[0].BusinessFrameSent, "类型层占位编码：Validate 要求 sent_ambiguous 带 BusinessFrameSent")
		require.Equal(t, CommitSentAmbiguous, outcomes[0].Commit)

		require.NoError(t, p.rec.Close(context.Background()))
		waitStoreLogs(t, store, 1) // 零业务帧 ⇒ 只有 err_logs 一行
		row := failureRow(t, storeRows(store))
		require.Equal(t, domain.ErrNetwork, row.ErrorType, "序 0b：网关超时 ⇒ network")
		requireCommitState(t, row, CommitUpstreamResponded)
	})

	t.Run("inbound WS upstream error", func(t *testing.T) {
		testHealthSink.reset()
		up, _ := fakeResponsesWSScript(t, `{"type":"error","error":{"type":"server_error","message":"`+inbandOverloadMsg+`"}}`)
		store := &captureLogStore{}
		p, srv := wsHealthProxy(t, up.URL, store)
		_, fc := wireObserverHarness(t, p)
		c := wsDialInbound(t, srv)
		defer c.CloseNow()
		_ = wsReadFrames(t, c, 1)
		readResponsesWSClose(t, c, websocket.StatusNormalClosure)
		requireValidOutcomes(t, fc.snapshot())
	})

	t.Run("images placeholder encodings", func(t *testing.T) {
		// images 出口的两种类型层编码（与 caller_images_stream.go 一致）：零帧失败
		// = sent_ambiguous+BusinessFrameSent（占位）；上游 5xx 前零帧 = upstream_responded。
		for _, tc := range []struct {
			name     string
			status   AttemptStatus
			commit   CommitState
			business bool
		}{
			{"零帧失败占位", 0, CommitSentAmbiguous, true},
			{"上游 5xx 零帧", 500, CommitUpstreamResponded, false},
		} {
			o := mergeDispatchBase(context.Background(), imagesStreamOutcome("req-1", streamImageSel(), "gpt-image-2",
				OperationTag(domain.OpImagesGenerations), AttemptTiming{}, AttemptUsage{}, ResultFailed, tc.status, tc.commit, tc.business, true, false))
			require.NoError(t, o.Validate(), tc.name)
		}
	})
}

// TestStreamFailureH7ReverseCodexZeroFrameFailureOutcomeValidates 是 H7 的**反向
// 变异留档**（oracle 要求可执行化）：codex 已提交失败出口在「零业务帧」时的
// outcome 必须合法，且必须**恰好**是 typeLayerTerminalFailure() 的编码。
//
// 变异检验（把注释里的口头约定变成红绿灯）：
//   - 若有人把占位退回 `o.BusinessFrameSent = framesSent`（零帧 ⇒ false），
//     Validate 拒收 → emitCodexOutcome 丢弃 → flow 捕获为空 → 本用例红；
//   - 若有人删掉 helper 或让某个出口自拼常量，helper 断言 + ws_report_test
//     的哨兵同时红。
//
// 场景说明：codex HTTP 的「零业务帧 + 已提交失败」只能由**上游失败在下行首帧
// 暴露**（序 0b：网关 UpstreamStreamTimeout 到期，归因 network）建立——纯流内
// 错误载荷在零帧时 Committed()=false，走 streamExitUncommitted 交 pipeline
// （stream_exit.go 的未提交分支），根本不进本出口、也不产出 AttemptOutcome。
// 故这里用 deadlineWriter 精确复现该出口（与 §5 H7 同一现场）。
func TestStreamFailureH7ReverseCodexZeroFrameFailureOutcomeValidates(t *testing.T) {
	testHealthSink.reset()
	up, _ := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{t6RespCreated}})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-1")}, up.URL, nil, nil, store)
	_, fc := wireObserverHarness(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	w := &h7DeadlineWriter{header: make(http.Header), ctx: ctx, first: make(chan struct{})}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"gpt-4o","stream":true}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer ck-1")
	req.Header.Set("Content-Type", "application/json")
	p.HandleResponses(w, req)
	select {
	case <-w.first:
	default:
		t.Fatal("首帧写未发生——零帧上游失败出口未建立")
	}

	outcomes := fc.snapshot()
	requireValidOutcomes(t, outcomes)
	o := outcomes[0]
	wantCommit, wantSent := typeLayerTerminalFailure()
	require.Equal(t, wantCommit, o.Commit, "codex 已提交失败出口必须用类型层唯一编码")
	require.Equal(t, wantSent, o.BusinessFrameSent)
	require.Equal(t, ResultFailed, o.Result)
	require.Equal(t, AttemptStatus(0), o.HTTPStatus)
	require.True(t, o.Terminal)

	// 反向变异（可执行版）：同一 outcome 若按真实交付事实（零帧）写
	// BusinessFrameSent=false，必须过不了 Validate —— 证明「退回 framesSent」
	// 不会静默通过，只会被观测面丢弃（现已留 Warn 痕迹）。
	mutated := o
	mutated.BusinessFrameSent = false
	require.Error(t, mutated.Validate(), "零帧 sent_ambiguous+false 必须被 Validate 拒绝（占位不可退回真实计数）")

	// 交付事实不经过 outcome：零业务帧 ⇒ 持久化 upstream_responded（不可计费）。
	require.NoError(t, p.rec.Close(context.Background()))
	waitStoreLogs(t, store, 1)
	row := failureRow(t, storeRows(store))
	require.Equal(t, domain.ErrNetwork, row.ErrorType, "序 0b：网关超时 ⇒ network")
	requireCommitState(t, row, CommitUpstreamResponded)
}

// h7DeadlineWriter 首帧 Write 阻塞到请求 ctx deadline 后返回 deadline 错误：
// 复现「零业务帧 + 网关超时」写失败出口（序 0b）。
type h7DeadlineWriter struct {
	header http.Header
	ctx    context.Context
	first  chan struct{}
}

func (w *h7DeadlineWriter) Header() http.Header { return w.header }
func (w *h7DeadlineWriter) WriteHeader(int)     {}
func (w *h7DeadlineWriter) Flush()              {}

func (w *h7DeadlineWriter) Write([]byte) (int, error) {
	select {
	case <-w.first:
	default:
		close(w.first)
	}
	<-w.ctx.Done()
	return 0, w.ctx.Err()
}

// requireValidOutcomes 断言捕获到的每个 dispatched outcome 都通过 Validate。
func requireValidOutcomes(t *testing.T, outcomes []AttemptOutcome) {
	t.Helper()
	require.NotEmpty(t, outcomes, "已提交失败出口必须产生一条 dispatched outcome")
	for i, o := range outcomes {
		require.NoError(t, o.Validate(), "outcome[%d] result=%v commit=%v business=%v", i, o.Result, o.Commit, o.BusinessFrameSent)
	}
}

// TestCommitStateForTruthTable 唯一真值表逐行断言（spec §5.2/§5.3）：零帧两行的
// writeFailed 是「任意」——不得因补写/写失败把 not_sent / upstream_responded
// 改写成可计费态。
func TestCommitStateForTruthTable(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		framesSent, writeFailed bool
		clientCancel            bool
		want                    CommitState
	}{
		{"已写帧+写失败+取消", true, true, true, CommitSentAmbiguous},
		{"已写帧+写失败+上游失败", true, true, false, CommitSentAmbiguous},
		{"已写帧+无写失败+取消", true, false, true, CommitResponseStarted},
		{"已写帧+无写失败+上游失败", true, false, false, CommitSentAmbiguous},
		{"零帧+无写失败+取消", false, false, true, CommitNotSent},
		{"零帧+写失败+取消（writeFailed 任意）", false, true, true, CommitNotSent},
		{"零帧+无写失败+上游失败", false, false, false, CommitUpstreamResponded},
		{"零帧+写失败+上游失败（writeFailed 任意）", false, true, false, CommitUpstreamResponded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, commitStateFor(tc.framesSent, tc.writeFailed, tc.clientCancel))
		})
	}
}

// TestStreamFailureH5bFactsSnapshotBeforeSupplementaryErrorFrame 补写 SSE error
// 帧也会失败时，交付事实必须取补写前快照：晚读 WriteFailed 会让同一次故障从
// response_started 漂移成 sent_ambiguous（两者都可计费，但取值不可复现）。
func TestStreamFailureH5bFactsSnapshotBeforeSupplementaryErrorFrame(t *testing.T) {
	w := &frameFailWriter{header: make(http.Header), failContains: []byte("event: error")}
	out := sserelay.NewOutput(w, 0, sserelay.OutputOptions{})
	defer out.Release()
	written, err := out.WriteFrame([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
	require.NoError(t, err)
	require.True(t, written)
	require.True(t, out.BusinessFrameSent())
	require.False(t, out.WriteFailed(), "补写前没有写失败")

	// 出口在补写 error 帧之前的快照（生产路径见 caller_*.go / codex_responses_http.go）。
	framesSent, writeFailed := out.BusinessFrameSent(), out.WriteFailed()

	// 客户端恰好也读不走：补写 error 帧失败 ⇒ WriteFailed 由 false 翻 true。
	_ = out.WriteError(buildErrorFrame(domain.FormatOpenAIChat, "boom"))
	require.True(t, out.WriteFailed(), "补写失败确实翻转 WriteFailed（漂移源）")

	require.Equal(t, CommitResponseStarted, commitStateFor(framesSent, writeFailed, true),
		"快照取值：已写帧 + 客户端结束 + 补写前未失败 ⇒ response_started")
	require.Equal(t, CommitSentAmbiguous, commitStateFor(out.BusinessFrameSent(), out.WriteFailed(), true),
		"晚读（错误示范）：同一事实会漂移成 sent_ambiguous")
}
