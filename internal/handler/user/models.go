// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/service"
)

// GetUserModels 用户面「模型 / 定价」目录（/api/user/models）：对用户**可见的
// 每个分组**展示该组**真实可路由的模型**、**模型价格（基底 + 全部条件变体）**、
// 以及**该用户在该组的有效倍率**（最终价由用户自算，不做合成价展示）。
//
// 逻辑（spec §3.3）：ListGroupsForUser → 逐组 RouteModels（false → 该组空模型，
// 不整页报错）→ 并集去重 → 单次价格快照读 PriceDisplayForModels → 组装
// CatalogGroup[]。有效倍率 = 专属覆盖 ?? 组默认（Group.PriceMultiplier）。
func (h *UserAPI) GetUserModels(w http.ResponseWriter, r *http.Request) {
	uid := currentUserID(r)
	groups, err := h.svc.ListGroupsForUser(r.Context(), uid)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	overrides, err := h.svc.GroupMultiplierOverrides(r.Context(), uid)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}

	// 逐组取可路由模型；并集所有组模型名（同一模型跨组重复只算一次价格）。
	names := make([][]string, 0, len(groups))
	all := make([]string, 0, len(groups))
	for _, g := range groups {
		models, ok := h.svc.RouteModels(g.ID)
		if !ok {
			models = nil // 快照未加载/组失效 → 该组空模型（前端「暂无」，不区分两者）
		}
		names = append(names, models)
		all = append(all, models...)
	}
	catalog := h.svc.PriceDisplayForModels(all)

	out := make([]CatalogGroup, 0, len(groups))
	for i, g := range groups {
		models := make([]CatalogModel, 0, len(names[i]))
		for _, name := range names[i] {
			models = append(models, toAPICatalogModel(name, catalog[name]))
		}
		out = append(out, CatalogGroup{
			Group:  toAPIUserGroup(g, effectiveMultiplier(g, overrides)),
			Models: models,
		})
	}
	httpface.WriteJSON(w, http.StatusOK, out)
}

// toAPICatalogModel 模型展示数据 → 契约类型（Mode + 价格分量 + 条件变体）。
// pd.Entry 缺（无价模型）→ Mode/价格字段全 null（前端 —）；变体仍透出（异源
// 情形：有变体无 entry）。单位：token/图像 token 价 = USD/1M，按次 = USD/次，
// 按张 = USD/张（毫分 → USD 经 httpface 共享换算）。
func toAPICatalogModel(name string, pd service.PriceDisplay) CatalogModel {
	m := CatalogModel{
		Model:    name,
		Variants: make([]CatalogVariant, 0, len(pd.Variants)),
	}
	if e := pd.Entry; e != nil {
		mode := string(e.Mode)
		m.Mode = &mode
		m.InputPerM = httpface.MillisToUSDPtr(e.InputPerM)
		m.OutputPerM = httpface.MillisToUSDPtr(e.OutputPerM)
		m.CacheReadPerM = httpface.MillisToUSDPtr(e.CacheReadPerM)
		m.CacheWritePerM = httpface.MillisToUSDPtr(e.CacheWritePerM)
		m.PricePerCall = httpface.MillisToUSDPtr(e.PricePerCall)
		m.ImgInTokPerM = httpface.MillisToUSDPtr(e.ImgInTokPerM)
		m.ImgOutTokPerM = httpface.MillisToUSDPtr(e.ImgOutTokPerM)
		m.PricePerImage = httpface.MillisToUSDPtr(e.PricePerImage)
		m.Provider = e.Provider // 自由字符串，不做 enum 校验
		m.SupportsPromptCaching = e.SupportsPromptCaching
	}
	for _, v := range pd.Variants {
		m.Variants = append(m.Variants, toAPICatalogVariant(v))
	}
	return m
}

// toAPICatalogVariant 条件变体 → 契约类型（Seq 升序；条件全可空 = 通配 AND）。
// 注意列名错位：SetCacheCreationPerM 语义 = 覆盖**缓存写**价（对应基底列
// CacheWritePerM），命名沿 PriceVariant 既有列——前端须挂「缓存写」列。
func toAPICatalogVariant(v *domain.PriceVariant) CatalogVariant {
	return CatalogVariant{
		Seq:                  v.Seq,
		ServiceTier:          v.ServiceTier,
		CtxMin:               v.CtxMin,
		CtxMax:               v.CtxMax,
		TimeStart:            v.TimeStart,
		TimeEnd:              v.TimeEnd,
		DowMask:              v.DowMask,
		Multiplier:           httpface.MultBPToNormal(v.MultBP),
		SetInputPerM:         httpface.MillisToUSDPtr(v.SetInputPerM),
		SetOutputPerM:        httpface.MillisToUSDPtr(v.SetOutputPerM),
		SetCacheReadPerM:     httpface.MillisToUSDPtr(v.SetCacheReadPerM),
		SetCacheCreationPerM: httpface.MillisToUSDPtr(v.SetCacheCreationPerM),
		SetPricePerCall:      httpface.MillisToUSDPtr(v.SetPricePerCall),
		SetImgInTokPerM:      httpface.MillisToUSDPtr(v.SetImgInTokPerM),
		SetImgOutTokPerM:     httpface.MillisToUSDPtr(v.SetImgOutTokPerM),
		SetPricePerImage:     httpface.MillisToUSDPtr(v.SetPricePerImage),
	}
}
