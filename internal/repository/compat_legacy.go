// SPDX-License-Identifier: AGPL-3.0-or-later
package repository

import (
	"context"
	"fmt"

	"github.com/is7qin/c3api/internal/domain"
)

// Legacy wrappers for old tests / handlers that still reference old Pricing tables.
// They delegate to unified price_entries with mode filtering.

type PricingManual struct {
	Model                                     string
	PromptPricePerMillion                     int64
	CompletionPricePerMillion                 int64
	CacheReadPricePerMillion                  *int64
	CacheCreationPricePerMillion              *int64
	PriorityPromptPricePerMillion             *int64
	PriorityCompletionPricePerMillion         *int64
	PriorityCacheReadPricePerMillion          *int64
	PriorityCacheCreationPricePerMillion      *int64
	FlexPromptPricePerMillion                 *int64
	FlexCompletionPricePerMillion             *int64
	FlexCacheReadPricePerMillion              *int64
	FlexCacheCreationPricePerMillion          *int64
	AboveThreshold                            *int64
	AbovePromptPricePerMillion                *int64
	AboveCompletionPricePerMillion            *int64
	AboveCacheReadPricePerMillion             *int64
	AboveCacheCreationPricePerMillion         *int64
	AbovePriorityPromptPricePerMillion        *int64
	AbovePriorityCompletionPricePerMillion    *int64
	AbovePriorityCacheReadPricePerMillion     *int64
	AbovePriorityCacheCreationPricePerMillion *int64
	AboveFlexPromptPricePerMillion            *int64
	AboveFlexCompletionPricePerMillion        *int64
	AboveFlexCacheReadPricePerMillion         *int64
	AboveFlexCacheCreationPricePerMillion     *int64
	FastMultiplier                            *int64
}

type ImagePriceManual struct {
	Model                           string
	InputImageTokenPricePerMillion  *int64
	OutputImageTokenPricePerMillion *int64
	OutputCostPerImageMilli         *int64
}

func (m *ImagePriceManual) HasAnyPrice() bool {
	return m.InputImageTokenPricePerMillion != nil || m.OutputImageTokenPricePerMillion != nil || m.OutputCostPerImageMilli != nil
}

type FunctionPriceManual struct {
	Model        string
	PricePerCall *int64
}

func (m *FunctionPriceManual) HasAnyPrice() bool { return m.PricePerCall != nil }

// UpsertFromLiteLLM legacy for Pricing (pricing worker)
func (r *Repository) UpsertFromLiteLLM(ctx context.Context, rows []*domain.Pricing) (int, error) {
	var entries []*domain.PriceEntry
	for _, p := range rows {
		pe := &domain.PriceEntry{Model: p.Model, Mode: domain.PriceModeToken, InputPerM: &p.PromptPricePerMillion, OutputPerM: &p.CompletionPricePerMillion, CacheReadPerM: p.CacheReadPricePerMillion, CacheWritePerM: p.CacheCreationPricePerMillion, Provider: p.Provider, MaxInputTokens: p.MaxInputTokens, MaxOutputTokens: p.MaxOutputTokens, SupportsPromptCaching: p.SupportsPromptCaching, Raw: p.Raw, Source: p.Source}
		entries = append(entries, pe)
	}
	return r.PriceEntries.UpsertFromLiteLLM(ctx, entries)
}
func (r *Repository) UpsertImageFromLiteLLM(ctx context.Context, rows []*domain.ImagePrice) (int, error) {
	var entries []*domain.PriceEntry
	for _, p := range rows {
		pe := &domain.PriceEntry{Model: p.Model, Mode: domain.PriceModeImage, ImgInTokPerM: p.InputImageTokenPricePerMillion, ImgOutTokPerM: p.OutputImageTokenPricePerMillion, PricePerImage: p.OutputCostPerImageMilli, Provider: p.Provider, Raw: p.Raw, Source: p.Source}
		entries = append(entries, pe)
	}
	return r.PriceEntries.UpsertFromLiteLLM(ctx, entries)
}
func (r *Repository) UpsertFunctionFromLiteLLM(ctx context.Context, rows []*domain.FunctionPrice) (int, error) {
	var entries []*domain.PriceEntry
	for _, p := range rows {
		pe := &domain.PriceEntry{Model: p.Model, Mode: domain.PriceModeCall, PricePerCall: p.PricePerCall, Provider: p.Provider, Raw: p.Raw, Source: p.Source}
		entries = append(entries, pe)
	}
	return r.PriceEntries.UpsertFromLiteLLM(ctx, entries)
}

func (r *Repository) UpsertManual(ctx context.Context, m *PricingManual) (*domain.Pricing, error) {
	pm := &PriceEntryManual{Model: m.Model, Mode: domain.PriceModeToken, InputPerM: &m.PromptPricePerMillion, OutputPerM: &m.CompletionPricePerMillion, CacheReadPerM: m.CacheReadPricePerMillion, CacheWritePerM: m.CacheCreationPricePerMillion}
	if _, err := r.PriceEntries.UpsertManual(ctx, pm); err != nil {
		return nil, err
	}
	return r.GetPricing(ctx, m.Model)
}
func (r *Repository) DeleteManual(ctx context.Context, model string) error {
	return r.PriceEntries.DeleteManual(ctx, model)
}
func (r *Repository) ListPricing(ctx context.Context, q ListQuery, source *domain.PricingSource, provider, model string) ([]*domain.Pricing, int64, error) {
	entries, total, err := r.PriceEntries.ListPriceEntries(ctx, q, source, func() *domain.PriceMode { m := domain.PriceModeToken; return &m }(), model)
	if err != nil {
		return nil, 0, err
	}
	var out []*domain.Pricing
	for _, e := range entries {
		if e.InputPerM == nil || e.OutputPerM == nil {
			continue
		}
		if provider != "" && (e.Provider == nil || *e.Provider != provider) {
			continue
		}
		p := &domain.Pricing{Model: e.Model, PromptPricePerMillion: *e.InputPerM, CompletionPricePerMillion: *e.OutputPerM, CacheReadPricePerMillion: e.CacheReadPerM, CacheCreationPricePerMillion: e.CacheWritePerM, Provider: e.Provider, MaxInputTokens: e.MaxInputTokens, MaxOutputTokens: e.MaxOutputTokens, SupportsPromptCaching: e.SupportsPromptCaching, Raw: e.Raw, Source: e.Source, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt}
		out = append(out, p)
	}
	return out, total, nil
}
func (r *Repository) GetPricing(ctx context.Context, model string) (*domain.Pricing, error) {
	e, err := r.PriceEntries.GetPriceEntry(ctx, model)
	if err != nil {
		return nil, err
	}
	if e.Mode != domain.PriceModeToken {
		return nil, fmt.Errorf("%w: model=%q", ErrNotFound, model)
	}
	if e.InputPerM == nil || e.OutputPerM == nil {
		return nil, fmt.Errorf("%w: model=%q", ErrNotFound, model)
	}
	return &domain.Pricing{Model: e.Model, PromptPricePerMillion: *e.InputPerM, CompletionPricePerMillion: *e.OutputPerM, CacheReadPricePerMillion: e.CacheReadPerM, CacheCreationPricePerMillion: e.CacheWritePerM, Provider: e.Provider, MaxInputTokens: e.MaxInputTokens, MaxOutputTokens: e.MaxOutputTokens, SupportsPromptCaching: e.SupportsPromptCaching, Raw: e.Raw, Source: e.Source, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt}, nil
}

func (r *Repository) UpsertImageManual(ctx context.Context, m *ImagePriceManual) (*domain.ImagePrice, error) {
	pm := &PriceEntryManual{Model: m.Model, Mode: domain.PriceModeImage, ImgInTokPerM: m.InputImageTokenPricePerMillion, ImgOutTokPerM: m.OutputImageTokenPricePerMillion, PricePerImage: m.OutputCostPerImageMilli}
	if _, err := r.PriceEntries.UpsertManual(ctx, pm); err != nil {
		return nil, err
	}
	return r.GetImagePrice(ctx, m.Model)
}
func (r *Repository) DeleteImageManual(ctx context.Context, model string) error {
	return r.PriceEntries.DeleteManual(ctx, model)
}
func (r *Repository) ListImagePrice(ctx context.Context, q ListQuery, source *domain.PricingSource, provider, model string) ([]*domain.ImagePrice, int64, error) {
	entries, total, err := r.PriceEntries.ListPriceEntries(ctx, q, source, func() *domain.PriceMode { m := domain.PriceModeImage; return &m }(), model)
	if err != nil {
		return nil, 0, err
	}
	var out []*domain.ImagePrice
	for _, e := range entries {
		out = append(out, &domain.ImagePrice{Model: e.Model, InputImageTokenPricePerMillion: e.ImgInTokPerM, OutputImageTokenPricePerMillion: e.ImgOutTokPerM, OutputCostPerImageMilli: e.PricePerImage, Provider: e.Provider, Raw: e.Raw, Source: e.Source, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt})
	}
	return out, total, nil
}
func (r *Repository) GetImagePrice(ctx context.Context, model string) (*domain.ImagePrice, error) {
	e, err := r.PriceEntries.GetPriceEntry(ctx, model)
	if err != nil {
		return nil, err
	}
	return &domain.ImagePrice{Model: e.Model, InputImageTokenPricePerMillion: e.ImgInTokPerM, OutputImageTokenPricePerMillion: e.ImgOutTokPerM, OutputCostPerImageMilli: e.PricePerImage, Provider: e.Provider, Raw: e.Raw, Source: e.Source, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt}, nil
}

func (r *Repository) UpsertFunctionManual(ctx context.Context, m *FunctionPriceManual) (*domain.FunctionPrice, error) {
	pm := &PriceEntryManual{Model: m.Model, Mode: domain.PriceModeCall, PricePerCall: m.PricePerCall}
	if _, err := r.PriceEntries.UpsertManual(ctx, pm); err != nil {
		return nil, err
	}
	return r.GetFunctionPrice(ctx, m.Model)
}
func (r *Repository) DeleteFunctionManual(ctx context.Context, model string) error {
	return r.PriceEntries.DeleteManual(ctx, model)
}
func (r *Repository) ListFunctionPrice(ctx context.Context, q ListQuery, source *domain.PricingSource, provider, model string) ([]*domain.FunctionPrice, int64, error) {
	entries, total, err := r.PriceEntries.ListPriceEntries(ctx, q, source, func() *domain.PriceMode { m := domain.PriceModeCall; return &m }(), model)
	if err != nil {
		return nil, 0, err
	}
	var out []*domain.FunctionPrice
	for _, e := range entries {
		out = append(out, &domain.FunctionPrice{Model: e.Model, PricePerCall: e.PricePerCall, Provider: e.Provider, Raw: e.Raw, Source: e.Source, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt})
	}
	return out, total, nil
}
func (r *Repository) GetFunctionPrice(ctx context.Context, model string) (*domain.FunctionPrice, error) {
	e, err := r.PriceEntries.GetPriceEntry(ctx, model)
	if err != nil {
		return nil, err
	}
	return &domain.FunctionPrice{Model: e.Model, PricePerCall: e.PricePerCall, Provider: e.Provider, Raw: e.Raw, Source: e.Source, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt}, nil
}
func (r *Repository) EnsureFunctionPriceSeed(ctx context.Context) error {
	_, err := r.PriceEntries.GetPriceEntry(ctx, domain.CodexSearchModel)
	if err == nil {
		return nil
	}
	pm := &PriceEntryManual{Model: domain.CodexSearchModel, Mode: domain.PriceModeCall}
	v := domain.DefaultCodexSearchPricePerCall
	pm.PricePerCall = &v
	_, err = r.PriceEntries.UpsertManual(ctx, pm)
	return err
}
