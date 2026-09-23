// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// 余额预警结算基准（真实 PG：TEST_DATABASE_URL 未设置 → b.Skip）：逐场景测
// SettleBalanceBatch / SettleFefoBatch 的耗时构成——每轮 StopTimer 重灌种子行
// （不计入计时），StartTimer 后跑 K 桶结算并采样 WAL 增量。
//
//	go test ./internal/repository/ -run '^$' -bench BenchmarkPGBalanceWarningSettlement -v

var balanceWarningBenchSeq atomic.Int64

type warningBenchScenario struct {
	name                 string
	rows, users, buckets int
	threshold            int64
	crossing, fefo       bool
}

func BenchmarkPGBalanceWarningSettlement(b *testing.B) {
	repos, pool := newBalanceWarningBenchRepository(b)

	scenarios := []warningBenchScenario{
		{"balance/single_500/disabled", 500, 1, 1, 0, false, false},
		{"balance/single_500/non_crossing", 500, 1, 1, 500, false, false},
		{"balance/single_500/crossing", 500, 1, 1, 500, true, false},
		{"balance/many_500/disabled", 500, 100, 1, 0, false, false},
		{"balance/many_500/non_crossing", 500, 100, 1, 500, false, false},
		{"balance/many_500/crossing", 500, 100, 1, 500, true, false},
		{"balance/drain_8000_k4/crossing", 8000, 40, 4, 500, true, false},
		{"fefo_spill/single_500/disabled", 500, 1, 1, 0, false, true},
		{"fefo_spill/single_500/non_crossing", 500, 1, 1, 500, false, true},
		{"fefo_spill/single_500/crossing", 500, 1, 1, 500, true, true},
		{"fefo_spill/many_500/disabled", 500, 100, 1, 0, false, true},
		{"fefo_spill/many_500/non_crossing", 500, 100, 1, 500, false, true},
		{"fefo_spill/many_500/crossing", 500, 100, 1, 500, true, true},
		{"fefo_spill/drain_8000_k4/crossing", 8000, 40, 4, 500, true, true},
	}
	for _, scenario := range scenarios {
		b.Run(scenario.name, func(b *testing.B) {
			var totalWAL int64
			for range b.N {
				b.StopTimer()
				seedBalanceWarningBench(b, repos, scenario)
				before := currentWALPosition(b, pool)
				b.StartTimer()
				result, err := settleWarningBuckets(scenario.buckets, func(bucket int) (domain.SettlementSummary, error) {
					if scenario.fefo {
						return repos.SettleFefoBatch(context.Background(), scenario.rows, scenario.buckets, bucket)
					}
					return repos.SettleBalanceBatch(context.Background(), scenario.rows, scenario.buckets, bucket)
				})
				b.StopTimer()
				require.NoError(b, err)
				require.Equal(b, int64(scenario.rows), result.Marked)
				require.Len(b, result.Balances, scenario.users)
				wantWarnings := 0
				if scenario.crossing {
					wantWarnings = scenario.users
				}
				require.Len(b, result.BalanceWarnings, wantWarnings)
				totalWAL += currentWALPosition(b, pool) - before
			}
			b.ReportMetric(float64(totalWAL)/float64(b.N), "wal-B/op")
			b.ReportMetric(float64(scenario.users+scenario.buckets), "result-rows/op")
			b.ReportMetric(float64(4*scenario.buckets), "db-roundtrips/op")
			b.ReportMetric(float64(scenario.buckets), "settlement-queries/op")
		})
	}
}

func newBalanceWarningBenchRepository(b *testing.B) (*Repository, *pgxpool.Pool) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		b.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL benchmark")
	}
	ctx := context.Background()
	pool, err := OpenPG(ctx, dsn, 5)
	require.NoError(b, err)
	b.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	b.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public`)
	require.NoError(b, err)
	repos, err := NewWithPG(ctx, entsql.OpenDB(dialect.Postgres, db), true, pool)
	require.NoError(b, err)
	require.NoError(b, repos.EnsureUsageLogPartitioned(ctx, time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)))
	return repos, pool
}

func seedBalanceWarningBench(b *testing.B, repos *Repository, scenario warningBenchScenario) {
	ctx := context.Background()
	fixture := balanceWarningBenchSeq.Add(1)
	rowsPerUser := scenario.rows / scenario.users
	logs := make([]*domain.UsageLog, 0, scenario.rows)
	for userIndex := range scenario.users {
		email := fmt.Sprintf("warning-bench-%d-%d@example.com", fixture, userIndex)
		user, err := repos.CreateUser(ctx, &domain.User{Email: email, PasswordHash: "hash", Role: domain.RoleUser, Status: domain.UserStatusActive})
		require.NoError(b, err)
		spill := int64(rowsPerUser * 100)
		if scenario.fefo {
			_, err = repos.Client.TempBalance.Create().SetUserID(user.ID).SetAmount(100).Save(ctx)
			require.NoError(b, err)
			spill -= 100
		}
		balance := spill + 1_000
		if scenario.crossing {
			balance = spill + scenario.threshold
		}
		_, err = repos.Client.User.UpdateOneID(user.ID).SetBalance(balance).
			SetBalanceWarningThreshold(scenario.threshold).Save(ctx)
		require.NoError(b, err)
		for rowIndex := range rowsPerUser {
			logs = append(logs, &domain.UsageLog{
				RequestID: fmt.Sprintf("warning-bench-%d-%d-%d", fixture, userIndex, rowIndex),
				UserID:    user.ID, Model: "gpt-4o", Format: domain.FormatOpenAIChat, ErrorType: domain.ErrNone,
				LatencyMS: 10, InputTokens: 3, OutputTokens: 5, TotalTokens: 8, Cost: 100,
				BillingTier: "auto", CreatedAt: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
			})
		}
	}
	for start := 0; start < len(logs); start += 500 {
		require.NoError(b, repos.Usages.InsertBatch(ctx, logs[start:min(start+500, len(logs))]))
	}
}

func settleWarningBuckets(buckets int, settle func(int) (domain.SettlementSummary, error)) (domain.SettlementSummary, error) {
	if buckets == 1 {
		return settle(0)
	}
	results := make([]domain.SettlementSummary, buckets)
	errs := make([]error, buckets)
	var wait sync.WaitGroup
	for bucket := range buckets {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results[bucket], errs[bucket] = settle(bucket)
		}()
	}
	wait.Wait()
	var total domain.SettlementSummary
	for bucket, result := range results {
		if errs[bucket] != nil {
			return domain.SettlementSummary{}, errs[bucket]
		}
		total.Marked += result.Marked
		total.Balances = append(total.Balances, result.Balances...)
		total.BalanceWarnings = append(total.BalanceWarnings, result.BalanceWarnings...)
	}
	return total, nil
}

func currentWALPosition(b *testing.B, pool *pgxpool.Pool) int64 {
	var position int64
	require.NoError(b, pool.QueryRow(context.Background(),
		`SELECT pg_wal_lsn_diff(pg_current_wal_insert_lsn(), '0/0')::bigint`).Scan(&position))
	return position
}
