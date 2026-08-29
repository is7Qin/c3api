// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestUsageRepoInsertBatch_R1_DuplicateOnly(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)
	at := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repos.EnsureUsageLogPartitions(ctx, at, at))

	logs := []*domain.UsageLog{
		usageLogFor("r1-a", at),
		usageLogFor("r1-b", at),
	}
	require.NoError(t, repos.Usages.InsertBatch(ctx, logs))
	require.NoError(t, repos.Usages.InsertBatch(ctx, logs))
	require.Equal(t, int64(2), pgCount(t, pool, `SELECT COUNT(*) FROM usage_logs`))
	require.Equal(t, int64(1), pgCount(t, pool, `SELECT COUNT(*) FROM usage_logs WHERE request_id='r1-a'`))
	require.Equal(t, int64(1), pgCount(t, pool, `SELECT COUNT(*) FROM usage_logs WHERE request_id='r1-b'`))
}

func TestUsageRepoInsertBatch_R2_MixedOldNew(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)
	at := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repos.EnsureUsageLogPartitions(ctx, at, at))

	old := usageLogFor("old-1", at)
	old.Model = "m-old"
	old.Cost = 100
	old.TotalTokens = 11
	old.Billed = false
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{old}))

	oldReplay := usageLogFor("old-1", at)
	oldReplay.Model = "m-mutated"
	oldReplay.Cost = 9999
	oldReplay.TotalTokens = 9999
	oldReplay.Billed = true
	new1 := usageLogFor("new-1", at)
	new2 := usageLogFor("new-2", at)
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{oldReplay, new1, new2}))

	require.Equal(t, int64(3), pgCount(t, pool, `SELECT COUNT(*) FROM usage_logs`))
	require.Equal(t, int64(1), pgCount(t, pool, `SELECT COUNT(*) FROM usage_logs WHERE request_id='old-1'`))
	require.Equal(t, int64(1), pgCount(t, pool, `SELECT COUNT(*) FROM usage_logs WHERE request_id='new-1'`))
	require.Equal(t, int64(1), pgCount(t, pool, `SELECT COUNT(*) FROM usage_logs WHERE request_id='new-2'`))

	var model string
	var cost int64
	var billed bool
	var totalTokens int64
	err := pool.QueryRow(ctx, `SELECT model, cost, billed, total_tokens FROM usage_logs WHERE request_id='old-1'`).Scan(&model, &cost, &billed, &totalTokens)
	require.NoError(t, err)
	require.Equal(t, "m-old", model)
	require.Equal(t, int64(100), cost)
	require.Equal(t, false, billed)
	require.Equal(t, int64(11), totalTokens)
}

func TestUsageRepoInsertBatch_R3_BilledNotOverwritten(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)
	at := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repos.EnsureUsageLogPartitions(ctx, at, at))

	seed := usageLogFor("bill-1", at)
	seed.Billed = true
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{seed}))

	replay := usageLogFor("bill-1", at)
	replay.Billed = false
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{replay}))

	var billed bool
	err := pool.QueryRow(ctx, `SELECT billed FROM usage_logs WHERE request_id='bill-1'`).Scan(&billed)
	require.NoError(t, err)
	require.True(t, billed, "billed=true existing row must not be overwritten by billed=false replay")
	require.Equal(t, int64(1), pgCount(t, pool, `SELECT COUNT(*) FROM usage_logs WHERE request_id='bill-1'`))
}

func TestUsageRepoInsertBatch_R4_PartitionUnavailable(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	at := time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC)
	err := repos.Usages.InsertBatch(ctx, []*domain.UsageLog{usageLogFor("no-part", at)})
	require.Error(t, err)
	require.True(t, errors.Is(err, domain.ErrPartitionUnavailable), "missing partition must map to ErrPartitionUnavailable, got %v", err)
}

func TestUsageRepoInsertBatch_R5_TargetOutsideErrorNotSuppressed(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)
	at := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repos.EnsureUsageLogPartitions(ctx, at, at))

	pgExec(t, pool, `CREATE UNIQUE INDEX usage_logs_test_model_created_at_uq ON usage_logs (model, created_at)`)
	t.Cleanup(func() { pgExec(t, pool, `DROP INDEX IF EXISTS usage_logs_test_model_created_at_uq`) })

	seed := usageLogFor("seed", at)
	seed.Model = "collision-model"
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{seed}))

	fresh := usageLogFor("fresh", at)
	fresh.Model = "collision-model"
	err := repos.Usages.InsertBatch(ctx, []*domain.UsageLog{fresh})
	require.Error(t, err)
	require.False(t, errors.Is(err, domain.ErrPartitionUnavailable))
	require.Equal(t, int64(1), pgCount(t, pool, `SELECT COUNT(*) FROM usage_logs WHERE request_id='seed'`))
	require.Equal(t, int64(0), pgCount(t, pool, `SELECT COUNT(*) FROM usage_logs WHERE request_id='fresh'`))
}

func TestUsageRepoInsertBatch_R8_ConcurrentSameBatchReplay(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)
	at := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repos.EnsureUsageLogPartitions(ctx, at, at))

	batch := []*domain.UsageLog{
		usageLogFor("c-1", at),
		usageLogFor("c-2", at),
	}

	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			errs[idx] = repos.Usages.InsertBatch(ctx, batch)
		}(i)
	}
	close(start)
	wg.Wait()

	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	require.Equal(t, int64(2), pgCount(t, pool, `SELECT COUNT(*) FROM usage_logs`))
}
