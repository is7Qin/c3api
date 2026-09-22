// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func validBase() AttemptOutcome {
	return AttemptOutcome{
		ID: "a1", RouteClassID: "rc", QualityClassID: "qc1", Fingerprint: "fp", TemplateID: 1, AccountID: 1,
		RequestedModel: "gpt-4o", MappedModel: "gpt-4o", CallerCategory: CallerChat, OperationTag: "chat_completions", Ordinal: 1,
		IdentityRevision: 1, Lane: LanePrimary, Generation: 1, Commit: CommitResponseStarted, Result: ResultSuccess, HTTPStatus: 200,
		BusinessFrameSent: true, HardContinuation: false, Terminal: true,
		Timing: AttemptTiming{LatencyMS: 10}, Usage: AttemptUsage{InputTokens: 1},
	}
}

func dispatchedFailedBase(status AttemptStatus, commit CommitState, terminal bool) AttemptOutcome {
	o := validBase()
	o.Result = ResultFailed
	o.HTTPStatus = status
	o.Commit = commit
	o.Terminal = terminal
	o.BusinessFrameSent = commit == CommitResponseStarted || commit == CommitClientCommitted || commit == CommitSentAmbiguous
	if commit == CommitUpstreamResponded {
		o.BusinessFrameSent = false
	}
	if commit == CommitNotSent {
		o.BusinessFrameSent = false
	}
	return o
}

func TestAttemptOutcomeContract(t *testing.T) {
	t.Run("valid success response_started", func(t *testing.T) {
		o := validBase()
		require.NoError(t, o.Validate())
		require.True(t, o.IsDispatched())
		require.True(t, o.IsCountedForQuality())
		require.False(t, o.IsFailed())
		require.True(t, o.HasPossiblyWrittenBytes())
	})
	t.Run("valid success client_committed", func(t *testing.T) {
		o := validBase()
		o.Commit = CommitClientCommitted
		require.NoError(t, o.Validate())
	})
	t.Run("rate_limited upstream_responded ordinary not terminal", func(t *testing.T) {
		o := dispatchedFailedBase(429, CommitUpstreamResponded, false)
		require.NoError(t, o.Validate())
		require.True(t, CanRetry(CallerChat, o))
	})
	t.Run("rate_limited hard must be terminal", func(t *testing.T) {
		o := dispatchedFailedBase(429, CommitUpstreamResponded, true)
		o.HardContinuation = true
		require.NoError(t, o.Validate())
		o2 := o
		o2.Terminal = false
		require.Error(t, o2.Validate())
	})
	t.Run("ordinary4xx upstream_responded terminal", func(t *testing.T) {
		o := dispatchedFailedBase(400, CommitUpstreamResponded, true)
		require.NoError(t, o.Validate())
		require.True(t, o.IsCountedForQuality())
		require.True(t, o.IsFailed())
		require.False(t, CanRetry(CallerChat, o))
	})
	t.Run("upstream5xx upstream_responded terminal", func(t *testing.T) {
		o := dispatchedFailedBase(500, CommitUpstreamResponded, true)
		require.NoError(t, o.Validate())
		require.False(t, CanRetry(CallerChat, o))
	})
	t.Run("network not_sent and sent_ambiguous", func(t *testing.T) {
		o := dispatchedFailedBase(0, CommitNotSent, false)
		require.NoError(t, o.Validate())
		require.True(t, CanRetry(CallerChat, o))
		o2 := dispatchedFailedBase(0, CommitSentAmbiguous, true)
		o2.BusinessFrameSent = true
		require.NoError(t, o2.Validate())
		require.False(t, CanRetry(CallerChat, o2))
	})
	t.Run("malformed upstream_responded terminal never retry", func(t *testing.T) {
		o := dispatchedFailedBase(400, CommitUpstreamResponded, true)
		o.IsMalformed = true
		require.NoError(t, o.Validate())
		require.False(t, CanRetry(CallerChat, o))
		o2 := dispatchedFailedBase(0, CommitUpstreamResponded, true)
		o2.IsMalformed = true
		o2.HTTPStatus = 0
		require.NoError(t, o2.Validate())
		require.False(t, CanRetry(CallerChat, o2))
	})
	t.Run("client_cancel flow only explicit states", func(t *testing.T) {
		for _, commit := range []CommitState{CommitNotSent, CommitSentAmbiguous, CommitResponseStarted, CommitClientCommitted} {
			o := validBase()
			o.Result = ResultClientCancel
			o.Commit = commit
			o.HTTPStatus = 0
			o.Terminal = true
			if commit == CommitNotSent {
				o.BusinessFrameSent = false
			} else {
				o.BusinessFrameSent = true
			}
			require.NoError(t, o.Validate(), "commit %v", commit)
			require.False(t, o.IsCountedForQuality())
			require.False(t, CanRetry(CallerChat, o))
		}
		o := validBase()
		o.Result = ResultClientCancel
		o.Commit = CommitUpstreamResponded
		o.HTTPStatus = 0
		o.Terminal = true
		require.Error(t, o.Validate())
	})
	t.Run("local and reservation reject non-dispatched", func(t *testing.T) {
		o := AttemptOutcome{ID: "a1", Commit: CommitNotSent, Result: ResultLocalReject, HTTPStatus: 0}
		require.NoError(t, o.Validate())
		require.False(t, o.IsDispatched())
		require.False(t, CanRetry(CallerChat, o))
		o2 := AttemptOutcome{ID: "a1", Commit: CommitNotSent, Result: ResultReservationReject, HTTPStatus: 0}
		require.NoError(t, o2.Validate())
		require.False(t, CanRetry(CallerChat, o2))
		o3 := AttemptOutcome{ID: "a1", Commit: CommitNotSent, Result: ResultLocalReject, HTTPStatus: 0, RouteClassID: "rc"}
		require.Error(t, o3.Validate())
	})
	t.Run("generation and revision must be >0", func(t *testing.T) {
		o := validBase()
		o.Generation = 0
		o.Result = ResultFailed
		o.Commit = CommitNotSent
		o.HTTPStatus = 0
		o.Terminal = false
		o.BusinessFrameSent = false
		require.Error(t, o.Validate())
		o = validBase()
		o.IdentityRevision = 0
		o.Result = ResultFailed
		o.Commit = CommitNotSent
		o.HTTPStatus = 0
		o.Terminal = false
		o.BusinessFrameSent = false
		require.Error(t, o.Validate())
		o = dispatchedFailedBase(0, CommitNotSent, false)
		require.NoError(t, o.Validate())
	})
	t.Run("negative 1xx 3xx >599 rejected", func(t *testing.T) {
		cases := []AttemptStatus{-1, 100, 199, 300, 301, 399, 600, 700}
		for _, s := range cases {
			o := dispatchedFailedBase(s, CommitUpstreamResponded, true)
			require.Error(t, o.Validate(), "status %d", s)
		}
	})
	t.Run("success plus ambiguous invalid", func(t *testing.T) {
		o := validBase()
		o.Commit = CommitSentAmbiguous
		o.BusinessFrameSent = true
		require.Error(t, o.Validate())
		o2 := validBase()
		o2.Commit = CommitUpstreamResponded
		o2.BusinessFrameSent = false
		require.Error(t, o2.Validate())
	})
	t.Run("429 using wrong not_sent invalid", func(t *testing.T) {
		o := dispatchedFailedBase(429, CommitNotSent, false)
		require.Error(t, o.Validate())
		require.False(t, CanRetry(CallerChat, o))
	})
	t.Run("zero generation revision for network not_sent invalid", func(t *testing.T) {
		o := validBase()
		o.Generation = 0
		o.IdentityRevision = 0
		o.Result = ResultFailed
		o.Commit = CommitNotSent
		o.HTTPStatus = 0
		o.Terminal = false
		o.BusinessFrameSent = false
		require.Error(t, o.Validate())
	})
	t.Run("valid metadata fixture for all ten callers", func(t *testing.T) {
		for _, cat := range AllCallerCategories() {
			o := dispatchedFailedBase(0, CommitNotSent, false)
			o.CallerCategory = cat
			if cat == CallerConverted {
				o.OperationTag = "chat_completions"
			}
			require.NoError(t, o.Validate(), "caller %s", cat)
			require.True(t, CanRetry(cat, o))
		}
	})
	t.Run("bytes written cannot be not_sent", func(t *testing.T) {
		o := validBase()
		o.Commit = CommitNotSent
		o.BusinessFrameSent = true
		require.Error(t, o.Validate())
	})
	t.Run("terminal not_sent invalid", func(t *testing.T) {
		o := dispatchedFailedBase(0, CommitNotSent, true)
		require.Error(t, o.Validate())
	})
	t.Run("dispatch metadata required", func(t *testing.T) {
		o := validBase()
		o.QualityClassID = ""
		require.Error(t, o.Validate())
		o = validBase()
		o.TemplateID = 0
		require.Error(t, o.Validate())
		o = validBase()
		o.AccountID = 0
		require.Error(t, o.Validate())
		o = validBase()
		o.RequestedModel = ""
		require.Error(t, o.Validate())
		o = validBase()
		o.MappedModel = ""
		require.Error(t, o.Validate())
		o = validBase()
		o.CallerCategory = ""
		require.Error(t, o.Validate())
		o = validBase()
		o.OperationTag = ""
		require.Error(t, o.Validate())
		o = validBase()
		o.Ordinal = 0
		require.Error(t, o.Validate())
		o = validBase()
		o.Ordinal = 1
		prev := AttemptID("prev")
		o.PreviousAttemptID = &prev
		require.Error(t, o.Validate())
		o = validBase()
		o.Ordinal = 2
		o.PreviousAttemptID = nil
		require.Error(t, o.Validate())
	})
}
