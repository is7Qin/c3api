// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestRoutingCompilerEmptyAndAllIneligible(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	c := NewRoutingCompiler()
	m := newMemLoader(map[int64][]*domain.Account{10: {}})
	s := newSched(t, m)
	view, err := c.Compile(CompilerInputs{Static: s.View().StaticView()})
	require.NoError(t, err)
	require.Empty(t, view.routes)
	m2 := newMemLoader(map[int64][]*domain.Account{10: {accWithEnabled(1, tpl, false, 10000), accWithEnabled(2, tpl, true, 10000)}})
	s2 := newSched(t, m2)
	accs2 := m2.byGroup[10]
	qraw := map[int64]CandidateQualityInput{1: qualityInput(30, 29, 100, 100), 2: qualityInput(30, 29, 100, 100)}
	q := buildQuality(10, domain.FormatOpenAIChat, "m", qraw, accs2)
	prices := map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}}
	// v5-§5.1A: OPEN health no longer excludes account 2 — only the statically
	// disabled account 1 stays out. Serving gates live in reserveOnView.
	hk := compilerHealthKeyFor(accs2[1], domain.FormatOpenAIChat, "m")
	h := NewRuntimeHealth(nil, "self", nil, nil, nil)
	h.view.Store(&healthView{entries: map[HealthKey]healthEntry{hk: {Key: hk, State: StateOPEN}}})
	s2.SetRuntimeHealth(h)
	view, err = c.Compile(CompilerInputs{Static: s2.View().StaticView(), Quality: q, Prices: prices})
	require.NoError(t, err)
	rd, ok := view.routes[RouteRefFor(10, string(domain.FormatOpenAIChat), "m")]
	require.True(t, ok)
	all := append(append([]int64{}, compiledAccountIDs(rd.Primary)...), compiledAccountIDs(rd.Explore.Ordered)...)
	all = append(all, compiledAccountIDs(rd.Degraded)...)
	require.NotContains(t, all, int64(1), "statically disabled stays out")
	require.Contains(t, all, int64(2), "OPEN health must not exclude post-v5")
}

func TestRoutingCompilerHealthLatchExclusion(t *testing.T) {
	// v5-§5.1A (COMPILED-HEALTH-FREE): live OPEN health + live latch must NOT
	// exclude from compilation — all three statically eligible accounts stay
	// in the plan; reserveOnView owns serving exclusion (suite-pinned
	// unmodified by TestSchedulerReserveAttemptUsesDynamicCandidateGates).
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	price := domain.ResolvedPrices{InputPerM: pricePtr(1000)}
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000), accWithEnabled(3, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	qraw := map[int64]CandidateQualityInput{1: qualityInput(30, 29, 100, 100), 2: qualityInput(30, 29, 100, 100), 3: qualityInput(30, 29, 100, 100)}
	q := buildQuality(10, domain.FormatOpenAIChat, "m", qraw, accs)
	prices := map[string]domain.ResolvedPrices{"m": price}
	c := NewRoutingCompiler()
	hk := compilerHealthKeyFor(accs[1], domain.FormatOpenAIChat, "m")
	h := NewRuntimeHealth(nil, "self", nil, nil, nil)
	h.view.Store(&healthView{entries: map[HealthKey]healthEntry{hk: {Key: hk, State: StateOPEN}}})
	s.SetRuntimeHealth(h)
	lk := compilerLatchKeyFor(accs[2])
	require.True(t, s.TryLatch(accs[2].ID, lk.Fingerprint, lk.Revision))
	view, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices})
	require.NoError(t, err)
	rd, ok := view.routes[RouteRefFor(10, string(domain.FormatOpenAIChat), "m")]
	require.True(t, ok)
	all := append(append([]int64{}, compiledAccountIDs(rd.Primary)...), compiledAccountIDs(rd.Explore.Ordered)...)
	all = append(all, compiledAccountIDs(rd.Degraded)...)
	require.ElementsMatch(t, []int64{1, 2, 3}, all)
}

func TestRoutingViewRebaseWithCompiler(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	price := domain.ResolvedPrices{InputPerM: pricePtr(1000)}
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	c := NewRoutingCompiler()
	q := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{1: qualityInput(30, 29, 100, 100)}, accs)
	prices := map[string]domain.ResolvedPrices{"m": price}
	dv, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices})
	require.NoError(t, err)
	base := s.View()
	s.publisher.publishWithBase(base.Generation(), base.StaticView(), func(cur *RoutingView) *DecisionView {
		require.NotNil(t, cur.StaticView())
		dv.generation = cur.Generation() + 1
		return dv
	})
	v1 := s.View()
	require.NotNil(t, v1.DecisionView())
	require.NotNil(t, v1.DecisionView().Routes())
	m.mu.Lock()
	m.byGroup[10] = append(m.byGroup[10], accWithEnabled(2, tpl, true, 10000))
	m.mu.Unlock()
	require.NoError(t, s.reload(nilContext()))
	// Atomic publication: the staged static pairs on the next compile.
	s.compileOnce()
	v2 := s.View()
	require.NotSame(t, v1, v2, "paired publish replaces the view")
	require.NotSame(t, v1.DecisionView(), v2.DecisionView(), "new static pairs with a fresh decision")
	require.Contains(t, v2.ByID(), int64(2))
	accs2 := []*domain.Account{accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000)}
	q2 := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{1: qualityInput(30, 29, 100, 100), 2: qualityInput(30, 29, 100, 100)}, accs2)
	dv2, err := c.Compile(CompilerInputs{Static: v2.StaticView(), Quality: q2, Prices: prices})
	require.NoError(t, err)
	s.publisher.publishWithBase(v2.Generation(), v2.StaticView(), func(cur *RoutingView) *DecisionView {
		require.Same(t, v2.StaticView(), cur.StaticView())
		dv2.generation = cur.Generation() + 1
		return dv2
	})
	v3 := s.View()
	require.Same(t, v2.StaticView(), v3.StaticView())
	require.NotSame(t, v1.DecisionView(), v3.DecisionView())
	_, ok := v3.DecisionView().Routes()[RouteRefFor(10, string(domain.FormatOpenAIChat), "m")]
	require.True(t, ok)
}
