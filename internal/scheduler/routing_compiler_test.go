// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"math"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func accWithEnabled(id int64, tpl *domain.Template, enabled bool, mult int) *domain.Account {
	a := acc(id, tpl, 10)
	a.Enabled = enabled
	a.UpstreamCostMultiplierBp = mult
	a.LifecycleRevision = 1
	return a
}

func qualityInput(attempts, successes int, inTok, outTok int64) CandidateQualityInput {
	// TTFT logs for success count: provide sumLog/sumsq if successes>=30
	var cnt Counts
	cnt.Attempts = attempts
	cnt.Successes = successes
	if successes >= 30 {
		logged := math.Log(100)
		cnt.TTFTCount = successes
		cnt.SumLog = logged * float64(successes)
		cnt.SumSq = logged * logged * float64(successes)
	}
	return CandidateQualityInput{Counts: cnt, InputTokens: inTok, OutputTokens: outTok}
}

func pricePtr(v int64) *int64 { return &v }

func TestRoutingCompilerDeterministicMapOrder(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	price := domain.ResolvedPrices{InputPerM: pricePtr(1000), OutputPerM: pricePtr(1000)}
	// Build static with two accounts order dependent
	m1 := newMemLoader(map[int64][]*domain.Account{10: {accWithEnabled(2, tpl, true, 10000), accWithEnabled(1, tpl, true, 10000)}})
	s1 := newSched(t, m1)
	m2 := newMemLoader(map[int64][]*domain.Account{10: {accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000)}})
	s2 := newSched(t, m2)
	// also shuffled quality map insertion order via separate maps
	q1 := map[int64]CandidateQualityInput{
		2: qualityInput(30, 29, 1000, 1000),
		1: qualityInput(30, 29, 1000, 1000),
	}
	q2 := map[int64]CandidateQualityInput{
		1: qualityInput(30, 29, 1000, 1000),
		2: qualityInput(30, 29, 1000, 1000),
	}
	prices := map[string]domain.ResolvedPrices{"m": price}
	c := NewRoutingCompiler()
	v1, err := c.Compile(CompilerInputs{Static: s1.View().StaticView(), Quality: q1, Prices: prices})
	require.NoError(t, err)
	v2, err := c.Compile(CompilerInputs{Static: s2.View().StaticView(), Quality: q2, Prices: prices})
	require.NoError(t, err)
	rr := RouteRef{GroupID: 10, Format: string(domain.FormatOpenAIChat), Model: "m"}
	d1, ok1 := v1.routes[rr]
	d2, ok2 := v2.routes[rr]
	require.True(t, ok1)
	require.True(t, ok2)
	require.Equal(t, d1.Primary, d2.Primary)
	require.Equal(t, d1.Explore.IDs, d2.Explore.IDs)
	require.Equal(t, d1.Explore.Cumulative, d2.Explore.Cumulative)
	require.Equal(t, d1.Degraded, d2.Degraded)
}

func TestRoutingCompilerLaneClassificationAndWeightsTail(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	price := domain.ResolvedPrices{InputPerM: pricePtr(1000), OutputPerM: pricePtr(1000)}
	m := newMemLoader(map[int64][]*domain.Account{10: {accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000), accWithEnabled(3, tpl, true, 10000)}})
	s := newSched(t, m)
	// 1: Primary candidate (n>=30 success high), 2: Degraded (n>=30 but low success), 3: Explore (n<30)
	logged := math.Log(100)
	loggedSlow := math.Log(500)
	q := map[int64]CandidateQualityInput{
		1: {Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2900, OutputTokens: 2900},
		2: {Counts: Counts{Attempts: 30, Successes: 15, TTFTCount: 30, SumLog: loggedSlow * 30, SumSq: loggedSlow * loggedSlow * 30}, InputTokens: 1500, OutputTokens: 1500},
		3: {Counts: Counts{Attempts: 10, Successes: 5, TTFTCount: 5}, InputTokens: 500, OutputTokens: 500},
	}
	prices := map[string]domain.ResolvedPrices{"m": price}
	c := NewRoutingCompiler()
	view, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices})
	require.NoError(t, err)
	rd, ok := view.routes[RouteRef{GroupID: 10, Format: string(domain.FormatOpenAIChat), Model: "m"}]
	require.True(t, ok)
	// union exactly once
	all := append(append([]int64{}, rd.Primary...), rd.Explore.IDs...)
	all = append(all, rd.Degraded...)
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	require.Equal(t, []int64{1, 2, 3}, all)
	// classification: 1 primary, 3 explore, 2 degraded (depending on cost/quality). Check counts sum.
	require.Len(t, rd.Primary, 1)
	require.Contains(t, rd.Primary, int64(1))
	require.Len(t, rd.Degraded, 1)
	require.Contains(t, rd.Degraded, int64(2))
	require.Len(t, rd.Explore.IDs, 1)
	require.Contains(t, rd.Explore.IDs, int64(3))
	// weights and fallback completeness
	require.NotEmpty(t, rd.Explore.Cumulative)
	require.Equal(t, len(rd.Explore.IDs), len(rd.Explore.Cumulative))
	require.NotZero(t, rd.Explore.Total)
	require.Equal(t, rd.Explore.IDs, rd.Explore.Fallback) // single element fallback same
	// cumulative monotonic
	for i := 1; i < len(rd.Explore.Cumulative); i++ {
		require.Greater(t, rd.Explore.Cumulative[i], rd.Explore.Cumulative[i-1])
	}
}

func TestRoutingCompilerCostTieBreakDeterministic(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	price := domain.ResolvedPrices{InputPerM: pricePtr(1000), OutputPerM: pricePtr(1000)}
	// Three candidates with same cost but different quality -> tie break by LCB etc then accountID
	m := newMemLoader(map[int64][]*domain.Account{10: {
		accWithEnabled(3, tpl, true, 10000),
		accWithEnabled(2, tpl, true, 10000),
		accWithEnabled(1, tpl, true, 10000),
	}})
	s := newSched(t, m)
	logged := math.Log(100)
	q := map[int64]CandidateQualityInput{
		1: {Counts: Counts{Attempts: 30, Successes: 28, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2800, OutputTokens: 2800},
		2: {Counts: Counts{Attempts: 30, Successes: 28, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2800, OutputTokens: 2800},
		3: {Counts: Counts{Attempts: 30, Successes: 28, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2800, OutputTokens: 2800},
	}
	prices := map[string]domain.ResolvedPrices{"m": price}
	c := NewRoutingCompiler()
	view, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices})
	require.NoError(t, err)
	rd, ok := view.routes[RouteRef{GroupID: 10, Format: string(domain.FormatOpenAIChat), Model: "m"}]
	require.True(t, ok)
	// All same cost and quality, should be sorted by accountID asc due to deterministic tie-break
	require.Equal(t, []int64{1, 2, 3}, rd.Primary)
}

func TestRoutingCompilerMissingPriceUnknown(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000)}})
	s := newSched(t, m)
	q := map[int64]CandidateQualityInput{
		1: {Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 29, SumLog: math.Log(100) * 29, SumSq: math.Log(100) * math.Log(100) * 29}, InputTokens: 2900, OutputTokens: 2900},
		2: {Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 29, SumLog: math.Log(100) * 29, SumSq: math.Log(100) * math.Log(100) * 29}, InputTokens: 2900, OutputTokens: 2900},
	}
	// Only price for other model, missing for "m"
	prices := map[string]domain.ResolvedPrices{"other": {InputPerM: pricePtr(1000)}}
	c := NewRoutingCompiler()
	view, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices})
	require.NoError(t, err)
	rd, ok := view.routes[RouteRef{GroupID: 10, Format: string(domain.FormatOpenAIChat), Model: "m"}]
	require.True(t, ok)
	require.Empty(t, rd.Primary)
	require.Empty(t, rd.Degraded)
	require.Len(t, rd.Explore.IDs, 2)
}

func TestRoutingCompilerInvalidInputs(t *testing.T) {
	// nil static -> empty view, no panic
	c := NewRoutingCompiler()
	view, err := c.Compile(CompilerInputs{Static: nil})
	require.NoError(t, err)
	require.NotNil(t, view)
	require.Empty(t, view.routes)
	// negative multiplier clamped, invalid counts still handled
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {accWithEnabled(1, tpl, true, -5)}})
	s := newSched(t, m)
	q := map[int64]CandidateQualityInput{
		1: {Counts: Counts{Attempts: -1, Successes: -1}},
	}
	prices := map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}}
	view, err = c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices})
	require.NoError(t, err)
	rd, ok := view.routes[RouteRef{GroupID: 10, Format: string(domain.FormatOpenAIChat), Model: "m"}]
	require.True(t, ok)
	// invalid counts -> should be treated as explore (no panic)
	require.NotEmpty(t, rd.Explore.IDs)
}

func TestRoutingCompilerImmutability(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	price := domain.ResolvedPrices{InputPerM: pricePtr(1000)}
	m := newMemLoader(map[int64][]*domain.Account{10: {accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000)}})
	s := newSched(t, m)
	q := map[int64]CandidateQualityInput{
		1: {Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 30, SumLog: math.Log(100) * 30, SumSq: math.Log(100) * math.Log(100) * 30}, InputTokens: 100, OutputTokens: 100},
		2: {Counts: Counts{Attempts: 10, Successes: 5}, InputTokens: 50, OutputTokens: 50},
	}
	prices := map[string]domain.ResolvedPrices{"m": price}
	c := NewRoutingCompiler()
	view, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices})
	require.NoError(t, err)
	rr := RouteRef{GroupID: 10, Format: string(domain.FormatOpenAIChat), Model: "m"}
	orig := append([]int64(nil), view.routes[rr].Primary...)
	// mutate input maps after compile should not affect view
	q[1] = CandidateQualityInput{Counts: Counts{Attempts: 100, Successes: 100}}
	prices["m"] = domain.ResolvedPrices{InputPerM: pricePtr(9999)}
	require.Equal(t, orig, view.routes[rr].Primary)
	// mutate view slices should not affect second compile
	if len(orig) > 0 {
		view.routes[rr].Primary[0] = 999
	}
	view2, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: map[int64]CandidateQualityInput{
		1: {Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 30, SumLog: math.Log(100) * 30, SumSq: math.Log(100) * math.Log(100) * 30}, InputTokens: 100, OutputTokens: 100},
		2: {Counts: Counts{Attempts: 10, Successes: 5}, InputTokens: 50, OutputTokens: 50},
	}, Prices: map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}}})
	require.NoError(t, err)
	require.NotEqual(t, view.routes[rr].Primary, view2.routes[rr].Primary)
}

func TestRoutingCompilerEmptyAndAllIneligible(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	c := NewRoutingCompiler()
	// empty group -> no routes? but static has group with no accounts
	m := newMemLoader(map[int64][]*domain.Account{10: {}})
	s := newSched(t, m)
	view, err := c.Compile(CompilerInputs{Static: s.View().StaticView()})
	require.NoError(t, err)
	require.Empty(t, view.routes)
	// all ineligible via health/latched/disabled
	m2 := newMemLoader(map[int64][]*domain.Account{10: {accWithEnabled(1, tpl, false, 10000), accWithEnabled(2, tpl, true, 10000)}})
	s2 := newSched(t, m2)
	q := map[int64]CandidateQualityInput{
		1: qualityInput(30, 29, 100, 100),
		2: qualityInput(30, 29, 100, 100),
	}
	prices := map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}}
	view, err = c.Compile(CompilerInputs{
		Static:  s2.View().StaticView(),
		Quality: q,
		Prices:  prices,
		Health:  map[int64]HealthState{2: StateOPEN},
		Latched: map[int64]bool{},
	})
	require.NoError(t, err)
	rd, ok := view.routes[RouteRef{GroupID: 10, Format: string(domain.FormatOpenAIChat), Model: "m"}]
	require.True(t, ok)
	require.Empty(t, rd.Primary)
	require.Empty(t, rd.Degraded)
	require.Empty(t, rd.Explore.IDs)
}

func TestRoutingCompilerHealthLatchExclusion(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	price := domain.ResolvedPrices{InputPerM: pricePtr(1000)}
	m := newMemLoader(map[int64][]*domain.Account{10: {
		accWithEnabled(1, tpl, true, 10000),
		accWithEnabled(2, tpl, true, 10000),
		accWithEnabled(3, tpl, true, 10000),
	}})
	s := newSched(t, m)
	q := map[int64]CandidateQualityInput{
		1: qualityInput(30, 29, 100, 100),
		2: qualityInput(30, 29, 100, 100),
		3: qualityInput(30, 29, 100, 100),
	}
	prices := map[string]domain.ResolvedPrices{"m": price}
	c := NewRoutingCompiler()
	view, err := c.Compile(CompilerInputs{
		Static:  s.View().StaticView(),
		Quality: q,
		Prices:  prices,
		Health:  map[int64]HealthState{2: StateOPEN},
		Latched: map[int64]bool{3: true},
	})
	require.NoError(t, err)
	rd, ok := view.routes[RouteRef{GroupID: 10, Format: string(domain.FormatOpenAIChat), Model: "m"}]
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
	m := newMemLoader(map[int64][]*domain.Account{10: {accWithEnabled(1, tpl, true, 10000)}})
	s := newSched(t, m)
	c := NewRoutingCompiler()
	q := map[int64]CandidateQualityInput{1: qualityInput(30, 29, 100, 100)}
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
	// static update retains decision
	m.mu.Lock()
	m.byGroup[10] = append(m.byGroup[10], accWithEnabled(2, tpl, true, 10000))
	m.mu.Unlock()
	require.NoError(t, s.reload(nilContext()))
	v2 := s.View()
	require.Same(t, v1.DecisionView(), v2.DecisionView())
	require.Contains(t, v2.ByID(), int64(2))
	// stale decision rebase retains new static
	staleGen := baseGen
	q2 := map[int64]CandidateQualityInput{
		1: qualityInput(30, 29, 100, 100),
		2: qualityInput(30, 29, 100, 100),
	}
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
	_, ok := v3.DecisionView().Routes()[RouteRef{GroupID: 10, Format: string(domain.FormatOpenAIChat), Model: "m"}]
	require.True(t, ok)
}
