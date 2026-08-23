package handler

import (
	"context"
	"fmt"
	"sort"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func (f *fakeStore) UpsertPriceEntriesFromLiteLLM(_ context.Context, rows []*domain.PriceEntry) (int, error) {
	n := 0
	for _, r := range rows {
		if _, ok := f.priceEntries[r.Model]; !ok {
			n++
		} else if f.priceEntries[r.Model].Source == domain.PricingSourceManual {
			continue
		} else {
			n++
		}
		f.priceEntries[r.Model] = r
	}
	return n, nil
}
func (f *fakeStore) UpsertPriceEntryManual(_ context.Context, m *repository.PriceEntryManual) (*domain.PriceEntry, error) {
	pe := &domain.PriceEntry{Model: m.Model, Mode: m.Mode, InputPerM: m.InputPerM, OutputPerM: m.OutputPerM, CacheReadPerM: m.CacheReadPerM, CacheWritePerM: m.CacheWritePerM, PricePerCall: m.PricePerCall, ImgInTokPerM: m.ImgInTokPerM, ImgOutTokPerM: m.ImgOutTokPerM, PricePerImage: m.PricePerImage, Source: domain.PricingSourceManual}
	f.priceEntries[m.Model] = pe
	return pe, nil
}
func (f *fakeStore) DeletePriceEntryManual(_ context.Context, model string) error {
	if _, ok := f.priceEntries[model]; !ok {
		return fmt.Errorf("%w: model=%q", repository.ErrNotFound, model)
	}
	if f.priceEntries[model].Source != domain.PricingSourceManual {
		return fmt.Errorf("%w: model=%q source=litellm", repository.ErrConflict, model)
	}
	delete(f.priceEntries, model)
	delete(f.priceVariants, model)
	return nil
}
func (f *fakeStore) ListPriceEntries(_ context.Context, q repository.ListQuery, source *domain.PricingSource, mode *domain.PriceMode, model string) ([]*domain.PriceEntry, int64, error) {
	var all []*domain.PriceEntry
	for _, e := range f.priceEntries {
		if source != nil && e.Source != *source {
			continue
		}
		if mode != nil && e.Mode != *mode {
			continue
		}
		if model != "" && e.Model != model {
			continue
		}
		all = append(all, e)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Model < all[j].Model })
	total := int64(len(all))
	start := q.Offset
	if start < 0 {
		start = 0
	}
	end := start + q.Limit
	if q.Limit <= 0 {
		end = start + 20
	}
	if start > len(all) {
		start = len(all)
	}
	if end > len(all) {
		end = len(all)
	}
	return all[start:end], total, nil
}
func (f *fakeStore) GetPriceEntry(_ context.Context, model string) (*domain.PriceEntry, error) {
	if e, ok := f.priceEntries[model]; ok {
		return e, nil
	}
	return nil, fmt.Errorf("%w: model=%q", repository.ErrNotFound, model)
}
func (f *fakeStore) ListPriceVariants(_ context.Context, model string) ([]*domain.PriceVariant, error) {
	return f.priceVariants[model], nil
}
func (f *fakeStore) ListAllPriceVariants(_ context.Context) ([]*domain.PriceVariant, error) {
	var all []*domain.PriceVariant
	for _, lst := range f.priceVariants {
		all = append(all, lst...)
	}
	return all, nil
}
func (f *fakeStore) ReplacePriceVariants(_ context.Context, model string, variants []*domain.PriceVariant) ([]*domain.PriceVariant, error) {
	f.priceVariants[model] = variants
	return variants, nil
}
