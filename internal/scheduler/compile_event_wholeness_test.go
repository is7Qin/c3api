// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// v5 scope-loss + probe-freeze fix proof suite (compile_event_* prefix):
// superseded fires return their drained scopes to the lane; scoped-carry
// publishes mark partial; the backstop forces a full recompile while the
// published view is partial. Deterministic: serial lane, no goroutines, no
// sleeps. Falsification suites elsewhere stay UNMODIFIED.

// supersedeCompiler stages a newer static root mid-Compile (deterministic
// supersede: pending moves between fire start and publish) and returns a
// throwaway decision the lane must drop.
type supersedeCompiler struct{ s *Scheduler }

func (c *supersedeCompiler) Compile(CompilerInputs) (*DecisionView, error) {
	cur := c.s.View()
	c.s.publisher.mu.Lock()
	c.s.publisher.pending = newStaticView(cur.Groups(), cur.ByID())
	c.s.publisher.mu.Unlock()
	return &DecisionView{routes: map[RouteRef]*RouteDecision{}}, nil
}

// TestCompileEvent_SupersedeDropRequeuesScopes pins fix (a): a fire dropped
// by a newer staging returns its drained scopes to scopeCh (overflow flag
// preserved) and re-arms — the dropped work is never silently lost, and the
// re-armed fire covers it.
func TestCompileEvent_SupersedeDropRequeuesScopes(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	a1 := accWithEnabled(1, tpl, true, 10000)
	m := newMemLoader(map[int64][]*domain.Account{10: {a1}})
	q := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{1: qualityInput(30, 29, 100, 100)}, []*domain.Account{a1})
	prices := map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}}
	s, _ := newProbedSched(t, m, q, prices)
	require.True(t, s.publishedViewWhole(), "converged full compile publishes a whole view")

	// Drain the coalesced compile signal so the re-arm below is observable.
	select {
	case <-s.compileCh:
	default:
	}

	// Arm the drop: one group scope plus a preserved overflow flag.
	s.enqueueCompileScope([]int64{10}, nil, scopeCauseGroup)
	s.scopeOverflow.Store(true)
	gen := s.View().Generation()
	preFallbacks := s.fallbackCount.Load()

	s.compiler = &supersedeCompiler{s: s}
	s.compileOnce()

	require.Equal(t, gen, s.View().Generation(), "superseded result must not publish")
	require.Equal(t, preFallbacks+1, s.fallbackCount.Load(), "dropped fire still records its full-fidelity fallback")
	reason := s.lastFallback.Load()
	require.NotNil(t, reason)
	require.Equal(t, fallbackScopeOverflow, reason.cause)

	// The re-arm fired (serial lane, no loop running — the signal sits).
	select {
	case <-s.compileCh:
	default:
		require.Fail(t, "supersede drop must re-arm the lane")
	}

	// The dropped fire's scope is back on the lane, not lost.
	scopes, overflow := s.drainCompileScopes()
	require.True(t, overflow, "preserved overflow flag must survive the drop")
	require.Len(t, scopes, 1, "dropped fire's scope must return to scopeCh")
	require.Equal(t, []int64{10}, scopes[0].groups)
	require.Equal(t, scopeCauseGroup, scopes[0].cause)

	// The re-armed fire covers the returned work and heals to whole.
	for _, sc := range scopes {
		s.enqueueCompileScope(sc.groups, sc.accounts, sc.cause)
	}
	if overflow {
		s.scopeOverflow.Store(true)
	}
	s.compiler = NewRoutingCompiler()
	s.compileOnce()
	require.Zero(t, len(s.scopeCh), "re-armed fire must consume the returned scope")
	require.False(t, s.scopeOverflow.Load(), "re-armed fire must consume the overflow flag")
	require.Greater(t, s.View().Generation(), gen, "re-armed fire must publish")
	require.True(t, s.publishedViewWhole(), "full-fidelity re-arm heals the view to whole")
}

// TestCompileEvent_WholenessPartialHealsViaBackstop pins fix (b)+(c): a
// scoped-carry publish over a holed carry marks partial; the backstop with a
// quiet DB forces the full path anyway (NOT skipped) and heals the view to
// whole; whole views still skip.
func TestCompileEvent_WholenessPartialHealsViaBackstop(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	a1 := accWithEnabled(1, tpl, true, 10000)
	a2 := accWithEnabled(2, tpl, true, 10000)
	m := newMemLoader(map[int64][]*domain.Account{10: {a1}, 20: {a2}})
	q := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{1: qualityInput(30, 29, 100, 100)}, []*domain.Account{a1})
	for k, v := range buildQuality(20, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{2: qualityInput(30, 29, 100, 100)}, []*domain.Account{a2}) {
		q[k] = v
	}
	prices := map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}}
	s, cl := newProbedSched(t, m, q, prices)
	require.True(t, s.publishedViewWhole(), "full fallback publishes a whole view")

	ref10 := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")

	// Plant a hole in the carry: drop route 10 from the published decision.
	s.PublishDecisionForTest(ref10, nil)
	holed := s.View().DecisionView()
	_, ok := holed.routes[ref10]
	require.False(t, ok, "hole planted: route missing from carry")

	// Stage a group-20-only edit; the scoped fire carries the hole forward.
	a4 := accWithEnabled(4, tpl, true, 10000)
	for k, v := range buildQuality(20, domain.FormatOpenAIChat, "m",
		map[int64]CandidateQualityInput{
			2: qualityInput(30, 29, 100, 100),
			4: qualityInput(30, 29, 100, 100),
		}, []*domain.Account{a2, a4}) {
		if _, ok := q[k]; !ok {
			q[k] = v
		}
	}
	wireSources(s, q, prices)
	m.mu.Lock()
	m.byGroup[20] = append(m.byGroup[20], a4)
	m.mu.Unlock()
	s.InvalidateGroup(20)
	s.compileOnce()

	partial := s.View().DecisionView()
	_, ok = partial.routes[ref10]
	require.False(t, ok, "scoped fire carries the hole forward")
	require.False(t, s.publishedViewWhole(), "scoped-carry publish over a holed carry marks partial")
	rd20, ok := partial.routes[RouteRefFor(20, string(domain.FormatOpenAIChat), "m")]
	require.True(t, ok)
	require.Contains(t, allLaneIDs(rd20), int64(4), "in-scope group picks up the newcomer")

	// Backstop with a quiet DB: probe hits, but the partial view forces the
	// full path anyway (NOT skipped) — then the lane heals whole.
	loads := cl.loadsN()
	s.backstopTick(context.Background())
	require.Equal(t, loads+1, cl.loadsN(), "partial view must force the full path despite probe-quiet")
	s.compileOnce()
	require.True(t, s.publishedViewWhole(), "backstop-forced full recompile heals the view to whole")
	healed, ok := s.View().DecisionView().routes[ref10]
	require.True(t, ok, "full recompile closes the hole")
	require.NotEmpty(t, allLaneIDs(healed))

	// Whole views still skip: the next quiet tick does zero work.
	loads = cl.loadsN()
	gen := s.View().Generation()
	s.backstopTick(context.Background())
	require.Equal(t, loads, cl.loadsN(), "whole views still skip on probe hit")
	require.Equal(t, gen, s.View().Generation())
}
