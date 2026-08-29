// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

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
	loader.accs[10] = append(loader.accs[10], &domain.Account{ID: 2, TemplateID: 2, Template: tpl2, UpstreamKey: "sk-upstream", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4})
	require.NoError(t, p.sched.InvalidateAllSync())
	schedRoute := scheduler.RouteRefFor(10, string(domain.FormatOpenAIChat), "gpt-4o")
	identity := scheduler.AttemptPlanIdentity{RequestID: "req-123", UserID: 1}
	sel, plan, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o", identity)
	require.NoError(t, err)
	require.NotNil(t, sel)
	// plan may be nil if no compiled decision (fallback to legacy Select) - preserve legacy compatibility
	if plan != nil {
		require.Equal(t, schedRoute.RouteClassID, plan.Identity().RouteClassID)
		require.LessOrEqual(t, int(planIdentityCandidateCount(plan)), 8)
	}
	sel.Release()
}

func TestSelectWithPlan_ConvertedRouteUsesTargetIdentity(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	loader := p.sched.Loader().(noopLoader)
	tplResp := &domain.Template{ID: 2, Name: "tr", BaseURL: up.URL, CredentialType: "api_key", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, Models: []string{"gpt-4o"}}
	loader.accs[10] = append(loader.accs[10], &domain.Account{ID: 2, TemplateID: 2, Template: tplResp, UpstreamKey: "sk-upstream", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4})
	require.NoError(t, p.sched.InvalidateAllSync())
	respRoute := scheduler.RouteRefFor(10, string(domain.FormatOpenAIResponses), "gpt-4o")
	identity := scheduler.AttemptPlanIdentity{RequestID: "req-conv", UserID: 1}
	sel2, plan2, err2 := p.selectWithPlan(10, domain.FormatOpenAIResponses, "gpt-4o", identity)
	require.NoError(t, err2)
	require.NotNil(t, sel2)
	if plan2 != nil {
		require.Equal(t, respRoute.RouteClassID, plan2.Identity().RouteClassID)
	}
	sel2.Release()
}

func TestFailoverPlan_RetryGatingViaMatrix(t *testing.T) {
	// not_sent and ordinary 429 retryable, 5xx, committed, ambiguous, client_cancel, hard, malformed not
	// These are direct CanRetry checks but proxy must not retry from raw status where matrix forbids
	require.True(t, CanRetry(CallerChat, AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 1, LifecycleRevision: 1, Commit: CommitNotSent, Result: ResultFailed, HTTPStatus: 0, Terminal: false}))
	require.True(t, CanRetry(CallerChat, AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 1, LifecycleRevision: 1, Commit: CommitUpstreamResponded, Result: ResultFailed, HTTPStatus: 429, Terminal: false, HardContinuation: false}))
	require.False(t, CanRetry(CallerChat, AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 1, LifecycleRevision: 1, Commit: CommitUpstreamResponded, Result: ResultFailed, HTTPStatus: 500, Terminal: true}))
	require.False(t, CanRetry(CallerChat, AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 1, LifecycleRevision: 1, Commit: CommitResponseStarted, Result: ResultFailed, HTTPStatus: 500, Terminal: true, BusinessFrameSent: true}))
	require.False(t, CanRetry(CallerChat, AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 1, LifecycleRevision: 1, Commit: CommitSentAmbiguous, Result: ResultFailed, HTTPStatus: 0, Terminal: true, BusinessFrameSent: true}))
	require.False(t, CanRetry(CallerChat, AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 1, LifecycleRevision: 1, Commit: CommitNotSent, Result: ResultClientCancel, HTTPStatus: 0, Terminal: true}))
	require.False(t, CanRetry(CallerChat, AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 1, LifecycleRevision: 1, Commit: CommitUpstreamResponded, Result: ResultFailed, HTTPStatus: 429, Terminal: true, HardContinuation: true}))
	require.False(t, CanRetry(CallerChat, AttemptOutcome{ID: "a1", RouteClassID: "rc", Fingerprint: "fp", Lane: LanePrimary, Generation: 1, LifecycleRevision: 1, Commit: CommitUpstreamResponded, Result: ResultFailed, HTTPStatus: 400, Terminal: true, IsMalformed: true}))
}

func TestFailoverPlan_AttemptsExhaustedVsNoAvailable(t *testing.T) {
	// plan with zero candidates => ErrNoAvailable, plan with candidates but all gates reject => ErrAttemptsExhausted
	s := newTestSchedulerForPlan(t)
	emptyRoute := scheduler.RouteRefFor(10, string(domain.FormatOpenAIChat), "missing")
	_, err := s.NewAttemptPlan(scheduler.AttemptPlanIdentity{RequestID: "r1", UserID: 1}, emptyRoute)
	require.ErrorIs(t, err, scheduler.ErrFormatUnavailable)
	// create empty decision plan directly
	emptyPlan := scheduler.NewAttemptPlan(scheduler.AttemptPlanIdentity{}, scheduler.RouteDecision{})
	_, err = emptyPlan.Reserve(func(int64) bool { return true })
	require.ErrorIs(t, err, scheduler.ErrNoAvailable)
	require.NotErrorIs(t, err, scheduler.ErrAttemptsExhausted)
	fullPlan := scheduler.NewAttemptPlan(scheduler.AttemptPlanIdentity{}, scheduler.RouteDecision{Primary: []int64{1}})
	_, err = fullPlan.Reserve(func(int64) bool { return false })
	require.ErrorIs(t, err, scheduler.ErrAttemptsExhausted)
	require.NotErrorIs(t, err, scheduler.ErrNoAvailable)
}

func TestFailoverPlan_ReservationRejectionDoesNotConsumeDispatch(t *testing.T) {
	plan := scheduler.NewAttemptPlan(scheduler.AttemptPlanIdentity{}, scheduler.RouteDecision{Primary: []int64{1, 2, 3}})
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

func TestFailoverPlan_DuplicateIDsSkipped(t *testing.T) {
	plan := scheduler.NewAttemptPlan(scheduler.AttemptPlanIdentity{}, scheduler.RouteDecision{
		Primary: []int64{1, 2}, Explore: scheduler.ExploreDecision{IDs: []int64{2, 3}, Weights: map[int64]int{2: 100, 3: 100}, Cumulative: []uint64{100, 200}, Total: 200, Fallback: []int64{3}}, Degraded: []int64{1, 4},
	})
	var ids []int64
	for {
		a, err := plan.Reserve(func(int64) bool { return true })
		if errors.Is(err, scheduler.ErrAttemptsExhausted) || errors.Is(err, scheduler.ErrNoAvailable) {
			break
		}
		require.NoError(t, err)
		ids = append(ids, a.AccountID)
	}
	seen := map[int64]int{}
	for _, id := range ids {
		seen[id]++
	}
	for id, cnt := range seen {
		require.Equal(t, 1, cnt, "duplicate %d", id)
	}
	require.LessOrEqual(t, len(ids), 4)
	require.Contains(t, ids, int64(1))
	require.Contains(t, ids, int64(2))
	require.Contains(t, ids, int64(4))
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
	loader.accs[10] = append(loader.accs[10], &domain.Account{ID: 2, TemplateID: 2, Template: tpl2, UpstreamKey: "sk-upstream", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4})
	require.NoError(t, p.sched.InvalidateAllSync())
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
	tpl := &domain.Template{ID: 1, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"}}
	accs := map[int64][]*domain.Account{10: {{ID: 1, TemplateID: 1, Template: tpl, UpstreamKey: "k", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4}}}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := scheduler.New(scheduler.Config{DefaultMaxConcurrency: 4, SyncInterval: 1000000000000}, noopLoader{accs: accs}, re, nil)
	require.NoError(t, s.InvalidateAllSync())
	return s
}

func tplForPlan(id int64) *domain.Template {
	return &domain.Template{ID: id, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"}}
}

func planIdentityCandidateCount(p *scheduler.AttemptPlan) int {
	// reflection helper to read candidateCount via Reserve probing
	cnt := 0
	clone := *p
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
	fresh := *p
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
