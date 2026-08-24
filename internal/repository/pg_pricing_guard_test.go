// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// TestManualEntryModels_PG verifies ManualEntryModels returns only manual source models.
func TestManualEntryModels_PG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	// clean slate: create two entries via manual and litellm
	manualModel := "pg-guard-manual-model"
	liteModel := "pg-guard-lite-model"
	_, err := repos.PriceEntries.UpsertManual(ctx, &repository.PriceEntryManual{Model: manualModel, Mode: domain.PriceModeToken, InputPerM: int64Ptr(100000), OutputPerM: int64Ptr(200000)})
	require.NoError(t, err)
	_, err = repos.PriceEntries.UpsertFromLiteLLM(ctx, []*domain.PriceEntry{{Model: liteModel, Mode: domain.PriceModeToken, InputPerM: int64Ptr(100000), OutputPerM: int64Ptr(200000), Source: domain.PricingSourceLitellm}})
	require.NoError(t, err)
	models, err := repos.PriceEntries.ManualEntryModels(ctx)
	require.NoError(t, err)
	require.Contains(t, models, manualModel)
	require.NotContains(t, models, liteModel)
}

// TestVariantGuard_ManualEntrySurvives_PG is the PG-mode sync guard test for F2:
// create manual entry + custom variants for model X; simulate sync that would emit X's variants; assert admin variants survive via ManualEntryModels filtering.
func TestVariantGuard_ManualEntrySurvives_PG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	model := "pg-variant-guard-model"
	_, err := repos.PriceEntries.UpsertManual(ctx, &repository.PriceEntryManual{Model: model, Mode: domain.PriceModeToken, InputPerM: int64Ptr(100000), OutputPerM: int64Ptr(200000)})
	require.NoError(t, err)
	// custom admin variants
	adminMult := 5000
	_, err = repos.PriceVariants.ReplaceBatch(ctx, model, []*domain.PriceVariant{{Model: model, Seq: 99, MultBP: &adminMult}})
	require.NoError(t, err)
	// simulate litellm sync attempting to replace variants for same model (service layer should filter; repo layer would delete)
	// Here we verify that ManualEntryModels correctly identifies manual model so service would filter.
	manuals, err := repos.PriceEntries.ManualEntryModels(ctx)
	require.NoError(t, err)
	isManual := false
	for _, m := range manuals {
		if m == model {
			isManual = true
			break
		}
	}
	require.True(t, isManual, "manual entry should be recognized")
	// ensure variants still exist and are admin's
	vars, err := repos.PriceVariants.ListByModel(ctx, model)
	require.NoError(t, err)
	require.Len(t, vars, 1)
	require.Equal(t, 99, vars[0].Seq)
	require.Equal(t, 5000, *vars[0].MultBP)
	// simulate filtered sync: do NOT call ReplaceBatch for manual model
	// verify that calling ReplaceBatch directly would clobber (to prove guard needed)
	// (this is just sanity)
	otherMult := 20000
	_, err = repos.PriceVariants.ReplaceBatch(ctx, model, []*domain.PriceVariant{{Model: model, Seq: 1, MultBP: &otherMult}})
	require.NoError(t, err)
	vars2, err := repos.PriceVariants.ListByModel(ctx, model)
	require.NoError(t, err)
	require.Len(t, vars2, 1)
	require.Equal(t, 1, vars2[0].Seq, "direct ReplaceBatch clobbers — proving service filter essential")
	// restore admin variants
	_, err = repos.PriceVariants.ReplaceBatch(ctx, model, []*domain.PriceVariant{{Model: model, Seq: 99, MultBP: &adminMult}})
	require.NoError(t, err)
}

// TestDeletePriceEntryCascadeManual_PG verifies D-C1: manual entry+variant →
// WithTx{DeletePriceVariantsByModel; DeletePriceEntryManual} → both tables empty.
// 冒烟发现 2026-08-24：删条目不清变体致孤儿挂新条目。
func TestDeletePriceEntryCascadeManual_PG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	model := "pg-cascade-manual-model"
	_, err := repos.PriceEntries.UpsertManual(ctx, &repository.PriceEntryManual{Model: model, Mode: domain.PriceModeToken, InputPerM: int64Ptr(100000), OutputPerM: int64Ptr(200000)})
	require.NoError(t, err)
	mult := 5000
	_, err = repos.PriceVariants.ReplaceBatch(ctx, model, []*domain.PriceVariant{{Model: model, Seq: 1, MultBP: &mult}, {Model: model, Seq: 2, MultBP: &mult}})
	require.NoError(t, err)
	err = repos.WithTx(ctx, func(tx repository.TxStore) error {
		if err := tx.DeletePriceVariantsByModel(ctx, model); err != nil {
			return err
		}
		return tx.DeletePriceEntryManual(ctx, model)
	})
	require.NoError(t, err)
	_, err = repos.PriceEntries.GetPriceEntry(ctx, model)
	require.ErrorIs(t, err, repository.ErrNotFound)
	vars, err := repos.PriceVariants.ListByModel(ctx, model)
	require.NoError(t, err)
	require.Empty(t, vars, "variants must be cascade-deleted with manual entry")
}

// TestVariantImageOverridesRoundTrip_PG verifies F-B: image-mode entry +
// img overrides round-trip via ReplaceBatch + ListByModel.
func TestVariantImageOverridesRoundTrip_PG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	model := "pg-variant-image-model"
	_, err := repos.PriceEntries.UpsertManual(ctx, &repository.PriceEntryManual{Model: model, Mode: domain.PriceModeImage, ImgInTokPerM: int64Ptr(100), ImgOutTokPerM: int64Ptr(200), PricePerImage: int64Ptr(300)})
	require.NoError(t, err)
	imgIn := int64(111)
	imgOut := int64(222)
	perImg := int64(333)
	_, err = repos.PriceVariants.ReplaceBatch(ctx, model, []*domain.PriceVariant{{Model: model, Seq: 1, SetImgInTokPerM: &imgIn, SetImgOutTokPerM: &imgOut, SetPricePerImage: &perImg}})
	require.NoError(t, err)
	vars, err := repos.PriceVariants.ListByModel(ctx, model)
	require.NoError(t, err)
	require.Len(t, vars, 1)
	require.Equal(t, imgIn, *vars[0].SetImgInTokPerM)
	require.Equal(t, imgOut, *vars[0].SetImgOutTokPerM)
	require.Equal(t, perImg, *vars[0].SetPricePerImage)
	// resolver yields same
	entry, err := repos.PriceEntries.GetPriceEntry(ctx, model)
	require.NoError(t, err)
	rp, ok := domain.ResolveEntryPrices(entry, vars, "auto", 0, time.Now())
	require.True(t, ok)
	require.Equal(t, imgIn, *rp.ImgInTokPerM)
	require.Equal(t, imgOut, *rp.ImgOutTokPerM)
	require.Equal(t, perImg, *rp.PricePerImage)
}

// TestCodexSearchSeed_Idempotent_PG verifies F-A: seed is idempotent and source=manual.
func TestCodexSearchSeed_Idempotent_PG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	// ensure seed runs
	require.NoError(t, repos.EnsureCodexSearchSeed(ctx))
	pe, err := repos.PriceEntries.GetPriceEntry(ctx, domain.CodexSearchModel)
	require.NoError(t, err)
	require.Equal(t, domain.CodexSearchModel, pe.Model)
	require.Equal(t, domain.PriceModeCall, pe.Mode)
	require.Equal(t, domain.PricingSourceManual, pe.Source)
	require.NotNil(t, pe.PricePerCall)
	require.Equal(t, domain.DefaultCodexSearchPricePerCall, *pe.PricePerCall)
	// idempotent second run
	require.NoError(t, repos.EnsureCodexSearchSeed(ctx))
	// still exactly 1 row for that model (ON CONFLICT DO NOTHING)
	pe2, err := repos.PriceEntries.GetPriceEntry(ctx, domain.CodexSearchModel)
	require.NoError(t, err)
	require.Equal(t, pe.Model, pe2.Model)
	require.Equal(t, *pe.PricePerCall, *pe2.PricePerCall)
	// count via List with model filter
	rows, total, err := repos.PriceEntries.ListPriceEntries(ctx, repository.ListQuery{Limit: 10, Offset: 0}, nil, nil, domain.CodexSearchModel)
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, rows, 1)
}

// TestDeletePriceEntryCascadeLitellmConflict_PG verifies D-C1 guard: litellm
// entry+variant → same cascade → ErrConflict AND variants intact (whole tx rolled back).
func TestDeletePriceEntryCascadeLitellmConflict_PG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	model := "pg-cascade-litellm-model"
	_, err := repos.PriceEntries.UpsertFromLiteLLM(ctx, []*domain.PriceEntry{{Model: model, Mode: domain.PriceModeToken, InputPerM: int64Ptr(100000), OutputPerM: int64Ptr(200000), Source: domain.PricingSourceLitellm}})
	require.NoError(t, err)
	mult := 7000
	_, err = repos.PriceVariants.ReplaceBatch(ctx, model, []*domain.PriceVariant{{Model: model, Seq: 1, MultBP: &mult}})
	require.NoError(t, err)
	err = repos.WithTx(ctx, func(tx repository.TxStore) error {
		if err := tx.DeletePriceVariantsByModel(ctx, model); err != nil {
			return err
		}
		return tx.DeletePriceEntryManual(ctx, model)
	})
	require.ErrorIs(t, err, repository.ErrConflict)
	// entry still exists
	pe, err := repos.PriceEntries.GetPriceEntry(ctx, model)
	require.NoError(t, err)
	require.Equal(t, model, pe.Model)
	require.Equal(t, domain.PricingSourceLitellm, pe.Source)
	// variants intact due to rollback
	vars, err := repos.PriceVariants.ListByModel(ctx, model)
	require.NoError(t, err)
	require.Len(t, vars, 1)
	require.Equal(t, 1, vars[0].Seq)
	require.Equal(t, 7000, *vars[0].MultBP)
}
