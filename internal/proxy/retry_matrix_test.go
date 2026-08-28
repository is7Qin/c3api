// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRetryMatrix(t *testing.T) {
	cats := AllCallerCategories()
	require.Len(t, cats, 10, "must have exactly 10 caller categories")
	kinds := []string{"not_sent", "429", "5xx", "sent_ambiguous", "response_started", "client_committed", "client_cancel", "malformed"}
	for _, cat := range cats {
		for _, kind := range kinds {
			t.Run(fmt.Sprintf("%s/%s", cat, kind), func(t *testing.T) {
				o := outcomeForKind(kind, cat)
				can := CanRetry(cat, o)
				switch kind {
				case "not_sent":
					require.True(t, can, "not_sent must be retryable for all categories")
					require.False(t, o.HasPossiblyWrittenBytes(), "not_sent must not be labeled as bytes possibly written")
				case "429":
					if cat.IsHardContinuation() {
						require.False(t, can, "hard continuation 429 must terminate")
					} else {
						require.True(t, can, "ordinary pre-response 429 must be retryable")
					}
					require.False(t, o.HasPossiblyWrittenBytes())
				case "5xx":
					require.False(t, can, "every 5xx must terminate because no idempotency capability exists")
				case "sent_ambiguous":
					require.False(t, can, "sent_ambiguous terminates")
					require.True(t, o.HasPossiblyWrittenBytes())
				case "response_started":
					require.False(t, can, "response_started terminates")
					require.True(t, o.HasPossiblyWrittenBytes())
				case "client_committed":
					require.False(t, can, "client_committed terminates")
					require.True(t, o.HasPossiblyWrittenBytes())
				case "client_cancel":
					require.False(t, can, "client cancel terminates")
				case "malformed":
					require.False(t, can, "malformed terminates")
				}
			})
		}
	}
	t.Run("golden all 5xx false and ordinary 429 true only where allowed", func(t *testing.T) {
		mat := RetryMatrixGolden()
		for _, cat := range cats {
			require.False(t, mat[cat]["5xx"], "5xx must be false for %s", cat)
			if cat.IsHardContinuation() {
				require.False(t, mat[cat]["429"], "hard continuation 429 false")
			} else {
				require.True(t, mat[cat]["429"], "ordinary 429 true")
			}
			require.True(t, mat[cat]["not_sent"])
		}
	})
	t.Run("WS dial not_sent only before business frame", func(t *testing.T) {
		o := AttemptOutcome{ID: "a1", Commit: CommitNotSent, Result: ResultFailed, IsWSDial: true, HasSentBusinessFrame: false}
		require.True(t, CanRetry(CallerResponsesWS, o))
		o2 := AttemptOutcome{ID: "a1", Commit: CommitResponseStarted, Result: ResultFailed, IsWSDial: true, HasSentBusinessFrame: true}
		require.False(t, CanRetry(CallerResponsesWS, o2))
		require.Error(t, (AttemptOutcome{ID: "a1", Commit: CommitNotSent, Result: ResultFailed, IsWSDial: true, HasSentBusinessFrame: true}).Validate())
	})
	t.Run("no bytes possibly written mislabeled not_sent", func(t *testing.T) {
		for _, cat := range cats {
			for _, k := range kinds {
				o := outcomeForKind(k, cat)
				if o.HasPossiblyWrittenBytes() {
					require.NotEqual(t, CommitNotSent, o.Commit, "bytes possibly written must not be CommitNotSent for %s/%s", cat, k)
				}
			}
		}
	})
}
