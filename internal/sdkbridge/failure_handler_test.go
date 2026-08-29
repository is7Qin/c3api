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
	// success path: check revision
	a, ok := f.accounts[id]
	if !ok {
		return fmt.Errorf("not found")
	}
	if a.LifecycleRevision != expected {
		return fmt.Errorf("%w: stale", repository.ErrStaleRevision)
	}
	a.LifecycleRevision = expected + 1
	f.callCount++
	return nil
}
func (f *retryFakeStore) SetAccountFailed(_ context.Context, _ int64, _ time.Time, _ string) error { return nil }
func (f *retryFakeStore) GetAccountGroups(_ context.Context, id int64) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.groups[id], nil
}
func (f *retryFakeStore) RecoverAccountCAS(_ context.Context, id int64, expected int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.accounts[id]
	if !ok {
		return fmt.Errorf("not found")
	}
	if a.LifecycleRevision != expected {
		return fmt.Errorf("%w: stale", repository.ErrStaleRevision)
	}
	a.LifecycleRevision = expected + 1
	return nil
}

type retryFakeLatch struct {
	mu sync.Mutex
	m  map[int64]string
	clears int
}

func newRetryFakeLatch() *retryFakeLatch { return &retryFakeLatch{m: make(map[int64]string)} }
func (f *retryFakeLatch) TryAcquire(id int64, fp string, rev int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[id] = fp
	_ = rev
	return true
}
func (f *retryFakeLatch) Clear(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, id)
	f.clears++
}
func (f *retryFakeLatch) IsLatched(id int64, fp string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.m[id]
	return ok && v == fp
}

type retryFakeFailer struct {
	mu     sync.Mutex
	calls  int
	reason string
}

func (f *retryFakeFailer) FailAccount(_ int64, r string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.reason = r
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

type fakeHealth struct {
	mu       sync.Mutex
	calls    []struct{ id, rev int64 }
	err      error
	ch       chan struct{}
}

func (f *fakeHealth) SetProbing(_ context.Context, id int64, rev int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, struct{ id, rev int64 }{id, rev})
	if f.ch != nil {
		select { case f.ch <- struct{}{}: default: }
	}
	return f.err
}

func newCodexAccountForRetry(id int64, rev int64) *domain.Account {
	tpl := &domain.Template{ID: 10, BaseURL: "https://api.openai.com", CredentialType: credential.TypeCodexOAuth, StripImageTools: false}
	return &domain.Account{
		ID: id, TemplateID: 10, Template: tpl, UpstreamKey: "sk-test", LifecycleRevision: rev,
		Ext: &domain.AccountExt{CredentialType: credential.TypeCodexOAuth},
	}
}

func TestFailureRetry_NonblockingAndProcessLifetime(t *testing.T) {
	ResetFailureRetryForTest()
	retryBackoff = 10 * time.Millisecond
	retryMaxBackoff = 20 * time.Millisecond
	store := &retryFakeStore{
		accounts: map[int64]*domain.Account{7: newCodexAccountForRetry(7, 1)},
		failSeq: []error{errors.New("transient db down")},
		groups: map[int64][]int64{7: {10}},
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
	retryBackoff = 10 * time.Millisecond
	store := &retryFakeStore{
		accounts: map[int64]*domain.Account{7: newCodexAccountForRetry(7, 1)},
		failSeq: []error{errors.New("transient")},
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

func TestFailure_StaleFencedByCanonicalFingerprint(t *testing.T) {
	ResetFailureRetryForTest()
	retryBackoff = 10 * time.Millisecond
	// account with initial fingerprint A
	acct := newCodexAccountForRetry(7, 5)
	store := &retryFakeStore{
		accounts: map[int64]*domain.Account{7: acct},
		failSeq: []error{errors.New("transient")},
	}
	latch := newRetryFakeLatch()
	failer := &retryFakeFailer{}
	deps := FailureDeps{Store: store, Failer: failer, Latch: latch}
	fpBefore, err := canonicalFingerprint(acct)
	require.NoError(t, err)
	// trigger failure with initial fingerprint
	err = HandleFailure(context.Background(), deps, 7, errors.New("fatal"))
	require.Error(t, err)
	require.Contains(t, latch.m, int64(7))
	require.Equal(t, fpBefore, latch.m[7])
	// mutate DB fingerprint before retry: change canonical base URL (affects CodexOAuth fingerprint)
	store.mu.Lock()
	newURL := "https://changed.example.com"
	store.accounts[7].BaseURL = &newURL
	store.mu.Unlock()
	// wait for retry to attempt and fence
	select {
	case <-time.After(300 * time.Millisecond):
	}
	// retry should have fenced and cleared latch, not CAS
	require.NotEqual(t, int64(6), store.accounts[7].LifecycleRevision, "should not have CASed after fingerprint mismatch")
	latch.mu.Lock()
	_, stillLatched := latch.m[7]
	latch.mu.Unlock()
	require.False(t, stillLatched, "stale fingerprint must clear latch and fence")
	ResetFailureRetryForTest()
}

func TestFailure_StaleFencedByExpectedRevision(t *testing.T) {
	ResetFailureRetryForTest()
	retryBackoff = 10 * time.Millisecond
	acct := newCodexAccountForRetry(7, 5)
	store := &retryFakeStore{
		accounts: map[int64]*domain.Account{7: acct},
		failSeq: []error{repository.ErrStaleRevision},
	}
	latch := newRetryFakeLatch()
	deps := FailureDeps{Store: store, Failer: &retryFakeFailer{}, Latch: latch}
	err := HandleFailure(context.Background(), deps, 7, errors.New("fatal"))
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrStaleFailureRevision))
	// stale should not be retried: callCount should be 1 (no retry)
	require.Equal(t, 0, len(retryQueue), "stale must not enqueue retry")
	ResetFailureRetryForTest()
}

func TestRecover_CASAndProbing(t *testing.T) {
	ResetFailureRetryForTest()
	acct := newCodexAccountForRetry(7, 3)
	store := &retryFakeStore{accounts: map[int64]*domain.Account{7: acct}, groups: map[int64][]int64{7: {10}}}
	latch := newRetryFakeLatch()
	latch.m[7] = "old"
	pubCh := make(chan struct{}, 1)
	pub := &retryFakePublisher{ch: pubCh}
	healthCh := make(chan struct{}, 1)
	health := &fakeHealth{ch: healthCh}
	deps := FailureDeps{Store: store, Latch: latch, Publisher: pub, Health: health}
	require.NoError(t, RecoverAccount(context.Background(), deps, 7))
	require.Equal(t, int64(4), store.accounts[7].LifecycleRevision, "CAS current revision+1")
	health.mu.Lock()
	require.Len(t, health.calls, 1)
	require.Equal(t, int64(7), health.calls[0].id)
	require.Equal(t, int64(4), health.calls[0].rev, "PROBING revision must be new revision")
	health.mu.Unlock()
	require.Empty(t, latch.m, "latch cleared after recover")
	select {
	case <-pubCh:
	case <-time.After(100 * time.Millisecond):
		require.FailNow(t, "publish not called")
	}
}

func TestRecover_HealthUnsupportedTyped(t *testing.T) {
	acct := newCodexAccountForRetry(7, 3)
	store := &retryFakeStore{accounts: map[int64]*domain.Account{7: acct}}
	deps := FailureDeps{Store: store, Health: nil}
	err := RecoverAccount(context.Background(), deps, 7)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrHealthUnsupported), "must return typed unsupported when health absent, never silent")
	require.Equal(t, int64(3), store.accounts[7].LifecycleRevision, "must not CAS when health unsupported")
}

func TestRecover_MissingExpectedRevision(t *testing.T) {
	acct := newCodexAccountForRetry(7, 0)
	store := &retryFakeStore{accounts: map[int64]*domain.Account{7: acct}}
	health := &fakeHealth{}
	deps := FailureDeps{Store: store, Health: health}
	err := RecoverAccount(context.Background(), deps, 7)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrMissingExpectedRevision))
}
