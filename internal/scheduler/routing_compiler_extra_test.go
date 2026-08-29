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
	health := map[HealthKey]HealthState{compilerHealthKeyFor(accs2[1], domain.FormatOpenAIChat, "m"): StateOPEN}
	view, err = c.Compile(CompilerInputs{Static: s2.View().StaticView(), Quality: q, Prices: prices, Health: health, Latched: map[LatchKey]bool{}})
	require.NoError(t, err)
	rd, ok := view.routes[RouteRefFor(10, string(domain.FormatOpenAIChat), "m")]
	require.True(t, ok)
	require.Empty(t, rd.Primary)
	require.Empty(t, rd.Degraded)
	require.Empty(t, rd.Explore.IDs)
}

func TestRoutingCompilerHealthLatchExclusion(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	price := domain.ResolvedPrices{InputPerM: pricePtr(1000)}
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000), accWithEnabled(3, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	qraw := map[int64]CandidateQualityInput{1: qualityInput(30, 29, 100, 100), 2: qualityInput(30, 29, 100, 100), 3: qualityInput(30, 29, 100, 100)}
	q := buildQuality(10, domain.FormatOpenAIChat, "m", qraw, accs)
	prices := map[string]domain.ResolvedPrices{"m": price}
	c := NewRoutingCompiler()
	health := map[HealthKey]HealthState{compilerHealthKeyFor(accs[1], domain.FormatOpenAIChat, "m"): StateOPEN}
	latched := map[LatchKey]bool{compilerLatchKeyFor(accs[2]): true}
	view, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices, Health: health, Latched: latched})
	require.NoError(t, err)
	rd, ok := view.routes[RouteRefFor(10, string(domain.FormatOpenAIChat), "m")]
	require.True(t, ok)
	all := append(append([]int64{}, rd.Primary...), rd.Explore.IDs...)
	all = append(all, rd.Degraded...)
	for _, id := range all {
		require.NotEqual(t, int64(2), id)
		require.NotEqual(t, int64(3), id)
	}
	require.Contains(t, all, int64(1))
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
	baseGen := s.View().Generation()
	s.publisher.publishWithBase(baseGen, func(cur *RoutingView) *DecisionView {
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
	v2 := s.View()
	require.Same(t, v1.DecisionView(), v2.DecisionView())
	require.Contains(t, v2.ByID(), int64(2))
	staleGen := baseGen
	accs2 := []*domain.Account{accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000)}
	q2 := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{1: qualityInput(30, 29, 100, 100), 2: qualityInput(30, 29, 100, 100)}, accs2)
	dv2, err := c.Compile(CompilerInputs{Static: v2.StaticView(), Quality: q2, Prices: prices})
	require.NoError(t, err)
	s.publisher.publishWithBase(staleGen, func(cur *RoutingView) *DecisionView {
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
