// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
)

// v5 C1–C3 proof suite (compile_event_* prefix): staleness bound, scope
// identity (pointer reuse + bit-for-bit oracle), O(1) probe behavior, fallback
// recording. Falsification suites elsewhere stay UNMODIFIED.

// probeCounts derives the §4 counter tuple from the in-memory loader with the
// same semantics as the A1 probe query (distinct accounts, max revision,
// groups, referenced templates, membership pairs, exts).
func (m *memLoader) probeCounts() compileProbeCounts {
	m.mu.Lock()
	defer m.mu.Unlock()
	var c compileProbeCounts
	seen := make(map[int64]struct{})
	tpls := make(map[int64]struct{})
	for _, accs := range m.byGroup {
		c.groups++
		for _, a := range accs {
			if a == nil {
				continue
			}
			c.memberships++
			if _, ok := seen[a.ID]; ok {
				continue
			}
			seen[a.ID] = struct{}{}
			c.accounts++
			if a.LifecycleRevision > c.maxRev {
				c.maxRev = a.LifecycleRevision
			}
			if a.Template != nil {
				tpls[a.Template.ID] = struct{}{}
			}
			if a.Ext != nil {
				c.exts++
			}
		}
	}
	c.templates = int64(len(tpls))
	return c
}

// newProbedSched builds a scheduler on a counting loader with the staleness
// probe wired to the memLoader counters and the baseline seeded. The real
// compiler stays installed so scoped fires engage (test doubles take the
// full-fidelity fallback by design).
func newProbedSched(t *testing.T, m *memLoader, q map[CandidateQualityKey]CandidateQualityInput, prices map[string]domain.ResolvedPrices) (*Scheduler, *countingLoader) {
	t.Helper()
	cl := &countingLoader{inner: m}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil, nil, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := New(testCfg(), cl, re, nil, nil, nil, nil)
	s.stalenessProbe = func(context.Context) (compileProbeCounts, error) { return m.probeCounts(), nil }
	require.NoError(t, s.reload(context.Background()))
	wireSources(s, q, prices)
	s.compileOnce()
	return s, cl
}

// TestCompileEvent_ProbeHitSkipsAllWork pins the C1 quiet-fire behavior: with
// no change, the backstop probe hits and the tick does zero rebuild, zero
// compile, zero serialization (no loader touch, no compiler call, same
// generation and bytes).
func TestCompileEvent_ProbeHitSkipsAllWork(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	q := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{
		1: qualityInput(30, 29, 100, 100), 2: qualityInput(30, 29, 100, 100),
	}, accs)
	prices := map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}}
	s, cl := newProbedSched(t, m, q, prices)
	cc := &countingCompiler{inner: NewRoutingCompiler(), calls: make(chan struct{}, 64)}
	s.compiler = cc

	gen := s.View().Generation()
	loads := cl.loadsN()
	bytesBefore := decisionViewBytes(s.View().DecisionView())
	fallbacks := s.fallbackCount.Load()

	s.backstopTick(context.Background())

	require.Equal(t, loads, cl.loadsN(), "probe hit must not touch the loader")
	require.Empty(t, cc.calls, "probe hit must not compile")
	require.Equal(t, gen, s.View().Generation(), "probe hit must not publish")
	require.Equal(t, bytesBefore, decisionViewBytes(s.View().DecisionView()))
	require.Equal(t, fallbacks, s.fallbackCount.Load(), "probe hit records no fallback")
}

// TestCompileEvent_StalenessBoundRecoversWithinSLO severs the event path (a
// loader-side change with NO invalidate — the missed-NOTIFY shape) and pins
// that ONE backstop tick reloads, and the lane then publishes, the new view.
// One tick ≤ SyncInterval < 2×SyncInterval SLO (v5-C3; probe/reload TOCTOU is
// closed by the two-period bound, not by coupling).
func TestCompileEvent_StalenessBoundRecoversWithinSLO(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	q := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{1: qualityInput(30, 29, 100, 100)}, accs)
	prices := map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}}
	s, cl := newProbedSched(t, m, q, prices)
	gen := s.View().Generation()
	loads := cl.loadsN()

	// Severed event: direct loader-side arrival, no InvalidateGroup/Account.
	m.mu.Lock()
	m.byGroup[10] = append(m.byGroup[10], accWithEnabled(2, tpl, true, 10000))
	m.mu.Unlock()

	s.backstopTick(context.Background())
	require.Equal(t, loads+1, cl.loadsN(), "probe mismatch must trigger exactly one reload")

	s.compileOnce()
	after := s.View()
	require.Greater(t, after.Generation(), gen, "backstop + lane must publish within the bound")
	require.Contains(t, after.ByID(), int64(2), "missed arrival converges via the backstop")
	rd, ok := after.DecisionView().Routes()[RouteRefFor(10, string(domain.FormatOpenAIChat), "m")]
	require.True(t, ok)
	require.Contains(t, allLaneIDs(rd), int64(2))
}

// TestCompileEvent_ScopeIdentityPointerReuseAndOracle pins G-scope-identity: a
// single-group edit recomputes ONLY that group's routes — every other route
// keeps pointer identity AND identical bytes — while the edited group's bytes
// equal the full-recompile oracle bit-for-bit (exact/estimate boundary:
// every published byte exact).
func TestCompileEvent_ScopeIdentityPointerReuseAndOracle(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	a1 := accWithEnabled(1, tpl, true, 10000)
	a2 := accWithEnabled(2, tpl, true, 10000)
	m := newMemLoader(map[int64][]*domain.Account{10: {a1}, 20: {a2}})
	q1 := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{1: qualityInput(30, 29, 100, 100)}, []*domain.Account{a1})
	for k, v := range buildQuality(20, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{2: qualityInput(30, 29, 100, 100)}, []*domain.Account{a2}) {
		q1[k] = v
	}
	prices := map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}}
	s, _ := newProbedSched(t, m, q1, prices)

	ref20 := RouteRefFor(20, string(domain.FormatOpenAIChat), "m")
	beforeDec := s.View().DecisionView()
	beforeRoute20 := beforeDec.routes[ref20]
	require.NotNil(t, beforeRoute20)

	// Group-10-only edit: append (a1 object kept, so its key/fingerprint and
	// quality entry are bit-identical — the diff names only the newcomer).
	a3 := accWithEnabled(3, tpl, true, 10000)
	q2 := shallowCopyMap(q1)
	for k, v := range buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{3: qualityInput(30, 29, 100, 100)}, []*domain.Account{a1, a3}) {
		if _, ok := q2[k]; !ok {
			q2[k] = v
		}
	}
	wireSources(s, q2, prices)
	m.mu.Lock()
	m.byGroup[10] = append(m.byGroup[10], a3)
	m.mu.Unlock()
	s.InvalidateGroup(10)
	s.compileOnce()

	afterDec := s.View().DecisionView()
	require.Same(t, beforeRoute20, afterDec.routes[ref20], "untouched routes keep pointer identity")
	oracle, err := s.compiler.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q2, Prices: prices})
	require.NoError(t, err)
	require.Equal(t, decisionViewBytes(oracle), decisionViewBytes(afterDec), "scoped output equals the full-recompile oracle bit-for-bit")
	rd10, ok := afterDec.routes[RouteRefFor(10, string(domain.FormatOpenAIChat), "m")]
	require.True(t, ok)
	require.Contains(t, allLaneIDs(rd10), int64(3), "edited group picks up the newcomer")
}

// TestCompileEvent_AccountsScopeResolvesViaMembership pins the accounts
// payload: an account-scoped request resolves through leaf groupIDs to that
// group's routes only (other groups keep pointer identity, bytes match the
// oracle).
func TestCompileEvent_AccountsScopeResolvesViaMembership(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	a1 := accWithEnabled(1, tpl, true, 10000)
	a2 := accWithEnabled(2, tpl, true, 10000)
	m := newMemLoader(map[int64][]*domain.Account{10: {a1}, 20: {a2}})
	q := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{1: qualityInput(30, 29, 100, 100)}, []*domain.Account{a1})
	for k, v := range buildQuality(20, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{2: qualityInput(30, 29, 100, 100)}, []*domain.Account{a2}) {
		q[k] = v
	}
	prices := map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}}
	s, _ := newProbedSched(t, m, q, prices)

	ref10 := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	ref20 := RouteRefFor(20, string(domain.FormatOpenAIChat), "m")
	beforeRoute10 := s.View().DecisionView().routes[ref10]
	beforeRoute20 := s.View().DecisionView().routes[ref20]
	require.NotNil(t, beforeRoute10)
	require.NotNil(t, beforeRoute20)

	// Stage a group-20 change (forces the scoped fire to publish a new pair),
	// but scope the fire ONLY via the accounts payload: discard the group
	// scope InvalidateGroup enqueued, then name account 2. Without working
	// accounts→routes resolution the recomputed set would miss group 20 and
	// the oracle comparison below would diverge.
	a4 := accWithEnabled(4, tpl, true, 10000)
	m.mu.Lock()
	m.byGroup[20] = append(m.byGroup[20], a4)
	m.mu.Unlock()
	s.InvalidateGroup(20)
	s.drainCompileScopes()
	s.enqueueCompileScope(nil, []int64{2}, "test-cause")
	s.compileOnce()

	afterDec := s.View().DecisionView()
	require.Same(t, beforeRoute10, afterDec.routes[ref10], "out-of-scope routes keep pointer identity")
	require.NotSame(t, beforeRoute20, afterDec.routes[ref20], "in-scope routes recompute")
	oracle, err := s.compiler.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices})
	require.NoError(t, err)
	require.Equal(t, decisionViewBytes(oracle), decisionViewBytes(afterDec))
	rd20, ok := afterDec.routes[ref20]
	require.True(t, ok)
	require.Contains(t, allLaneIDs(rd20), int64(4), "scoped group picks up the newcomer")
}

// TestCompileEvent_FallbackRecorded pins the full-fidelity fallback: an
// unscoped wakeup and a scope-channel overflow both take FULL recompile with
// a recorded reason (counter + cause/scope-size/route-count).
func TestCompileEvent_FallbackRecorded(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	q := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{1: qualityInput(30, 29, 100, 100)}, accs)
	prices := map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}}
	s, _ := newProbedSched(t, m, q, prices)
	base := s.fallbackCount.Load()
	gen := s.View().Generation()

	// Unscoped wakeup: no scope, no staging, no drift → FULL + recorded.
	s.compileOnce()
	require.Equal(t, base+1, s.fallbackCount.Load())
	reason := s.lastFallback.Load()
	require.NotNil(t, reason)
	require.Equal(t, fallbackUnscoped, reason.cause)
	require.Zero(t, reason.scopeEntries)
	require.Equal(t, gen, s.View().Generation(), "byte-identical fallback must not publish")

	// Overflow: burst past scopeChCap degrades to FULL + recorded (exact,
	// never a miss).
	for i := 0; i < scopeChCap+1; i++ {
		s.enqueueCompileScope([]int64{999}, nil, scopeCauseGroup)
	}
	s.compileOnce()
	require.Equal(t, base+2, s.fallbackCount.Load())
	reason = s.lastFallback.Load()
	require.NotNil(t, reason)
	require.Equal(t, fallbackScopeOverflow, reason.cause)
	require.NotZero(t, reason.totalRoutes)
}

// fakeStalenessSource feeds a canned repository tuple through the kept
// stalenessQuerier seam (v5-C1: the lane maps the repo-owned tuple 1:1 and
// never queries for it).
type fakeStalenessSource struct {
	snap domain.CompileStaleness
	err  error
}

func (f *fakeStalenessSource) CompileStalenessSnapshot(context.Context) (domain.CompileStaleness, error) {
	return f.snap, f.err
}

// TestCompileEvent_ProbeSnapshotMappingIsExact pins the post-layering seam
// contract: the Config-wired probe supplier maps the repository tuple onto
// the lane tuple field-exact (including the updated_at nanos that close the
// out-of-band content-edit gap) and propagates supplier errors to the
// fail-safe path.
func TestCompileEvent_ProbeSnapshotMappingIsExact(t *testing.T) {
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil, nil, nil)
	require.NoError(t, re.Reload(context.Background()))
	cfg := testCfg()
	cfg.StalenessProbe = &fakeStalenessSource{snap: domain.CompileStaleness{
		Accounts: 3, MaxLifecycleRevision: 9, AccountsUpdatedAtNano: 11,
		Groups: 2, GroupsUpdatedAtNano: 22,
		Templates: 4, TemplatesUpdatedAtNano: 44,
		Memberships: 5, Exts: 6,
	}}
	s := New(cfg, newMemLoader(nil), re, nil, nil, nil, nil)
	require.NotNil(t, s.stalenessProbe)
	c, err := s.stalenessProbe(context.Background())
	require.NoError(t, err)
	require.Equal(t, compileProbeCounts{
		accounts: 3, maxRev: 9, accUpdated: 11,
		groups: 2, grpUpdated: 22,
		templates: 4, tplUpdated: 44,
		memberships: 5, exts: 6,
	}, c)

	cfgErr := testCfg()
	cfgErr.StalenessProbe = &fakeStalenessSource{err: context.DeadlineExceeded}
	sErr := New(cfgErr, newMemLoader(nil), re, nil, nil, nil, nil)
	require.NotNil(t, sErr.stalenessProbe)
	_, err = sErr.stalenessProbe(context.Background())
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
