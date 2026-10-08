// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package pgtest hands each PostgreSQL integration test its own isolated
// database, cloned from a single migrated template with CREATE DATABASE ...
// TEMPLATE.
//
// Cloning a small database is a file-level copy, far cheaper than replaying the
// full ent migration per test, and it removes the shared-schema contention that
// used to force the whole suite to run serially (go test -p 1).
//
// Lifecycle, and the "the template has no resident connection" invariant it
// protects:
//
//   - One maintenance connection is opened lazily against the maintenance
//     database (postgres by default, C3API_TEST_MAINT_DB to override) and reused
//     for template bookkeeping and clone DDL. It never points at the template.
//   - pg_advisory_lock serialises template (re)creation across every test
//     binary on the instance.
//   - The template name embeds the content hash of the schema/migration
//     sources, so checkouts sharing the same sources agree on it. Readiness is
//     not "the database exists" alone: a template is reused only when it also
//     carries a build-complete marker (a database COMMENT, templateReadyMarker)
//     written as the final step of a successful build. A database whose name
//     matches but whose marker is absent or wrong — an empty/half-built
//     template left by a killed build, or an unmarked override — is dropped
//     and rebuilt, so a half-built template can never masquerade as fresh. This
//     supersedes the earlier clone spec's "an existing template is reused
//     without reading inside it" invariant (clone-spec §2.2/§8.2).
//   - The template build opens its own database/sql connection and closes it
//     before returning, so no connection is pointing at the template by the time
//     any CREATE DATABASE ... TEMPLATE runs.
//
// pgtest is a normal (non _test.go) package in the production tree. Nothing but
// tests and test tooling imports it, so it never reaches a production binary.
//
// Data-layer wiring: the two steps that must talk to the ent/repository data
// layer — migrating a fresh template and topping up a clone's day partitions —
// are injected via RegisterDataLayerHooks by package pgrepo (which imports the
// repository). Keeping this file free of the repository import lets internal
// (package repository) tests import pgtest without the "import cycle not allowed
// in test" error; those tests only use CloneEmpty, which needs no hook.
package pgtest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	// Registers the "pgx" driver the maintenance connection and template build
	// open through database/sql. The data layer links pgx in as well; importing
	// the stdlib adapter here keeps pgtest's database/sql usage self-contained
	// (and deliberately avoids github.com/lib/pq).
	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	// templatePrefix names migrated template databases. Only databases matching
	// it are swept; per-test clones never are.
	templatePrefix = "c3api_test_template_"
	// clonePrefix names the per-test clones. It deliberately differs from
	// templatePrefix (and from the base database name c3api_test) so a sweep can
	// never match a clone.
	clonePrefix = "c3api_clone_"
	// defaultMaintDB is the maintenance database used for CREATE/DROP DATABASE
	// and advisory locks when C3API_TEST_MAINT_DB is unset.
	defaultMaintDB = "postgres"
	// templateLockKey is the advisory-lock key that serialises template rebuilds
	// across test binaries. Its value is arbitrary but fixed.
	templateLockKey = int64(0x6333617069706774)
	// templateReadyMarker is written as a COMMENT on a template database as the
	// last step of a successful build. Its presence (and exact value) is what
	// makes a template reusable; an empty or half-built template — one whose
	// build was killed before this step — never carries it.
	templateReadyMarker = "pgtest:template-ready"
	// hashLen is how many hex digits of the source hash go into a template name.
	hashLen = 12
	// identMaxLen is PostgreSQL's identifier length limit in bytes.
	identMaxLen = 63
	// dbPoolMax is the connection-pool ceiling OpenPool gives every pool it opens
	// for a test. Kept small: the budget is -p concurrent binaries times this.
	dbPoolMax = 4
)

// skipMessage is returned by env resolution when TEST_DATABASE_URL is unset.
const skipMessage = "TEST_DATABASE_URL not set; skipping PostgreSQL integration test"

// Data-layer hooks. They are registered once (via an init in package pgrepo)
// and must be non-nil before Clone is used.
var (
	hookMu           sync.RWMutex
	migrateTemplate  func(ctx context.Context, dsn string) error
	ensureCloneDaily func(ctx context.Context, dsn string) error
)

// RegisterDataLayerHooks installs the repository-dependent steps pgtest cannot
// perform itself: migrateTemplate builds a fresh template database, and
// ensureCloneDaily tops up a clone's day partitions (a template only carries the
// partitions that existed when it was built). Both are idempotent.
func RegisterDataLayerHooks(migrate, cloneDaily func(ctx context.Context, dsn string) error) {
	hookMu.Lock()
	migrateTemplate = migrate
	ensureCloneDaily = cloneDaily
	hookMu.Unlock()
}

func migrateHook() (func(ctx context.Context, dsn string) error, bool) {
	hookMu.RLock()
	defer hookMu.RUnlock()
	return migrateTemplate, migrateTemplate != nil
}

func cloneDailyHook() func(ctx context.Context, dsn string) error {
	hookMu.RLock()
	defer hookMu.RUnlock()
	return ensureCloneDaily
}

// testEnv is the resolved, process-wide configuration shared by every helper.
type testEnv struct {
	base      *url.URL // parsed TEST_DATABASE_URL
	maintName string   // maintenance database name
	root      string   // repository root (holds go.mod)
}

// dsn returns the base DSN with its database name replaced by dbname.
func (e *testEnv) dsn(dbname string) string {
	u := *e.base
	u.Path = "/" + dbname
	return u.String()
}

var (
	envOnce   sync.Once
	cachedEnv *testEnv
	cachedErr error
)

func loadEnv() (*testEnv, error) {
	envOnce.Do(func() { cachedEnv, cachedErr = openEnv() })
	return cachedEnv, cachedErr
}

func openEnv() (*testEnv, error) {
	raw := os.Getenv("TEST_DATABASE_URL")
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse TEST_DATABASE_URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("TEST_DATABASE_URL is not a full DSN: %q", raw)
	}
	maint := strings.TrimSpace(os.Getenv("C3API_TEST_MAINT_DB"))
	if maint == "" {
		maint = defaultMaintDB
	}
	root, err := findRepoRoot()
	if err != nil {
		return nil, err
	}
	return &testEnv{base: u, maintName: maint, root: root}, nil
}

// envOrSkip resolves the test environment, skipping the test when
// TEST_DATABASE_URL is unset (integration tests only run against a real server).
func envOrSkip(t testing.TB) *testEnv {
	t.Helper()
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip(skipMessage)
	}
	e, err := loadEnv()
	if err != nil {
		t.Fatalf("pgtest: %v", err)
	}
	return e
}

// maint is the single connection that runs template bookkeeping and clone DDL.
// It is pinned to one session so session-scoped advisory locks are held and
// released on the same backend.
type maint struct {
	sqlDB *sql.DB
	conn  *sql.Conn
}

var (
	maintOnce sync.Once
	maintVal  *maint
	maintErr  error
)

// state returns the process-wide maintenance connection and environment,
// creating them on first use.
func state(t testing.TB) (*maint, *testEnv) {
	t.Helper()
	e := envOrSkip(t)
	maintOnce.Do(func() { maintVal, maintErr = openMaintenance(e) })
	if maintErr != nil {
		t.Fatalf("pgtest: %v", maintErr)
	}
	return maintVal, e
}

func openMaintenance(e *testEnv) (*maint, error) {
	sqlDB, err := sql.Open("pgx", e.dsn(e.maintName))
	if err != nil {
		return nil, fmt.Errorf("open maintenance database %q: %w", e.maintName, err)
	}
	// One session only: advisory locks are per-session, so lock and unlock must
	// land on the same backend.
	sqlDB.SetMaxOpenConns(1)
	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("connect to maintenance database %q: %w", e.maintName, err)
	}
	if err := ensureCanCreateDatabase(ctx, conn); err != nil {
		_ = conn.Close()
		_ = sqlDB.Close()
		return nil, err
	}
	return &maint{sqlDB: sqlDB, conn: conn}, nil
}

// Close releases the maintenance connection and its pool.
func (m *maint) Close() error {
	var firstErr error
	if m.conn != nil {
		if err := m.conn.Close(); err != nil {
			firstErr = err
		}
	}
	if m.sqlDB != nil {
		if err := m.sqlDB.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func ensureCanCreateDatabase(ctx context.Context, conn *sql.Conn) error {
	var ok bool
	err := conn.QueryRowContext(ctx,
		`SELECT rolcreatedb OR rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&ok)
	if err != nil {
		return fmt.Errorf("check CREATEDB privilege: %w", err)
	}
	if !ok {
		return errors.New("pgtest: the role in TEST_DATABASE_URL needs CREATEDB (or superuser) to " +
			"create the template and per-test clone databases; grant it with: ALTER ROLE <role> CREATEDB")
	}
	return nil
}

// Clone creates an isolated database cloned from the migrated template and
// returns its DSN. It is idempotent per test: repeated calls within the same
// test return the same clone, so a test that needs several handles (a pooled
// repository plus a pool-less one, or a second pool for a different session
// TimeZone) shares one database. The clone is dropped when the test finishes
// (kept, with its DSN logged, when the test failed).
func Clone(t testing.TB) string {
	t.Helper()
	return cloneFor(t, false)
}

// CloneEmpty creates an isolated, empty database (a plain CREATE DATABASE, no
// template) and returns its DSN. It exists for the migration, partition and
// EXPLAIN tests that must start from an empty database and apply their own
// migration. The database is dropped at cleanup like Clone's.
func CloneEmpty(t testing.TB) string {
	t.Helper()
	return cloneFor(t, true)
}

// DSN returns the base PostgreSQL DSN from TEST_DATABASE_URL, skipping the test
// when it is unset. Most tests want Clone or CloneEmpty instead; DSN is for the
// rare caller that needs the raw configured URL.
func DSN(t testing.TB) string {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip(skipMessage)
	}
	return raw
}

// OpenPool opens a pgx connection pool on dsn with MaxConns=dbPoolMax and a
// lock_timeout, registering t.Cleanup(pool.Close).
//
// A caller that additionally builds a *sql.DB on this pool
// (stdlib.OpenDBFromPool) MUST register its db.Close AFTER OpenPool returns:
// cleanup runs LIFO, so the *sql.DB closes first and the pool second. Closing
// the pool while a *sql.DB still borrows from it would block.
func OpenPool(t testing.TB, dsn string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pgtest: parse clone DSN %q: %v", dsn, err)
	}
	cfg.MaxConns = dbPoolMax
	// Rolling rotation of connections (mirrors the production pool) so a long
	// test does not pin a backend forever.
	cfg.MaxConnLifetime = 30 * time.Minute
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	if _, ok := cfg.ConnConfig.RuntimeParams["lock_timeout"]; !ok {
		cfg.ConnConfig.RuntimeParams["lock_timeout"] = "5s"
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pgtest: open pool on %q: %v", dsn, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

var (
	cloneByT      sync.Map // test name -> *clone
	cloneEmptyByT sync.Map // test name -> *clone
)

func cloneFor(t testing.TB, empty bool) string {
	t.Helper()
	m := &cloneByT
	if empty {
		m = &cloneEmptyByT
	}
	key := cloneKey(m, t)
	if v, ok := m.Load(key); ok {
		return v.(*clone).dsn
	}
	c := newClone(t, empty)
	c.key = key
	c.registry = m
	m.Store(key, c)
	return c.dsn
}

// cloneKey picks the registry key for t: the longest already-registered ancestor
// of t.Name() (the root test, or a subtest that created the clone first), so
// that test and everything beneath it share one clone; otherwise t's own name,
// giving t an exclusive clone. Sharing is bound to the ancestor that created the
// clone, so it is only ever dropped by the test that owns its lifetime: a clone
// cached under a still-running ancestor cannot be dropped under a sibling's
// feet, and a subtest that creates the first clone gets an exclusive one.
func cloneKey(m *sync.Map, t testing.TB) string {
	name := t.Name()
	for p := name; ; {
		if _, ok := m.Load(p); ok {
			return p
		}
		i := strings.LastIndexByte(p, '/')
		if i < 0 {
			return name
		}
		p = p[:i]
	}
}

type clone struct {
	name     string
	dsn      string
	maint    *maint
	key      string
	registry *sync.Map
}

func newClone(t testing.TB, empty bool) *clone {
	t.Helper()
	var migrate func(ctx context.Context, dsn string) error
	if !empty {
		var ok bool
		migrate, ok = migrateHook()
		if !ok {
			t.Fatalf("pgtest: data-layer hooks are not registered; blank-import " +
				"github.com/is7qin/c3api/internal/testsupport/pgtest/pgrepo in the test package")
		}
	}
	m, e := state(t)
	name := nextCloneName(packageSlug())
	ctx := context.Background()
	// CloneEmpty is deliberately a plain CREATE DATABASE (a copy of template1)
	// rather than clone-then-empty-public: the internal package-repository tests
	// that need an empty database cannot blank-import pgrepo (import cycle), so
	// CloneEmpty must not depend on the migration hook. template1 is vanilla in
	// the test image, so either way the result is an unmigrated database.
	sql := "CREATE DATABASE " + quoteIdent(name)
	if !empty {
		tpl, err := ensureTemplate(t, m, e, migrate)
		if err != nil {
			t.Fatalf("pgtest: %v", err)
		}
		sql += " TEMPLATE " + quoteIdent(tpl)
	}
	if err := createClone(ctx, m, sql); err != nil {
		t.Fatalf("pgtest: create clone %q (is the source free of connections?): %v", name, err)
	}
	c := &clone{name: name, dsn: e.dsn(name), maint: m}
	t.Cleanup(func() { c.remove(t) })
	if !empty {
		if hook := cloneDailyHook(); hook != nil {
			if err := hook(ctx, c.dsn); err != nil {
				t.Fatalf("pgtest: ensure daily partitions on clone %q: %v", name, err)
			}
		}
	}
	return c
}

// createClone runs the clone-creation DDL, retrying briefly while PostgreSQL is
// still tearing down the last backend of a freshly built template ("source
// database ... is being accessed by other users", SQLSTATE 55006): closing the
// template's database/sql handle returns before the server has released the
// session, so the very next CREATE DATABASE ... TEMPLATE can lose that race.
func createClone(ctx context.Context, m *maint, stmt string) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := m.conn.ExecContext(ctx, stmt)
		if err == nil || !strings.Contains(err.Error(), "is being accessed by other users") || time.Now().After(deadline) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// remove is the single cleanup registered per clone: drop it. Keeping the step
// in one cleanup removes any dependence on caller-registered cleanup order.
func (c *clone) remove(t testing.TB) {
	if c.registry != nil {
		c.registry.Delete(c.key)
	}
	if t.Failed() {
		t.Logf("pgtest: keeping clone %q for debugging (test failed); DSN: %s", c.name, c.dsn)
		return
	}
	ctx := context.Background()
	if n, err := c.activeConnections(ctx); err == nil && n > 0 {
		t.Logf("pgtest: clone %q still had %d active connection(s); DROP ... WITH (FORCE) terminated them", c.name, n)
	}
	if _, err := c.maint.conn.ExecContext(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(c.name)+" WITH (FORCE)"); err != nil {
		t.Errorf("pgtest: drop clone %q: %v", c.name, err)
	}
}

func (c *clone) activeConnections(ctx context.Context) (int, error) {
	var n int
	err := c.maint.conn.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_stat_activity WHERE datname = $1`, c.name).Scan(&n)
	return n, err
}

// ensureTemplate returns the migrated template's name, (re)building it when the
// name is absent or its readiness marker is missing or wrong. Reuse therefore
// requires "exists ∧ ready", which supersedes the earlier clone spec's "an
// existing template is reused without reading inside it" invariant (§2.2/§8.2).
// The whole check-and-build runs under the advisory lock so exactly one binary
// builds a given template.
func ensureTemplate(t testing.TB, m *maint, e *testEnv, migrate func(ctx context.Context, dsn string) error) (string, error) {
	t.Helper()
	name, err := templateName(e)
	if err != nil {
		return "", err
	}
	ctx := context.Background()
	if _, err := m.conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", templateLockKey); err != nil {
		return "", fmt.Errorf("acquire template lock: %w", err)
	}
	defer func() {
		if _, err := m.conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", templateLockKey); err != nil {
			t.Errorf("pgtest: release template lock: %v", err)
		}
	}()

	exists, ready, err := templateState(ctx, m.conn, name)
	if err != nil {
		return "", err
	}
	if exists && ready {
		return name, nil
	}
	if exists {
		// The name is present but the build did not finish (or an override was
		// never marked): reuse would hand out an empty/half-built template. Drop
		// it and rebuild from scratch.
		t.Logf("pgtest: rebuilding template %q (present but not marked ready)", name)
	}
	if _, err := m.conn.ExecContext(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(name)+" WITH (FORCE)"); err != nil {
		return "", fmt.Errorf("drop stale template %q: %w", name, err)
	}
	if _, err := m.conn.ExecContext(ctx, "CREATE DATABASE "+quoteIdent(name)); err != nil {
		_, _ = m.conn.ExecContext(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(name)+" WITH (FORCE)")
		return "", fmt.Errorf("create template %q: %w", name, err)
	}
	// migrate owns and closes its own connection to the template, so the
	// template is connection-free again once it returns.
	if err := migrate(ctx, e.dsn(name)); err != nil {
		// Do not leave a half-migrated template behind: a later run would see
		// the name and reuse it.
		_, _ = m.conn.ExecContext(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(name)+" WITH (FORCE)")
		return "", fmt.Errorf("migrate template %q: %w", name, err)
	}
	// Write the readiness marker last, through the maintenance connection (never
	// a template connection), so "marker present ∧ value == marker" proves the
	// migration — schema, partitions and seed — completed. CREATE DATABASE ...
	// TEMPLATE does not copy a database COMMENT: a clone is a fresh oid with none.
	if _, err := m.conn.ExecContext(ctx,
		"COMMENT ON DATABASE "+quoteIdent(name)+" IS "+quoteLiteral(templateReadyMarker)); err != nil {
		_, _ = m.conn.ExecContext(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(name)+" WITH (FORCE)")
		return "", fmt.Errorf("mark template %q ready: %w", name, err)
	}
	return name, nil
}

// templateState reports whether the database name exists and, when it does,
// whether it carries the ready marker. The marker is a database COMMENT read
// through the shared pg_shdescription catalog via shobj_description, so no
// connection is ever opened against the template. An unmarked database scans as
// NULL (sql.NullString.Valid == false).
func templateState(ctx context.Context, conn *sql.Conn, name string) (exists, ready bool, err error) {
	var comment sql.NullString
	err = conn.QueryRowContext(ctx,
		`SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = $1`, name).Scan(&comment)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("check database %q: %w", name, err)
	}
	return true, comment.Valid && comment.String == templateReadyMarker, nil
}

// TemplateName returns the name of the template the current sources build. The
// name embeds the content hash of the schema/migration sources, so checkouts
// sharing the same sources agree on it.
func TemplateName() (string, error) {
	e, err := loadEnv()
	if err != nil {
		return "", err
	}
	return templateName(e)
}

func templateName(e *testEnv) (string, error) {
	if override := strings.TrimSpace(os.Getenv("C3API_TEST_TEMPLATE")); override != "" {
		return truncateIdent(override), nil
	}
	hash, err := templateHash(e)
	if err != nil {
		return "", err
	}
	return templatePrefix + hash, nil
}

// templateHash hashes the sources that determine the template's schema and
// seed: every ent schema file, the generated migrate schema, and the repository
// files that carry partition DDL, the price/codex seeds and the migration hook.
func templateHash(e *testEnv) (string, error) {
	schemaDir := filepath.Join(e.root, "internal", "ent", "schema")
	entries, err := os.ReadDir(schemaDir)
	if err != nil {
		return "", fmt.Errorf("read ent schema dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	paths := make([]string, 0, len(names)+4)
	for _, n := range names {
		paths = append(paths, filepath.Join(schemaDir, n))
	}
	paths = append(paths,
		filepath.Join(e.root, "internal", "ent", "migrate", "schema.go"),
		filepath.Join(e.root, "internal", "repository", "partition.go"),
		filepath.Join(e.root, "internal", "repository", "price_bootstrap.go"),
		filepath.Join(e.root, "internal", "repository", "repository.go"),
	)
	h := sha256.New()
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", p, err)
		}
		_, _ = h.Write(data)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:hashLen], nil
}

// SweepStaleTemplates drops every template database whose content hash differs
// from the current sources'. Only c3api_test_template_* databases are
// considered; per-test clones are never touched, so a concurrent run's clones
// are safe. It is the cross-run cleanup behind scripts/test.sh.
func SweepStaleTemplates(ctx context.Context) ([]string, error) {
	m, err := openMaintenanceOrErr()
	if err != nil {
		return nil, err
	}
	defer func() { _ = m.Close() }()
	// Serialise against concurrent template builds: the advisory lock makes
	// build and sweep mutually exclusive, so a sweep never races a rebuild of the
	// same template. It does NOT cover clone creation — CREATE DATABASE ...
	// TEMPLATE (createClone) runs outside the lock — so a checkout with a
	// different source hash can still sweep a template this run is cloning from.
	// That cross-hash edge is accepted: holding the lock across every clone would
	// serialise the whole suite.
	if _, err := m.conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", templateLockKey); err != nil {
		return nil, fmt.Errorf("acquire template lock: %w", err)
	}
	defer func() { _, _ = m.conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", templateLockKey) }()
	current, err := TemplateName()
	if err != nil {
		return nil, err
	}
	rows, err := m.conn.QueryContext(ctx,
		"SELECT datname FROM pg_database WHERE datname LIKE $1 ORDER BY datname", templatePrefix+"%")
	if err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			_ = rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var dropped []string
	for _, n := range names {
		if n == current {
			continue
		}
		if _, err := m.conn.ExecContext(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(n)+" WITH (FORCE)"); err != nil {
			return dropped, fmt.Errorf("drop stale template %q: %w", n, err)
		}
		dropped = append(dropped, n)
	}
	return dropped, nil
}

func openMaintenanceOrErr() (*maint, error) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		return nil, errors.New("pgtest: TEST_DATABASE_URL is not set")
	}
	e, err := loadEnv()
	if err != nil {
		return nil, err
	}
	return openMaintenance(e)
}

// Precheck verifies the configured role may create databases, failing fast
// before any clone is attempted. It is the pre-flight behind scripts/test.sh.
func Precheck() error {
	m, err := openMaintenanceOrErr()
	if err != nil {
		return err
	}
	return m.Close()
}

var (
	slugOnce sync.Once
	slugVal  string
)

// packageSlug is the directory path of the running test binary relative to the
// repository root, sanitised into a single identifier fragment (e.g.
// "internal_repository"). It scopes clone names to the package that made them.
func packageSlug() string {
	slugOnce.Do(func() {
		wd, err := os.Getwd()
		if err != nil {
			slugVal = "pkg"
			return
		}
		rel := wd
		if e, err := loadEnv(); err == nil {
			if r, rerr := filepath.Rel(e.root, wd); rerr == nil {
				rel = r
			}
		}
		slugVal = sanitize(rel)
	})
	return slugVal
}

var cloneSeq atomic.Uint64

// nextCloneName embeds the process id so two test binaries in the same package
// directory (or a rerun after a crashed run that leaked clones) never collide:
// the sequence counter alone restarts at 1 in every process.
func nextCloneName(slug string) string {
	return truncateIdent(fmt.Sprintf("%s%s_%d_%d", clonePrefix, slug, os.Getpid(), cloneSeq.Add(1)))
}

func sanitize(s string) string {
	s = filepath.ToSlash(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func truncateIdent(name string) string {
	if len(name) <= identMaxLen {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := "_" + hex.EncodeToString(sum[:])[:8]
	return name[:identMaxLen-len(suffix)] + suffix
}

func quoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

// quoteLiteral quotes s as a PostgreSQL string literal (single-quote doubling).
// COMMENT ... IS takes a literal, not an expression, so a bound parameter will
// not do; the marker is a fixed constant, but quoting keeps it safe regardless.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}
	for {
		if isFile(filepath.Join(dir, "go.mod")) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("repository root (go.mod) not found above %s", dir)
		}
		dir = parent
	}
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
