// SPDX-License-Identifier: AGPL-3.0-or-later
// Plan-backed-only routing contract (fresh ownership): every real AI dispatch
// executes a compiled attempt plan with canonical attempt identity. An
// absent/invalid compiled plan fails closed through the existing typed
// selection error path — no plan-less selection lane, no placeholder dispatch
// identity reaches observation.
package proxy

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

// addAnthropicStaticNoDecision adds an anthropic-capable account to the group
// static set WITHOUT publishing any anthropic decision route: the compiled
// plan lane misses for anthropic while the static snapshot stays loaded.
func addAnthropicStaticNoDecision(t *testing.T, p *Proxy, upstream string) {
	t.Helper()
	loader := p.sched.Loader().(noopLoader)
	tplA := &domain.Template{ID: 5, Name: "ta", BaseURL: upstream, CredentialType: "api_key", SupportedFormats: []domain.RequestFormat{domain.FormatAnthropic}, Models: []string{"claude-x"}}
	loader.accs[10] = append(loader.accs[10], &domain.Account{ID: 5, TemplateID: 5, Template: tplA, UpstreamKey: "sk-upstream", Enabled: true, LifecycleRevision: 1, MaxConcurrency: 4})
	require.NoError(t, p.sched.InvalidateAllSync())
}

func TestSelectWithPlan_NoCompiledDecision_FailsClosedWithTypedError(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	addAnthropicStaticNoDecision(t, p, up.URL)

	sel, plan, err := p.selectWithPlan(10, domain.FormatAnthropic, "claude-x",
		scheduler.AttemptPlanIdentity{RequestID: "req-noplan", UserID: 1})
	require.ErrorIs(t, err, scheduler.ErrFormatUnavailable)
	require.Nil(t, sel, "no selection may be produced without a compiled plan")
	require.Nil(t, plan)
}

func TestHandleAnthropic_NoCompiledDecision_RejectsWithoutDispatch(t *testing.T) {
	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	addAnthropicStaticNoDecision(t, p, up.URL)
	rec, fc := wireObserverHarness(t, p)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"claude-x","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	w := httptest.NewRecorder()
	p.HandleAnthropic(w, req)

	require.Equal(t, http.StatusNotFound, w.Code, "absent compiled plan fails closed as 404")
	require.Zero(t, atomic.LoadInt32(&hits), "no upstream dispatch without a plan")
	require.Empty(t, fc.snapshot())
	require.Zero(t, snapshotQualityAttempts(rec))
	ri, ok := p.sched.Runtime(5)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "rejected selection must not hold a lease")
}

// A model-less request resolves the compiled default bucket but the reserved
// Attempt carries no canonical model identity: the plan execution is invalid
// and must fail closed — never dispatch under a fabricated identity.
func TestHandleChat_ModellessRequest_FailsClosedWithoutDispatch(t *testing.T) {
	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	p := newTestProxyFullModel(t, up.URL, 1)
	rec, fc := wireObserverHarness(t, p)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":null,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	w := httptest.NewRecorder()
	p.HandleChat(w, req)

	require.Equal(t, http.StatusNotFound, w.Code, "invalid attempt identity fails closed as 404")
	require.Zero(t, atomic.LoadInt32(&hits))
	require.Empty(t, fc.snapshot(), "no observation may be recorded without canonical identity")
	require.Zero(t, snapshotQualityAttempts(rec))
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "failed reservation releases its lease exactly once")
	require.Zero(t, rec.GlobalInflight())
}

func TestSelectNextWithPlan_PlanlessHasNoLegacyLane(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)

	sel, err := p.selectNextWithPlan(nil)
	require.ErrorIs(t, err, scheduler.ErrNoAvailable)
	require.Nil(t, sel, "a plan-less failover step must never select")
}

func TestShouldRetryWithPlan_PlanlessNeverRetries(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)

	require.False(t, p.shouldRetryWithPlan(context.Background(), 0, errors.New("network"), nil))
	require.False(t, p.shouldRetryWithPlan(context.Background(), http.StatusTooManyRequests, nil, nil))
}

func TestShouldRetryWithPlan_UsesCanonicalAttemptVerdicts(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := chatProxyWithPlan(t, up.URL, 2, []int64{1, 2})

	sel, plan, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o",
		scheduler.AttemptPlanIdentity{RequestID: "req-retry", UserID: 1})
	require.NoError(t, err)
	require.NotNil(t, plan)
	defer sel.Release()

	require.True(t, p.shouldRetryWithPlan(context.Background(), 0, errors.New("dial failed"), plan),
		"not-sent network failure is retryable")
	require.True(t, p.shouldRetryWithPlan(context.Background(), http.StatusTooManyRequests, nil, plan),
		"ordinary 429 is retryable")
	require.False(t, p.shouldRetryWithPlan(context.Background(), http.StatusInternalServerError, nil, plan),
		"5xx never replays")
	require.False(t, p.shouldRetryWithPlan(context.Background(), http.StatusBadRequest, nil, plan))

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	require.False(t, p.shouldRetryWithPlan(cancelled, 0, errors.New("canceled"), plan),
		"client cancel is never a failover")
}

func TestRetryOutcomeForAttempt_CarriesCanonicalIdentity(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := chatProxyWithPlan(t, up.URL, 2, []int64{1, 2})

	sel, plan, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o",
		scheduler.AttemptPlanIdentity{RequestID: "req-ident", UserID: 1})
	require.NoError(t, err)
	defer sel.Release()
	attempt, ok := plan.CurrentAttempt()
	require.True(t, ok)
	require.NoError(t, attempt.Validate())

	o := retryOutcomeForAttempt(attempt, http.StatusTooManyRequests, nil, context.Background())
	require.NoError(t, o.Validate())
	require.Equal(t, AttemptID(attempt.AttemptID), o.ID)
	require.Equal(t, RouteClassID(attempt.RouteClassID), o.RouteClassID)
	require.Equal(t, QualityClassID(attempt.QualityClassID), o.QualityClassID)
	require.Equal(t, CandidateFingerprint(attempt.CandidateFingerprint), o.Fingerprint)
	require.Equal(t, attempt.TemplateID, o.TemplateID)
	require.Equal(t, attempt.AccountID, o.AccountID)
	require.Equal(t, Generation(attempt.RoutingGeneration), o.Generation)
	require.Equal(t, LifecycleRevision(attempt.LifecycleRevision), o.LifecycleRevision)
	require.Equal(t, LaneID(attempt.Lane), o.Lane)
	require.EqualValues(t, attempt.Ordinal, o.Ordinal)
}

func TestDispatchIdentity_PlanBackedSuccessCarriesCanonicalAttemptIdentity(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := chatProxyWithPlan(t, up.URL, 2, []int64{1, 2})
	rec, fc := wireObserverHarness(t, p)

	w := doChat(t, p)
	require.Equal(t, 200, w.Code)

	flow := fc.snapshot()
	require.Len(t, flow, 1)
	o := flow[0]
	want := expectedRouteClassBytes(t, 10, "gpt-4o")
	require.Equal(t, hex.EncodeToString(want[:]), string(o.RouteClassID),
		"dispatch identity must be the compiled route class")
	require.Len(t, string(o.QualityClassID), 64, "quality class must be the canonical hex id")
	require.Len(t, string(o.Fingerprint), 64, "fingerprint must be the candidate hash")
	require.True(t, strings.HasSuffix(string(o.ID), ":1"), "attempt id must be the plan-canonical reqID:ordinal")
	require.Equal(t, LanePrimary, o.Lane)
	require.Greater(t, int64(o.Generation), int64(0))
	require.EqualValues(t, 1, o.Ordinal)
	require.Nil(t, o.PreviousAttemptID)
	require.NoError(t, o.Validate())
	require.Equal(t, int64(1), snapshotQualityAttempts(rec))
}

// The failover loop is plan-only: a loop entered without a plan cannot
// advance to any second dispatch (no legacy selection lane to fall through to).
type reject429Attempt struct{ calls *atomic.Int32 }

func (a reject429Attempt) call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64,
	start time.Time, sel *scheduler.Selection, reqModel string, body []byte, st attemptState) (int, []byte, http.Header, bool, error) {
	a.calls.Add(1)
	return http.StatusTooManyRequests, []byte(`{"error":{"message":"slow down"}}`), nil, false, nil
}

func TestFailoverLoopWithPlan_PlanlessAdvancesNoFurtherDispatch(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := chatProxyWithPlan(t, up.URL, 2, []int64{1, 2})
	p.cfg.FailoverAttempts = 2

	var calls atomic.Int32
	sel, err := p.sched.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.failoverLoopWithPlan(httptest.NewRecorder(), req, domain.FormatOpenAIChat,
			"req-noplan-loop", 10, time.Now(), "gpt-4o", nil, sel, nil, attemptState{},
			reject429Attempt{calls: &calls}, &httpSink{}, false)
	}()
	<-done

	require.EqualValues(t, 1, calls.Load(), "exactly one dispatch: a retryable 429 must not advance without a plan")
	for id := int64(1); id <= 2; id++ {
		ri, ok := p.sched.Runtime(id)
		require.True(t, ok)
		require.Zero(t, ri.Concurrency, "account %d lease released exactly once", id)
	}
}
