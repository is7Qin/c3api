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
	"github.com/is7qin/c3api/internal/repository"
)

// Test doubles for failure handling.
type retryFakeStore struct {
	mu        sync.Mutex
	accounts  map[int64]*domain.Account
	failSeq   []error
	callCount int
	published []int64
	groups    map[int64][]int64
}

func (f *retryFakeStore) GetAccount(_ context.Context, id int64) (*domain.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.accounts[id]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	cp := *a
	// deep copy template to avoid race on fingerprint recompute
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
	return &cp, nil
}
func (f *retryFakeStore) FailAccountCAS(_ context.Context, id int64, expected int64, _ string, _ time.Time, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.callCount < len(f.failSeq) {
		err := f.failSeq[f.callCount]
		f.callCount++
		if err != nil {
			return err
		}
	}
	// success path: fence on the identity generation
	a, ok := f.accounts[id]
	if !ok {
		return fmt.Errorf("not found")
	}
	if a.IdentityRevision != expected {
		return fmt.Errorf("%w: stale", repository.ErrStaleIdentityRevision)
	}
	a.LifecycleRevision++
	f.callCount++
	return nil
}
func (f *retryFakeStore) SetAccountFailed(_ context.Context, _ int64, _ time.Time, _ string) error {
	return nil
}
func (f *retryFakeStore) GetAccountGroups(_ context.Context, id int64) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.groups[id], nil
}

type retryFakeLatch struct {
	mu     sync.Mutex
	m      map[int64]string
	kv     map[int64]int64
	clears int
}

func newRetryFakeLatch() *retryFakeLatch {
	return &retryFakeLatch{m: make(map[int64]string), kv: make(map[int64]int64)}
}
func (f *retryFakeLatch) TryAcquire(id int64, fp string, rev int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[id] = fp
	f.kv[id] = rev
	return true
}
func (f *retryFakeLatch) Clear(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, id)
	delete(f.kv, id)
	f.clears++
}
func (f *retryFakeLatch) IsLatched(id int64, fp string, rev int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.m[id]
	if !ok {
		return false
	}
	if fp != "" && v != fp {
		return false
	}
	if rev != 0 && f.kv[id] != rev {
		return false
	}
	return true
}

type retryFakeFailer struct {
	mu    sync.Mutex
	calls int
}

func (f *retryFakeFailer) FailAccount(_ int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
}

type retryFakePublisher struct {
	mu   sync.Mutex
	gids [][]int64
	ch   chan struct{}
}

func (f *retryFakePublisher) PublishGroups(_ context.Context, gids []int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gids = append(f.gids, gids)
	if f.ch != nil {
		select {
		case f.ch <- struct{}{}:
		default:
		}
	}
}

func newCodexAccountForRetry(id int64, rev int64) *domain.Account {
	tpl := &domain.Template{ID: 10, BaseURL: "https://api.openai.com", CredentialType: credential.TypeCodexOAuth, StripImageTools: false}
	return &domain.Account{
		// IdentityRevision 与 C 同置 rev（镜像生产不变量：两者都自 1 起、独立推进）。
		// 测试意图是「账号处于代际 rev」，PROBING 现在以 K 落键。
		ID: id, TemplateID: 10, Template: tpl, UpstreamKey: "sk-test", LifecycleRevision: rev, IdentityRevision: rev,
		Ext: &domain.AccountExt{CredentialType: credential.TypeCodexOAuth},
	}
}

func TestFailureRetry_NonblockingAndProcessLifetime(t *testing.T) {
	ResetFailureRetryForTest()
	oldBackoff, oldMax := retryBackoff, retryMaxBackoff
	defer func() { retryBackoff, retryMaxBackoff = oldBackoff, oldMax }()
	retryBackoff = 10 * time.Millisecond
	retryMaxBackoff = 20 * time.Millisecond
	store := &retryFakeStore{
		accounts: map[int64]*domain.Account{7: newCodexAccountForRetry(7, 1)},
		failSeq:  []error{errors.New("transient db down")},
		groups:   map[int64][]int64{7: {10}},
	}
	latch := newRetryFakeLatch()
	failer := &retryFakeFailer{}
	pubCh := make(chan struct{}, 1)
	pub := &retryFakePublisher{ch: pubCh}
	deps := FailureDeps{Store: store, Failer: failer, Latch: latch, Publisher: pub}
	// barrier for nonblocking: HandleFailure must return quickly
	done := make(chan error, 1)
	go func() { done <- HandleFailure(context.Background(), deps, 7, errors.New("fatal")) }()
	select {
	case err := <-done:
		require.Error(t, err, "first attempt transient should return error")
	case <-time.After(200 * time.Millisecond):
		require.FailNow(t, "HandleFailure blocking, expected nonblocking")
	}
	// retry should eventually succeed and publish
	select {
	case <-pubCh:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "retry did not succeed within deadline")
	}
	require.Equal(t, int64(2), store.accounts[7].LifecycleRevision, "retry must CAS revision+1")
	require.Empty(t, latch.m, "latch cleared after retry success")
	require.Equal(t, 1, failer.calls, "FailAccount called once (initial, fail-closed)")
	ResetFailureRetryForTest()
}

func TestFailureRetry_NoRetryAfterShutdown(t *testing.T) {
	ResetFailureRetryForTest()
	oldBackoff := retryBackoff
	defer func() { retryBackoff = oldBackoff }()
	retryBackoff = 10 * time.Millisecond
	store := &retryFakeStore{
		accounts: map[int64]*domain.Account{7: newCodexAccountForRetry(7, 1)},
		failSeq:  []error{errors.New("transient")},
	}
	latch := newRetryFakeLatch()
	failer := &retryFakeFailer{}
	pub := &retryFakePublisher{ch: make(chan struct{}, 1)}
	deps := FailureDeps{Store: store, Failer: failer, Latch: latch, Publisher: pub}
	ShutdownFailureRetry()
	// after shutdown, HandleFailure should not enqueue retry
	err := HandleFailure(context.Background(), deps, 7, errors.New("fatal"))
	require.Error(t, err)
	// give a window via barrier: ensure no publish arrives
	select {
	case <-pub.ch:
		require.FailNow(t, "retry occurred after shutdown, must not")
	case <-time.After(150 * time.Millisecond):
	}
	require.Equal(t, 1, store.callCount, "only initial attempt, no retry after shutdown")
	ResetFailureRetryForTest()
}

// TestFailure_StaleFencedByCanonicalFingerprint pins the identity fence: a retry
// task whose captured fingerprint no longer matches the stored account must be
// dropped (no CAS) and must clear the latch. Driven directly through
// handleRetryOnce — the retry loop's first attempt has no backoff, so routing
// this through HandleFailure+queue races both the latch assertion and the store
// mutation (observed as a load-flaky rev==6 under `-race` stress).
func TestFailure_StaleFencedByCanonicalFingerprint(t *testing.T) {
	acct := newCodexAccountForRetry(7, 5)
	store := &retryFakeStore{
		accounts: map[int64]*domain.Account{7: acct},
		failSeq:  []error{errors.New("transient")},
	}
	fpBefore, err := canonicalFingerprint(acct)
	require.NoError(t, err)
	latch := newRetryFakeLatch()
	require.True(t, latch.TryAcquire(7, fpBefore, 5), "HandleFailure's acquire step")
	pubCh := make(chan struct{}, 1)
	deps := FailureDeps{Store: store, Failer: &retryFakeFailer{}, Latch: latch, Publisher: &retryFakePublisher{ch: pubCh}}

	// Mutate the stored identity before the retry attempt: change the canonical
	// base URL (affects the CodexOAuth fingerprint).
	store.mu.Lock()
	newURL := "https://changed.example.com"
	store.accounts[7].BaseURL = &newURL
	store.mu.Unlock()

	requeued := handleRetryOnce(context.Background(), failureRetryTask{
		accountID: 7, fingerprint: fpBefore, identityRevision: 5, reason: "fatal", deps: deps,
	})
	require.False(t, requeued, "fingerprint mismatch must fully fence, never requeue")
	store.mu.Lock()
	rev := store.accounts[7].LifecycleRevision
	store.mu.Unlock()
	require.NotEqual(t, int64(6), rev, "should not have CASed after fingerprint mismatch")
	select {
	case <-pubCh:
		require.FailNow(t, "fenced retry must not publish")
	default:
	}
	require.False(t, latch.IsLatched(7, fpBefore, 0), "stale fingerprint must clear latch and fence")
}

func TestFailure_StaleFencedByExpectedRevision(t *testing.T) {
	ResetFailureRetryForTest()
	retryBackoff = 10 * time.Millisecond
	acct := newCodexAccountForRetry(7, 5)
	store := &retryFakeStore{
		accounts: map[int64]*domain.Account{7: acct},
		failSeq:  []error{repository.ErrStaleIdentityRevision},
	}
	latch := newRetryFakeLatch()
	deps := FailureDeps{Store: store, Failer: &retryFakeFailer{}, Latch: latch}
	err := HandleFailure(context.Background(), deps, 7, errors.New("fatal"))
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrStaleFailureRevision))
	// stale should not be retried: callCount should be 1 (no retry)
	retryMu.Lock()
	qlen := len(retryQueue)
	retryMu.Unlock()
	require.Equal(t, 0, qlen, "stale must not enqueue retry")
	ResetFailureRetryForTest()
}
