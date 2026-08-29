// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func setBalanceWarningThreshold(t *testing.T, repos *repository.Repository, userID, threshold int64) {
	t.Helper()
	_, err := repos.Client.User.UpdateOneID(userID).SetBalanceWarningThreshold(threshold).Save(context.Background())
	require.NoError(t, err)
}

func warningEventsByUID(t *testing.T, events []domain.BalanceWarningEvent) map[int64]domain.BalanceWarningEvent {
	t.Helper()
	out := make(map[int64]domain.BalanceWarningEvent, len(events))
	for _, event := range events {
		_, duplicate := out[event.EntityID]
		require.False(t, duplicate, "uid %d emitted more than one warning", event.EntityID)
		out[event.EntityID] = event
	}
	return out
}

func requireBalanceWarning(t *testing.T, event domain.BalanceWarningEvent, userID, balance, threshold int64, email string) {
	t.Helper()
	require.Equal(t, domain.NotificationBalanceWarningCrossed, event.EventType)
	require.Equal(t, domain.NotificationUser, event.EntityType)
	require.Equal(t, userID, event.EntityID)
	require.Equal(t, balance, event.BalanceMillis)
	require.Equal(t, threshold, event.ThresholdMillis)
	require.Equal(t, email, event.Email)
}

type pgBalanceWarningSink struct {
	mu     sync.Mutex
	events []domain.BalanceWarningEvent
}

func (s *pgBalanceWarningSink) TrySubmit(event domain.BalanceWarningEvent) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	return true
}

func (s *pgBalanceWarningSink) snapshot() []domain.BalanceWarningEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.BalanceWarningEvent(nil), s.events...)
}

func TestPGBalanceWarning(t *testing.T) {
	repos := newPGReposShared(t)
	ensureCursorPartitions(t, repos)
	ctx := context.Background()

	t.Run("TestPGUserBalanceWarningThresholdProjection", func(t *testing.T) {
		created, err := repos.CreateUser(ctx, &domain.User{
			Email:                   "warning-projection@example.com",
			PasswordHash:            "hash",
			Role:                    domain.RoleUser,
			Status:                  domain.UserStatusActive,
			BalanceWarningThreshold: 321,
		})
		require.NoError(t, err)
		require.Equal(t, int64(321), created.BalanceWarningThreshold)
		loaded, err := repos.GetUser(ctx, created.ID)
		require.NoError(t, err)
		require.Equal(t, int64(321), loaded.BalanceWarningThreshold)
	})

	t.Run("TestPGBalanceWarningBalanceLaneBoundaries", func(t *testing.T) {
		exact := seedPGUser(t, repos, "warning-exact@example.com")
		below := seedPGUser(t, repos, "warning-below@example.com")
		forced := seedPGUser(t, repos, "warning-forced@example.com")
		disabled := seedPGUser(t, repos, "warning-disabled@example.com")
		alreadyBelow := seedPGUser(t, repos, "warning-already-below@example.com")
		for _, user := range []struct {
			id        int64
			balance   int64
			threshold int64
		}{
			{exact.ID, 600, 500},
			{below.ID, 700, 500},
			{forced.ID, 100, 50},
			{disabled.ID, 600, 0},
			{alreadyBelow.ID, 400, 500},
		} {
			require.NoError(t, repos.UpdateUserBalance(ctx, user.id, user.balance))
			setBalanceWarningThreshold(t, repos, user.id, user.threshold)
		}
		seedCursorRows(t, repos, exact.ID, 2, 50)
		seedCursorRows(t, repos, below.ID, 1, 250)
		seedCursorRows(t, repos, forced.ID, 1, 200)
		seedCursorRows(t, repos, disabled.ID, 1, 100)
		seedCursorRows(t, repos, alreadyBelow.ID, 1, 100)

		result, err := repos.SettleBalanceBatch(ctx, 100, 1, 0)
		require.NoError(t, err)
		require.Equal(t, int64(6), result.Marked)
		require.Len(t, result.Balances, 5)
		require.Len(t, result.BalanceWarnings, 3)
		byUID := warningEventsByUID(t, result.BalanceWarnings)
		requireBalanceWarning(t, byUID[exact.ID], exact.ID, 500, 500, exact.Email)
		requireBalanceWarning(t, byUID[below.ID], below.ID, 450, 500, below.Email)
		requireBalanceWarning(t, byUID[forced.ID], forced.ID, -100, 50, forced.Email)
		require.NotContains(t, byUID, disabled.ID)
		require.NotContains(t, byUID, alreadyBelow.ID)
		_, err = repos.Client.User.UpdateOneID(exact.ID).
			SetEmail("warning-changed@example.com").
			SetBalanceWarningThreshold(400).
			Save(ctx)
		require.NoError(t, err)
		requireBalanceWarning(t, byUID[exact.ID], exact.ID, 500, 500, exact.Email)
	})

	t.Run("TestPGBalanceWarningFefoLaneRequiresPermanentSpill", func(t *testing.T) {
		tempOnly := seedPGUser(t, repos, "warning-temp-only@example.com")
		spill := seedPGUser(t, repos, "warning-spill@example.com")
		forced := seedPGUser(t, repos, "warning-fefo-forced@example.com")
		for _, user := range []*domain.User{tempOnly, spill} {
			require.NoError(t, repos.UpdateUserBalance(ctx, user.ID, 600))
			setBalanceWarningThreshold(t, repos, user.ID, 500)
		}
		require.NoError(t, repos.UpdateUserBalance(ctx, forced.ID, 100))
		setBalanceWarningThreshold(t, repos, forced.ID, 50)
		seedTempBalance(t, repos, tempOnly.ID, 200, nil)
		seedTempBalance(t, repos, spill.ID, 50, nil)
		seedTempBalance(t, repos, forced.ID, 50, nil)
		seedCursorRows(t, repos, tempOnly.ID, 1, 100)
		seedCursorRows(t, repos, spill.ID, 1, 150)
		seedCursorRows(t, repos, forced.ID, 1, 250)

		result, err := repos.SettleFefoBatch(ctx, 100, 1, 0)
		require.NoError(t, err)
		require.Equal(t, int64(3), result.Marked)
		require.Len(t, result.Balances, 2, "temporary balance covering the full cost must not update permanent balance")
		byUID := warningEventsByUID(t, result.BalanceWarnings)
		require.Len(t, byUID, 2)
		requireBalanceWarning(t, byUID[spill.ID], spill.ID, 500, 500, spill.Email)
		requireBalanceWarning(t, byUID[forced.ID], forced.ID, -100, 50, forced.Email)
		require.Equal(t, int64(600), cursorBalance(t, repos, tempOnly.ID))
	})

	t.Run("TestPGBalanceWarningConcurrentBucketsEmitOneEventPerUID", func(t *testing.T) {
		const users = 8
		uids := make([]int64, 0, users)
		for index := range users {
			user := seedPGUser(t, repos, "warning-bucket-"+string(rune('a'+index))+"@example.com")
			uids = append(uids, user.ID)
			require.NoError(t, repos.UpdateUserBalance(ctx, user.ID, 1_000))
			setBalanceWarningThreshold(t, repos, user.ID, 900)
			seedCursorRows(t, repos, user.ID, 2, 50)
		}
		balances := billing.NewBalances(repos, nil)
		require.NoError(t, balances.Reload(ctx))
		sink := &pgBalanceWarningSink{}
		flusher := billing.NewFlusher(billing.FlushConfig{FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour}, repos, balances, nil)
		flusher.SetBalanceWarningSink(sink)
		drainCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		require.NoError(t, flusher.Close(drainCtx))
		events := warningEventsByUID(t, sink.snapshot())
		require.Len(t, events, users)
		for _, userID := range uids {
			event, ok := events[userID]
			require.True(t, ok)
			require.Equal(t, int64(900), event.BalanceMillis)
			balance, ok := balances.BalanceOf(userID)
			require.True(t, ok)
			require.Equal(t, int64(900), balance, "existing Balances.Set behavior must remain intact")
		}
		require.Zero(t, cursorUnbilledCount(t, repos))
	})

	t.Run("TestPGBalanceWarningRollbackReturnsNoCandidate", func(t *testing.T) {
		healthy := seedPGUser(t, repos, "warning-rollback@example.com")
		poison := seedPGUser(t, repos, "warning-rollback-poison@example.com")
		require.NoError(t, repos.UpdateUserBalance(ctx, healthy.ID, 600))
		require.NoError(t, repos.UpdateUserBalance(ctx, poison.ID, math.MinInt64+1))
		setBalanceWarningThreshold(t, repos, healthy.ID, 500)
		seedCursorRows(t, repos, healthy.ID, 1, 100)
		seedCursorRows(t, repos, poison.ID, 1, 1_000)
		result, err := repos.SettleBalanceBatch(ctx, 100, 1, 0)
		require.Error(t, err)
		require.Empty(t, result.BalanceWarnings)
		require.Equal(t, int64(600), cursorBalance(t, repos, healthy.ID))
		require.Equal(t, 2, cursorUnbilledCount(t, repos))
	})
}
