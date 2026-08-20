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

// imageSeedFetcher 构造带 image 行的拉取结果（Task A 双线 seed；litellm 行带
// provider——litellm 数据直贴）。
func imageSeedFetcher() *fakePriceFetcher {
	return &fakePriceFetcher{res: &pricing.FetchResult{
		Rows: []*domain.Pricing{
			{Model: "gpt-4o", PromptPricePerMillion: 250000, CompletionPricePerMillion: 1000000, Source: domain.PricingSourceLitellm},
		},
		ImageRows: []*domain.ImagePrice{
			{Model: "gpt-image-2", InputImageTokenPricePerMillion: i64p(800000), OutputImageTokenPricePerMillion: i64p(3000000), Provider: strPtr("openai"), Source: domain.PricingSourceLitellm},
		},
	}}
}

func i64p(v int64) *int64 { return &v }
func strPtr(s string) *string { return &s }

// TestPutImagePrice 手动设图片价格：单位换算（token 价 USD/1M ×1e5 存储、
// per-image USD/张 ×1e5 存储、回显反向换算）+ 接管 litellm 行 +
// 缺省分量清空 + 校验（全 nil → 400、负数 → 400）。
func TestPutImagePrice(t *testing.T) {
	f := imageSeedFetcher()
	h, do := newPricingRouter(t, f)

	// 新模型设价：token 价 8.0/30.0 USD/1M → 800,000/3,000,000 毫分/1M（×1e5，
	// 与 chat 价同系数）；per-image 0.054 USD/张 → 5,400 毫分/张；回显反向换算
	rec := do(http.MethodPut, "/api/admin/image-price?model=gpt-image-3",
		`{"input_image_token_price_per_million":8.0,"output_image_token_price_per_million":30.0,"output_cost_per_image":0.054}`)
	require.Equal(t, 200, rec.Code, "put: %s", rec.Body.String())
	var p ImagePrice
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	require.Equal(t, "gpt-image-3", p.Model)
	require.NotNil(t, p.InputImageTokenPricePerMillion)
	require.Equal(t, 8.0, *p.InputImageTokenPricePerMillion, "token 价回显 USD/1M（per-million）")
	require.NotNil(t, p.OutputImageTokenPricePerMillion)
	require.Equal(t, 30.0, *p.OutputImageTokenPricePerMillion)
	require.NotNil(t, p.OutputCostPerImage)
	require.Equal(t, 0.054, *p.OutputCostPerImage, "per-image 回显 USD/张（×1e5 独立换算）")
	require.Equal(t, PricingSource("manual"), p.Source)

	// 接管 litellm 行：先 sync 入库再手动设价（litellm 行带 provider → 接管后清空）
	_, err := h.svc.SyncPricingNow(context.Background())
	require.NoError(t, err)
	rec = do(http.MethodPut, "/api/admin/image-price?model=gpt-image-2",
		`{"input_image_token_price_per_million":8.0,"output_cost_per_image":0.054}`)
	require.Equal(t, 200, rec.Code, "takeover: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	require.Equal(t, PricingSource("manual"), p.Source, "手动设价接管 litellm 行")
	require.Nil(t, p.OutputImageTokenPricePerMillion, "缺省分量 → nil（清空）")
	require.Nil(t, p.Provider, "manual 接管后 provider nil（S-2）")

	// 全 nil → 400（行有效性 = 至少一价）
	rec = do(http.MethodPut, "/api/admin/image-price?model=m-none", `{}`)
	require.Equal(t, 400, rec.Code, "all nil: %s", rec.Body.String())
	rec = do(http.MethodPut, "/api/admin/image-price?model=m-none", `{"input_image_token_price_per_million":null}`)
	require.Equal(t, 400, rec.Code, "explicit null: %s", rec.Body.String())

	// 负数 → 400（token 价与 per-image 各自校验）
	rec = do(http.MethodPut, "/api/admin/image-price?model=m-neg",
		`{"input_image_token_price_per_million":-0.01,"output_cost_per_image":0.054}`)
	require.Equal(t, 400, rec.Code, "negative token price: %s", rec.Body.String())
	rec = do(http.MethodPut, "/api/admin/image-price?model=m-neg",
		`{"input_image_token_price_per_million":8.0,"output_cost_per_image":-0.054}`)
	require.Equal(t, 400, rec.Code, "negative per-image: %s", rec.Body.String())

	// 非法 JSON → 400
	rec = do(http.MethodPut, "/api/admin/image-price?model=m-bad", `not json`)
	require.Equal(t, 400, rec.Code, "invalid json: %s", rec.Body.String())
}

// TestImagePriceList 列表：全量 / source 筛选 / model 模糊 / 分页。
func TestImagePriceList(t *testing.T) {
	f := imageSeedFetcher()
	h, do := newPricingRouter(t, f)
	_, err := h.svc.SyncPricingNow(context.Background())
	require.NoError(t, err)
	_, err = h.svc.UpsertManualImagePrice(context.Background(), &repository.ImagePriceManual{
		Model: "aiml-image", OutputCostPerImageMilli: i64p(5400),
	})
	require.NoError(t, err)

	rec := do(http.MethodGet, "/api/admin/image-price", "")
	require.Equal(t, 200, rec.Code, "list all: %s", rec.Body.String())
	var list ImagePriceListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(2), list.Total)
	require.Len(t, list.Rows, 2)

	rec = do(http.MethodGet, "/api/admin/image-price?source=litellm", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(1), list.Total)
	require.Equal(t, PricingSource("litellm"), list.Rows[0].Source)

	// litellm 行回显 provider（拉取直贴）；manual 行 nil
	rec = do(http.MethodGet, "/api/admin/image-price?source=litellm", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.NotNil(t, list.Rows[0].Provider)
	require.Equal(t, "openai", string(*list.Rows[0].Provider), "litellm 行回显 provider")
	rec = do(http.MethodGet, "/api/admin/image-price?source=manual", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Nil(t, list.Rows[0].Provider, "manual 行 provider nil")

	// provider 等值筛选：命中 openai → 1 行（manual 行不命中）；不命中 → 0 行
	rec = do(http.MethodGet, "/api/admin/image-price?provider=openai", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(1), list.Total)
	require.Equal(t, "gpt-image-2", list.Rows[0].Model)
	rec = do(http.MethodGet, "/api/admin/image-price?provider=bedrock", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Zero(t, list.Total, "provider 不命中 → 空列表")
	// 非 enum 自由字符串也可筛（DB 自由字符串等值）
	rec = do(http.MethodGet, "/api/admin/image-price?provider=some_future_vendor", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Zero(t, list.Total)

	rec = do(http.MethodGet, "/api/admin/image-price?model=IMAGE", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(2), list.Total, "model 模糊（大小写不敏感；gpt-image-2 + aiml-image）")

	rec = do(http.MethodGet, "/api/admin/image-price?page=2&page_size=1", "")
	require.Equal(t, 200, rec.Code, "pagination: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(2), list.Total)
	require.Len(t, list.Rows, 1, "第二页一行")

	// 非法参数 400
	for _, tc := range []struct{ name, query string }{
		{"source 非法", "?source=bogus"},
		{"sort 非法", "?sort=bogus"},
		{"page_size 越界", "?page_size=1001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(http.MethodGet, "/api/admin/image-price"+tc.query, "")
			require.Equal(t, 400, rec.Code, "%s: %s", tc.name, rec.Body.String())
		})
	}
}

// TestDeleteImagePrice 删除手动图片价格：litellm 行 → 409、manual 行删除 200、
// 不存在 → 404。
func TestDeleteImagePrice(t *testing.T) {
	f := imageSeedFetcher()
	h, do := newPricingRouter(t, f)
	_, err := h.svc.SyncPricingNow(context.Background())
	require.NoError(t, err)
	_, err = h.svc.UpsertManualImagePrice(context.Background(), &repository.ImagePriceManual{
		Model: "manual-m", InputImageTokenPricePerMillion: i64p(100),
	})
	require.NoError(t, err)

	rec := do(http.MethodDelete, "/api/admin/image-price?model=gpt-image-2", "")
	require.Equal(t, 409, rec.Code, "litellm 行 → 409: %s", rec.Body.String())
	// G3-2：409 响应体恒英文（对外分层），不含中文
	require.Contains(t, errMsg(t, rec), "manual price only", "409 消息英文（manual price only）")
	require.NotContains(t, errMsg(t, rec), "只允许删手动价", "409 消息不得含中文")

	rec = do(http.MethodDelete, "/api/admin/image-price?model=manual-m", "")
	require.Equal(t, 200, rec.Code, "manual 删除: %s", rec.Body.String())

	rec = do(http.MethodDelete, "/api/admin/image-price?model=no-such", "")
	require.Equal(t, 404, rec.Code, "不存在 → 404: %s", rec.Body.String())
}
