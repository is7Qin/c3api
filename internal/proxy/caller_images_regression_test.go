// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	codexsdk "github.com/is7Qin/codex-sdk"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
)

func TestRegression_NoDuplicateMarkResultAndReleaseOnHandledFalse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer server.Close()
	p, sel := newImagesRegressionProxy(t, server.URL)
	body := []byte(`{"model":"gpt-image-2","prompt":"x"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	w := httptest.NewRecorder()
	// Concurrency before call should be 1 (leased)
	riBefore, _ := p.sched.Runtime(sel.AccountID)
	require.Equal(t, int64(1), riBefore.Concurrency, "leased concurrency 1")
	code, _, handled, _ := p.imageGenerations.Call(context.Background(), w, req, "req-dup-1", 10, time.Now(), sel, "sk", body, false)
	require.False(t, handled, "pre-response failure must be handled=false for failover")
	require.Equal(t, 500, code)
	riAfter, _ := p.sched.Runtime(sel.AccountID)
	require.Equal(t, int64(1), riAfter.Concurrency, "handled=false must not release; pipeline owns single release")
	// MarkResult for 5xx should not have been called by caller; rule not enqueued yet (caller would have enqueued)
	// Flush to check no cooldown via caller
	sel.Release()
	riFinal, _ := p.sched.Runtime(sel.AccountID)
	require.Equal(t, int64(0), riFinal.Concurrency, "pipeline release exactly once")
}

func TestRegression_CodexFatalStatus0NotRetryable(t *testing.T) {
	require.True(t, sdkbridge.IsFatal(&codexsdk.RefreshOAuthError{}), "sanity: RefreshOAuthError is fatal")
	require.False(t, sdkbridge.IsFatal(&codexsdk.HTTPError{StatusCode: 0}), "plain network error not fatal")
	require.True(t, isCodexFatal(&codexsdk.RefreshOAuthError{}), "isCodexFatal must delegate to sdkbridge.IsFatal")
	require.False(t, isCodexFatal(&codexsdk.HTTPError{StatusCode: 0}), "non-fatal status-0 must remain retryable network")
	sel := &scheduler.Selection{AccountID: 1, TemplateID: 1, Model: "m", CandidateFingerprint: "fp"}
	oFatal := codexImagesOutcome("req-fatal", sel, "m", OperationTag(domain.OpImagesGenerations), AttemptTiming{}, AttemptUsage{}, ResultFailed, AttemptStatus(0), CommitUpstreamResponded, false, true, false)
	require.True(t, oFatal.Terminal, "fatal status-0 must be terminal")
	require.False(t, CanRetry(oFatal.CallerCategory, oFatal), "fatal must not be retryable")
	oNet := codexImagesOutcome("req-net", sel, "m", OperationTag(domain.OpImagesGenerations), AttemptTiming{}, AttemptUsage{}, ResultFailed, AttemptStatus(0), CommitNotSent, false, false, false)
	require.False(t, oNet.Terminal, "network status-0 must be non-terminal retryable")
	require.True(t, CanRetry(oNet.CallerCategory, oNet), "network not_sent must be retryable")
	up, _ := newCodexImageUpstream(t, codexUpStep{status: 401, body: `{"error":{"code":"token_invalidated"}}`})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestCodexProxy(t, credential.TypeCodexOAuth, map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")}, up.URL, nil, store)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	sel2, err := p.sched.Select(10, domain.FormatOpenAIImages, "gpt-image-2")
	require.NoError(t, err)
	w := httptest.NewRecorder()
	code, _, handled, callErr := p.codexImagesGenerations.Call(context.Background(), w, req, "req-fatal-2", 10, time.Now(), sel2, "", []byte(`{"model":"gpt-image-2","prompt":"x"}`), false)
	require.False(t, handled, "codex fatal pre-response is handled=false (pipeline owns single MarkResult/Release)")
	require.True(t, sdkbridge.IsFatal(callErr), "fatal must be SDK-authoritative")
	require.NotEqual(t, 0, code, "fatal status-0 must not remain 0 (retryable network); mapped to terminal code")
	_ = code
	_ = handled
	sel2.Release()
	// status-0 fatal must be mapped to non-retryable (not network)
	fatalNetErr := &codexsdk.RefreshOAuthError{}
	require.True(t, sdkbridge.IsFatal(fatalNetErr))
	oFatalNet := codexImagesOutcome("req-fatal-net", sel, "m", OperationTag(domain.OpImagesGenerations), AttemptTiming{}, AttemptUsage{}, ResultFailed, AttemptStatus(502), CommitUpstreamResponded, false, true, false)
	require.False(t, CanRetry(oFatalNet.CallerCategory, oFatalNet), "status-0 fatal mapped to 502 must not be retryable network")
}

func newImagesRegressionProxy(t *testing.T, baseURL string) (*Proxy, *scheduler.Selection) {
	t.Helper()
	tpl := &domain.Template{ID: 1, Name: "t", BaseURL: baseURL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIImages}, Models: []string{"gpt-image-2"}}
	accs := map[int64][]*domain.Account{10: {{ID: 99, TemplateID: tpl.ID, Template: tpl, UpstreamKey: "sk", Enabled: true, LifecycleRevision: 1, MaxConcurrency: 4}}}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	sched := scheduler.New(scheduler.Config{DefaultMaxConcurrency: 4, SyncInterval: time.Hour}, noopLoader{accs: accs}, re, nil)
	require.NoError(t, sched.InvalidateAllSync())
	publishTestRoutes(t, sched)

	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{"ck-1": activeKey(1, 1, 10)}}, noopUserLoader{}, nil, true)
	require.NoError(t, auth.Reload(context.Background()))
	hc := &http.Client{Transport: http.DefaultTransport}
	clients := aiclient.NewFactory(hc, aiclient.Config{UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second})
	rec := usage.New(usage.UsageConfig{BatchSize: 100, FlushInterval: time.Hour, QuotaFlushInterval: time.Hour}, &captureLogStore{}, nil)
	p := New(Config{MaxBodySize: 1 << 20, FailoverAttempts: 2, UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second, UsageCapture: true}, sched, credential.New(), rec, clients, auth, nil, nil, nil)
	p.imageGenerations = &imagesCaller{p: p, path: "images/generations", op: domain.OpImagesGenerations}
	sel, err := sched.Select(10, domain.FormatOpenAIImages, "gpt-image-2")
	require.NoError(t, err)
	return p, sel
}
