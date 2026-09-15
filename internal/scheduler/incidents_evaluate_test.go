// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// Phase 4 RED: incident evaluation (pure vote + lane cycle-gating), decision
// bytes, validation, scoped-vs-full equality, prune, stats.

var incidentTestRC domain.RouteClassIDVal = domain.RouteClassIDVal{0xAA}

func incidentLogged() float64 { return math.Log(100) }

// incidentCounts builds incident-comparable counts (attempts ≥ 30 suffice;
// TTFT is deliberately NOT part of comparability — non-stream outages count).
func incidentCounts(attempts, successes int) Counts {
	return Counts{
		Attempts:  attempts,
		Successes: successes,
		TTFTCount: attempts,
		SumLog:    float64(attempts) * incidentLogged(),
		SumSq:     incidentLogged() * incidentLogged() * float64(attempts),
	}
}

func incidentFacts(ids []int64, baseURL string) []compilerCandidateFacts {
	out := make([]compilerCandidateFacts, 0, len(ids))
	for _, id := range ids {
		var fp domain.CandidateFingerprintVal
		fp[0] = byte(id)
		fp[1] = 0xF1
		out = append(out, compilerCandidateFacts{
			compilerAccountFacts: compilerAccountFacts{
				accountID:           id,
				baseURL:             baseURL,
				identityFingerprint: fp,
			},
		})
	}
	return out
}

func incidentMaps(facts []compilerCandidateFacts, cur, base Counts) (map[CandidateQualityKey]CandidateQualityInput, map[CandidateQualityKey]Counts) {
	q := make(map[CandidateQualityKey]CandidateQualityInput, len(facts))
	b := make(map[CandidateQualityKey]Counts, len(facts))
	for _, f := range facts {
		k := CandidateQualityKey{RouteClassID: incidentTestRC, Fingerprint: f.identityFingerprint}
		q[k] = CandidateQualityInput{Counts: cur}
		b[k] = base
	}
	return q, b
}

func TestRouteIncidentEvaluate(t *testing.T) {
	good := incidentCounts(60, 55)
	bad := incidentCounts(40, 8)
	okCur := incidentCounts(40, 35)

	t.Run("degraded domain kind", func(t *testing.T) {
		facts := incidentFacts([]int64{1, 2, 3}, "https://a.example.com/v1")
		q, b := incidentMaps(facts, bad, good)
		vote := evaluateRouteIncident(facts, incidentTestRC, q, b)
		require.True(t, vote.degraded)
		require.Equal(t, IncidentKindDomain, vote.kind)
		require.Equal(t, 3, vote.comparable)
		require.Equal(t, 3, vote.degradedCount)
		require.Equal(t, 1, vote.domains)
		require.Len(t, vote.cands, 3)
	})

	t.Run("healthy overlap", func(t *testing.T) {
		facts := incidentFacts([]int64{1, 2, 3}, "https://a.example.com/v1")
		q, b := incidentMaps(facts, okCur, good)
		vote := evaluateRouteIncident(facts, incidentTestRC, q, b)
		require.False(t, vote.degraded)
		require.Equal(t, "", vote.kind)
		require.Equal(t, 3, vote.comparable)
	})

	t.Run("touching intervals are healthy", func(t *testing.T) {
		require.False(t, IsDegraded(Interval{LCB: 0.7, UCB: 0.8}, Interval{LCB: 0.8, UCB: 0.9}, 30, 30),
			"UCB == LCB touches but does not cross (strict <)")
	})

	t.Run("current n=29 abstains", func(t *testing.T) {
		facts := incidentFacts([]int64{1, 2}, "https://a.example.com/v1")
		q, b := incidentMaps(facts, incidentCounts(29, 2), good)
		vote := evaluateRouteIncident(facts, incidentTestRC, q, b)
		require.Equal(t, 0, vote.comparable)
		require.False(t, vote.degraded)
	})

	t.Run("baseline n=29 abstains", func(t *testing.T) {
		facts := incidentFacts([]int64{1, 2}, "https://a.example.com/v1")
		q, b := incidentMaps(facts, bad, incidentCounts(29, 25))
		vote := evaluateRouteIncident(facts, incidentTestRC, q, b)
		require.Equal(t, 0, vote.comparable)
		require.False(t, vote.degraded)
	})

	t.Run("empty intervals never degrade", func(t *testing.T) {
		require.False(t, IsDegraded(Wilson95(0, 0), Wilson95(0, 0), 0, 0))
	})

	t.Run("baseline-empty with current-hot is inactive", func(t *testing.T) {
		facts := incidentFacts([]int64{1, 2}, "https://a.example.com/v1")
		q, _ := incidentMaps(facts, bad, good)
		vote := evaluateRouteIncident(facts, incidentTestRC, q, nil)
		require.Equal(t, 0, vote.comparable, "no baseline, lanes still use current")
		require.False(t, vote.degraded)
	})

	t.Run("TTFT-less evidence still counts", func(t *testing.T) {
		facts := incidentFacts([]int64{1, 2}, "https://a.example.com/v1")
		noTTFT := Counts{Attempts: 40, Successes: 8}
		q, b := incidentMaps(facts, noTTFT, good)
		vote := evaluateRouteIncident(facts, incidentTestRC, q, b)
		require.Equal(t, 2, vote.comparable, "non-stream outages are comparable on attempts alone")
		require.True(t, vote.degraded, "40-attempt 20%-success vs 60-attempt 92% baseline degrades")
	})

	t.Run("empty origin abstains fail-closed", func(t *testing.T) {
		facts := incidentFacts([]int64{1, 2}, "")
		badURL := incidentFacts([]int64{3}, "://bad-url")
		facts = append(facts, badURL...)
		q, b := incidentMaps(facts, bad, good)
		vote := evaluateRouteIncident(facts, incidentTestRC, q, b)
		require.Equal(t, 0, vote.comparable)
		require.False(t, vote.degraded)
	})

	t.Run("domain collisions share one origin", func(t *testing.T) {
		facts := append(incidentFacts([]int64{1}, "https://A.Example.COM:443/x"),
			incidentFacts([]int64{2}, "https://a.example.com/y")...)
		facts = append(facts, incidentFacts([]int64{3}, "http://a.example.com:80/")...)
		q, b := incidentMaps(facts, bad, good)
		vote := evaluateRouteIncident(facts, incidentTestRC, q, b)
		require.True(t, vote.degraded)
		byDomain := map[string]int{}
		for _, c := range vote.cands {
			byDomain[c.Domain]++
		}
		require.Equal(t, map[string]int{"https://a.example.com:443": 2, "http://a.example.com:80": 1}, byDomain,
			"scheme/port casing + paths collapse, http vs https differ")
		require.Equal(t, 2, vote.domains)
	})

	t.Run("model-only kind", func(t *testing.T) {
		facts := append(incidentFacts([]int64{1}, "https://a.example.com/"),
			incidentFacts([]int64{2}, "https://b.example.com/")...)
		facts = append(facts, incidentFacts([]int64{3, 4}, "https://c.example.com/")...)
		q, b := incidentMaps(facts, bad, good)
		// c.example.com recovers: healthy while a/b stay degraded.
		for _, f := range facts[2:] {
			k := CandidateQualityKey{RouteClassID: incidentTestRC, Fingerprint: f.identityFingerprint}
			q[k] = CandidateQualityInput{Counts: okCur}
		}
		vote := evaluateRouteIncident(facts, incidentTestRC, q, b)
		require.Equal(t, 4, vote.comparable)
		require.Equal(t, 2, vote.degradedCount)
		require.False(t, DomainIncident(vote.cands), "2/4 degraded is not a domain majority")
		require.True(t, ModelIncident(vote.cands), "2/3 incident domains is a model majority")
		require.True(t, vote.degraded)
		require.Equal(t, IncidentKindModel, vote.kind)
	})

	t.Run("both kinds", func(t *testing.T) {
		facts := append(incidentFacts([]int64{1, 2}, "https://a.example.com/"),
			incidentFacts([]int64{3, 4}, "https://b.example.com/")...)
		q, b := incidentMaps(facts, bad, good)
		q[CandidateQualityKey{RouteClassID: incidentTestRC, Fingerprint: facts[3].identityFingerprint}] =
			CandidateQualityInput{Counts: okCur}
		vote := evaluateRouteIncident(facts, incidentTestRC, q, b)
		require.True(t, vote.degraded)
		require.Equal(t, IncidentKindBoth, vote.kind)
	})

	t.Run("evidence hash tracks count changes", func(t *testing.T) {
		facts := incidentFacts([]int64{1, 2}, "https://a.example.com/v1")
		q, b := incidentMaps(facts, bad, good)
		first := evaluateRouteIncident(facts, incidentTestRC, q, b)
		again := evaluateRouteIncident(facts, incidentTestRC, q, b)
		require.Equal(t, first.evidence, again.evidence, "deterministic on identical inputs")
		for _, f := range facts {
			k := CandidateQualityKey{RouteClassID: incidentTestRC, Fingerprint: f.identityFingerprint}
			shifted := q[k]
			shifted.Counts.Attempts++
			q[k] = shifted
		}
		moved := evaluateRouteIncident(facts, incidentTestRC, q, b)
		require.NotEqual(t, first.evidence, moved.evidence, "count drift refreshes the cycle")
	})
}

func TestRouteIncidentTrackerCycles(t *testing.T) {
	good := incidentCounts(60, 55)
	bad := incidentCounts(40, 8)
	okCur := incidentCounts(40, 35)
	facts := incidentFacts([]int64{1, 2, 3}, "https://a.example.com/v1")
	route := RouteRef{GroupID: 10, Format: "openai-chat", Model: "m"}
	m1, m2, m3 := int64(100), int64(101), int64(102)

	tr := newIncidentTracker()
	q, b := incidentMaps(facts, bad, good)

	// Fire 1 (M1, degraded): active with detection evidence.
	first := tr.evaluate(route, evaluateRouteIncident(facts, incidentTestRC, q, b), m1)
	require.True(t, first.Active)
	require.Equal(t, IncidentKindDomain, first.Kind)
	require.Equal(t, 3, first.Comparable)
	require.Equal(t, 3, first.Degraded)
	require.Equal(t, 1, first.Domains)
	require.Equal(t, m1, first.EvaluatedMinute)

	// Identical refire (M1, same counts): frozen — same incident, no streak burn.
	frozen := tr.evaluate(route, evaluateRouteIncident(facts, incidentTestRC, q, b), m1)
	require.Equal(t, first, frozen)
	require.Equal(t, 0, tr.states[route].HealthyStreak)

	// Same counts at M2: fresh evaluation (M advanced), still degraded.
	moved := tr.evaluate(route, evaluateRouteIncident(facts, incidentTestRC, q, b), m2)
	require.True(t, moved.Active)
	require.Equal(t, m2, moved.EvaluatedMinute)
	require.Equal(t, 0, tr.states[route].HealthyStreak, "degraded re-fire resets the streak")

	// Fresh healthy at M3 (changed counts): streak 1, detection evidence stands.
	q2, _ := incidentMaps(facts, okCur, good)
	recovering := tr.evaluate(route, evaluateRouteIncident(facts, incidentTestRC, q2, b), m3)
	require.True(t, recovering.Active, "one healthy vote does not clear")
	require.Equal(t, first.Kind, recovering.Kind)
	require.Equal(t, m2, recovering.EvaluatedMinute, "detection evidence stands during recovery")
	require.Equal(t, 1, tr.states[route].HealthyStreak)

	// Second fresh healthy vote clears.
	q3, _ := incidentMaps(facts, incidentCounts(41, 36), good)
	cleared := tr.evaluate(route, evaluateRouteIncident(facts, incidentTestRC, q3, b), m3+1)
	require.False(t, cleared.Active)
	require.Equal(t, RouteIncident{}, cleared, "cleared incident is the zero value")
}

func TestRouteIncidentIdleGapNeverRecovers(t *testing.T) {
	good := incidentCounts(60, 55)
	bad := incidentCounts(40, 8)
	facts := incidentFacts([]int64{1, 2}, "https://a.example.com/v1")
	route := RouteRef{GroupID: 10, Format: "openai-chat", Model: "m"}
	tr := newIncidentTracker()
	q, b := incidentMaps(facts, bad, good)

	active := tr.evaluate(route, evaluateRouteIncident(facts, incidentTestRC, q, b), 100)
	require.True(t, active.Active)
	// Idle fleet: identical evidence across minutes re-affirms, never recovers.
	for _, m := range []int64{101, 102, 103} {
		still := tr.evaluate(route, evaluateRouteIncident(facts, incidentTestRC, q, b), m)
		require.True(t, still.Active, "minute %d: static evidence keeps the incident", m)
		require.Equal(t, 0, tr.states[route].HealthyStreak)
	}
}

func TestRouteIncidentSparseNeverActivates(t *testing.T) {
	good := incidentCounts(60, 55)
	bad := incidentCounts(40, 8)
	facts := incidentFacts([]int64{1}, "https://a.example.com/v1")
	route := RouteRef{GroupID: 10, Format: "openai-chat", Model: "m"}
	tr := newIncidentTracker()
	q, b := incidentMaps(facts, bad, good)
	for _, m := range []int64{100, 101, 102} {
		got := tr.evaluate(route, evaluateRouteIncident(facts, incidentTestRC, q, b), m)
		require.False(t, got.Active, "single comparable domain casts healthy votes only")
	}
}

func TestRouteIncidentValidation(t *testing.T) {
	valid := &RouteDecision{Incident: RouteIncident{
		Active: true, Kind: IncidentKindBoth, Comparable: 4, Degraded: 3, Domains: 2, EvaluatedMinute: 100,
	}}
	require.NoError(t, validateRouteDecision(valid))

	badKind := &RouteDecision{Incident: RouteIncident{Active: true, Kind: "region", Comparable: 2, Degraded: 2}}
	require.Error(t, validateRouteDecision(badKind), "kind enum enforced")

	inverted := &RouteDecision{Incident: RouteIncident{Active: true, Kind: IncidentKindDomain, Comparable: 2, Degraded: 3}}
	require.Error(t, validateRouteDecision(inverted), "degraded ≤ comparable enforced")

	dirtyInactive := &RouteDecision{Incident: RouteIncident{Kind: IncidentKindDomain, Comparable: 1}}
	require.Error(t, validateRouteDecision(dirtyInactive), "inactive must be the zero value")

	negative := &RouteDecision{Incident: RouteIncident{Active: true, Kind: IncidentKindModel, Comparable: 2, Degraded: 1, Domains: -1}}
	require.Error(t, validateRouteDecision(negative), "domains ≥ 0 enforced")

	require.NoError(t, validateRouteDecision(&RouteDecision{}), "zero incident is valid")
}

// incidentLaneFixture builds a two-group scheduler whose group-10 route is
// degraded (current bad vs baseline good) and group-20 healthy, wired through
// the windowed provider with a fixed evaluated minute.
func incidentLaneFixture(t *testing.T) (*Scheduler, *memLoader, map[CandidateQualityKey]CandidateQualityInput, map[CandidateQualityKey]Counts, map[string]domain.ResolvedPrices, int64) {
	t.Helper()
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	a1 := accWithEnabled(1, tpl, true, 10000)
	a2 := accWithEnabled(2, tpl, true, 10000)
	a3 := accWithEnabled(3, tpl, true, 10000)
	b1 := accWithEnabled(4, tpl, true, 10000)
	b2 := accWithEnabled(5, tpl, true, 10000)
	m := newMemLoader(map[int64][]*domain.Account{10: {a1, a2, a3}, 20: {b1, b2}})
	badInput := func() CandidateQualityInput {
		return CandidateQualityInput{Counts: incidentCounts(40, 8), InputTokens: 800, OutputTokens: 800}
	}
	okInput := func() CandidateQualityInput {
		return CandidateQualityInput{Counts: incidentCounts(40, 35), InputTokens: 3500, OutputTokens: 3500}
	}
	q := buildQuality(10, domain.FormatOpenAIChat, "m",
		map[int64]CandidateQualityInput{1: badInput(), 2: badInput(), 3: badInput()}, []*domain.Account{a1, a2, a3})
	for k, v := range buildQuality(20, domain.FormatOpenAIChat, "m",
		map[int64]CandidateQualityInput{4: okInput(), 5: okInput()}, []*domain.Account{b1, b2}) {
		q[k] = v
	}
	base := make(map[CandidateQualityKey]Counts, len(q))
	for k := range q {
		base[k] = incidentCounts(60, 55)
	}
	prices := map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000), OutputPerM: pricePtr(1000)}}
	s, _ := newProbedSched(t, m, q, prices)
	boundary := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	s.sources = &CompilerSources{
		Quality: func(time.Time) WindowedQuality {
			return WindowedQuality{Current: q, Baseline: base, SettledBoundary: boundary}
		},
		Prices: func() map[string]domain.ResolvedPrices { return prices },
	}
	return s, m, q, base, prices, boundary.Unix()
}

func TestRouteIncidentScopedFullByteEquality(t *testing.T) {
	s, _, q, base, prices, minute := incidentLaneFixture(t)
	s.InvalidateGroup(10)
	s.compileOnce()

	afterDec := s.View().DecisionView()
	ref10 := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	rd10, ok := afterDec.routes[ref10]
	require.True(t, ok)
	inc := rd10.Incident
	require.True(t, inc.Active, "group-10 route carries the domain incident")
	require.Equal(t, IncidentKindDomain, inc.Kind)
	require.Equal(t, 3, inc.Comparable)
	require.Equal(t, 3, inc.Degraded)
	require.Equal(t, 1, inc.Domains)
	require.Equal(t, minute, inc.EvaluatedMinute)

	ref20 := RouteRefFor(20, string(domain.FormatOpenAIChat), "m")
	rd20, ok := afterDec.routes[ref20]
	require.True(t, ok)
	require.False(t, rd20.Incident.Active, "healthy route stays quiet")

	// Lanes are untouched by incidents: the same compile without a baseline
	// serves identical lane membership (detect + surface only).
	oracle := NewRoutingCompiler()
	plain, err := oracle.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices})
	require.NoError(t, err)
	plain10 := plain.routes[ref10]
	require.Equal(t, allLaneIDs(plain10), allLaneIDs(rd10), "no lane reordering from incidents")

	// Scoped fire equals the full-recompile oracle bit-for-bit with the
	// identical transition function (frozen evaluations reuse stored state).
	full, err := oracle.Compile(CompilerInputs{
		Static: s.View().StaticView(), Quality: q, Baseline: base, Prices: prices,
		EvaluatedMinute: minute, IncidentEval: s.incidentEvaluator(),
	})
	require.NoError(t, err)
	require.Equal(t, decisionViewBytes(full), decisionViewBytes(afterDec),
		"scoped output equals the full-recompile oracle bit-for-bit, incidents included")
}

func TestRouteIncidentSingleMThreading(t *testing.T) {
	s, _, _, _, _, minute := incidentLaneFixture(t)
	s.InvalidateGroup(10)
	s.compileOnce()
	ref10 := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	first := s.View().DecisionView().routes[ref10].Incident
	require.True(t, first.Active)

	// A second fire under a different wall clock but the same evaluated
	// minute freezes: identical evidence, no streak burn, same bytes.
	s.timeNow = func() time.Time { return time.Date(2026, time.August, 29, 12, 5, 0, 0, time.UTC) }
	before := decisionViewBytes(s.View().DecisionView())
	s.RequestCompile()
	s.compileOnce()
	second := s.View().DecisionView().routes[ref10].Incident
	require.Equal(t, first, second)
	require.Equal(t, minute, second.EvaluatedMinute)
	require.Equal(t, before, decisionViewBytes(s.View().DecisionView()))
}

func TestRouteIncidentPruneOnFull(t *testing.T) {
	s, m, _, _, _, _ := incidentLaneFixture(t)
	s.InvalidateGroup(10)
	s.compileOnce()
	require.Greater(t, s.incidents.activeCount(), 0)

	// Drop group 20 from the loader, reload, and force the full-fidelity path
	// via scope overflow: pruned routes must lose their incident state.
	m.mu.Lock()
	delete(m.byGroup, 20)
	m.mu.Unlock()
	require.NoError(t, s.reload(context.Background()))
	for i := 0; i < scopeChCap+1; i++ {
		s.enqueueCompileScope([]int64{10}, nil, "prune-test")
	}
	s.compileOnce()
	for r := range s.incidents.states {
		require.NotEqual(t, int64(20), r.GroupID, "full compiles prune gone routes")
	}
	st := s.Stats().(SchedulerStats)
	require.Equal(t, s.incidents.activeCount(), st.ActiveIncidents)
	require.Greater(t, st.LastIncidentEvalUnixMs, int64(0))
}

// TestCompileMinuteAdvanceTrigger pins the M-advance riding rule: windowed
// inputs are a function of M, so a lane-quiet boundary crossing fires the
// lane once (settle-visibility without any other trigger); busy lanes and
// same-minute ticks stay silent. No periodic-unconditional recompile.
func TestCompileMinuteAdvanceTrigger(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)

	drain := func() {
		for {
			select {
			case <-s.compileCh:
			default:
				return
			}
		}
	}
	fireMinute := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	s.timeNow = func() time.Time { return fireMinute }
	s.compileOnce()
	require.Equal(t, fireMinute.UTC().Truncate(time.Minute).Unix(), s.lastFireMinute.Load())
	drain()

	// Same minute: silent.
	s.fireOnMinuteAdvance()
	require.Empty(t, s.compileCh, "busy/same-minute lane stays silent")

	// Boundary crossed with no intervening fire: exactly one signal.
	s.timeNow = func() time.Time { return fireMinute.Add(61 * time.Second) }
	s.fireOnMinuteAdvance()
	require.Len(t, s.compileCh, 1)
	// Coalesced: further ticks before the fire stay at cap 1.
	s.fireOnMinuteAdvance()
	require.Len(t, s.compileCh, 1)

	// The fire restamps: the next boundary — and only the next — refires.
	s.compileOnce()
	drain()
	s.fireOnMinuteAdvance()
	require.Empty(t, s.compileCh)
	s.timeNow = func() time.Time { return fireMinute.Add(121 * time.Second) }
	s.fireOnMinuteAdvance()
	require.Len(t, s.compileCh, 1)
}
