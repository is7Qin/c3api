// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/scheduler"
)

type bpsTestStatusError int

func (e bpsTestStatusError) Error() string   { return "BPS test upstream status" }
func (e bpsTestStatusError) StatusCode() int { return int(e) }

func TestBPSResponsesPreservesProviderNeutralStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code int
	}{
		{name: "rate", err: bpsTestStatusError(http.StatusTooManyRequests), code: http.StatusTooManyRequests},
		{name: "policy", err: bpsTestStatusError(http.StatusForbidden), code: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, safe := bpsErrorResponse(tc.err)
			require.Equal(t, tc.code, code)
			require.ErrorIs(t, safe, tc.err)
			require.Equal(t, "BPS upstream rejected request", safe.Error())
		})
	}
}

// bpsTestRawError 模拟 SDK 的 APIError：带状态码和原始错误体。
type bpsTestRawError struct{ body string }

func (e bpsTestRawError) Error() string   { return "bps: upstream error" }
func (e bpsTestRawError) StatusCode() int { return http.StatusBadRequest }
func (e bpsTestRawError) RawJSON() string { return e.body }

// TestBPSUpstreamBodyFlowsToPipeline 上游错误体必须像 codex/静态 key 一样进入
// pipeline：规则可按 error.message 匹配，err_logs 落真实原因，WS 面回上游 message。
func TestBPSUpstreamBodyFlowsToPipeline(t *testing.T) {
	raw := `{"error":{"message":"model is not available for this account","type":"invalid_request_error"}}`
	cause := bpsTestRawError{body: raw}

	code, safe := bpsErrorResponse(cause)
	require.Equal(t, http.StatusBadRequest, code)
	require.JSONEq(t, raw, string(upstreamBody(safe)), "bpsAttemptError 必须透出上游 body")
	require.Equal(t, "model is not available for this account", upstreamErrMsg(upstreamBody(safe)))

	p := &Proxy{}
	w := httptest.NewRecorder()
	code, body, handled, handledErr := p.bpsHTTPError(context.Background(), w, "req", 1, time.Now(),
		&scheduler.Selection{AccountID: 1}, "gpt-5.1", cause)
	require.False(t, handled)
	require.Equal(t, http.StatusBadRequest, code)
	require.JSONEq(t, raw, string(body), "HTTP 面把上游 body 交给 pipeline")
	require.ErrorIs(t, handledErr, cause)

	code, msg, _, handled, _ := p.bpsWSError("req", &scheduler.Selection{AccountID: 1}, "gpt-5.1", cause)
	require.False(t, handled)
	require.Equal(t, http.StatusBadRequest, code)
	require.Equal(t, "model is not available for this account", string(msg), "WS 面优先用上游 error.message")
}

func TestBPSResponsesNeverFallsThroughStaticCaller(t *testing.T) {
	p := &Proxy{cfg: Config{UsageCapture: false}}
	sel := &scheduler.Selection{
		AccountID:      7,
		CredentialType: credential.TypeBPS,
		Model:          "gpt-4o",
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	w := httptest.NewRecorder()
	code, _, handled, err := (&responsesCaller{p: p}).Call(
		context.Background(), w, req, "rid", 10, time.Now(), sel, "must-not-be-used", []byte(`{"model":"gpt-4o"}`), false,
	)
	require.NoError(t, err)
	require.True(t, handled)
	require.Zero(t, code)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Contains(t, w.Body.String(), "BPS responses unavailable")
}

// TestBPSContinuationPolicy BPS 没有服务端续接：只跳过解析，不碰 body。
func TestBPSContinuationPolicy(t *testing.T) {
	require.True(t, bpsSkipsContinuation(&scheduler.Selection{CredentialType: credential.TypeBPS}))
	require.False(t, bpsSkipsContinuation(&scheduler.Selection{CredentialType: credential.TypeAPIKey}))
	require.False(t, bpsSkipsContinuation(nil))

	// 两条车道都必须接线到该判定（防漏接）；body 不出现任何 strip 调用。
	for _, name := range []string{"caller.go", "caller_responses_ws.go"} {
		src, err := os.ReadFile(name)
		require.NoError(t, err, name)
		require.Contains(t, string(src), "bpsSkipsContinuation(sel)", "%s 必须调用 bpsSkipsContinuation", name)
		require.Contains(t, string(src), `contPrevID = ""`, "%s 必须置空 contPrevID", name)
		require.NotContains(t, string(src), "bpsStripContinuation", "%s 不得再做 body 剥离", name)
	}
}

// TestBPSContinuationIDDuplicateAllowed 回退入口的 previous_response_id 重复拒绝：
// BPS 侧字段由 SDK 丢弃，c3api 不再为它做单遍扫描。
func TestBPSContinuationIDDuplicateAllowed(t *testing.T) {
	require.ElementsMatch(t, []string{"model", "stream"}, sensitiveBodyKeys)
	dup := []byte(`{"model":"m","previous_response_id":"a","previous_response_id":"b"}`)
	require.Empty(t, duplicateTopLevelKey(dup, sensitiveBodyKeys))
	escaped := []byte(`{"model":"m","previous_\u0072esponse_id":"a","previous_response_id":"b"}`)
	require.Empty(t, duplicateTopLevelKey(escaped, sensitiveBodyKeys))
}
