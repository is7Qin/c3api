// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

// Counter-fidelity gate (v3 §7 G-counter-fidelity): increment==event, zero
// tolerance. Single-threaded deterministic scripts (no sleeps, no parallelism
// except the explicit smoke), fixed bucket, exact per-cell ChainCounts.

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func foldVal(seed byte) domain.RouteClassIDVal {
	var v domain.RouteClassIDVal
	for i := range v {
		v[i] = seed + byte(i)
	}
	return v
}

func foldFP(seed byte) domain.CandidateFingerprintVal {
	var v domain.CandidateFingerprintVal
	for i := range v {
		v[i] = seed + byte(i)
	}
	return v
}

// foldOneRow folds one consumer row through the request walk.
// the Submit queue is gone; a single-fact FoldChain call carries the
// same row with identical counting.
func foldOneRow(t *testing.T, o *FlowOwner, minute int64, row repository.RoutingFlowRow) {
	t.Helper()
	foldRows(t, o, minute, row)
}

// foldRows folds rows through one request-walk call (Submit successor
// for tests: same rows, same minute, identical counting).
func foldRows(t *testing.T, o *FlowOwner, minute int64, rows ...repository.RoutingFlowRow) {
	t.Helper()
	o.FoldChain(minute, len(rows), func(i int) (
		domain.RouteClassIDVal, domain.CandidateFingerprintVal, int64, int64, int64, uint8, string, string, string, bool, bool,
	) {
		row := rows[i]
		var prev int64
		hasPrev := row.PreviousAccountID != nil
		if hasPrev {
			prev = *row.PreviousAccountID
		}
		return row.RouteClassID, row.CandidateFingerprint, row.AccountID, prev,
			row.Generation, uint8(row.Ordinal), row.Lane, row.Outcome, row.PreviousOutcome,
			row.IsTerminal, hasPrev
	})
}

// foldConsumerRows folds consumer rows through the live cell path
// (: the synchronous EnqueueFlowMinute seam is deleted — zero
// production callers — so tests drive the same rowToSeed/makeFact/addFact
// primitive the request walk lands on, with identical facts, deltas, and
// class counters. Post-seal folds land residual exactly like the sealed
// consumer path did; closed/finalized recorders reject with ErrCapacity).
func foldConsumerRows(o *FlowOwner, minute int64, rows []repository.RoutingFlowRow) error {
	if o.closed.Load() || o.rec.finalized() {
		return ErrCapacity
	}
	for _, row := range rows {
		seed, delta, err := rowToSeed(row)
		if err != nil {
			return err
		}
		f, err := makeFact(seed, minute)
		if err != nil {
			return err
		}
		if o.sealed.Load() {
			f.residual = true
		}
		if !o.addFact(f, delta) {
			o.edgeRowsDropped.Add(delta)
			o.overflowed.Add(delta)
		}
	}
	return nil
}

type foldScriptEdge struct {
	route      domain.RouteClassIDVal
	fp         domain.CandidateFingerprintVal
	account    int64
	prev       int64
	hasPrev    bool
	prevToken  string
	generation int64
	ordinal    uint8
	lane       string
	outcome    string
	transition string
	terminal   bool
	times      int
}

// foldScript emits one chain per call through the request walk and returns
// the scripted per-fact event totals keyed by packed fact.
func foldScript(t *testing.T, o *FlowOwner, bucket int64, chains [][]foldScriptEdge) map[attemptFact]int64 {
	t.Helper()
	want := make(map[attemptFact]int64)
	for _, chain := range chains {
		n := 0
		for _, e := range chain {
			n += e.times
		}
		flat := make([]foldScriptEdge, 0, n)
		for _, e := range chain {
			for k := 0; k < e.times; k++ {
				flat = append(flat, e)
			}
		}
		o.FoldChain(bucket, len(flat), func(i int) (
			route domain.RouteClassIDVal,
			fp domain.CandidateFingerprintVal,
			accountID, prevAccount, generation int64,
			ordinal uint8,
			lane, outcome, prevOutcome string,
			terminal, hasPrev bool,
		) {
			e := flat[i]
			return e.route, e.fp, e.account, e.prev, e.generation, e.ordinal,
				e.lane, e.outcome, e.prevToken, e.terminal, e.hasPrev
		})
		for _, e := range flat {
			f, err := makeFact(foldSeed{
				route: e.route, fp: e.fp, accountID: e.account, prevAccount: e.prev,
				generation: e.generation, ordinal: e.ordinal, lane: e.lane,
				outcome: e.outcome, prevOutcome: e.prevToken, transition: e.transition,
				terminal: e.terminal, hasPrev: e.hasPrev,
			}, bucket)
			require.NoError(t, err)
			want[f]++
		}
	}
	return want
}

func foldRowsByFact(t *testing.T, rows []repository.RoutingFlowRow) map[attemptFact]int64 {
	t.Helper()
	got := make(map[attemptFact]int64)
	for _, r := range rows {
		lane, ok := foldLaneCode(r.Lane)
		require.True(t, ok, "expanded lane must be a codebook string")
		outcome, ok := foldOutcomeCode(r.Outcome)
		require.True(t, ok, "expanded outcome must be a codebook string")
		prev, ok := foldOutcomeCode(r.PreviousOutcome)
		require.True(t, ok || r.PreviousOutcome == "", "expanded prevOutcome must be a codebook string")
		transition, ok := foldTransitionCode(r.TransitionReason)
		require.True(t, ok, "expanded transition must be a codebook string")
		var prevAcct int64
		hasPrev := r.PreviousAccountID != nil
		if hasPrev {
			prevAcct = *r.PreviousAccountID
		}
		f := canonicalFact(attemptFact{
			route:           r.RouteClassID,
			fingerprint:     r.CandidateFingerprint,
			accountID:       r.AccountID,
			prevAccount:     prevAcct,
			generation:      r.Generation,
			minuteBucket:    r.TerminalMinute.UTC().Unix(),
			ordinal:         uint8(r.Ordinal),
			lane:            lane,
			outcome:         outcome,
			prevOutcome:     prev,
			transition:      transition,
			identityVersion: uint8(r.IdentityVersion),
			isTerminal:      r.IsTerminal,
			hasPrev:         hasPrev,
			hasPrevOutcome:  r.PreviousOutcome != "",
		})
		got[f] += r.ChainCount
	}
	return got
}

// TestFoldCounterFidelity_DeterministicIncrementsEqualChainCounts covers the
// §5.2 derivation surface in one deterministic script: every lane, every
// outcome class, every transition code, terminal and nonterminal edges,
// hasPrev set and unset, multi-event folds. ChainCounts equal scripted counts
// per cell exactly — increment==event.
func TestFoldCounterFidelity_DeterministicIncrementsEqualChainCounts(t *testing.T) {
	ResetFlowChainCountersForTest()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	bucket := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC).Unix()

	lanes := []string{"primary", "explore", "degraded"}
	outcomes := []string{"success", "error", "4xx", "429", "5xx", "network"}
	var chains [][]foldScriptEdge
	var total int64
	seq := 0
	for _, lane := range lanes {
		for _, outcome := range outcomes {
			seq++
			// Three-edge chain: nonterminal linkage plus a terminal
			// edge, each folded `times` events. Transition is derived
			// by the walk (ordinal 1 -> init, else failover); explicit
			// transition codes ride the consumer-row fold below.
			times := 1 + seq%2
			chain := []foldScriptEdge{
				{route: foldVal(1), fp: foldFP(byte(seq)), account: int64(1000 + seq), generation: 7, ordinal: 1, lane: lane, outcome: outcome, terminal: false, times: times},
				{route: foldVal(1), fp: foldFP(byte(seq)), account: int64(2000 + seq), prev: int64(1000 + seq), hasPrev: true, prevToken: outcome, generation: 7, ordinal: 2, lane: lane, outcome: "success", terminal: false, times: times},
				{route: foldVal(1), fp: foldFP(byte(seq)), account: int64(3000 + seq), prev: int64(2000 + seq), hasPrev: true, prevToken: "success", generation: 7, ordinal: 3, lane: lane, outcome: outcome, terminal: true, times: times},
			}
			chains = append(chains, chain)
			total += int64(3 * times)
		}
	}
	// Consumer rows ride the same facts with explicit transitions:
	// the old-alias "init", "retry", "flow", "plan", plus a rule-ok outcome.
	require.NoError(t, foldConsumerRows(rec.FlowOwner(), bucket, []repository.RoutingFlowRow{
		{IdentityVersion: 1, RouteClassID: foldVal(9), Ordinal: 1, Lane: "primary", AccountID: 91, TransitionReason: "init", Outcome: "ok", IsTerminal: true, Generation: 1, CandidateFingerprint: foldFP(9), ChainCount: 4},
		{IdentityVersion: 1, RouteClassID: foldVal(9), Ordinal: 2, Lane: "explore", AccountID: 92, PreviousAccountID: func() *int64 { v := int64(91); return &v }(), PreviousOutcome: "success", TransitionReason: "retry", Outcome: "error", IsTerminal: true, Generation: 1, CandidateFingerprint: foldFP(9), ChainCount: 2},
		{IdentityVersion: 1, RouteClassID: foldVal(9), Ordinal: 2, Lane: "degraded", AccountID: 93, PreviousAccountID: func() *int64 { v := int64(91); return &v }(), PreviousOutcome: "429", TransitionReason: "flow", Outcome: "4xx", IsTerminal: false, Generation: 1, CandidateFingerprint: foldFP(9), ChainCount: 3},
		{IdentityVersion: 1, RouteClassID: foldVal(9), Ordinal: 3, Lane: "primary", AccountID: 94, PreviousAccountID: func() *int64 { v := int64(93); return &v }(), PreviousOutcome: "4xx", TransitionReason: "plan", Outcome: "5xx", IsTerminal: true, Generation: 1, CandidateFingerprint: foldFP(9), ChainCount: 1},
	}))
	total += 10

	want := foldScript(t, owner, bucket, chains)
	// The consumer rows above fold 4 + 2 + 3 + 1 events into four more cells.
	{
		f1, err := makeFact(foldSeed{route: foldVal(9), fp: foldFP(9), accountID: 91, generation: 1, ordinal: 1, lane: "primary", outcome: "ok", transition: "init", terminal: true}, bucket)
		require.NoError(t, err)
		want[f1] += 4
		f2, err := makeFact(foldSeed{route: foldVal(9), fp: foldFP(9), accountID: 92, prevAccount: 91, generation: 1, ordinal: 2, lane: "explore", outcome: "error", prevOutcome: "success", transition: "retry", terminal: true, hasPrev: true}, bucket)
		require.NoError(t, err)
		want[f2] += 2
		f3, err := makeFact(foldSeed{route: foldVal(9), fp: foldFP(9), accountID: 93, prevAccount: 91, generation: 1, ordinal: 2, lane: "degraded", outcome: "4xx", prevOutcome: "429", transition: "flow", terminal: false, hasPrev: true}, bucket)
		require.NoError(t, err)
		want[f3] += 3
		f4, err := makeFact(foldSeed{route: foldVal(9), fp: foldFP(9), accountID: 94, prevAccount: 93, generation: 1, ordinal: 3, lane: "primary", outcome: "5xx", prevOutcome: "4xx", transition: "plan", terminal: true, hasPrev: true}, bucket)
		require.NoError(t, err)
		want[f4] += 1
	}

	fm, ok := rec.FlowMinute(bucket)
	require.True(t, ok, "folded facts must be visible to the tick read")
	rows := fm.FlowRows()
	got := foldRowsByFact(t, rows)
	require.Equal(t, len(want), len(got), "cell population must match exactly")
	for f, wc := range want {
		require.Equal(t, wc, got[f], "count mismatch for fact %+v", f)
	}
	var sum int64
	for _, r := range rows {
		sum += r.ChainCount
	}
	require.Equal(t, total, sum, "every scripted event conserved exactly once")

	st := owner.SnapshotStats()
	require.Equal(t, total, st.EdgeRowsAccepted, "accepted == offered events")
	require.Zero(t, st.EdgeRowsDropped)
	require.Zero(t, st.ResidualRows)
	require.Zero(t, owner.cells.overflowTotal(), "zero-overflow proof")
	require.Zero(t, FlowChainEnqueueOverflow())
}

// TestFoldCounterFidelity_CapOverflowCountsWithoutCellAdds scripts the §5.1
// cap row: attempts beyond 8 count cap-overflow per dropped attempt with zero
// cell adds.
func TestFoldCounterFidelity_CapOverflowCountsWithoutCellAdds(t *testing.T) {
	ResetFlowChainCountersForTest()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	bucket := time.Date(2026, 8, 29, 12, 10, 0, 0, time.UTC).Unix()

	beforeCap := FlowChainCapacityOverflow()
	beforeStats := owner.SnapshotStats()
	seeds := make([]foldScriptEdge, 10)
	for i := range seeds {
		seeds[i] = foldScriptEdge{route: foldVal(2), fp: foldFP(2), account: 50, generation: 1, ordinal: uint8(i + 1), lane: "primary", outcome: "success", terminal: i == 7}
	}
	owner.FoldChain(bucket, len(seeds), func(i int) (
		domain.RouteClassIDVal, domain.CandidateFingerprintVal, int64, int64, int64, uint8, string, string, string, bool, bool,
	) {
		e := seeds[i]
		return e.route, e.fp, e.account, 0, e.generation, e.ordinal, e.lane, e.outcome, "", e.terminal, false
	})
	require.Equal(t, beforeCap+2, FlowChainCapacityOverflow(), "attempts 9-10 count cap-overflow")
	st := owner.SnapshotStats()
	require.Equal(t, beforeStats.EdgeRowsDropped+2, st.EdgeRowsDropped)
	require.Equal(t, beforeStats.Overflowed+2, st.Overflowed)
	require.Equal(t, beforeStats.EdgeRowsAccepted+8, st.EdgeRowsAccepted, "first 8 facts emit")
	fm, ok := rec.FlowMinute(bucket)
	require.True(t, ok)
	require.Len(t, fm.FlowRows(), 8, "zero cell adds past the cap")
}

// TestFoldCounterFidelity_TableFullCountsOverflowWithZeroAdds scripts the
// §5.1 table-full row on a tiny deterministic table: the unlanded fact lands
// in the per-shard overflow counter with zero cell adds, and the conservation
// equation still balances.
func TestFoldCounterFidelity_TableFullCountsOverflowWithZeroAdds(t *testing.T) {
	ResetFlowChainCountersForTest()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	owner.cells.resetCellsForTest(2)
	bucket := time.Date(2026, 8, 29, 12, 20, 0, 0, time.UTC).Unix()

	// Same shard (low 6 bits equal), three distinct keys.
	seeds := []foldScriptEdge{
		{route: foldVal(3), fp: foldFP(3), account: 7, generation: 1, ordinal: 1, lane: "primary", outcome: "success", terminal: false},
		{route: foldVal(3), fp: foldFP(3), account: 7, generation: 1, ordinal: 2, lane: "primary", outcome: "success", terminal: false},
		{route: foldVal(3), fp: foldFP(3), account: 7, generation: 1, ordinal: 3, lane: "primary", outcome: "success", terminal: true},
	}
	owner.FoldChain(bucket, len(seeds), func(i int) (
		domain.RouteClassIDVal, domain.CandidateFingerprintVal, int64, int64, int64, uint8, string, string, string, bool, bool,
	) {
		e := seeds[i]
		return e.route, e.fp, e.account, 0, e.generation, e.ordinal, e.lane, e.outcome, "", e.terminal, false
	})
	require.Equal(t, int64(1), owner.cells.overflowTotal(), "unlanded fact lands in the per-shard counter")
	require.Equal(t, int64(1), FlowChainEnqueueOverflow())
	st := owner.SnapshotStats()
	require.Equal(t, int64(2), st.EdgeRowsAccepted)
	require.Equal(t, int64(1), st.EdgeRowsDropped)
	require.Equal(t, int64(3), st.EdgeRowsAccepted+st.EdgeRowsDropped+st.ResidualRows, "conservation holds across the overflow")
	fm, ok := rec.FlowMinute(bucket)
	require.True(t, ok)
	require.Len(t, fm.FlowRows(), 2, "zero cell adds for the overflowed fact")
}

// TestFoldCounterFidelity_TerminalClassIsCellIdentity proves option (a):
// terminality is final at emission — the terminal bit is cell identity, so
// two otherwise-identical edges land in two distinct cells and there is never
// a provisional increment to repair.
func TestFoldCounterFidelity_TerminalClassIsCellIdentity(t *testing.T) {
	ResetFlowChainCountersForTest()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	bucket := time.Date(2026, 8, 29, 12, 30, 0, 0, time.UTC).Unix()

	mk := func(terminal bool) foldScriptEdge {
		return foldScriptEdge{route: foldVal(4), fp: foldFP(4), account: 60, generation: 1, ordinal: 1, lane: "primary", outcome: "success", terminal: terminal}
	}
	for _, term := range []bool{false, true, true} {
		e := mk(term)
		owner.FoldChain(bucket, 1, func(i int) (
			domain.RouteClassIDVal, domain.CandidateFingerprintVal, int64, int64, int64, uint8, string, string, string, bool, bool,
		) {
			return e.route, e.fp, e.account, 0, e.generation, e.ordinal, e.lane, e.outcome, "", e.terminal, false
		})
	}
	fm, ok := rec.FlowMinute(bucket)
	require.True(t, ok)
	rows := fm.FlowRows()
	require.Len(t, rows, 2, "terminal and nonterminal classes stay distinct cells")
	for _, r := range rows {
		if r.IsTerminal {
			require.Equal(t, int64(2), r.ChainCount, "terminal cell holds exactly the completed count")
		} else {
			require.Equal(t, int64(1), r.ChainCount)
		}
	}
}

// TestFoldCounterFidelity_ConcurrentAddsAreExact is the exactness-under-
// concurrency smoke: M goroutines x K single-edge chains onto one hot cell
// land exactly MxK (barrier start, WaitGroup join — no sleeps).
func TestFoldCounterFidelity_ConcurrentAddsAreExact(t *testing.T) {
	ResetFlowChainCountersForTest()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	bucket := time.Date(2026, 8, 29, 12, 40, 0, 0, time.UTC).Unix()

	const m = 8
	const k = 200
	route, fp := foldVal(5), foldFP(5)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(m)
	for g := 0; g < m; g++ {
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < k; i++ {
				owner.FoldChain(bucket, 1, func(_ int) (
					domain.RouteClassIDVal, domain.CandidateFingerprintVal, int64, int64, int64, uint8, string, string, string, bool, bool,
				) {
					return route, fp, 70, 0, 1, 1, "primary", "success", "", true, false
				})
			}
		}()
	}
	close(start)
	wg.Wait()

	fm, ok := rec.FlowMinute(bucket)
	require.True(t, ok)
	rows := fm.FlowRows()
	require.Len(t, rows, 1)
	require.Equal(t, int64(m*k), rows[0].ChainCount, "concurrent adds land exactly MxK")
	st := owner.SnapshotStats()
	require.Equal(t, int64(m*k), st.EdgeRowsAccepted)
	require.Zero(t, st.EdgeRowsDropped)
	require.Zero(t, owner.cells.overflowTotal())
}

// TestFoldCounterFidelity_ZeroOverflowAtGate is the provisioning proof: mixed
// request-walk and consumer-seam traffic leaves every overflow counter at
// zero. (The edges and empty-marker halves have no live writer.)
func TestFoldCounterFidelity_ZeroOverflowAtGate(t *testing.T) {
	ResetFlowChainCountersForTest()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	bucket := time.Date(2026, 8, 29, 12, 50, 0, 0, time.UTC).Unix()

	owner.FoldChain(bucket, 2, func(i int) (
		domain.RouteClassIDVal, domain.CandidateFingerprintVal, int64, int64, int64, uint8, string, string, string, bool, bool,
	) {
		if i == 0 {
			return foldVal(6), foldFP(6), 80, 0, 1, 1, "primary", "success", "", false, false
		}
		return foldVal(6), foldFP(6), 81, 80, 1, 2, "explore", "429", "success", true, true
	})
	require.NoError(t, foldConsumerRows(rec.FlowOwner(), bucket, []repository.RoutingFlowRow{
		{IdentityVersion: 1, RouteClassID: foldVal(6), Ordinal: 1, Lane: "degraded", AccountID: 82, TransitionReason: "plan", Outcome: "5xx", IsTerminal: true, Generation: 3, CandidateFingerprint: foldFP(6), ChainCount: 1},
	}))

	st := owner.SnapshotStats()
	require.Equal(t, int64(3), st.EdgeRowsAccepted)
	require.Zero(t, st.EdgeRowsDropped)
	require.Zero(t, owner.cells.overflowTotal(), "zero-overflow proof")
	require.Zero(t, FlowChainEnqueueOverflow())
	require.Zero(t, FlowChainCapacityOverflow())
}
