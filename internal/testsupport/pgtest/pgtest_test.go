// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package pgtest_test

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"

	// Registers pgtest's repository-dependent hooks (template migration and the
	// per-clone day-partition top-up).
	_ "github.com/is7qin/c3api/internal/testsupport/pgtest/pgrepo"
)

func skipNoPG(t *testing.T) {
	t.Helper()
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping PostgreSQL integration test")
	}
}

// openMaint opens a raw connection to the maintenance database so the tests can
// inspect/modify template databases directly.
func openMaint(t *testing.T) *pgx.Conn {
	t.Helper()
	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)
	maint := os.Getenv("C3API_TEST_MAINT_DB")
	if maint == "" {
		maint = "postgres"
	}
	u.Path = "/" + maint
	conn, err := pgx.Connect(context.Background(), u.String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// TestCloneIdempotentIsolated pins the per-test clone contract: repeated Clone
// calls return the same DSN, and CloneEmpty gives an independent, unmigrated
// database.
func TestCloneIdempotentIsolated(t *testing.T) {
	skipNoPG(t)
	dsn1 := pgtest.Clone(t)
	dsn2 := pgtest.Clone(t)
	require.Equal(t, dsn1, dsn2, "Clone must be idempotent per test")

	emptyDSN := pgtest.CloneEmpty(t)
	require.NotEqual(t, dsn1, emptyDSN, "CloneEmpty is a separate database")

	ctx := context.Background()
	migrated := pgtest.OpenPool(t, dsn1)
	empty := pgtest.OpenPool(t, emptyDSN)

	var inMigrated bool
	require.NoError(t, migrated.QueryRow(ctx,
		`SELECT to_regclass('public.users') IS NOT NULL`).Scan(&inMigrated))
	require.True(t, inMigrated, "Clone is migrated from the template")

	var inEmpty bool
	require.NoError(t, empty.QueryRow(ctx,
		`SELECT to_regclass('public.users') IS NOT NULL`).Scan(&inEmpty))
	require.False(t, inEmpty, "CloneEmpty carries no migrated tables")

	// The two clones are independent databases: an object created in one is
	// invisible from the other.
	_, err := migrated.Exec(ctx, `CREATE TABLE iso_probe(id int)`)
	require.NoError(t, err)
	var probeInEmpty bool
	require.NoError(t, empty.QueryRow(ctx,
		`SELECT to_regclass('public.iso_probe') IS NOT NULL`).Scan(&probeInEmpty))
	require.False(t, probeInEmpty, "clones must be mutually invisible")
}

// dsnFor rewrites the configured DSN's database name, mirroring how pgtest
// addresses its template and clone databases.
func dsnFor(t *testing.T, db string) string {
	t.Helper()
	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)
	u.Path = "/" + db
	return u.String()
}

// TestCloneTopsUpDayPartitionsFromStaleTemplate pins acceptance A5: a template
// carries only the day partitions that existed when it was built, and the
// template is reused for as long as its source hash is unchanged. A clone made
// from a template built on an earlier day must therefore have today's partition
// created for it by the per-clone top-up (CloneEmpty skips that step, so this
// exercises the Clone path).
func TestCloneTopsUpDayPartitionsFromStaleTemplate(t *testing.T) {
	skipNoPG(t)
	ctx := context.Background()
	m := openMaint(t)

	// Build a template that is already a couple of days old: its four
	// partitioned tables carry partitions for (now-2) and (now-1) only, never
	// today's. This stands in for a reusable template whose source hash has not
	// changed since it was built.
	staleName := "c3api_test_template_staleprobe"
	_, err := m.Exec(ctx, "DROP DATABASE IF EXISTS "+staleName+" WITH (FORCE)")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = m.Exec(context.Background(), "DROP DATABASE IF EXISTS "+staleName+" WITH (FORCE)")
	})
	_, err = m.Exec(ctx, "CREATE DATABASE "+staleName)
	require.NoError(t, err)

	sqlDB, err := sql.Open("pgx", dsnFor(t, staleName))
	require.NoError(t, err)
	repo, err := repository.New(entsql.OpenDB(dialect.Postgres, sqlDB), false)
	require.NoError(t, err)
	past := time.Now().UTC().AddDate(0, 0, -2)
	require.NoError(t, repo.EnsureUsageLogPartitioned(ctx, past))
	require.NoError(t, repo.EnsureErrLogPartitioned(ctx, past))
	require.NoError(t, repo.EnsureUsageStatsPartitioned(ctx, past))
	require.NoError(t, repo.EnsureUsageEntityStatsPartitioned(ctx, past))
	// Leave no resident connection on the template before cloning from it.
	require.NoError(t, sqlDB.Close())

	// Point pgtest at the stale template and clone from it.
	t.Setenv("C3API_TEST_TEMPLATE", staleName)
	dsn := pgtest.Clone(t)

	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	today := time.Now().UTC().Format("20060102")
	for _, tbl := range []string{"usage_logs", "err_logs", "usage_stats", "usage_entity_stats"} {
		var ok bool
		require.NoError(t, conn.QueryRow(ctx,
			`SELECT to_regclass($1) IS NOT NULL`, "public."+tbl+"_"+today).Scan(&ok))
		require.True(t, ok, "clone must carry today's %s partition (daily top-up)", tbl)
	}
}

// TestTemplateReusedAcrossClones pins that an existing template is reused rather
// than rebuilt: each of two independent clones (each created inside its own
// subtest, so each re-enters ensureTemplate) leaves the template's OID unchanged.
func TestTemplateReusedAcrossClones(t *testing.T) {
	skipNoPG(t)
	name, err := pgtest.TemplateName()
	require.NoError(t, err)
	m := openMaint(t)
	ctx := context.Background()

	var oids []uint32
	cloneAndRecordOID := func(t *testing.T) {
		pgtest.Clone(t) // builds the template on the first call, reuses it after
		var oid uint32
		require.NoError(t, m.QueryRow(ctx,
			`SELECT oid FROM pg_database WHERE datname = $1`, name).Scan(&oid))
		oids = append(oids, oid)
	}
	t.Run("first clone builds or reuses", cloneAndRecordOID)
	t.Run("second clone reuses", cloneAndRecordOID)

	require.Len(t, oids, 2)
	require.NotZero(t, oids[0])
	require.Equal(t, oids[0], oids[1], "an existing template must be reused, not rebuilt")
}

// TestOpenPoolMaxConns pins the pool ceiling (dbPoolMax = 4).
func TestOpenPoolMaxConns(t *testing.T) {
	skipNoPG(t)
	pool := pgtest.OpenPool(t, pgtest.Clone(t))
	require.Equal(t, int32(4), pool.Config().MaxConns)
	require.Equal(t, "5s", pool.Config().ConnConfig.RuntimeParams["lock_timeout"])
}

// TestSweepStaleTemplates pins that sweep drops only template databases whose
// name differs from the current one, and never the current template.
func TestSweepStaleTemplates(t *testing.T) {
	skipNoPG(t)
	pgtest.Clone(t) // ensure the current template exists
	ctx := context.Background()
	m := openMaint(t)

	stale := "c3api_test_template_000000000000"
	_, err := m.Exec(ctx, `DROP DATABASE IF EXISTS `+stale+` WITH (FORCE)`)
	require.NoError(t, err)
	_, err = m.Exec(ctx, `CREATE DATABASE `+stale)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = m.Exec(context.Background(), `DROP DATABASE IF EXISTS `+stale+` WITH (FORCE)`)
	})

	dropped, err := pgtest.SweepStaleTemplates(ctx)
	require.NoError(t, err)
	require.Contains(t, dropped, stale)

	current, err := pgtest.TemplateName()
	require.NoError(t, err)
	require.NotContains(t, dropped, current, "sweep must keep the current template")

	var exists bool
	require.NoError(t, m.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, stale).Scan(&exists))
	require.False(t, exists, "stale template must be gone")
}
