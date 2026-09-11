// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

// wsRetryAttempt is a valid canonical resp-ws attempt identity (the shape
// ReserveAttempt records); the retry decision consumes it via
// retryOutcomeForAttempt — identity from the attempt, terminal facts from the
// observed code/callErr.
func wsRetryAttempt() scheduler.Attempt {
	prev := "req-ws:1"
	return scheduler.Attempt{
		AttemptID: "req-ws:2", RouteClassID: strings.Repeat("a", 64), QualityClassID: strings.Repeat("b", 64),
		CandidateFingerprint: strings.Repeat("c", 64), TemplateID: 3, AccountID: 9,
		RequestedModel: "gpt-4o", MappedModel: "gpt-4o", Lane: scheduler.AttemptLanePrimary,
		Ordinal: 2, RoutingGeneration: 5, LifecycleRevision: 4, PreviousAttemptID: &prev,
		CallerCategory: "responses_ws", OperationTag: "responses_ws",
	}
}

func TestWSFirstFrameWriteFailureReturnNotSentRetryable(t *testing.T) {
	a := wsRetryAttempt()
	require.NoError(t, a.Validate())

	o := retryOutcomeForAttempt(a, 0, errors.New("upstream first frame write failed"), context.Background())
	require.Equal(t, CommitNotSent, o.Commit)
	require.False(t, o.Terminal)
	require.True(t, CanRetry(CallerResponsesWS, o))
	require.Equal(t, AttemptID(a.AttemptID), o.ID, "the retry decision consumes the canonical attempt identity")

	// Terminal case must not be retryable
	o2 := retryOutcomeForAttempt(a, 0, nil, context.Background())
	require.True(t, o2.Terminal)
	require.False(t, CanRetry(CallerResponsesWS, o2))
}

func TestWSFirstFrameFailureRetriesSecondCandidateBarrier(t *testing.T) {
	// Compiled plan over two accounts: after a first not-sent failure the
	// second candidate must still be reservable (failover advances the plan).
	s := newTestSchedulerForPlan(t)
	tpl2 := tplForPlan(2)
	loader := s.Loader().(noopLoader)
	loader.accs[10] = append(loader.accs[10], &domain.Account{ID: 2, TemplateID: 2, Template: tpl2, UpstreamKey: "k2", Enabled: true, MaxConcurrency: 10, LifecycleRevision: 1})
	require.NoError(t, s.InvalidateAllSync())
	publishTestRoutes(t, s)

	plan, err := s.NewAttemptPlan(scheduler.AttemptPlanIdentity{RequestID: "r1", UserID: 1}, scheduler.RouteRefFor(10, string(domain.FormatOpenAIChat), "gpt-4o"))
	require.NoError(t, err)
	sel1, a1, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	require.Equal(t, int64(1), sel1.AccountID)
	require.True(t, CanRetry(CallerCategory(a1.CallerCategory), retryOutcomeForAttempt(a1, 0, errors.New("first frame fail"), context.Background())), "first frame not-sent must be retryable")
	sel1.Release()
	sel2, _, err := s.ReserveAttempt(&plan)
	require.NoError(t, err, "second candidate must be available after first not-sent failure")
	require.Equal(t, int64(2), sel2.AccountID)
	sel2.Release()
	_, _, err = s.ReserveAttempt(&plan)
	require.ErrorIs(t, err, scheduler.ErrAttemptsExhausted)
}

func TestWSFirstFrameAttemptReturnsCallErrPreserved(t *testing.T) {
	// Ensure the error text path is not erased: code 0 with a callErr is a
	// not-sent retryable classification.
	a := wsRetryAttempt()
	msg := "upstream first frame write failed: broken pipe"
	o := retryOutcomeForAttempt(a, 0, errors.New(msg), context.Background())
	require.Equal(t, CommitNotSent, o.Commit)
	require.False(t, o.Terminal)
	require.True(t, CanRetry(CallerResponsesWS, o))
	// Fallback case where callErr nil would be terminal and information erased
	o2 := retryOutcomeForAttempt(a, 0, nil, context.Background())
	require.True(t, o2.Terminal, "code 0 with nil callErr must not be retryable - information would be erased")
}
