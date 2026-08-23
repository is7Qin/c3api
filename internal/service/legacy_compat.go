// SPDX-License-Identifier: AGPL-3.0-or-later
package service

import (
	"context"
	"fmt"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// Legacy wrappers delegating old Pricing APIs to unified table.

func (s *Service) GetPrice(model string) (*domain.Pricing, error) {
	pe, err := s.GetPriceEntry(context.Background(), model)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if pe.Mode != domain.PriceModeToken || pe.InputPerM == nil || pe.OutputPerM == nil {
		return nil, fmt.Errorf("%w: model=%q", ErrNotFound, model)
	}
	return &domain.Pricing{Model: pe.Model, PromptPricePerMillion: *pe.InputPerM, CompletionPricePerMillion: *pe.OutputPerM, CacheReadPricePerMillion: pe.CacheReadPerM, CacheCreationPricePerMillion: pe.CacheWritePerM, Provider: pe.Provider, Raw: pe.Raw, Source: pe.Source, CreatedAt: pe.CreatedAt, UpdatedAt: pe.UpdatedAt}, nil
}
func (s *Service) GetImagePrice(model string) (*domain.ImagePrice, error) {
	pe, err := s.GetPriceEntry(context.Background(), model)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return &domain.ImagePrice{Model: pe.Model, InputImageTokenPricePerMillion: pe.ImgInTokPerM, OutputImageTokenPricePerMillion: pe.ImgOutTokPerM, OutputCostPerImageMilli: pe.PricePerImage, Provider: pe.Provider, Raw: pe.Raw, Source: pe.Source, CreatedAt: pe.CreatedAt, UpdatedAt: pe.UpdatedAt}, nil
}
func (s *Service) GetFunctionPrice(model string) (*domain.FunctionPrice, error) {
	pe, err := s.GetPriceEntry(context.Background(), model)
	if err != nil {
		if model == domain.CodexSearchModel {
			v := domain.DefaultCodexSearchPricePerCall
			return &domain.FunctionPrice{Model: model, PricePerCall: &v, Source: domain.PricingSourceManual}, nil
		}
		return nil, mapRepoErr(err)
	}
	return &domain.FunctionPrice{Model: pe.Model, PricePerCall: pe.PricePerCall, Provider: pe.Provider, Raw: pe.Raw, Source: pe.Source, CreatedAt: pe.CreatedAt, UpdatedAt: pe.UpdatedAt}, nil
}
func (s *Service) ListPricing(ctx context.Context, q repository.ListQuery, source *domain.PricingSource, provider, model string) ([]*domain.Pricing, int64, error) {
	if source != nil && !source.Valid() {
		return nil, 0, fmt.Errorf("%w: invalid source %q", ErrInvalidInput, *source)
	}
	if err := validateListQuery(q, listSortFields["pricing"]); err != nil {
		return nil, 0, err
	}
	return s.store.ListPricing(ctx, q, source, provider, model)
}
func (s *Service) ListImagePrice(ctx context.Context, q repository.ListQuery, source *domain.PricingSource, provider, model string) ([]*domain.ImagePrice, int64, error) {
	if source != nil && !source.Valid() {
		return nil, 0, fmt.Errorf("%w: invalid source %q", ErrInvalidInput, *source)
	}
	if err := validateListQuery(q, listSortFields["image_price"]); err != nil {
		return nil, 0, err
	}
	return s.store.ListImagePrice(ctx, q, source, provider, model)
}
func (s *Service) ListFunctionPrice(ctx context.Context, q repository.ListQuery, source *domain.PricingSource, provider, model string) ([]*domain.FunctionPrice, int64, error) {
	if source != nil && !source.Valid() {
		return nil, 0, fmt.Errorf("%w: invalid source %q", ErrInvalidInput, *source)
	}
	if err := validateListQuery(q, listSortFields["function_price"]); err != nil {
		return nil, 0, err
	}
	return s.store.ListFunctionPrice(ctx, q, source, provider, model)
}
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
func (s *Service) UpsertManualImagePrice(ctx context.Context, m *repository.ImagePriceManual) (*domain.ImagePrice, error) {
	if m.Model == "" {
		return nil, fmt.Errorf("%w: model is required", ErrInvalidInput)
	}
	if !m.HasAnyPrice() {
		return nil, fmt.Errorf("%w: at least one image price component is required", ErrInvalidInput)
	}
	nonNeg := func(v *int64, name string) error {
		if v != nil && *v < 0 {
			return fmt.Errorf("%w: %s must be >= 0", ErrInvalidInput, name)
		}
		return nil
	}
	for _, f := range []struct {
		name string
		v    *int64
	}{
		{"input_image_token_price_per_million", m.InputImageTokenPricePerMillion},
		{"output_image_token_price_per_million", m.OutputImageTokenPricePerMillion},
		{"output_cost_per_image", m.OutputCostPerImageMilli},
	} {
		if err := nonNeg(f.v, f.name); err != nil {
			return nil, err
		}
	}
	p, err := s.store.UpsertImageManual(ctx, m)
	if err != nil {
		return nil, err
	}
	s.reloadPricing(ctx)
	return p, nil
}
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
	s.reloadPricing(ctx)
	return p, nil
}
func (s *Service) DeleteManualPricing(ctx context.Context, model string) error {
	if err := s.store.DeleteManual(ctx, model); err != nil {
		return mapRepoErr(err)
	}
	s.reloadPricing(ctx)
	return nil
}
func (s *Service) DeleteManualImagePrice(ctx context.Context, model string) error {
	if err := s.store.DeleteImageManual(ctx, model); err != nil {
		return mapRepoErr(err)
	}
	s.reloadPricing(ctx)
	return nil
}
func (s *Service) DeleteManualFunctionPrice(ctx context.Context, model string) error {
	if err := s.store.DeleteFunctionManual(ctx, model); err != nil {
		return mapRepoErr(err)
	}
	s.reloadPricing(ctx)
	return nil
}
func (s *Service) GetPriceEntry(ctx context.Context, model string) (*domain.PriceEntry, error) {
	pe, err := s.store.GetPriceEntry(ctx, model)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return pe, nil
}
func (s *Service) ReloadImagePricing()                                { s.ReloadPricing() }
func (s *Service) ReloadImagePricingCtx(ctx context.Context) error    { return s.ReloadPricingCtx(ctx) }
func (s *Service) ReloadFunctionPricing()                             { s.ReloadPricing() }
func (s *Service) ReloadFunctionPricingCtx(ctx context.Context) error { return s.ReloadPricingCtx(ctx) }

func buildPricingSnapshot(rows []*domain.Pricing) map[string]*domain.Pricing {
	m := make(map[string]*domain.Pricing, len(rows))
	for _, p := range rows {
		if prev, ok := m[p.Model]; ok && prev.Source == domain.PricingSourceManual {
			continue
		}
		m[p.Model] = p
	}
	return m
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
