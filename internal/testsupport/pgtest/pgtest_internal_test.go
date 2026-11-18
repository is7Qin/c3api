// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package pgtest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMigrationHashFilesFollowsDirectory pins that the template hash takes its
// migration list from the directory itself. A hardcoded list (even "every file
// that exists today") keeps the suite green on the release that adds a
// migration, while every test then clones a template built from the previous
// schema — the exact 假绿 this hash exists to prevent.
func TestMigrationHashFilesFollowsDirectory(t *testing.T) {
	dir := t.TempDir()

	write := func(name string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("-- probe\n"), 0o600))
	}

	// Nothing to hash is an error, not an empty list: an empty template would
	// otherwise be built and reused under a stable name.
	_, err := migrationHashFiles(dir)
	require.Error(t, err, "an empty migrations dir must fail the build, not hash nothing")

	write("0001_baseline.up.sql")
	got, err := migrationHashFiles(dir)
	require.NoError(t, err)
	require.Equal(t, []string{"0001_baseline.up.sql"}, got)

	// A second migration (and a sibling down file) must appear without touching
	// any code; non-SQL files are not part of the schema.
	write("0001_baseline.down.sql")
	write("0002_probe.up.sql")
	write("notes.txt")
	got, err = migrationHashFiles(dir)
	require.NoError(t, err)
	require.Equal(t,
		[]string{"0001_baseline.down.sql", "0001_baseline.up.sql", "0002_probe.up.sql"}, got,
		"the list must come from the directory and stay sorted (same content ⇒ same template name)")
}

// TestTemplateHashUsesMigrationDirectory pins the production call site: the
// template inputs must be exactly migrationHashFiles(<repo>/internal/db/migrate/migrations).
// Hashing a different directory (or a subset) would make "a changed migration
// rebuilds the template" false while this file's other test stays green.
func TestTemplateHashUsesMigrationDirectory(t *testing.T) {
	e, err := loadEnv()
	require.NoError(t, err)
	inputs, err := templateHashInputs(e)
	require.NoError(t, err)

	dir := filepath.Join(e.root, "internal", "db", "migrate", "migrations")
	want, err := migrationHashFiles(dir)
	require.NoError(t, err)

	var got []string
	for _, p := range inputs {
		if filepath.Dir(p) == dir {
			got = append(got, filepath.Base(p))
		}
	}
	require.Equal(t, want, got,
		"templateHashInputs must hash exactly the files migrationHashFiles reports for the embedded migrations")
}

// TestTemplateHashCoversMigrations pins that the template name depends on the
// things that actually build the schema. The template is built by running the
// versioned migrations, so a changed migration file must produce a different
// name — otherwise the suite clones an old template and stays green while
// testing a schema nobody ships (假绿).
//
// The two version constants (`migrate.RequiredVersion`,
// `repository.EnsureAllTablePartitionsVersion`) are covered by content hashing of
// the files that define them: pgtest cannot import those packages (their own
// tests import pgtest, so the reverse import is a test import cycle).
func TestTemplateHashCoversMigrations(t *testing.T) {
	e, err := loadEnv()
	require.NoError(t, err)
	inputs, err := templateHashInputs(e)
	require.NoError(t, err)

	var migrations, migratePkg, repositoryFiles int
	for _, p := range inputs {
		switch {
		case strings.Contains(p, filepath.Join("internal", "db", "migrate", "migrations")):
			migrations++
		case strings.HasSuffix(p, filepath.Join("db", "migrate", "migrate.go")):
			migratePkg++
		case strings.HasSuffix(p, filepath.Join("db", "migrate", "runner.go")):
			migratePkg++
		case strings.Contains(p, filepath.Join("internal", "repository")):
			repositoryFiles++
		}
	}
	require.NotZero(t, migrations, "template hash must include every embedded migration file")
	require.Equal(t, 2, migratePkg, "template hash must include the migration runner sources (they define RequiredVersion)")
	require.NotZero(t, repositoryFiles, "template hash must include the repository sources that carry the partition rules and seeds")

	// 名字必须随之改变（这样"改了迁移仍复用旧模板"在结构上不可能）。
	name, err := templateName(e)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(name, templatePrefix))
	require.Len(t, strings.TrimPrefix(name, templatePrefix), hashLen)
}

// TestEnsureTemplateDropsOnMigrateFailure pins the "any failure after CREATE →
// DROP + error" contract (spec §5.7/A5): a template whose migration fails must
// not be left behind, or a later run would see the name and reuse a half-built
// database. It exercises ensureTemplate directly with a migrate function that
// always errors, driven through a dedicated C3API_TEST_TEMPLATE probe so the
// shared current-hash template — which parallel packages may be cloning — is
// never touched.
func TestEnsureTemplateDropsOnMigrateFailure(t *testing.T) {
	m, e := state(t)
	ctx := context.Background()

	const probe = "c3api_test_template_migrate_fail_probe"
	t.Setenv("C3API_TEST_TEMPLATE", probe)
	_, _ = m.conn.ExecContext(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(probe)+" WITH (FORCE)")
	t.Cleanup(func() {
		_, _ = m.conn.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+quoteIdent(probe)+" WITH (FORCE)")
	})

	sentinel := errors.New("boom: migration failed")
	failing := func(context.Context, string) error { return sentinel }

	_, err := ensureTemplate(t, m, e, failing)
	require.ErrorIs(t, err, sentinel)

	var exists bool
	require.NoError(t, m.conn.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, probe).Scan(&exists))
	require.False(t, exists, "a template whose migration failed must be dropped, not left behind")

	// A second attempt must also not find (and reuse) a leftover half-built
	// template: it rebuilds, fails again, and the probe is gone again.
	_, err = ensureTemplate(t, m, e, failing)
	require.ErrorIs(t, err, sentinel)
	require.NoError(t, m.conn.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, probe).Scan(&exists))
	require.False(t, exists, "the retry must also leave no half-built template")
}
