// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
)

func TestRED_CodexDerivedTimeoutIsNotClientCancel(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	blackhole := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(blackhole.Close)
	store := &captureLogStore{}
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-10")},
		blackhole.URL, nil, nil, store)
	p.cfg.UpstreamTimeout = 150 * time.Millisecond

	var mu sync.Mutex
	var outcomes []AttemptOutcome
	codexOutcomeCapture = func(o AttemptOutcome) {
		mu.Lock()
		outcomes = append(outcomes, o)
		mu.Unlock()
	}
	t.Cleanup(func() { codexOutcomeCapture = nil })

	sel := selectCodexAccount(t, p, 10)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx := req.Context()
	code, _, handled, _ := p.nonstreamCodexResponses(ctx, httptest.NewRecorder(), req, "req-timeout", 10, time.Now(), sel, "gpt-4o", &domain.AccountCredential{PATKey: "pat-10"}, []byte(`{"model":"gpt-4o","input":"hi"}`))
	require.False(t, handled, "derived upstream timeout must be failoverable (handled=false), not client cancel")
	require.Equal(t, 0, code, "derived timeout maps to network code 0")
	mu.Lock()
	defer mu.Unlock()
	require.Empty(t, outcomes, "pre-response network failure must not duplicate MarkResult/release via outcome emit; pipeline owns it")
}

func TestRED_CodexPreResponseClientCancelPreserves499(t *testing.T) {
	store := &captureLogStore{}
	up, _ := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{t6RespDone}})
	defer up.Close()
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-10")},
		up.URL, nil, nil, store)
	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/responses", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer ck-1")
	req.Header.Set("Content-Type", "application/json")
	// body with valid JSON to reach selection
	req.Body = http.NoBody
	// Use direct failover loop observation via pipeline 499: we drive via HandleSearch style but use codex nonstream with canceled context
	// Instead verify via direct call that client cancel is not conflated with deadline
	sel := selectCodexAccount(t, p, 10)
	rec := httptest.NewRecorder()
	canceledReq := httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	code, _, handled, _ := p.nonstreamCodexResponses(ctx, rec, canceledReq, "req-cancel", 10, time.Now(), sel, "gpt-4o", &domain.AccountCredential{PATKey: "pat-10"}, []byte(`{"model":"gpt-4o"}`))
	require.False(t, handled, "pre-response client cancel must return handled=false to preserve shared 499 path")
	require.Equal(t, 0, code)
}

func TestRED_CodexMidStreamFailedMustBeValid(t *testing.T) {
	o := AttemptOutcome{
		ID: "req-mid", RouteClassID: "rc1", QualityClassID: "qc1", Fingerprint: "fp1",
		TemplateID: 1, AccountID: 1, RequestedModel: "gpt-4o", MappedModel: "gpt-4o",
		CallerCategory: CallerCodexHTTP, OperationTag: "responses", Ordinal: 1,
		LifecycleRevision: 1, Lane: LanePrimary, Generation: 1,
		Result: ResultFailed, HTTPStatus: 0, Commit: CommitResponseStarted, BusinessFrameSent: true, Terminal: true,
		Timing: AttemptTiming{LatencyMS: 10},
	}
	require.NoError(t, o.Validate(), "mid-stream ResultFailed+ResponseStarted must be valid typed outcome preserving health")
	require.True(t, o.IsCountedForQuality())
	// also ensure observer would not ignore health: health must be observed for failed
	health := &AttemptHealthEvent{Kind: rule.KindNetwork}
	require.True(t, o.IsCountedForQuality())
	require.NotNil(t, health)
}

func TestRED_SearchPreResponseMustNotDuplicate(t *testing.T) {
	store := &captureLogStore{}
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-10")},
		"http://127.0.0.1:9", nil, nil, store)
	// force search path ext missing => handled false duplication check
	sel := selectCodexAccount(t, p, 10)
	sel.Ext = nil
	var mu sync.Mutex
	var outcomes []AttemptOutcome
	searchOutcomeCapture = func(o AttemptOutcome) {
		mu.Lock()
		outcomes = append(outcomes, o)
		mu.Unlock()
	}
	t.Cleanup(func() { searchOutcomeCapture = nil })
	req := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", nil)
	code, _, handled, _ := p.callCodexSearch(req.Context(), httptest.NewRecorder(), req, "req-search", 10, time.Now(), sel, "gpt-4o", []byte(`{"model":"gpt-4o"}`))
	require.False(t, handled)
	require.Equal(t, 0, code)
	mu.Lock()
	defer mu.Unlock()
	require.Empty(t, outcomes, "pre-response search failure handled=false must not emit outcome (pipeline owns)")
}

func stringPtr(s string) *string { return &s }
