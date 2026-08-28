// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func validBase() AttemptOutcome {
	return AttemptOutcome{
		ID: "a1", RouteClassID: "rc", Fingerprint: "fp", LifecycleRevision: 1,
		Lane: LanePrimary, Generation: 1, Commit: CommitNotSent, Result: ResultSuccess, HTTPStatus: 200,
		BusinessFrameSent: false, HardContinuation: false,
		Timing: AttemptTiming{LatencyMS: 10}, Usage: AttemptUsage{InputTokens: 1},
	}
}

func TestAttemptOutcomeContract(t *testing.T) {
	t.Run("valid ordinary success response_started", func(t *testing.T) {
		o := validBase()
		o.Commit = CommitResponseStarted
		o.BusinessFrameSent = true
		o.Result = ResultSuccess
		o.HTTPStatus = 200
		require.NoError(t, o.Validate())
		require.True(t, o.IsDispatched())
		require.True(t, o.IsCountedForQuality())
		require.False(t, o.IsFailed())
		require.True(t, o.HasPossiblyWrittenBytes())
	})
	t.Run("ordinary 4xx failed counts", func(t *testing.T) {
		o := validBase()
		o.Commit = CommitNotSent
		o.Result = ResultFailed
		o.HTTPStatus = 400
		require.NoError(t, o.Validate())
		require.True(t, o.IsCountedForQuality())
		require.True(t, o.IsFailed())
	})
	t.Run("malformed counts failed but not retryable", func(t *testing.T) {
		o := validBase()
		o.Result = ResultFailed
		o.HTTPStatus = 400
		o.IsMalformed = true
		o.BusinessFrameSent = false
		require.NoError(t, o.Validate())
		require.True(t, o.IsFailed())
		require.False(t, CanRetry(CallerChat, o))
	})
	t.Run("post-commit failure has bytes", func(t *testing.T) {
		o := validBase()
		o.Commit = CommitResponseStarted
		o.BusinessFrameSent = true
		o.Result = ResultFailed
		o.HTTPStatus = 500
		require.NoError(t, o.Validate())
		require.True(t, o.HasPossiblyWrittenBytes())
		require.True(t, o.IsFailed())
	})
	t.Run("client cancel flow only", func(t *testing.T) {
		o := validBase()
		o.Result = ResultClientCancel
		o.HTTPStatus = 0
		require.True(t, o.IsDispatched())
		require.False(t, o.IsCountedForQuality())
		require.False(t, o.IsFailed())
	})
	t.Run("local reject not dispatched not counted not retryable", func(t *testing.T) {
		o := AttemptOutcome{ID: "a1", Commit: CommitNotSent, Result: ResultLocalReject, HTTPStatus: 0}
		require.NoError(t, o.Validate())
		require.False(t, o.IsDispatched())
		require.False(t, o.IsCountedForQuality())
		require.False(t, CanRetry(CallerChat, o))
	})
	t.Run("reservation reject not dispatched", func(t *testing.T) {
		o := AttemptOutcome{ID: "a1", Commit: CommitNotSent, Result: ResultReservationReject, HTTPStatus: 0}
		require.NoError(t, o.Validate())
		require.False(t, o.IsDispatched())
		require.False(t, CanRetry(CallerChat, o))
	})
	t.Run("bytes written cannot be not_sent", func(t *testing.T) {
		o := validBase()
		o.Commit = CommitNotSent
		o.BusinessFrameSent = true
		o.Result = ResultFailed
		require.Error(t, o.Validate())
	})
	t.Run("WS dial not_sent only before business frame", func(t *testing.T) {
		o := validBase()
		o.Commit = CommitNotSent
		o.BusinessFrameSent = false
		require.NoError(t, o.Validate())
		o2 := validBase()
		o2.Commit = CommitNotSent
		o2.BusinessFrameSent = true
		require.Error(t, o2.Validate())
	})
	t.Run("response_started requires BusinessFrameSent", func(t *testing.T) {
		o := validBase()
		o.Commit = CommitResponseStarted
		o.BusinessFrameSent = false
		require.Error(t, o.Validate())
	})
	t.Run("terminal not_sent invalid", func(t *testing.T) {
		o := validBase()
		o.Commit = CommitNotSent
		o.BusinessFrameSent = false
		o.Terminal = true
		require.Error(t, o.Validate())
		require.False(t, CanRetry(CallerChat, o))
	})
	t.Run("required fields where dispatch exists", func(t *testing.T) {
		o := validBase()
		o.RouteClassID = ""
		require.Error(t, o.Validate())
		o = validBase()
		o.Fingerprint = ""
		require.Error(t, o.Validate())
		o = validBase()
		o.Lane = ""
		require.Error(t, o.Validate())
		o = validBase()
		o.Lane = LaneID("bad")
		require.Error(t, o.Validate())
		o = validBase()
		o.Generation = -1
		require.Error(t, o.Validate())
		o = validBase()
		o.LifecycleRevision = -1
		require.Error(t, o.Validate())
	})
	t.Run("nonnegative timing usage", func(t *testing.T) {
		o := validBase()
		o.Timing.LatencyMS = -1
		require.Error(t, o.Validate())
		o = validBase()
		neg := int64(-5)
		o.Timing.TTFTMS = &neg
		require.Error(t, o.Validate())
		o = validBase()
		o.Usage.InputTokens = -1
		require.Error(t, o.Validate())
	})
	t.Run("result status commit consistency", func(t *testing.T) {
		o := validBase()
		o.Result = ResultSuccess
		o.HTTPStatus = 500
		require.Error(t, o.Validate())
		o = validBase()
		o.Result = ResultLocalReject
		o.HTTPStatus = 429
		require.Error(t, o.Validate())
		o = validBase()
		o.Result = ResultUnknown
		require.Error(t, o.Validate())
		o = validBase()
		o.Commit = CommitState(99)
		require.Error(t, o.Validate())
		o = validBase()
		o.Result = ResultSuccess
		o.IsMalformed = true
		require.Error(t, o.Validate())
	})
	t.Run("local reject with business frame invalid", func(t *testing.T) {
		o := AttemptOutcome{ID: "a1", Commit: CommitNotSent, Result: ResultLocalReject, BusinessFrameSent: true}
		require.Error(t, o.Validate())
	})
	t.Run("stale future capability absence 5xx no retry", func(t *testing.T) {
		o := validBase()
		o.Result = ResultFailed
		o.HTTPStatus = 500
		o.Commit = CommitNotSent
		o.BusinessFrameSent = false
		require.False(t, CanRetry(CallerChat, o))
	})
}
