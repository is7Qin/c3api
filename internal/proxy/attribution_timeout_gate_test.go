// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/aiclient"
)

// --- 归因正确化门禁（spec-commit-state-and-round-billing §3 / §3.2） ---
//
// G1 / G2 是一对反例：同一个入站 WS 客户端「读完就断」的两种时点必须归到**不同**
// 类别。
//   - G1：已读到终态帧 ⇒ 本轮完成（none），err_logs 无行（旧实现记 abort）。
//   - G2：终态帧之前断 ⇒ abort，且 error_message 落可读文案（旧实现把库内部文本
//     `failed to get reader: failed to read frame header: EOF` 写进用户可见列）。

// fakeResponsesWSHoldScript 下发脚本帧后**保持连接**（不主动关闭）：让客户端成为
// 首退者，才测得到「谁先结束」。客户端断开后网关的关闭传播会把上游读解开，
// 处理器随之退出（`<-r.Context().Done()`）。
func fakeResponsesWSHoldScript(t *testing.T, frames ...string) (*httptest.Server, *inbandSSEUpstream) {
	t.Helper()
	h := &inbandSSEUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-upstream" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Responses-Websockets") != aiclient.ResponsesWSBetaHeader {
			w.WriteHeader(http.StatusBadRequest)
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
		<-r.Context().Done() // 保持连接：客户端断 → 网关关闭传播 → 本处理器退出
	}))
	t.Cleanup(srv.Close)
	return srv, h
}

// waitRelayIdle 等一次请求的收尾完成（inflight 归零在 finish/记录之后）：裸断
// 场景拿不到网关关闭帧，只能靠这个同步点再读落库结果。
func waitRelayIdle(t *testing.T, p *Proxy) {
	t.Helper()
	require.Eventually(t, func() bool { return p.Inflight() == 0 }, 5*time.Second, 10*time.Millisecond,
		"relay 收尾必须完成（inflight 归零）")
}

// 终态帧（Responses 协议）与业务帧样本。
const (
	wsDeltaFrame         = `{"type":"response.output_text.delta","delta":"hi"}`
	wsCompletedFrame     = `{"type":"response.completed","response":{"id":"r1","usage":{"input_tokens":3,"output_tokens":4}}}`
	wsCompletedNoUsage   = `{"type":"response.completed","response":{"id":"r1"}}`
	wsCompletedThenDelta = `{"type":"response.output_text.delta","delta":"again"}`
)

// G1（§3 归因表第一行）：客户端收到完整响应后**裸断**（无关闭帧的 TCP 直断）⇒
// error_type=none、err_logs 无该行、commit_state=response_started。
//
// 旧实现：relayClassify 只看「上游关闭码是否正常」，客户端断开一律落
// relayEndClientAbort ⇒ 一次完整交付被记成「已中止」+ 落一行 err_logs。
func TestStreamFailureG1CompletedThenBareDisconnectIsNone(t *testing.T) {
	testHealthSink.reset()
	up, _ := fakeResponsesWSHoldScript(t, wsDeltaFrame, wsCompletedFrame)
	store := &captureLogStore{}
	p, srv := wsHealthProxy(t, up.URL, store)

	c := wsDialInbound(t, srv)
	got := wsReadFrames(t, c, 2) // 业务帧 + 终态帧（网关已读且已写）
	require.Contains(t, got[1], `response.completed`)
	_ = c.CloseNow() // 裸断：不发关闭帧

	waitRelayIdle(t, p)
	rows := uniqueRows(t, drainLogs(t, p, store))
	require.Len(t, rows, 1, "一次完成交付恰一行；rows=%+v", rows)
	row := rows[0]
	require.Equal(t, domain.ErrNone, row.ErrorType,
		"已读到终态帧 ⇒ none（旧实现记 abort：客户端读完就断被当成中止）")
	require.Nil(t, row.ErrorMessage, "成功行不带错误文本（NULL）")
	require.NotNil(t, row.CommitState, "error_type=none 的行必须带 commit_state（I9）")
	require.Equal(t, "response_started", *row.CommitState)
	require.Equal(t, int64(3), row.InputTokens, "终态帧 usage 照常记账")
	require.Equal(t, int64(4), row.OutputTokens)

	p.sched.FlushRules()
	require.Empty(t, testHealthSink.eventsFor(1), "完成行不投递失败健康事件")
}

// G1 附加（G13 的本次相关面，§5/§6 的 CompletedWithoutUsage 本体属下一提交）：
// 终态帧**不带 usage** 时同样按完成收尾，不得因本次改动伪造 token 或写两行。
func TestStreamFailureG1CompletedWithoutUsageIsNoneWithoutFabrication(t *testing.T) {
	testHealthSink.reset()
	up, _ := fakeResponsesWSHoldScript(t, wsDeltaFrame, wsCompletedNoUsage)
	store := &captureLogStore{}
	p, srv := wsHealthProxy(t, up.URL, store)

	c := wsDialInbound(t, srv)
	_ = wsReadFrames(t, c, 2)
	_ = c.CloseNow()

	waitRelayIdle(t, p)
	rows := uniqueRows(t, drainLogs(t, p, store))
	require.Len(t, rows, 1, "仍是一行；rows=%+v", rows)
	require.Equal(t, domain.ErrNone, rows[0].ErrorType, "无 usage 的 completed 同样是完成")
	require.Zero(t, rows[0].InputTokens, "不伪造 0 以外的用量")
	require.Zero(t, rows[0].OutputTokens)
}

// G2（§3 归因表第二行 + §3.2）：终态帧**之前**客户端断 ⇒ abort，且
// error_message 落可读文案，**不含**库内部文本。
func TestStreamFailureG2ClientDisconnectBeforeTerminalIsAbort(t *testing.T) {
	testHealthSink.reset()
	up, _ := fakeResponsesWSHoldScript(t, wsDeltaFrame)
	store := &captureLogStore{}
	p, srv := wsHealthProxy(t, up.URL, store)

	c := wsDialInbound(t, srv)
	_ = wsReadFrames(t, c, 1) // 只读到业务帧，终态帧未到
	_ = c.CloseNow()

	waitRelayIdle(t, p)
	rows := uniqueRows(t, drainLogs(t, p, store))
	require.Len(t, rows, 1, "rows=%+v", rows)
	row := rows[0]
	require.Equal(t, domain.ErrAbort, row.ErrorType, "终态帧之前断 ⇒ abort")
	require.NotNil(t, row.ErrorMessage, "中止行必须留可读原因（不是 NULL）")
	require.NotContains(t, *row.ErrorMessage, "failed to get reader",
		"§3.2：库内部文案禁止落用户可见字段；got=%q", *row.ErrorMessage)
	require.Equal(t, msgClientAbortedBeforeDone, *row.ErrorMessage)
	require.NotNil(t, row.CommitState)
	require.Equal(t, "response_started", *row.CommitState,
		"已写过业务帧 ⇒ response_started（交付事实与类别正交）")
}

// TestStreamFailureG2TerminalFrameThenNextRoundDisconnectIsAbort 长连接的轮次
// 语义：上一轮 completed 之后又出现新帧 ⇒ 已进入下一轮，此时断仍是 abort
// （completed 事实是「当前轮」，不是「本连接曾经完成过」）。
func TestStreamFailureG2TerminalFrameThenNextRoundDisconnectIsAbort(t *testing.T) {
	testHealthSink.reset()
	up, _ := fakeResponsesWSHoldScript(t, wsCompletedFrame, wsCompletedThenDelta)
	store := &captureLogStore{}
	p, srv := wsHealthProxy(t, up.URL, store)

	c := wsDialInbound(t, srv)
	_ = wsReadFrames(t, c, 2)
	_ = c.CloseNow()

	waitRelayIdle(t, p)
	rows := uniqueRows(t, drainLogs(t, p, store))
	require.Len(t, rows, 1, "rows=%+v", rows)
	require.Equal(t, domain.ErrAbort, rows[0].ErrorType,
		"下一轮已开始且未读到其终态帧 ⇒ abort（不是「本连接完成过就恒 none」）")
}
