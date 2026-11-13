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

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// --- 中止行必须带可读原因（spec-abort-cause-truth） ---
//
// 症状（用户实证）：模板开 `codex_responses_transport = ws` 后，usage 日志里的
// 行显示「已中止」（error_type=abort → web/src/locales/zh.ts:86），但「错误信息」
// 列空着——err_logs.error_message 是 NULL。
//
// 根因面（三个中止出口都不写 ErrorMessage，全仓 `l.ErrorMessage = &` 六处没有一处
// 在这些路径上）：
//   - recordStreamAbort        —— HTTP 入站 · 已提交后上游断流（codex 出站 ws 走这条）
//   - reportChatOutcome        —— chat 流式中止
//   - reportWSOutcome          —— 入站 WS 中止/上游错误
//
// 判据：abort 行的 ErrorMessage 非 nil、含**上游原文**（不是只有 "HTTP 500" 这类
// 无信息量的包装），且受域内 500 字截断约束。

// failureRow 取出唯一一条失败行（usage/errlog 双轨共享同一指针，去重后取一条）。
// 归因迁移后「已提交后的上游失败」不再记 abort——本文件的判据是**可读原因**，
// 类别由归因表决定（abort 只留给客户端侧结束）。
func failureRow(t *testing.T, rows []*domain.UsageLog) *domain.UsageLog {
	t.Helper()
	seen := make(map[*domain.UsageLog]struct{}, len(rows))
	for _, l := range rows {
		if _, ok := seen[l]; ok {
			continue
		}
		seen[l] = struct{}{}
		if l.ErrorType != domain.ErrNone {
			return l
		}
	}
	require.FailNowf(t, "没有失败行", "rows=%+v", rows)
	return nil
}

// TestCodexWSOutboundCommittedFailureCarriesErrorMessage 用户现场：HTTP 入站 + codex
// 出站 WS。上游先给一帧正常内容（已提交），再给一帧带 status 的错误帧（SDK 的
// sseView 把它翻成 *HTTPError，帧体只挂在 Raw 上——`HTTPError.Error()` 只有
// "upstream HTTP 500"）。断言：客户端 200 + 协议错误帧；日志恰 1 条 5xx 行，且
// error_message 带上游 message。
func TestCodexWSOutboundCommittedFailureCarriesErrorMessage(t *testing.T) {
	testHealthSink.reset()
	up, mock := newCodexWSScriptUpstream(t,
		`{"type":"response.output_text.delta","delta":"hi"}`,
		`{"type":"error","status":500,"error":{"type":"server_error","message":"`+inbandOverloadMsg+`"}}`)
	store := &captureLogStore{}
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-10")}, up.URL, nil, nil, store,
		withCodexResponsesTransport(domain.CodexResponsesTransportWS))
	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	resp := postResponses(t, srv, `{"model":"gpt-4o","input":"hi","stream":true}`)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, resp.StatusCode, "已提交 ⇒ 200；body=%q", body)
	require.Len(t, mock.framesSnapshot(), 1, "上游 create 恰 1（已提交不换号）")

	require.NoError(t, p.rec.Close(context.Background()))
	waitStoreLogs(t, store, 1)
	rows := storeRows(store)
	require.Equal(t, logTracks{usage: 1, errs: 1, abort: 0}, inbandTracks(rows), "rows=%+v", rows)

	row := failureRow(t, rows)
	require.Equal(t, domain.Err5xx, row.ErrorType, "已提交后的上游失败 ⇒ 5xx（不是 abort）")
	require.NotNil(t, row.ErrorMessage,
		"失败行必须带错误文本（否则前端只剩类别 + 空列）")
	require.Contains(t, *row.ErrorMessage, inbandOverloadMsg,
		"error_message 必须含上游原文，而不是只有 HTTP 状态包装；got=%q", *row.ErrorMessage)
	require.LessOrEqual(t, len(*row.ErrorMessage), domain.ErrMsgMaxLen, "域内截断 500")
}

// TestChatCommittedFailureCarriesErrorMessage chat 车道已提交后命中流内错误帧：
// 同一条双轨失败行（usage_logs + err_logs），error_message 必须带上游 message。
func TestChatCommittedFailureCarriesErrorMessage(t *testing.T) {
	testHealthSink.reset()
	up, _ := newInbandSSEUpstream(t, "/v1/chat/completions", "Authorization", "Bearer sk-upstream",
		chatNormalFrame, chatErrFrame)
	store := &captureLogStore{}
	p := newInbandChatProxy(t, up.URL, 1, store)

	rec := postChatStream(t, p)
	require.Equal(t, http.StatusOK, rec.Code, "已提交 ⇒ 200；body=%q", rec.Body.String())

	rows := drainLogs(t, p, store)
	require.Equal(t, logTracks{usage: 1, errs: 1, abort: 0}, inbandTracks(rows), "rows=%+v", rows)

	row := failureRow(t, rows)
	require.Equal(t, domain.Err5xx, row.ErrorType, "流内错误载荷 ⇒ 5xx（序 1）")
	require.NotNil(t, row.ErrorMessage, "失败行必须带错误文本")
	require.Contains(t, *row.ErrorMessage, inbandOverloadMsg, "got=%q", *row.ErrorMessage)
}

// TestInboundWSCommittedFailureCarriesErrorMessage 入站 WS：上游发错误帧后 1000
// 关闭 ⇒ 记 5xx（关闭码保持 1000，客户端观感不变），落盘行必须带上游 message。
func TestInboundWSCommittedFailureCarriesErrorMessage(t *testing.T) {
	testHealthSink.reset()
	up, _ := fakeResponsesWSScript(t,
		`{"type":"error","error":{"type":"server_error","message":"`+inbandOverloadMsg+`"}}`)
	store := &captureLogStore{}
	p, srv := wsHealthProxy(t, up.URL, store)

	c := wsDialInbound(t, srv)
	defer c.CloseNow()
	_ = wsReadFrames(t, c, 1)
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	rows := drainLogs(t, p, store)
	require.Equal(t, logTracks{usage: 0, errs: 1, abort: 0}, inbandTracks(rows),
		"零业务帧 + 上游错误 ⇒ err_logs 1 行、usage_logs 0 行（upstream_responded 不可计费）；rows=%+v", rows)

	row := failureRow(t, rows)
	require.NotNil(t, row.CommitState)
	require.Equal(t, "upstream_responded", *row.CommitState, "错误帧不是业务交付 ⇒ 零业务帧")
	require.Equal(t, domain.Err5xx, row.ErrorType, "流内错误载荷 ⇒ 5xx（序 1）")
	require.NotNil(t, row.ErrorMessage, "失败行必须带错误文本")
	require.Contains(t, *row.ErrorMessage, inbandOverloadMsg,
		"入站 WS 的错误帧原文必须落到 error_message；got=%q", *row.ErrorMessage)
}

// TestInboundWSUpstreamAbnormalCloseCarriesErrorMessage 入站 WS：上游先给一帧业务
// 内容再以 1011 异常关闭（无错误帧）。已提交后的上游断流 ⇒ network + status 0
// （序 5），关闭码仍是上游真值 1011；该行同样必须带关闭帧描述。
func TestInboundWSUpstreamAbnormalCloseCarriesErrorMessage(t *testing.T) {
	testHealthSink.reset()
	up, _ := fakeResponsesWSAbnormalClose(t, `{"type":"response.output_text.delta","delta":"hi"}`)
	store := &captureLogStore{}
	p, srv := wsHealthProxy(t, up.URL, store)

	c := wsDialInbound(t, srv)
	defer c.CloseNow()
	_ = wsReadFrames(t, c, 1)
	readResponsesWSClose(t, c, websocket.StatusInternalError)

	rows := drainLogs(t, p, store)
	require.Equal(t, logTracks{usage: 1, errs: 1, abort: 0}, inbandTracks(rows), "rows=%+v", rows)

	row := failureRow(t, rows)
	require.Equal(t, domain.ErrNetwork, row.ErrorType,
		"上游断流 ⇒ network（序 5）；旧实现记 abort 是误标")
	require.NotNil(t, row.ErrorMessage, "上游异常关闭行必须带错误文本")
	require.Contains(t, *row.ErrorMessage, "StatusInternalError",
		"文本取关闭帧描述（coder/websocket 的 CloseError.Error()：status = ... and reason = ...）；got=%q", *row.ErrorMessage)
}

// fakeResponsesWSAbnormalClose 与 fakeResponsesWSScript 同握手面，但先下发一帧
// 业务帧，再以 1011 异常关闭（无关闭帧语义可循的上游故障）。
func fakeResponsesWSAbnormalClose(t *testing.T, frames ...string) (*httptest.Server, *inbandSSEUpstream) {
	t.Helper()
	h := &inbandSSEUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		h.mu.Lock()
		h.hits++
		h.mu.Unlock()
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
		_ = c.Close(websocket.StatusInternalError, "upstream blew up")
	}))
	t.Cleanup(srv.Close)
	return srv, h
}

// TestCauseMessageShape 判据单测：source → 文本的取值优先级（上游 message 优先、
// 退到 error 文本、空则 nil、域内截断）。
func TestCauseMessageShape(t *testing.T) {
	t.Run("nil ⇒ nil（成功/无原因路径保持 NULL）", func(t *testing.T) {
		require.Nil(t, causeMessage(nil))
	})

	t.Run("PayloadError ⇒ 上游 message 原样", func(t *testing.T) {
		msg := causeMessage(&payloadErrStub{msg: inbandOverloadMsg})
		require.NotNil(t, msg)
		require.Equal(t, inbandOverloadMsg, *msg)
	})

	t.Run("rawJSONer（*HTTPError 形态）⇒ 取信封里的 error.message", func(t *testing.T) {
		msg := causeMessage(&rawErrStub{text: "codexsdk: upstream HTTP 500", raw: `{"type":"error","status":500,"error":{"message":"boom detail"}}`})
		require.NotNil(t, msg)
		require.Equal(t, "boom detail", *msg, "不能只有 'upstream HTTP 500' 这种无信息量包装")
	})

	t.Run("无信封 ⇒ err.Error()", func(t *testing.T) {
		msg := causeMessage(io.ErrUnexpectedEOF)
		require.NotNil(t, msg)
		require.Equal(t, "unexpected EOF", *msg)
	})

	t.Run("超长 ⇒ 截断到 500 且不拆 rune", func(t *testing.T) {
		msg := causeMessage(&rawErrStub{text: "x", raw: `{"error":{"message":"` + strings.Repeat("超", 600) + `"}}`})
		require.NotNil(t, msg)
		require.Len(t, []rune(*msg), domain.ErrMsgMaxLen)
	})
}

// payloadErrStub 模拟 *sserelay.PayloadError 的两条接口（error + RawJSON）。
type payloadErrStub struct{ msg string }

func (e *payloadErrStub) Error() string { return e.msg }
func (e *payloadErrStub) RawJSON() string {
	return `{"error":{"type":"server_error","message":"` + e.msg + `"}}`
}

// rawErrStub 模拟 *codexsdk.HTTPError：Error() 只有状态包装，原文在 RawJSON。
type rawErrStub struct{ text, raw string }

func (e *rawErrStub) Error() string   { return e.text }
func (e *rawErrStub) RawJSON() string { return e.raw }
