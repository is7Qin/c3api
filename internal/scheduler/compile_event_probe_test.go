// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/rule"
)

// compileProbeTestSchema is this test's dedicated namespace inside the shared
// TEST_DATABASE_URL database (per-package real-PG convention: DROP + CREATE
// up front, serial suite so no cross-test interference).
const compileProbeTestSchema = "compile_probe_test"

// TestCompileEvent_ProductionProbeWired pins the v5-F1 production wiring: the
// backstop tick consumes the repository-owned O(1) tuple (§9-A1) against real
// PostgreSQL and, on a quiet fleet, does ZERO reload/compile/serialization
// work (loader touches, compiler calls, generation, published bytes, fallback
// count — counters/bytes, not vibes). It also pins the seam default
// explicitly (New leaves the probe unwired; nil supplier is a no-op), proves
// a direct-DB content edit WITHOUT a revision bump moves the extended tuple
// (the pre-extension counts+rev tuple would have missed it) and fires the
// backstop exactly once, and proves the nil-probe fail-safe branch unreachable
// from production by asserting the single authorized call-site exists in
// cmd/server/main.go.
func TestCompileEvent_ProductionProbeWired(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + compileProbeTestSchema
	} else {
		dsn += "?search_path=" + compileProbeTestSchema
	}
	ctx := context.Background()
	pool, err := repository.OpenPG(ctx, dsn, 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+compileProbeTestSchema+` CASCADE; CREATE SCHEMA `+compileProbeTestSchema+`;`)
	require.NoError(t, err)
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), true)
	require.NoError(t, err)

	// --- seed real groups/accounts (the probe's aggregate source) ---
	tpl, err := repos.CreateTemplate(ctx, &domain.Template{
		Name: "t-probe", BaseURL: "http://upstream.example.com", CredentialType: credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		Models:           []string{"m"},
		ModelMapping:     domain.ModelMapping{},
	})
	require.NoError(t, err)
	g, err := repos.CreateGroup(ctx, &domain.Group{
		Name: "g-probe", Visibility: domain.GroupVisibilityPublic, PriceMultiplier: 10000,
	})
	require.NoError(t, err)
	var accIDs []int64
	for _, name := range []string{"acc-probe-1", "acc-probe-2"} {
		acc, err := repos.CreateAccount(ctx, &domain.Account{
			Name: name, TemplateID: tpl.ID, UpstreamKey: "sk-upstream",
			MaxConcurrency: 4,
		})
		require.NoError(t, err)
		accIDs = append(accIDs, acc.ID)
		require.NoError(t, repos.SetAccountGroups(ctx, acc.ID, []int64{g.ID}))
	}

	// --- seam default, explicitly: New leaves the probe unwired, nil is a no-op ---
	newRuleEngine := func(t *testing.T) *rule.RuleEngine {
		t.Helper()
		re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
		require.NoError(t, re.Reload(ctx))
		return re
	}
	unwired := New(testCfg(), newMemLoader(nil), newRuleEngine(t), nil, nil)
	require.Nil(t, unwired.stalenessProbe, "seam default must be unwired (nil-probe fail-safe)")
	nilCfg := testCfg()
	nilCfg.StalenessProbe = nil
	stillUnwired := New(nilCfg, newMemLoader(nil), newRuleEngine(t), nil, nil)
	require.Nil(t, stillUnwired.stalenessProbe, "explicit nil supplier must not wire the probe")

	// --- production-equivalent wiring: counting loader + real repository tuple ---
	memTpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	memAccs := []*domain.Account{accWithEnabled(1, memTpl, true, 10000), accWithEnabled(2, memTpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: memAccs})
	cl := &countingLoader{inner: m}
	wiredCfg := testCfg()
	wiredCfg.StalenessProbe = repos.Groups
	s := New(wiredCfg, cl, newRuleEngine(t), nil, nil)
	require.NotNil(t, s.stalenessProbe, "wired scheduler must carry the probe — the nil-probe branch is unreachable from production wiring")
	require.NoError(t, s.reload(ctx))
	q := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{
		1: qualityInput(30, 29, 100, 100), 2: qualityInput(30, 29, 100, 100),
	}, memAccs)
	prices := map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}}
	wireSources(s, q, prices)
	s.compileOnce()
	cc := &countingCompiler{inner: NewRoutingCompiler(), calls: make(chan struct{}, 64)}
	s.compiler = cc

	gen := s.View().Generation()
	loads := cl.loadsN()
	bytesBefore := decisionViewBytes(s.View().DecisionView())
	fallbacks := s.fallbackCount.Load()

	s.backstopTick(ctx)

	require.Equal(t, loads, cl.loadsN(), "probe hit must not touch the loader")
	require.Empty(t, cc.calls, "probe hit must not compile")
	require.Equal(t, gen, s.View().Generation(), "probe hit must not publish")
	require.Equal(t, bytesBefore, decisionViewBytes(s.View().DecisionView()))
	require.Equal(t, fallbacks, s.fallbackCount.Load(), "probe hit records no fallback")

	// --- out-of-band content edit: a direct-DB key rotation in the general
	// UpdateAccount shape (content WITHOUT a lifecycle_revision bump) must
	// move the extended tuple where the old counts+rev tuple could not ---
	before, err := repos.Groups.CompileStalenessSnapshot(ctx)
	require.NoError(t, err)
	tag, err := pool.Exec(ctx, `UPDATE accounts SET upstream_key='sk-rotated-out-of-band', updated_at=(NOW() + INTERVAL '1 hour') WHERE id=$1`, accIDs[0])
	require.NoError(t, err)
	require.Equal(t, int64(1), tag.RowsAffected())
	after, err := repos.Groups.CompileStalenessSnapshot(ctx)
	require.NoError(t, err)
	// Old-shape projection is identical: the pre-extension tuple would have
	// missed this edit (no COUNT moved, no revision bumped).
	require.Equal(t, before.Accounts, after.Accounts)
	require.Equal(t, before.MaxLifecycleRevision, after.MaxLifecycleRevision)
	require.Equal(t, before.Groups, after.Groups)
	require.Equal(t, before.Templates, after.Templates)
	require.Equal(t, before.Memberships, after.Memberships)
	require.Equal(t, before.Exts, after.Exts)
	// The extended tuple moves on the content edit alone.
	require.Greater(t, after.AccountsUpdatedAtNano, before.AccountsUpdatedAtNano)
	require.NotEqual(t, before, after)

	loads2 := cl.loadsN()
	s.backstopTick(ctx)
	require.Equal(t, loads2+1, cl.loadsN(), "content-edit mismatch must trigger exactly one reload")
	s.backstopTick(ctx)
	require.Equal(t, loads2+1, cl.loadsN(), "baseline converged: the second tick is a hit")

	// --- code-path proof: the single authorized production call-site ---
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	mainSrc, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "cmd", "server", "main.go"))
	require.NoError(t, err)
	require.Contains(t, string(mainSrc), "StalenessProbe:        repos.Groups,",
		"production must wire the repo-backed probe in the scheduler Config literal (§9-A2 sole call-site)")
}
