// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/pkg/sserelay"
)

// --- 流失败归因契约门禁（spec-stream-failure-truth §7 G1–G9） ---
//
// 判据面一律是**行为**：客户端拿到什么、日志行的 error_type/status_code/
// error_message、分表成员资格、规则事件收到什么 kind/status、计费金额。这些
// 都与「已提交后上游失败 = abort」直接对立——旧实现在本文件里全红。
//
// 读回协议：异步一律先 p.rec/p.errlog Close（或 p.sched.FlushRules）再断言；
// 双轨行共用同一 *domain.UsageLog 指针，断言前按指针去重。禁 time.Sleep。

// uniqueRows 按指针去重（usage_logs 与 err_logs 双轨共享同一行对象）。
func uniqueRows(t *testing.T, rows []*domain.UsageLog) []*domain.UsageLog {
	t.Helper()
	seen := make(map[*domain.UsageLog]struct{}, len(rows))
	out := make([]*domain.UsageLog, 0, len(rows))
	for _, l := range rows {
		if _, ok := seen[l]; ok {
			continue
		}
		seen[l] = struct{}{}
		out = append(out, l)
	}
	return out
}

// G1：codex 出站 ws，业务帧已写（已提交）后上游投递无 status 的错误帧 ⇒
// 唯一日志行 error_type=5xx、status_code=0（不合成上游没给过的码）、
// error_message 含上游原文。旧实现记 abort。
func TestStreamFailureG1CodexWSInbandPayloadIs5xx(t *testing.T) {
	testHealthSink.reset()
	up, mock := newCodexWSScriptUpstream(t,
		`{"type":"response.output_text.delta","delta":"hi"}`,
		`{"type":"error","error":{"type":"server_error","message":"`+inbandOverloadMsg+`"}}`)
	store := &captureLogStore{}
	p, _ := newWSErrProxy(t, up.URL, map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-10")}, store)
	pinWSRoute(p, 10)
	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	resp := postResponses(t, srv, `{"model":"gpt-4o","input":"hi","stream":true}`)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, resp.StatusCode, "已提交 ⇒ 200；body=%q", body)
	require.Len(t, mock.framesSnapshot(), 1, "已提交不换号：上游 create 恰 1")

	require.NoError(t, p.rec.Close(context.Background()))
	waitStoreLogs(t, store, 2) // 双轨：usage_logs + err_logs 同指针两条
	rows := uniqueRows(t, storeRows(store))
	require.Len(t, rows, 1, "一次尝试只有一个终态归因；rows=%+v", rows)
	require.Equal(t, domain.Err5xx, rows[0].ErrorType,
		"上游在流内声明失败 ⇒ 5xx（不是 abort：客户端侧并未结束本次尝试）")
	require.Equal(t, 0, rows[0].StatusCode, "序 1 不合成状态码（上游只给了 message）")
	require.NotNil(t, rows[0].ErrorMessage, "失败行必须留因")
	require.Contains(t, *rows[0].ErrorMessage, inbandOverloadMsg, "error_message 必须含上游原文")
}

// G2：同 G1 现场 ⇒ 规则事件收到 kind=5xx 且 **status 为空**（不合成状态）。
// 协议：MarkResult 异步投递 → FlushRules 排空 → 读 health sink。
// G2：健康面口径同 S1b——已提交的流内失败只有 5xx 归因、没有合成状态码。
// 文案用**非过载** 5xx：过载命中 seed-5xx-503-overload 后全透不惩罚（见
// TestInbandOverloadFailureIsNotPenalized），不再产生规则事件，因此
// 「事件带 kind=5xx、status=nil」这条契约只能在非过载文案上验证。
func TestStreamFailureG2CodexWSInbandPayloadHealthIs5xxNoStatus(t *testing.T) {
	testHealthSink.reset()
	up, _ := newCodexWSScriptUpstream(t,
		`{"type":"response.output_text.delta","delta":"hi"}`,
		`{"type":"error","error":{"type":"server_error","message":"`+inbandServerErrMsg+`"}}`)
	store := &captureLogStore{}
	p, _ := newWSErrProxy(t, up.URL, map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-10")}, store)
	pinWSRoute(p, 10)
	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	resp := postResponses(t, srv, `{"model":"gpt-4o","input":"hi","stream":true}`)
	_, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)

	p.sched.FlushRules() // MarkResult 异步投递 → 排空后再读 sink
	events := testHealthSink.eventsFor(10)
	require.Len(t, events, 1, "失败账号恰一条规则事件；events=%+v", events)
	require.Equal(t, rule.Kind5xx, events[0].kind, "健康 kind 必须与归因一致（5xx）")
	require.Nil(t, events[0].status,
		"序 1 不合成状态：规则事件 HTTPStatus 必须为空（带 http_status 条件的规则因此不匹配）")
}

// G3a：出站 ws，错误帧带 status=429 且已提交 ⇒ 429 + status_code=429 +
// 规则事件 Kind429 / status 429（SDK 把 numric status 翻成 HTTP 面同形错误）。
func TestStreamFailureG3aWSCodexErrorFrameStatus429(t *testing.T) {
	testHealthSink.reset()
	up, _ := newCodexWSScriptUpstream(t,
		`{"type":"response.output_text.delta","delta":"hi"}`,
		wsErrFrame429)
	store := &captureLogStore{}
	p, _ := newWSErrProxy(t, up.URL, map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-10")}, store)
	pinWSRoute(p, 10)
	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	resp := postResponses(t, srv, `{"model":"gpt-4o","input":"hi","stream":true}`)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "已提交 ⇒ 200；body=%q", body)

	require.NoError(t, p.rec.Close(context.Background()))
	waitStoreLogs(t, store, 2)
	rows := uniqueRows(t, storeRows(store))
	require.Len(t, rows, 1, "rows=%+v", rows)
	require.Equal(t, domain.Err429, rows[0].ErrorType, "序 2：上游给了 429 就必须记 429")
	require.Equal(t, http.StatusTooManyRequests, rows[0].StatusCode, "归因状态 = 上游真值")

	p.sched.FlushRules()
	events := testHealthSink.eventsFor(10)
	require.Len(t, events, 1, "events=%+v", events)
	require.Equal(t, rule.Kind429, events[0].kind, "健康 kind = 429")
	require.NotNil(t, events[0].status, "规则事件必须带状态（429 是真值，不是合成值）")
	require.Equal(t, http.StatusTooManyRequests, *events[0].status)
}

// G3b：native SSE（/v1/responses 车道）data: 内含 "status":429 且已提交 ⇒
// 5xx + status_code=0。能力边界（spec §3.3）：raw in-band status 不被解析
// （sserelay.PayloadError 不含 status），不会变成 429。
func TestStreamFailureG3bNativeSSEInbandStatusIs5xxNoStatus(t *testing.T) {
	testHealthSink.reset()
	errFrame := `data: {"type":"error","status":429,"error":{"type":"rate_limit_error","message":"` + inbandOverloadMsg + `"}}` + "\n\n"
	up, _ := newInbandSSEUpstream(t, "/v1/responses", "Authorization", "Bearer sk-upstream",
		`data: {"type":"response.output_text.delta","delta":"hi"}`+"\n\n", errFrame)
	store := &captureLogStore{}
	p := newTestProxyFormatLogs(t, up.URL, domain.FormatOpenAIResponses, store)

	rec := postResponsesStream(t, p)
	require.Equal(t, http.StatusOK, rec.Code, "已提交 ⇒ 200；body=%q", rec.Body.String())

	rows := uniqueRows(t, drainLogs(t, p, store))
	require.Len(t, rows, 1, "rows=%+v", rows)
	require.Equal(t, domain.Err5xx, rows[0].ErrorType,
		"data: 内的 status 不参与归因（能力边界）⇒ 落序 1 的 5xx")
	require.Equal(t, 0, rows[0].StatusCode, "raw in-band status 不被解析 ⇒ 不合成")
	require.NotNil(t, rows[0].ErrorMessage)
	require.Contains(t, *rows[0].ErrorMessage, inbandOverloadMsg)
}

// G4：入站 ws，上游发一帧业务帧后以 1011 异常关闭（无错误帧）⇒ network +
// status_code=0 + 规则事件 KindNetwork（序 5：关闭帧/EOF/读失败）。
func TestStreamFailureG4InboundWSAbnormalCloseIsNetwork(t *testing.T) {
	testHealthSink.reset()
	up, _ := fakeResponsesWSAbnormalClose(t, `{"type":"response.output_text.delta","delta":"hi"}`)
	store := &captureLogStore{}
	p, srv := wsHealthProxy(t, up.URL, store)

	c := wsDialInbound(t, srv)
	defer c.CloseNow()
	_ = wsReadFrames(t, c, 1)
	readResponsesWSClose(t, c, websocket.StatusInternalError)

	rows := uniqueRows(t, drainLogs(t, p, store))
	require.Len(t, rows, 1, "rows=%+v", rows)
	require.Equal(t, domain.ErrNetwork, rows[0].ErrorType,
		"上游断流是上游侧失败（不是 abort）；旧实现记 abort")
	require.Equal(t, 0, rows[0].StatusCode, "关闭帧无 HTTP 状态 ⇒ 0")

	p.sched.FlushRules()
	events := testHealthSink.eventsFor(1)
	require.Len(t, events, 1, "events=%+v", events)
	require.Equal(t, rule.KindNetwork, events[0].kind, "流失败归因必须与健康 kind 一致")
}

// G5①：纯客户端取消 ⇒ abort + 无健康事件（I3：只有客户端侧结束可记 abort）。
func TestStreamFailureG5aClientCancelIsAbortWithoutHealth(t *testing.T) {
	testHealthSink.reset()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, chatNormalFrame)
		fl.Flush()
		cancel() // 首帧之后客户端侧取消（序 0a）
		<-r.Context().Done()
	}))
	defer up.Close()
	store := &captureLogStore{}
	p := newInbandChatProxy(t, up.URL, 1, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	rows := uniqueRows(t, drainLogs(t, p, store))
	require.Len(t, rows, 1, "rows=%+v", rows)
	require.Equal(t, domain.ErrAbort, rows[0].ErrorType, "客户端侧结束 ⇒ abort")

	p.sched.FlushRules()
	require.Empty(t, testHealthSink.eventsFor(1), "客户端取消不投递健康事件（不惩罚账号）")
}

// G5②：上游错误载荷与客户端取消同时成立时，归因答 5xx（序 1 优先），不得被
// 取消语义覆盖。
//
// 实现选择（B）：**不动** classifyStreamExit（0a–0c 仍是唯一出口判定），序 1 的
// 优先级由出口侧「先看 cause、再信 exit kind」的顺序承担——一旦落到已提交失败
// 出口，归因只看 cause，不看 ctx 是否已取消。
//
// 事实补充（实测，非推测）：真实 relay 的读循环逐行 checkCancel（relay.go:509），
// 观测量级的取消总在读侧先被看见，所以「已取消的 ctx + 已被 relay 返回的
// PayloadError」在真实链路上不会同时出现——请求侧看到的仍是 abort（G5①）。
// 因此这里断言的是归因接缝本身：同一条 cause 在取消中也必须答 5xx，且必须给出
// 与聊天/codex 一致的 kind（I4）；对照组（非载荷 cause）仍答 abort，证明这不是
// 把 abort 语义放宽。
func TestStreamFailureG5bPayloadErrorBeatsCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	payload := &sserelay.PayloadError{Type: "server_error", Message: inbandOverloadMsg}

	f := streamFailureOf(ctx, streamExitCommitted, payload)
	require.Equal(t, domain.Err5xx, f.ET, "序 1 优先：上游已表态 ⇒ 5xx，取消不得覆盖")
	require.Equal(t, rule.Kind5xx, f.Kind)
	require.Zero(t, f.Status, "序 1 不合成状态")
	require.NotNil(t, streamFailureHealth(f), "5xx 必须投递健康事件")

	// 对照组：同一已取消 ctx + 一个纯取消 cause ⇒ 仍然是客户端侧结束（不投递健康）。
	g := streamFailureOf(ctx, streamExitClientCancel, context.Canceled)
	require.Equal(t, domain.ErrAbort, g.ET, "真正的客户端取消仍是 abort（未放宽）")
	require.Nil(t, streamFailureHealth(g))
}

// G4（spec-commit-state-and-round-billing §5）：网关 `UpstreamStreamTimeout`
// （经 `context.WithTimeoutCause` 注入的哨兵）⇒ `error_type=timeout`；已交付的
// 真实产出照常计费——金额断言 5/7/190 一字不动（旧实现记 network；本用例在旧
// 实现上必红：类别断言与 timeout 冲突）。已交付失败仍双轨（usage_logs + err_logs）。
func TestStreamFailureG4GatewayTimeoutIsTimeoutAndBilled(t *testing.T) {
	testHealthSink.reset()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`+"\n\n")
		fl.Flush()
		<-r.Context().Done() // 首帧后停滞 → UpstreamStreamTimeout 触发中止
	}))
	defer up.Close()
	store := &captureLogStore{}
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
	}
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 100*time.Millisecond, store, &BillingHooks{
		Resolver: &fakePriceLookup{entries: map[string]*domain.PriceEntry{"gpt-4o": proxyPricingEntry()}, variants: map[string][]*domain.PriceVariant{"gpt-4o": proxyPricingVariants()}},
		Balances: billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{}}, nil),
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))
	raw := storeRows(store)
	rows := uniqueRows(t, raw)
	require.Len(t, rows, 1, "一次尝试一个终态行；rows=%+v", rows)
	require.Len(t, raw, 2, "已交付的失败双轨：usage_logs 1 行 + err_logs 1 行（同指针）")

	row := rows[0]
	require.Equal(t, domain.ErrTimeout, row.ErrorType,
		"序 0b：网关自己的 UpstreamStreamTimeout（sentinel）到期是上游侧失败 ⇒ timeout（旧实现记 network）")
	require.Equal(t, int64(5), row.InputTokens, "中止前已累积的 usage 帧不丢")
	require.Equal(t, int64(7), row.OutputTokens)
	require.Equal(t, int64(190), row.Cost, "5×1e7+7×2e7 → 190 毫分：已交付的失败必须计费")
}

// G7：上游失败 + 未交付（响应头已提交但 0 张图 / 0 业务帧）⇒ 5xx +
// status_code=500；CallCount=0；不计费（usage_logs 0 行、err_logs 1 行）。
func TestStreamFailureG7UndeliveredUpstreamFailureIs5xxAndNotBilled(t *testing.T) {
	testHealthSink.reset()
	p, store := newImageStreamTestProxy(t, nil)
	r, rec := streamImageReq(t, nil)
	// 上游 500 信封：statusOf 非 0 ⇒ 序 4（5xx + 原状态）。
	genErr := &fakeEnvelope{status: http.StatusInternalServerError, body: `{"error":{"message":"boom"}}`}
	code, _, handled, err := p.streamImageGeneration(context.Background(), rec, r, "req-1", 10,
		time.Now(), streamImageSel(), "gpt-image-2", streamImageCred(), streamImageParams(),
		fakeStreamGen([]domain.ImageStreamEvent{{Type: domain.ImageStreamEventKeepalive}}, genErr, nil))
	require.NoError(t, err)
	require.Equal(t, 0, code)
	require.True(t, handled)

	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))
	raw := storeRows(store)
	rows := uniqueRows(t, raw)
	require.Len(t, rows, 1, "rows=%+v", rows)
	require.Len(t, raw, 1, "未交付的失败只进 err_logs：usage_logs 0 行")

	row := rows[0]
	require.Equal(t, domain.Err5xx, row.ErrorType, "seq 4：上游给了 500 ⇒ 5xx")
	require.Equal(t, http.StatusInternalServerError, row.StatusCode, "归因状态 = 上游真值")
	require.Zero(t, row.CallCount, "无 completed → 0 张落账")
}

// G9：relayWSContFail 的 contAcked 分支（网关绑定失败，帧已对客户端可见）不得
// 再默认 abort——它属归因输入域（I9），必须走与流中止同轨的派生归因。
//
// 直接驱动收尾函数：contAcked=true 的前置状态在生产由 relayWS 的"首个 id 帧
// ACK 成功"建立，本用例只关心该终态下的归因，故把前置事实作为实参给出。
func TestStreamFailureG9ContFailAfterAckIsNotAbort(t *testing.T) {
	testHealthSink.reset()
	logs := &captureLogStore{}
	p := newTestProxyFormatLogs(t, "http://127.0.0.1:1", domain.FormatOpenAIResponsesWS, logs)
	sel, err := p.sched.Select(10, domain.FormatOpenAIResponsesWS, "gpt-4o")
	require.NoError(t, err, "真实抢槽（finish 的 Release 与之平衡）")
	reqID := newReqID()
	start := time.Now()
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		client, aerr := websocket.Accept(w, r, &websocket.AcceptOptions{
			CompressionMode: websocket.CompressionContextTakeover,
		})
		if aerr != nil {
			return
		}
		defer client.CloseNow()
		var wg sync.WaitGroup
		// 轮上下文：contAcked=true 的收尾行 = 当前轮（G9 只关心归因，轮号用首轮）。
		rd := newRound(reqID, start)
		rd.businessN = 1
		p.relayWSContFail(client, &fakeTransport{}, &wg, r.Context(), errContUnavailable,
			true, rd, sel, 10, "gpt-4o", start)
	}))
	defer srv.Close()
	c := dialResponsesWS(t, srv)
	defer c.CloseNow()

	// 终态错误帧必达客户达（fail-closed 收尾的既有语义）。
	rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rcancel()
	_, frame, rerr := c.Read(rctx)
	require.NoError(t, rerr, "收尾必写终端错误帧")
	require.Contains(t, string(frame), `"type":"error"`)
	<-done

	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))
	rows := uniqueRows(t, storeRows(logs))
	require.Len(t, rows, 1, "rows=%+v", rows)
	require.NotEqual(t, domain.ErrAbort, rows[0].ErrorType,
		"I9：relayWSContFail 属归因输入域，contAcked 分支不得再默认 abort")
	require.NotNil(t, rows[0].ErrorMessage, "失败行必须留因")
}
