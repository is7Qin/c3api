// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/quality"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
)

type handledPipelineAttempt struct{}

func (handledPipelineAttempt) call(context.Context, http.ResponseWriter, *http.Request, string, int64, time.Time, *scheduler.Selection, string, []byte, attemptState) (int, []byte, http.Header, bool, error) {
	return http.StatusOK, nil, nil, true, nil
}

func TestPipelineObserver_preResponse429CompletesOnceAndHandledAttemptIsUntouched(t *testing.T) {
	recorder, err := quality.NewRecorder(32)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, recorder.Close()) })
	p := &Proxy{qualityRecorder: recorder}
	attempt := scheduler.Attempt{
		AttemptID: "req-1:1", RouteClassID: strings.Repeat("a", 64), QualityClassID: strings.Repeat("b", 64), CandidateFingerprint: strings.Repeat("c", 64),
		TemplateID: 1, AccountID: 7, RequestedModel: "gpt-4o", MappedModel: "gpt-4o", Lane: scheduler.AttemptLanePrimary, Ordinal: 1, RoutingGeneration: 2, LifecycleRevision: 3,
		CallerCategory: "chat", OperationTag: "chat_completions",
	}
	var healthCalls, flowCalls, releaseCalls atomic.Int32
	p.pipelineFlowAppend = func(outcome AttemptOutcome) {
		require.Equal(t, AttemptID(attempt.AttemptID), outcome.ID)
		require.Equal(t, uint8(1), outcome.Ordinal)
		flowCalls.Add(1)
	}
	observer, owns := p.pipelineObserver(&scheduler.Selection{}, attempt)
	require.True(t, owns)
	observer.markHealth = func(outcome AttemptOutcome, event AttemptHealthEvent) {
		require.Equal(t, attempt.AccountID, outcome.AccountID)
		require.Equal(t, rule.Kind429, event.Kind)
		require.True(t, event.Retryable)
		healthCalls.Add(1)
	}
	observer.release = func() { releaseCalls.Add(1) }
	outcome := pipelineOutcome(attempt, 429, false, false)
	p.completePipelineObservation(observer, outcome, pipelineHealth(429, "busy"))
	require.Equal(t, int32(1), healthCalls.Load())
	require.Equal(t, int32(1), flowCalls.Load())
	require.Equal(t, int32(1), releaseCalls.Load())

	// The pipeline returns before creating or completing any observer for handled=true.
	require.Equal(t, int32(1), healthCalls.Load())
	require.Equal(t, int32(1), flowCalls.Load())
	require.Equal(t, int32(1), releaseCalls.Load())
}

func TestFailoverPipeline_handledTruePerformsNoSharedCleanup(t *testing.T) {
	p := &Proxy{}
	p.pipelineFlowAppend = func(AttemptOutcome) { t.Fatal("handled attempt must not append flow") }
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	p.failoverLoopWithPlan(httptest.NewRecorder(), req, "chat", "chat", "req-handled", 1, time.Now(), "gpt-4o", nil, &scheduler.Selection{}, nil, attemptState{}, handledPipelineAttempt{}, &httpSink{}, false)
}
