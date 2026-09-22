// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/continuation"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

// §7 G-select-parity, proxy half: golden failover sequences through
// the threaded settle API (selectWithPlan → selectNextWithPlan → contPin →
// shouldRetryWithPlan). The walk itself is oracle-pinned by the scheduler
// select_session parity corpus; this file pins the proxy threading on top:
// every threaded Attempt must coincide with the session derivation
// (require.Equal against CurrentAttempt at each step), and the projected
// [(accountID, ordinal, attemptID)] sequences are byte-identical goldens.
func parityFP(t *testing.T, fpHex string) [32]byte {
	t.Helper()
	raw, err := hex.DecodeString(fpHex)
	require.NoError(t, err)
	var out [32]byte
	copy(out[:], raw)
	return out
}

func TestSelectSessionParity_threadedFailoverSequence(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := chatProxyWithPlan(t, up.URL, 3, []int64{1, 2, 3})
	p.cfg.FailoverAttempts = 3

	sel, plan, attempt, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o",
		scheduler.AttemptPlanIdentity{RequestID: "req-px1", UserID: 1})
	require.NoError(t, err)
	require.Equal(t, "req-px1", plan.Identity().RequestID)
	dec, ok := p.sched.View().DecisionView().Route(10, string(domain.FormatOpenAIChat), "gpt-4o")
	require.True(t, ok)
	require.Equal(t, dec.RouteClassID, plan.Identity().RouteClassID)
	require.Equal(t, dec.RouteClassID, attempt.RouteClassID)

	var got []string
	rec := func(a scheduler.Attempt) string {
		return fmt.Sprintf("%d|%s|%d|%s", a.AccountID, string(a.Lane), a.Ordinal, a.AttemptID)
	}
	cur, ok := plan.CurrentAttempt()
	require.True(t, ok)
	require.Equal(t, attempt, cur, "threaded settle value coincides with the session derivation")
	got = append(got, rec(attempt))
	sel.Release()

	for i := 0; i < 3; i++ {
		nextSel, nextAttempt, err := p.selectNextWithPlan(&plan)
		if err != nil {
			require.ErrorIs(t, err, scheduler.ErrAttemptsExhausted)
			got = append(got, "ERR:exhausted")
			break
		}
		cur, ok := plan.CurrentAttempt()
		require.True(t, ok)
		require.Equal(t, nextAttempt, cur, "threaded settle value coincides with the session derivation")
		got = append(got, rec(nextAttempt))
		nextSel.Release()
	}
	require.Equal(t, []string{
		"1|primary|1|req-px1:1",
		"2|primary|2|req-px1:2",
		"3|primary|3|req-px1:3",
		"ERR:exhausted",
	}, got)
}

func TestSelectSessionParity_hardContinuationPin(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := chatProxyWithPlan(t, up.URL, 3, []int64{1, 2, 3})

	// Probe the bound identity for account 2 from a throwaway session.
	probeSel, probePlan, _, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o",
		scheduler.AttemptPlanIdentity{RequestID: "req-probe", UserID: 1})
	require.NoError(t, err)
	probeSel.Release()
	probeSel2, bound, err := p.selectNextWithPlan(&probePlan)
	require.NoError(t, err)
	probeSel2.Release()
	require.Equal(t, int64(2), bound.AccountID)
	b := &continuation.Binding{AccountID: 2, Fingerprint: parityFP(t, bound.CandidateFingerprint), IdentityRevision: bound.IdentityRevision}

	sel, plan, attempt, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o",
		scheduler.AttemptPlanIdentity{RequestID: "req-pin", UserID: 1})
	require.NoError(t, err)
	require.Equal(t, int64(1), attempt.AccountID)

	pinned, pinnedAttempt, ferr := p.contPin(&plan, sel, attempt, b)
	require.Nil(t, ferr)
	require.Equal(t, int64(2), pinned.AccountID)
	require.Equal(t, uint8(1), pinnedAttempt.Ordinal, "skipped unbound candidate refunds its ordinal")
	require.Equal(t, "req-pin:1", pinnedAttempt.AttemptID)
	cur, ok := plan.CurrentAttempt()
	require.True(t, ok)
	require.Equal(t, pinnedAttempt, cur)
	pinned.Release()

	// Stale binding (revision drift on the bound account) fails closed.
	sel2, plan2, attempt2, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o",
		scheduler.AttemptPlanIdentity{RequestID: "req-pin-stale", UserID: 1})
	require.NoError(t, err)
	stale := &continuation.Binding{AccountID: attempt2.AccountID, Fingerprint: parityFP(t, attempt2.CandidateFingerprint), IdentityRevision: attempt2.IdentityRevision + 1}
	_, _, ferr = p.contPin(&plan2, sel2, attempt2, stale)
	require.NotNil(t, ferr)
}

func TestSelectSessionParity_retryMatrixOnThreadedAttempts(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := chatProxyWithPlan(t, up.URL, 2, []int64{1, 2})

	sel, plan, attempt, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o",
		scheduler.AttemptPlanIdentity{RequestID: "req-rm", UserID: 1})
	require.NoError(t, err)
	defer sel.Release()
	_ = plan

	require.True(t, p.shouldRetryWithPlan(context.Background(), 0, errors.New("dial failed"), attempt, false))
	require.True(t, p.shouldRetryWithPlan(context.Background(), http.StatusTooManyRequests, nil, attempt, false))
	require.False(t, p.shouldRetryWithPlan(context.Background(), http.StatusInternalServerError, nil, attempt, false))
	require.False(t, p.shouldRetryWithPlan(context.Background(), http.StatusBadRequest, nil, attempt, false))
	// Hard continuation never migrates, even on 429.
	require.False(t, p.shouldRetryWithPlan(context.Background(), http.StatusTooManyRequests, nil, attempt, true))
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	require.False(t, p.shouldRetryWithPlan(cancelled, 0, errors.New("canceled"), attempt, false))
}

func TestSelectSessionParity_unwiredContinuationFailsClosed(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := chatProxyWithPlan(t, up.URL, 1, []int64{1})
	p.cont = nil
	sel, _, attempt, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o",
		scheduler.AttemptPlanIdentity{RequestID: "req-unwired", UserID: 1})
	require.NoError(t, err)
	defer sel.Release()
	_, ferr := p.contResolve(context.Background(), 1, 10, contProtocolREST, "resp-123", attempt)
	require.NotNil(t, ferr)
}
