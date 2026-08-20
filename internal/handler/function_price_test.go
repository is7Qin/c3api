// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/pricing"
	"github.com/is7qin/c3api/internal/repository"
)

// repositoryFunctionManual 构造 repository.FunctionPriceManual（毫分/次）。
func repositoryFunctionManual(model string, perCall int64) *repository.FunctionPriceManual {
	return &repository.FunctionPriceManual{Model: model, PricePerCall: i64p(perCall)}
}

// functionSeedFetcher 构造带按单元价行的拉取结果（价格表三件套 seed；litellm
// 行带 provider——litellm 数据直贴）。
func functionSeedFetcher() *fakePriceFetcher {
	return &fakePriceFetcher{res: &pricing.FetchResult{
		Rows: []*domain.Pricing{
			{Model: "gpt-4o", PromptPricePerMillion: 250000, CompletionPricePerMillion: 1000000, Source: domain.PricingSourceLitellm},
		},
		FunctionRows: []*domain.FunctionPrice{
			{Model: "search-alpha", PricePerCall: i64p(10), Provider: strPtr("openai"), Source: domain.PricingSourceLitellm},
		},
	}}
}

// TestPutFunctionPricesModel 手动设按单元价：单位换算（USD/次 ×1e5 存储毫分/
// 次、回显反向换算）+ 接管 litellm 行 + codex-search 可改 + 校验（缺省 → 400、
// 负数 → 400、非法 JSON → 400）。
func TestPutFunctionPricesModel(t *testing.T) {
	f := functionSeedFetcher()
	h, do := newPricingRouter(t, f)

	// 新模型设价：0.01 USD/次 → 1000 毫分/次；回显反向换算
	rec := do(http.MethodPut, "/api/admin/function-prices?model=search-beta", `{"price_per_call":0.01}`)
	require.Equal(t, 200, rec.Code, "put: %s", rec.Body.String())
	var p FunctionPrice
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	require.Equal(t, "search-beta", p.Model)
	require.NotNil(t, p.PricePerCall)
	require.Equal(t, 0.01, *p.PricePerCall, "回显 USD/次")
	require.Equal(t, PricingSource("manual"), p.Source)

	// 接管 litellm 行：先 sync 入库再手动设价（litellm 行带 provider → 接管后清空）
	_, err := h.svc.SyncPricingNow(context.Background())
	require.NoError(t, err)
	rec = do(http.MethodPut, "/api/admin/function-prices?model=search-alpha", `{"price_per_call":0.005}`)
	require.Equal(t, 200, rec.Code, "takeover: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	require.Equal(t, PricingSource("manual"), p.Source, "手动设价接管 litellm 行")
	require.Equal(t, 0.005, *p.PricePerCall)
	require.Nil(t, p.Provider, "manual 接管后 provider nil（S-2）")

	// codex-search 价可管理端改
	rec = do(http.MethodPut, "/api/admin/function-prices?model=codex-search", `{"price_per_call":0.02}`)
	require.Equal(t, 200, rec.Code, "codex-search upsert: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	require.Equal(t, PricingSource("manual"), p.Source)
	require.Equal(t, 0.02, *p.PricePerCall, "codex-search 改价生效")

	// 缺省 price_per_call → 400（契约 required）
	rec = do(http.MethodPut, "/api/admin/function-prices?model=m-none", `{}`)
	require.Equal(t, 400, rec.Code, "missing price_per_call: %s", rec.Body.String())
	rec = do(http.MethodPut, "/api/admin/function-prices?model=m-none", `{"price_per_call":null}`)
	require.Equal(t, 400, rec.Code, "null price_per_call: %s", rec.Body.String())

	// 负数 → 400
	rec = do(http.MethodPut, "/api/admin/function-prices?model=m-neg", `{"price_per_call":-0.01}`)
	require.Equal(t, 400, rec.Code, "negative: %s", rec.Body.String())

	// 非法 JSON → 400
	rec = do(http.MethodPut, "/api/admin/function-prices?model=m-bad", `not json`)
	require.Equal(t, 400, rec.Code, "invalid json: %s", rec.Body.String())
}

// TestFunctionPricesList 列表：全量 / source 筛选 / model 模糊 / 分页 /
// 非法参数 400。
func TestFunctionPricesList(t *testing.T) {
	f := functionSeedFetcher()
	h, do := newPricingRouter(t, f)
	_, err := h.svc.SyncPricingNow(context.Background())
	require.NoError(t, err)
	_, err = h.svc.UpsertManualFunctionPrice(context.Background(), repositoryFunctionManual("codex-search", 1000))
	require.NoError(t, err)

	rec := do(http.MethodGet, "/api/admin/function-prices", "")
	require.Equal(t, 200, rec.Code, "list all: %s", rec.Body.String())
	var list FunctionPriceListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(2), list.Total)
	require.Len(t, list.Rows, 2)

	rec = do(http.MethodGet, "/api/admin/function-prices?source=litellm", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(1), list.Total)
	require.Equal(t, PricingSource("litellm"), list.Rows[0].Source)
	require.NotNil(t, list.Rows[0].Provider, "litellm 行回显 provider")
	require.Equal(t, "openai", string(*list.Rows[0].Provider))

	// manual 行 provider nil；provider 等值筛选（命中/不命中/非 enum 可筛）
	rec = do(http.MethodGet, "/api/admin/function-prices?source=manual", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Nil(t, list.Rows[0].Provider, "manual 行 provider nil")
	rec = do(http.MethodGet, "/api/admin/function-prices?provider=openai", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(1), list.Total, "provider 等值筛选命中")
	rec = do(http.MethodGet, "/api/admin/function-prices?provider=bedrock", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Zero(t, list.Total, "provider 不命中 → 空列表")
	rec = do(http.MethodGet, "/api/admin/function-prices?provider=some_future_vendor", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Zero(t, list.Total, "非 enum 自由字符串可筛（DB 自由字符串等值）")

	rec = do(http.MethodGet, "/api/admin/function-prices?model=SEARCH-", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(1), list.Total, "model 模糊（大小写不敏感）")

	rec = do(http.MethodGet, "/api/admin/function-prices?page=2&page_size=1", "")
	require.Equal(t, 200, rec.Code, "pagination: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(2), list.Total)
	require.Len(t, list.Rows, 1, "第二页一行")

	// 非法参数 400
	for _, tc := range []struct{ name, query string }{
		{"bad source", "?source=bogus"},
		{"bad sort", "?sort=bogus"},
		{"page_size out of range", "?page_size=1001"},
		{"bad order", "?order=sideways"},
	} {
		rec := do(http.MethodGet, "/api/admin/function-prices"+tc.query, "")
		require.Equal(t, 400, rec.Code, "%s: %s", tc.name, rec.Body.String())
	}
}

// TestDeleteFunctionPricesModel 删除手动按单元价：litellm 行 → 409、缺失 →
// 404、手动行 → 200（删除后快照消失）。
func TestDeleteFunctionPricesModel(t *testing.T) {
	f := functionSeedFetcher()
	h, do := newPricingRouter(t, f)
	_, err := h.svc.SyncPricingNow(context.Background())
	require.NoError(t, err)
	_, err = h.svc.UpsertManualFunctionPrice(context.Background(), repositoryFunctionManual("search-beta", 1000))
	require.NoError(t, err)

	rec := do(http.MethodDelete, "/api/admin/function-prices?model=search-alpha", "")
	require.Equal(t, 409, rec.Code, "litellm 行 → 409: %s", rec.Body.String())

	rec = do(http.MethodDelete, "/api/admin/function-prices?model=nope", "")
	require.Equal(t, 404, rec.Code, "缺失 → 404")

	rec = do(http.MethodDelete, "/api/admin/function-prices?model=search-beta", "")
	require.Equal(t, 200, rec.Code, "手动行 → 200: %s", rec.Body.String())
	var del DeletedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &del))
	require.True(t, del.Deleted)

	// 删除后列表精确筛选 → 空（单查端点已删，list 筛选覆盖该语义）
	rec = do(http.MethodGet, "/api/admin/function-prices?model=search-beta", "")
	require.Equal(t, 200, rec.Code, "list after delete: %s", rec.Body.String())
	var list FunctionPriceListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Zero(t, list.Total, "删除后列表精确筛选为空")
}

// TestSyncPricingNowFunctionResponse 手动 sync 响应含 function 行统计
// （FunctionRows/FunctionUpdated）。
func TestSyncPricingNowFunctionResponse(t *testing.T) {
	f := functionSeedFetcher()
	_, do := newPricingRouter(t, f)
	rec := do(http.MethodPost, "/api/admin/pricing/sync", "")
	require.Equal(t, 200, rec.Code, "sync: %s", rec.Body.String())
	var stats PricingSyncResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &stats))
	require.Equal(t, 1, stats.Rows)
	require.NotNil(t, stats.FunctionRows)
	require.Equal(t, 1, *stats.FunctionRows)
	require.NotNil(t, stats.FunctionUpdated)
	require.Equal(t, 1, *stats.FunctionUpdated)
}
