// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"errors"
	"net/http"

	"github.com/coder/websocket"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
)

// —— 错误分类与透传策略（S-F）端到端 ——

func strPtrT(s string) *string                                { return &s }
func intPtrT(v int) *int                                      { return &v }
func statusPtrT(s domain.AccountStatus) *domain.AccountStatus { return &s }

// newTestProxyRules 同 newTestProxyFormatLogs，但规则表预填自定义规则
// （非空表 → 不写种子——错误分类测试需要精确控制规则集）。
func newTestProxyRules(t *testing.T, upstream string, format domain.RequestFormat, rules ...domain.Rule) *Proxy {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{format}, Models: []string{"gpt-4o"},
	}
	store := &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}
	for _, r := range rules {
		_, err := store.CreateRule(context.Background(), r)
		require.NoError(t, err)
	}
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour, QuotaFlushInterval: time.Hour,
	}, noopLogStore{}, nil)
	accs := map[int64][]*domain.Account{10: {{
		ID: 1, TemplateID: 1, Template: tpl, UpstreamKey: "sk-upstream",
		Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4,
	}}}
	cfg := Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second,
		GroupKeyRPM: 0, UsageCapture: true,
	}
	re := rule.New(rule.Config{}, store, nil)
	require.NoError(t, re.Reload(context.Background()))
	sched := scheduler.New(scheduler.Config{
		DefaultMaxConcurrency: 4, SyncInterval: time.Hour,
	}, noopLoader{accs: accs}, re, nil)
	require.NoError(t, sched.InvalidateAllSync())
	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{
		"ck-1": activeKey(1, 1, 10),
	}}, noopUserLoader{}, nil)
	require.NoError(t, auth.Reload(context.Background()))
	hc := &http.Client{Transport: http.DefaultTransport}
	clients := aiclient.NewFactory(hc, aiclient.Config{
		UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second,
	})
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, FlushInterval: time.Hour}, noopErrLogStore{}, nil)
	return New(cfg, sched, credential.New(), rec, clients, auth, nil, nil, errlogW)
}

// fakeUpstreamStatus 按模式返回固定状态码 + body 的假上游（错误分类矩阵用）。
func fakeUpstreamStatus(t *testing.T, code int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
}

// TestErrClassifyUserCase401Balance 用户案例红绿（spec 测试节）：fake 上游 401 +
// body 含 balance → 用户规则（kind=4xx + http=401 + contains balance → unhealthy
// 30m）→ 账号 unhealthy + 冷却 30m；响应 502 固定文案（body 无 CreditsError/
// 工作区 ID/链接——泄漏修复）。
func TestErrClassifyUserCase401Balance(t *testing.T) {
	upstreamBody := `{"error":{"message":"Insufficient balance for workspace, credits balance: $0.00, workspace: ws_abc123, see https://example.com/billing"}}`
	up := fakeUpstreamStatus(t, 401, upstreamBody)
	defer up.Close()
	p := newTestProxyRules(t, up.URL, domain.FormatOpenAIChat,
		domain.Rule{Name: "balance-401", Enabled: true, Priority: 10,
			When: domain.RuleWhen{Kind: strPtrT("4xx"), HTTPStatus: intPtrT(401),
				ErrorMessageContains: strPtrT("balance")},
			Then: domain.RuleThen{Status: statusPtrT(domain.StatusUnhealthy), Cooldown: strPtrT("30m")}},
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, http.StatusBadGateway, rec.Code, "未声明 transmit → 归一 502，body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"upstream rejected request"`, "归一固定文案")
	require.NotContains(t, rec.Body.String(), "CreditsError", "上游私有错误体不得透传")
	require.NotContains(t, rec.Body.String(), "ws_abc123", "工作区 ID 不得透传")
	require.NotContains(t, rec.Body.String(), "example.com", "账单链接不得透传")

	// punish → MarkResult(Result4xx) → 规则命中 → unhealthy + 冷却 30m（bug 修复：
	// 此前 4xx 不进规则引擎，规则永不触发）
	p.sched.FlushRules()
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusUnhealthy, ri.Status)
	require.NotNil(t, ri.CooldownUntil)
	require.InDelta(t, 30*time.Minute, ri.CooldownUntil.Sub(time.Now()), float64(2*time.Minute),
		"用户规则冷却 30m")
	require.Zero(t, ri.Concurrency, "4xx 归一路径也必须释放并发槽")
}

// TestErrClassify4xxNormalizeMatrix 响应归一矩阵：401/403/404/409 无规则 →
// 502 + 固定文案（泄漏修复）；400 → 原文透传状态码 400（seed-4xx-400
// transmit=true，现状等价）。
func TestErrClassify4xxNormalizeMatrix(t *testing.T) {
	for _, tc := range []struct {
		code int
		body string
	}{
		{401, `{"error":{"message":"invalid upstream key"}}`},
		{403, `{"error":{"message":"forbidden"}}`},
		{404, `{"error":{"message":"model not found"}}`},
		{409, `{"error":{"message":"conflict"}}`},
	} {
		t.Run(strings.TrimPrefix(http.StatusText(tc.code), " "), func(t *testing.T) {
			up := fakeUpstreamStatus(t, tc.code, tc.body)
			defer up.Close()
			p := newTestProxyRules(t, up.URL, domain.FormatOpenAIChat) // 空规则 → 种子

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
				`{"model":"gpt-4o","messages":[]}`))
			req.Header.Set("Authorization", "Bearer ck-1")
			rec := httptest.NewRecorder()
			p.HandleChat(rec, req)

			require.Equal(t, http.StatusBadGateway, rec.Code, "无规则 4xx → 归一 502")
			require.Contains(t, rec.Body.String(), `"upstream rejected request"`)
			require.NotContains(t, rec.Body.String(), "invalid upstream key", "上游原文不得透传")
		})
	}

	// 400 → seed-4xx-400 transmit=true → 原文透传（状态码 400）
	up := fakeUpstreamStatus(t, 400, `{"error":{"message":"bad request"}}`)
	defer up.Close()
	p := newTestProxyRules(t, up.URL, domain.FormatOpenAIChat)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 400, rec.Code, "400 透传原文（seed-4xx-400，现状等价）")
	require.Contains(t, rec.Body.String(), "bad request")
	require.NotContains(t, rec.Body.String(), "upstream rejected request")
}

// TestErrClassifyTransmitRulePassthrough 用户 transmit 规则命中 → 原文透传
// （穿透语义：transmit=true 规则 = 用户需要错误详情）。
func TestErrClassifyTransmitRulePassthrough(t *testing.T) {
	up := fakeUpstreamStatus(t, 403, `{"error":{"message":"payment required: upgrade plan"}}`)
	defer up.Close()
	p := newTestProxyRules(t, up.URL, domain.FormatOpenAIChat,
		domain.Rule{Name: "tx-403", Enabled: true, Priority: 10,
			When: domain.RuleWhen{Kind: strPtrT("4xx"), HTTPStatus: intPtrT(403)},
			Then: domain.RuleThen{Transmit: true}},
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, 403, rec.Code, "transmit 规则 → 原始状态码透传")
	require.Contains(t, rec.Body.String(), "upgrade plan", "原文透传")
	require.NotContains(t, rec.Body.String(), "upstream rejected request")

	// transmit-only 规则无状态动作 → 不投递 MarkResult → 账号不冷却
	p.sched.FlushRules()
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusActive, ri.Status)
	require.Nil(t, ri.CooldownUntil)
}

// TestErrClassifyWSRelayNetworkSeed5s ws_relay 中继失联（上游错误关闭）→
// MarkResult(ResultError, code=0) → ruleKind 单点分流 kind=network →
// seed-network 命中：unhealthy + 冷却 5s（连接级独立类型——不吃 seed-5xx 的
// 10m，防呆 b 红绿）。
func TestErrClassifyWSRelayNetworkSeed5s(t *testing.T) {
	ft := &fakeTransport{readQueue: []fakeRead{
		{typ: 0, err: errors.New("upstream network error")}, // 网络失联（非 HTTP 状态 → code==0）
	}}
	env, c, _ := newRelayWSTest(t, ft, nil,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`))
	defer c.CloseNow()
	readResponsesWSClose(t, c, websocket.StatusInternalError) // 完成关闭握手（客户端循环退出依赖对端回帧）
	r := <-env.out
	require.True(t, r.handled)
	require.NoError(t, env.p.rec.Close(context.Background()))

	env.p.sched.FlushRules()
	ri, ok := env.p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusUnhealthy, ri.Status, "连接级失联 → ResultError 冷却")
	require.NotNil(t, ri.CooldownUntil)
	require.InDelta(t, 5*time.Second, ri.CooldownUntil.Sub(time.Now()), float64(2*time.Second),
		"seed-network 冷却 5s（非 seed-5xx 的 10m——连接级独立类型）")
	require.Zero(t, ri.Concurrency, "并发槽必须释放")
}
