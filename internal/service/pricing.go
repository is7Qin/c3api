// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/pricing"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
)

// ErrPriceFetch 价格拉取上游失败（管理端手动 sync 映射 502 Bad Gateway；
// 与 fetch 内部错误包装，保留详情）。
var ErrPriceFetch = errors.New("service: price fetch failed")

// PricingSyncStats 一次手动同步的拉取统计（POST /api/admin/pricing/sync 响应）。
// Task A 双线扩展：ImageRows/ImageUpdated 为 image_price 行统计（与文本价
// 独立判定独立落库）；价格表三件套再扩：FunctionRows/FunctionUpdated 为
// function_price 行统计（同款独立判定独立落库）；Skipped 语义含三线判定——
// 文本价、image 价与 function 价均无效的条目计一次。
type PricingSyncStats struct {
	Rows            int // 拉取到的有效模型行数
	Skipped         int // 解析时跳过的非法条目数（文本价、image 价与 function 价均无效）
	Updated         int // 文本价 upsert 落库数（manual 行不计）
	ImageRows       int // 拉取到的有效 image 价行数
	ImageUpdated    int // image 价 upsert 落库数（manual 行不计）
	FunctionRows    int // 拉取到的有效按单元价行数（search 等）
	FunctionUpdated int // 按单元价 upsert 落库数（manual 行不计）
}

// pricingReloadPage 快照全量加载的分页大小（litellm 官方表 ~2k 模型 → 3 页内取完）。
const pricingReloadPage = 1000

// ReloadPricing 全量重载价格快照（公开）：litellm 同步拉取成功后 + 管理端改价
// 后调用，读路径（GetPrice）即时生效。失败 fail-safe（Warn + 保留旧快照）。
func (s *Service) ReloadPricing() {
	s.reloadPricing(context.Background())
}

// ReloadPricingCtx 全量重载价格快照并返回错误（snapshot.Registry 适配：错误
// 进注册表 Status 可观测；启动就绪统一首刷入口）。失败保留旧快照（fail-safe，
// 与 ReloadPricing 同语义——错误仅上报，不替换快照）。
func (s *Service) ReloadPricingCtx(ctx context.Context) error {
	m, err := s.loadPricing(ctx)
	if err != nil {
		return err
	}
	s.pricing.Store(&m)
	return nil
}

// reloadPricing 从 DB 全量加载价格快照（管理端改价/sync 路径调用）。
// 失败 fail-safe（评审 M-1 同款）：仅 Warn + 保留旧快照/空快照，不阻断服务
// 启动——读快照缺失按 ErrNotFound（Phase 5 计费拒绝计费而非按 0 计价）。
func (s *Service) reloadPricing(ctx context.Context) {
	m, err := s.loadPricing(ctx)
	if err != nil {
		return // loadPricing 已 Warn
	}
	s.pricing.Store(&m)
}

// loadPricing 从 DB 分页加载价格快照；错误原样返回（Warn 在此统一——调用方
// 决定 fail-safe 或上报语义，不重复告警）。
func (s *Service) loadPricing(ctx context.Context) (map[string]*domain.Pricing, error) {
	var all []*domain.Pricing
	for offset := 0; ; offset += pricingReloadPage {
		rows, _, err := s.store.ListPricing(ctx, repository.ListQuery{
			Limit: pricingReloadPage, Offset: offset, Sort: "model", Order: "asc",
		}, nil, "", "")
		if err != nil {
			if s.log != nil {
				s.log.Warn("pricing snapshot reload failed", logx.Error(err))
			}
			return nil, err
		}
		all = append(all, rows...)
		if len(rows) < pricingReloadPage {
			break
		}
	}
	return buildPricingSnapshot(all), nil
}

// buildPricingSnapshot 快照构建：model → 价格行。同一 model 出现多行（防御——
// 仓库 unique(model) 约束下不应出现）按 source 优先级收敛：manual 覆盖 litellm
// （与表内"一行 = 最终生效价"、拉取 upsert 的行级互斥语义一致）。
func buildPricingSnapshot(rows []*domain.Pricing) map[string]*domain.Pricing {
	m := make(map[string]*domain.Pricing, len(rows))
	for _, p := range rows {
		if prev, ok := m[p.Model]; ok && prev.Source == domain.PricingSourceManual {
			continue // 已收敛为 manual：后续行（含 manual/litellm）一律不覆盖
		}
		m[p.Model] = p
	}
	return m
}

// GetPrice 快照读价（Phase 5 计费热路径，零 DB）：命中返回价格行；缺失 →
// ErrNotFound（计费方应拒绝计费而非按 0 计价——M-1 语义，本任务注明）。
func (s *Service) GetPrice(model string) (*domain.Pricing, error) {
	if m := s.pricing.Load(); m != nil {
		if p, ok := (*m)[model]; ok {
			return p, nil
		}
	}
	// G3-2 分层约定：对外（响应体）恒英文、内部可中文——本错误仅内部消费
	// （forward.go/caller.go 只 log.Warn，不向响应体回显；402 走固定 errNoPrice
	// 文案），中文保留可读性。
	return nil, fmt.Errorf("%w: model=%q（无价格数据：请管理端设价或等待 litellm 同步）", ErrNotFound, model)
}

// ServiceTierPolicy 读取 service_tier 转发策略（settings 快照，零 DB；
// BillingHooks.TierPolicy 实现，main 装配）：priority/flex/fast 分别按
// service_tier_policy_priority / service_tier_policy_flex /
// service_tier_policy_fast 取值；缺失/未知值 → passthrough 默认（auto/空恒
// 透传在 proxy 侧短路，不查策略）。
func (s *Service) ServiceTierPolicy(tier billing.Tier) billing.TierPolicyMode {
	key := "service_tier_policy_priority"
	if tier == billing.TierFlex {
		key = "service_tier_policy_flex"
	}
	if tier == billing.TierFast {
		key = "service_tier_policy_fast"
	}
	switch s.settingValue(key) {
	case "strip":
		return billing.TierPolicyStrip
	case "reject":
		return billing.TierPolicyReject
	default:
		return billing.TierPolicyPassthrough
	}
}

// UpsertManualPricing 手动设价（管理端 PUT /api/admin/pricing?model=X）：校验 + 落库
// （upsert 强制 source=manual，可接管 litellm 行）+ 成功后重载快照（读路径
// 即时生效）。PricingManual 可选字段（nil = 清空）校验语义：非 nil 且 < 0 →
// 400（与主价一致）；FastMultiplier 万分数 0 < m ≤ 100000（×1.0..×10.0，评审
// I-2 上限——Anthropic Fast Mode 实测 ≤ ×6.0）。manual 行不写 raw/provider/mode。
func (s *Service) UpsertManualPricing(ctx context.Context, m *repository.PricingManual) (*domain.Pricing, error) {
	if m.Model == "" {
		return nil, fmt.Errorf("%w: model is required", ErrInvalidInput)
	}
	if m.PromptPricePerMillion < 0 || m.CompletionPricePerMillion < 0 {
		return nil, fmt.Errorf("%w: prices must be >= 0", ErrInvalidInput)
	}
	nonNeg := func(v *int64, name string) error {
		if v != nil && *v < 0 {
			return fmt.Errorf("%w: %s must be >= 0", ErrInvalidInput, name)
		}
		return nil
	}
	// 可选字段逐个校验（显式字段序 → 报错字段名确定，评审 I-1：map 迭代顺序随机）。
	for _, f := range []struct {
		name string
		v    *int64
	}{
		{"cache_read", m.CacheReadPricePerMillion}, {"cache_creation", m.CacheCreationPricePerMillion},
		{"priority_prompt", m.PriorityPromptPricePerMillion}, {"priority_completion", m.PriorityCompletionPricePerMillion},
		{"priority_cache_read", m.PriorityCacheReadPricePerMillion}, {"priority_cache_creation", m.PriorityCacheCreationPricePerMillion},
		{"flex_prompt", m.FlexPromptPricePerMillion}, {"flex_completion", m.FlexCompletionPricePerMillion},
		{"flex_cache_read", m.FlexCacheReadPricePerMillion}, {"flex_cache_creation", m.FlexCacheCreationPricePerMillion},
		{"above_threshold", m.AboveThreshold},
		{"above_prompt", m.AbovePromptPricePerMillion}, {"above_completion", m.AboveCompletionPricePerMillion},
		{"above_cache_read", m.AboveCacheReadPricePerMillion}, {"above_cache_creation", m.AboveCacheCreationPricePerMillion},
		{"above_priority_prompt", m.AbovePriorityPromptPricePerMillion}, {"above_priority_completion", m.AbovePriorityCompletionPricePerMillion},
		{"above_priority_cache_read", m.AbovePriorityCacheReadPricePerMillion}, {"above_priority_cache_creation", m.AbovePriorityCacheCreationPricePerMillion},
		{"above_flex_prompt", m.AboveFlexPromptPricePerMillion}, {"above_flex_completion", m.AboveFlexCompletionPricePerMillion},
		{"above_flex_cache_read", m.AboveFlexCacheReadPricePerMillion}, {"above_flex_cache_creation", m.AboveFlexCacheCreationPricePerMillion},
	} {
		if err := nonNeg(f.v, f.name); err != nil {
			return nil, err
		}
	}
	if m.FastMultiplier != nil && (*m.FastMultiplier <= 0 || *m.FastMultiplier > 100000) {
		return nil, fmt.Errorf("%w: fast_multiplier must be in (0, 100000] (×1.0..×10.0)", ErrInvalidInput)
	}
	p, err := s.store.UpsertManual(ctx, m)
	if err != nil {
		return nil, err
	}
	s.reloadPricing(ctx)
	return p, nil
}

// DeleteManualPricing 删除手动价（管理端 DELETE /api/admin/pricing?model=X）：仅
// source=manual 行可删（litellm 行 → ErrConflict；仓库语义）；成功后重载快照
// ——该 model 从快照消失（缺失窗口内 GetPrice → ErrNotFound，下轮拉取补回）。
func (s *Service) DeleteManualPricing(ctx context.Context, model string) error {
	if err := s.store.DeleteManual(ctx, model); err != nil {
		return mapRepoErr(err)
	}
	s.reloadPricing(ctx)
	return nil
}

// ListPricing 管理端价格列表（GET /api/admin/pricing）：分页 + source/provider/model
// 筛选 + sort 白名单校验（非法 → ErrInvalidInput 400）。source 枚举非法 → 400
// （handler 显式校验已做，service 兜底双保险）；provider 为自由字符串等值
// （DB 侧不限于 openapi Provider enum——litellm_provider 动态，新厂商可筛）。
func (s *Service) ListPricing(ctx context.Context, q repository.ListQuery, source *domain.PricingSource, provider, model string) ([]*domain.Pricing, int64, error) {
	if source != nil && !source.Valid() {
		return nil, 0, fmt.Errorf("%w: invalid source %q", ErrInvalidInput, *source)
	}
	if err := validateListQuery(q, listSortFields["pricing"]); err != nil {
		return nil, 0, err
	}
	return s.store.ListPricing(ctx, q, source, provider, model)
}

// SetPriceFetcher 注入价格拉取器（管理端手动 sync 用；与 SyncWorker 共享同一
// fetcher 实例——main 装配）。重复调用覆盖；nil 注入会被下次 SyncPricingNow
// 以错误拒绝。
func (s *Service) SetPriceFetcher(f pricing.Fetcher) { s.priceFetcher = f }

// SyncPricingNow 手动触发一次价格同步（管理端 POST /api/admin/pricing/sync）：与
// SyncWorker.Sync 同路径语义——fetch（price_source_url settings 快照，零 DB）
// → 文本价 UpsertFromLiteLLM + image 价 UpsertImageFromLiteLLM + 按单元价
// UpsertFunctionFromLiteLLM（三线独立判定独立落库，均 manual 行级互斥，永不
// 覆盖手动价）→ 三线快照重载。与 cron 并发安全（幂等 upsert，最坏浪费一次
// fetch——M-3 语义）。错误映射：拉取失败 → ErrPriceFetch（502）；url 未配置 →
// ErrInvalidInput（400）；upsert 失败 → 原样（500；文本价错误优先，无则透传
// image 价错误、再透传 function 价错误——三线均部分成功可接受）。返回拉取
// 统计（含 image/function 行统计）。
func (s *Service) SyncPricingNow(ctx context.Context) (*PricingSyncStats, error) {
	if s.priceFetcher == nil {
		return nil, errors.New("pricing: fetcher not injected")
	}
	url := s.settingValue("price_source_url")
	if url == "" {
		return nil, fmt.Errorf("%w: price_source_url not set, skip sync", ErrInvalidInput)
	}
	res, err := s.priceFetcher.Fetch(ctx, url)
	if err != nil {
		// G3-1：502 响应体为固定文案（handler/pricing.go PostPricingSync），
		// 上游详情（含 sourceURL）仅在此 Warn 落日志——防经 admin 面回显外泄。
		if s.log != nil {
			s.log.Warn("pricing sync failed", logx.Error(err))
		}
		// %w: %w 多重包装保链：%v 会断链，errors.Is 无法命中 fetch 层错误。
		return nil, fmt.Errorf("%w: %w", ErrPriceFetch, err)
	}
	n, err := s.store.UpsertFromLiteLLM(ctx, res.Rows)
	nImage, imgErr := s.store.UpsertImageFromLiteLLM(ctx, res.ImageRows)
	nFunction, fnErr := s.store.UpsertFunctionFromLiteLLM(ctx, res.FunctionRows)
	if err == nil {
		err = imgErr
	}
	if err == nil {
		err = fnErr
	}
	// upsert 部分成功后仍刷新三线快照（已落库的批立即生效）；fetch 失败则数据
	// 未变，保留旧快照（上方因错误直接返回）。
	s.reloadPricing(ctx)
	s.reloadImagePricing(ctx)
	s.reloadFunctionPricing(ctx)
	if err != nil {
		return nil, err
	}
	return &PricingSyncStats{
		Rows: len(res.Rows), Skipped: res.Skipped, Updated: n,
		ImageRows: len(res.ImageRows), ImageUpdated: nImage,
		FunctionRows: len(res.FunctionRows), FunctionUpdated: nFunction,
	}, nil
}

// PriceSourceURL 价格拉取源 URL（pricing.SyncWorker 的 SettingReader 实现；
// settings 快照读，零 DB）。
func (s *Service) PriceSourceURL() string { return s.settingValue("price_source_url") }

// PriceSyncCron 价格同步 cron 表达式（pricing.SyncWorker 的 SettingReader 实现；
// settings 快照读，零 DB）。
func (s *Service) PriceSyncCron() string { return s.settingValue("price_sync_cron") }
