// SPDX-License-Identifier: AGPL-3.0-or-later
package sdkbridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestRecoverProbingFailedReturnsTypedErrorAndStateObservable(t *testing.T) {
	acct := newCodexAccountForRetry(7, 3)
	store := &retryFakeStore{accounts: map[int64]*domain.Account{7: acct}, groups: map[int64][]int64{7: {10}}}
	latch := newRetryFakeLatch()
	latch.m[7] = "old"
	pubCh := make(chan struct{}, 1)
	pub := &retryFakePublisher{ch: pubCh}
	health := &fakeHealth{err: errors.New("redis down")}
	deps := FailureDeps{Store: store, Latch: latch, Publisher: pub, Health: health}
	errCh := make(chan error, 1)
	go func() { errCh <- RecoverAccount(context.Background(), deps, 7) }()
	var err error
	select {
	case err = <-errCh:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "barrier timeout")
	}
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrRecoverProbingFailed), "must return typed ErrRecoverProbingFailed, not generic")
	require.Contains(t, err.Error(), "7", "error must expose account state observably")
	require.Contains(t, err.Error(), "4", "error must expose new rev observably")
	// CAS already succeeded: revision incremented
	require.Equal(t, int64(4), store.accounts[7].LifecycleRevision, "CAS must have succeeded before probing failure")
	// fail-closed: latch not cleared, no publish after probing failure
	require.Contains(t, latch.m, int64(7), "latch must remain on probing failure (fail-closed, state observable)")
	select {
	case <-pubCh:
		require.FailNow(t, "must not publish after probing failed")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestRecoverProbingSuccessClearsLatchWithBarrier(t *testing.T) {
	acct := newCodexAccountForRetry(7, 3)
	store := &retryFakeStore{accounts: map[int64]*domain.Account{7: acct}, groups: map[int64][]int64{7: {10}}}
	latch := newRetryFakeLatch()
	latch.m[7] = "old"
	pubCh := make(chan struct{}, 1)
	pub := &retryFakePublisher{ch: pubCh}
	health := &fakeHealth{err: nil, ch: make(chan struct{}, 1)}
	deps := FailureDeps{Store: store, Latch: latch, Publisher: pub, Health: health}
	errCh := make(chan error, 1)
	go func() { errCh <- RecoverAccount(context.Background(), deps, 7) }()
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "barrier timeout")
	}
	require.Equal(t, int64(4), store.accounts[7].LifecycleRevision)
	require.Empty(t, latch.m, "latch cleared on success")
	select {
	case <-pubCh:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "publish barrier timeout")
	}
}

