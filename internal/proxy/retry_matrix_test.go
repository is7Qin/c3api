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
		Terminal: false, BusinessFrameSent: false, HardContinuation: false,
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
	rows := []row{
		{"chat not_sent network true", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 0; o.BusinessFrameSent = false; o.Terminal = false; o.HardContinuation = false }, true},
		{"responses not_sent network true", CallerResponses, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 0; o.BusinessFrameSent = false; o.Terminal = false }, true},
		{"search not_sent network true", CallerSearch, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 0; o.BusinessFrameSent = false; o.Terminal = false }, true},
		{"chat 429 ordinary upstream_responded true", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitUpstreamResponded; o.Result = ResultFailed; o.HTTPStatus = 429; o.BusinessFrameSent = false; o.HardContinuation = false; o.Terminal = false }, true},
		{"responses_ws 429 ordinary pre-frame true", CallerResponsesWS, func(o *AttemptOutcome) { o.Commit = CommitUpstreamResponded; o.Result = ResultFailed; o.HTTPStatus = 429; o.BusinessFrameSent = false; o.HardContinuation = false; o.Terminal = false }, true},
		{"codex_ws 429 ordinary true", CallerCodexWS, func(o *AttemptOutcome) { o.Commit = CommitUpstreamResponded; o.Result = ResultFailed; o.HTTPStatus = 429; o.BusinessFrameSent = false; o.HardContinuation = false; o.Terminal = false }, true},
		{"chat 429 hard REST false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitUpstreamResponded; o.Result = ResultFailed; o.HTTPStatus = 429; o.BusinessFrameSent = false; o.HardContinuation = true; o.Terminal = true }, false},
		{"responses 429 hard false", CallerResponses, func(o *AttemptOutcome) { o.Commit = CommitUpstreamResponded; o.Result = ResultFailed; o.HTTPStatus = 429; o.BusinessFrameSent = false; o.HardContinuation = true; o.Terminal = true }, false},
		{"responses_ws 429 hard false", CallerResponsesWS, func(o *AttemptOutcome) { o.Commit = CommitUpstreamResponded; o.Result = ResultFailed; o.HTTPStatus = 429; o.BusinessFrameSent = false; o.HardContinuation = true; o.Terminal = true }, false},
		{"codex_ws 429 hard false", CallerCodexWS, func(o *AttemptOutcome) { o.Commit = CommitUpstreamResponded; o.Result = ResultFailed; o.HTTPStatus = 429; o.BusinessFrameSent = false; o.HardContinuation = true; o.Terminal = true }, false},
		{"chat 429 wrong not_sent invalid false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 429; o.BusinessFrameSent = false; o.Terminal = false }, false},
		{"chat 4xx ordinary terminal false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitUpstreamResponded; o.Result = ResultFailed; o.HTTPStatus = 400; o.BusinessFrameSent = false; o.Terminal = true }, false},
		{"chat 5xx false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitUpstreamResponded; o.Result = ResultFailed; o.HTTPStatus = 500; o.Terminal = true }, false},
		{"responses_ws 5xx false", CallerResponsesWS, func(o *AttemptOutcome) { o.Commit = CommitUpstreamResponded; o.Result = ResultFailed; o.HTTPStatus = 503; o.Terminal = true }, false},
		{"search 5xx false", CallerSearch, func(o *AttemptOutcome) { o.Commit = CommitUpstreamResponded; o.Result = ResultFailed; o.HTTPStatus = 599; o.Terminal = true }, false},
		{"chat sent_ambiguous false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitSentAmbiguous; o.Result = ResultFailed; o.HTTPStatus = 0; o.BusinessFrameSent = true; o.Terminal = true }, false},
		{"chat response_started false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitResponseStarted; o.Result = ResultFailed; o.HTTPStatus = 500; o.BusinessFrameSent = true; o.Terminal = true }, false},
		{"chat client_committed false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitClientCommitted; o.Result = ResultFailed; o.HTTPStatus = 200; o.BusinessFrameSent = true; o.Terminal = true }, false},
		{"chat client_cancel not_sent false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultClientCancel; o.HTTPStatus = 0; o.BusinessFrameSent = false; o.Terminal = true }, false},
		{"chat client_cancel response_started false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitResponseStarted; o.Result = ResultClientCancel; o.HTTPStatus = 0; o.BusinessFrameSent = true; o.Terminal = true }, false},
		{"chat malformed upstream_responded false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitUpstreamResponded; o.Result = ResultFailed; o.HTTPStatus = 400; o.IsMalformed = true; o.Terminal = true }, false},
		{"chat malformed response_started false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitResponseStarted; o.Result = ResultFailed; o.HTTPStatus = 0; o.IsMalformed = true; o.BusinessFrameSent = true; o.Terminal = true }, false},
		{"chat terminal not_sent invalid false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 0; o.BusinessFrameSent = false; o.Terminal = true }, false},
		{"chat local reject false", CallerChat, func(o *AttemptOutcome) { *o = AttemptOutcome{ID: "a1", Commit: CommitNotSent, Result: ResultLocalReject, HTTPStatus: 0, Terminal: false} }, false},
		{"chat reservation reject false", CallerChat, func(o *AttemptOutcome) { *o = AttemptOutcome{ID: "a1", Commit: CommitNotSent, Result: ResultReservationReject, HTTPStatus: 0} }, false},
		{"chat unknown invalid false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultUnknown; o.HTTPStatus = 0 }, false},
		{"responses_ws not_sent after business frame invalid false", CallerResponsesWS, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 0; o.BusinessFrameSent = true }, false},
		{"chat 1xx invalid false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitUpstreamResponded; o.Result = ResultFailed; o.HTTPStatus = 100; o.Terminal = true }, false},
		{"chat 3xx invalid false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitUpstreamResponded; o.Result = ResultFailed; o.HTTPStatus = 301; o.Terminal = true }, false},
		{"chat >599 invalid false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitUpstreamResponded; o.Result = ResultFailed; o.HTTPStatus = 600; o.Terminal = true }, false},
		{"chat success ambiguous invalid false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitSentAmbiguous; o.Result = ResultSuccess; o.HTTPStatus = 200; o.BusinessFrameSent = true; o.Terminal = true }, false},
		{"chat success upstream_responded invalid false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitUpstreamResponded; o.Result = ResultSuccess; o.HTTPStatus = 200; o.Terminal = true }, false},
		{"chat zero generation invalid false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 0; o.Generation = 0; o.BusinessFrameSent = false; o.Terminal = false }, false},
		{"chat zero revision invalid false", CallerChat, func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 0; o.LifecycleRevision = 0; o.Terminal = false }, false},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			o := baseOutcome()
			r.mutate(&o)
			got := CanRetry(r.cat, o)
			require.Equal(t, r.expect, got, "cat=%s hard=%v commit=%v status=%d business=%v terminal=%v result=%v malformed=%v err=%v", r.cat, o.HardContinuation, o.Commit, o.HTTPStatus, o.BusinessFrameSent, o.Terminal, o.Result, o.IsMalformed, o.Validate())
		})
	}
	t.Run("10callers ordinary 429 true hard 429 false", func(t *testing.T) {
		for _, cat := range cats {
			oOrd := baseOutcome()
			oOrd.Commit = CommitUpstreamResponded; oOrd.Result = ResultFailed; oOrd.HTTPStatus = 429; oOrd.BusinessFrameSent = false; oOrd.HardContinuation = false; oOrd.Terminal = false
			require.True(t, CanRetry(cat, oOrd), "ordinary 429 must be true for %s", cat)
			oHard := baseOutcome()
			oHard.Commit = CommitUpstreamResponded; oHard.Result = ResultFailed; oHard.HTTPStatus = 429; oHard.BusinessFrameSent = false; oHard.HardContinuation = true; oHard.Terminal = true
			require.False(t, CanRetry(cat, oHard), "hard 429 must be false for %s", cat)
		}
	})
	t.Run("all 5xx false", func(t *testing.T) {
		for _, cat := range cats {
			o := baseOutcome()
			o.Commit = CommitUpstreamResponded; o.Result = ResultFailed; o.HTTPStatus = 500; o.Terminal = true
			require.False(t, CanRetry(cat, o))
		}
	})
	t.Run("all not_sent true when valid", func(t *testing.T) {
		for _, cat := range cats {
			o := baseOutcome()
			o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 0; o.BusinessFrameSent = false; o.Terminal = false
			require.True(t, CanRetry(cat, o))
		}
	})
}

func TestRetryMatrix_Regressions(t *testing.T) {
	base := baseOutcome
	t.Run("hard REST 429 false", func(t *testing.T) {
		o := base(); o.Commit = CommitUpstreamResponded; o.Result = ResultFailed; o.HTTPStatus = 429; o.HardContinuation = true; o.BusinessFrameSent = false; o.Terminal = true
		require.False(t, CanRetry(CallerChat, o))
	})
	t.Run("ordinary WS pre-business-frame 429 true", func(t *testing.T) {
		o := base(); o.Commit = CommitUpstreamResponded; o.Result = ResultFailed; o.HTTPStatus = 429; o.HardContinuation = false; o.BusinessFrameSent = false; o.Terminal = false
		require.True(t, CanRetry(CallerResponsesWS, o))
	})
	t.Run("hard WS 429 false", func(t *testing.T) {
		o := base(); o.Commit = CommitUpstreamResponded; o.Result = ResultFailed; o.HTTPStatus = 429; o.HardContinuation = true; o.BusinessFrameSent = false; o.Terminal = true
		require.False(t, CanRetry(CallerResponsesWS, o))
	})
	t.Run("terminal not_sent false", func(t *testing.T) {
		o := base(); o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 0; o.Terminal = true
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
	t.Run("429 using wrong not_sent false", func(t *testing.T) {
		o := base(); o.Commit = CommitNotSent; o.Result = ResultFailed; o.HTTPStatus = 429; o.Terminal = false
		require.False(t, CanRetry(CallerChat, o))
		require.Error(t, o.Validate())
	})
}
