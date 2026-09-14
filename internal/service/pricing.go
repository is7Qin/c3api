// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/pricing"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/logx"
)

var ErrPriceFetch = errors.New("service: price fetch failed")

type PricingSyncStats struct {
	Rows     int
	Skipped  int
	Updated  int
	Variants int
}

type PricingPreview struct {
	ToAdd           int                   `json:"to_add"`
	ToUpdate        int                   `json:"to_update"`
	Skipped         int                   `json:"skipped"`
	Entries         []PricingPreviewEntry `json:"entries"`
	VariantsChanged int                   `json:"variants_changed"`
}

type PricingPreviewEntry struct {
	Model  string `json:"model"`
	Mode   string `json:"mode"`
	Action string `json:"action"` // add/update
}

type priceSnapshot struct {
	entries  map[string]*domain.PriceEntry
	variants map[string][]*domain.PriceVariant
}

const pricingReloadPage = 1000

// SetCompileNotifier 注入路由编译触发（装配期回填，SetRecoverProber 同款：
// 可选函数面，nil = 未装配）。生产装配 scheduler.RequestCompile（非阻塞
// select/default，下游 200ms 去抖收敛）——定价写面只在解析价格真实变化时
// 调用，同值写静默。
func (s *Service) SetCompileNotifier(fn func()) { s.compileNotify = fn }

// ReloadPricingAndNotifyCompiler 快照重载 + 编译通知（定价写面统一出口）：
// 重载前后解析价格不变 → 静默（同值 PUT 不驱逐重编译）；变化 → 非阻塞通知。
// 管理面冷路径，O(模型数) 纯内存比较。
func (s *Service) ReloadPricingAndNotifyCompiler() {
	s.reloadPricingAndNotifyCompiler(context.Background())
}

func (s *Service) reloadPricingAndNotifyCompiler(ctx context.Context) {
	// 未装配编译通知面 = 纯重载（测试/降级路径），连 before 快照都省了。
	// 定价写面统一出口：全部 6 处写面（5 手工写面 + pricing sync worker 的
	// Reload 回调）汇聚于此；跨实例传播不依赖编译通知的变化检测，无条件发布。
	if s.compileNotify == nil {
		s.reloadPricing(ctx)
		s.publish(ctx, notify.Change{Pricing: true})
		return
	}
	at := time.Now()
	before := s.ResolvedPricesByModel(at)
	s.reloadPricing(ctx)
	after := s.ResolvedPricesByModel(at)
	// 相等性唯一主人是编译道（scheduler.EqualResolvedPrices）：service 不再
	// 手写对偶比较，字段增减自动跟随。
	if !maps.EqualFunc(before, after, scheduler.EqualResolvedPrices) {
		s.compileNotify()
	}
	// 跨实例传播不依赖上面的变化检测：同值写本地静默编译，但对端实例仍需
	// 重载（快照内容一致则重载无害）。
	s.publish(ctx, notify.Change{Pricing: true})
}
func (s *Service) ReloadPricingCtx(ctx context.Context) error {
	m, err := s.loadPricingSnapshot(ctx)
	if err != nil {
		return err
	}
	s.priceSnapshot.Store(m)
	return nil
}
func (s *Service) reloadPricing(ctx context.Context) {
	m, err := s.loadPricingSnapshot(ctx)
	if err != nil {
		return
	}
	s.priceSnapshot.Store(m)
}
func (s *Service) loadPricingSnapshot(ctx context.Context) (*priceSnapshot, error) {
	var all []*domain.PriceEntry
	for offset := 0; ; offset += pricingReloadPage {
		rows, _, err := s.store.ListPriceEntries(ctx, repository.ListQuery{Limit: pricingReloadPage, Offset: offset, Sort: "model", Order: "asc"}, nil, nil, nil, "")
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
	variants, err := s.store.ListAllPriceVariants(ctx)
	if err != nil {
		if s.log != nil {
			s.log.Warn("pricing variant snapshot reload failed", logx.Error(err))
		}
		return nil, err
	}
	entriesMap := make(map[string]*domain.PriceEntry, len(all))
	for _, e := range all {
		entriesMap[e.Model] = e
	}
	vMap := make(map[string][]*domain.PriceVariant)
	for _, v := range variants {
		vMap[v.Model] = append(vMap[v.Model], v)
	}
	for _, lst := range vMap {
		slices.SortFunc(lst, func(a, b *domain.PriceVariant) int { return cmp.Compare(a.Seq, b.Seq) })
	}
	return &priceSnapshot{entries: entriesMap, variants: vMap}, nil
}

// ResolvePrices 模型价格解析：快照零 DB 读 + 委托 domain 解析核
// （entry→基底→首中变体；纯函数与测试假实现共用，防逻辑漂移）。
func (s *Service) ResolvePrices(model string, promptTokens int64, tier string, at time.Time) (domain.ResolvedPrices, bool) {
	snap := s.priceSnapshot.Load()
	if snap == nil {
		return domain.ResolvedPrices{}, false
	}
	if s.tzLoc != nil {
		at = at.In(s.tzLoc)
	}
	return domain.ResolveEntryPrices((*snap).entries[model], (*snap).variants[model], tier, promptTokens, at)
}

// ResolvedPricesByModel 编译道价格源：整张定价快照 → 模型→生效单价映射
// （routing compiler pricesFn 的装配面）。基底解析口径：tier 空、
// promptTokens=0、at 由调用方给定（经 tzLoc 归一，与 ResolvePrices 同纪律）；
// 解析失败（无 entry）的模型缺席——compiler 侧 costKnown=false 落 Explore，
// 与缺价语义一致。快照未加载返回 nil（编译道按无价处理，不 panic）。
// 冷路径（compile lane 后台专用），O(模型数) 纯内存。
func (s *Service) ResolvedPricesByModel(at time.Time) map[string]domain.ResolvedPrices {
	snap := s.priceSnapshot.Load()
	if snap == nil {
		return nil
	}
	if s.tzLoc != nil {
		at = at.In(s.tzLoc)
	}
	out := make(map[string]domain.ResolvedPrices, len((*snap).entries))
	for model, entry := range (*snap).entries {
		if rp, ok := domain.ResolveEntryPrices(entry, (*snap).variants[model], "", 0, at); ok {
			out[model] = rp
		}
	}
	return out
}

// validation helpers
func (s *Service) UpsertPriceEntry(ctx context.Context, m *repository.PriceEntryManual) (*domain.PriceEntry, error) {
	if m.Model == "" {
		return nil, fmt.Errorf("%w: model is required", ErrInvalidInput)
	}
	if !m.Mode.Valid() {
		return nil, fmt.Errorf("%w: invalid mode %q", ErrInvalidInput, m.Mode)
	}
	switch m.Mode {
	case domain.PriceModeToken:
		if m.InputPerM == nil || m.OutputPerM == nil {
			return nil, fmt.Errorf("%w: token mode requires input_per_m+output_per_m", ErrInvalidInput)
		}
	case domain.PriceModeCall:
		if m.PricePerCall == nil {
			return nil, fmt.Errorf("%w: call mode requires price_per_call", ErrInvalidInput)
		}
	case domain.PriceModeImage:
		if m.ImgInTokPerM == nil && m.ImgOutTokPerM == nil && m.PricePerImage == nil {
			return nil, fmt.Errorf("%w: image mode requires at least one image component", ErrInvalidInput)
		}
	}
	nonNeg := func(v *int64, name string) error {
		if v != nil && *v < 0 {
			return fmt.Errorf("%w: %s must be >=0", ErrInvalidInput, name)
		}
		return nil
	}
	for _, f := range []struct {
		name string
		v    *int64
	}{
		{"input_per_m", m.InputPerM}, {"output_per_m", m.OutputPerM}, {"cache_read_per_m", m.CacheReadPerM}, {"cache_write_per_m", m.CacheWritePerM},
		{"price_per_call", m.PricePerCall}, {"img_in_tok_per_m", m.ImgInTokPerM}, {"img_out_tok_per_m", m.ImgOutTokPerM}, {"price_per_image", m.PricePerImage},
	} {
		if err := nonNeg(f.v, f.name); err != nil {
			return nil, err
		}
	}
	p, err := s.store.UpsertPriceEntryManual(ctx, m)
	if err != nil {
		return nil, err
	}
	s.reloadPricingAndNotifyCompiler(ctx)
	return p, nil
}

func (s *Service) DeletePriceEntry(ctx context.Context, model string) error {
	err := s.store.WithTx(ctx, func(tx repository.TxStore) error {
		if err := tx.DeletePriceVariantsByModel(ctx, model); err != nil {
			return err
		}
		if err := tx.DeletePriceEntryManual(ctx, model); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return mapRepoErr(err)
	}
	s.reloadPricingAndNotifyCompiler(ctx)
	return nil
}

func (s *Service) GetPriceEntry(ctx context.Context, model string) (*domain.PriceEntry, error) {
	pe, err := s.store.GetPriceEntry(ctx, model)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return pe, nil
}

func (s *Service) ListPriceEntries(ctx context.Context, q repository.ListQuery, source *domain.PricingSource, mode *domain.PriceMode, provider *string, model string) ([]*domain.PriceEntry, int64, error) {
	if source != nil && !source.Valid() {
		return nil, 0, fmt.Errorf("%w: invalid source %q", ErrInvalidInput, *source)
	}
	if mode != nil && !mode.Valid() {
		return nil, 0, fmt.Errorf("%w: invalid mode %q", ErrInvalidInput, *mode)
	}
	if err := validateListQuery(q, listSortFields["price_entries"]); err != nil {
		return nil, 0, err
	}
	return s.store.ListPriceEntries(ctx, q, source, mode, provider, model)
}

func (s *Service) ListPriceVariants(ctx context.Context, model string) ([]*domain.PriceVariant, error) {
	return s.store.ListPriceVariants(ctx, model)
}

func (s *Service) ReplacePriceVariants(ctx context.Context, model string, variants []*domain.PriceVariant) ([]*domain.PriceVariant, error) {
	// effect at-least-one check mirrored
	for _, v := range variants {
		if v.MultBP == nil && v.SetInputPerM == nil && v.SetOutputPerM == nil && v.SetCacheReadPerM == nil && v.SetCacheCreationPerM == nil && v.SetPricePerCall == nil && v.SetImgInTokPerM == nil && v.SetImgOutTokPerM == nil && v.SetPricePerImage == nil {
			return nil, fmt.Errorf("%w: variant seq %d requires at least one effect", ErrInvalidInput, v.Seq)
		}
		if v.MultBP != nil && (*v.MultBP < 0 || *v.MultBP > 100000) {
			return nil, fmt.Errorf("%w: variant seq %d multiplier must be in [0,10]", ErrInvalidInput, v.Seq)
		}
	}
	// entry existence check? allow variants for non-existent model? For now allow but warn; service layer still writes.
	out, err := s.store.ReplacePriceVariants(ctx, model, variants)
	if err != nil {
		return nil, err
	}
	s.reloadPricingAndNotifyCompiler(ctx)
	return out, nil
}

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

func (s *Service) SetPriceFetcher(f pricing.Fetcher) { s.priceFetcher = f }

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
		if s.log != nil {
			s.log.Warn("pricing sync failed", logx.Error(err))
		}
		return nil, fmt.Errorf("%w: %w", ErrPriceFetch, err)
	}
	entries := res.PriceEntries
	n, err := s.store.UpsertPriceEntriesFromLiteLLM(ctx, entries)
	if err != nil {
		s.reloadPricingAndNotifyCompiler(ctx)
		return nil, err
	}
	if len(res.Variants) > 0 {
		filtered := res.Variants
		if manualModels, merr := s.store.ManualEntryModels(ctx); merr == nil && len(manualModels) > 0 {
			manualSet := make(map[string]struct{}, len(manualModels))
			for _, m := range manualModels {
				manualSet[m] = struct{}{}
			}
			// 手工定价优先：过滤掉 liteLLM 的同名变体（in-place，与原 retain
			// 循环同语义——DeleteFunc 额外把尾部清零）。
			filtered = slices.DeleteFunc(filtered, func(v *domain.PriceVariant) bool {
				_, isManual := manualSet[v.Model]
				return isManual
			})
		}
		if len(filtered) > 0 {
			if verr := func() error {
				_, e := s.store.UpsertPriceVariantsFromLiteLLM(ctx, filtered)
				return e
			}(); verr != nil {
				err = verr
			}
		}
	}
	s.reloadPricingAndNotifyCompiler(ctx)
	if err != nil {
		return nil, err
	}
	return &PricingSyncStats{Rows: len(entries), Skipped: res.Skipped, Updated: n, Variants: len(res.Variants)}, nil
}

func (s *Service) PreviewPricingSync(ctx context.Context) (*PricingPreview, error) {
	if s.priceFetcher == nil {
		return nil, errors.New("pricing: fetcher not injected")
	}
	url := s.settingValue("price_source_url")
	if url == "" {
		return nil, fmt.Errorf("%w: price_source_url not set, skip sync", ErrInvalidInput)
	}
	res, err := s.priceFetcher.Fetch(ctx, url)
	if err != nil {
		if s.log != nil {
			s.log.Warn("pricing sync preview failed", logx.Error(err))
		}
		return nil, fmt.Errorf("%w: %w", ErrPriceFetch, err)
	}
	entries := res.PriceEntries
	snap := s.priceSnapshot.Load()
	preview := &PricingPreview{Skipped: res.Skipped}
	if snap == nil {
		preview.ToAdd = len(entries)
		for _, e := range entries {
			preview.Entries = append(preview.Entries, PricingPreviewEntry{Model: e.Model, Mode: string(e.Mode), Action: "add"})
		}
		preview.VariantsChanged = len(res.Variants)
		return preview, nil
	}
	for _, e := range entries {
		if _, ok := (*snap).entries[e.Model]; ok {
			preview.ToUpdate++
			preview.Entries = append(preview.Entries, PricingPreviewEntry{Model: e.Model, Mode: string(e.Mode), Action: "update"})
		} else {
			preview.ToAdd++
			preview.Entries = append(preview.Entries, PricingPreviewEntry{Model: e.Model, Mode: string(e.Mode), Action: "add"})
		}
	}
	preview.VariantsChanged = len(res.Variants)
	return preview, nil
}

func (s *Service) PriceSourceURL() string { return s.settingValue("price_source_url") }
func (s *Service) PriceSyncCron() string  { return s.settingValue("price_sync_cron") }
