// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/repository"
)

// 图片生成价格管理面（/api/admin/image-price）：列表 / 手动设价 / 删除手动价
// （Task A 数据面；images 端点计费价格来源）。PUT/DELETE 的 model 走 query
// 参数（生成契约签名 params.Model——模型名可含 `/`，同 /pricing 不入路径）；
// 错误映射走 httpface.WriteServiceErr（ErrNotFound → 404、ErrConflict → 409、
// ErrInvalidInput → 400——含"至少一价非 nil"校验）。
// 与 /api/admin/pricing 同形态同单位（token 价 USD/1M per-million；仅多 per-image
// USD/张 分量，见换算函数注释）。

// GetImagePrice 图片价格列表（分页/筛选/排序，对齐 GetPricing），ServerInterface。
func (h *AdminAPI) GetImagePrice(w http.ResponseWriter, r *http.Request, params GetImagePriceParams) {
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
	rows, total, err := h.svc.ListImagePrice(r.Context(), q, src, string(deref(params.Provider)), deref(params.Model))
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]ImagePrice, 0, len(rows))
	for _, p := range rows {
		out = append(out, toAPIImagePrice(p))
	}
	httpface.WriteJSON(w, http.StatusOK, ImagePriceListResponse{Total: total, Rows: out})
}

// PutImagePriceModel 手动设图片价格（API 边界换算——**单位规则与 pricings 相同**）：
// token 价收 USD/1M image tokens（per-million 口径，与字段名 *_price_per_million
// 及 chat 价 PromptPricePerMillion 语义一致）→ ×1e5 → 毫分/1M（复用
// usdToMillis，与 chat 价同系数）；per-image 收 USD/张 → ×1e5 → 毫分/张
// （usdPerImageToMilli，系数与 usdToMillis 相同但单位语义独立——按张 flat 计费，
// 独立函数自文档化）。三分量全缺省（nil）→ 400（service 校验：行有效性 = 至少
// 一价非 nil）。upsert 强制 source=manual，可接管 litellm 行，ServerInterface。
func (h *AdminAPI) PutImagePriceModel(w http.ResponseWriter, r *http.Request, params PutImagePriceModelParams) {
	var in ImagePriceUpsert
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	p, err := h.svc.UpsertManualImagePrice(r.Context(), &repository.ImagePriceManual{
		Model:                           params.Model,
		InputImageTokenPricePerMillion:  usdToMillisPtr(in.InputImageTokenPricePerMillion),
		OutputImageTokenPricePerMillion: usdToMillisPtr(in.OutputImageTokenPricePerMillion),
		OutputCostPerImageMilli:         usdPerImageToMilliPtr(in.OutputCostPerImage),
	})
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIImagePrice(p))
}

// DeleteImagePriceModel 删除手动图片价格：仅 source=manual 可删（litellm 行 →
// 409、不存在 → 404，service 错误映射），ServerInterface。
func (h *AdminAPI) DeleteImagePriceModel(w http.ResponseWriter, r *http.Request, params DeleteImagePriceModelParams) {
	if err := h.svc.DeleteManualImagePrice(r.Context(), params.Model); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, DeletedResponse{Deleted: true})
}

// toAPIImagePrice 图片价格领域对象 → 契约类型（API 边界换算：毫分/1M →
// USD/1M（per-million，与 pricings 同口径）、毫分/张 → USD/张；raw 完整镜像
// 不对外暴露，对齐 toAPIPricing）。
func toAPIImagePrice(p *domain.ImagePrice) ImagePrice {
	return ImagePrice{
		Model:                           p.Model,
		InputImageTokenPricePerMillion:  millisToUSDPtr(p.InputImageTokenPricePerMillion),
		OutputImageTokenPricePerMillion: millisToUSDPtr(p.OutputImageTokenPricePerMillion),
		OutputCostPerImage:              milliPerImageToUSDPtr(p.OutputCostPerImageMilli),
		Provider:                        (*Provider)(p.Provider),
		Source:                          PricingSource(p.Source),
		CreatedAt:                       p.CreatedAt,
		UpdatedAt:                       p.UpdatedAt,
	}
}
