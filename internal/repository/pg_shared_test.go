// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

var sharedPG struct {
	pool   *pgxpool.Pool
	db     *sql.DB
	repo   *repository.Repository
	schema string
}

func TestMain(m *testing.M) {
	code := m.Run()
	if sharedPG.db != nil {
		if _, err := sharedPG.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+sharedPGSchema()+` CASCADE`); err != nil {
			fmt.Fprintf(os.Stderr, "shared PostgreSQL schema cleanup failed: %v\n", err)
			if code == 0 {
				code = 1
			}
		}
		if err := sharedPG.db.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "shared PostgreSQL database close failed: %v\n", err)
			if code == 0 {
				code = 1
			}
		}
	}
	if sharedPG.pool != nil {
		sharedPG.pool.Close()
	}
	os.Exit(code)
}

func newPGReposShared(t *testing.T) *repository.Repository {
	t.Helper()
	ensurePGShared(t)
	resetPGSharedData(t)
	return sharedPG.repo
}

func ensurePGShared(t *testing.T) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	if sharedPG.repo == nil {
		ctx := context.Background()
		cfg, err := pgxpool.ParseConfig(dsn)
		require.NoError(t, err)
		cfg.MaxConns = 5
		cfg.MaxConnLifetime = 30 * time.Minute
		cfg.ConnConfig.RuntimeParams["lock_timeout"] = "5s"
		cfg.ConnConfig.RuntimeParams["search_path"] = sharedPGSchema()
		sharedPG.pool, err = pgxpool.NewWithConfig(ctx, cfg)
		require.NoError(t, err)
		sharedPG.db = stdlib.OpenDBFromPool(sharedPG.pool)
		_, err = sharedPG.db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+sharedPGSchema()+` CASCADE`)
		require.NoError(t, err)
		_, err = sharedPG.db.ExecContext(ctx, `CREATE SCHEMA `+sharedPGSchema())
		require.NoError(t, err)
		_, err = sharedPG.db.ExecContext(ctx, `SET search_path TO `+sharedPGSchema())
		require.NoError(t, err)
		sharedPG.repo, err = repository.NewWithPG(ctx, entsql.OpenDB(dialect.Postgres, sharedPG.db), true, sharedPG.pool)
		require.NoError(t, err)
		require.NoError(t, sharedPG.repo.EnsureUsageLogPartitioned(ctx, time.Now()))
		require.NoError(t, sharedPG.repo.EnsureErrLogPartitioned(ctx, time.Now()))
		require.NoError(t, sharedPG.repo.EnsureUsageStatsPartitioned(ctx, time.Now()))
		require.NoError(t, sharedPG.repo.EnsureUsageEntityStatsPartitioned(ctx, time.Now()))
		require.NoError(t, sharedPG.repo.EnsurePriceVariantsEffectCheck(ctx))
	}
}

func pgSharedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ensurePGShared(t)
	return sharedPG.pool
}

func pgSharedConn(t *testing.T) *pgx.Conn {
	t.Helper()
	ensurePGShared(t)
	cfg, err := pgx.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)
	cfg.RuntimeParams["search_path"] = sharedPGSchema()
	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, conn.Close(ctx))
	})
	return conn
}

func newPGReposNoPoolShared(t *testing.T) *repository.Repository {
	t.Helper()
	ensurePGShared(t)
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, sharedPG.db), false)
	require.NoError(t, err)
	return repos
}

func sharedPGSchema() string {
	if sharedPG.schema == "" {
		sharedPG.schema = "c3api_test_shared_" + strconv.Itoa(os.Getpid()) + "_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return sharedPG.schema
}

func resetPGSharedData(t *testing.T) {
	t.Helper()
	_, err := sharedPG.pool.Exec(context.Background(), `TRUNCATE TABLE
		account_groups, group_assignments, account_exts, template_exts, keys,
		temp_balances, redemption_uses, accounts, groups, users,
		redemption_codes, rules, settings, email_templates, price_variants,
		price_entries, templates, usage_logs, err_logs, usage_stats,
		usage_entity_stats, stats_agg_watermark RESTART IDENTITY`)
	require.NoError(t, err)
}

func TestPGSharedFixtureIsolation(t *testing.T) {
	for _, name := range []string{"first", "second"} {
		t.Run(name, func(t *testing.T) {
			repos := newPGReposShared(t)
			_, err := repos.Templates.CreateTemplate(context.Background(), testSharedTemplate(name))
			require.NoError(t, err)
			rows, _, err := repos.Templates.ListTemplates(context.Background(), repository.ListQuery{})
			require.NoError(t, err)
			require.Len(t, rows, 1)
			require.Equal(t, name, rows[0].Name)
		})
	}
	var currentSchema string
	err := sharedPG.db.QueryRowContext(context.Background(), `SELECT current_schema()`).Scan(&currentSchema)
	require.NoError(t, err)
	require.Equal(t, sharedPGSchema(), currentSchema, "shared *sql.DB must use the shared schema")
	count, err := sharedPG.repo.Client.Template.Query().Count(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, count, "shared Ent client must use the shared schema")
	var partitioned bool
	err = sharedPG.pool.QueryRow(context.Background(), `SELECT relkind = 'p' FROM pg_class WHERE oid = 'usage_logs'::regclass`).Scan(&partitioned)
	require.NoError(t, err)
	require.True(t, partitioned, "shared cleanup must preserve partition definitions")
}

func testSharedTemplate(name string) *domain.Template {
	return &domain.Template{
		Name:             name,
		BaseURL:          "https://u/v1",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		ModelMapping:     domain.ModelMapping{},
	}
}
