// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func baseOutcome() AttemptOutcome {
	return AttemptOutcome{
		ID: "attempt-1", RouteClassID: "rc1", Fingerprint: "fp1",
		LifecycleRevision: 1, Lane: LanePrimary, Generation: 1,
		Timing: AttemptTiming{LatencyMS: 10}, Usage: AttemptUsage{},
	}
}

func TestRetryMatrix(t *testing.T) {
	cats := AllCallerCategories()
	require.Len(t, cats, 10)

	type row struct {
		name   string
		cat    CallerCategory
		mutate func(o *AttemptOutcome)
		expect bool
	}
	// Explicit independent expectations, not derived via CanRetry.
	// Covers 10 callers x ordinary/hard, REST hard, WS ordinary, pre/post business frame, invalid/terminal.
	rows := []row{
		// not_sent ordinary true for all
		{"chat not_sent ordinary", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 0; o.BusinessFrameSent = false; o.HardContinuation = false }, true},
		{"responses not_sent ordinary", CallerResponses, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 0; o.BusinessFrameSent = false }, true},
		// 429 ordinary pre-response true
		{"chat 429 ordinary pre-frame true", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 429; o.BusinessFrameSent = false; o.HardContinuation = false }, true},
		{"responses_ws 429 ordinary pre-frame true", CallerResponsesWS, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 429; o.BusinessFrameSent = false; o.HardContinuation = false }, true},
		{"search 429 ordinary true", CallerSearch, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 429; o.BusinessFrameSent = false; o.HardContinuation = false }, true},
		// 429 hard continuation false (REST hard)
		{"chat 429 hard REST false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 429; o.BusinessFrameSent = false; o.HardContinuation = true }, false},
		{"responses 429 hard false", CallerResponses, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 429; o.BusinessFrameSent = false; o.HardContinuation = true }, false},
		// 429 hard WS false
		{"responses_ws 429 hard false", CallerResponsesWS, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 429; o.BusinessFrameSent = false; o.HardContinuation = true }, false},
		{"codex_ws 429 hard false", CallerCodexWS, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 429; o.BusinessFrameSent = false; o.HardContinuation = true }, false},
		// 429 with business frame sent false even ordinary
		{"chat 429 ordinary but BusinessFrameSent true false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 429; o.BusinessFrameSent = true; o.HardContinuation = false }, false},
		// 5xx all false
		{"chat 5xx false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 500; o.BusinessFrameSent = false }, false},
		{"responses_ws 5xx false", CallerResponsesWS, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 500 }, false},
		{"search 5xx false", CallerSearch, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 503 }, false},
		// sent_ambiguous false
		{"chat sent_ambiguous false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitSentAmbiguous; o.Result = ResultFailed; o.HTTPStatus = 0; o.BusinessFrameSent = true }, false},
		// response_started false
		{"chat response_started false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitResponseStarted; o.Result = ResultFailed; o.HTTPStatus = 500; o.BusinessFrameSent = true }, false},
		// client_committed false
		{"chat client_committed false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitClientCommitted; o.Result = ResultFailed; o.HTTPStatus = 200; o.BusinessFrameSent = true }, false},
		// client_cancel false
		{"chat client_cancel false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultClientCancel; o.HTTPStatus = 0 }, false},
		// malformed false
		{"chat malformed false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 400; o.IsMalformed = true; o.BusinessFrameSent = false }, false},
		// terminal not_sent false (invalid)
		{"chat terminal not_sent false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 0; o.BusinessFrameSent = false; o.Terminal = true }, false},
		// local reject false
		{"chat local reject false", CallerChat, func(o *AttemptOutcome) { *o = AttemptOutcome{ID: "a1", Commit: CommitNotSent, Result: ResultLocalReject, HTTPStatus: 0} }, false},
		{"chat reservation reject false", CallerChat, func(o *AttemptOutcome) { *o = AttemptOutcome{ID: "a1", Commit: CommitNotSent, Result: ResultReservationReject, HTTPStatus: 0} }, false},
		// unknown false
		{"chat unknown false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultUnknown; o.HTTPStatus = 0 }, false},
		// WS not_sent after business frame false (invalid)
		{"responses_ws not_sent after business frame false", CallerResponsesWS, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 0; o.BusinessFrameSent = true }, false},
		// invalid lane/commit false
		{"chat invalid lane false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 0; o.Lane = LaneID("bad") }, false},
		{"chat invalid commit false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitState(99); o.Result = ResultFailed }, false},
		// WS ordinary vs hard matrix for all 10 callers explicit 429
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			o := baseOutcome()
			r.mutate(&o)
			got := CanRetry(r.cat, o)
			require.Equal(t, r.expect, got, "cat=%s hard=%v commit=%v status=%d business=%v terminal=%v result=%v malformed=%v", r.cat, o.HardContinuation, o.Commit, o.HTTPStatus, o.BusinessFrameSent, o.Terminal, o.Result, o.IsMalformed)
		})
	}
	// Exhaustive 10 callers x ordinary/hard for 429 pre-frame
	t.Run("10callers ordinary 429 true hard 429 false", func(t *testing.T) {
		for _, cat := range cats {
			oOrd := baseOutcome()
			oOrd.Commit = CommitNotSent; oOrd.Result = ResultFailed; oOrd.HTTPStatus = 429; oOrd.BusinessFrameSent = false; oOrd.HardContinuation = false
			require.True(t, CanRetry(cat, oOrd), "ordinary 429 must be true for %s", cat)
			oHard := baseOutcome()
			oHard.Commit = CommitNotSent; oHard.Result = ResultFailed; oHard.HTTPStatus = 429; oHard.BusinessFrameSent = false; oHard.HardContinuation = true
			require.False(t, CanRetry(cat, oHard), "hard 429 must be false for %s", cat)
		}
	})
	t.Run("all 5xx false", func(t *testing.T) {
		for _, cat := range cats {
			o := baseOutcome()
			o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 500; o.BusinessFrameSent = false
			require.False(t, CanRetry(cat, o))
		}
	})
	t.Run("all not_sent true when valid", func(t *testing.T) {
		for _, cat := range cats {
			o := baseOutcome()
			o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 0; o.BusinessFrameSent = false
			require.True(t, CanRetry(cat, o))
		}
	})
	t.Run("no bytes possibly written mislabeled not_sent", func(t *testing.T) {
		for _, cat := range cats {
			o := baseOutcome()
			o.Commit = CommitNotSent; o.BusinessFrameSent = true; o.Result = ResultFailed
			require.Error(t, o.Validate())
			require.False(t, CanRetry(cat, o))
		}
	})
}

// Regression suite matching verifier examples
func TestRetryMatrix_Regressions(t *testing.T) {
	base := baseOutcome
	t.Run("hard REST 429 false", func(t *testing.T) {
		o := base(); o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 429; o.HardContinuation = true; o.BusinessFrameSent = false
		require.False(t, CanRetry(CallerChat, o))
		require.False(t, CanRetry(CallerResponses, o))
	})
	t.Run("ordinary WS pre-business-frame 429 true", func(t *testing.T) {
		o := base(); o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 429; o.HardContinuation = false; o.BusinessFrameSent = false
		require.True(t, CanRetry(CallerResponsesWS, o))
		require.True(t, CanRetry(CallerCodexWS, o))
	})
	t.Run("hard WS 429 false", func(t *testing.T) {
		o := base(); o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 429; o.HardContinuation = true; o.BusinessFrameSent = false
		require.False(t, CanRetry(CallerResponsesWS, o))
		require.False(t, CanRetry(CallerCodexWS, o))
	})
	t.Run("terminal not_sent false", func(t *testing.T) {
		o := base(); o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 0; o.Terminal = true; o.BusinessFrameSent = false
		require.False(t, CanRetry(CallerChat, o))
		require.Error(t, o.Validate())
	})
	t.Run("local reject status0 false", func(t *testing.T) {
		o := AttemptOutcome{ID: "a1", Commit: CommitNotSent, Result: ResultLocalReject, HTTPStatus: 0}
		require.False(t, CanRetry(CallerChat, o))
	})
	t.Run("reservation reject false", func(t *testing.T) {
		o := AttemptOutcome{ID: "a1", Commit: CommitNotSent, Result: ResultReservationReject, HTTPStatus: 0}
		require.False(t, CanRetry(CallerChat, o))
	})
	t.Run("unknown false", func(t *testing.T) {
		o := base(); o.Result = ResultUnknown
		require.False(t, CanRetry(CallerChat, o))
		require.Error(t, o.Validate())
	})
	t.Run("WS not_sent after business frame false", func(t *testing.T) {
		o := base(); o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 0; o.BusinessFrameSent = true
		require.False(t, CanRetry(CallerResponsesWS, o))
		require.Error(t, o.Validate())
	})
}
