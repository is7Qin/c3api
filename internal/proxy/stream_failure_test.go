// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/pkg/sserelay"
)

// --- classifyStreamFailure 纯函数单测（spec §3 序 1–5 五档 + 0b/0c） ---

// statusCarryingPayload 既是 PayloadError（经 Unwrap 可达）又实现 StatusCode()：
// 用来证明序 1 命中后**不再**问 statusOf（否则会拿到 503 而不是 0）。
type statusCarryingPayload struct{ err error }

func (e statusCarryingPayload) Error() string   { return e.err.Error() }
func (e statusCarryingPayload) Unwrap() error   { return e.err }
func (e statusCarryingPayload) StatusCode() int { return http.StatusServiceUnavailable }

// TestClassifyStreamFailureFiveSteps 五档各一条：序 1（流内错误载荷）、序 2
// （429）、序 3（4xx）、序 4（5xx）、序 5（断流/关闭帧）。
func TestClassifyStreamFailureFiveSteps(t *testing.T) {
	payload := &sserelay.PayloadError{Type: "server_error", Message: inbandOverloadMsg}

	t.Run("序 1 流内错误载荷 ⇒ 5xx + 不合成状态", func(t *testing.T) {
		f := classifyStreamFailure(payload)
		require.Equal(t, domain.Err5xx, f.ET)
		require.Equal(t, rule.Kind5xx, f.Kind)
		require.Zero(t, f.Status, "上游没给过状态码 ⇒ 0（不合成，§3.1）")
		require.Contains(t, f.Message, inbandOverloadMsg, "message 取上游原文")
	})

	t.Run("序 1 不调用 statusOf", func(t *testing.T) {
		// 该错误链上同时挂着 StatusCode()=503：若序 1 之后仍问 statusOf，
		// Status 会变成 503。断言 0 即证明没问。
		f := classifyStreamFailure(statusCarryingPayload{err: payload})
		require.Equal(t, domain.Err5xx, f.ET)
		require.Zero(t, f.Status, "序 1 命中即返回：不得再问 statusOf")
	})

	t.Run("序 2 429 ⇒ 429 + 原状态", func(t *testing.T) {
		f := classifyStreamFailure(&fakeEnvelope{status: http.StatusTooManyRequests, body: `{"error":{"message":"rate limited"}}`})
		require.Equal(t, domain.Err429, f.ET)
		require.Equal(t, rule.Kind429, f.Kind)
		require.Equal(t, http.StatusTooManyRequests, f.Status)
		require.Contains(t, f.Message, "rate limited", "信封 message 优先")
	})

	t.Run("序 3 4xx ⇒ 4xx + 原状态", func(t *testing.T) {
		f := classifyStreamFailure(&fakeEnvelope{status: http.StatusUnprocessableEntity, body: `{"error":{"message":"bad shape"}}`})
		require.Equal(t, domain.Err4xx, f.ET)
		require.Equal(t, rule.Kind4xx, f.Kind)
		require.Equal(t, http.StatusUnprocessableEntity, f.Status)
	})

	t.Run("序 4 5xx ⇒ 5xx + 原状态", func(t *testing.T) {
		f := classifyStreamFailure(&fakeEnvelope{status: http.StatusInternalServerError, body: `{"error":{"message":"boom"}}`})
		require.Equal(t, domain.Err5xx, f.ET)
		require.Equal(t, rule.Kind5xx, f.Kind)
		require.Equal(t, http.StatusInternalServerError, f.Status)
	})

	t.Run("序 5 无状态（关闭帧/EOF）⇒ network + 0", func(t *testing.T) {
		f := classifyStreamFailure(errors.New("failed to get reader: failed to read frame header: EOF"))
		require.Equal(t, domain.ErrNetwork, f.ET)
		require.Equal(t, rule.KindNetwork, f.Kind)
		require.Zero(t, f.Status)
		require.Contains(t, f.Message, "EOF", "无信封 ⇒ 退 err.Error()")
	})

	t.Run("nil cause 不 panic", func(t *testing.T) {
		f := classifyStreamFailure(nil)
		require.Equal(t, domain.ErrNetwork, f.ET, "无原因（防御路径）落序 5")
		require.Zero(t, f.Status)
		require.Empty(t, f.Message)
		require.Nil(t, f.message(), "空 message ⇒ NULL（不留空串）")
	})
}

// TestStreamFailureHealth 归因 → 健康事件：kind/status/message 单一口径；不投递
// 健康（Kind 零值，序 0c）⇒ nil。
func TestStreamFailureHealth(t *testing.T) {
	f := streamFailure{Status: 429, ET: domain.Err429, Kind: rule.Kind429, Message: "rate limited"}
	h := streamFailureHealth(f)
	require.NotNil(t, h)
	require.Equal(t, rule.Kind429, h.Kind)
	require.Equal(t, 429, h.Status, "归因状态透传到规则事件（§4）")
	require.Equal(t, "rate limited", h.ErrorMessage)
	require.False(t, h.Retryable, "Retryable 由观测点按矩阵填，归因不预设")

	require.Nil(t, streamFailureHealth(streamFailure{ET: domain.ErrAbort}),
		"客户端侧结束不投递健康事件（Kind 零值 ⇒ nil）")
}

// TestStreamFailureOfWriteFailed 序 0b / 0c 必须分开：写失败本身是客户端侧结束
// （abort，不投递健康），但网关自己的 UpstreamStreamTimeout 到期是上游侧失败
// （network）——把后者记 abort 是误标。
func TestStreamFailureOfWriteFailed(t *testing.T) {
	cause := errors.New("write: broken pipe")

	f := streamFailureOf(context.Background(), streamExitWriteFailed, cause)
	require.Equal(t, domain.ErrAbort, f.ET, "序 0c：客户端侧结束")
	require.Zero(t, f.Kind, "0c 不投递健康事件")

	deadlineCtx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	<-deadlineCtx.Done()
	f = streamFailureOf(deadlineCtx, streamExitWriteFailed, cause)
	require.Equal(t, domain.ErrNetwork, f.ET, "序 0b：网关 UpstreamStreamTimeout 到期")
	require.Equal(t, rule.KindNetwork, f.Kind)

	f = streamFailureOf(context.Background(), streamExitCommitted, &fakeEnvelope{status: http.StatusBadGateway})
	require.Equal(t, domain.Err5xx, f.ET, "已提交出口回到序 1–5")
	require.Equal(t, http.StatusBadGateway, f.Status)
}
