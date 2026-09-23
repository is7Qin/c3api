// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
)

func TestNormalizedFailoverAttempts(t *testing.T) {
	p := &Proxy{cfg: Config{FailoverAttempts: 0}}
	require.Equal(t, 3, p.normalizedAttempts(), "0 must normalize to default 3")
	p.cfg.FailoverAttempts = 10
	require.Equal(t, 8, p.normalizedAttempts(), ">8 must clamp to 8")
	p.cfg.FailoverAttempts = 1
	require.Equal(t, 1, p.normalizedAttempts())
	p.cfg.FailoverAttempts = 8
	require.Equal(t, 8, p.normalizedAttempts())
	p.cfg.FailoverAttempts = 2
	require.Equal(t, 2, p.normalizedAttempts())
}

func TestSelectWithPlan_FirstSelectionUsesPlanWhenIdentityAvailable(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	// add second account for plan lanes
	tpl2 := &domain.Template{ID: 2, Name: "t2", BaseURL: up.URL, CredentialType: "api_key", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"}}
	loader := p.sched.Loader().(noopLoader)
	loader.accs[10] = append(loader.accs[10], &domain.Account{ID: 2, TemplateID: 2, Template: tpl2, UpstreamKey: "sk-upstream", Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 4})
	require.NoError(t, p.sched.InvalidateAllSync())
	publishTestRoutes(t, p.sched)

	schedRoute := scheduler.RouteRefFor(10, string(domain.FormatOpenAIChat), "gpt-4o")
	// 精确模型桶显式编译（默认桶由 publishTestRoutes 武装）——计划身份应携带
	// 命中路由的 canonical RouteClassID。
	p.sched.PublishDecisionForTest(schedRoute, &scheduler.RouteDecision{Primary: []scheduler.CompiledCandidate{{AccountID: 1}, {AccountID: 2}}})
	identity := scheduler.AttemptPlanIdentity{RequestID: "req-123", UserID: 1}
	sel, plan, _, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o", identity)
	require.NoError(t, err)
	require.NotNil(t, sel)
	// Plan-only contract: a successful selection is plan-backed, never a
	// plan-less selection.
	// the session is a stack value — bound identity echoes the request.
	require.Equal(t, "req-123", plan.Identity().RequestID)
	// RouteClassID is borrowed from the interned published decision.
	dec, ok := p.sched.View().DecisionView().Route(10, string(domain.FormatOpenAIChat), "gpt-4o")
	require.True(t, ok)
	require.NotEmpty(t, dec.RouteClassID)
	require.Equal(t, dec.RouteClassID, plan.Identity().RouteClassID)
	require.LessOrEqual(t, int(planIdentityCandidateCount(plan)), 8)
	sel.Release()
}

func TestSelectWithPlan_ConvertedRouteUsesTargetIdentity(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	loader := p.sched.Loader().(noopLoader)
	tplResp := &domain.Template{ID: 2, Name: "tr", BaseURL: up.URL, CredentialType: "api_key", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, Models: []string{"gpt-4o"}}
	loader.accs[10] = append(loader.accs[10], &domain.Account{ID: 2, TemplateID: 2, Template: tplResp, UpstreamKey: "sk-upstream", Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 4})
	require.NoError(t, p.sched.InvalidateAllSync())
	publishTestRoutes(t, p.sched)

	respRoute := scheduler.RouteRefFor(10, string(domain.FormatOpenAIResponses), "gpt-4o")
	// 精确模型桶显式编译——转换路由的计划身份携带目标 canonical RouteClassID。
	p.sched.PublishDecisionForTest(respRoute, &scheduler.RouteDecision{Primary: []scheduler.CompiledCandidate{{AccountID: 2}}})
	identity := scheduler.AttemptPlanIdentity{RequestID: "req-conv", UserID: 1}
	sel2, plan2, _, err2 := p.selectWithPlan(10, domain.FormatOpenAIResponses, "gpt-4o", identity)
	require.NoError(t, err2)
	require.NotNil(t, sel2)
	// the session is a stack value — bound identity echoes the request.
	require.Equal(t, "req-conv", plan2.Identity().RequestID, "plan-only contract: selection is plan-backed")
	// RouteClassID is borrowed from the interned published decision.
	dec2, ok := p.sched.View().DecisionView().Route(10, string(domain.FormatOpenAIResponses), "gpt-4o")
	require.True(t, ok)
	require.Equal(t, dec2.RouteClassID, plan2.Identity().RouteClassID)
	sel2.Release()
}

func retryBase(commit CommitState, result AttemptResult, status AttemptStatus, terminal bool) AttemptOutcome {
	return AttemptOutcome{
		ID: "a1", RouteClassID: "rc", QualityClassID: "qc1", Fingerprint: "fp", TemplateID: 1, AccountID: 1,
		RequestedModel: "gpt-4o", MappedModel: "gpt-4o", CallerCategory: CallerChat, OperationTag: "chat_completions", Ordinal: 1,
		Lane: LanePrimary, Generation: 1, IdentityRevision: 1, Commit: commit, Result: result, HTTPStatus: status, Terminal: terminal,
	}
}

func TestFailoverPlan_RetryGatingViaMatrix(t *testing.T) {
	require.True(t, CanRetry(CallerChat, func() AttemptOutcome { o := retryBase(CommitNotSent, ResultFailed, 0, false); return o }()))
	require.True(t, CanRetry(CallerChat, func() AttemptOutcome {
		o := retryBase(CommitUpstreamResponded, ResultFailed, 429, false)
		o.HardContinuation = false
		return o
	}()))
	require.False(t, CanRetry(CallerChat, func() AttemptOutcome { o := retryBase(CommitUpstreamResponded, ResultFailed, 500, true); return o }()))
	require.False(t, CanRetry(CallerChat, func() AttemptOutcome {
		o := retryBase(CommitResponseStarted, ResultFailed, 500, true)
		o.BusinessFrameSent = true
		return o
	}()))
	require.False(t, CanRetry(CallerChat, func() AttemptOutcome {
		o := retryBase(CommitSentAmbiguous, ResultFailed, 0, true)
		o.BusinessFrameSent = true
		return o
	}()))
	require.False(t, CanRetry(CallerChat, func() AttemptOutcome { o := retryBase(CommitNotSent, ResultClientCancel, 0, true); return o }()))
	require.False(t, CanRetry(CallerChat, func() AttemptOutcome {
		o := retryBase(CommitUpstreamResponded, ResultFailed, 429, true)
		o.HardContinuation = true
		return o
	}()))
	require.False(t, CanRetry(CallerChat, func() AttemptOutcome {
		o := retryBase(CommitUpstreamResponded, ResultFailed, 400, true)
		o.IsMalformed = true
		return o
	}()))
}

func TestFailoverPlan_AttemptsExhaustedVsNoAvailable(t *testing.T) {
	// plan with zero candidates => ErrNoAvailable, plan with candidates but all gates reject => ErrAttemptsExhausted
	s := newTestSchedulerForPlan(t)
	emptyRoute := scheduler.RouteRefFor(10, string(domain.FormatOpenAIChat), "missing")
	_, err := s.NewAttemptPlan(scheduler.AttemptPlanIdentity{RequestID: "r1", UserID: 1}, emptyRoute)
	// Sentinel update (plan-not-ready): this scheduler is static-only (no
	// compiled decision ever published), so the miss is compile lag, not an
	// unroutable model — ErrPlanNotReady (503), not ErrFormatUnavailable.
	// The genuinely-unroutable 404 is locked by scheduler_test.go:456,471,490,534
	// on compiled schedulers.
	require.ErrorIs(t, err, scheduler.ErrPlanNotReady)
	// create empty decision plan directly
	emptyPlan, err := scheduler.NewAttemptPlan(scheduler.AttemptPlanIdentity{}, &scheduler.RouteDecision{})
	require.NoError(t, err)
	_, err = emptyPlan.Reserve(func(int64) bool { return true })
	require.ErrorIs(t, err, scheduler.ErrNoAvailable)
	require.NotErrorIs(t, err, scheduler.ErrAttemptsExhausted)
	fullPlan, err := scheduler.NewAttemptPlan(scheduler.AttemptPlanIdentity{}, &scheduler.RouteDecision{Primary: []scheduler.CompiledCandidate{{AccountID: 1}}})
	require.NoError(t, err)
	_, err = fullPlan.Reserve(func(int64) bool { return false })
	require.ErrorIs(t, err, scheduler.ErrAttemptsExhausted)
	require.NotErrorIs(t, err, scheduler.ErrNoAvailable)
}

func TestFailoverPlan_ReservationRejectionDoesNotConsumeDispatch(t *testing.T) {
	plan, err := scheduler.NewAttemptPlan(scheduler.AttemptPlanIdentity{}, &scheduler.RouteDecision{Primary: []scheduler.CompiledCandidate{{AccountID: 1}, {AccountID: 2}, {AccountID: 3}}})
	require.NoError(t, err)
	// first reserve rejects 1, accepts 2 -> ordinal 1
	a1, err := plan.Reserve(func(id int64) bool { return id == 2 })
	require.NoError(t, err)
	require.Equal(t, int64(2), a1.AccountID)
	require.Equal(t, uint8(1), a1.Ordinal)
	// next reserve accepts 3 -> ordinal 2, not 3
	a2, err := plan.Reserve(func(id int64) bool { return id == 3 })
	require.NoError(t, err)
	require.Equal(t, uint8(2), a2.Ordinal)
}

func TestFailoverPlan_DuplicateIDsRejectedAtPlanBoundary(t *testing.T) {
	// Given
	decision := &scheduler.RouteDecision{
		Primary: []scheduler.CompiledCandidate{{AccountID: 1}, {AccountID: 2}}, Explore: scheduler.ExploreDecision{Ordered: []scheduler.CompiledCandidate{{AccountID: 2}, {AccountID: 3}}, Weights: map[int64]int{2: 100, 3: 100}, Cumulative: []uint64{100, 200}, Total: 200, Fallback: []uint16{1}}, Degraded: []scheduler.CompiledCandidate{{AccountID: 1}, {AccountID: 4}},
	}

	// When
	_, err := scheduler.NewAttemptPlan(scheduler.AttemptPlanIdentity{}, decision)

	// Then
	var invalid *scheduler.InvalidRouteDecisionError
	require.ErrorAs(t, err, &invalid)
	require.ErrorIs(t, err, scheduler.ErrInvalidRouteDecision)
}

func TestFailoverPlan_ReleaseExactlyOnceBeforeRetry(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	loader := p.sched.Loader().(noopLoader)
	tpl2 := &domain.Template{ID: 2, Name: "t2", BaseURL: up.URL, CredentialType: "api_key", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"}}
	loader.accs[10] = append(loader.accs[10], &domain.Account{ID: 2, TemplateID: 2, Template: tpl2, UpstreamKey: "sk-upstream", Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 4})
	require.NoError(t, p.sched.InvalidateAllSync())
	publishTestRoutes(t, p.sched)

	p.cfg.FailoverAttempts = 2
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	// both accounts should have released concurrency exactly once
	for _, id := range []int64{1, 2} {
		ri, ok := p.sched.Runtime(id)
		require.True(t, ok)
		require.Equal(t, int64(0), ri.Concurrency, "account %d must be released exactly once", id)
	}
}

// helpers for plan tests
func newTestSchedulerForPlan(t *testing.T) *scheduler.Scheduler {
	t.Helper()
	tpl := &domain.Template{ID: 1, Name: "t", BaseURL: "http://127.0.0.1:9", CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"}}
	accs := map[int64][]*domain.Account{10: {{ID: 1, TemplateID: 1, Template: tpl, UpstreamKey: "k", Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 4}}}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil, nil, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := scheduler.New(scheduler.Config{SyncInterval: 1000000000000}, noopLoader{accs: accs}, re, nil, nil, nil, nil)
	require.NoError(t, s.InvalidateAllSync())
	return s
}

// chatProxy builds a proxy on group 10 with n chat accounts (IDs 1..n) all
// pointing at the same upstream, and publishes a compiled plan over them.
func chatProxyWithPlan(t *testing.T, upstream string, n int, primary []int64) *Proxy {
	t.Helper()
	p := newTestProxy(t, upstream, 1)
	loader := p.sched.Loader().(noopLoader)
	for id := int64(2); id <= int64(n); id++ {
		tplx := &domain.Template{ID: id, Name: "t", BaseURL: upstream, CredentialType: "api_key", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"}}
		loader.accs[10] = append(loader.accs[10], &domain.Account{ID: id, TemplateID: id, Template: tplx, UpstreamKey: "sk-upstream", Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 4})
	}
	require.NoError(t, p.sched.InvalidateAllSync())
	publishTestRoutes(t, p.sched)

	route := scheduler.RouteRefFor(10, string(domain.FormatOpenAIChat), "gpt-4o")
	compiled := make([]scheduler.CompiledCandidate, len(primary))
	for i, id := range primary {
		compiled[i] = scheduler.CompiledCandidate{AccountID: id}
	}
	p.sched.PublishDecisionForTest(route, &scheduler.RouteDecision{Primary: compiled})
	return p
}

func TestSelectWithPlan_StampsNormalizedMaxAttempts(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := chatProxyWithPlan(t, up.URL, 2, []int64{1, 2})
	p.cfg.FailoverAttempts = 5

	sel, plan, _, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o", scheduler.AttemptPlanIdentity{RequestID: "req-stamp", UserID: 1})
	require.NoError(t, err)
	// the session is a stack value — bound identity echoes the request.
	require.Equal(t, "req-stamp", plan.Identity().RequestID, "compiled route must yield a plan")
	require.Equal(t, uint8(5), plan.Identity().MaxAttempts)
	sel.Release()
}

func TestSelectWithPlan_LateEligibleOverflowAccount(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := chatProxyWithPlan(t, up.URL, 3, []int64{1, 2, 3})
	// Latch the first two plan candidates: the third (late in the tail) must
	// still be reservable — no truncation, no false exhaustion.
	loader := p.sched.Loader().(noopLoader)
	for _, a := range loader.accs[10][:2] {
		fp, err := scheduler.CandidateFingerprint(a)
		require.NoError(t, err)
		require.True(t, p.sched.TryLatch(a.ID, fp, a.IdentityRevision))
	}

	sel, plan, _, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o", scheduler.AttemptPlanIdentity{RequestID: "req-overflow", UserID: 1})
	require.NoError(t, err)
	// the session is a stack value — bound identity echoes the request.
	require.Equal(t, "req-overflow", plan.Identity().RequestID)
	require.Equal(t, int64(3), sel.AccountID)
	sel.Release()
}

func TestFailoverLoop_PlanDispatchBoundedByMaxAttempts(t *testing.T) {
	var hits int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	defer up.Close()
	p := chatProxyWithPlan(t, up.URL, 4, []int64{1, 2, 3, 4})
	p.cfg.FailoverAttempts = 2

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, int64(2), atomic.LoadInt64(&hits), "dispatch must stop at max attempts")
	for id := int64(1); id <= 4; id++ {
		ri, ok := p.sched.Runtime(id)
		require.True(t, ok)
		require.Equal(t, int64(0), ri.Concurrency, "account %d lease must be released exactly once", id)
	}
}

// Replay safety: an upstream 5xx terminates the plan-driven failover — no
// second dispatch, even with attempt budget left (no proven idempotency).
func TestFailoverLoop_Plan5xxTerminatesWithoutRetry(t *testing.T) {
	var hits int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer up.Close()
	p := chatProxyWithPlan(t, up.URL, 4, []int64{1, 2, 3, 4})
	p.cfg.FailoverAttempts = 3

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, int64(1), atomic.LoadInt64(&hits), "5xx must not replay across accounts")
	for id := int64(1); id <= 4; id++ {
		ri, ok := p.sched.Runtime(id)
		require.True(t, ok)
		require.Equal(t, int64(0), ri.Concurrency, "account %d lease must be released exactly once", id)
	}
}

func tplForPlan(id int64) *domain.Template {
	return &domain.Template{ID: id, Name: "t", BaseURL: "http://127.0.0.1:9", CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"}}
}

// the session is a stack value — probe a copy, never the live session.
func planIdentityCandidateCount(p scheduler.AttemptPlan) int {
	// helper to probe plan capacity via Reserve (no exported candidate count)
	cnt := 0
	clone := p
	for {
		_, err := clone.Reserve(func(int64) bool { return false })
		if err != nil {
			break
		}
		cnt++
		if cnt > 10 {
			break
		}
	}
	// Instead return estimated via loop with true reserve on fresh plan
	fresh := p
	n := 0
	for {
		_, err := fresh.Reserve(func(int64) bool { return true })
		if err != nil {
			break
		}
		n++
	}
	return n
}

func init() { _ = strings.Contains }

// ensure compile reference to scheduler errors
var _ = scheduler.ErrAttemptsExhausted
var _ = scheduler.ErrNoAvailable
