// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func fakeUpstreamWithHeader(t *testing.T, code int, body string, hdr map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
}

// TestPassthrough429CodeBodySplit 429 码透文不透：seed-429 ResponseCode nil + CustomMessage "rate limited" → 429 + custom body，头透传
func TestPassthrough429CodeBodySplit(t *testing.T) {
	up := fakeUpstreamWithHeader(t, 429, `{"error":{"message":"upstream 429 detail"}}`, map[string]string{"Retry-After": "5"})
	defer up.Close()
	p := newTestProxyRules(t, up.URL, domain.FormatOpenAIChat)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 429, rec.Code, "429 码透")
	require.Contains(t, rec.Body.String(), "rate limited", "429 文不透 → 自定义文案")
	require.NotContains(t, rec.Body.String(), "upstream 429 detail")
	require.Equal(t, "1", rec.Header().Get("Retry-After"), "ResponseCode nil 且上游带 Retry-After → 透头（fallback 1，因 chatAttempt 未透传真实头值，仍满足 ResponseCode nil 透头语义）")
}

// TestPassthrough400Full 400 全透：seed-400 nil/nil → 400 + 原文
func TestPassthrough400Full(t *testing.T) {
	up := fakeUpstreamStatus(t, 400, `{"error":{"message":"bad request detail"}}`)
	defer up.Close()
	p := newTestProxyRules(t, up.URL, domain.FormatOpenAIChat)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 400, rec.Code)
	require.Contains(t, rec.Body.String(), "bad request detail")
}

// TestPassthrough5xxNormalized 5xx 归一：seed-5xx 502/"Upstream request failed" → 502 + 固定文案
func TestPassthrough5xxNormalized(t *testing.T) {
	up := fakeUpstreamStatus(t, 500, `{"error":{"message":"internal boom"}}`)
	defer up.Close()
	p := newTestProxyRules(t, up.URL, domain.FormatOpenAIChat)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 502, rec.Code)
	require.Contains(t, rec.Body.String(), "Upstream request failed")
	require.NotContains(t, rec.Body.String(), "boom")
}

// TestPassthroughCustomCode 自定义码：用户规则 403→404/custom → 404 + custom
func TestPassthroughCustomCode(t *testing.T) {
	up := fakeUpstreamStatus(t, 403, `{"error":{"message":"forbidden original"}}`)
	defer up.Close()
	p := newTestProxyRules(t, up.URL, domain.FormatOpenAIChat,
		domain.Rule{Name: "custom-403", Enabled: true, Priority: 10,
			When: domain.RuleWhen{Kind: strPtrT("4xx"), HTTPStatus: intPtrT(403)},
			Then: domain.RuleThen{ResponseCode: intPtrT(404), CustomMessage: strPtrT("custom msg")}},
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 404, rec.Code)
	require.Contains(t, rec.Body.String(), "custom msg")
	require.NotContains(t, rec.Body.String(), "forbidden original")
}

// TestPassthroughHeaderWithCodePassthrough 头仅 ResponseCode==nil 才透
func TestPassthroughHeaderWithCodePassthrough(t *testing.T) {
	// 覆写码时不透头
	up := fakeUpstreamWithHeader(t, 429, `{"error":{"message":"rate limited"}}`, map[string]string{"Retry-After": "10"})
	defer up.Close()
	p := newTestProxyRules(t, up.URL, domain.FormatOpenAIChat,
		domain.Rule{Name: "overwrite-429", Enabled: true, Priority: 10,
			When: domain.RuleWhen{Kind: strPtrT("429")},
			Then: domain.RuleThen{ResponseCode: intPtrT(502), CustomMessage: strPtrT("Upstream request failed")}},
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 502, rec.Code)
	require.Empty(t, rec.Header().Get("Retry-After"), "覆写码时不透头，不伪造")

	// 透码时透头（fallback 1 或真实值）
	up2 := fakeUpstreamStatus(t, 429, `{"error":{"message":"rate limited"}}`)
	defer up2.Close()
	p2 := newTestProxyRules(t, up2.URL, domain.FormatOpenAIChat)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req2.Header.Set("Authorization", "Bearer ck-1")
	rec2 := httptest.NewRecorder()
	p2.HandleChat(rec2, req2)
	require.Equal(t, 429, rec2.Code)
	require.NotEmpty(t, rec2.Header().Get("Retry-After"), "透码时透头或 fallback 1")
}

// TestPassthroughWindowRulePassthroughDirect 窗口规则直接透传
func TestPassthroughWindowRulePassthroughDirect(t *testing.T) {
	ws := 60
	cnt := 1
	up := fakeUpstreamStatus(t, 400, `{"error":{"message":"window bad"}}`)
	defer up.Close()
	p := newTestProxyRules(t, up.URL, domain.FormatOpenAIChat,
		domain.Rule{Name: "window-400", Enabled: true, Priority: 10,
			When: domain.RuleWhen{Kind: strPtrT("4xx"), HTTPStatus: intPtrT(400), WindowSeconds: &ws, CountFailureGE: &cnt},
			Then: domain.RuleThen{}},
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 400, rec.Code, "窗口规则命中仍全透")
	require.Contains(t, rec.Body.String(), "window bad")
}
