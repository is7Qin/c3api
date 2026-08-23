// SPDX-License-Identifier: AGPL-3.0-or-later
package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/pricing"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
)

var ErrPriceFetch = errors.New("service: price fetch failed")

type PricingSyncStats struct {
	Rows            int
	Skipped         int
	Updated         int
	ImageRows       int
	ImageUpdated    int
	FunctionRows    int
	FunctionUpdated int
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

func (s *Service) ReloadPricing() { s.reloadPricing(context.Background()) }
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
		rows, _, err := s.store.ListPriceEntries(ctx, repository.ListQuery{Limit: pricingReloadPage, Offset: offset, Sort: "model", Order: "asc"}, nil, nil, "")
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
		sort.Slice(lst, func(i, j int) bool { return lst[i].Seq < lst[j].Seq })
	}
	return &priceSnapshot{entries: entriesMap, variants: vMap}, nil
}

// ResolvePrices 模型价格解析：base + 首中即停变体。
func (s *Service) ResolvePrices(model string, promptTokens int64, tier string, at time.Time) (domain.ResolvedPrices, bool) {
	snap := s.priceSnapshot.Load()
	if snap == nil {
		return domain.ResolvedPrices{}, false
	}
	entry, ok := (*snap).entries[model]
	if !ok {
		return domain.ResolvedPrices{}, false
	}
	rp := domain.ResolvedPrices{
		Mode:           entry.Mode,
		InputPerM:      entry.InputPerM,
		OutputPerM:     entry.OutputPerM,
		CacheReadPerM:  entry.CacheReadPerM,
		CacheWritePerM: entry.CacheWritePerM,
		PricePerCall:   entry.PricePerCall,
		ImgInTokPerM:   entry.ImgInTokPerM,
		ImgOutTokPerM:  entry.ImgOutTokPerM,
		PricePerImage:  entry.PricePerImage,
		Provider:       entry.Provider,
	}
	vars := (*snap).variants[model]
	for _, v := range vars {
		if !variantMatches(v, tier, promptTokens, at) {
			continue
		}
		seq := v.Seq
		rp.VariantSeq = &seq
		if v.MultBP != nil {
			mult := int64(*v.MultBP)
			applyMult := func(p **int64) {
				if *p != nil {
					val := (**p*mult + 5000) / 10000
					// need to copy to avoid mutating snapshot; allocate new
					nv := val
					*p = &nv
				}
			}
			applyMult(&rp.InputPerM)
			applyMult(&rp.OutputPerM)
			applyMult(&rp.CacheReadPerM)
			applyMult(&rp.CacheWritePerM)
			applyMult(&rp.PricePerCall)
			applyMult(&rp.ImgInTokPerM)
			applyMult(&rp.ImgOutTokPerM)
			applyMult(&rp.PricePerImage)
		}
		if v.SetInputPerM != nil {
			nv := *v.SetInputPerM
			rp.InputPerM = &nv
		}
		if v.SetOutputPerM != nil {
			nv := *v.SetOutputPerM
			rp.OutputPerM = &nv
		}
		break
	}
	return rp, true
}

func variantMatches(v *domain.PriceVariant, tier string, promptTokens int64, at time.Time) bool {
	if v.ServiceTier != nil && *v.ServiceTier != tier {
		return false
	}
	if v.CtxMin != nil && promptTokens < *v.CtxMin {
		return false
	}
	if v.CtxMax != nil && promptTokens >= *v.CtxMax {
		return false
	}
	if v.TimeStart != nil || v.TimeEnd != nil {
		if !timeMatches(v.TimeStart, v.TimeEnd, at) {
			return false
		}
	}
	if v.DowMask != nil {
		wd := int(at.Weekday()) // 0=Sun
		if (*v.DowMask>>wd)&1 == 0 {
			return false
		}
	}
	return true
}

func timeMatches(start, end *string, at time.Time) bool {
	if start == nil && end == nil {
		return true
	}
	// parse HH:MM
	parse := func(s string) int {
		var h, m int
		fmt.Sscanf(s, "%d:%d", &h, &m)
		return h*60 + m
	}
	cur := at.Hour()*60 + at.Minute()
	if start != nil && end != nil {
		s := parse(*start)
		e := parse(*end)
		if s == e {
			return true
		}
		if s < e {
			return cur >= s && cur < e
		}
		// midnight wrap
		return cur >= s || cur < e
	}
	if start != nil {
		s := parse(*start)
		return cur >= s
	}
	if end != nil {
		e := parse(*end)
		return cur < e
	}
	return true
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
	s.reloadPricing(ctx)
	return p, nil
}

func (s *Service) DeletePriceEntry(ctx context.Context, model string) error {
	if err := s.store.DeletePriceEntryManual(ctx, model); err != nil {
		return mapRepoErr(err)
	}
	s.reloadPricing(ctx)
	return nil
}

func (s *Service) ListPriceEntries(ctx context.Context, q repository.ListQuery, source *domain.PricingSource, mode *domain.PriceMode, model string) ([]*domain.PriceEntry, int64, error) {
	if source != nil && !source.Valid() {
		return nil, 0, fmt.Errorf("%w: invalid source %q", ErrInvalidInput, *source)
	}
	if mode != nil && !mode.Valid() {
		return nil, 0, fmt.Errorf("%w: invalid mode %q", ErrInvalidInput, *mode)
	}
	if err := validateListQuery(q, listSortFields["price_entries"]); err != nil {
		return nil, 0, err
	}
	return s.store.ListPriceEntries(ctx, q, source, mode, model)
}

func (s *Service) ListPriceVariants(ctx context.Context, model string) ([]*domain.PriceVariant, error) {
	return s.store.ListPriceVariants(ctx, model)
}

func (s *Service) ReplacePriceVariants(ctx context.Context, model string, variants []*domain.PriceVariant) ([]*domain.PriceVariant, error) {
	// effect at-least-one check mirrored
	for _, v := range variants {
		if v.MultBP == nil && v.SetInputPerM == nil && v.SetOutputPerM == nil {
			return nil, fmt.Errorf("%w: variant seq %d requires at least one effect", ErrInvalidInput, v.Seq)
		}
	}
	// entry existence check? allow variants for non-existent model? For now allow but warn; service layer still writes.
	out, err := s.store.ReplacePriceVariants(ctx, model, variants)
	if err != nil {
		return nil, err
	}
	s.reloadPricing(ctx)
	return out, nil
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
	// res now contains PriceEntries? Need adapter: pricing.FetchResult still returns old types
	// For refactor, FetchResult should return []*domain.PriceEntry + variants
	// But fetch.go not yet updated; handle both: if PriceEntries present use it, else fallback to Rows conversion
	var entries []*domain.PriceEntry
	if res.PriceEntries != nil {
		entries = res.PriceEntries
	} else {
		// fallback: convert old Pricing to PriceEntry (temporary)
		for _, p := range res.Rows {
			pe := &domain.PriceEntry{
				Model: p.Model, Mode: domain.PriceModeToken,
				InputPerM: &p.PromptPricePerMillion, OutputPerM: &p.CompletionPricePerMillion,
				CacheReadPerM: p.CacheReadPricePerMillion, CacheWritePerM: p.CacheCreationPricePerMillion,
				Provider: p.Provider, Raw: p.Raw, Source: p.Source,
			}
			entries = append(entries, pe)
		}
	}
	n, err := s.store.UpsertPriceEntriesFromLiteLLM(ctx, entries)
	if err != nil {
		s.reloadPricing(ctx)
		return nil, err
	}
	s.reloadPricing(ctx)
	return &PricingSyncStats{Rows: len(entries), Skipped: res.Skipped, Updated: n}, nil
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
	var entries []*domain.PriceEntry
	if res.PriceEntries != nil {
		entries = res.PriceEntries
	} else {
		for _, p := range res.Rows {
			pe := &domain.PriceEntry{
				Model: p.Model, Mode: domain.PriceModeToken,
				InputPerM: &p.PromptPricePerMillion, OutputPerM: &p.CompletionPricePerMillion,
				CacheReadPerM: p.CacheReadPricePerMillion, CacheWritePerM: p.CacheCreationPricePerMillion,
				Provider: p.Provider, Raw: p.Raw, Source: p.Source,
			}
			entries = append(entries, pe)
		}
	}
	snap := s.priceSnapshot.Load()
	preview := &PricingPreview{Skipped: res.Skipped}
	if snap == nil {
		preview.ToAdd = len(entries)
		for _, e := range entries {
			preview.Entries = append(preview.Entries, PricingPreviewEntry{Model: e.Model, Mode: string(e.Mode), Action: "add"})
		}
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
	// variants changes crude count
	if res.Variants != nil {
		preview.VariantsChanged = len(res.Variants)
	}
	return preview, nil
}

func (s *Service) PriceSourceURL() string { return s.settingValue("price_source_url") }
func (s *Service) PriceSyncCron() string  { return s.settingValue("price_sync_cron") }
