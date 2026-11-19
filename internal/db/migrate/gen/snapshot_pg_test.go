// SPDX-License-Identifier: AGPL-3.0-or-later
package gen

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/db/migrate"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"
)

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	body, err := os.ReadFile(src)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dst, body, 0o644))
}

func copyArtifacts(t *testing.T, migrationsDir, testdataDir string) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	migs, testdata := filepath.Join(tmp, "migrations"), filepath.Join(tmp, "testdata")
	require.NoError(t, os.MkdirAll(migs, 0o755))
	require.NoError(t, os.MkdirAll(testdata, 0o755))
	entries, err := os.ReadDir(migrationsDir)
	require.NoError(t, err)
	for _, e := range entries {
		if !e.IsDir() {
			copyFile(t, filepath.Join(migrationsDir, e.Name()), filepath.Join(migs, e.Name()))
		}
	}
	entries, err = os.ReadDir(testdataDir)
	require.NoError(t, err)
	for _, e := range entries {
		if !e.IsDir() {
			copyFile(t, filepath.Join(testdataDir, e.Name()), filepath.Join(testdata, e.Name()))
		}
	}
	return migs, testdata
}

func TestSnapshotHeadMatchesCommitted(t *testing.T) {
	dsn := pgtest.DSN(t)
	migrationsDir, testdataDir, err := defaultDirs()
	require.NoError(t, err)
	tag, text, err := SnapshotHead(context.Background(), Config{DSN: dsn, MigrationsDir: migrationsDir, TestdataDir: testdataDir})
	require.NoError(t, err)
	head, _ := migrate.Head()
	require.Equal(t, fmt.Sprintf("%04d", head), tag)
	want, err := os.ReadFile(filepath.Join(testdataDir, tag+".catalog.txt"))
	require.NoError(t, err)
	require.Equal(t, string(want), text, "head snapshot must be reproducible")
}

func TestCheckDetectsHeadDrift(t *testing.T) {
	dsn := pgtest.DSN(t)
	migrationsDir, testdataDir, err := defaultDirs()
	require.NoError(t, err)
	migs, testdata := copyArtifacts(t, migrationsDir, testdataDir)
	head, _ := migrate.Head()
	path := filepath.Join(testdata, fmt.Sprintf("%04d.catalog.txt", head))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString("drift\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	_, err = Check(context.Background(), Config{DSN: dsn, MigrationsDir: migs, TestdataDir: testdata})
	require.ErrorContains(t, err, fmt.Sprintf("%04d.catalog.txt", head))
	require.NotContains(t, err.Error(), "0001.catalog.txt: missing")
}

func TestMigrationFilesRejectsBadStem(t *testing.T) {
	for _, name := range []string{"x0002_b.up.sql", "0000_zero.up.sql", "12345_wide.up.sql"} {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("-- x\n"), 0o644))
		_, err := migrationFiles(dir)
		require.Error(t, err, name)
	}
	_, err := migrationFiles(t.TempDir())
	require.Error(t, err, "empty migrations dir")
}

func TestUnregisteredPartitionParent(t *testing.T) {
	require.Equal(t, "foo", unregisteredPartitionParent("table|foo|relkind=p|persistence=p|tablespace=|options=\n"))
	require.Empty(t, unregisteredPartitionParent("table|attempt_logs|relkind=p|persistence=p|tablespace=|options=\n"))
	require.Empty(t, unregisteredPartitionParent("table|foo|relkind=r|persistence=p|tablespace=|options=\n"))
}

func TestRenderCatalogRejectsUnregisteredPartitionParent(t *testing.T) {
	dsn := pgtest.DSN(t)
	_, err := renderCatalog(context.Background(), dsn,
		`CREATE TABLE public.foo (id bigint NOT NULL, created_at timestamptz NOT NULL, PRIMARY KEY (id, created_at)) PARTITION BY RANGE (created_at)`)
	require.ErrorContains(t, err, "not registered in catalog.PartitionParents")
}

func TestSnapshotHeadRejectsCRLFAndDuplicateVersions(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "0001_a.up.sql"), []byte("SELECT 1;\n"), 0o644))
	crlf := filepath.Join(dir, "0002_b.up.sql")
	require.NoError(t, os.WriteFile(crlf, []byte("SELECT 1;\r\n"), 0o644))
	_, _, err := snapshotHead(context.Background(), Config{MigrationsDir: dir}, "SELECT 1;")
	require.ErrorContains(t, err, "must be LF-only without BOM")

	require.NoError(t, os.WriteFile(crlf, []byte("SELECT 1;\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "0002_c.up.sql"), []byte("SELECT 2;\n"), 0o644))
	_, err = migrationFiles(dir)
	require.ErrorContains(t, err, "duplicate migration version 0002")
}
