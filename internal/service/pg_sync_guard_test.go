// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"os"
	"testing"
	"time"

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
	svc := New(repo, nil, NopInvalidator{}, nil, nil, nil, nil, ServiceDeps{EmailCodeStore: testEmailCodes})
	require.NoError(t, svc.ReloadPricingCtx(ctx))
	return repo, svc
}

// TestSyncPricingGuardsManualVariants_PG 手工变体守卫的 PG 真库回归（后经
// pricing.SyncWorker 驱动：worker 持 fetcher+repo+settings+reload+snapshot，
// service 只出 settings 快照与快照 membership）：手工模型变体存活且 entry 保持
// manual；新模型落库且快照可解析（reload/publish/notify 链真实跑过）。
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
	// fake fetcher emits variants for guard model + a brand-new model
	fetcher := &fakePriceFetcher{res: &pricing.FetchResult{
		PriceEntries: []*domain.PriceEntry{
			{Model: model, Mode: domain.PriceModeToken, InputPerM: int64Ptr(999), OutputPerM: int64Ptr(999), Source: domain.PricingSourceLitellm},
			{Model: "pg-svc-new-model", Mode: domain.PriceModeToken, InputPerM: int64Ptr(1000), OutputPerM: int64Ptr(2000), Source: domain.PricingSourceLitellm},
		},
		Variants: []*domain.PriceVariant{{Model: model, Seq: 1, MultBP: intPtr(20000)}},
	}}
	w := pricing.NewSyncWorker(pricing.SyncWorkerConfig{
		Fetcher: fetcher, Repo: svc.store.(*repository.Repository), Settings: svc,
		Reload: svc.ReloadPricingAndNotifyCompiler, Snapshot: svc, Log: nil,
	})
	stats, err := w.SyncNow(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, stats.Rows)
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
	// new model landed AND snapshot reloaded (reload chain ran)
	_, ok := svc.ResolvePrices("pg-svc-new-model", 0, "auto", time.Now())
	require.True(t, ok, "sync 成功必须刷新快照（reload/publish/notify 链）")
}
