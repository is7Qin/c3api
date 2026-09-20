// SPDX-License-Identifier: AGPL-3.0-or-later
// Hard-continuation wiring: the Responses REST/WS callers bind every produced
// response id to the canonical plan attempt identity (account/fingerprint/
// lifecycle revision) through internal/continuation BEFORE the id becomes
// visible to the client, and resolve previous_response_id to a pinned dispatch
// that never migrates. Ordinary (non-Responses) requests issue zero Redis
// commands. Missing/expired/conflicting/stale/Redis-outage all fail closed.
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/coder/websocket"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/continuation"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
	"github.com/is7qin/c3api/pkg/redisx"
)

// --- fixtures ---

// contHook counts Redis commands and can gate the first script run on a
// channel (ACK-before-visible proofs, no sleeps).
type contHook struct {
	n       atomic.Int64
	mu      sync.Mutex
	gate    chan struct{} // non-nil = block script commands until closed
	started chan struct{} // closed once when a gated command arrives
	once    sync.Once
}

func (h *contHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *contHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.n.Add(1)
		h.mu.Lock()
		gate := h.gate
		h.mu.Unlock()
		if gate != nil && (cmd.Name() == "evalsha" || cmd.Name() == "eval") {
			h.once.Do(func() { close(h.started) })
			<-gate
		}
		return next(ctx, cmd)
	}
}
func (h *contHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		h.n.Add(int64(len(cmds)))
		return next(ctx, cmds)
	}
}

func (h *contHook) setGate(gate chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gate = gate
}

// contFixture wires a miniredis-backed continuation store plus the counting
// hook, and warms both Lua scripts so production command counts are exact.
func contFixture(t *testing.T) (*miniredis.Miniredis, *continuation.Store, *contHook) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })
	h := &contHook{started: make(chan struct{})}
	c.AddHook(h)
	s, err := continuation.New(c, "cont-test-secret-0123456789")
	require.NoError(t, err)
	ctx := context.Background()
	warm := domain.RouteClassIDVal{}
	_, err = s.CreateOrRefresh(ctx, 1, 1, warm, "warm", "warm-rest", 1, contFP(t, 1, "warm"), 1)
	require.NoError(t, err)
	_, _, err = s.Lookup(ctx, 1, 1, warm, "warm", "warm-lookup")
	require.NoError(t, err)
	h.n.Store(0)
	return mr, s, h
}

func contFP(t *testing.T, accountID int64, key string) domain.CandidateFingerprintVal {
	t.Helper()
	f, err := domain.CandidateFingerprint(accountID, 1, credential.TypeAPIKey, "https://cont.invalid", key, "", "", "", false, "", "", "", "")
	require.NoError(t, err)
	return f
}

func contRouteID(t *testing.T, format domain.RequestFormat, model string, op domain.OperationTag) domain.RouteClassIDVal {
	t.Helper()
	id, err := domain.RouteClassID(10, format, model, op)
	require.NoError(t, err)
	return id
}

// contAcc builds a responses-capable account fixture (fingerprint-stable).
func contAcc(id int64, tpl *domain.Template, key, baseURL string) *domain.Account {
	bu := baseURL
	return &domain.Account{ID: id, TemplateID: tpl.ID, Template: tpl, BaseURL: &bu, UpstreamKey: key, Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 4}
}

// contProxy builds a Responses (or resp-ws) proxy over the given accounts with
// the continuation store wired (nil store = unwired).
func contProxy(t *testing.T, format domain.RequestFormat, accs []*domain.Account, store *continuation.Store) *Proxy {
	t.Helper()
	tpl := accs[0].Template
	loader := noopLoader{accs: map[int64][]*domain.Account{10: accs}}
	rec := usage.New(usage.UsageConfig{BatchSize: 100, FlushInterval: time.Hour, QuotaFlushInterval: time.Hour}, &captureLogStore{}, nil)
	cfg := Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second,
		UsageCapture: true,
	}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil, testHealthSink, nil)
	require.NoError(t, re.Reload(context.Background()))
	sched := scheduler.New(scheduler.Config{SyncInterval: time.Hour}, loader, re, nil, nil, nil, nil)
	require.NoError(t, sched.InvalidateAllSync())
	publishTestRoutes(t, sched)
	// Production compiler publishes per-model route buckets (scheduler.go
	// buildRoutes); the continuation key is the request-canonical route class,
	// so the plan must resolve the model bucket (publishTestRoutes only covers
	// the "" default bucket).
	ids := make([]int64, 0, len(accs))
	for _, a := range accs {
		ids = append(ids, a.ID)
	}
	for _, m := range tpl.Models {
		compiled := make([]scheduler.CompiledCandidate, len(ids))
		for i, id := range ids {
			compiled[i] = scheduler.CompiledCandidate{AccountID: id}
		}
		sched.PublishDecisionForTest(scheduler.RouteRefFor(10, string(format), m), &scheduler.RouteDecision{Primary: compiled})
	}
	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{"ck-1": activeKey(1, 1, 10)}}, noopUserLoader{}, nil, true)
	require.NoError(t, auth.Reload(context.Background()))
	clients := aiclient.NewFactory(&http.Client{Transport: http.DefaultTransport}, aiclient.Config{UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second})
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, FlushInterval: time.Hour}, noopErrLogStore{}, nil)
	p := New(cfg, sched, credential.New(), rec, clients, auth, nil, nil, errlogW, Deps{Continuation: store})
	t.Cleanup(func() { _ = p.rec.Close(context.Background()) })
	return p
}

// contUpstream is a Responses fake: records auth header + hit count, returns a
// non-streaming response with the given id (or fails per mode). Modes "500" /
// "429" fail always; "500-after-1" / "429-after-1" fail from the second hit on
// (the first create succeeds, the pinned continuation then fails closed).
func contUpstream(t *testing.T, respID, key, mode string, hits *atomic.Int32, authSeen *atomic.Value) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		authSeen.Store(r.Header.Get("Authorization"))
		failMode := mode
		if strings.HasSuffix(mode, "-after-1") {
			failMode = ""
			if n > 1 {
				failMode = strings.TrimSuffix(mode, "-after-1")
			}
		}
		if failMode == "500" {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
			return
		}
		if failMode == "429" {
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if b, _ := body["stream"].(bool); b {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fl := w.(http.Flusher)
			fmt.Fprintf(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":%q,\"model\":%q}}\n\n", respID, body["model"])
			fl.Flush()
			fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\""+respID+"\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": respID, "object": "response", "model": body["model"],
			"usage": map[string]any{"input_tokens": 3, "output_tokens": 5, "total_tokens": 8},
		})
	}))
}

func contResponsesReq(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer ck-1")
	return req
}

func contLookupBinding(t *testing.T, s *continuation.Store, tag string, contID string) (*continuation.Binding, bool) {
	t.Helper()
	format := domain.FormatOpenAIResponses
	op := domain.OperationTag(domain.OpResponses)
	if tag == contProtocolWS {
		format = domain.FormatOpenAIResponsesWS
		op = domain.OperationTag(domain.OpResponsesWS)
	}
	b, ok, err := s.Lookup(context.Background(), 1, 10, contRouteID(t, format, "gpt-4o", op), tag, contID)
	require.NoError(t, err)
	return b, ok
}

// --- frame id extraction (ACK gate prefilter) ---

func TestContFrameID(t *testing.T) {
	// nested response object id (created/in_progress/completed events)
	require.Equal(t, "resp_nested", contFrameID([]byte(`{"type":"response.created","response":{"id":"resp_nested","model":"gpt-4o"}}`)))
	// flat response_id (delta events) must pass the prefilter, not just the parser
	require.Equal(t, "resp_flat", contFrameID([]byte(`{"type":"response.output_text.delta","response_id":"resp_flat"}`)))
	// id-less frames and malformed payloads extract nothing
	require.Empty(t, contFrameID([]byte(`{"type":"response.output_text.delta","delta":"hi"}`)))
	require.Empty(t, contFrameID([]byte(`{"type":"response.refusal.delta","item_id":"ctr_1"}`)))
	require.Empty(t, contFrameID([]byte(`not json at all`)))
	require.Empty(t, contFrameID([]byte(`{"id"`)))
}

// --- REST create side ---

func TestContinuationRESTCreateACKCanonicalIdentity(t *testing.T) {
	_, s, hook := contFixture(t)
	var hits atomic.Int32
	var authSeen atomic.Value
	up := contUpstream(t, "resp_create_1", "sk-acc1", "", &hits, &authSeen)
	defer up.Close()
	tpl := &domain.Template{ID: 1, Name: "t", BaseURL: up.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, Models: []string{"gpt-4o"}}
	p := contProxy(t, domain.FormatOpenAIResponses, []*domain.Account{contAcc(1, tpl, "sk-acc1", up.URL)}, s)

	w := httptest.NewRecorder()
	p.HandleResponses(w, contResponsesReq(`{"model":"gpt-4o","input":"hi"}`))
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), "resp_create_1")
	// Counted BEFORE the verification lookup: the store is Redis-authoritative
	// (no L1), so the test's own Lookup would add a command of its own.
	require.Equal(t, int64(1), hook.n.Load(), "one create command per Responses request (warm scripts)")

	b, ok := contLookupBinding(t, s, contProtocolREST, "resp_create_1")
	require.True(t, ok, "created response id must be bound before visibility")
	require.Equal(t, int64(1), b.AccountID)
	require.Equal(t, int64(1), b.IdentityRevision)
	wantFP, err := domain.AccountCandidateFingerprint(contAcc(1, tpl, "sk-acc1", up.URL))
	require.NoError(t, err)
	require.Equal(t, wantFP, b.Fingerprint, "binding fingerprint must be the canonical candidate fingerprint")
}

func TestContinuationOrdinaryRequestsZeroRedis(t *testing.T) {
	_, s, hook := contFixture(t)
	var hits atomic.Int32
	var authSeen atomic.Value
	up := contUpstream(t, "resp_x", "sk-upstream", "", &hits, &authSeen)
	defer up.Close()
	tpl := &domain.Template{ID: 1, Name: "t", BaseURL: up.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"}}
	p := contProxy(t, domain.FormatOpenAIChat, []*domain.Account{contAcc(1, tpl, "sk-acc1", up.URL)}, s)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	w := httptest.NewRecorder()
	p.HandleChat(w, req)
	require.Equal(t, 200, w.Code)
	require.Zero(t, hook.n.Load(), "ordinary chat requests must issue zero Redis commands")
}

func TestContinuationStreamFirstIDAckBeforeVisible(t *testing.T) {
	mr, s, hook := contFixture(t)
	_ = mr
	var hits atomic.Int32
	var authSeen atomic.Value
	up := contUpstream(t, "resp_stream_1", "sk-acc1", "", &hits, &authSeen)
	defer up.Close()
	tpl := &domain.Template{ID: 1, Name: "t", BaseURL: up.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, Models: []string{"gpt-4o"}}
	p := contProxy(t, domain.FormatOpenAIResponses, []*domain.Account{contAcc(1, tpl, "sk-acc1", up.URL)}, s)

	gate := make(chan struct{})
	hook.setGate(gate)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.HandleResponses(w, contResponsesReq(`{"model":"gpt-4o","stream":true,"input":"hi"}`))
	}()

	<-hook.started // the create EVALSHA is in flight: the id frame was produced…
	require.Empty(t, w.Body.String(), "no frame may become visible before the Redis ACK")
	close(gate) // ACK proceeds
	<-done
	require.Contains(t, w.Body.String(), "resp_stream_1", "full stream delivered after ACK")
	_, ok := contLookupBinding(t, s, contProtocolREST, "resp_stream_1")
	require.True(t, ok)
}

func TestContinuationStreamBufferCapFailClosed(t *testing.T) {
	old := contMaxBuffer
	contMaxBuffer = 4096
	t.Cleanup(func() { contMaxBuffer = old })
	_, s, _ := contFixture(t)
	// Upstream streams > cap bytes of id-less frames before any response id.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		junk := strings.Repeat("x", 2048)
		for i := 0; i < 8; i++ {
			fmt.Fprintf(w, "event: response.keepalive\ndata: {\"type\":\"response.keepalive\",\"pad\":%q}\n\n", junk)
			fl.Flush()
		}
		fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_late\"}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer up.Close()
	tpl := &domain.Template{ID: 1, Name: "t", BaseURL: up.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, Models: []string{"gpt-4o"}}
	p := contProxy(t, domain.FormatOpenAIResponses, []*domain.Account{contAcc(1, tpl, "sk-acc1", up.URL)}, s)

	w := httptest.NewRecorder()
	p.HandleResponses(w, contResponsesReq(`{"model":"gpt-4o","stream":true,"input":"hi"}`))
	require.NotContains(t, w.Body.String(), "resp_late", "buffered frames must be discarded on cap breach")
	require.NotContains(t, w.Body.String(), "keepalive")
}

// --- REST continuation (lookup + pin) side ---

func TestContinuationCrossInstancePin(t *testing.T) {
	mr, s1, _ := contFixture(t)
	c2, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c2) })
	s2, err := continuation.New(c2, "cont-test-secret-0123456789")
	require.NoError(t, err)

	var hits1, hits2 atomic.Int32
	var auth1, auth2 atomic.Value
	up1 := contUpstream(t, "resp_gen_1", "sk-acc1", "", &hits1, &auth1)
	defer up1.Close()
	up2 := contUpstream(t, "resp_gen_2", "sk-acc2", "", &hits2, &auth2)
	defer up2.Close()
	tpl := &domain.Template{ID: 1, Name: "t", BaseURL: up1.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, Models: []string{"gpt-4o"}}
	acc1 := contAcc(1, tpl, "sk-acc1", up1.URL)
	acc2 := contAcc(2, tpl, "sk-acc2", up2.URL)

	// Instance 1 produces the first response (plan prefers account 1).
	p1 := contProxy(t, domain.FormatOpenAIResponses, []*domain.Account{acc1, acc2}, s1)
	w := httptest.NewRecorder()
	p1.HandleResponses(w, contResponsesReq(`{"model":"gpt-4o","input":"hi"}`))
	require.Equal(t, 200, w.Code)
	require.EqualValues(t, 1, hits1.Load())

	// Instance 2 continues: the binding pins account 2… wait, account 1 owns
	// the binding; the pin must dispatch to account 1 even though this test
	// asserts cross-instance resolution.
	p2 := contProxy(t, domain.FormatOpenAIResponses, []*domain.Account{acc2, acc1}, s2) // plan prefers acc2
	w2 := httptest.NewRecorder()
	p2.HandleResponses(w2, contResponsesReq(`{"model":"gpt-4o","input":"again","previous_response_id":"resp_gen_1"}`))
	require.Equal(t, 200, w2.Code, "body=%s", w2.Body.String())
	require.EqualValues(t, 0, hits2.Load(), "pinned continuation must not touch the preferred-but-unbound account")
	require.EqualValues(t, 2, hits1.Load(), "continuation must dispatch to the bound account")
	require.Equal(t, "Bearer sk-acc1", auth1.Load().(string))
}

// failover_attempts=1: the initial unbound reservation and every skipped
// unbound candidate must not consume the single attempt slot — the pin scan
// reaches the bound account behind them (regression: skipped reservations
// burned ordinal, so the bound candidate failed closed with 409).
func TestContinuationMaxAttemptsOneReachesBoundAccount(t *testing.T) {
	mr, s1, _ := contFixture(t)
	c2, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c2) })
	s2, err := continuation.New(c2, "cont-test-secret-0123456789")
	require.NoError(t, err)

	var hits1, hits2 atomic.Int32
	var auth1, auth2 atomic.Value
	up1 := contUpstream(t, "resp_single", "sk-acc1", "", &hits1, &auth1)
	defer up1.Close()
	up2 := contUpstream(t, "resp_other", "sk-acc2", "", &hits2, &auth2)
	defer up2.Close()
	tpl := &domain.Template{ID: 1, Name: "t", BaseURL: up1.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, Models: []string{"gpt-4o"}}
	acc1 := contAcc(1, tpl, "sk-acc1", up1.URL)
	acc2 := contAcc(2, tpl, "sk-acc2", up2.URL)

	p1 := contProxy(t, domain.FormatOpenAIResponses, []*domain.Account{acc1, acc2}, s1)
	w := httptest.NewRecorder()
	p1.HandleResponses(w, contResponsesReq(`{"model":"gpt-4o","input":"hi"}`))
	require.Equal(t, 200, w.Code)
	require.EqualValues(t, 1, hits1.Load())

	p2 := contProxy(t, domain.FormatOpenAIResponses, []*domain.Account{acc2, acc1}, s2) // plan prefers acc2
	p2.cfg.FailoverAttempts = 1
	w2 := httptest.NewRecorder()
	p2.HandleResponses(w2, contResponsesReq(`{"model":"gpt-4o","input":"again","previous_response_id":"resp_single"}`))
	require.Equal(t, 200, w2.Code, "body=%s", w2.Body.String())
	require.EqualValues(t, 0, hits2.Load(), "the single attempt slot belongs to the bound account, never the unbound preference")
	require.EqualValues(t, 2, hits1.Load(), "continuation must dispatch to the bound account")
}

func TestContinuationMissingFailClosed(t *testing.T) {
	_, s, _ := contFixture(t)
	var hits atomic.Int32
	var authSeen atomic.Value
	up := contUpstream(t, "resp_nope", "sk-acc1", "", &hits, &authSeen)
	defer up.Close()
	tpl := &domain.Template{ID: 1, Name: "t", BaseURL: up.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, Models: []string{"gpt-4o"}}
	p := contProxy(t, domain.FormatOpenAIResponses, []*domain.Account{contAcc(1, tpl, "sk-acc1", up.URL)}, s)

	w := httptest.NewRecorder()
	p.HandleResponses(w, contResponsesReq(`{"model":"gpt-4o","input":"hi","previous_response_id":"resp_missing"}`))
	require.Equal(t, http.StatusGone, w.Code)
	require.Zero(t, hits.Load(), "unresolvable continuation must never dispatch")
	require.NotContains(t, w.Body.String(), "resp_missing", "no raw ids in errors")
}

func TestContinuationExpiredFailClosed(t *testing.T) {
	mr, s, _ := contFixture(t)
	var hits atomic.Int32
	var authSeen atomic.Value
	up := contUpstream(t, "resp_exp", "sk-acc1", "", &hits, &authSeen)
	defer up.Close()
	tpl := &domain.Template{ID: 1, Name: "t", BaseURL: up.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, Models: []string{"gpt-4o"}}
	p := contProxy(t, domain.FormatOpenAIResponses, []*domain.Account{contAcc(1, tpl, "sk-acc1", up.URL)}, s)

	w := httptest.NewRecorder()
	p.HandleResponses(w, contResponsesReq(`{"model":"gpt-4o","input":"hi"}`))
	require.Equal(t, 200, w.Code)
	mr.FastForward(continuation.TTL + time.Hour)

	// Fresh binding authority (second instance / cold L1): the Redis key is
	// gone → 410, never dispatch. The producing store's L1 mirrors the real
	// 24h deadline (putAt start+TTL) — miniredis FastForward cannot expire an
	// in-process deadline, and production L1 never outlives the Redis TTL.
	c2, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c2) })
	s2, err := continuation.New(c2, "cont-test-secret-0123456789")
	require.NoError(t, err)
	p2 := contProxy(t, domain.FormatOpenAIResponses, []*domain.Account{contAcc(1, tpl, "sk-acc1", up.URL)}, s2)
	w2 := httptest.NewRecorder()
	p2.HandleResponses(w2, contResponsesReq(`{"model":"gpt-4o","input":"hi","previous_response_id":"resp_exp"}`))
	require.Equal(t, http.StatusGone, w2.Code)
	require.EqualValues(t, 1, hits.Load(), "expired binding must not dispatch")
}

func TestContinuationRevisionStaleFailClosed(t *testing.T) {
	_, s, _ := contFixture(t)
	var hits atomic.Int32
	var authSeen atomic.Value
	up := contUpstream(t, "resp_rev", "sk-acc1", "", &hits, &authSeen)
	defer up.Close()
	tpl := &domain.Template{ID: 1, Name: "t", BaseURL: up.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, Models: []string{"gpt-4o"}}
	acc := contAcc(1, tpl, "sk-acc1", up.URL)
	p := contProxy(t, domain.FormatOpenAIResponses, []*domain.Account{acc}, s)

	w := httptest.NewRecorder()
	p.HandleResponses(w, contResponsesReq(`{"model":"gpt-4o","input":"hi"}`))
	require.Equal(t, 200, w.Code)

	// C 是客户端 CAS 令牌，**不是**身份代际：只推进 C 不得使绑定失效
	// （四代模型：绑定按 (I=指纹, K) 围栏，C 只围栏管理员写入）。
	acc.LifecycleRevision = 2
	require.NoError(t, p.sched.InvalidateAllSync())
	publishTestRoutes(t, p.sched)
	wC := httptest.NewRecorder()
	p.HandleResponses(wC, contResponsesReq(`{"model":"gpt-4o","input":"hi","previous_response_id":"resp_rev"}`))
	require.Equal(t, 200, wC.Code, "C bump must not fence the continuation binding")
	require.EqualValues(t, 2, hits.Load(), "C bump must still dispatch on the bound account")

	// K（身份代际）推进才使绑定失效：陈旧绑定 fail closed 且不再派发。
	acc.IdentityRevision = 2
	require.NoError(t, p.sched.InvalidateAllSync())
	publishTestRoutes(t, p.sched)
	w2 := httptest.NewRecorder()
	p.HandleResponses(w2, contResponsesReq(`{"model":"gpt-4o","input":"hi","previous_response_id":"resp_rev"}`))
	require.Equal(t, http.StatusConflict, w2.Code)
	require.EqualValues(t, 2, hits.Load(), "stale identity revision must fail closed without dispatch")
}

func TestContinuationCreateConflictFailClosed(t *testing.T) {
	_, s, _ := contFixture(t)
	// Pre-bind the id to a foreign account identity: the real create conflicts.
	foreign := contFP(t, 999, "sk-foreign")
	_, err := s.CreateOrRefresh(context.Background(), 1, 10, contRouteID(t, domain.FormatOpenAIResponses, "gpt-4o", domain.OpResponses), contProtocolREST, "resp_dup", 999, foreign, 1)
	require.NoError(t, err)

	var hits atomic.Int32
	var authSeen atomic.Value
	up := contUpstream(t, "resp_dup", "sk-acc1", "", &hits, &authSeen)
	defer up.Close()
	tpl := &domain.Template{ID: 1, Name: "t", BaseURL: up.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, Models: []string{"gpt-4o"}}
	p := contProxy(t, domain.FormatOpenAIResponses, []*domain.Account{contAcc(1, tpl, "sk-acc1", up.URL)}, s)

	w := httptest.NewRecorder()
	p.HandleResponses(w, contResponsesReq(`{"model":"gpt-4o","input":"hi"}`))
	require.Equal(t, http.StatusBadGateway, w.Code)
	require.NotContains(t, w.Body.String(), "resp_dup", "conflicted id must never become visible")
}

func TestContinuationRedisDownFailClosed(t *testing.T) {
	mr, s, _ := contFixture(t)
	var hits atomic.Int32
	var authSeen atomic.Value
	up := contUpstream(t, "resp_down", "sk-acc1", "", &hits, &authSeen)
	defer up.Close()
	tpl := &domain.Template{ID: 1, Name: "t", BaseURL: up.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, Models: []string{"gpt-4o"}}
	p := contProxy(t, domain.FormatOpenAIResponses, []*domain.Account{contAcc(1, tpl, "sk-acc1", up.URL)}, s)

	oldTO := contOpTimeout
	contOpTimeout = 300 * time.Millisecond
	t.Cleanup(func() { contOpTimeout = oldTO })
	mr.Close()

	w := httptest.NewRecorder()
	p.HandleResponses(w, contResponsesReq(`{"model":"gpt-4o","input":"hi"}`))
	require.NotEqual(t, 200, w.Code, "Redis outage must fail closed on create")
	require.NotContains(t, w.Body.String(), "resp_down", "unbound id must not leak")

	w2 := httptest.NewRecorder()
	p.HandleResponses(w2, contResponsesReq(`{"model":"gpt-4o","input":"hi","previous_response_id":"resp_any"}`))
	require.Equal(t, http.StatusServiceUnavailable, w2.Code)
	// The create above consumed exactly one upstream hit (ACK-before-visible
	// dials first, then discards on bind failure); the failed lookup must add
	// no dispatch of its own.
	require.EqualValues(t, 1, hits.Load(), "fail-closed lookup must not dispatch")
}

// --- no migration (hard continuation) ---

func TestContinuationHardFailureNeverMigrates(t *testing.T) {
	mr, s1, _ := contFixture(t)
	c2, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c2) })
	s2, err := continuation.New(c2, "cont-test-secret-0123456789")
	require.NoError(t, err)

	var hits1, hits2 atomic.Int32
	var auth1, auth2 atomic.Value
	upBound := contUpstream(t, "resp_h", "sk-acc1", "500-after-1", &hits1, &auth1)
	defer upBound.Close()
	upOther := contUpstream(t, "resp_other", "sk-acc2", "", &hits2, &auth2)
	defer upOther.Close()
	tpl := &domain.Template{ID: 1, Name: "t", BaseURL: upBound.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, Models: []string{"gpt-4o"}}
	acc1 := contAcc(1, tpl, "sk-acc1", upBound.URL)
	acc2 := contAcc(2, tpl, "sk-acc2", upOther.URL)

	p1 := contProxy(t, domain.FormatOpenAIResponses, []*domain.Account{acc1, acc2}, s1)
	w := httptest.NewRecorder()
	p1.HandleResponses(w, contResponsesReq(`{"model":"gpt-4o","input":"hi"}`))
	require.Equal(t, 200, w.Code)

	// Continuation pinned to account 1 (the bound one, now failing): the
	// request must fail closed on the bound account — never migrate.
	p2 := contProxy(t, domain.FormatOpenAIResponses, []*domain.Account{acc2, acc1}, s2)
	w2 := httptest.NewRecorder()
	p2.HandleResponses(w2, contResponsesReq(`{"model":"gpt-4o","input":"hi","previous_response_id":"resp_h"}`))
	// Bound-account 500 reaches the client as the gateway-wide normalized 5xx
	// (seed-5xx: 502 "Upstream request failed") — continuation never changes
	// error normalization. The hard guarantee is the dispatch accounting below.
	require.Equal(t, http.StatusBadGateway, w2.Code)
	require.EqualValues(t, 2, hits1.Load(), "exactly one dispatch on the pinned account")
	require.EqualValues(t, 0, hits2.Load(), "no migration to another account")
}

func TestContinuationHard429NeverMigrates(t *testing.T) {
	mr, s1, _ := contFixture(t)
	c2, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c2) })
	s2, err := continuation.New(c2, "cont-test-secret-0123456789")
	require.NoError(t, err)
	var hits1, hits2 atomic.Int32
	var auth1, auth2 atomic.Value
	upBound := contUpstream(t, "resp_429", "sk-acc1", "429-after-1", &hits1, &auth1)
	defer upBound.Close()
	upOther := contUpstream(t, "resp_other", "sk-acc2", "", &hits2, &auth2)
	defer upOther.Close()
	tpl := &domain.Template{ID: 1, Name: "t", BaseURL: upBound.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, Models: []string{"gpt-4o"}}
	acc1 := contAcc(1, tpl, "sk-acc1", upBound.URL)
	acc2 := contAcc(2, tpl, "sk-acc2", upOther.URL)
	p1 := contProxy(t, domain.FormatOpenAIResponses, []*domain.Account{acc1, acc2}, s1)
	w := httptest.NewRecorder()
	p1.HandleResponses(w, contResponsesReq(`{"model":"gpt-4o","input":"hi"}`))
	require.Equal(t, 200, w.Code)
	p2 := contProxy(t, domain.FormatOpenAIResponses, []*domain.Account{acc2, acc1}, s2)
	w2 := httptest.NewRecorder()
	p2.HandleResponses(w2, contResponsesReq(`{"model":"gpt-4o","input":"hi","previous_response_id":"resp_429"}`))
	require.Equal(t, http.StatusTooManyRequests, w2.Code)
	require.EqualValues(t, 2, hits1.Load(), "hard 429 is terminal: no failover")
	require.EqualValues(t, 0, hits2.Load())
}

func TestCanRetry_HardContinuationNetworkNeverMigrates(t *testing.T) {
	o := outcomeForKind("not_sent", CallerResponses, true)
	require.False(t, CanRetry(CallerResponses, o), "hard continuation must never migrate, even pre-send")
	ordinary := outcomeForKind("not_sent", CallerResponses, false)
	require.True(t, CanRetry(CallerResponses, ordinary), "ordinary not-sent network stays retryable")
}

// --- WS side ---

// contFakeWS records the account credential and streams created/completed.
func contFakeWS(t *testing.T, respID string, hits *atomic.Int32, authSeen *atomic.Value, mode string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		authSeen.Store(r.Header.Get("Authorization"))
		if mode == "silent" { // accept, never send a frame
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow()
			<-r.Context().Done()
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := context.Background()
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.created","response":{"id":"`+respID+`","model":"gpt-4o"}}`))
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.output_text.delta","delta":"hi"}`))
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"`+respID+`","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`))
		_ = c.Close(websocket.StatusNormalClosure, "")
	}))
}

func contWSServer(t *testing.T, p *Proxy) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	t.Cleanup(srv.Close)
	return srv
}

func contWSDial(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/responses"
	c, _, err := websocket.Dial(context.Background(), u, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer ck-1"}},
	})
	require.NoError(t, err)
	return c
}

func TestContinuationWSCreateACK(t *testing.T) {
	_, s, _ := contFixture(t)
	var hits atomic.Int32
	var authSeen atomic.Value
	up := contFakeWS(t, "rsp_ws_1", &hits, &authSeen, "")
	defer up.Close()
	tpl := &domain.Template{ID: 1, Name: "t", BaseURL: up.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS}, Models: []string{"gpt-4o"}}
	p := contProxy(t, domain.FormatOpenAIResponsesWS, []*domain.Account{contAcc(1, tpl, "sk-acc1", up.URL)}, s)
	srv := contWSServer(t, p)

	c := contWSDial(t, srv)
	defer c.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	for i := 0; i < 3; i++ {
		_, f, err := c.Read(ctx)
		require.NoError(t, err)
		require.NotContains(t, string(f), `"type":"error"`)
	}
	b, ok := contLookupBinding(t, s, contProtocolWS, "rsp_ws_1")
	require.True(t, ok, "WS response id must be bound before frames reach the client")
	require.Equal(t, int64(1), b.AccountID)
	require.Equal(t, int64(1), b.IdentityRevision)
}

func TestContinuationWSPinAndMissingFailClosed(t *testing.T) {
	mr, s1, _ := contFixture(t)
	c2, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c2) })
	s2, err := continuation.New(c2, "cont-test-secret-0123456789")
	require.NoError(t, err)
	var hits1, hits2 atomic.Int32
	var auth1, auth2 atomic.Value
	up1 := contFakeWS(t, "rsp_ws_a", &hits1, &auth1, "")
	defer up1.Close()
	up2 := contFakeWS(t, "rsp_ws_b", &hits2, &auth2, "")
	defer up2.Close()
	tpl := &domain.Template{ID: 1, Name: "t", BaseURL: up1.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS}, Models: []string{"gpt-4o"}}
	acc1 := contAcc(1, tpl, "sk-acc1", up1.URL)
	acc2 := contAcc(2, tpl, "sk-acc2", up2.URL)

	p1 := contProxy(t, domain.FormatOpenAIResponsesWS, []*domain.Account{acc1, acc2}, s1)
	srv1 := contWSServer(t, p1)
	c := contWSDial(t, srv1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	for i := 0; i < 3; i++ {
		_, _, err := c.Read(ctx)
		require.NoError(t, err)
	}
	c.CloseNow()
	require.EqualValues(t, 1, hits1.Load())

	// Continuation on another instance: pinned to account 1 despite acc2-first plan.
	p2 := contProxy(t, domain.FormatOpenAIResponsesWS, []*domain.Account{acc2, acc1}, s2)
	srv2 := contWSServer(t, p2)
	c2conn := contWSDial(t, srv2)
	require.NoError(t, c2conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-4o","input":"again","previous_response_id":"rsp_ws_a"}`)))
	for i := 0; i < 3; i++ {
		_, _, err := c2conn.Read(ctx)
		require.NoError(t, err)
	}
	c2conn.CloseNow()
	require.EqualValues(t, 0, hits2.Load(), "pinned WS continuation must not touch the unbound account")
	require.EqualValues(t, 2, hits1.Load())

	// Missing binding: error frame, no dial.
	c3 := contWSDial(t, srv2)
	require.NoError(t, c3.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-4o","input":"x","previous_response_id":"rsp_missing"}`)))
	_, f, err := c3.Read(ctx)
	if err == nil {
		require.Contains(t, string(f), `"type":"error"`, "unresolvable WS continuation must fail closed with an error frame")
		require.NotContains(t, string(f), "rsp_missing")
	}
	c3.CloseNow()
	require.EqualValues(t, 2, hits1.Load(), "fail-closed continuation must not dial upstream")
}

func TestContinuationWSAckFailClosed(t *testing.T) {
	mr, s, _ := contFixture(t)
	var hits atomic.Int32
	var authSeen atomic.Value
	up := contFakeWS(t, "rsp_ws_dead", &hits, &authSeen, "")
	defer up.Close()
	tpl := &domain.Template{ID: 1, Name: "t", BaseURL: up.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS}, Models: []string{"gpt-4o"}}
	p := contProxy(t, domain.FormatOpenAIResponsesWS, []*domain.Account{contAcc(1, tpl, "sk-acc1", up.URL)}, s)
	srv := contWSServer(t, p)

	oldTO := contOpTimeout
	contOpTimeout = 300 * time.Millisecond
	t.Cleanup(func() { contOpTimeout = oldTO })
	mr.Close() // Redis outage: the created frame must never reach the client

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := contWSDial(t, srv)
	defer c.CloseNow()
	require.NoError(t, c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	_, f, err := c.Read(ctx)
	require.NoError(t, err, "gateway must surface a terminal error frame")
	require.Contains(t, string(f), `"type":"error"`)
	require.NotContains(t, string(f), "rsp_ws_dead", "unbound id must not leak")
}
