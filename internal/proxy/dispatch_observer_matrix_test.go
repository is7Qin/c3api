// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/quality"
	"github.com/is7qin/c3api/internal/scheduler"
)

// --- observer harness: recorder + flow capture wired onto a test proxy ---

type flowCapture struct {
	mu       sync.Mutex
	outcomes []AttemptOutcome
}

func (f *flowCapture) append(o AttemptOutcome) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outcomes = append(f.outcomes, o)
}

func (f *flowCapture) snapshot() []AttemptOutcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]AttemptOutcome(nil), f.outcomes...)
}

func wireObserverHarness(t *testing.T, p *Proxy) (*quality.Recorder, *flowCapture) {
	t.Helper()
	rec, err := quality.NewRecorder(64)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rec.Close() })
	p.SetQualityRecorder(rec)
	fc := &flowCapture{}
	p.pipelineFlowAppend = fc.append
	return rec, fc
}

func snapshotQualityAttempts(rec *quality.Recorder) int64 {
	var total int64
	for _, rows := range rec.Snapshot().Quality {
		for _, qm := range rows {
			total += qm.Attempts()
		}
	}
	return total
}

func requireExactlyOneSuccessObservation(t *testing.T, rec *quality.Recorder, fc *flowCapture, cat CallerCategory) {
	t.Helper()
	flow := fc.snapshot()
	require.Len(t, flow, 1, "one dispatch must append exactly one flow outcome")
	require.Equal(t, cat, flow[0].CallerCategory)
	require.Equal(t, ResultSuccess, flow[0].Result)
	require.NoError(t, flow[0].Validate())
	require.NotZero(t, flow[0].TemplateID, "template identity must be real")
	require.NotZero(t, flow[0].AccountID, "account identity must be real")
	require.NotEmpty(t, flow[0].MappedModel, "mapped model identity must be real")
	require.EqualValues(t, 1, flow[0].Ordinal)
	require.Equal(t, int64(1), snapshotQualityAttempts(rec), "success dispatch must feed quality exactly once")
	require.Zero(t, rec.GlobalInflight(), "dispatch AttemptContext must be completed, not leaked")
}

// --- per-category success: one owner observation feeding quality + flow ---

func TestAttemptObserverMatrix_successFeedsQualityAndFlow_chat(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	rec, fc := wireObserverHarness(t, p)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec2 := httptest.NewRecorder()
	p.HandleChat(rec2, req)
	require.Equal(t, 200, rec2.Code)
	requireExactlyOneSuccessObservation(t, rec, fc, CallerChat)
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "success releases the lease exactly once")
}

func TestAttemptObserverMatrix_successFeedsQualityAndFlow_responses(t *testing.T) {
	up := fakeResponsesOutcome(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyResponses(t, up.URL, 1, store)
	rec, fc := wireObserverHarness(t, p)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4o","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec2 := httptest.NewRecorder()
	p.HandleResponses(rec2, req)
	require.Equal(t, 200, rec2.Code)
	requireExactlyOneSuccessObservation(t, rec, fc, CallerResponses)
}

func TestAttemptObserverMatrix_successFeedsQualityAndFlow_anthropic(t *testing.T) {
	up := fakeAnthropicOutcome(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyAnthropicCapture(t, up.URL, 1, store)
	rec, fc := wireObserverHarness(t, p)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"gpt-4o","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec2 := httptest.NewRecorder()
	p.HandleAnthropic(rec2, req)
	require.Equal(t, 200, rec2.Code)
	requireExactlyOneSuccessObservation(t, rec, fc, CallerAnthropic)
}

func TestAttemptObserverMatrix_successFeedsQualityAndFlow_converted(t *testing.T) {
	up := &capturedUpstream{}
	srv := up.srv(t)
	defer srv.Close()
	store := &captureLogStore{}
	p := newConvertedTestProxyLogs(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIResponses},
		[]domain.ProtocolConvert{domain.ProtocolConvertChatToResp}, store, 30*time.Second)
	rec, fc := wireObserverHarness(t, p)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec2 := httptest.NewRecorder()
	p.HandleChat(rec2, req)
	require.Equal(t, 200, rec2.Code)
	requireExactlyOneSuccessObservation(t, rec, fc, CallerConverted)
}

func TestAttemptObserverMatrix_successFeedsQualityAndFlow_images(t *testing.T) {
	up, _ := fakeImagesUpstream(t, "/v1/images/generations")
	defer up.Close()
	p, _ := newTestImagesProxy(t, up.URL, nil)
	rec, fc := wireObserverHarness(t, p)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(
		`{"model":"gpt-image-1","prompt":"a cat","n":2}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec2 := httptest.NewRecorder()
	p.HandleImagesGenerations(rec2, req)
	require.Equal(t, 200, rec2.Code)
	requireExactlyOneSuccessObservation(t, rec, fc, CallerImages)
}

func TestAttemptObserverMatrix_successFeedsQualityAndFlow_imagesCodex(t *testing.T) {
	up, _ := newCodexImageUpstream(t, codexUpStep{status: 200, body: codexTestImageResponse})
	defer up.Close()
	pat := "pat-key-1"
	ext := &domain.AccountExt{
		AccountID: 11, CredentialType: credential.TypeCodexPAT,
		CodexIdentity: &domain.CodexIdentity{InstallationID: "inst-" + strings.Repeat("1", 32)},
		CodexPATKey:   &pat,
	}
	store := &captureLogStore{}
	p, _ := newTestCodexProxy(t, credential.TypeCodexPAT, map[int64]*domain.AccountExt{11: ext}, up.URL, nil, store)
	rec, fc := wireObserverHarness(t, p)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(
		`{"model":"gpt-image-2","prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec2 := httptest.NewRecorder()
	p.HandleImagesGenerations(rec2, req)
	require.Equal(t, 200, rec2.Code, "body=%s", rec2.Body.String())
	requireExactlyOneSuccessObservation(t, rec, fc, CallerImagesCodex)
}

func TestAttemptObserverMatrix_successFeedsQualityAndFlow_codexHTTP(t *testing.T) {
	up, _ := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{t6RespCreated, t6RespItemEv, t6RespDone}})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-10")}, up.URL, nil, nil, store)
	rec, fc := wireObserverHarness(t, p)
	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()
	resp := postResponses(t, srv, `{"model":"gpt-4o","input":"hi"}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	requireExactlyOneSuccessObservation(t, rec, fc, CallerCodexHTTP)
}

func TestAttemptObserverMatrix_successFeedsQualityAndFlow_search(t *testing.T) {
	up, _ := newCodexSearchUpstream(t, codexSearchStep{status: 200, body: searchRespRaw})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestSearchProxy(t, []searchTestAcct{{id: 10, tplID: 1, credType: credential.TypeAPIKey, key: "sk-upstream"}},
		up.URL, searchBillingHooks(nil), store)
	rec, fc := wireObserverHarness(t, p)
	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()
	resp := postSearch(t, srv, searchReqBody, "")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	requireExactlyOneSuccessObservation(t, rec, fc, CallerSearch)
}

func TestAttemptObserverMatrix_successFeedsQualityAndFlow_responsesWS(t *testing.T) {
	hooks := &fakeWSHooks{frameLimit: 1}
	up := fakeResponsesWS(t, hooks)
	defer up.Close()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS},
		Models:           []string{"gpt-4o"},
	}
	store := &captureLogStore{}
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, nil)
	rec, fc := wireObserverHarness(t, p)
	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer srv.Close()
	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	for i := 0; i < 4; i++ {
		_ = readResponsesWSFrame(t, c)
	}
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)
	requireExactlyOneSuccessObservation(t, rec, fc, CallerResponsesWS)
}

func TestAttemptObserverMatrix_successFeedsQualityAndFlow_codexWS(t *testing.T) {
	up, _ := newCodexWSUpstream(t, []int{200}, 1)
	pat := "pat-1"
	store := &captureLogStore{}
	p, _ := newTestCodexWSProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: {
			AccountID: 10, CredentialType: credential.TypeCodexPAT,
			CodexIdentity: &domain.CodexIdentity{InstallationID: "inst-" + strings.Repeat("2", 32)},
			CodexPATKey:   &pat,
		}}, up.URL, nil, store)
	rec, fc := wireObserverHarness(t, p)
	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer srv.Close()
	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	for i := 0; i < 4; i++ {
		_ = readResponsesWSFrame(t, c)
	}
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)
	requireExactlyOneSuccessObservation(t, rec, fc, CallerCodexWS)
}

// --- retry chain: ordinals + previous linkage preserved through flow ---

func TestAttemptObserverMatrix_retryChainOrdinalsPreserved(t *testing.T) {
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
	p := newTestProxy(t, up.URL, 1)
	loader := p.sched.Loader().(noopLoader)
	tpl2 := &domain.Template{ID: 2, Name: "t2", BaseURL: up.URL, CredentialType: credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"}}
	loader.accs[10] = append(loader.accs[10], &domain.Account{ID: 2, TemplateID: 2, Template: tpl2,
		UpstreamKey: "sk-upstream", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4})
	require.NoError(t, p.sched.InvalidateAllSync())
	p.cfg.FailoverAttempts = 2
	rec, fc := wireObserverHarness(t, p)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec2 := httptest.NewRecorder()
	p.HandleChat(rec2, req)
	require.Equal(t, 200, rec2.Code)

	flow := fc.snapshot()
	require.Len(t, flow, 2, "two dispatches must append two flow outcomes")
	require.EqualValues(t, 1, flow[0].Ordinal)
	require.EqualValues(t, 2, flow[1].Ordinal, "chain ordinal must advance on retry")
	require.Nil(t, flow[0].PreviousAttemptID)
	require.NotNil(t, flow[1].PreviousAttemptID, "retry must link to the previous attempt")
	require.Equal(t, flow[0].ID, *flow[1].PreviousAttemptID)
	require.Equal(t, ResultFailed, flow[0].Result)
	require.Equal(t, ResultSuccess, flow[1].Result)
	require.Equal(t, int64(2), snapshotQualityAttempts(rec), "both dispatches count in quality")
	for _, id := range []int64{1, 2} {
		ri, ok := p.sched.Runtime(id)
		require.True(t, ok)
		require.Zero(t, ri.Concurrency, "account %d lease released exactly once", id)
	}
	require.Zero(t, rec.GlobalInflight())
}

// --- client cancel: flow-only, excluded from quality ---

func TestAttemptObserverMatrix_clientCancelIsFlowOnly(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyTimeoutLogs(t, up.URL, 1, store)
	rec, fc := wireObserverHarness(t, p)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer ck-1")
	rec2 := httptest.NewRecorder()
	p.HandleChat(rec2, req)

	flow := fc.snapshot()
	require.Len(t, flow, 1, "cancel must still be recorded in flow")
	require.Equal(t, ResultClientCancel, flow[0].Result)
	require.Zero(t, snapshotQualityAttempts(rec), "cancel must be excluded from the quality denominator")
	require.Zero(t, rec.GlobalInflight())
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency)
	require.Zero(t, ri.ErrCount, "client cancel must not cool the account")
}

// --- local reject: not an attempt, no observation, lease released once ---

func TestAttemptObserverMatrix_localRejectIsNotAnAttempt(t *testing.T) {
	up, _ := newCodexImageUpstream(t, codexUpStep{status: 200, body: codexTestImageResponse})
	defer up.Close()
	pat := "pat-key-1"
	ext := &domain.AccountExt{
		AccountID: 11, CredentialType: credential.TypeCodexPAT,
		CodexIdentity: &domain.CodexIdentity{InstallationID: "inst-" + strings.Repeat("1", 32)},
		CodexPATKey:   &pat,
	}
	store := &captureLogStore{}
	p, _ := newTestCodexProxy(t, credential.TypeCodexPAT, map[int64]*domain.AccountExt{11: ext}, up.URL, nil, store)
	rec, fc := wireObserverHarness(t, p)
	// valid JSON (passes the gate) but prompt missing → codex images params
	// local reject after selection: upstream never touched.
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(
		`{"model":"gpt-image-2"}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec2 := httptest.NewRecorder()
	p.HandleImagesGenerations(rec2, req)
	require.Equal(t, http.StatusBadRequest, rec2.Code)

	require.Empty(t, fc.snapshot(), "local reject must not append flow")
	require.Zero(t, snapshotQualityAttempts(rec), "local reject must not count in quality")
	require.Zero(t, rec.GlobalInflight(), "dispatch AttemptContext must be abandoned, not leaked")
	ri, ok := p.sched.Runtime(11)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "local reject releases the lease exactly once")
	require.Zero(t, ri.ErrCount, "local reject must not mark health")
}

// --- post-commit failure: quality drops, no failover, single MarkResult ---

func TestAttemptObserverMatrix_postCommitFailureNoFailover(t *testing.T) {
	// upstream starts SSE then stalls forever → UpstreamStreamTimeout fires after
	// the first frame was committed to the client: post-commit failure.
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
		ID: 1, Name: "t", BaseURL: up.URL, CredentialType: credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
	}, 1, true, 150*time.Millisecond, store, nil)
	loader := p.sched.Loader().(noopLoader)
	tpl2 := &domain.Template{ID: 2, Name: "t2", BaseURL: up.URL, CredentialType: credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"}}
	loader.accs[10] = append(loader.accs[10], &domain.Account{ID: 2, TemplateID: 2, Template: tpl2,
		UpstreamKey: "sk-upstream", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4})
	require.NoError(t, p.sched.InvalidateAllSync())
	p.cfg.FailoverAttempts = 2
	rec, fc := wireObserverHarness(t, p)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec2 := httptest.NewRecorder()
	p.HandleChat(rec2, req)

	flow := fc.snapshot()
	require.Len(t, flow, 1, "post-commit failure must terminate the request (no second dispatch)")
	require.Equal(t, ResultFailed, flow[0].Result)
	require.True(t, flow[0].HasPossiblyWrittenBytes())
	require.False(t, CanRetry(flow[0].CallerCategory, flow[0]))
	require.Equal(t, int64(1), snapshotQualityAttempts(rec), "post-commit failure lowers quality")
	require.Zero(t, rec.GlobalInflight())
}

// --- failure path: loop keeps the single MarkResult owner, one observation ---

func TestAttemptObserverMatrix_preResponse429SingleMarkResult(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	p.cfg.FailoverAttempts = 1
	rec, fc := wireObserverHarness(t, p)
	sel, plan, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o",
		scheduler.AttemptPlanIdentity{RequestID: "req-429", UserID: 1})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	p.failoverLoopWithPlan(httptest.NewRecorder(), req, domain.FormatOpenAIChat, domain.FormatOpenAIChat,
		"req-429", 10, time.Now(), "gpt-4o", nil, sel, plan, attemptState{},
		rejectedPipelineAttempt{code: http.StatusTooManyRequests}, &httpSink{}, false)

	flow := fc.snapshot()
	require.Len(t, flow, 1)
	require.Equal(t, ResultFailed, flow[0].Result)
	require.Equal(t, AttemptStatus(http.StatusTooManyRequests), flow[0].HTTPStatus)
	require.Equal(t, CommitUpstreamResponded, flow[0].Commit)
	require.Equal(t, int64(1), snapshotQualityAttempts(rec))
	p.sched.FlushRules()
	ri, ok := p.sched.Runtime(sel.AccountID)
	require.True(t, ok)
	require.Equal(t, 1, ri.ErrCount, "failover classification remains the single MarkResult owner")
	require.Zero(t, ri.Concurrency)
	require.Zero(t, rec.GlobalInflight())
}

// --- panic supervision: quality pin must not outlive the dispatch ---

func TestAttemptObserverMatrix_panicDuringDispatchAbandonsPin(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	rec, fc := wireObserverHarness(t, p)
	sel, plan, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o",
		scheduler.AttemptPlanIdentity{RequestID: "req-panic", UserID: 1})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	require.Panics(t, func() {
		p.failoverLoopWithPlan(httptest.NewRecorder(), req, domain.FormatOpenAIChat, domain.FormatOpenAIChat,
			"req-panic", 10, time.Now(), "gpt-4o", nil, sel, plan, attemptState{},
			panickingAttempt{}, &httpSink{}, false)
	})
	require.Empty(t, fc.snapshot(), "panic without a terminal outcome must not fabricate flow")
	require.Zero(t, snapshotQualityAttempts(rec))
	require.Zero(t, rec.GlobalInflight(), "owner cleanup must release the quality pin on panic")
	ri, ok := p.sched.Runtime(sel.AccountID)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "panic guard releases the lease exactly once")
}

type panickingAttempt struct{}

func (panickingAttempt) call(context.Context, http.ResponseWriter, *http.Request, string, int64, time.Time,
	*scheduler.Selection, string, []byte, attemptState) (int, []byte, http.Header, bool, error) {
	panic("dispatch panic")
}

// --- dispatch-time ownership: the AttemptContext exists while the call runs ---

func TestAttemptObserverMatrix_attemptContextBeginsAtDispatch(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	rec, fc := wireObserverHarness(t, p)
	sel, plan, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o",
		scheduler.AttemptPlanIdentity{RequestID: "req-begin", UserID: 1})
	require.NoError(t, err)

	inflightDuringCall := make(chan int64, 1)
	proceed := make(chan struct{})
	att := &barrierAttempt{fn: func(ctx context.Context, s *scheduler.Selection) (int, []byte, http.Header, bool, error) {
		inflightDuringCall <- rec.GlobalInflight()
		<-proceed
		return http.StatusOK, nil, nil, true, nil
	}}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.failoverLoopWithPlan(httptest.NewRecorder(), req, domain.FormatOpenAIChat, domain.FormatOpenAIChat,
			"req-begin", 10, time.Now(), "gpt-4o", nil, sel, plan, attemptState{}, att, &httpSink{}, false)
	}()
	got := <-inflightDuringCall
	// The barrier attempt returns handled=true without completing the observer;
	// owner cleanup abandons it when the loop unwinds.
	close(proceed)
	<-done
	require.EqualValues(t, 1, got, "AttemptContext must be created by the owner at dispatch time, before the call returns")
	require.Empty(t, fc.snapshot(), "uncompleted dispatch must not fabricate a flow outcome")
	require.Zero(t, rec.GlobalInflight())
}

type barrierAttempt struct {
	fn func(ctx context.Context, sel *scheduler.Selection) (int, []byte, http.Header, bool, error)
}

func (b *barrierAttempt) call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64,
	start time.Time, sel *scheduler.Selection, reqModel string, body []byte, st attemptState) (int, []byte, http.Header, bool, error) {
	return b.fn(ctx, sel)
}
