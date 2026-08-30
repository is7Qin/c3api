// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func TestPGUpdateBalanceWarningThreshold(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	t.Run("TestPGUpdateBalanceWarningThresholdReturnsPreviousValue", func(t *testing.T) {
		created, err := repos.CreateUser(ctx, &domain.User{
			Email:                   "warning-threshold-update@example.com",
			PasswordHash:            "hash",
			Role:                    domain.RoleUser,
			Status:                  domain.UserStatusActive,
			BalanceWarningThreshold: 100_000,
		})
		require.NoError(t, err)
		updated, previous, err := repos.UpdateUserBalanceWarningThreshold(ctx, created.ID, 200_000)
		require.NoError(t, err)
		require.Equal(t, int64(100_000), previous)
		require.Equal(t, int64(200_000), updated.BalanceWarningThreshold)
		_, _, err = repos.UpdateUserBalanceWarningThreshold(ctx, created.ID+1, 0)
		require.ErrorIs(t, err, repository.ErrNotFound)
	})

	t.Run("TestPGUpdateBalanceWarningThresholdConcurrentWritersReturnOrderedChain", func(t *testing.T) {
		created, err := repos.CreateUser(ctx, &domain.User{
			Email:                   "warning-threshold-concurrent@example.com",
			PasswordHash:            "hash",
			Role:                    domain.RoleUser,
			Status:                  domain.UserStatusActive,
			BalanceWarningThreshold: 100_000,
		})
		require.NoError(t, err)
		type result struct {
			threshold int64
			previous  int64
			err       error
		}
		start := make(chan struct{})
		results := make(chan result, 2)
		var wait sync.WaitGroup
		for _, threshold := range []int64{200_000, 300_000} {
			wait.Go(func() {
				<-start
				_, previous, err := repos.UpdateUserBalanceWarningThreshold(ctx, created.ID, threshold)
				results <- result{threshold: threshold, previous: previous, err: err}
			})
		}
		close(start)
		wait.Wait()
		close(results)
		byThreshold := make(map[int64]result, 2)
		for result := range results {
			require.NoError(t, result.err)
			byThreshold[result.threshold] = result
		}
		stored, err := repos.GetUser(ctx, created.ID)
		require.NoError(t, err)
		final := byThreshold[stored.BalanceWarningThreshold]
		require.Equal(t, int64(100_000), byThreshold[final.previous].previous)
	})
}
