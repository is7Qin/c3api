// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

// Completion-walk counting rows (v3 §5.1 table, proxy side): the settle maps
// every old lifecycle row — cap-overflow, duplicate completion, force-mark
// finalization, single bucket mint, close-after-complete, close with/without
// terminal, panic-cancel — onto the fold with exact counts. Table-full lives
// in the quality counter-fidelity test (same walk, shard-scoped). Direct
// foldOwner unit tests: no HTTP, fixed attempts, barrier-free.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/quality"
	"github.com/is7qin/c3api/internal/scheduler"
)

func foldCompletionAttempt(ordinal uint8, prevAcct *int64) scheduler.Attempt {
	var prevID *string
	if ordinal > 1 {
		s := "req-x:1"
		prevID = &s
	}
	return scheduler.Attempt{
		AttemptID:            "req-x:2",
		RouteClassID:         strings.Repeat("a", 64),
		QualityClassID:       strings.Repeat("b", 64),
		CandidateFingerprint: strings.Repeat("c", 64),
		TemplateID:           3,
		AccountID:            9,
		RequestedModel:       "gpt-4o",
		MappedModel:          "gpt-4o",
		Lane:                 scheduler.AttemptLanePrimary,
		Ordinal:              ordinal,
		RoutingGeneration:    5,
		IdentityRevision:     4,
		PreviousAttemptID:    prevID,
		PreviousAccountID:    prevAcct,
		CallerCategory:       "chat",
		OperationTag:         "chat_completions",
	}
}

func foldCompletionOutcome(token string, terminal bool) AttemptOutcome {
	o := AttemptOutcome{Terminal: terminal}
	switch token {
	case "success":
		o.Result = ResultSuccess
		o.HTTPStatus = 200
		o.Commit = CommitClientCommitted
		o.BusinessFrameSent = true
	case "429":
		o.Result = ResultFailed
		o.HTTPStatus = 429
		o.Commit = CommitUpstreamResponded
	case "client_cancel":
		o.Result = ResultClientCancel
		o.Commit = CommitNotSent
	}
	return o
}

func foldCompletionRecorder(t *testing.T) (*quality.Recorder, *foldOwner) {
	t.Helper()
	quality.ResetFlowChainCountersForTest()
	rec, err := quality.NewRecorder(50000)
	require.NoError(t, err)
	return rec, newFoldOwner(rec, nil)
}

// TestFoldCompletion_SettleEmitsSingleTerminalEdge pins the byte-identical
// request-path row: ordinal-1 success emits one terminal edge with the
// canonical initial transition.
func TestFoldCompletion_SettleEmitsSingleTerminalEdge(t *testing.T) {
	rec, f := foldCompletionRecorder(t)
	f.arm(foldCompletionAttempt(1, nil))
	f.append(foldCompletionOutcome("success", true))
	f.settle(false)

	rows := collectFlowRows(rec)
	require.Len(t, rows, 1)
	require.EqualValues(t, 1, rows[0].Ordinal)
	require.Equal(t, "success", rows[0].Outcome)
	require.Equal(t, "initial", rows[0].TransitionReason)
	require.True(t, rows[0].IsTerminal)
	require.Equal(t, int64(1), rows[0].ChainCount)
	require.Nil(t, rows[0].PreviousAccountID)
	require.Zero(t, quality.FlowChainIncompleteObserved())
	require.Zero(t, quality.FlowChainEnqueueOverflow())
}

// TestFoldCompletion_SettleForceMarksLastEdgeTerminal is the Finalize row:
// terminality is final at the completion walk (option (a)) — the last edge
// flips, earlier edges keep theirs, every fact counted once.
func TestFoldCompletion_SettleForceMarksLastEdgeTerminal(t *testing.T) {
	rec, f := foldCompletionRecorder(t)
	f.arm(foldCompletionAttempt(1, nil))
	f.append(foldCompletionOutcome("429", false))
	f.arm(foldCompletionAttempt(2, nil))
	f.append(foldCompletionOutcome("429", false))
	f.settle(false)

	rows := collectFlowRows(rec)
	require.Len(t, rows, 2)
	require.False(t, rows[0].IsTerminal)
	require.True(t, rows[1].IsTerminal, "exhaustion force-marks the last edge")
	require.Equal(t, int64(1), rows[0].ChainCount)
	require.Equal(t, int64(1), rows[1].ChainCount)
	require.Zero(t, quality.FlowChainIncompleteObserved())
}

// TestFoldCompletion_SettleMintsOneBucketForAllFacts is the minuteBucket
// half of the Finalize row: every fact of one chain carries the identical
// bucket, so all cells expand under one TerminalMinute.
func TestFoldCompletion_SettleMintsOneBucketForAllFacts(t *testing.T) {
	rec, f := foldCompletionRecorder(t)
	for i, terminal := range []bool{false, false, true} {
		f.arm(foldCompletionAttempt(uint8(i+1), nil))
		f.append(foldCompletionOutcome("success", terminal))
	}
	f.settle(false)

	rows := collectFlowRows(rec)
	require.Len(t, rows, 3)
	require.Equal(t, rows[0].TerminalMinute, rows[1].TerminalMinute)
	require.Equal(t, rows[1].TerminalMinute, rows[2].TerminalMinute)
	require.False(t, rows[0].TerminalMinute.IsZero())
}

// TestFoldCompletion_DuplicateSettleEmitsOnce is the duplicate-Complete row:
// the walk runs once (guard flag); the second settle errors nothing, emits
// nothing, counts nothing.
func TestFoldCompletion_DuplicateSettleEmitsOnce(t *testing.T) {
	rec, f := foldCompletionRecorder(t)
	f.arm(foldCompletionAttempt(1, nil))
	f.append(foldCompletionOutcome("success", true))
	f.settle(false)
	f.settle(false)

	rows := collectFlowRows(rec)
	require.Len(t, rows, 1, "duplicate settle must not re-emit")
	require.Equal(t, int64(1), rows[0].ChainCount)
	require.Zero(t, quality.FlowChainIncompleteObserved())
}

// TestFoldCompletion_SettleEmptyArmedCountsIncomplete is the Close row for an
// armed-but-empty chain (abandon): submit-nothing preserved, incomplete +1.
func TestFoldCompletion_SettleEmptyArmedCountsIncomplete(t *testing.T) {
	rec, f := foldCompletionRecorder(t)
	f.arm(foldCompletionAttempt(1, nil))
	f.settle(false)

	require.Empty(t, collectFlowRows(rec), "close submits nothing")
	require.Equal(t, int64(1), quality.FlowChainIncompleteObserved())
}

// TestFoldCompletion_SettleNeverArmedCountsNothing: a request that never
// began a dispatch records no flow and no incomplete count.
func TestFoldCompletion_SettleNeverArmedCountsNothing(t *testing.T) {
	rec, f := foldCompletionRecorder(t)
	f.settle(false)

	require.Empty(t, collectFlowRows(rec))
	require.Zero(t, quality.FlowChainIncompleteObserved())
}

// TestFoldCompletion_SettlePanickedTerminalCountsNothing is the Close row
// for a panicked terminal chain: no rows, no incomplete bump.
func TestFoldCompletion_SettlePanickedTerminalCountsNothing(t *testing.T) {
	rec, f := foldCompletionRecorder(t)
	f.arm(foldCompletionAttempt(1, nil))
	f.append(foldCompletionOutcome("success", true))
	f.settle(true)

	require.Empty(t, collectFlowRows(rec), "panic submits nothing")
	require.Zero(t, quality.FlowChainIncompleteObserved())
}

// TestFoldCompletion_SettlePanickedNonterminalCountsIncomplete is the
// panic-Cancel row (deferred Close): no rows, incomplete +1.
func TestFoldCompletion_SettlePanickedNonterminalCountsIncomplete(t *testing.T) {
	rec, f := foldCompletionRecorder(t)
	f.arm(foldCompletionAttempt(1, nil))
	f.append(foldCompletionOutcome("429", false))
	f.settle(true)

	require.Empty(t, collectFlowRows(rec), "panic submits nothing")
	require.Equal(t, int64(1), quality.FlowChainIncompleteObserved())
}

// TestFoldCompletion_AppendBeyondCapCountsOverflow is the cap-overflow row:
// attempt 9+ counts cap-overflow with zero cell adds and never rewrites the
// last real edge.
func TestFoldCompletion_AppendBeyondCapCountsOverflow(t *testing.T) {
	rec, f := foldCompletionRecorder(t)
	for i := uint8(1); i <= 9; i++ {
		ordinal := i
		if ordinal > 8 {
			ordinal = 8
		}
		f.arm(foldCompletionAttempt(ordinal, nil))
		f.append(foldCompletionOutcome("success", i == 8))
	}
	f.settle(false)

	rows := collectFlowRows(rec)
	require.Len(t, rows, 8, "the 9th attempt never reaches the cells")
	require.Equal(t, int64(1), quality.FlowChainCapacityOverflow())
}

// TestFoldCompletion_ClientCancelVerbatim pins the Amendment-A1 outcome
// codebook: the cancel edge keeps its census string verbatim (the old tree
// writes client_cancel; folding it into error would corrupt PG/dashboards).
// Flow-only-ness and conservation are unchanged.
func TestFoldCompletion_ClientCancelVerbatim(t *testing.T) {
	rec, f := foldCompletionRecorder(t)
	f.arm(foldCompletionAttempt(1, nil))
	f.append(foldCompletionOutcome("client_cancel", true))
	f.settle(false)

	rows := collectFlowRows(rec)
	require.Len(t, rows, 1, "client cancel stays flow-only and conserved")
	require.Equal(t, "client_cancel", rows[0].Outcome)
	require.True(t, rows[0].IsTerminal)
}

// TestFoldCompletion_SettleConservesPreviousOutcomeLinkage pins the real
// recorded transition: the second edge carries the first edge's outcome.
func TestFoldCompletion_SettleConservesPreviousOutcomeLinkage(t *testing.T) {
	rec, f := foldCompletionRecorder(t)
	prevAcct := int64(9)
	f.arm(foldCompletionAttempt(1, nil))
	f.append(foldCompletionOutcome("429", false))
	f.arm(foldCompletionAttempt(2, &prevAcct))
	f.append(foldCompletionOutcome("success", true))
	f.settle(false)

	rows := collectFlowRows(rec)
	require.Len(t, rows, 2)
	require.Equal(t, "failover", rows[1].TransitionReason)
	require.Equal(t, "429", rows[1].PreviousOutcome)
	require.NotNil(t, rows[1].PreviousAccountID)
	require.Equal(t, prevAcct, *rows[1].PreviousAccountID)
}
