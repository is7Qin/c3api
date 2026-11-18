// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"database/sql"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	// 注册 database/sql 的 pgx 驱动。
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"
)

// TestNewWithoutMigrateCreatesNoSchema 钉住「app 启动不建表」这条承诺的行为面
// （spec §1 承诺 1）。
//
// 为什么必须有它：把 NewWithoutMigrate 改成委托给 ent 自动迁移（`NewWithPG(..., true, ...)`）
// 时，整个 `go test ./...` 仍然全绿——没有任何用例观察过「构造仓库会写 schema」。
// 这里用一个**空库**做观察点：构造前后 public 下的表数必须是 0；一旦自动迁移回来，
// 会冒出 27 张 ent 表，断言立刻红。
//
// 注意它守的是**构造函数**这一侧；`cmd/server` 的调用点是否被换成 NewWithPG 由
// cmd/server/automigrate_gate_test.go 的源码门守（两侧都要有，缺一条就能绕过）。
func TestNewWithoutMigrateCreatesNoSchema(t *testing.T) {
	ctx := context.Background()
	db := openEmptyDatabase(t)

	repos, err := repository.NewWithoutMigrate(ctx, entsql.OpenDB(dialect.Postgres, db), nil)
	require.NoError(t, err)
	require.NotNil(t, repos)

	require.Zero(t, countPublicTables(t, db),
		"NewWithoutMigrate 不得建任何表：建表真相只有 `c3api -migrate up` 一处")
	for _, absent := range []string{"users", "usage_logs", "schema_migrations"} {
		var exists bool
		require.NoError(t, db.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, "public."+absent).Scan(&exists))
		require.False(t, exists, "构造仓库不得建表：%s", absent)
	}
}

// openEmptyDatabase 打开一个私有空库（pgtest 克隆，隔离粒度 = database）。
func openEmptyDatabase(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", pgtest.CloneEmpty(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.PingContext(context.Background()))
	return db
}

// countPublicTables 数 public 下的表/分区父表（不含索引、序列、分区子表）。
func countPublicTables(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM pg_class c JOIN pg_namespace ns ON ns.oid = c.relnamespace
		 WHERE ns.nspname = 'public' AND c.relkind IN ('r', 'p') AND NOT c.relispartition`).Scan(&n))
	return n
}
