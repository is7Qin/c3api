// SPDX-License-Identifier: AGPL-3.0-or-later
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
