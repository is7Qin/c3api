// SPDX-License-Identifier: AGPL-3.0-or-later
package sdkbridge

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// RED tests with barriers/no sleeps, require-only.

// TestCanonicalFingerprint_NoEmptyBaseURLFallback：空 base_url 绝不 fallback
// 到任何默认 origin（旧缺陷裁决保持）。细化后的契约（accountcred.go：codex
// 数据面 URL 归 SDK 官方默认所有，网关不派生 BaseURL）：SDK 托管类型（codex）
// 空 base_url 合法——凭据身份由 PAT/OAuth 输入承载，canonical fingerprint
// 可产出（health 事件/探测的 Attempt 身份对生产 codex 账号可用）；静态路由族
// （api_key）空 base_url = 不可路由，仍返回 typed missing identity。
func TestCanonicalFingerprint_NoEmptyBaseURLFallback(t *testing.T) {
	tpl := &domain.Template{ID: 10, BaseURL: "", CredentialType: credential.TypeCodexOAuth, StripImageTools: false}
	acct := &domain.Account{ID: 7, TemplateID: 10, Template: tpl, UpstreamKey: "sk-test", LifecycleRevision: 1, Ext: &domain.AccountExt{CredentialType: credential.TypeCodexOAuth}}
	fp, err := canonicalFingerprint(acct)
	require.NoError(t, err, "codex empty base_url must fingerprint via credential identity")
	require.NotEmpty(t, fp)

	tplStatic := &domain.Template{ID: 11, BaseURL: "", CredentialType: credential.TypeAPIKey}
	acctStatic := &domain.Account{ID: 8, TemplateID: 11, Template: tplStatic, UpstreamKey: "sk-test", LifecycleRevision: 1}
	_, err = canonicalFingerprint(acctStatic)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrMissingCandidateFingerprint), "api_key empty baseURL must return typed missing identity, not fallback to default")
}

func TestCredentialDiscriminatorMatchesScheduler_TemplateAuthority(t *testing.T) {
	// Scheduler uses Template.CredentialType only; Ext mismatched must be ignored.
	// Case A: Template api_key but Ext codex_oauth => should NOT self-fail via SDK (non-Codex)
	tplA := &domain.Template{ID: 10, BaseURL: "https://api.openai.com", CredentialType: credential.TypeAPIKey}
	acctA := &domain.Account{ID: 1, TemplateID: 10, Template: tplA, UpstreamKey: "k1", LifecycleRevision: 1, Ext: &domain.AccountExt{CredentialType: credential.TypeCodexOAuth, CodexIdentity: &domain.CodexIdentity{InstallationID: "i", SessionID: "s"}}}
	storeA := &fakeCASStore2{accounts: map[int64]*domain.Account{1: acctA}}
	latchA := newFakeLatch2()
	failerA := &fakeFailer2{}
	depsA := FailureDeps{Store: storeA, Failer: failerA, Latch: latchA}
	require.NoError(t, HandleFailure(context.Background(), depsA, 1, fmt.Errorf("fatal")))
	require.Empty(t, failerA.failed, "non-Codex Template must not self-fail even if Ext says Codex")
	require.Empty(t, latchA.m)

	// Case B: Template codex_oauth but Ext api_key => should self-fail (scheduler authority is Template)
	tplB := &domain.Template{ID: 10, BaseURL: "https://api.openai.com", CredentialType: credential.TypeCodexOAuth}
	acctB := &domain.Account{ID: 2, TemplateID: 10, Template: tplB, UpstreamKey: "", LifecycleRevision: 1, Ext: &domain.AccountExt{CredentialType: credential.TypeAPIKey}}
	storeB := &fakeCASStore2{accounts: map[int64]*domain.Account{2: acctB}}
	latchB := newFakeLatch2()
	failerB := &fakeFailer2{}
	depsB := FailureDeps{Store: storeB, Failer: failerB, Latch: latchB}
	require.NoError(t, HandleFailure(context.Background(), depsB, 2, fmt.Errorf("fatal")))
	require.Equal(t, []int64{2}, failerB.failed, "Template Codex must self-fail even if Ext says api_key")
}

func TestCanonicalFingerprint_MatchesSchedulerAuthority(t *testing.T) {
	tpl := &domain.Template{ID: 10, BaseURL: "https://api.openai.com/v1", CredentialType: credential.TypeCodexOAuth, StripImageTools: true}
	acct := &domain.Account{ID: 7, TemplateID: 10, Template: tpl, UpstreamKey: "sk", LifecycleRevision: 1, Ext: &domain.AccountExt{CredentialType: credential.TypeCodexOAuth, CodexPATKey: strPtrBlockers("pat"), CodexEmail: strPtrBlockers("e@e.com"), CodexAccountID: strPtrBlockers("ca"), CodexIdentity: &domain.CodexIdentity{InstallationID: "i", SessionID: "s", ThreadID: "th", WindowID: "w"}}}
	fpSched, err := domain.AccountCandidateFingerprint(acct)
	require.NoError(t, err)
	want := domain.CandidateFPHex(fpSched)
	got, err := canonicalFingerprint(acct)
	require.NoError(t, err)
	require.Equal(t, want, got, "sdkbridge fingerprint must exactly match scheduler authority via shared domain helper")
}

func strPtrBlockers(s string) *string { v := s; return &v }

// per-ID faking store for HOL test

type holStore struct {
	mu       sync.Mutex
	accounts map[int64]*domain.Account
	seq      map[int64][]error
	calls    map[int64]int
	pubs     map[int64]chan struct{}
	groups   map[int64][]int64
}

func (f *holStore) GetAccount(_ context.Context, id int64) (*domain.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.accounts[id]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	cp := *a
	if a.Template != nil {
		t := *a.Template
		cp.Template = &t
	}
	if a.Ext != nil {
		e := *a.Ext
		if a.Ext.CodexIdentity != nil {
			ci := *a.Ext.CodexIdentity
			e.CodexIdentity = &ci
		}
		cp.Ext = &e
	}
	if a.BaseURL != nil {
		b := *a.BaseURL
		cp.BaseURL = &b
	}
	return &cp, nil
}
func (f *holStore) FailAccountCAS(_ context.Context, id int64, expected int64, _ string, _ time.Time, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = make(map[int64]int)
	}
	idx := f.calls[id]
	f.calls[id]++
	if seq, ok := f.seq[id]; ok && idx < len(seq) {
		if seq[idx] != nil {
			return seq[idx]
		}
	}
	a, ok := f.accounts[id]
	if !ok {
		return fmt.Errorf("not found")
	}
	if a.LifecycleRevision != expected {
		return fmt.Errorf("stale")
	}
	a.LifecycleRevision = expected + 1
	return nil
}
func (f *holStore) SetAccountFailed(_ context.Context, _ int64, _ time.Time, _ string) error {
	return nil
}
func (f *holStore) GetAccountGroups(_ context.Context, id int64) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.groups[id], nil
}
func (f *holStore) RecoverAccountCAS(_ context.Context, id int64, expected int64) error { return nil }

type holPublisher struct {
	mu   sync.Mutex
	ch   map[int64]chan struct{}
	hits map[int64]int
}

func (p *holPublisher) PublishGroups(_ context.Context, gids []int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, id := range gids {
		if ch, ok := p.ch[id]; ok {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
		p.hits[id]++
	}
}

func TestRetryHOLFairness_SecondTaskNotStarved(t *testing.T) {
	ResetFailureRetryForTest()
	retryBackoff = 20 * time.Millisecond
	retryMaxBackoff = 40 * time.Millisecond

	tplA := &domain.Template{ID: 10, BaseURL: "https://api.openai.com", CredentialType: credential.TypeCodexOAuth}
	tplB := &domain.Template{ID: 10, BaseURL: "https://api.openai.com", CredentialType: credential.TypeCodexOAuth}
	acct7 := &domain.Account{ID: 7, TemplateID: 10, Template: tplA, UpstreamKey: "sk", LifecycleRevision: 1, Ext: &domain.AccountExt{CredentialType: credential.TypeCodexOAuth}}
	acct8 := &domain.Account{ID: 8, TemplateID: 10, Template: tplB, UpstreamKey: "sk2", LifecycleRevision: 1, Ext: &domain.AccountExt{CredentialType: credential.TypeCodexOAuth}}

	store := &holStore{
		accounts: map[int64]*domain.Account{7: acct7, 8: acct8},
		seq: map[int64][]error{
			7: {errors.New("transient"), errors.New("transient"), errors.New("transient"), errors.New("transient"), errors.New("transient")},
			8: {errors.New("transient")},
		},
		groups: map[int64][]int64{8: {10}},
	}
	latch := newRetryFakeLatch()
	pubCh8 := make(chan struct{}, 1)
	genPub := &retryFakePublisher{ch: pubCh8}
	// For 7, use no-op publisher to avoid mixing
	deps7 := FailureDeps{Store: store, Failer: &retryFakeFailer{}, Latch: latch, Publisher: &retryFakePublisher{}}
	deps8 := FailureDeps{Store: store, Failer: &retryFakeFailer{}, Latch: latch, Publisher: genPub, Log: nil}

	// trigger both failures via HandleFailure (each enqueues retry after transient CAS failure)
	// barrier: ensure nonblocking
	done7 := make(chan error, 1)
	done8 := make(chan error, 1)
	go func() { done7 <- HandleFailure(context.Background(), deps7, 7, errors.New("fatal7")) }()
	go func() { done8 <- HandleFailure(context.Background(), deps8, 8, errors.New("fatal8")) }()
	barrier := make(chan struct{})
	go func() {
		var c int
		for c < 2 {
			select {
			case <-done7:
				c++
			case <-done8:
				c++
			case <-time.After(500 * time.Millisecond):
				close(barrier)
				return
			}
		}
		close(barrier)
	}()
	select {
	case <-barrier:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "HandleFailure barrier timeout")
	}
	// Both returned transient errors (enqueued). Now wait for 8 to succeed despite 7 never succeeding.
	select {
	case <-pubCh8:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "HOL fairness: second task never persisted while first permanently failing")
	}
	store.mu.Lock()
	rev8 := store.accounts[8].LifecycleRevision
	store.mu.Unlock()
	require.Equal(t, int64(2), rev8, "second task must have succeeded despite first starving")
	// first task still not succeeded (should still be retrying, not succeeded)
	store.mu.Lock()
	rev7 := store.accounts[7].LifecycleRevision
	store.mu.Unlock()
	require.Equal(t, int64(1), rev7, "first permanent failure must not have succeeded")
	ResetFailureRetryForTest()
}

// TestRetryStaleFencedAfterReAdd_MustNotDisableReadded pins the generation/identity
// fence of the SDK failure-retry path: a task captured at failure time must be
// dropped — never CAS'd, never published — once the account was removed and re-added
// with a new revision and identity, and the process-local latch must be cleared so
// the re-added account becomes selectable again.
//
// Driven directly through handleRetryOnce with the exact task HandleFailure would
// have enqueued. The retry loop's FIRST attempt has no backoff (enqueue → immediate
// dequeue), so driving this through the queue races the test's own setup and flakes
// under load. Enqueue/HOL-fairness behavior is covered by
// TestRetryHOLFairness_SecondTaskNotStarved.
func TestRetryStaleFencedAfterReAdd_MustNotDisableReadded(t *testing.T) {
	tpl := &domain.Template{ID: 10, BaseURL: "https://api.openai.com", CredentialType: credential.TypeCodexOAuth}
	origURL := "https://api.openai.com"
	acct := &domain.Account{ID: 7, TemplateID: 10, Template: tpl, UpstreamKey: "sk", BaseURL: &origURL, LifecycleRevision: 1, Ext: &domain.AccountExt{CredentialType: credential.TypeCodexOAuth}}
	store := &holStore{
		accounts: map[int64]*domain.Account{7: acct},
		groups:   map[int64][]int64{7: {10}},
	}
	staleFP, err := canonicalFingerprint(acct)
	require.NoError(t, err)
	latch := newRetryFakeLatch()
	require.True(t, latch.TryAcquire(7, staleFP, 1), "HandleFailure's acquire step")
	pubCh := make(chan struct{}, 1)
	deps := FailureDeps{Store: store, Failer: &retryFakeFailer{}, Latch: latch, Publisher: &retryFakePublisher{ch: pubCh}}

	// Simulate remove/re-add: bump revision and change identity before the retry fires.
	store.mu.Lock()
	store.accounts[7].LifecycleRevision = 5
	newURL := "https://changed.example.com"
	store.accounts[7].BaseURL = &newURL
	store.mu.Unlock()

	requeued := handleRetryOnce(context.Background(), failureRetryTask{
		accountID: 7, fingerprint: staleFP, revision: 1, reason: "fatal", deps: deps,
	})
	require.False(t, requeued, "stale task must be fully fenced, never requeued")
	store.mu.Lock()
	rev := store.accounts[7].LifecycleRevision
	store.mu.Unlock()
	require.Equal(t, int64(5), rev, "stale retry must not CAS re-added account (generation fencing)")
	select {
	case <-pubCh:
		require.FailNow(t, "fenced retry must not publish for stale re-added account")
	default:
	}
	require.False(t, latch.IsLatched(7, staleFP), "stale retry must clear latch for fencing")
}

// TestRetryStaleFencedWhenDeleted_MustNotDisableReaddedSameID pins the deletion
// fence: a retry task enqueued before the account was deleted must be dropped
// (latch cleared, nothing CAS'd, nothing published) and must stay fenced when the
// same ID is re-added with a new revision. Driven directly through handleRetryOnce
// for the same reason as the sibling re-add test (queue's first attempt has no
// backoff and would race this test's setup).
func TestRetryStaleFencedWhenDeleted_MustNotDisableReaddedSameID(t *testing.T) {
	tpl := &domain.Template{ID: 10, BaseURL: "https://api.openai.com", CredentialType: credential.TypeCodexOAuth}
	origURL := "https://api.openai.com"
	acct := &domain.Account{ID: 7, TemplateID: 10, Template: tpl, UpstreamKey: "sk", BaseURL: &origURL, LifecycleRevision: 1, Ext: &domain.AccountExt{CredentialType: credential.TypeCodexOAuth}}
	store := &holStore{accounts: map[int64]*domain.Account{7: acct}, groups: map[int64][]int64{7: {10}}}
	staleFP, err := canonicalFingerprint(acct)
	require.NoError(t, err)
	latch := newRetryFakeLatch()
	require.True(t, latch.TryAcquire(7, staleFP, 1), "HandleFailure's acquire step")
	pubCh := make(chan struct{}, 1)
	deps := FailureDeps{Store: store, Failer: &retryFakeFailer{}, Latch: latch, Publisher: &retryFakePublisher{ch: pubCh}}
	task := failureRetryTask{accountID: 7, fingerprint: staleFP, revision: 1, reason: "fatal", deps: deps}

	// Mark deleted before the retry runs.
	now := time.Now()
	store.mu.Lock()
	store.accounts[7].DeletedAt = &now
	store.mu.Unlock()
	require.False(t, handleRetryOnce(context.Background(), task), "deleted account task must be fenced")
	require.False(t, latch.IsLatched(7, staleFP), "deleted account retry must be fenced and clear latch")

	// Re-add same ID (deleted flag cleared, new revision): the old task must still
	// not CAS it.
	store.mu.Lock()
	store.accounts[7].DeletedAt = nil
	store.accounts[7].LifecycleRevision = 10
	store.mu.Unlock()
	require.False(t, handleRetryOnce(context.Background(), task), "stale task must stay fenced after re-add")
	store.mu.Lock()
	rev := store.accounts[7].LifecycleRevision
	store.mu.Unlock()
	require.Equal(t, int64(10), rev, "re-added same-ID account must not be disabled by stale callback")
	select {
	case <-pubCh:
		require.FailNow(t, "fenced retry must not publish for deleted/re-added account")
	default:
	}
}
