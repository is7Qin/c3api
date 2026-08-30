// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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
	sel, err := p.sched.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	attempt := scheduler.Attempt{
		AttemptID: "req-1:1", RouteClassID: strings.Repeat("a", 64), QualityClassID: strings.Repeat("b", 64), CandidateFingerprint: strings.Repeat("c", 64),
		TemplateID: sel.TemplateID, AccountID: sel.AccountID, RequestedModel: "gpt-4o", MappedModel: sel.Model, Lane: scheduler.AttemptLanePrimary, Ordinal: 1, RoutingGeneration: 2, LifecycleRevision: 3,
		CallerCategory: "chat", OperationTag: "chat_completions",
	}
	flowCalls := 0
	p.pipelineFlowAppend = func(outcome AttemptOutcome) {
		require.Equal(t, AttemptID(attempt.AttemptID), outcome.ID)
		require.Equal(t, uint8(1), outcome.Ordinal)
		flowCalls++
	}
	observer, owns := p.pipelineObserver(sel, attempt)
	require.True(t, owns)
	require.Nil(t, observer.markHealth, "failover classification remains the single MarkResult owner")
	require.Nil(t, observer.release, "failover retry/finish remains the single lease release owner")
	outcome := pipelineOutcome(attempt, 429, false, false)
	require.NoError(t, observer.Complete(outcome, nil))
	require.Equal(t, 1, flowCalls)
	runtime, ok := p.sched.Runtime(sel.AccountID)
	require.True(t, ok)
	require.Equal(t, int64(1), runtime.Concurrency, "observer must leave the lease to failover cleanup")
	p.cfg.FailoverAttempts = 1
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	p.failoverLoopWithPlan(httptest.NewRecorder(), req, domain.FormatOpenAIChat, domain.FormatOpenAIChat, "req-1", 10, time.Now(), "gpt-4o", nil, sel, nil, attemptState{}, rejectedPipelineAttempt{code: 429}, &httpSink{}, false)
	runtime, ok = p.sched.Runtime(sel.AccountID)
	require.True(t, ok)
	require.Zero(t, runtime.Concurrency, "observed exhaustion has one pipeline release owner")
}

func TestFailoverPipeline_observed4xxPreservesFinishAndMarkResult(t *testing.T) {
	up := fakeUpstreamStatus(t, 401, `{"error":{"message":"balance"}}`)
	defer up.Close()
	p := newTestProxyRules(t, up.URL, domain.FormatOpenAIChat,
		domain.Rule{Name: "balance-401", Enabled: true, Priority: 10,
			When: domain.RuleWhen{Kind: strPtrT("4xx"), HTTPStatus: intPtrT(401)},
			Then: domain.RuleThen{Status: statusPtrT(domain.StatusUnhealthy), ResponseCode: intPtrT(502), CustomMessage: strPtrT("upstream rejected request")}},
	)
	sel, err := p.sched.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	attempt := scheduler.Attempt{
		AttemptID: "req-4xx:1", RouteClassID: strings.Repeat("a", 64), QualityClassID: strings.Repeat("b", 64), CandidateFingerprint: strings.Repeat("c", 64),
		TemplateID: sel.TemplateID, AccountID: sel.AccountID, RequestedModel: "gpt-4o", MappedModel: sel.Model, Lane: scheduler.AttemptLanePrimary, Ordinal: 1, RoutingGeneration: 2, LifecycleRevision: 3,
		CallerCategory: "chat", OperationTag: "chat_completions",
	}
	flowCalls := 0
	p.pipelineFlowAppend = func(AttemptOutcome) { flowCalls++ }
	observer, owns := p.pipelineObserver(sel, attempt)
	require.True(t, owns)
	require.Nil(t, observer.markHealth)
	require.Nil(t, observer.release)
	// The observer is never completed by hand: the failover loop owns the
	// exactly-one completion for every dispatch (plan-backed or legacy).

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	p.failoverLoopWithPlan(rec, req, domain.FormatOpenAIChat, domain.FormatOpenAIChat, "req-4xx", 10, time.Now(), "gpt-4o", nil, sel, nil, attemptState{}, rejectedPipelineAttempt{code: 401, body: []byte(`{"error":{"message":"balance"}}`)}, &httpSink{}, false)

	require.Equal(t, 1, flowCalls, "the loop observes the failed dispatch exactly once")
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), "upstream rejected request")
	p.sched.FlushRules()
	runtime, ok := p.sched.Runtime(sel.AccountID)
	require.True(t, ok)
	require.Equal(t, 1, runtime.ErrCount, "observed 4xx has one MarkResult owner")
	require.Equal(t, domain.StatusUnhealthy, runtime.Status)
	require.Zero(t, runtime.Concurrency, "observed 4xx finish releases once")
}

func TestFailoverPipeline_handledTruePerformsNoSharedCleanup(t *testing.T) {
	p := &Proxy{}
	p.pipelineFlowAppend = func(AttemptOutcome) { t.Fatal("handled attempt must not append flow") }
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	p.failoverLoopWithPlan(httptest.NewRecorder(), req, "chat", "chat", "req-handled", 1, time.Now(), "gpt-4o", nil, &scheduler.Selection{}, nil, attemptState{}, handledPipelineAttempt{}, &httpSink{}, false)
}
