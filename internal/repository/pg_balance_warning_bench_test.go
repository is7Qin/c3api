// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

const (
	benchBalanceLegacyHash = "e9ea120fef3b3a6e685763ecc74fad380885d7ada83ad90d6cfa8555e4375d2f"
	benchFefoLegacyHash    = "9b754fe3ea890133e3b5a0015b40b69a200cfa3619b8847b7822ee4b71cdb0ba"
)

var balanceWarningBenchSeq atomic.Int64

type warningBenchScenario struct {
	name                 string
	rows, users, buckets int
	threshold            int64
	crossing, fefo       bool
}

type legacySettleRequest struct {
	pool             *pgxpool.Pool
	sql              string
	limit, k, bucket int
}

func BenchmarkPGBalanceWarningSettlement(b *testing.B) {
	repos, pool := newBalanceWarningBenchRepository(b)
	balanceLegacy := legacySettlementSQL(settleBalanceSQL, "t.delta")
	fefoLegacy := legacySettlementSQL(settleFefoSQL, "s.spill AS delta")
	require.Equal(b, benchBalanceLegacyHash, fmt.Sprintf("%x", sha256.Sum256([]byte(balanceLegacy))))
	require.Equal(b, benchFefoLegacyHash, fmt.Sprintf("%x", sha256.Sum256([]byte(fefoLegacy))))

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
		for variantIndex := range 2 {
			current := (variantIndex == 1) != (os.Getenv("WARNING_BENCH_CURRENT_FIRST") != "")
			variant := "legacy_head"
			if current {
				variant = "warning_current"
			}
			b.Run(scenario.name+"/variant="+variant, func(b *testing.B) {
				var totalWAL int64
				for range b.N {
					b.StopTimer()
					seedBalanceWarningBench(b, repos, scenario)
					before := currentWALPosition(b, pool)
					b.StartTimer()
					result, err := settleWarningBuckets(scenario.buckets, func(bucket int) (domain.SettlementSummary, error) {
						if current {
							if scenario.fefo {
								return repos.SettleFefoBatch(context.Background(), scenario.rows, scenario.buckets, bucket)
							}
							return repos.SettleBalanceBatch(context.Background(), scenario.rows, scenario.buckets, bucket)
						}
						legacySQL := balanceLegacy
						if scenario.fefo {
							legacySQL = fefoLegacy
						}
						return runLegacySettle(context.Background(), legacySettleRequest{pool, legacySQL, scenario.rows, scenario.buckets, bucket})
					})
					b.StopTimer()
					require.NoError(b, err)
					require.Equal(b, int64(scenario.rows), result.Marked)
					require.Len(b, result.Balances, scenario.users)
					wantWarnings := 0
					if current && scenario.crossing {
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

func runLegacySettle(ctx context.Context, request legacySettleRequest) (domain.SettlementSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, settleTimeout)
	defer cancel()
	conn, err := request.pool.Acquire(ctx)
	if err != nil {
		return domain.SettlementSummary{}, err
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return domain.SettlementSummary{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, billingSyncCommitOffSQL); err != nil {
		return domain.SettlementSummary{}, err
	}
	rows, err := tx.Query(ctx, request.sql, request.limit, request.k, request.bucket)
	if err != nil {
		return domain.SettlementSummary{}, err
	}
	result, err := scanLegacySettle(rows)
	if err != nil || result.Marked != result.BatchRows {
		return result, err
	}
	return result, tx.Commit(ctx)
}

func scanLegacySettle(rows pgx.Rows) (domain.SettlementSummary, error) {
	defer rows.Close()
	var result domain.SettlementSummary
	seen := false
	for rows.Next() {
		var uid, balance, batch, debited, forced, marked, ghosts int64
		if err := rows.Scan(&uid, &balance, &batch, &debited, &forced, &marked, &ghosts); err != nil {
			return domain.SettlementSummary{}, err
		}
		if !seen && uid == -1 {
			result = domain.SettlementSummary{BatchRows: batch, DebitedUsers: debited, ForcedUsers: forced, Marked: marked, Quarantined: ghosts}
			seen = true
			continue
		}
		result.Balances = append(result.Balances, domain.UserBalance{UserID: uid, Balance: balance})
	}
	if err := rows.Err(); err != nil {
		return domain.SettlementSummary{}, err
	}
	if !seen {
		return domain.SettlementSummary{}, fmt.Errorf("legacy billing settle: aggregate sentinel row missing")
	}
	return result, nil
}

func legacySettlementSQL(current, deltaProjection string) string {
	return strings.NewReplacer(
		"RETURNING u.id AS uid, u.balance AS balance_after, "+deltaProjection+",\n\t\tu.balance_warning_threshold AS threshold, u.email)",
		"RETURNING u.id AS uid, u.balance AS balance_after)",
		"changed AS (\n\tSELECT settled.*,\n\t\tthreshold > 0 AND balance_after + delta > threshold\n\t\t\tAND balance_after <= threshold AS crossed\n\tFROM (\n\t\tSELECT uid, balance_after, delta, threshold, email FROM debited\n\t\tUNION ALL\n\t\tSELECT uid, balance_after, delta, threshold, email FROM forced) settled),\n", "",
		"(SELECT COUNT(*) FROM ghosts)::bigint,\n\tNULL::bigint, NULL::text\nUNION ALL\nSELECT uid, balance_after, 0, 0, 0, 0, 0,\n\tCASE WHEN crossed THEN threshold END,\n\tCASE WHEN crossed THEN email END\nFROM changed\nORDER BY 1",
		"(SELECT COUNT(*) FROM ghosts)::bigint\nUNION ALL\nSELECT uid, balance_after, 0, 0, 0, 0, 0 FROM debited\nUNION ALL\nSELECT uid, balance_after, 0, 0, 0, 0, 0 FROM forced\nORDER BY 1",
	).Replace(current)
}

func currentWALPosition(b *testing.B, pool *pgxpool.Pool) int64 {
	var position int64
	require.NoError(b, pool.QueryRow(context.Background(),
		`SELECT pg_wal_lsn_diff(pg_current_wal_insert_lsn(), '0/0')::bigint`).Scan(&position))
	return position
}
