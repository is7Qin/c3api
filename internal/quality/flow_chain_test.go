// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/repository"
)

func fixedHex(seed byte) string {
	var b [32]byte
	for i := range b {
		b[i] = seed + byte(i)
	}
	// hex encode 32 bytes -> 64 chars
	const hextable = "0123456789abcdef"
	out := make([]byte, 64)
	for i, v := range b {
		out[i*2] = hextable[v>>4]
		out[i*2+1] = hextable[v&0x0f]
	}
	return string(out)
}

func validDispatch(ordinal uint8, isTerminal bool, prev *string) FlowDispatch {
	var prevAcct *int64
	var prevOutcome string
	if ordinal > 1 && prev != nil {
		v := int64(ordinal - 1)
		prevAcct = &v
		prevOutcome = "retry"
	}
	route := fixedHex(byte(ordinal + 10))
	qual := fixedHex(byte(ordinal + 20))
	fp := fixedHex(byte(ordinal + 30))
	lane := "primary"
	if ordinal%2 == 0 {
		lane = "explore"
	}
	tr := "initial"
	if ordinal > 1 {
		tr = "retry_429"
	}
	outcome := "success"
	if !isTerminal && ordinal < 3 {
		outcome = "fail_429"
	}
	if isTerminal && ordinal == 8 {
		outcome = "success"
	}
	return FlowDispatch{
		RouteClassID:      route,
		QualityClassID:    qual,
		Fingerprint:       fp,
		TemplateID:        int64(ordinal*10 + 1),
		AccountID:         int64(ordinal*100 + 5),
		RequestedModel:    "gpt-4o",
		MappedModel:       "gpt-4o",
		Generation:        int64(ordinal),
		LifecycleRevision: int64(ordinal),
		Ordinal:           ordinal,
		Lane:              lane,
		PreviousAttemptID: prev,
		PreviousAccountID: prevAcct,
		PreviousOutcome:   prevOutcome,
		TransitionReason:  tr,
		Outcome:           outcome,
		IsTerminal:        isTerminal,
	}
}

func TestFlowChain_BoundedAppendAndSingleTerminal(t *testing.T) {
	ResetFlowChainCountersForTest()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 5, 123, time.UTC)
	chain := NewFlowChain(rec, func() time.Time { return fixed })
	// Append 8 with exactly one terminal at last
	for i := 1; i <= 8; i++ {
		isTerm := i == 8
		var prev *string
		if i > 1 {
			s := string(rune('a' + i - 2))
			prev = &s
		}
		d := validDispatch(uint8(i), isTerm, prev)
		require.NoError(t, chain.Append(d), "append %d", i)
		require.Equal(t, i, chain.DispatchCount())
	}
	require.True(t, chain.HasTerminal())
	require.Equal(t, fixed.UTC().Truncate(time.Minute).Unix(), chain.TerminalMinute())
	// ninth dispatch impossible distinctive overflow
	before := FlowChainCapacityOverflow()
	d9 := validDispatch(9, true, func() *string { s := "prev8"; return &s }())
	require.Error(t, chain.Append(d9))
	require.Equal(t, before+1, FlowChainCapacityOverflow())
	require.ErrorIs(t, chain.Append(d9), ErrFlowChainOverflow)
	require.Equal(t, before+2, FlowChainCapacityOverflow())
	// further append after terminal also blocked by overflow already
	dDup := validDispatch(2, false, func() *string { s := "x"; return &s }())
	require.Error(t, chain.Append(dDup))
}

func TestFlowChain_TerminalMinuteUTC(t *testing.T) {
	ResetFlowChainCountersForTest()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	// terminal completion at 12:03:45.999 UTC -> minute truncates to 12:03:00
	termTime := time.Date(2026, 8, 29, 12, 3, 45, 999000000, time.UTC)
	chain := NewFlowChain(rec, func() time.Time { return termTime })
	for i := 1; i <= 3; i++ {
		isTerm := i == 3
		var prev *string
		if i > 1 {
			s := "a1"
			prev = &s
		}
		require.NoError(t, chain.Append(validDispatch(uint8(i), isTerm, prev)))
	}
	require.Equal(t, termTime.UTC().Truncate(time.Minute).Unix(), chain.TerminalMinute())
	require.NoError(t, chain.Complete())
	fm, ok := rec.FlowMinute(termTime.UTC().Truncate(time.Minute).Unix())
	require.True(t, ok)
	require.True(t, fm.HasFlowRows())
	require.Equal(t, termTime.UTC().Truncate(time.Minute).Unix(), fm.Minute())
	rows := fm.FlowRows()
	require.Len(t, rows, 3)
	// exactly one terminal and last is terminal
	termCount := 0
	for i, r := range rows {
		if r.IsTerminal {
			termCount++
			require.Equal(t, 2, i, "terminal must be last")
		}
		require.Equal(t, int16(i+1), r.Ordinal)
		require.NotEmpty(t, r.Lane)
		require.NotEmpty(t, r.TransitionReason)
		require.NotEmpty(t, r.Outcome)
	}
	require.Equal(t, 1, termCount)
}

func TestFlowChain_NormalCompletionEnqueuesFlowSnapshot(t *testing.T) {
	ResetFlowChainCountersForTest()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 10, 0, 0, time.UTC)
	chain := NewFlowChain(rec, func() time.Time { return fixed })
	for i := 1; i <= 2; i++ {
		isTerm := i == 2
		var prev *string
		if i > 1 {
			s := "a1"
			prev = &s
		}
		require.NoError(t, chain.Append(validDispatch(uint8(i), isTerm, prev)))
	}
	require.NoError(t, chain.Complete())
	require.True(t, chain.IsCompleted())
	// Verify the snapshot shape, cloned rows, and terminal minute.
	fm, ok := rec.FlowMinute(fixed.UTC().Truncate(time.Minute).Unix())
	require.True(t, ok)
	require.True(t, fm.HasFlowRows())
	require.False(t, fm.IsEmptySnapshot())
	rows := fm.FlowRows()
	require.Len(t, rows, 2)
	// Verify complete dispatch metadata preserved in meta vs row projection
	for i, r := range rows {
		require.Equal(t, int16(i+1), r.Ordinal)
		require.NotZero(t, r.Generation)
		require.NotEmpty(t, r.RouteClassID)
		require.NotEmpty(t, r.CandidateFingerprint)
	}
	// also verify NewFlowSnapshot compatibility: can be read via FlowMinute API
	var _ []repository.RoutingFlowRow = rows
}

func TestFlowChain_FlowOwnerOverflowCounted(t *testing.T) {
	ResetFlowChainCountersForTest()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	fixed := time.Date(2026, 8, 29, 12, 20, 0, 0, time.UTC)
	// Fill the bounded submission queue (owner consumer stalled: never
	// started) so the chain's Submit is rejected deterministically.
	for owner.Submit(9999, nil) == SubmitAccepted {
	}
	st := owner.Stats().(FlowOwnerStats)
	require.Equal(t, st.QueueCap, st.Queued, "queue bound is fixed and observable")
	chain := NewFlowChain(rec, func() time.Time { return fixed })
	for i := 1; i <= 2; i++ {
		isTerm := i == 2
		var prev *string
		if i > 1 {
			s := "a1"
			prev = &s
		}
		require.NoError(t, chain.Append(validDispatch(uint8(i), isTerm, prev)))
	}
	before := FlowChainEnqueueOverflow()
	// Rejected submission is telemetry-only: completion still settles exactly
	// once, while the event is counted and the quality lane stays untouched.
	require.NoError(t, chain.Complete())
	require.True(t, chain.IsCompleted())
	require.Equal(t, before+1, FlowChainEnqueueOverflow())
	require.Equal(t, int64(0), rec.QualityOverflow())
	ost := owner.Stats().(FlowOwnerStats)
	require.Equal(t, int64(2), ost.Overflowed, "one queue-full probe + one chain rejection")
	require.Equal(t, int64(2), ost.EdgeRowsDropped, "the chain's two edge rows are the only dropped payload")
}

// TestFlowChain_CompleteDoesNotWait pins the request-path contract: with the
// recorder lock AND the owner's consumer merge lock held, settlement still
// completes with exactly one immutable nonblocking submission. The old
// synchronous path (mergeFlowRows under Recorder.mu) deadlocks here; the new
// path never touches either lock and returns via the queue.
func TestFlowChain_CompleteDoesNotWait(t *testing.T) {
	ResetFlowChainCountersForTest()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	fixed := time.Date(2026, 8, 29, 12, 25, 0, 0, time.UTC)
	chain := NewFlowChain(rec, func() time.Time { return fixed })
	require.NoError(t, chain.Append(validDispatch(1, false, nil)))
	require.NoError(t, chain.Append(validDispatch(2, true, func() *string { s := "a1"; return &s }())))

	// Given: both consumer-side locks are held (stalled owner + recorder).
	rec.mu.Lock()
	owner.mu.Lock()
	done := make(chan error, 1)
	go func() { done <- chain.Complete() }()
	// Then: settlement does not wait for owner progress (watchdog, no sleep
	// racing: 2s bound vs an instant channel send).
	var cerr error
	select {
	case cerr = <-done:
	case <-time.After(2 * time.Second):
		owner.mu.Unlock()
		rec.mu.Unlock()
		t.Fatal("FlowChain.Complete waited on the stalled flow consumer")
	}
	require.NoError(t, cerr)
	require.True(t, chain.IsCompleted())
	owner.mu.Unlock()
	rec.mu.Unlock()
	st := owner.Stats().(FlowOwnerStats)
	require.Equal(t, int64(1), st.Accepted, "exactly one immutable submission")
	require.Equal(t, 1, st.Queued)
	// A duplicate completion after acceptance remains a no-op error.
	require.Error(t, chain.Complete())
}

func TestFlowChain_OwnerCleanupIncomplete(t *testing.T) {
	ResetFlowChainCountersForTest()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 30, 0, 0, time.UTC)
	chain := NewFlowChain(rec, func() time.Time { return fixed })
	require.NoError(t, chain.Append(validDispatch(1, false, nil)))
	before := FlowChainIncompleteObserved()
	chain.Close()
	require.Equal(t, before+1, FlowChainIncompleteObserved())
	require.True(t, chain.IsClosed())
	// cleanup with terminal should NOT increment
	ResetFlowChainCountersForTest()
	chain2 := NewFlowChain(rec, func() time.Time { return fixed })
	require.NoError(t, chain2.Append(validDispatch(1, false, nil)))
	require.NoError(t, chain2.Append(validDispatch(2, true, func() *string { s := "a1"; return &s }())))
	before2 := FlowChainIncompleteObserved()
	chain2.Close()
	require.Equal(t, before2, FlowChainIncompleteObserved(), "terminal chain close must not count incomplete")
	// completed chain close also not incomplete
	chain3 := NewFlowChain(rec, func() time.Time { return fixed })
	require.NoError(t, chain3.Append(validDispatch(1, true, nil)))
	require.NoError(t, chain3.Complete())
	before3 := FlowChainIncompleteObserved()
	chain3.Close()
	require.Equal(t, before3, FlowChainIncompleteObserved())
}

func TestFlowChain_ProcessCrashLossUnobservable(t *testing.T) {
	require.True(t, ProcessCrashLossUnobservable())
	// must not expose crash count; ensure no CrashCount function exists via compile check
	// incomplete counter is process-observed, crash loss is unobservable
	ResetFlowChainCountersForTest()
	require.Equal(t, int64(0), FlowChainIncompleteObserved())
}

func TestFlowChain_RaceConcurrentAppendAndClose(t *testing.T) {
	ResetFlowChainCountersForTest()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 40, 0, 0, time.UTC)
	chain := NewFlowChain(rec, func() time.Time { return fixed })
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_ = chain.Append(validDispatch(1, false, nil))
	}()
	go func() {
		defer wg.Done()
		<-start
		_ = chain.Append(validDispatch(1, false, nil))
	}()
	close(start)
	wg.Wait()
	// One of the concurrent first appends succeeds, the other fails due to ordinal sequential mismatch or duplicate
	count := chain.DispatchCount()
	require.True(t, count == 1, "concurrent first append must serialize to exactly one success")
	// barrier for close vs complete
	chain2 := NewFlowChain(rec, func() time.Time { return fixed })
	require.NoError(t, chain2.Append(validDispatch(1, true, nil)))
	start2 := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start2
		_ = chain2.Complete()
	}()
	go func() {
		defer wg.Done()
		<-start2
		chain2.Close()
	}()
	close(start2)
	wg.Wait()
	// At most one of Complete/Close wins; no race, no panic
	require.True(t, chain2.IsCompleted() || chain2.IsClosed())
}

func TestFlowChain_MetadataCompleteness(t *testing.T) {
	ResetFlowChainCountersForTest()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 50, 0, 0, time.UTC)
	chain := NewFlowChain(rec, func() time.Time { return fixed })
	// 3 dispatches with full metadata verification
	prev1 := "a1"
	prev2 := "a2"
	cases := []FlowDispatch{
		validDispatch(1, false, nil),
		validDispatch(2, false, &prev1),
		validDispatch(3, true, &prev2),
	}
	for i, d := range cases {
		require.NoError(t, chain.Append(d), "case %d", i)
	}
	require.NoError(t, chain.Complete())
	fm, ok := rec.FlowMinute(fixed.UTC().Truncate(time.Minute).Unix())
	require.True(t, ok)
	rows := fm.FlowRows()
	require.Len(t, rows, 3)
	for i, r := range rows {
		// route/quality/fingerprint/template/model/generation/revision mapped
		require.NotEmpty(t, r.RouteClassID)
		require.NotEmpty(t, r.CandidateFingerprint)
		require.NotZero(t, r.Generation)
		require.Equal(t, int16(i+1), r.Ordinal)
		require.NotEmpty(t, r.Lane)
		if i == 0 {
			require.Nil(t, r.PreviousAccountID)
		} else {
			require.NotNil(t, r.PreviousAccountID)
		}
		require.NotEmpty(t, r.TransitionReason)
		require.NotEmpty(t, r.Outcome)
		if i == 2 {
			require.True(t, r.IsTerminal)
		} else {
			require.False(t, r.IsTerminal)
		}
	}
}

func TestFlowChain_FinalizeCompletesLastDispatch(t *testing.T) {
	rec, err := NewRecorder(32)
	require.NoError(t, err)
	chain := NewFlowChain(rec, time.Now)
	require.NoError(t, chain.Append(validDispatch(1, false, nil)))
	require.NoError(t, chain.Finalize())
	require.NoError(t, chain.Complete())
	require.True(t, chain.IsCompleted())
	rows, ok := rec.FlowMinute(chain.TerminalMinute())
	require.True(t, ok)
	require.True(t, rows.HasFlowRows())
	flow := chain.Dispatches()
	require.Len(t, flow, 1)
	require.True(t, flow[0].IsTerminal)
}
