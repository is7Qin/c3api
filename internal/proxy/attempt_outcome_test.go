// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAttemptOutcomeContract(t *testing.T) {
	t.Run("valid ordinary success", func(t *testing.T) {
		o := AttemptOutcome{
			ID: "a1", RouteClassID: "rc", Fingerprint: "fp", LifecycleRevision: 1,
			Lane: LanePrimary, Generation: 1, Commit: CommitNotSent, Result: ResultSuccess, HTTPStatus: 200,
		}
		require.NoError(t, o.Validate())
		require.True(t, o.IsDispatched())
		require.True(t, o.IsCountedForQuality())
		require.False(t, o.IsFailed())
		require.False(t, o.HasPossiblyWrittenBytes())
	})
	t.Run("ordinary 4xx failed counts", func(t *testing.T) {
		o := AttemptOutcome{ID: "a1", Commit: CommitNotSent, Result: ResultFailed, HTTPStatus: 400}
		require.NoError(t, o.Validate())
		require.True(t, o.IsCountedForQuality())
		require.True(t, o.IsFailed())
	})
	t.Run("malformed counts failed", func(t *testing.T) {
		o := AttemptOutcome{ID: "a1", Commit: CommitNotSent, Result: ResultFailed, HTTPStatus: 400, IsMalformed: true}
		require.NoError(t, o.Validate())
		require.True(t, o.IsFailed())
	})
	t.Run("post-commit failure counts failed but not retryable", func(t *testing.T) {
		o := AttemptOutcome{ID: "a1", Commit: CommitResponseStarted, Result: ResultFailed, HTTPStatus: 500, HasSentBusinessFrame: true}
		require.NoError(t, o.Validate())
		require.True(t, o.HasPossiblyWrittenBytes())
		require.True(t, o.IsFailed())
	})
	t.Run("client cancel flow only", func(t *testing.T) {
		o := AttemptOutcome{ID: "a1", Commit: CommitNotSent, Result: ResultClientCancel}
		require.True(t, o.IsDispatched())
		require.False(t, o.IsCountedForQuality())
		require.False(t, o.IsFailed())
	})
	t.Run("local reject not dispatched", func(t *testing.T) {
		o := AttemptOutcome{ID: "a1", Commit: CommitNotSent, Result: ResultLocalReject}
		require.False(t, o.IsDispatched())
		require.False(t, o.IsCountedForQuality())
	})
	t.Run("bytes written cannot be not_sent", func(t *testing.T) {
		o := AttemptOutcome{ID: "a1", Commit: CommitNotSent, Result: ResultFailed, HasSentBusinessFrame: true}
		require.Error(t, o.Validate())
		o2 := AttemptOutcome{ID: "a1", Commit: CommitResponseStarted, Result: ResultFailed, HasSentBusinessFrame: true}
		require.True(t, o2.HasPossiblyWrittenBytes())
	})
	t.Run("WS dial not_sent only before business frame", func(t *testing.T) {
		o := AttemptOutcome{ID: "a1", Commit: CommitNotSent, Result: ResultFailed, IsWSDial: true, HasSentBusinessFrame: false}
		require.NoError(t, o.Validate())
		o2 := AttemptOutcome{ID: "a1", Commit: CommitNotSent, Result: ResultFailed, IsWSDial: true, HasSentBusinessFrame: true}
		require.Error(t, o2.Validate())
	})
	t.Run("malformed enum", func(t *testing.T) {
		o := AttemptOutcome{ID: "a1", Commit: CommitState(99), Result: ResultFailed}
		require.Error(t, o.Validate())
		o2 := AttemptOutcome{ID: "", Commit: CommitNotSent}
		require.Error(t, o2.Validate())
		o3 := AttemptOutcome{ID: "a1", Commit: CommitNotSent, Lane: LaneID("bad")}
		require.Error(t, o3.Validate())
		o4 := AttemptOutcome{ID: "a1", Commit: CommitNotSent, Result: ResultSuccess, IsMalformed: true}
		require.Error(t, o4.Validate(), "malformed must be failed")
	})
	t.Run("stale future capability absence", func(t *testing.T) {
		o := AttemptOutcome{ID: "a1", Commit: CommitNotSent, Result: ResultFailed, HTTPStatus: 500}
		require.False(t, CanRetry(CallerChat, o), "5xx must not retry without idempotency capability")
	})
}
