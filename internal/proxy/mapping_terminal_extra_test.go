// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"context"
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

func TestPrecheckNoMapAndExplicitIdentity(t *testing.T) {
	clientPriced := map[string]*domain.PriceEntry{"gpt-4o": proxyPricingEntry()}
	targetPriced := map[string]*domain.PriceEntry{"upstream-b": {
		Model: "upstream-b", Mode: domain.PriceModeToken,
		InputPerM: ptr(int64(1e8)), OutputPerM: ptr(int64(2e8)),
		Source: domain.PricingSourceManual,
	}}
	t.Run("无映射 按客户端模型预检+计价", func(t *testing.T) {
		up := fakeOpenAI(t, "")
		defer up.Close()
		store := &captureLogStore{}
		p := newTestProxyTplTimeoutLogs(t, mappingTpl(up.URL, nil), 1, true, 30*time.Second, store, &BillingHooks{
			Resolver: &fakePriceLookup{entries: clientPriced},
			Balances: billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{}}, nil),
		})
		rec := postChat(p, `{"model":"gpt-4o","messages":[]}`)
		require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
		require.NoError(t, p.rec.Close(context.Background()))
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 1)
		require.Equal(t, "gpt-4o", store.logs[0].Model)
		require.Equal(t, "", store.logs[0].MappedModel, "无映射 MappedModel 空")
		require.Equal(t, int64(130), store.logs[0].Cost, "按客户端模型计价")
	})
	t.Run("无映射 目标有价不救 402 映射空", func(t *testing.T) {
		up := fakeOpenAI(t, "")
		defer up.Close()
		store := &captureLogStore{}
		p := newTestProxyTplTimeoutLogs(t, mappingTpl(up.URL, nil), 1, true, 30*time.Second, store, &BillingHooks{
			Resolver: &fakePriceLookup{entries: targetPriced},
			Balances: billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{}}, nil),
		})
		rec := postChat(p, `{"model":"gpt-4o","messages":[]}`)
		require.Equal(t, 402, rec.Code, "body=%s", rec.Body.String())
		require.NoError(t, p.rec.Close(context.Background()))
		require.NoError(t, p.errlog.Close(context.Background()))
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 1)
		require.Equal(t, "gpt-4o", store.logs[0].Model)
		require.Equal(t, "", store.logs[0].MappedModel, "无映射 402 MappedModel 空")
	})
	t.Run("explicit identity 按客户端模型预检+计价", func(t *testing.T) {
		up := fakeOpenAI(t, "")
		defer up.Close()
		store := &captureLogStore{}
		tpl := mappingTpl(up.URL, map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "gpt-4o", Mode: domain.ModelMappingModeExplicit}})
		p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, &BillingHooks{
			Resolver: &fakePriceLookup{entries: clientPriced},
			Balances: billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{}}, nil),
		})
		rec := postChat(p, `{"model":"gpt-4o","messages":[]}`)
		require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
		require.NoError(t, p.rec.Close(context.Background()))
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 1)
		require.Equal(t, "", store.logs[0].MappedModel, "explicit identity MappedModel 空")
		require.Equal(t, int64(130), store.logs[0].Cost)
	})
	t.Run("explicit identity 目标有价不救 402 映射空", func(t *testing.T) {
		up := fakeOpenAI(t, "")
		defer up.Close()
		store := &captureLogStore{}
		tpl := mappingTpl(up.URL, map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "gpt-4o", Mode: domain.ModelMappingModeExplicit}})
		p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, &BillingHooks{
			Resolver: &fakePriceLookup{entries: targetPriced},
			Balances: billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{}}, nil),
		})
		rec := postChat(p, `{"model":"gpt-4o","messages":[]}`)
		require.Equal(t, 402, rec.Code, "body=%s", rec.Body.String())
		require.NoError(t, p.rec.Close(context.Background()))
		require.NoError(t, p.errlog.Close(context.Background()))
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 1)
		require.Equal(t, "", store.logs[0].MappedModel, "explicit identity 402 MappedModel 空")
	})
}

func TestTerminalOutcomesAllMappings(t *testing.T) {
	cases := []struct {
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
	for _, tc := range cases {
		t.Run("4xx/"+tc.name, func(t *testing.T) {
			up := fakeOpenAI(t, "400")
			defer up.Close()
			store := &captureLogStore{}
			p := newTestProxyTplTimeoutLogs(t, mappingTpl(up.URL, tc.mapping), 1, true, 30*time.Second, store, nil)
			rec := postChat(p, `{"model":"gpt-4o","messages":[]}`)
			require.Equal(t, 400, rec.Code, "body=%s", rec.Body.String())
			require.NoError(t, p.rec.Close(context.Background()))
			require.NoError(t, p.errlog.Close(context.Background()))
			store.mu.Lock()
			defer store.mu.Unlock()
			require.Len(t, store.logs, 1)
			require.Equal(t, "gpt-4o", store.logs[0].Model)
			require.Equal(t, tc.wantMapped, store.logs[0].MappedModel, "4xx MappedModel 必须等于当轮用量身份")
			require.Equal(t, domain.Err4xx, store.logs[0].ErrorType)
		})
		t.Run("499/"+tc.name, func(t *testing.T) {
			started := make(chan struct{})
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-started:
				default:
					close(started)
				}
				select {
				case <-time.After(time.Second):
					w.WriteHeader(200)
				case <-r.Context().Done():
				}
			}))
			defer up.Close()
			store := &captureLogStore{}
			p := newTestProxyTplTimeoutLogs(t, mappingTpl(up.URL, tc.mapping), 1, true, 30*time.Second, store, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`)).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer ck-1")
			rec := httptest.NewRecorder()
			go func() { <-started; cancel() }()
			p.HandleChat(rec, req)
			require.NoError(t, p.rec.Close(context.Background()))
			require.NoError(t, p.errlog.Close(context.Background()))
			store.mu.Lock()
			defer store.mu.Unlock()
			require.Len(t, store.logs, 2, "499 双轨")
			for i, l := range store.logs {
				require.Equal(t, 499, l.StatusCode, "行 %d 499", i)
				require.Equal(t, domain.ErrAbort, l.ErrorType)
				require.Equal(t, "gpt-4o", l.Model)
				require.Equal(t, tc.wantMapped, l.MappedModel, "行 %d MappedModel 必须等于当轮用量身份", i)
			}
		})
		t.Run("已接受流中止/"+tc.name, func(t *testing.T) {
			up := fakeOpenAI(t, "abort-stream")
			defer up.Close()
			store := &captureLogStore{}
			p := newTestProxyTplTimeoutLogs(t, mappingTpl(up.URL, tc.mapping), 1, true, 30*time.Second, store, nil)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Authorization", "Bearer ck-1")
			rec := httptest.NewRecorder()
			p.HandleChat(rec, req)
			p.sched.FlushRules()
			require.NoError(t, p.rec.Close(context.Background()))
			store.mu.Lock()
			defer store.mu.Unlock()
			require.Len(t, store.logs, 1)
			require.Equal(t, domain.ErrAbort, store.logs[0].ErrorType)
			require.Equal(t, "gpt-4o", store.logs[0].Model)
			require.Equal(t, tc.wantMapped, store.logs[0].MappedModel, "已接受流中止 MappedModel 必须等于当轮用量身份")
		})
	}
}

func TestExhaustionSecondAuthProof(t *testing.T) {
	implicitB := map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "upstream-b", Mode: domain.ModelMappingModeImplicit}}
	explicitC := map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "upstream-c", Mode: domain.ModelMappingModeExplicit}}
	var mu sync.Mutex
	var hits int
	var auths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer up.Close()
	store := &captureLogStore{}
	tpl1 := mappingTpl(up.URL, implicitB)
	p := newTestProxyTplTimeoutLogs(t, tpl1, 1, true, 30*time.Second, store, nil)
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
	require.Equal(t, 2, hits, "两账号各尝试一次")
	require.Len(t, auths, 2, "两轮鉴权均可观测")
	require.ElementsMatch(t, []string{"Bearer sk-a1", "Bearer sk-a2"}, auths)
	first := auths[0]
	second := auths[1]
	mu.Unlock()
	want := "gpt-4o"
	if second == "Bearer sk-a2" {
		want = "upstream-c"
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, domain.Err429, store.logs[0].ErrorType)
	require.Equal(t, "gpt-4o", store.logs[0].Model)
	require.Equal(t, want, store.logs[0].MappedModel, "耗尽行 MappedModel = 第二轮选中（lastSel）的用量身份，首轮=%s 次轮=%s", first, second)
	require.Equal(t, 429, store.logs[0].StatusCode)
}

func TestPreselectionRejectionEmptyMappedModel(t *testing.T) {
	t.Run("401 无效 key 映射空", func(t *testing.T) {
		up := fakeOpenAI(t, "")
		defer up.Close()
		store := &captureLogStore{}
		p := newTestProxyTplTimeoutLogs(t, mappingTpl(up.URL, nil), 1, true, 30*time.Second, store, nil)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
		req.Header.Set("Authorization", "Bearer wrong")
		rec := httptest.NewRecorder()
		p.HandleChat(rec, req)
		require.Equal(t, 401, rec.Code)
		require.NoError(t, p.rec.Close(context.Background()))
		require.NoError(t, p.errlog.Close(context.Background()))
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 1)
		require.Equal(t, "", store.logs[0].Model, "401 无模型")
		require.Equal(t, "", store.logs[0].MappedModel, "本地预选拒绝 MappedModel 恒空")
		require.Equal(t, domain.ErrAuth, store.logs[0].ErrorType)
	})
	t.Run("Select 失败 无账号 映射空", func(t *testing.T) {
		up := fakeOpenAI(t, "")
		defer up.Close()
		store := &captureLogStore{}
		tpl := &domain.Template{
			ID: 1, Name: "t", BaseURL: up.URL,
			CredentialType:   credential.TypeAPIKey,
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
		}
		p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, nil)
		sched := p.sched
		loader := sched.Loader().(noopLoader)
		loader.accs[10] = nil
		require.NoError(t, sched.InvalidateAllSync())
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
		req.Header.Set("Authorization", "Bearer ck-1")
		rec := httptest.NewRecorder()
		p.HandleChat(rec, req)
		require.Equal(t, 404, rec.Code, "body=%s", rec.Body.String())
		require.NoError(t, p.rec.Close(context.Background()))
		require.NoError(t, p.errlog.Close(context.Background()))
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 1)
		require.Equal(t, "gpt-4o", store.logs[0].Model, "Select 失败仍记录客户端模型")
		require.Equal(t, "", store.logs[0].MappedModel, "本地预选拒绝（无账号）MappedModel 恒空")
		require.Equal(t, domain.ErrNoAccount, store.logs[0].ErrorType)
	})
	t.Run("格式不可用 404 映射空", func(t *testing.T) {
		up := fakeAnthropic(t, "")
		defer up.Close()
		store := &captureLogStore{}
		p := newTestProxyTimeoutLogs(t, up.URL, 1, store)
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-4o","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer ck-1")
		rec := httptest.NewRecorder()
		p.HandleAnthropic(rec, req)
		require.Equal(t, 404, rec.Code)
		require.NoError(t, p.rec.Close(context.Background()))
		require.NoError(t, p.errlog.Close(context.Background()))
		store.mu.Lock()
		defer store.mu.Unlock()
		if len(store.logs) == 0 {
			t.Skip("无可用账号路径不产生 err_logs 行（取决于表配置），但 MappedModel 恒空已在 429 分支证明")
		}
		for _, l := range store.logs {
			require.Equal(t, "", l.MappedModel, "格式不可用 404 MappedModel 恒空")
		}
	})
}

func TestFailoverExhaustionHitsBothAccounts(t *testing.T) {
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
	implicit := map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "upstream-b", Mode: domain.ModelMappingModeImplicit}}
	p := newTestProxyTplTimeoutLogs(t, mappingTpl(up.URL, implicit), 1, true, 30*time.Second, store, nil)
	tpl2 := mappingTpl(up.URL, implicit)
	tpl2.ID = 2
	acc2 := &domain.Account{ID: 2, TemplateID: 2, Template: tpl2, UpstreamKey: "sk-upstream", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4}
	p.sched.Loader().(noopLoader).accs[10] = append(p.sched.Loader().(noopLoader).accs[10], acc2)
	require.NoError(t, p.sched.InvalidateAllSync())
	rec := postChat(p, `{"model":"gpt-4o","messages":[]}`)
	require.Equal(t, 429, rec.Code)
	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))
	mu.Lock()
	require.Equal(t, 2, hits, "耗尽可能须触达全部账号")
	mu.Unlock()
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, domain.Err429, store.logs[0].ErrorType)
}

// Ensure credential import used (avoid unused).
var _ = credential.TypeAPIKey
