// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

type handledPipelineAttempt struct{}

func (handledPipelineAttempt) call(context.Context, http.ResponseWriter, *http.Request, string, int64, time.Time, *scheduler.Selection, string, []byte, attemptState) (int, []byte, http.Header, bool, error) {
	return http.StatusOK, nil, nil, true, nil
}

type rejectedPipelineAttempt struct {
	code int
	body []byte
}

func (a rejectedPipelineAttempt) call(context.Context, http.ResponseWriter, *http.Request, string, int64, time.Time, *scheduler.Selection, string, []byte, attemptState) (int, []byte, http.Header, bool, error) {
	return a.code, a.body, nil, false, nil
}

func TestPipelineObserver_observedPreResponseLeavesCleanupToFailover(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	sel, plan, attempt, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o",
		scheduler.AttemptPlanIdentity{RequestID: "req-1", UserID: 1})
	require.NoError(t, err)
	// the threaded settle value and the session derivation coincide.
	current, ok := plan.CurrentAttempt()
	require.True(t, ok)
	require.Equal(t, attempt, current)
	flowCalls := 0
	p.pipelineFlowAppend = func(outcome AttemptOutcome) {
		require.Equal(t, AttemptID(attempt.AttemptID), outcome.ID)
		require.EqualValues(t, attempt.Ordinal, outcome.Ordinal)
		flowCalls++
	}
	observer, owns := p.pipelineObserver(sel, attempt, p.pipelineFlowAppend)
	require.True(t, owns)
	require.Nil(t, observer.markHealth, "failover classification remains the single MarkResult owner")
	require.Nil(t, observer.release, "failover retry/finish remains the single lease release owner")
	outcome := dispatchFailureOutcome(pipelineBase(attempt), 429, false, false)
	require.NoError(t, observer.Complete(outcome, nil))
	require.Equal(t, 1, flowCalls)
	runtime, ok := p.sched.Runtime(sel.AccountID)
	require.True(t, ok)
	require.Equal(t, int64(1), runtime.Concurrency, "observer must leave the lease to failover cleanup")
	p.cfg.FailoverAttempts = 1
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	p.failoverLoopWithPlan(httptest.NewRecorder(), req, domain.FormatOpenAIChat, "req-1", 10, time.Now(), "gpt-4o", nil, sel, plan, attempt, attemptState{}, rejectedPipelineAttempt{code: 429}, &httpSink{}, false)
	runtime, ok = p.sched.Runtime(sel.AccountID)
	require.True(t, ok)
	require.Zero(t, runtime.Concurrency, "observed exhaustion has one pipeline release owner")
}

func TestFailoverPipeline_observed4xxPreservesFinishAndMarkResult(t *testing.T) {
	testHealthSink.reset()
	up := fakeUpstreamStatus(t, 401, `{"error":{"message":"balance"}}`)
	defer up.Close()
	p := newTestProxyRules(t, up.URL, domain.FormatOpenAIChat,
		domain.Rule{Name: "balance-401", Enabled: true, Priority: 10,
			When: domain.RuleWhen{Kind: strPtrT("4xx"), HTTPStatus: intPtrT(401)},
			Then: domain.RuleThen{Throttle: &domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: int64PtrT(60 * 1000)}, ResponseCode: intPtrT(502), CustomMessage: strPtrT("upstream rejected request")}},
	)
	sel, plan, attempt, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o",
		scheduler.AttemptPlanIdentity{RequestID: "req-4xx", UserID: 1})
	require.NoError(t, err)
	// the threaded settle value and the session derivation coincide.
	current, ok := plan.CurrentAttempt()
	require.True(t, ok)
	require.Equal(t, attempt, current)
	flowCalls := 0
	p.pipelineFlowAppend = func(AttemptOutcome) { flowCalls++ }
	observer, owns := p.pipelineObserver(sel, attempt, p.pipelineFlowAppend)
	require.True(t, owns)
	require.Nil(t, observer.markHealth)
	require.Nil(t, observer.release)
	// The observer is never completed by hand: the failover loop owns the
	// exactly-one completion for every plan-backed dispatch.

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	p.failoverLoopWithPlan(rec, req, domain.FormatOpenAIChat, "req-4xx", 10, time.Now(), "gpt-4o", nil, sel, plan, attempt, attemptState{}, rejectedPipelineAttempt{code: 401, body: []byte(`{"error":{"message":"balance"}}`)}, &httpSink{}, false)

	require.Equal(t, 1, flowCalls, "the loop observes the failed dispatch exactly once")
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), "upstream rejected request")
	p.sched.FlushRules()
	require.Len(t, testHealthSink.throttlesFor(sel.AccountID), 1, "observed 4xx has one MarkResult owner")
	runtime, ok := p.sched.Runtime(sel.AccountID)
	require.True(t, ok)
	require.Zero(t, runtime.Concurrency, "observed 4xx finish releases once")
}

func TestFailoverPipeline_handledTruePerformsNoSharedCleanup(t *testing.T) {
	p := &Proxy{}
	p.pipelineFlowAppend = func(AttemptOutcome) { t.Fatal("handled attempt must not append flow") }
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	// the session and attempt thread by value — zero values here prove
	// a handled terminal performs no shared cleanup without any identity.
	p.failoverLoopWithPlan(httptest.NewRecorder(), req, "chat", "req-handled", 1, time.Now(), "gpt-4o", nil, &scheduler.Selection{}, scheduler.AttemptPlan{}, scheduler.Attempt{}, attemptState{}, handledPipelineAttempt{}, &httpSink{}, false)
}
