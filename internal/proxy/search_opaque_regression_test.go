// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// TestSearchOpaqueMappingRegression 透明 Search 回归（映射已配置时）：
// 覆盖显式/隐式两种模式的 success / 4xx / 耗尽，断言请求体/响应体字节不变、
// 日志 Model/MappedModel/Format、计费固定 codex-search、failover 选号透明。
func TestSearchOpaqueMappingRegression(t *testing.T) {
	mappingExplicit := map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "upstream-b", Mode: domain.ModelMappingModeExplicit}}
	mappingImplicit := map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "upstream-b", Mode: domain.ModelMappingModeImplicit}}

	t.Run("explicit_success_opaque", func(t *testing.T) {
		up, upc := newCodexSearchUpstream(t, codexSearchStep{status: 200, body: searchRespRaw})
		defer up.Close()
		store := &captureLogStore{}
		p, _ := newTestSearchProxy(t, []searchTestAcct{{id: 10, tplID: 1, credType: credential.TypeAPIKey, key: "sk-upstream", mapping: mappingExplicit}},
			up.URL, searchBillingHooks(&fakeFunctionPriceLookup{entries: map[string]*domain.PriceEntry{
				"codex-search": {Model: "codex-search", Mode: domain.PriceModeCall, PricePerCall: i64ptr(2500), Source: domain.PricingSourceManual},
			}}), store)
		srv := httptest.NewServer(AIRouter(p))
		defer srv.Close()
		resp := postSearch(t, srv, searchReqBody, "")
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		require.Equal(t, http.StatusOK, resp.StatusCode, "body=%s", string(body))
		require.Equal(t, searchRespRaw, string(body), "响应字节透明直回")
		require.Equal(t, 1, upc.callsN())
		require.Equal(t, searchReqBody, string(upc.body(0)), "上游请求体保持客户端 model，不被映射改写")
		require.Equal(t, "/v1/alpha/search", upc.path(0))
		require.NoError(t, p.rec.Close(context.Background()))
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 1)
		lg := store.logs[0]
		require.Equal(t, domain.FormatOpenAISearch, lg.Format)
		require.Equal(t, "gpt-4o", lg.Model, "日志 Model = 客户端请求模型")
		require.Equal(t, "", lg.MappedModel, "explicit 透明：MappedModel 为空（sel==req，故 mappedFor 空）")
		require.Equal(t, int64(1), lg.CallCount)
		require.NotNil(t, lg.PricePerCallMillis)
		require.Equal(t, int64(2500), *lg.PricePerCallMillis)
		require.Equal(t, int64(2500), lg.Cost, "固定按次计费 2500")
		require.Equal(t, domain.ErrNone, lg.ErrorType)
		require.Zero(t, lg.InputTokens)
	})

	t.Run("implicit_success_opaque", func(t *testing.T) {
		up, upc := newCodexSearchUpstream(t, codexSearchStep{status: 200, body: searchRespRaw})
		defer up.Close()
		store := &captureLogStore{}
		p, _ := newTestSearchProxy(t, []searchTestAcct{{id: 10, tplID: 1, credType: credential.TypeAPIKey, key: "sk-upstream", mapping: mappingImplicit}},
			up.URL, searchBillingHooks(&fakeFunctionPriceLookup{entries: map[string]*domain.PriceEntry{
				"codex-search": {Model: "codex-search", Mode: domain.PriceModeCall, PricePerCall: i64ptr(1800), Source: domain.PricingSourceManual},
			}}), store)
		srv := httptest.NewServer(AIRouter(p))
		defer srv.Close()
		resp := postSearch(t, srv, searchReqBody, "")
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		require.Equal(t, http.StatusOK, resp.StatusCode, "body=%s", string(body))
		require.Equal(t, searchRespRaw, string(body))
		require.Equal(t, searchReqBody, string(upc.body(0)), "implicit 同样透明，不改写 model")
		require.NoError(t, p.rec.Close(context.Background()))
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 1)
		lg := store.logs[0]
		require.Equal(t, "gpt-4o", lg.Model)
		require.Equal(t, "", lg.MappedModel, "implicit 透明：MappedModel 为空（不回填客户端模型）")
		require.Equal(t, int64(1800), *lg.PricePerCallMillis)
		require.Equal(t, int64(1800), lg.Cost)
	})

	t.Run("explicit_4xx_opaque", func(t *testing.T) {
		up, upc := newCodexSearchUpstream(t, codexSearchStep{status: 403, body: `{"detail":"Forbidden"}`})
		defer up.Close()
		store := &captureLogStore{}
		p, _ := newTestSearchProxy(t, []searchTestAcct{{id: 10, tplID: 1, credType: credential.TypeAPIKey, key: "sk-upstream", mapping: mappingExplicit}},
			up.URL, searchBillingHooks(nil), store)
		srv := httptest.NewServer(AIRouter(p))
		defer srv.Close()
		resp := postSearch(t, srv, searchReqBody, "")
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		require.Equal(t, http.StatusBadGateway, resp.StatusCode, "4xx 归一 502，body=%s", string(body))
		require.Contains(t, string(body), "upstream rejected request")
		require.Equal(t, 1, upc.callsN(), "4xx 不转移")
		require.Equal(t, searchReqBody, string(upc.body(0)), "4xx 请求体仍透明")
		waitStoreLogs(t, store, 1)
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 1)
		lg := store.logs[0]
		require.Equal(t, domain.FormatOpenAISearch, lg.Format)
		require.Equal(t, "gpt-4o", lg.Model)
		require.Equal(t, "", lg.MappedModel)
		require.Equal(t, domain.Err4xx, lg.ErrorType)
		require.Equal(t, 403, lg.StatusCode)
		require.Zero(t, lg.CallCount)
		require.Zero(t, lg.Cost, "4xx 不计费")
		ri, ok := p.sched.Runtime(10)
		require.True(t, ok)
		require.Zero(t, ri.Concurrency, "4xx 释放并发槽")
		require.Equal(t, domain.StatusActive, ri.Status, "4xx 不冷却")
	})

	t.Run("implicit_4xx_opaque", func(t *testing.T) {
		up, upc := newCodexSearchUpstream(t, codexSearchStep{status: 422, body: `{"error":{"message":"bad"}}`})
		defer up.Close()
		store := &captureLogStore{}
		p, _ := newTestSearchProxy(t, []searchTestAcct{{id: 10, tplID: 1, credType: credential.TypeAPIKey, key: "sk-upstream", mapping: mappingImplicit}},
			up.URL, searchBillingHooks(nil), store)
		srv := httptest.NewServer(AIRouter(p))
		defer srv.Close()
		resp := postSearch(t, srv, searchReqBody, "")
		defer resp.Body.Close()
		require.Equal(t, http.StatusBadGateway, resp.StatusCode)
		require.Equal(t, searchReqBody, string(upc.body(0)))
		waitStoreLogs(t, store, 1)
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Equal(t, "", store.logs[0].MappedModel)
		require.Zero(t, store.logs[0].Cost)
	})

	t.Run("exhaustion_opaque_two_attempts", func(t *testing.T) {
		up, upc := newCodexSearchUpstream(t,
			codexSearchStep{status: 500, body: `{"error":{"message":"boom"}}`},
			codexSearchStep{status: 500, body: `{"error":{"message":"boom2"}}`},
		)
		defer up.Close()
		store := &captureLogStore{}
		p, _ := newTestSearchProxy(t, []searchTestAcct{
			{id: 10, tplID: 1, credType: credential.TypeAPIKey, key: "sk-upstream", mapping: mappingImplicit},
			{id: 20, tplID: 2, credType: credential.TypeAPIKey, key: "sk-upstream2", mapping: mappingImplicit},
		}, up.URL, searchBillingHooks(nil), store)
		srv := httptest.NewServer(AIRouter(p))
		defer srv.Close()
		resp := postSearch(t, srv, searchReqBody, "")
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		require.Equal(t, http.StatusBadGateway, resp.StatusCode, "耗尽归一 502，body=%s", string(body))
		require.Equal(t, 2, upc.callsN(), "500 耗尽应转移至第二账号")
		for i := 0; i < upc.callsN(); i++ {
			require.Equal(t, searchReqBody, string(upc.body(i)), "每轮请求体透明，不被映射改写 %d", i)
		}
		waitStoreLogs(t, store, 1)
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 1)
		lg := store.logs[0]
		require.Equal(t, domain.FormatOpenAISearch, lg.Format)
		require.Equal(t, "gpt-4o", lg.Model)
		require.Equal(t, "", lg.MappedModel, "耗尽行透明 MappedModel 为空")
		require.Equal(t, domain.Err5xx, lg.ErrorType)
		require.Zero(t, lg.CallCount)
		require.Zero(t, lg.Cost)
		for _, id := range []int64{10, 20} {
			ri, ok := p.sched.Runtime(id)
			require.True(t, ok)
			require.Zero(t, ri.Concurrency, "account %d 并发槽释放", id)
		}
	})

	t.Run("codex_path_implicit_opaque", func(t *testing.T) {
		up, upc := newCodexSearchUpstream(t, codexSearchStep{status: 200, body: searchRespRaw})
		defer up.Close()
		store := &captureLogStore{}
		p, _ := newTestSearchProxy(t, []searchTestAcct{{id: 10, tplID: 1, credType: credential.TypeCodexPAT, ext: codexPATExt(10, "pat-10"), mapping: mappingImplicit}},
			up.URL, searchBillingHooks(nil), store)
		srv := httptest.NewServer(AIRouter(p))
		defer srv.Close()
		resp := postSearch(t, srv, searchReqBody, "turn-123")
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		require.Equal(t, http.StatusOK, resp.StatusCode, "body=%s", string(body))
		require.Equal(t, searchRespRaw, string(body), "SDK 路径响应透明")
		require.Equal(t, 1, upc.callsN())
		require.Equal(t, "/backend-api/codex/alpha/search", upc.path(0))
		require.Equal(t, "Bearer pat-10", upc.auth(0))
		require.Equal(t, "", upc.turn(0), "x-codex-turn-metadata 不转发")
		require.Equal(t, searchReqBody, string(upc.body(0)), "SDK 路径请求体透明")
		require.NoError(t, p.rec.Close(context.Background()))
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 1)
		require.Equal(t, "", store.logs[0].MappedModel, "codex 路径同样透明")
		require.Equal(t, int64(1), store.logs[0].CallCount)
		require.Equal(t, int64(1000), store.logs[0].Cost, "codex-search 默认按次价 1000")
	})

	t.Run("billing_fixed_codex_search_never_uses_mapped_price", func(t *testing.T) {
		// 显式定价若随映射身份变化，search 会错误使用 upstream-b 的 token 价；
		// 透明 search 恒按 codex-search 按次价，不受映射影响。
		up, upc := newCodexSearchUpstream(t, codexSearchStep{status: 200, body: searchRespRaw})
		defer up.Close()
		store := &captureLogStore{}
		// 定价表仅为 upstream-b 配高 token 价，若 search 误用映射身份计费会受其影响
		fake := &fakeFunctionPriceLookup{entries: map[string]*domain.PriceEntry{
			"upstream-b": {Model: "upstream-b", Mode: domain.PriceModeToken, InputPerM: i64ptr(9e9), OutputPerM: i64ptr(9e9), Source: domain.PricingSourceManual},
			"codex-search": {Model: "codex-search", Mode: domain.PriceModeCall, PricePerCall: i64ptr(1234), Source: domain.PricingSourceManual},
		}}
		p, _ := newTestSearchProxy(t, []searchTestAcct{{id: 10, tplID: 1, credType: credential.TypeAPIKey, key: "sk-upstream", mapping: mappingImplicit}},
			up.URL, searchBillingHooks(fake), store)
		srv := httptest.NewServer(AIRouter(p))
		defer srv.Close()
		resp := postSearch(t, srv, searchReqBody, "")
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, searchReqBody, string(upc.body(0)))
		require.NoError(t, p.rec.Close(context.Background()))
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 1)
		require.Equal(t, int64(1234), *store.logs[0].PricePerCallMillis, "search 恒取 codex-search 按次价，不取 upstream-b token 价")
		require.Equal(t, int64(1234), store.logs[0].Cost)
	})

	t.Run("request_bytes_never_rewritten_with_mapping", func(t *testing.T) {
		// 额外防回归：搜索体含特殊 model 别名，确认上游收到的 raw JSON 与客户端完全一致（键序/空白不归一）
		raw := `{"model":"gpt-4o","query":"hi","extra":123}`
		up, upc := newCodexSearchUpstream(t, codexSearchStep{status: 200, body: `{"results":[]}`})
		defer up.Close()
		store := &captureLogStore{}
		p, _ := newTestSearchProxy(t, []searchTestAcct{{id: 10, tplID: 1, credType: credential.TypeAPIKey, key: "sk-upstream", mapping: mappingImplicit}},
			up.URL, searchBillingHooks(nil), store)
		srv := httptest.NewServer(AIRouter(p))
		defer srv.Close()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/alpha/search", strings.NewReader(raw))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer ck-1")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		require.Equal(t, http.StatusOK, resp.StatusCode, "body=%s", string(b))
		require.Equal(t, raw, string(upc.body(0)), "search 请求字节全透明，直达上游")
		require.Equal(t, `{"results":[]}`, strings.TrimSpace(string(b)), "响应字节透明直回")
	})
}
