// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSettlement_HealthyHeadLaterFailure_RemainsUnbilled(t *testing.T) {
	repos := newPGRepos(t)
	ensureCursorPartitions(t, repos)
	ctx := context.Background()
	healthy := seedPGUser(t, repos, "f7-healthy@example.com")
	poison := seedPGUser(t, repos, "f7-poison@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, healthy.ID, 1_000_000))
	require.NoError(t, repos.UpdateUserBalance(ctx, poison.ID, math.MinInt64+1))
	// healthy seeded first => smallest id = healthy head
	seedCursorRows(t, repos, healthy.ID, 1, 1000)
	seedCursorRows(t, repos, poison.ID, 1, 1000)
	all := fetchAllUnbilled(t, repos)
	require.Len(t, all, 2)
	healthyID := all[0].ID
	poisonID := all[1].ID

	_, err := repos.SettleBalanceBatch(ctx, 10, 1, 0)
	require.Error(t, err, "deterministic 22xxx must fail closed, not write-off")

	// Neither row should be marked; balances unchanged
	require.False(t, usageLogByID(t, repos, healthyID).Billed, "healthy head must NOT be marked after later failure")
	require.False(t, usageLogByID(t, repos, poisonID).Billed, "poison row must remain unbilled for replay")
	require.Equal(t, int64(1_000_000), cursorBalance(t, repos, healthy.ID))
	require.Equal(t, int64(math.MinInt64+1), cursorBalance(t, repos, poison.ID))
	require.Equal(t, 2, cursorUnbilledCount(t, repos))
}

func TestSettlement_SameUserAggregateFailure_RemainsUnbilled(t *testing.T) {
	repos := newPGRepos(t)
	ensureCursorPartitions(t, repos)
	ctx := context.Background()
	u := seedPGUser(t, repos, "f7-agg@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, math.MinInt64+1))
	seedCursorRows(t, repos, u.ID, 2, 1000)
	all := fetchAllUnbilled(t, repos)
	require.Len(t, all, 2)

	_, err := repos.SettleBalanceBatch(ctx, 10, 1, 0)
	require.Error(t, err, "same-user aggregate failure must not prove row causality")

	for _, r := range all {
		require.False(t, usageLogByID(t, repos, r.ID).Billed, "no row marked without deduction")
	}
	require.Equal(t, int64(math.MinInt64+1), cursorBalance(t, repos, u.ID))
	require.Equal(t, 2, cursorUnbilledCount(t, repos))
}
