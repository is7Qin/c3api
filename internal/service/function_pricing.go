// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"fmt"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
)

// functionPriceReloadPage 快照全量加载的分页大小（与 pricingReloadPage 同量级：
// function_price 行数 ≤ pricings 行数，单页内取完）。
const functionPriceReloadPage = 1000

// ReloadFunctionPricing 全量重载按单元价快照（公开）：litellm 同步拉取成功后 +
// 管理端改价后调用，读路径（GetFunctionPrice）即时生效。失败 fail-safe
// （Warn + 保留旧快照，对齐 ReloadPricing）。
func (s *Service) ReloadFunctionPricing() {
	s.reloadFunctionPricing(context.Background())
}

// ReloadFunctionPricingCtx 全量重载按单元价快照并返回错误（snapshot.Registry
// 适配：错误进注册表 Status 可观测；启动就绪统一首刷入口）。失败保留旧快照
// （fail-safe，与 ReloadFunctionPricing 同语义）。
func (s *Service) ReloadFunctionPricingCtx(ctx context.Context) error {
	m, err := s.loadFunctionPricing(ctx)
	if err != nil {
		return err
	}
	s.functionPrice.Store(&m)
	return nil
}

// reloadFunctionPricing 从 DB 全量加载按单元价快照（管理端改价/sync 路径调用）。
// 失败 fail-safe：仅 Warn + 保留旧快照/空快照，不阻断服务启动——读快照缺失
// 按 GetFunctionPrice 兜底语义（codex-search → 默认行；其他 → 错误，计费方
// 拒绝）。
func (s *Service) reloadFunctionPricing(ctx context.Context) {
	m, err := s.loadFunctionPricing(ctx)
	if err != nil {
		return // loadFunctionPricing 已 Warn
	}
	s.functionPrice.Store(&m)
}

// loadFunctionPricing 从 DB 分页加载按单元价快照；错误原样返回（Warn 在此统一）。
func (s *Service) loadFunctionPricing(ctx context.Context) (map[string]*domain.FunctionPrice, error) {
	var all []*domain.FunctionPrice
	for offset := 0; ; offset += functionPriceReloadPage {
		rows, _, err := s.store.ListFunctionPrice(ctx, repository.ListQuery{
			Limit: functionPriceReloadPage, Offset: offset, Sort: "model", Order: "asc",
		}, nil, "", "")
		if err != nil {
			if s.log != nil {
				s.log.Warn("function price snapshot reload failed", logx.Error(err))
			}
			return nil, err
		}
		all = append(all, rows...)
		if len(rows) < functionPriceReloadPage {
			break
		}
	}
	return buildFunctionPriceSnapshot(all), nil
}

// buildFunctionPriceSnapshot 快照构建：model → 按单元价行。同一 model 出现多行
// （防御——仓库 unique(model) 约束下不应出现）按 source 优先级收敛：manual
// 覆盖 litellm（与表内"一行 = 最终生效价"、拉取 upsert 的行级互斥语义一致，
// 对齐 buildPricingSnapshot）。
func buildFunctionPriceSnapshot(rows []*domain.FunctionPrice) map[string]*domain.FunctionPrice {
	m := make(map[string]*domain.FunctionPrice, len(rows))
	for _, p := range rows {
		if prev, ok := m[p.Model]; ok && prev.Source == domain.PricingSourceManual {
			continue // 已收敛为 manual：后续行（含 manual/litellm）一律不覆盖
		}
		m[p.Model] = p
	}
	return m
}

// GetFunctionPrice 快照读按单元价（search 等按单元计费热路径，零 DB）：命中
// 返回价格行；查无 + model == codex-search → 默认价行（domain 常量兜底——
// **防御语义**：表删/初始化种子失败/快照加载失败时计费不中断，按 $0.01/次
// 继续（宁多勿漏；codex-search 为网关自有固定功能，默认价即业务定值）；
// 查无其他 → ErrNotFound（计费方应拒绝计费而非按 0 计价——对齐 GetPrice
// M-1 语义）。返回的默认行是每次调用新建（不可变常量值，防调用方原地修改
// 污染快照语义——与快照行只读约定一致）。
func (s *Service) GetFunctionPrice(model string) (*domain.FunctionPrice, error) {
	if m := s.functionPrice.Load(); m != nil {
		if p, ok := (*m)[model]; ok {
			return p, nil
		}
	}
	if model == domain.CodexSearchModel {
		v := domain.DefaultCodexSearchPricePerCall
		return &domain.FunctionPrice{
			Model:        model,
			PricePerCall: &v,
			Source:       domain.PricingSourceManual,
		}, nil
	}
	return nil, fmt.Errorf("%w: model=%q（无按单元价格数据：请管理端设价或等待 litellm 同步）", ErrNotFound, model)
}

// UpsertManualFunctionPrice 手动设按单元价（管理端 PUT /api/admin/function-prices?
// model=X）：校验（model 非空；price_per_call 全 nil → 400——行有效性 = 按
// 单元价非 nil；非 nil 且 < 0 → 400）+ 落库（upsert 强制 source=manual，可
// 接管 litellm 行）+ 成功后重载快照（读路径即时生效）。FunctionPriceManual
// 单位：毫分/次（handler 边界已由 API 入参 USD 换算）。
func (s *Service) UpsertManualFunctionPrice(ctx context.Context, m *repository.FunctionPriceManual) (*domain.FunctionPrice, error) {
	if m.Model == "" {
		return nil, fmt.Errorf("%w: model is required", ErrInvalidInput)
	}
	if !m.HasAnyPrice() {
		return nil, fmt.Errorf("%w: price_per_call is required", ErrInvalidInput)
	}
	if m.PricePerCall != nil && *m.PricePerCall < 0 {
		return nil, fmt.Errorf("%w: price_per_call must be >= 0", ErrInvalidInput)
	}
	p, err := s.store.UpsertFunctionManual(ctx, m)
	if err != nil {
		return nil, err
	}
	s.reloadFunctionPricing(ctx)
	return p, nil
}

// DeleteManualFunctionPrice 删除手动按单元价（管理端 DELETE /api/admin/function-
// prices?model=X）：仅 source=manual 行可删（litellm 行 → ErrConflict；仓库
// 语义）；成功后重载快照——该 model 从快照消失（缺失窗口内 GetFunctionPrice
// → 兜底/ErrNotFound，下轮拉取补回；codex-search 种子行删除后下轮启动
// bootstrap 幂等补回）。
func (s *Service) DeleteManualFunctionPrice(ctx context.Context, model string) error {
	if err := s.store.DeleteFunctionManual(ctx, model); err != nil {
		return mapRepoErr(err)
	}
	s.reloadFunctionPricing(ctx)
	return nil
}

// ListFunctionPrice 管理端按单元价列表（GET /api/admin/function-prices）：分页 +
// source/model 筛选 + sort 白名单校验（非法 → ErrInvalidInput 400）。
func (s *Service) ListFunctionPrice(ctx context.Context, q repository.ListQuery, source *domain.PricingSource, provider, model string) ([]*domain.FunctionPrice, int64, error) {
	if source != nil && !source.Valid() {
		return nil, 0, fmt.Errorf("%w: invalid source %q", ErrInvalidInput, *source)
	}
	if err := validateListQuery(q, listSortFields["function_price"]); err != nil {
		return nil, 0, err
	}
	return s.store.ListFunctionPrice(ctx, q, source, provider, model)
}
