// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/quality"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/scheduler"
)

// --- flow-chain producer harness: plan-backed proxy + recorder + tap ---

func collectFlowRows(rec *quality.Recorder) []repository.RoutingFlowRow {
	var out []repository.RoutingFlowRow
	for _, fm := range rec.Snapshot().Flow {
		out = append(out, fm.FlowRows()...)
	}
	return out
}

func sumChainCount(rows []repository.RoutingFlowRow) int64 {
	var sum int64
	for _, r := range rows {
		sum += r.ChainCount
	}
	return sum
}

func expectedRouteClassBytes(t *testing.T, groupID int64, model string) [32]byte {
	t.Helper()
	id, err := domain.RouteClassID(groupID, domain.FormatOpenAIChat, model, domain.OpChatCompletions)
	require.NoError(t, err)
	return id
}

func chatBody() *strings.Reader {
	return strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
}

func doChat(t *testing.T, p *Proxy) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", chatBody())
	req.Header.Set("Authorization", "Bearer ck-1")
	rec2 := httptest.NewRecorder()
	p.HandleChat(rec2, req)
	return rec2
}

// --- success: one canonical terminal edge, conserved into the recorder ---

func TestFlowChainProducer_planBackedSuccessEnqueuesCanonicalRow(t *testing.T) {
	quality.ResetFlowChainCountersForTest()
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := chatProxyWithPlan(t, up.URL, 2, []int64{1, 2})
	rec, fc := wireObserverHarness(t, p)

	rec2 := doChat(t, p)
	require.Equal(t, 200, rec2.Code)

	rows := collectFlowRows(rec)
	require.Len(t, rows, 1, "one dispatched request must produce exactly one flow edge")
	require.Equal(t, int64(1), rows[0].ChainCount)
	require.EqualValues(t, 1, rows[0].Ordinal)
	require.True(t, rows[0].IsTerminal)
	require.Equal(t, "success", rows[0].Outcome)
	require.Equal(t, "initial", rows[0].TransitionReason)
	require.Nil(t, rows[0].PreviousAccountID)
	require.NotEmpty(t, rows[0].Lane)
	require.Greater(t, rows[0].Generation, int64(0))
	want := expectedRouteClassBytes(t, 10, "gpt-4o")
	require.Equal(t, want, [32]byte(rows[0].RouteClassID), "route class must be the real compiled identity")
	require.NotEqual(t, [32]byte{}, [32]byte(rows[0].CandidateFingerprint), "fingerprint must be real, never synthetic")
	require.NotZero(t, rows[0].AccountID)

	require.Len(t, fc.snapshot(), 1, "the observation tap still receives every completed outcome")
	require.Equal(t, int64(1), snapshotQualityAttempts(rec))
	require.Zero(t, quality.FlowChainIncompleteObserved(), "a completed chain must not count incomplete")
	require.Zero(t, rec.GlobalInflight())
}

// --- retry 429 → success: two edges, real previous linkage ---

func TestFlowChainProducer_retry429ThenSuccessConservesChain(t *testing.T) {
	quality.ResetFlowChainCountersForTest()
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "slow down"}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "c1", "object": "chat.completion",
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 2, "completion_tokens": 3, "total_tokens": 5},
		})
	}))
	defer up.Close()
	p := chatProxyWithPlan(t, up.URL, 2, []int64{1, 2})
	p.cfg.FailoverAttempts = 2
	rec, _ := wireObserverHarness(t, p)

	rec2 := doChat(t, p)
	require.Equal(t, 200, rec2.Code)

	rows := collectFlowRows(rec)
	require.Len(t, rows, 2, "both dispatches must be conserved as distinct edges")
	require.EqualValues(t, 1, rows[0].Ordinal)
	require.Equal(t, "429", rows[0].Outcome)
	require.False(t, rows[0].IsTerminal)
	require.EqualValues(t, 2, rows[1].Ordinal)
	require.Equal(t, "success", rows[1].Outcome)
	require.True(t, rows[1].IsTerminal)
	require.Equal(t, "failover", rows[1].TransitionReason)
	require.Equal(t, "429", rows[1].PreviousOutcome, "previous outcome must be the real recorded edge")
	require.NotNil(t, rows[1].PreviousAccountID)
	require.Equal(t, rows[0].AccountID, *rows[1].PreviousAccountID)
	require.Zero(t, quality.FlowChainIncompleteObserved())
	require.Equal(t, int64(2), snapshotQualityAttempts(rec))
}

// --- exhaustion: the last real edge is finalized, never fabricated ---

func TestFlowChainProducer_exhaustionFinalizesLastEdge(t *testing.T) {
	quality.ResetFlowChainCountersForTest()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "slow down"}})
	}))
	defer up.Close()
	p := chatProxyWithPlan(t, up.URL, 2, []int64{1, 2})
	p.cfg.FailoverAttempts = 2
	rec, _ := wireObserverHarness(t, p)

	rec2 := doChat(t, p)
	require.Equal(t, http.StatusTooManyRequests, rec2.Code)

	rows := collectFlowRows(rec)
	require.Len(t, rows, 2, "every real dispatch is conserved on exhaustion")
	require.False(t, rows[0].IsTerminal)
	require.True(t, rows[1].IsTerminal, "the last edge is finalized on exhaustion")
	require.Equal(t, "429", rows[1].Outcome)
	require.Zero(t, quality.FlowChainIncompleteObserved(), "an exhausted chain is complete, not incomplete")
}

// --- handled failure (4xx): terminal edge, complete path, not incomplete ---

func TestFlowChainProducer_handledFailureCompletesNotIncomplete(t *testing.T) {
	quality.ResetFlowChainCountersForTest()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "bad"}})
	}))
	defer up.Close()
	p := chatProxyWithPlan(t, up.URL, 2, []int64{1, 2})
	rec, _ := wireObserverHarness(t, p)

	rec2 := doChat(t, p)
	require.Equal(t, http.StatusBadRequest, rec2.Code)

	rows := collectFlowRows(rec)
	require.Len(t, rows, 1)
	require.True(t, rows[0].IsTerminal)
	require.Equal(t, "4xx", rows[0].Outcome)
	require.Zero(t, quality.FlowChainIncompleteObserved(), "handled failure must never mark incomplete")
}

// --- client cancel: in flow, excluded from quality ---

func TestFlowChainProducer_clientCancelIsFlowOnly(t *testing.T) {
	quality.ResetFlowChainCountersForTest()
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := chatProxyWithPlan(t, up.URL, 2, []int64{1, 2})
	rec, _ := wireObserverHarness(t, p)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer ck-1")
	rec2 := httptest.NewRecorder()
	p.HandleChat(rec2, req)

	rows := collectFlowRows(rec)
	require.Len(t, rows, 1, "client cancel must be included in flow")
	require.Equal(t, "client_cancel", rows[0].Outcome)
	require.True(t, rows[0].IsTerminal)
	require.Zero(t, snapshotQualityAttempts(rec), "client cancel must stay out of the quality denominator")
	require.Zero(t, quality.FlowChainIncompleteObserved())
	require.Zero(t, rec.GlobalInflight())
}

// --- post-commit failure: single terminal edge, quality lowered ---

func TestFlowChainProducer_postCommitFailureSingleTerminalEdge(t *testing.T) {
	quality.ResetFlowChainCountersForTest()
	release := make(chan struct{})
	defer close(release)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		fl.Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyTplTimeoutLogs(t, &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL, CredentialType: "api_key",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
	}, 1, true, 150*time.Millisecond, store, nil)
	route := scheduler.RouteRefFor(10, string(domain.FormatOpenAIChat), "gpt-4o")
	p.sched.PublishDecisionForTest(route, &scheduler.RouteDecision{Primary: []int64{1}})
	p.cfg.FailoverAttempts = 2
	rec, _ := wireObserverHarness(t, p)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec2 := httptest.NewRecorder()
	p.HandleChat(rec2, req)

	rows := collectFlowRows(rec)
	require.Len(t, rows, 1, "post-commit failure terminates the flow with one edge")
	require.True(t, rows[0].IsTerminal)
	require.Equal(t, int64(1), snapshotQualityAttempts(rec))
	require.Zero(t, quality.FlowChainIncompleteObserved())
}

// --- panic: chain closed exactly once as incomplete, no rows ---

func TestFlowChainProducer_panicClosesChainAsIncomplete(t *testing.T) {
	quality.ResetFlowChainCountersForTest()
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := chatProxyWithPlan(t, up.URL, 2, []int64{1, 2})
	rec, fc := wireObserverHarness(t, p)
	sel, plan, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o",
		scheduler.AttemptPlanIdentity{RequestID: "req-flow-panic", UserID: 1})
	require.NoError(t, err)
	require.NotNil(t, plan)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	require.Panics(t, func() {
		p.failoverLoopWithPlan(httptest.NewRecorder(), req, domain.FormatOpenAIChat, domain.FormatOpenAIChat,
			"req-flow-panic", 10, time.Now(), "gpt-4o", nil, sel, plan, attemptState{},
			panickingAttempt{}, &httpSink{}, false)
	})
	require.Empty(t, collectFlowRows(rec), "a panicked chain must not enqueue rows")
	require.Empty(t, fc.snapshot())
	require.Equal(t, int64(1), quality.FlowChainIncompleteObserved(), "panic closes the chain as incomplete")
	require.Zero(t, rec.GlobalInflight())
	ri, ok := p.sched.Runtime(sel.AccountID)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "panic guard releases the lease exactly once")
}

// --- duplicate completion: the chain records the edge once ---

type doubleCompleteAttempt struct{}

func (doubleCompleteAttempt) call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64,
	start time.Time, sel *scheduler.Selection, reqModel string, body []byte, st attemptState) (int, []byte, http.Header, bool, error) {
	d := dispatchFromContext(ctx)
	outcome := mergeDispatchBase(ctx, AttemptOutcome{
		Result: ResultSuccess, HTTPStatus: 200, Commit: CommitClientCommitted,
		BusinessFrameSent: true, Terminal: true, Timing: AttemptTiming{LatencyMS: 5},
		CallerCategory: CallerChat, OperationTag: "chat_completions",
	})
	_ = d.observer.Complete(outcome, nil)
	_ = d.observer.Complete(outcome, nil) // duplicate completion must be a no-op
	return 200, nil, nil, true, nil
}

func TestFlowChainProducer_duplicateCompletionAppendsExactlyOnce(t *testing.T) {
	quality.ResetFlowChainCountersForTest()
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := chatProxyWithPlan(t, up.URL, 2, []int64{1, 2})
	rec, _ := wireObserverHarness(t, p)
	sel, plan, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o",
		scheduler.AttemptPlanIdentity{RequestID: "req-flow-dup", UserID: 1})
	require.NoError(t, err)
	require.NotNil(t, plan)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	p.failoverLoopWithPlan(httptest.NewRecorder(), req, domain.FormatOpenAIChat, domain.FormatOpenAIChat,
		"req-flow-dup", 10, time.Now(), "gpt-4o", nil, sel, plan, attemptState{},
		doubleCompleteAttempt{}, &httpSink{}, false)

	rows := collectFlowRows(rec)
	require.Len(t, rows, 1, "duplicate completion must append exactly one edge")
	require.Equal(t, int64(1), rows[0].ChainCount)
	require.Zero(t, quality.FlowChainIncompleteObserved())
	require.Zero(t, rec.GlobalInflight())
}

// --- no synthetic identity: plan-less dispatches never produce rows ---
// Driven through failoverLoop (the plan-less lane, plan=nil by construction)
// so the invariant holds regardless of whether the fixture scheduler has
// published decisions for this route.

func TestFlowChainProducer_legacyRequestProducesNoRows(t *testing.T) {
	quality.ResetFlowChainCountersForTest()
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	rec, fc := wireObserverHarness(t, p)
	sel, err := p.sched.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	p.failoverLoop(httptest.NewRecorder(), req, domain.FormatOpenAIChat, domain.FormatOpenAIChat,
		"req-flow-legacy", 10, time.Now(), "gpt-4o", nil, sel, attemptState{},
		singleCompleteAttempt{}, &httpSink{}, false)
	sel.Release()

	require.Empty(t, collectFlowRows(rec), "placeholder identity must never flow into production rows")
	require.Len(t, fc.snapshot(), 1, "the legacy observation tap is unaffected")
	require.Equal(t, int64(1), snapshotQualityAttempts(rec))
	require.Zero(t, quality.FlowChainIncompleteObserved())
	require.Zero(t, rec.GlobalInflight())
}

type singleCompleteAttempt struct{}

func (singleCompleteAttempt) call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64,
	start time.Time, sel *scheduler.Selection, reqModel string, body []byte, st attemptState) (int, []byte, http.Header, bool, error) {
	d := dispatchFromContext(ctx)
	outcome := mergeDispatchBase(ctx, AttemptOutcome{
		Result: ResultSuccess, HTTPStatus: 200, Commit: CommitClientCommitted,
		BusinessFrameSent: true, Terminal: true, Timing: AttemptTiming{LatencyMS: 5},
		CallerCategory: CallerChat, OperationTag: "chat_completions",
	})
	_ = d.observer.Complete(outcome, nil)
	return 200, nil, nil, true, nil
}

// --- concurrent same-minute requests: conservation across chains ---

func TestFlowChainProducer_concurrentRequestsConserveSameMinute(t *testing.T) {
	quality.ResetFlowChainCountersForTest()
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := chatProxyWithPlan(t, up.URL, 4, []int64{1, 2, 3, 4})
	rec, _ := wireObserverHarness(t, p)

	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			rec2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", chatBody())
			rec2.Header.Set("Authorization", "Bearer ck-1")
			w := httptest.NewRecorder()
			p.HandleChat(w, rec2)
		}()
	}
	close(start)
	wg.Wait()

	rows := collectFlowRows(rec)
	require.Equal(t, int64(n), sumChainCount(rows), "every same-minute request must be conserved exactly once")
	require.Equal(t, int64(n), snapshotQualityAttempts(rec))
	require.Zero(t, quality.FlowChainIncompleteObserved())
	require.Zero(t, rec.GlobalInflight())
}

// --- adapter purity: only real attempt metadata reaches the edge ---

func TestFlowDispatchAdapter_usesRealAttemptMetadataOnly(t *testing.T) {
	prev := "req-x:1"
	prevAcct := int64(7)
	a := scheduler.Attempt{
		AttemptID: "req-x:2", RouteClassID: strings.Repeat("a", 64), QualityClassID: strings.Repeat("b", 64),
		CandidateFingerprint: strings.Repeat("c", 64), TemplateID: 3, AccountID: 9,
		RequestedModel: "gpt-4o", MappedModel: "gpt-4o-0806", Lane: scheduler.AttemptLaneExplore,
		Ordinal: 2, RoutingGeneration: 5, LifecycleRevision: 4,
		PreviousAttemptID: &prev, PreviousAccountID: &prevAcct,
		CallerCategory: "chat", OperationTag: "chat_completions",
	}
	require.NoError(t, a.Validate())
	o := pipelineBase(a)
	o.Result = ResultFailed
	o.HTTPStatus = 503
	o.Commit = CommitUpstreamResponded
	o.Terminal = true

	d := flowDispatchFromAttempt(a, o, "429")
	require.Equal(t, a.RouteClassID, d.RouteClassID)
	require.Equal(t, a.QualityClassID, d.QualityClassID)
	require.Equal(t, a.CandidateFingerprint, d.Fingerprint)
	require.Equal(t, a.TemplateID, d.TemplateID)
	require.Equal(t, a.AccountID, d.AccountID)
	require.Equal(t, a.RequestedModel, d.RequestedModel)
	require.Equal(t, a.MappedModel, d.MappedModel)
	require.Equal(t, int64(a.RoutingGeneration), d.Generation)
	require.Equal(t, a.LifecycleRevision, d.LifecycleRevision)
	require.Equal(t, a.Ordinal, d.Ordinal)
	require.Equal(t, string(a.Lane), d.Lane)
	require.Equal(t, a.PreviousAttemptID, d.PreviousAttemptID)
	require.Equal(t, a.PreviousAccountID, d.PreviousAccountID)
	require.Equal(t, "429", d.PreviousOutcome)
	require.Equal(t, "failover", d.TransitionReason)
	require.Equal(t, "5xx", d.Outcome)
	require.True(t, d.IsTerminal)

	rc, err := hex.DecodeString(d.RouteClassID)
	require.NoError(t, err)
	require.Len(t, rc, 32)
}
