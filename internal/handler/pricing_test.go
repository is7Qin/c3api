// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/pricing"
	"github.com/is7qin/c3api/internal/repository"
)

// manualReq 构造 PricingManual（可选 cache 价；矩阵字段走显式设置）。
func manualReq(model string, prompt, completion int64, cache ...*int64) *repository.PricingManual {
	m := &repository.PricingManual{
		Model: model, PromptPricePerMillion: prompt, CompletionPricePerMillion: completion,
	}
	if len(cache) >= 1 {
		m.CacheReadPricePerMillion = cache[0]
	}
	if len(cache) >= 2 {
		m.CacheCreationPricePerMillion = cache[1]
	}
	return m
}

// fakePriceFetcher 测试用价格拉取器（返回注入结果/错误）。
type fakePriceFetcher struct {
	res *pricing.FetchResult
	err error
}

func (f *fakePriceFetcher) Fetch(ctx context.Context, sourceURL string) (*pricing.FetchResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.res, nil
}

// newPricingRouter 价格面测试接线：admin token 中间件 + 契约路由 + 可选 fetcher
// 注入（sync 端点测试用）。
func newPricingRouter(t *testing.T, f pricing.Fetcher) (*AdminAPI, func(method, path, body string) *httptest.ResponseRecorder) {
	t.Helper()
	h, _, do := newListTestRouter(t)
	if f != nil {
		h.svc.SetPriceFetcher(f)
	}
	return h, do
}

// seedPricing 通过 service 公开路径造数：litellm 行走 sync（fake fetcher），
// manual 行走手动设价——与真实数据路径一致。
func seedPricing(t *testing.T, h *AdminAPI, f pricing.Fetcher) {
	t.Helper()
	h.svc.SetPriceFetcher(f)
	_, err := h.svc.SyncPricingNow(context.Background())
	require.NoError(t, err, "seed sync")
	_, err = h.svc.UpsertManualPricing(context.Background(), manualReq("gpt-4o", 100, 300))
	require.NoError(t, err)
	_, err = h.svc.UpsertManualPricing(context.Background(), manualReq("gpt-4o-mini", 50, 150))
	require.NoError(t, err)
}

// TestPricingList 列表：分页（page/page_size）+ source/model 筛选 + 排序 +
// 非法参数 400。
func TestPricingList(t *testing.T) {
	f := &fakePriceFetcher{res: &pricing.FetchResult{Rows: []*domain.Pricing{
		{Model: "claude-3-5-sonnet", PromptPricePerMillion: 300000, CompletionPricePerMillion: 1500000, Provider: strPtr("anthropic"), Source: domain.PricingSourceLitellm},
		{Model: "claude-3-opus", PromptPricePerMillion: 1500000, CompletionPricePerMillion: 7500000, Provider: strPtr("anthropic"), Source: domain.PricingSourceLitellm},
	}, Skipped: 2}}
	h, do := newPricingRouter(t, f)
	seedPricing(t, h, f)

	// 全部：manual 2 + litellm 2
	rec := do(http.MethodGet, "/api/admin/pricing", "")
	require.Equal(t, 200, rec.Code, "list all: %s", rec.Body.String())
	var list PricingListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(4), list.Total)
	require.Len(t, list.Rows, 4)

	// source 筛选
	rec = do(http.MethodGet, "/api/admin/pricing?source=manual", "")
	require.Equal(t, 200, rec.Code, "filter manual: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(2), list.Total)
	for _, p := range list.Rows {
		require.Equal(t, PricingSource("manual"), p.Source)
	}
	rec = do(http.MethodGet, "/api/admin/pricing?source=litellm", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(2), list.Total)
	for _, p := range list.Rows {
		require.Equal(t, PricingSource("litellm"), p.Source)
	}

	// model 模糊搜索（大小写不敏感）
	rec = do(http.MethodGet, "/api/admin/pricing?model=GPT-4O", "")
	require.Equal(t, 200, rec.Code, "model search: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(2), list.Total, "gpt-4o + gpt-4o-mini")

	// litellm 行回显 provider（拉取直贴）；manual 行 nil
	rec = do(http.MethodGet, "/api/admin/pricing?source=litellm", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	for _, p := range list.Rows {
		require.NotNil(t, p.Provider, "litellm 行回显 provider")
		require.Equal(t, "anthropic", string(*p.Provider))
	}
	rec = do(http.MethodGet, "/api/admin/pricing?source=manual", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	for _, p := range list.Rows {
		require.Nil(t, p.Provider, "manual 行 provider nil（无厂商概念）")
	}

	// provider 等值筛选：命中 anthropic → 2 行；不命中 → 0 行；非 enum 可筛
	rec = do(http.MethodGet, "/api/admin/pricing?provider=anthropic", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(2), list.Total, "provider 等值筛选命中（claude 两行）")
	rec = do(http.MethodGet, "/api/admin/pricing?provider=openai", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Zero(t, list.Total, "provider 不命中（manual 行无 provider）")
	rec = do(http.MethodGet, "/api/admin/pricing?provider=some_future_vendor", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Zero(t, list.Total, "非 enum 自由字符串可筛（DB 自由字符串等值）")
	// provider + model 组合筛选
	rec = do(http.MethodGet, "/api/admin/pricing?provider=anthropic&model=claude-3-opus", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(1), list.Total)

	// 排序 + 分页
	rec = do(http.MethodGet, "/api/admin/pricing?sort=model&order=desc", "")
	require.Equal(t, 200, rec.Code, "sort: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, "gpt-4o-mini", list.Rows[0].Model, "model desc 首行")
	require.Equal(t, "claude-3-5-sonnet", list.Rows[len(list.Rows)-1].Model, "model desc 末行")

	rec = do(http.MethodGet, "/api/admin/pricing?page=2&page_size=2", "")
	require.Equal(t, 200, rec.Code, "pagination: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(4), list.Total)
	require.Len(t, list.Rows, 2, "第二页两行")

	// 非法参数 400
	for _, tc := range []struct{ name, query string }{
		{"source 非法", "?source=bogus"},
		{"sort 非法", "?sort=bogus"},
		{"page_size 越界", "?page_size=1001"},
		{"page_size 非数字", "?page_size=abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(http.MethodGet, "/api/admin/pricing"+tc.query, "")
			require.Equal(t, 400, rec.Code, "%s: %s", tc.name, rec.Body.String())
			require.NotEmpty(t, errMsg(t, rec), "must be ErrorResponse JSON")
		})
	}
}

// TestPutPricing 手动设价：新模型成功 / 接管 litellm 行（source → manual）/
// 负数与非法 JSON 400。
func TestPutPricing(t *testing.T) {
	f := &fakePriceFetcher{res: &pricing.FetchResult{Rows: []*domain.Pricing{
		{Model: "gpt-4o", PromptPricePerMillion: 250000, CompletionPricePerMillion: 1000000, Source: domain.PricingSourceLitellm},
	}}}
	h, do := newPricingRouter(t, f)

	// 新模型设价成功（API 输入 USD/1M 正常值 → 存储毫分 → 回显 USD）
	rec := do(http.MethodPut, "/api/admin/pricing?model=claude-3-5-sonnet",
		`{"prompt_price_per_million":3.0,"completion_price_per_million":15.0}`)
	require.Equal(t, 200, rec.Code, "put: %s", rec.Body.String())
	var p Pricing
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	require.Equal(t, "claude-3-5-sonnet", p.Model)
	require.Equal(t, 3.0, p.PromptPricePerMillion)
	require.Equal(t, 15.0, p.CompletionPricePerMillion)
	require.Equal(t, PricingSource("manual"), p.Source)

	// 接管 litellm 行：先同步入库再手动设价
	_, err := h.svc.SyncPricingNow(context.Background())
	require.NoError(t, err)
	rec = do(http.MethodPut, "/api/admin/pricing?model=gpt-4o", `{"prompt_price_per_million":0.00999,"completion_price_per_million":0.00888}`)
	require.Equal(t, 200, rec.Code, "takeover: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	require.Equal(t, PricingSource("manual"), p.Source, "手动设价接管 litellm 行")
	rec = do(http.MethodGet, "/api/admin/pricing?model=gpt-4o&source=manual", "")
	var list PricingListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(1), list.Total, "接管后行 source=manual")

	// 带 cache 价设价：响应与列表 roundtrip
	rec = do(http.MethodPut, "/api/admin/pricing?model=m-cache",
		`{"prompt_price_per_million":0.0001,"completion_price_per_million":0.0002,"cache_read_price_per_million":0.0003,"cache_creation_price_per_million":0.0004}`)
	require.Equal(t, 200, rec.Code, "put with cache: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	require.Equal(t, 0.0003, *p.CacheReadPricePerMillion, "响应含 cache_read")
	require.Equal(t, 0.0004, *p.CacheCreationPricePerMillion, "响应含 cache_creation")
	rec = do(http.MethodGet, "/api/admin/pricing?model=m-cache", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Rows, 1)
	require.Equal(t, 0.0003, *list.Rows[0].CacheReadPricePerMillion, "列表 roundtrip cache_read")
	require.Equal(t, 0.0004, *list.Rows[0].CacheCreationPricePerMillion)

	// 缺省 cache 字段 → nil（不设缓存价）
	rec = do(http.MethodPut, "/api/admin/pricing?model=m-nocache", `{"prompt_price_per_million":0.00001,"completion_price_per_million":0.00002}`)
	require.Equal(t, 200, rec.Code, "put without cache: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	require.Nil(t, p.CacheReadPricePerMillion, "缺省 → nil")
	require.Nil(t, p.CacheCreationPricePerMillion)

	// 负数 400（含 cache 价负数）
	rec = do(http.MethodPut, "/api/admin/pricing?model=m-neg", `{"prompt_price_per_million":-0.01,"completion_price_per_million":0.0001}`)
	require.Equal(t, 400, rec.Code, "negative: %s", rec.Body.String())
	rec = do(http.MethodPut, "/api/admin/pricing?model=m-neg", `{"prompt_price_per_million":0.0001,"completion_price_per_million":-0.01}`)
	require.Equal(t, 400, rec.Code, "negative completion: %s", rec.Body.String())
	rec = do(http.MethodPut, "/api/admin/pricing?model=m-neg",
		`{"prompt_price_per_million":0.0001,"completion_price_per_million":0.0001,"cache_read_price_per_million":-0.01}`)
	require.Equal(t, 400, rec.Code, "negative cache_read: %s", rec.Body.String())
	rec = do(http.MethodPut, "/api/admin/pricing?model=m-neg",
		`{"prompt_price_per_million":0.0001,"completion_price_per_million":0.0001,"cache_creation_price_per_million":-0.01}`)
	require.Equal(t, 400, rec.Code, "negative cache_creation: %s", rec.Body.String())

	// 非法 JSON 400
	rec = do(http.MethodPut, "/api/admin/pricing?model=m-neg", `{`)
	require.Equal(t, 400, rec.Code, "bad json: %s", rec.Body.String())

	// model 缺失（query required）→ 400（生成契约参数绑定）
	rec = do(http.MethodPut, "/api/admin/pricing", `{"prompt_price_per_million":0.001,"completion_price_per_million":0.002}`)
	require.Equal(t, 400, rec.Code, "missing model: %s", rec.Body.String())
}

// TestDeletePricing 删除手动价：成功 200；litellm 行 → 409；不存在 → 404。
func TestDeletePricing(t *testing.T) {
	f := &fakePriceFetcher{res: &pricing.FetchResult{Rows: []*domain.Pricing{
		{Model: "claude-3-5-sonnet", PromptPricePerMillion: 300000, CompletionPricePerMillion: 1500000, Source: domain.PricingSourceLitellm},
	}}}
	h, do := newPricingRouter(t, f)
	_, err := h.svc.SyncPricingNow(context.Background())
	require.NoError(t, err)
	_, err = h.svc.UpsertManualPricing(context.Background(), manualReq("gpt-4o", 100, 300))
	require.NoError(t, err)

	// 删除手动行成功
	rec := do(http.MethodDelete, "/api/admin/pricing?model=gpt-4o", "")
	require.Equal(t, 200, rec.Code, "delete manual: %s", rec.Body.String())
	var del DeletedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &del))
	require.True(t, del.Deleted)

	// litellm 行 → 409
	rec = do(http.MethodDelete, "/api/admin/pricing?model=claude-3-5-sonnet", "")
	require.Equal(t, 409, rec.Code, "delete litellm row must 409: %s", rec.Body.String())
	require.Contains(t, errMsg(t, rec), "claude-3-5-sonnet", "409 消息含 model")
	require.Contains(t, errMsg(t, rec), "manual price only", "409 响应体恒英文（G3-2 分层）")

	// 不存在 → 404
	rec = do(http.MethodDelete, "/api/admin/pricing?model=no-such-model", "")
	require.Equal(t, 404, rec.Code, "delete missing must 404: %s", rec.Body.String())
	require.Contains(t, errMsg(t, rec), "no-such-model", "404 消息含 model")
}

// TestPricingSync 手动触发同步：成功 200 返回拉取统计（+ 快照/库可见）；
// 拉取失败 → 502；url 未配置 → 400；manual 行不被覆盖。
func TestPricingSync(t *testing.T) {
	t.Run("success with stats", func(t *testing.T) {
		f := &fakePriceFetcher{res: &pricing.FetchResult{Rows: []*domain.Pricing{
			{Model: "claude-3-5-sonnet", PromptPricePerMillion: 300000, CompletionPricePerMillion: 1500000, Source: domain.PricingSourceLitellm},
			{Model: "claude-3-opus", PromptPricePerMillion: 1500000, CompletionPricePerMillion: 7500000, Source: domain.PricingSourceLitellm},
		}, Skipped: 3}}
		_, do := newPricingRouter(t, f)

		rec := do(http.MethodPost, "/api/admin/pricing/sync", "")
		require.Equal(t, 200, rec.Code, "sync: %s", rec.Body.String())
		var stats PricingSyncResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &stats))
		require.Equal(t, 2, stats.Rows)
		require.Equal(t, 3, stats.Skipped)
		require.Equal(t, 2, stats.Updated)

		// 拉取行入库且快照可见
		rec = do(http.MethodGet, "/api/admin/pricing", "")
		var list PricingListResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
		require.Equal(t, int64(2), list.Total)
		require.Equal(t, PricingSource("litellm"), list.Rows[0].Source)
	})

	t.Run("fetch failure -> 502 fixed copy, no sourceURL leak", func(t *testing.T) {
		// fetch 层错误按 fetch.go 拼装形态（"pricing: fetch %s: ..."，含
		// sourceURL——G3-1 外泄面）：502 响应体必须固定文案，不含任何上游细节。
		const secret = "https://secret-upstream.example/price.json"
		fetchErr := fmt.Errorf("pricing: fetch %s: %w", secret, errors.New("upstream unreachable"))
		_, do := newPricingRouter(t, &fakePriceFetcher{err: fetchErr})
		rec := do(http.MethodPost, "/api/admin/pricing/sync", "")
		require.Equal(t, http.StatusBadGateway, rec.Code, "fetch fail must 502: %s", rec.Body.String())
		require.Equal(t, "pricing sync failed", errMsg(t, rec), "502 固定文案")
		require.NotContains(t, rec.Body.String(), secret, "sourceURL 不得回显外泄")
		require.NotContains(t, rec.Body.String(), "upstream unreachable", "上游错误详情不得回显")
	})

	t.Run("url not set -> 400", func(t *testing.T) {
		h, do := newPricingRouter(t, &fakePriceFetcher{res: &pricing.FetchResult{}})
		_, err := h.svc.UpdateSetting(context.Background(), "price_source_url", "")
		require.NoError(t, err)
		rec := do(http.MethodPost, "/api/admin/pricing/sync", "")
		require.Equal(t, 400, rec.Code, "url not set must 400: %s", rec.Body.String())
	})

	t.Run("manual row not overwritten", func(t *testing.T) {
		f := &fakePriceFetcher{res: &pricing.FetchResult{Rows: []*domain.Pricing{
			{Model: "gpt-4o", PromptPricePerMillion: 250000, CompletionPricePerMillion: 1000000, Source: domain.PricingSourceLitellm},
		}}}
		h, do := newPricingRouter(t, f)
		_, err := h.svc.UpsertManualPricing(context.Background(), manualReq("gpt-4o", 100, 300))
		require.NoError(t, err)

		rec := do(http.MethodPost, "/api/admin/pricing/sync", "")
		require.Equal(t, 200, rec.Code, "sync: %s", rec.Body.String())
		var stats PricingSyncResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &stats))
		require.Equal(t, 0, stats.Updated, "manual 行不计入 updated")

		rec = do(http.MethodGet, "/api/admin/pricing?model=gpt-4o", "")
		var list PricingListResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
		require.Len(t, list.Rows, 1)
		require.Equal(t, 0.001, list.Rows[0].PromptPricePerMillion, "manual 价不被拉取覆盖")
		require.Equal(t, PricingSource("manual"), list.Rows[0].Source)
	})
}
