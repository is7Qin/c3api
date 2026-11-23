// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"
)

// The ...Shared helpers used to fan a single migrated schema out across the
// whole package. They now return handles onto the test's own private clone:
// pgtest.Clone is idempotent per test, so every ...Shared helper invoked within
// one test shares one database, while different tests stay isolated. No
// TRUNCATE / schema drop is needed, and the shared fixture reset (TestMain,
// resetPGSharedData, TestPGSharedFixtureIsolation) is gone.

// newPGReposShared returns a pooled repository on the test's private clone.
func newPGReposShared(t *testing.T) *repository.Repository {
	t.Helper()
	dsn := pgtest.Clone(t)
	pool := pgtest.OpenPool(t, dsn)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	repos, err := repository.NewWithPG(context.Background(), entsql.OpenDB(dialect.Postgres, db), false, pool)
	require.NoError(t, err)
	return repos
}

// pgSharedPool returns a second pool onto the same clone as newPGReposShared.
func pgSharedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return pgtest.OpenPool(t, pgtest.Clone(t))
}

// pgSharedConn returns a raw pgx connection onto the same clone.
func pgSharedConn(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), pgtest.Clone(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, conn.Close(ctx))
	})
	return conn
}

// ensureWritePartitions 预建夹具会写到的三张「近期写入」分区表在 [earliest, now+1h]
// 的每日分区。pgtest 克隆只保证 [now, now+1d]（对齐生产 -migrate up 的尾步 / 每小时
// 预建），而夹具常按「相对 now 的分钟/小时偏移」造历史行：跨 UTC 零点时这些行落在
// 昨天，缺分区就是 SQLSTATE 23514。
//
// 这是 package repository 内同名 helper 在外部测试包（repository_test）里的镜像：
// 两个测试包各自编译，unexported helper 不能跨包复用；实现与约定保持一致。
func ensureWritePartitions(t *testing.T, ctx context.Context, repos *repository.Repository, earliest time.Time) {
	t.Helper()
	until := time.Now().UTC().Add(time.Hour)
	require.NoError(t, repos.Partitions.EnsureAttemptLogPartitions(ctx, earliest, until), "attempt_logs 分区")
	require.NoError(t, repos.Partitions.EnsureErrLogPartitions(ctx, earliest, until), "err_logs 分区")
	require.NoError(t, repos.Partitions.EnsureUsageLogPartitions(ctx, earliest, until), "usage_logs 分区")
}

// newPGReposNoPoolShared returns a pool-less repository on the same clone
// (pool == nil → ent txDriver carrier; the counterpart of newPGReposShared's
// pgx direct-connection carrier).
func newPGReposNoPoolShared(t *testing.T) *repository.Repository {
	t.Helper()
	dsn := pgtest.Clone(t)
	pool := pgtest.OpenPool(t, dsn)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), false)
	require.NoError(t, err)
	return repos
}
