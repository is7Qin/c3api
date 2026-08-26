// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"os"
	"testing"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/pricing"
	"github.com/is7qin/c3api/internal/repository"
)

func newPGServiceRepos(t *testing.T) (*repository.Repository, *Service) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	ctx := context.Background()
	pool, err := repository.OpenPG(ctx, dsn, 5)
	require.NoError(t, err)
	t.Cleanup(func() { pool.Close() })
	db := stdlib.OpenDBFromPool(pool)
	drv := entsql.OpenDB("postgres", db)
	repo, err := repository.NewWithPG(ctx, drv, true, pool)
	require.NoError(t, err)
	require.NoError(t, repo.EnsurePriceVariantsEffectCheck(ctx))
	_, err = pool.Exec(ctx, "DELETE FROM price_variants")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "DELETE FROM price_entries")
	require.NoError(t, err)
	svc := New(repo, nil, NopInvalidator{}, nil, nil, nil, nil)
	require.NoError(t, svc.ReloadPricingCtx(ctx))
	return repo, svc
}

func TestSyncPricingGuardsManualVariants_PG(t *testing.T) {
	_, svc := newPGServiceRepos(t)
	ctx := context.Background()
	model := "pg-svc-guard-model"
	_, err := svc.UpsertPriceEntry(ctx, &repository.PriceEntryManual{Model: model, Mode: domain.PriceModeToken, InputPerM: int64Ptr(100000), OutputPerM: int64Ptr(200000)})
	require.NoError(t, err)
	mult := 5000
	_, err = svc.ReplacePriceVariants(ctx, model, []*domain.PriceVariant{{Model: model, Seq: 99, MultBP: &mult}})
	require.NoError(t, err)
	// set price_source_url for sync
	_, err = svc.store.(*repository.Repository).Settings.Set(ctx, "price_source_url", domain.SettingTypeString, "http://example.com/prices.json")
	require.NoError(t, err)
	// also need price_sync_cron? not needed for SyncPricingNow
	// fake fetcher emits variants for guard model
	fetcher := &fakePriceFetcher{res: &pricing.FetchResult{
		PriceEntries: []*domain.PriceEntry{{Model: model, Mode: domain.PriceModeToken, InputPerM: int64Ptr(999), OutputPerM: int64Ptr(999), Source: domain.PricingSourceLitellm}},
		Variants:     []*domain.PriceVariant{{Model: model, Seq: 1, MultBP: intPtr(20000)}},
	}}
	svc.SetPriceFetcher(fetcher)
	// also need to ensure priceSnapshot reload after sync
	_, err = svc.SyncPricingNow(ctx)
	require.NoError(t, err)
	// admin variants must survive
	vars, err := svc.ListPriceVariants(ctx, model)
	require.NoError(t, err)
	require.Len(t, vars, 1)
	require.Equal(t, 99, vars[0].Seq)
	require.Equal(t, 5000, *vars[0].MultBP)
	// entry must remain manual
	pe, err := svc.GetPriceEntry(ctx, model)
	require.NoError(t, err)
	require.Equal(t, domain.PricingSourceManual, pe.Source)
}
