// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPGDeleteGroupOnlyLeavesAccountAndMembership pins the spec §7 registered
// asymmetry on real PostgreSQL: DeleteGroup only sets groups.deleted_at and does
// NOT delete the account or the membership row. Consequently the single-group
// loader (whose membership subquery does not filter deleted groups) still returns
// the account, while the full loader (which filters group.DeletedAtIsNil) drops
// the group entirely. Skips without TEST_DATABASE_URL (skip never counts as pass).
func TestPGDeleteGroupOnlyLeavesAccountAndMembership(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	g := seedPGGroup(t, repos, "to-delete")
	acc := seedPGAccount(t, repos, tpl.ID, "keep-me")
	require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{g.ID}))

	// Baseline: the group and its single member are visible to both loaders.
	before, err := repos.Groups.LoadGroupAccounts(ctx, g.ID)
	require.NoError(t, err)
	require.Len(t, before, 1)
	full0, err := repos.Groups.LoadGroupsAccounts(ctx)
	require.NoError(t, err)
	require.Contains(t, full0, g.ID)

	// Soft-delete ONLY the group.
	require.NoError(t, repos.Groups.DeleteGroup(ctx, g.ID))

	// The account is still alive and the membership row is preserved.
	got, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, acc.ID, got.ID)
	members, err := repos.Accounts.GetAccountGroups(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, []int64{g.ID}, members, "membership row must survive a group soft delete")

	// Single-group loader: BASE semantics still include the account (registered
	// bug, spec §7) — the reload path relies on this exact response shape.
	single, err := repos.Groups.LoadGroupAccounts(ctx, g.ID)
	require.NoError(t, err)
	require.Len(t, single, 1, "single-group loader still returns the account")
	require.Equal(t, acc.ID, single[0].ID)

	// Full loader excludes the soft-deleted group.
	full, err := repos.Groups.LoadGroupsAccounts(ctx)
	require.NoError(t, err)
	require.NotContains(t, full, g.ID, "full loader must exclude the soft-deleted group")
}
