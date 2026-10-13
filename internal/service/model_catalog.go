// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"

	"github.com/is7qin/c3api/internal/domain"
)

// 用户面「模型 / 定价」目录只读投影（/api/user/models）：复用调度器模型快照 +
// 价格快照 + 分组归属读，零新增常驻结构。最终价由用户按有效倍率自算
// （不做合成价展示）。

// PriceDisplay 模型展示价格：entry 基底（nil = 无价模型，Mode 不可知）+ 全部
// 条件变体（按 Seq 升序，装载时已排序）。**无 at / 无时区归一**——展示「基底价
// + 全部条件变体」，不解析生效变体。
type PriceDisplay struct {
	Entry    *domain.PriceEntry
	Variants []*domain.PriceVariant
}

// RouteModels 组可路由模型（去重排序，与 GET /v1/models 同源）：经既有 Scheduler
// 注入面能力探测（s.sched.(RouteModelsProvider)）——未装配/不支持 → (nil,false)
// 降级；否则委托 GroupModels。false = 快照未加载/组失效——调用方按该组空模型集合
// 处理（不区分两者）。
func (s *Service) RouteModels(groupID int64) ([]string, bool) {
	rm, ok := s.sched.(RouteModelsProvider)
	if !ok {
		return nil, false
	}
	return rm.GroupModels(groupID)
}

// GroupMultiplierOverrides 该用户在各组的**专属**倍率覆盖（万分数，仅
// group_assignments.price_multiplier 非 nil 的项）：用户面模型目录/分组列表据此
// 求有效倍率 = 覆盖 ?? 组默认 Group.PriceMultiplier。经既有 Store 面读取——无新增
// 装配参数、无新增常驻结构（与调度器模型面同属「复用既有依赖」）。
func (s *Service) GroupMultiplierOverrides(ctx context.Context, userID int64) (map[int64]int, error) {
	as, err := s.store.ListAssignmentsByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]int, len(as))
	for _, a := range as {
		if a.PriceMultiplier != nil {
			out[a.GroupID] = *a.PriceMultiplier
		}
	}
	return out, nil
}

// PriceDisplayForModels 给定模型集合的展示数据（**单次价格快照读**）：entry
// 基底 + 全部 variants（按 Seq 升序）。快照未加载 → nil。无 entry 且无变体的
// 模型不进 map（调用方按未命中 → 无价处理）；异源情形（有变体无 entry）仍以
// Entry=nil + Variants 收录，调用方按无价模型（Mode=null）处理。
func (s *Service) PriceDisplayForModels(models []string) map[string]PriceDisplay {
	snap := s.priceSnapshot.Load()
	if snap == nil {
		return nil
	}
	out := make(map[string]PriceDisplay, len(models))
	for _, m := range models {
		if _, ok := out[m]; ok {
			continue // 同一模型跨组重复：只算一次
		}
		entry := (*snap).entries[m]
		variants := (*snap).variants[m]
		if entry == nil && len(variants) == 0 {
			continue
		}
		out[m] = PriceDisplay{Entry: entry, Variants: variants}
	}
	return out
}
