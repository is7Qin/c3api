// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func validBase() AttemptOutcome {
	return AttemptOutcome{
		ID: "a1", RouteClassID: "rc", Fingerprint: "fp", LifecycleRevision: 1,
		Lane: LanePrimary, Generation: 1, Commit: CommitResponseStarted, Result: ResultSuccess, HTTPStatus: 200,
		BusinessFrameSent: true, HardContinuation: false, Terminal: true,
		Timing: AttemptTiming{LatencyMS: 10}, Usage: AttemptUsage{InputTokens: 1},
	}
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
		o := AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 1, LifecycleRevision: 1, Commit: CommitUpstreamResponded, Result: ResultFailed, HTTPStatus: 429, Terminal: false, BusinessFrameSent: false}
		require.NoError(t, o.Validate())
		require.True(t, CanRetry(CallerChat, o))
	})
	t.Run("rate_limited hard must be terminal", func(t *testing.T) {
		o := AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 1, LifecycleRevision: 1, Commit: CommitUpstreamResponded, Result: ResultFailed, HTTPStatus: 429, HardContinuation: true, Terminal: true}
		require.NoError(t, o.Validate())
		o2 := o
		o2.Terminal = false
		require.Error(t, o2.Validate())
	})
	t.Run("ordinary4xx upstream_responded terminal", func(t *testing.T) {
		o := AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 1, LifecycleRevision: 1, Commit: CommitUpstreamResponded, Result: ResultFailed, HTTPStatus: 400, Terminal: true}
		require.NoError(t, o.Validate())
		require.True(t, o.IsCountedForQuality())
		require.True(t, o.IsFailed())
		require.False(t, CanRetry(CallerChat, o))
	})
	t.Run("upstream5xx upstream_responded terminal", func(t *testing.T) {
		o := AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 1, LifecycleRevision: 1, Commit: CommitUpstreamResponded, Result: ResultFailed, HTTPStatus: 500, Terminal: true}
		require.NoError(t, o.Validate())
		require.False(t, CanRetry(CallerChat, o))
	})
	t.Run("network not_sent and sent_ambiguous", func(t *testing.T) {
		o := AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 1, LifecycleRevision: 1, Commit: CommitNotSent, Result: ResultFailed, HTTPStatus: 0, Terminal: false, BusinessFrameSent: false}
		require.NoError(t, o.Validate())
		require.True(t, CanRetry(CallerChat, o))
		o2 := AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 1, LifecycleRevision: 1, Commit: CommitSentAmbiguous, Result: ResultFailed, HTTPStatus: 0, Terminal: true, BusinessFrameSent: true}
		require.NoError(t, o2.Validate())
		require.False(t, CanRetry(CallerChat, o2))
	})
	t.Run("malformed upstream_responded terminal never retry", func(t *testing.T) {
		o := AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 1, LifecycleRevision: 1, Commit: CommitUpstreamResponded, Result: ResultFailed, HTTPStatus: 400, IsMalformed: true, Terminal: true}
		require.NoError(t, o.Validate())
		require.False(t, CanRetry(CallerChat, o))
		o2 := AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 1, LifecycleRevision: 1, Commit: CommitUpstreamResponded, Result: ResultFailed, HTTPStatus: 0, IsMalformed: true, Terminal: true}
		require.NoError(t, o2.Validate())
		require.False(t, CanRetry(CallerChat, o2))
	})
	t.Run("client_cancel flow only explicit states", func(t *testing.T) {
		for _, commit := range []CommitState{CommitNotSent, CommitSentAmbiguous, CommitResponseStarted, CommitClientCommitted} {
			o := AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 1, LifecycleRevision: 1, Commit: commit, Result: ResultClientCancel, HTTPStatus: 0, Terminal: true}
			if commit == CommitNotSent {
				o.BusinessFrameSent = false
			} else {
				o.BusinessFrameSent = true
			}
			require.NoError(t, o.Validate(), "commit %v", commit)
			require.False(t, o.IsCountedForQuality())
			require.False(t, CanRetry(CallerChat, o))
		}
		o := AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 1, LifecycleRevision: 1, Commit: CommitUpstreamResponded, Result: ResultClientCancel, HTTPStatus: 0, Terminal: true}
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
		o := AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 0, LifecycleRevision: 1, Commit: CommitNotSent, Result: ResultFailed, HTTPStatus: 0, Terminal: false}
		require.Error(t, o.Validate())
		o = AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 1, LifecycleRevision: 0, Commit: CommitNotSent, Result: ResultFailed, HTTPStatus: 0, Terminal: false}
		require.Error(t, o.Validate())
		o = AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 1, LifecycleRevision: 1, Commit: CommitNotSent, Result: ResultFailed, HTTPStatus: 0, Terminal: false}
		require.NoError(t, o.Validate())
	})
	t.Run("negative 1xx 3xx >599 rejected", func(t *testing.T) {
		cases := []AttemptStatus{-1, 100, 199, 300, 301, 399, 600, 700}
		for _, s := range cases {
			o := AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 1, LifecycleRevision: 1, Commit: CommitUpstreamResponded, Result: ResultFailed, HTTPStatus: s, Terminal: true}
			require.Error(t, o.Validate(), "status %d", s)
		}
	})
	t.Run("success plus ambiguous invalid", func(t *testing.T) {
		o := AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 1, LifecycleRevision: 1, Commit: CommitSentAmbiguous, Result: ResultSuccess, HTTPStatus: 200, BusinessFrameSent: true, Terminal: true}
		require.Error(t, o.Validate())
		o2 := AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 1, LifecycleRevision: 1, Commit: CommitUpstreamResponded, Result: ResultSuccess, HTTPStatus: 200, Terminal: true}
		require.Error(t, o2.Validate())
	})
	t.Run("429 using wrong not_sent invalid", func(t *testing.T) {
		o := AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 1, LifecycleRevision: 1, Commit: CommitNotSent, Result: ResultFailed, HTTPStatus: 429, Terminal: false}
		require.Error(t, o.Validate())
		require.False(t, CanRetry(CallerChat, o))
	})
	t.Run("zero generation revision for network not_sent invalid", func(t *testing.T) {
		o := AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 0, LifecycleRevision: 0, Commit: CommitNotSent, Result: ResultFailed, HTTPStatus: 0, Terminal: false}
		require.Error(t, o.Validate())
	})
	t.Run("valid metadata fixture for all ten callers", func(t *testing.T) {
		for _, cat := range AllCallerCategories() {
			o := AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 1, LifecycleRevision: 1, Commit: CommitNotSent, Result: ResultFailed, HTTPStatus: 0, Terminal: false}
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
		o := AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 1, LifecycleRevision: 1, Commit: CommitNotSent, Result: ResultFailed, HTTPStatus: 0, Terminal: true}
		require.Error(t, o.Validate())
	})
}
