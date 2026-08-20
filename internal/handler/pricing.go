// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"errors"
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
)

// 模型价格管理面（/api/admin/pricing）：列表 / 手动设价 / 删除手动价 / 手动触发
// 同步。PUT/DELETE 的 model 走 query 参数（生成契约签名 params.Model——模型名
// 是自由字符串可含 `/`，路径参数单段匹配会拆段 404，故不入路径）；错误映射
// 走 httpface.WriteServiceErr（ErrNotFound → 404、ErrConflict → 409、ErrInvalidInput →
// 400），sync 拉取上游失败 → 502（ErrPriceFetch）。

// GetPricing 价格列表（增强分页范式 page/page_size + source/provider/model
// 筛选 + sort 白名单，ServerInterface）。
func (h *AdminAPI) GetPricing(w http.ResponseWriter, r *http.Request, params GetPricingParams) {
	q, err := pageToQuery(params.Page, params.PageSize)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	q.Sort = deref(params.Sort)
	q.Order = string(deref(params.Order))
	var src *domain.PricingSource
	if params.Source != nil {
		s := domain.PricingSource(*params.Source)
		src = &s
	}
	rows, total, err := h.svc.ListPricing(r.Context(), q, src, string(deref(params.Provider)), deref(params.Model))
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]Pricing, 0, len(rows))
	for _, p := range rows {
		out = append(out, toAPIPricing(p))
	}
	httpface.WriteJSON(w, http.StatusOK, PricingListResponse{Total: total, Rows: out})
}

// PutPricingModel 手动设价（API 边界换算：价格列 USD/1M → 毫分/1M、fast
// 倍率正常值 → 万分数；upsert 强制 source=manual，可接管 litellm 行；负数/
// model 空 → 400，service 校验）。可选字段（cache 价 + Phase 5 矩阵 22 列）
// 缺省（nil）→ 清空（接管行该矩阵价清除，PUT 全量替换语义），ServerInterface。
func (h *AdminAPI) PutPricingModel(w http.ResponseWriter, r *http.Request, params PutPricingModelParams) {
	var in PricingUpsert
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	p, err := h.svc.UpsertManualPricing(r.Context(), &repository.PricingManual{
		Model:                                     params.Model,
		PromptPricePerMillion:                     usdToMillis(in.PromptPricePerMillion),
		CompletionPricePerMillion:                 usdToMillis(in.CompletionPricePerMillion),
		CacheReadPricePerMillion:                  usdToMillisPtr(in.CacheReadPricePerMillion),
		CacheCreationPricePerMillion:              usdToMillisPtr(in.CacheCreationPricePerMillion),
		PriorityPromptPricePerMillion:             usdToMillisPtr(in.PriorityPromptPricePerMillion),
		PriorityCompletionPricePerMillion:         usdToMillisPtr(in.PriorityCompletionPricePerMillion),
		PriorityCacheReadPricePerMillion:          usdToMillisPtr(in.PriorityCacheReadPricePerMillion),
		PriorityCacheCreationPricePerMillion:      usdToMillisPtr(in.PriorityCacheCreationPricePerMillion),
		FlexPromptPricePerMillion:                 usdToMillisPtr(in.FlexPromptPricePerMillion),
		FlexCompletionPricePerMillion:             usdToMillisPtr(in.FlexCompletionPricePerMillion),
		FlexCacheReadPricePerMillion:              usdToMillisPtr(in.FlexCacheReadPricePerMillion),
		FlexCacheCreationPricePerMillion:          usdToMillisPtr(in.FlexCacheCreationPricePerMillion),
		AboveThreshold:                            in.AboveThreshold,
		AbovePromptPricePerMillion:                usdToMillisPtr(in.AbovePromptPricePerMillion),
		AboveCompletionPricePerMillion:            usdToMillisPtr(in.AboveCompletionPricePerMillion),
		AboveCacheReadPricePerMillion:             usdToMillisPtr(in.AboveCacheReadPricePerMillion),
		AboveCacheCreationPricePerMillion:         usdToMillisPtr(in.AboveCacheCreationPricePerMillion),
		AbovePriorityPromptPricePerMillion:        usdToMillisPtr(in.AbovePriorityPromptPricePerMillion),
		AbovePriorityCompletionPricePerMillion:    usdToMillisPtr(in.AbovePriorityCompletionPricePerMillion),
		AbovePriorityCacheReadPricePerMillion:     usdToMillisPtr(in.AbovePriorityCacheReadPricePerMillion),
		AbovePriorityCacheCreationPricePerMillion: usdToMillisPtr(in.AbovePriorityCacheCreationPricePerMillion),
		AboveFlexPromptPricePerMillion:            usdToMillisPtr(in.AboveFlexPromptPricePerMillion),
		AboveFlexCompletionPricePerMillion:        usdToMillisPtr(in.AboveFlexCompletionPricePerMillion),
		AboveFlexCacheReadPricePerMillion:         usdToMillisPtr(in.AboveFlexCacheReadPricePerMillion),
		AboveFlexCacheCreationPricePerMillion:     usdToMillisPtr(in.AboveFlexCacheCreationPricePerMillion),
		FastMultiplier:                            normalToMultI64Ptr(in.FastMultiplier),
	})
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIPricing(p))
}

// DeletePricingModel 删除手动价：仅 source=manual 可删（litellm 行 → 409、
// 不存在 → 404，service 错误映射），ServerInterface。
func (h *AdminAPI) DeletePricingModel(w http.ResponseWriter, r *http.Request, params DeletePricingModelParams) {
	if err := h.svc.DeleteManualPricing(r.Context(), params.Model); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, DeletedResponse{Deleted: true})
}

// PostPricingSync 手动触发一次价格同步（拉取官方价格表 → upsert → 快照重载；
// 成功 200 返回拉取统计；拉取上游失败 → 502、url 未配置 → 400），ServerInterface。
func (h *AdminAPI) PostPricingSync(w http.ResponseWriter, r *http.Request) {
	stats, err := h.svc.SyncPricingNow(r.Context())
	if err != nil {
		if errors.Is(err, service.ErrPriceFetch) {
			// G3-1：固定文案，不带上游细节（err 内含 fetch.go 拼入的
			// sourceURL——回显即外泄）；详情已由 SyncPricingNow 的 fetch 失败
			// 分支 log.Warn 落日志。
			httpface.WriteErr(w, http.StatusBadGateway, "pricing sync failed")
			return
		}
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, PricingSyncResponse{
		Rows: stats.Rows, Skipped: stats.Skipped, Updated: stats.Updated,
		ImageRows:    ptr(stats.ImageRows), // Task A 双线：image 价行统计（可选字段）
		ImageUpdated: ptr(stats.ImageUpdated),
		// 价格表三件套：按单元价行统计（可选字段）。
		FunctionRows:    ptr(stats.FunctionRows),
		FunctionUpdated: ptr(stats.FunctionUpdated),
	})
}

// toAPIPricing 价格领域对象 → 契约类型（API 边界换算：毫分/1M → USD/1M、
// 万分数 → 正常值；raw 完整镜像不对外暴露——前端无需 litellm 原始条目；
// 如需可后续加端点）。
func toAPIPricing(p *domain.Pricing) Pricing {
	return Pricing{
		Model:                                     p.Model,
		PromptPricePerMillion:                     millisToUSD(p.PromptPricePerMillion),
		CompletionPricePerMillion:                 millisToUSD(p.CompletionPricePerMillion),
		MaxInputTokens:                            p.MaxInputTokens,
		MaxOutputTokens:                           p.MaxOutputTokens,
		CacheReadPricePerMillion:                  millisToUSDPtr(p.CacheReadPricePerMillion),
		CacheCreationPricePerMillion:              millisToUSDPtr(p.CacheCreationPricePerMillion),
		PriorityPromptPricePerMillion:             millisToUSDPtr(p.PriorityPromptPricePerMillion),
		PriorityCompletionPricePerMillion:         millisToUSDPtr(p.PriorityCompletionPricePerMillion),
		PriorityCacheReadPricePerMillion:          millisToUSDPtr(p.PriorityCacheReadPricePerMillion),
		PriorityCacheCreationPricePerMillion:      millisToUSDPtr(p.PriorityCacheCreationPricePerMillion),
		FlexPromptPricePerMillion:                 millisToUSDPtr(p.FlexPromptPricePerMillion),
		FlexCompletionPricePerMillion:             millisToUSDPtr(p.FlexCompletionPricePerMillion),
		FlexCacheReadPricePerMillion:              millisToUSDPtr(p.FlexCacheReadPricePerMillion),
		FlexCacheCreationPricePerMillion:          millisToUSDPtr(p.FlexCacheCreationPricePerMillion),
		AboveThreshold:                            p.AboveThreshold,
		AbovePromptPricePerMillion:                millisToUSDPtr(p.AbovePromptPricePerMillion),
		AboveCompletionPricePerMillion:            millisToUSDPtr(p.AboveCompletionPricePerMillion),
		AboveCacheReadPricePerMillion:             millisToUSDPtr(p.AboveCacheReadPricePerMillion),
		AboveCacheCreationPricePerMillion:         millisToUSDPtr(p.AboveCacheCreationPricePerMillion),
		AbovePriorityPromptPricePerMillion:        millisToUSDPtr(p.AbovePriorityPromptPricePerMillion),
		AbovePriorityCompletionPricePerMillion:    millisToUSDPtr(p.AbovePriorityCompletionPricePerMillion),
		AbovePriorityCacheReadPricePerMillion:     millisToUSDPtr(p.AbovePriorityCacheReadPricePerMillion),
		AbovePriorityCacheCreationPricePerMillion: millisToUSDPtr(p.AbovePriorityCacheCreationPricePerMillion),
		AboveFlexPromptPricePerMillion:            millisToUSDPtr(p.AboveFlexPromptPricePerMillion),
		AboveFlexCompletionPricePerMillion:        millisToUSDPtr(p.AboveFlexCompletionPricePerMillion),
		AboveFlexCacheReadPricePerMillion:         millisToUSDPtr(p.AboveFlexCacheReadPricePerMillion),
		AboveFlexCacheCreationPricePerMillion:     millisToUSDPtr(p.AboveFlexCacheCreationPricePerMillion),
		FastMultiplier:                            multI64ToNormalPtr(p.FastMultiplier),
		Provider:                                  (*Provider)(p.Provider),
		Mode:                                      p.Mode,
		SupportsPromptCaching:                     p.SupportsPromptCaching,
		Source:                                    PricingSource(p.Source),
		CreatedAt:                                 p.CreatedAt,
		UpdatedAt:                                 p.UpdatedAt,
	}
}
