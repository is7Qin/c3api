// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// Settlement regression scenarios A/E/G (B/C live in pg_settlement_failure_test.go).

func TestSettlement_SuccessUnchanged(t *testing.T) {
	repos := newPGRepos(t)
	ensureCursorPartitions(t, repos)
	ctx := context.Background()
	u := seedPGUser(t, repos, "f7-a@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 1_000_000))
	seedCursorRows(t, repos, u.ID, 2, 100_000)
	all := fetchAllUnbilled(t, repos)
	require.Len(t, all, 2)
	res, err := repos.SettleBalanceBatch(ctx, 10, 1, 0)
	require.NoError(t, err)
	require.Equal(t, int64(2), res.Marked)
	require.Equal(t, int64(0), res.Quarantined)
	require.Equal(t, int64(800_000), cursorBalance(t, repos, u.ID))
	for _, r := range all {
		require.True(t, usageLogByID(t, repos, r.ID).Billed)
		require.False(t, usageLogByID(t, repos, r.ID).Overdraft)
	}
}

func TestSettlement_Cancellation_NoWriteOff(t *testing.T) {
	repos := newPGRepos(t)
	ensureCursorPartitions(t, repos)
	ctx := context.Background()
	u := seedPGUser(t, repos, "f7-e@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 1_000_000))
	seedCursorRows(t, repos, u.ID, 2, 100_000)
	all := fetchAllUnbilled(t, repos)
	require.Len(t, all, 2)
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	_, err := repos.SettleBalanceBatch(cancelCtx, 10, 1, 0)
	require.Error(t, err)
	for _, r := range all {
		require.False(t, usageLogByID(t, repos, r.ID).Billed, "cancelled settlement must not mark")
	}
	require.Equal(t, 2, cursorUnbilledCount(t, repos))
	require.Equal(t, int64(1_000_000), cursorBalance(t, repos, u.ID))
	res, err := repos.SettleBalanceBatch(ctx, 10, 1, 0)
	require.NoError(t, err)
	require.Equal(t, int64(2), res.Marked)
	require.Zero(t, cursorUnbilledCount(t, repos))
}

func TestSettlement_ZeroCostRegression(t *testing.T) {
	repos := newPGRepos(t)
	ensureCursorPartitions(t, repos)
	ctx := context.Background()
	u := seedPGUser(t, repos, "f7-g@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 500_000))
	z1 := cursorLog(u.ID, 0)
	z2 := cursorLog(u.ID, 0)
	paid := cursorLog(u.ID, 120_000)
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{z1, z2, paid}))
	rows := fetchAllUnbilled(t, repos)
	require.Len(t, rows, 3)
	// Fefo/balance lanes only handle cost>0; zero-cost must be via sweep
	// Simulate sweep: mark zero-cost only
	var zeroIDs []int64
	for _, r := range rows {
		if r.Cost <= 0 {
			zeroIDs = append(zeroIDs, r.ID)
		}
	}
	require.Len(t, zeroIDs, 2)
	require.NoError(t, repos.MarkBilledBulk(ctx, zeroIDs))
	for _, id := range zeroIDs {
		require.True(t, usageLogByID(t, repos, id).Billed)
		require.False(t, usageLogByID(t, repos, id).Overdraft)
	}
	require.Equal(t, int64(500_000), cursorBalance(t, repos, u.ID), "zero-cost must not affect balance")
	// paid row still unbilled, not touched by zero-cost path
	var paidID int64
	for _, r := range rows {
		if r.Cost > 0 {
			paidID = r.ID
		}
	}
	require.False(t, usageLogByID(t, repos, paidID).Billed, "positive-cost row must not be marked by zero-cost path")
	// now settle paid row normally
	res, err := repos.SettleBalanceBatch(ctx, 10, 1, 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), res.Marked)
	require.Equal(t, int64(380_000), cursorBalance(t, repos, u.ID))
}
