// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

// Todo 3（model-mapping-mode）：用量身份（UsageLog.MappedModel）与缺价预检模型
// 的逐行接线测试。规格 §3 identity matrix：
//   - 非 Search 选中尝试：日志 MappedModel = Selection.UsageMappedModel（implicit
//     回填客户端模型；explicit 非 identity = 目标；explicit identity/无映射 = 空）；
//     缺价预检模型 = 用量身份非空 ? 用量身份 : sel.Model。
//   - Search：保持既有 mappedFor(reqModel, sel.Model) 终态日志与固定按次计费，
//     不触达 Selection 身份方法。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// mappingTpl 构造单模板 chat 测试代理模板（映射矩阵共用基座）。
func mappingTpl(upstream string, mapping map[string]domain.ModelMappingEntry) *domain.Template {
	return &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
		ModelMapping: mapping,
	}
}

// postChat 向代理发一条非流式 chat 请求（Bearer ck-1）。
func postChat(p *Proxy, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	return rec
}

// TestMappedModelIdentityMatrix 五行身份矩阵（规格 §3）：成功路径日志
// Model = 客户端请求模型恒定；MappedModel 按模式/映射逐行判定——implicit
// 恒回填客户端模型（含 identity 非空），explicit 仅非 identity 记目标。
func TestMappedModelIdentityMatrix(t *testing.T) {
	tests := []struct {
		name       string
		mapping    map[string]domain.ModelMappingEntry
		wantMapped string
	}{
		{"无映射", nil, ""},
		{"explicit 目标不同", map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "upstream-b", Mode: domain.ModelMappingModeExplicit}}, "upstream-b"},
		{"explicit identity", map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "gpt-4o", Mode: domain.ModelMappingModeExplicit}}, ""},
		{"implicit 目标不同", map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "upstream-b", Mode: domain.ModelMappingModeImplicit}}, "gpt-4o"},
		{"implicit identity", map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "gpt-4o", Mode: domain.ModelMappingModeImplicit}}, "gpt-4o"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			up := fakeOpenAI(t, "")
			defer up.Close()
			store := &captureLogStore{}
			p := newTestProxyTplTimeoutLogs(t, mappingTpl(up.URL, tc.mapping), 1, true, 30*time.Second, store, nil)
			rec := postChat(p, `{"model":"gpt-4o","messages":[]}`)
			require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
			require.NoError(t, p.rec.Close(context.Background()))
			store.mu.Lock()
			defer store.mu.Unlock()
			require.Len(t, store.logs, 1, "must capture exactly one usage log")
			require.Equal(t, "gpt-4o", store.logs[0].Model, "Model = 客户端请求模型")
			require.Equal(t, tc.wantMapped, store.logs[0].MappedModel)
		})
	}
}

// TestPrecheckPriceMappingModel 缺价预检模型逐行（规格 §3）：预检查价模型 =
// 用量身份非空 ? 用量身份 : sel.Model——implicit 按客户端模型定价（目标无价
// 不 402），explicit 按目标定价（客户端有价不救）。价格只配一侧即构成另一侧
// 的 402 判定面；cost 数值证明实际结算价目跟走用量身份。
func TestPrecheckPriceMappingModel(t *testing.T) {
	clientPriced := func() map[string]*domain.PriceEntry {
		return map[string]*domain.PriceEntry{"gpt-4o": proxyPricingEntry()}
	}
	targetPriced := func() map[string]*domain.PriceEntry {
		return map[string]*domain.PriceEntry{"upstream-b": {
			Model: "upstream-b", Mode: domain.PriceModeToken,
			InputPerM: ptr(int64(1e8)), OutputPerM: ptr(int64(2e8)),
			Source: domain.PricingSourceManual,
		}}
	}
	tests := []struct {
		name       string
		mode       domain.ModelMappingMode
		prices     map[string]*domain.PriceEntry
		wantCode   int
		wantCost   int64  // 200 行：cost 证明结算价目跟走用量身份
		wantMapped string // 402 行：拒绝明细 MappedModel = 当轮用量身份
	}{
		{"implicit 按客户端模型预检+计价", domain.ModelMappingModeImplicit, clientPriced(), 200, 130, ""},
		{"explicit 按目标预检（客户端有价不救）", domain.ModelMappingModeExplicit, clientPriced(), 402, 0, "upstream-b"},
		{"explicit 按目标预检+计价", domain.ModelMappingModeExplicit, targetPriced(), 200, 1300, ""},
		{"implicit 客户端无价 402（目标有价不救）", domain.ModelMappingModeImplicit, targetPriced(), 402, 0, "gpt-4o"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			up := fakeOpenAI(t, "")
			defer up.Close()
			store := &captureLogStore{}
			tpl := mappingTpl(up.URL, map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "upstream-b", Mode: tc.mode}})
			p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, &BillingHooks{
				Resolver: &fakePriceLookup{entries: tc.prices},
				Balances: billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{}}, nil),
			})
			rec := postChat(p, `{"model":"gpt-4o","messages":[]}`)
			require.Equal(t, tc.wantCode, rec.Code, "body=%s", rec.Body.String())
			require.NoError(t, p.rec.Close(context.Background()))
			require.NoError(t, p.errlog.Close(context.Background()))
			store.mu.Lock()
			defer store.mu.Unlock()
			if tc.wantCode == 200 {
				require.Len(t, store.logs, 1, "must capture exactly one usage log")
				require.Equal(t, "gpt-4o", store.logs[0].Model, "Model = 客户端请求模型")
				require.Equal(t, tc.wantCost, store.logs[0].Cost, "结算价目跟走用量身份（implicit=客户端 / explicit=目标）")
			} else {
				require.Len(t, store.logs, 1, "缺价 402 拒绝行进 err_logs（预用量拒绝无 usage_logs 明细）")
				l := store.logs[0]
				require.Equal(t, domain.ErrBilling, l.ErrorType)
				require.Equal(t, "gpt-4o", l.Model, "Model = 客户端请求模型")
				require.Equal(t, tc.wantMapped, l.MappedModel, "402 拒绝行 MappedModel = 当轮选中尝试的用量身份")
			}
		})
	}
}

// TestBillingModelTerminalOutcomes 非 Search 选中尝试的终态逐路径（规格 §3）：
// 4xx 透传、首字节前 499、流中止——每条日志 MappedModel = 当轮选中尝试的
// 用量身份（implicit 回填客户端模型，non-empty）。
func TestBillingModelTerminalOutcomes(t *testing.T) {
	implicitMapping := map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "upstream-b", Mode: domain.ModelMappingModeImplicit}}
	t.Run("4xx 透传", func(t *testing.T) {
		up := fakeOpenAI(t, "400")
		defer up.Close()
		store := &captureLogStore{}
		p := newTestProxyTplTimeoutLogs(t, mappingTpl(up.URL, implicitMapping), 1, true, 30*time.Second, store, nil)
		rec := postChat(p, `{"model":"gpt-4o","messages":[]}`)
		require.Equal(t, 400, rec.Code, "body=%s", rec.Body.String())
		require.NoError(t, p.rec.Close(context.Background()))
		require.NoError(t, p.errlog.Close(context.Background()))
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 1, "must capture exactly one err_logs row")
		l := store.logs[0]
		require.Equal(t, domain.Err4xx, l.ErrorType)
		require.Equal(t, "gpt-4o", l.Model, "Model = 客户端请求模型")
		require.Equal(t, "gpt-4o", l.MappedModel, "4xx 行 MappedModel = 当轮选中尝试的用量身份（implicit=客户端）")
	})
	t.Run("首字节前 499", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(time.Second):
				w.WriteHeader(200)
			case <-r.Context().Done():
			}
		}))
		defer up.Close()
		store := &captureLogStore{}
		p := newTestProxyTplTimeoutLogs(t, mappingTpl(up.URL, implicitMapping), 1, true, 30*time.Second, store, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
			`{"model":"gpt-4o","messages":[]}`)).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer ck-1")
		rec := httptest.NewRecorder()
		time.AfterFunc(100*time.Millisecond, cancel)
		p.HandleChat(rec, req)
		require.NoError(t, p.rec.Close(context.Background()))
		require.NoError(t, p.errlog.Close(context.Background()))
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 2, "499 abort 双轨：usage_logs + err_logs 各一行")
		for i, l := range store.logs {
			require.Equal(t, statusClientClosedRequest, l.StatusCode, "行 %d：首字节前断连记 499", i)
			require.Equal(t, domain.ErrAbort, l.ErrorType, "行 %d：断连记 abort", i)
			require.Equal(t, "gpt-4o", l.Model, "行 %d：Model = 客户端请求模型", i)
			require.Equal(t, "gpt-4o", l.MappedModel, "行 %d：499 行 MappedModel = 当轮选中尝试的用量身份（implicit=客户端）", i)
		}
	})
	t.Run("流中止", func(t *testing.T) {
		up := fakeOpenAI(t, "stall-stream")
		defer up.Close()
		store := &captureLogStore{}
		p := newTestProxyTplTimeoutLogs(t, mappingTpl(up.URL, implicitMapping), 1, true, 100*time.Millisecond, store, nil)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
			`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer ck-1")
		rec := httptest.NewRecorder()
		p.HandleChat(rec, req)
		require.NoError(t, p.rec.Close(context.Background()))
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 1, "must capture exactly one usage log")
		l := store.logs[0]
		require.Equal(t, domain.ErrAbort, l.ErrorType, "停滞超时按上游读失败记 ErrAbort")
		require.Equal(t, "gpt-4o", l.Model, "Model = 客户端请求模型")
		require.Equal(t, "gpt-4o", l.MappedModel, "流中止行 MappedModel = 当轮选中尝试的用量身份（implicit=客户端）")
	})
}

// TestFailoverMappingIdentityFresh failover 跨模式身份新鲜度（规格 §3）：终态
// 日志/计价身份 = 实际终态选中的用量身份，无首轮身份泄漏。
//   - 成功终态：上游 A 恒 429、上游 B 恒 200（账号按模板分指不同上游）——
//     无论洗牌顺序，成功行恒为账号 2（上游 B）的用量身份。
//   - 耗尽终态（同上游恒 429，两账号各试一次，首试账号 429 冷却后转移）：
//     implicit+implicit（目标不同）耗尽行恒为客户端模型（与后试账号无关）；
//     implicit+explicit 混合经上游观察到的首个鉴权 key 判定首试账号——
//     耗尽行身份 = 后试账号（lastSel）的用量身份。
func TestFailoverMappingIdentityFresh(t *testing.T) {
	implicitB := map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "upstream-b", Mode: domain.ModelMappingModeImplicit}}
	implicitD := map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "upstream-d", Mode: domain.ModelMappingModeImplicit}}
	explicitC := map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "upstream-c", Mode: domain.ModelMappingModeExplicit}}
	t.Run("implicit→explicit 成功", func(t *testing.T) {
		up1 := fakeOpenAI(t, "429")
		defer up1.Close()
		up2 := fakeOpenAI(t, "")
		defer up2.Close()
		store := &captureLogStore{}
		p := newTestProxyTplTimeoutLogs(t, mappingTpl(up1.URL, implicitB), 1, true, 30*time.Second, store, nil)
		tpl2 := mappingTpl(up2.URL, explicitC)
		tpl2.ID = 2
		acc2 := &domain.Account{ID: 2, TemplateID: 2, Template: tpl2, UpstreamKey: "sk-upstream", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4}
		p.sched.Loader().(noopLoader).accs[10] = append(p.sched.Loader().(noopLoader).accs[10], acc2)
		require.NoError(t, p.sched.InvalidateAllSync())

		rec := postChat(p, `{"model":"gpt-4o","messages":[]}`)
		require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
		require.NoError(t, p.rec.Close(context.Background()))
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 1, "must capture exactly one usage log")
		require.Equal(t, "gpt-4o", store.logs[0].Model, "Model = 客户端请求模型")
		require.Equal(t, "upstream-c", store.logs[0].MappedModel, "成功行身份 = 终态选中账号（explicit=目标），非首轮 implicit 身份")
	})
	t.Run("explicit→implicit 成功", func(t *testing.T) {
		up1 := fakeOpenAI(t, "429")
		defer up1.Close()
		up2 := fakeOpenAI(t, "")
		defer up2.Close()
		store := &captureLogStore{}
		p := newTestProxyTplTimeoutLogs(t, mappingTpl(up1.URL, explicitC), 1, true, 30*time.Second, store, nil)
		tpl2 := mappingTpl(up2.URL, implicitB)
		tpl2.ID = 2
		acc2 := &domain.Account{ID: 2, TemplateID: 2, Template: tpl2, UpstreamKey: "sk-upstream", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4}
		p.sched.Loader().(noopLoader).accs[10] = append(p.sched.Loader().(noopLoader).accs[10], acc2)
		require.NoError(t, p.sched.InvalidateAllSync())

		rec := postChat(p, `{"model":"gpt-4o","messages":[]}`)
		require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
		require.NoError(t, p.rec.Close(context.Background()))
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 1, "must capture exactly one usage log")
		require.Equal(t, "gpt-4o", store.logs[0].Model, "Model = 客户端请求模型")
		require.Equal(t, "gpt-4o", store.logs[0].MappedModel, "成功行身份 = 终态选中账号（implicit=客户端），非首轮 explicit 目标")
	})
	t.Run("implicit+implicit 耗尽恒客户端身份", func(t *testing.T) {
		var mu sync.Mutex
		var hits int
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits++
			mu.Unlock()
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
		}))
		defer up.Close()
		store := &captureLogStore{}
		p := newTestProxyTplTimeoutLogs(t, mappingTpl(up.URL, implicitB), 1, true, 30*time.Second, store, nil)
		tpl2 := mappingTpl(up.URL, implicitD)
		tpl2.ID = 2
		acc2 := &domain.Account{ID: 2, TemplateID: 2, Template: tpl2, UpstreamKey: "sk-upstream", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4}
		p.sched.Loader().(noopLoader).accs[10] = append(p.sched.Loader().(noopLoader).accs[10], acc2)
		require.NoError(t, p.sched.InvalidateAllSync())

		rec := postChat(p, `{"model":"gpt-4o","messages":[]}`)
		require.Equal(t, 429, rec.Code, "body=%s", rec.Body.String())
		require.NoError(t, p.rec.Close(context.Background()))
		require.NoError(t, p.errlog.Close(context.Background()))
		mu.Lock()
		require.Equal(t, 2, hits, "两账号各尝试一次")
		mu.Unlock()
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 1, "must capture exactly one err_logs row")
		l := store.logs[0]
		require.Equal(t, domain.Err429, l.ErrorType)
		require.Equal(t, "gpt-4o", l.Model, "Model = 客户端请求模型")
		require.Equal(t, "gpt-4o", l.MappedModel, "耗尽行身份 = 最后一轮 implicit 选中（客户端模型），非任何轮目标")
	})
	t.Run("implicit+explicit 耗尽无泄漏", func(t *testing.T) {
		var mu sync.Mutex
		var firstAuth string
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			if firstAuth == "" {
				firstAuth = r.Header.Get("Authorization")
			}
			mu.Unlock()
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
		}))
		defer up.Close()
		store := &captureLogStore{}
		tpl1 := mappingTpl(up.URL, implicitB)
		tpl1.CredentialType = credential.TypeAPIKey
		p := newTestProxyTplTimeoutLogs(t, tpl1, 1, true, 30*time.Second, store, nil)
		// 两账号独立上游 key——上游观察到的首个鉴权头判定首试账号。
		p.sched.Loader().(noopLoader).accs[10][0].UpstreamKey = "sk-a1"
		tpl2 := mappingTpl(up.URL, explicitC)
		tpl2.ID = 2
		acc2 := &domain.Account{ID: 2, TemplateID: 2, Template: tpl2, UpstreamKey: "sk-a2", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4}
		p.sched.Loader().(noopLoader).accs[10] = append(p.sched.Loader().(noopLoader).accs[10], acc2)
		require.NoError(t, p.sched.InvalidateAllSync())

		rec := postChat(p, `{"model":"gpt-4o","messages":[]}`)
		require.Equal(t, 429, rec.Code, "body=%s", rec.Body.String())
		require.NoError(t, p.rec.Close(context.Background()))
		require.NoError(t, p.errlog.Close(context.Background()))
		mu.Lock()
		first := firstAuth
		mu.Unlock()
		require.Contains(t, []string{"Bearer sk-a1", "Bearer sk-a2"}, first, "首试账号可观察")
		// 首试账号 429 冷却 → 转移后试另一账号；耗尽行 = 后试账号（lastSel）身份。
		want := "gpt-4o"
		if first == "Bearer sk-a1" {
			want = "upstream-c" // 后试 = explicit 账号 → 目标模型
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 1, "must capture exactly one err_logs row")
		l := store.logs[0]
		require.Equal(t, domain.Err429, l.ErrorType)
		require.Equal(t, "gpt-4o", l.Model, "Model = 客户端请求模型")
		require.Equal(t, want, l.MappedModel, "耗尽行身份 = 后试账号（lastSel）的用量身份，无首轮身份泄漏")
	})
}

// TestSearchMappedModelPreserved Search 身份不变量（规格 §3/§5）：Search 终态
// 日志保持既有 mappedFor(reqModel, sel.Model) 语义与固定按次计费——implicit
// 行仍记映射目标（非客户端模型）、identity 行仍空，4xx/耗尽同理；不触达
// Selection 身份方法。
func TestSearchMappedModelPreserved(t *testing.T) {
	t.Run("implicit 成功仍记映射目标", func(t *testing.T) {
		up, _ := newCodexSearchUpstream(t, codexSearchStep{status: 200, body: searchRespRaw})
		store := &captureLogStore{}
		p, _ := newTestSearchProxy(t, []searchTestAcct{{id: 10, tplID: 1, credType: credential.TypeAPIKey, key: "sk-upstream",
			mapping: map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "upstream-b", Mode: domain.ModelMappingModeImplicit}}}},
			up.URL, searchBillingHooks(&fakeFunctionPriceLookup{entries: map[string]*domain.PriceEntry{
				"codex-search": {Model: "codex-search", Mode: domain.PriceModeCall, PricePerCall: i64ptr(2500), Source: domain.PricingSourceManual},
			}}), store)
		srv := httptest.NewServer(AIRouter(p))
		defer srv.Close()
		resp := postSearch(t, srv, searchReqBody, "")
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		require.Equal(t, http.StatusOK, resp.StatusCode, "body=%s", string(b))
		require.NoError(t, p.rec.Close(context.Background()))
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 1)
		lg := store.logs[0]
		require.Equal(t, domain.FormatOpenAISearch, lg.Format)
		require.Equal(t, "gpt-4o", lg.Model, "Model = 客户端请求模型")
		require.Equal(t, "upstream-b", lg.MappedModel, "Search implicit 仍记映射目标（mappedFor 语义，非客户端模型）")
		require.Equal(t, int64(2500), lg.Cost, "固定按次计费不随映射身份变化")
	})
	t.Run("implicit identity 成功 MappedModel 空", func(t *testing.T) {
		up, _ := newCodexSearchUpstream(t, codexSearchStep{status: 200, body: searchRespRaw})
		store := &captureLogStore{}
		p, _ := newTestSearchProxy(t, []searchTestAcct{{id: 10, tplID: 1, credType: credential.TypeAPIKey, key: "sk-upstream",
			mapping: map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "gpt-4o", Mode: domain.ModelMappingModeImplicit}}}},
			up.URL, searchBillingHooks(nil), store)
		srv := httptest.NewServer(AIRouter(p))
		defer srv.Close()
		resp := postSearch(t, srv, searchReqBody, "")
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.NoError(t, p.rec.Close(context.Background()))
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 1)
		require.Equal(t, "gpt-4o", store.logs[0].Model)
		require.Equal(t, "", store.logs[0].MappedModel, "Search identity 仍空（mappedFor 相等语义，非客户端非空）")
	})
	t.Run("implicit 4xx 保持既有", func(t *testing.T) {
		up, _ := newCodexSearchUpstream(t, codexSearchStep{status: 403, body: `{"detail":"Forbidden"}`})
		store := &captureLogStore{}
		p, _ := newTestSearchProxy(t, []searchTestAcct{{id: 10, tplID: 1, credType: credential.TypeAPIKey, key: "sk-upstream",
			mapping: map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "upstream-b", Mode: domain.ModelMappingModeImplicit}}}},
			up.URL, searchBillingHooks(nil), store)
		srv := httptest.NewServer(AIRouter(p))
		defer srv.Close()
		resp := postSearch(t, srv, searchReqBody, "")
		defer resp.Body.Close()
		require.Equal(t, http.StatusBadGateway, resp.StatusCode)
		waitStoreLogs(t, store, 1)
		store.mu.Lock()
		defer store.mu.Unlock()
		lg := store.logs[0]
		require.Equal(t, domain.Err4xx, lg.ErrorType)
		require.Equal(t, "gpt-4o", lg.Model, "Model = 客户端请求模型")
		require.Equal(t, "upstream-b", lg.MappedModel, "Search 4xx 仍记映射目标（mappedFor 语义）")
	})
	t.Run("implicit 耗尽保持既有", func(t *testing.T) {
		up, _ := newCodexSearchUpstream(t, codexSearchStep{status: 429, body: `{"error":{"message":"rate limited"}}`})
		store := &captureLogStore{}
		p, _ := newTestSearchProxy(t, []searchTestAcct{{id: 10, tplID: 1, credType: credential.TypeAPIKey, key: "sk-upstream",
			mapping: map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "upstream-b", Mode: domain.ModelMappingModeImplicit}}}},
			up.URL, searchBillingHooks(nil), store)
		srv := httptest.NewServer(AIRouter(p))
		defer srv.Close()
		resp := postSearch(t, srv, searchReqBody, "")
		defer resp.Body.Close()
		require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
		waitStoreLogs(t, store, 1)
		store.mu.Lock()
		defer store.mu.Unlock()
		lg := store.logs[0]
		require.Equal(t, domain.Err429, lg.ErrorType)
		require.Equal(t, "gpt-4o", lg.Model, "Model = 客户端请求模型")
		require.Equal(t, "upstream-b", lg.MappedModel, "Search 耗尽行仍记映射目标（mappedFor 语义，非客户端模型）")
	})
}
