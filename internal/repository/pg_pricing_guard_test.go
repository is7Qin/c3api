// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
	"testing"

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
