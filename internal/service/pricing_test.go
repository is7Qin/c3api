// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/pricing"
	"github.com/is7qin/c3api/internal/repository"
)

func newPricingSvc(t *testing.T, fs *fakeStore) *Service {
	t.Helper()
	svc := New(fs, nil, NopInvalidator{}, nil, nil, nil, nil)
	require.NoError(t, svc.ReloadPricingCtx(context.Background()))
	return svc
}

type fakePriceFetcher struct {
	res *pricing.FetchResult
	err error
	url string
}

func (f *fakePriceFetcher) Fetch(ctx context.Context, sourceURL string) (*pricing.FetchResult, error) {
	f.url = sourceURL
	if f.err != nil {
		return nil, f.err
	}
	return f.res, nil
}

func int64Ptr(v int64) *int64 { return &v }

func TestPriceEntrySnapshotLoad(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertPriceEntriesFromLiteLLM(context.Background(), []*domain.PriceEntry{
		{Model: "gpt-4o", Mode: domain.PriceModeToken, InputPerM: int64Ptr(250000), OutputPerM: int64Ptr(1000000), Source: domain.PricingSourceLitellm},
		{Model: "img-m", Mode: domain.PriceModeImage, PricePerImage: int64Ptr(5400), Source: domain.PricingSourceLitellm},
	})
	require.NoError(t, err)
	svc := newPricingSvc(t, fs)
	pe, err := svc.GetPriceEntry(context.Background(), "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(250000), *pe.InputPerM)
	_, err = svc.GetPriceEntry(context.Background(), "missing")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPriceEntryManualValidation(t *testing.T) {
	fs := newFakeStore()
	svc := newPricingSvc(t, fs)
	_, err := svc.UpsertPriceEntry(context.Background(), &repository.PriceEntryManual{Model: "", Mode: domain.PriceModeToken})
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.UpsertPriceEntry(context.Background(), &repository.PriceEntryManual{Model: "m", Mode: domain.PriceModeToken, InputPerM: int64Ptr(100)})
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.UpsertPriceEntry(context.Background(), &repository.PriceEntryManual{Model: "m", Mode: domain.PriceModeToken, InputPerM: int64Ptr(100), OutputPerM: int64Ptr(200)})
	require.NoError(t, err)
}

func TestResolvePricesWithVariant(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertPriceEntriesFromLiteLLM(context.Background(), []*domain.PriceEntry{
		{Model: "m", Mode: domain.PriceModeToken, InputPerM: int64Ptr(100000), OutputPerM: int64Ptr(200000), Source: domain.PricingSourceManual},
	})
	require.NoError(t, err)
	_, err = fs.ReplacePriceVariants(context.Background(), "m", []*domain.PriceVariant{
		{Model: "m", Seq: 1, ServiceTier: strPtr("priority"), SetInputPerM: int64Ptr(150000)},
	})
	require.NoError(t, err)
	svc := newPricingSvc(t, fs)
	rp, ok := svc.ResolvePrices("m", 0, "priority", time.Now())
	require.True(t, ok)
	require.Equal(t, int64(150000), *rp.InputPerM)
}

func TestReplacePriceVariants_MultBPValidation(t *testing.T) {
	fs := newFakeStore()
	svc := newPricingSvc(t, fs)
	// MaxInt rejected
	maxInt := 1 << 30
	_, err := svc.ReplacePriceVariants(context.Background(), "m", []*domain.PriceVariant{{Model: "m", Seq: 1, MultBP: &maxInt}})
	require.ErrorIs(t, err, ErrInvalidInput)
	// negative rejected
	neg := -1
	_, err = svc.ReplacePriceVariants(context.Background(), "m", []*domain.PriceVariant{{Model: "m", Seq: 1, MultBP: &neg}})
	require.ErrorIs(t, err, ErrInvalidInput)
	// boundary 100000 allowed, 100001 rejected
	v100k := 100000
	_, err = svc.ReplacePriceVariants(context.Background(), "m", []*domain.PriceVariant{{Model: "m", Seq: 1, MultBP: &v100k}})
	require.NoError(t, err)
	v100001 := 100001
	_, err = svc.ReplacePriceVariants(context.Background(), "m", []*domain.PriceVariant{{Model: "m", Seq: 1, MultBP: &v100001}})
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestReplacePriceVariants_CallSetPricePerCall(t *testing.T) {
	fs := newFakeStore()
	svc := newPricingSvc(t, fs)
	callPrice := int64(2000)
	_, err := svc.ReplacePriceVariants(context.Background(), "m-call", []*domain.PriceVariant{{Model: "m-call", Seq: 1, SetPricePerCall: &callPrice}})
	require.NoError(t, err)
	// also resolver yields that price
	_, err = fs.UpsertPriceEntriesFromLiteLLM(context.Background(), []*domain.PriceEntry{
		{Model: "m-call", Mode: domain.PriceModeCall, PricePerCall: int64Ptr(1000), Source: domain.PricingSourceManual},
	})
	require.NoError(t, err)
	require.NoError(t, svc.ReloadPricingCtx(context.Background()))
	rp, ok := svc.ResolvePrices("m-call", 0, "auto", time.Now())
	require.True(t, ok)
	require.NotNil(t, rp.PricePerCall)
	require.Equal(t, callPrice, *rp.PricePerCall)
	// empty effect should be rejected
	_, err = svc.ReplacePriceVariants(context.Background(), "m-call", []*domain.PriceVariant{{Model: "m-call", Seq: 2}})
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestSyncPricingGuardsManualVariants(t *testing.T) {
	fs := newFakeStore()
	// create manual entry + custom variants for model X
	_, err := fs.UpsertPriceEntryManual(context.Background(), &repository.PriceEntryManual{Model: "guard-model", Mode: domain.PriceModeToken, InputPerM: int64Ptr(100000), OutputPerM: int64Ptr(200000)})
	require.NoError(t, err)
	mult := 5000
	_, err = fs.ReplacePriceVariants(context.Background(), "guard-model", []*domain.PriceVariant{{Model: "guard-model", Seq: 99, MultBP: &mult}})
	require.NoError(t, err)
	// also create a litellm model that should be overwritten
	_, err = fs.UpsertPriceEntriesFromLiteLLM(context.Background(), []*domain.PriceEntry{{Model: "litellm-model", Mode: domain.PriceModeToken, InputPerM: int64Ptr(100000), OutputPerM: int64Ptr(200000), Source: domain.PricingSourceLitellm}})
	require.NoError(t, err)
	svc := newPricingSvc(t, fs)
	// settings needed for SyncPricingNow
	_, err = fs.SetSetting(context.Background(), "price_source_url", domain.SettingTypeString, "http://example.com/prices.json")
	require.NoError(t, err)
	// fake fetcher emits variants for both models: guard-model's variants should be dropped, litellm-model's should be applied
	fetcher := &fakePriceFetcher{res: &pricing.FetchResult{
		PriceEntries: []*domain.PriceEntry{{Model: "guard-model", Mode: domain.PriceModeToken, InputPerM: int64Ptr(999), OutputPerM: int64Ptr(999), Source: domain.PricingSourceLitellm}},
		Variants: []*domain.PriceVariant{
			{Model: "guard-model", Seq: 1, MultBP: intPtr(20000)},
			{Model: "litellm-model", Seq: 1, MultBP: intPtr(30000)},
		},
	}}
	svc.SetPriceFetcher(fetcher)
	_, err = svc.SyncPricingNow(context.Background())
	require.NoError(t, err)
	// guard-model variants must survive (seq 99), not replaced by fetcher's seq 1
	vars, err := fs.ListPriceVariants(context.Background(), "guard-model")
	require.NoError(t, err)
	require.Len(t, vars, 1)
	require.Equal(t, 99, vars[0].Seq)
	require.Equal(t, 5000, *vars[0].MultBP)
	// litellm-model variants should be applied
	vars2, err := fs.ListPriceVariants(context.Background(), "litellm-model")
	require.NoError(t, err)
	require.Len(t, vars2, 1)
	require.Equal(t, 1, vars2[0].Seq)
	require.Equal(t, 30000, *vars2[0].MultBP)
	// entry for guard-model must remain manual (not overwritten)
	pe, err := fs.GetPriceEntry(context.Background(), "guard-model")
	require.NoError(t, err)
	require.Equal(t, domain.PricingSourceManual, pe.Source)
}
