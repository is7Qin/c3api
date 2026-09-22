// SPDX-License-Identifier: AGPL-3.0-or-later
package sdkbridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

func TestRequeueDoesNotPanicAfterReset(t *testing.T) {
	ResetFailureRetryForTest()
	defer ResetFailureRetryForTest()
	origBackoff := retryBackoff
	origMax := retryMaxBackoff
	retryBackoff = 50 * time.Millisecond
	retryMaxBackoff = 100 * time.Millisecond
	defer func() {
		retryBackoff = origBackoff
		retryMaxBackoff = origMax
	}()

	store := &retryFakeStore{
		accounts: map[int64]*domain.Account{99: newCodexAccountForRetry(99, 1)},
		failSeq:  []error{errors.New("transient"), errors.New("transient"), errors.New("transient")},
		groups:   map[int64][]int64{99: {10}},
	}
	latch := newRetryFakeLatch()
	pubCh := make(chan struct{}, 1)
	pub := &retryFakePublisher{ch: pubCh}
	deps := FailureDeps{Store: store, Failer: &retryFakeFailer{}, Latch: latch, Publisher: pub}

	// Trigger failure that will transiently fail CAS and enqueue retry.
	// HandleFailure failSeq[0] transient -> enqueue. Worker handles retry, failSeq[1] transient -> requeueWithBackoff launches goroutine.
	err := HandleFailure(context.Background(), deps, 99, errors.New("fatal"))
	require.Error(t, err)

	// Barrier: ensure the retry task has been dequeued and requeue goroutine started.
	// Wait briefly via channel, not Sleep, to let worker loop process first retry attempt before reset.
	select {
	case <-time.After(20 * time.Millisecond):
	case <-pubCh:
		require.FailNow(t, "unexpected publish before reset")
	}

	// Capture goroutine panic via done channel: Reset must cancel and wait for worker loop to exit,
	// and the delayed requeue goroutine holds a captured context/queue snapshot so it will not deref nil.
	done := make(chan struct{})
	go func() { ResetFailureRetryForTest(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "Reset did not return, deadlock")
	}

	// Recreate fresh worker for next assertion: next failure after reset must work without panic.
	// Re-arm store for a second attempt that succeeds on retry.
	store2 := &retryFakeStore{
		accounts: map[int64]*domain.Account{100: {
			ID: 100, TemplateID: 10, Template: &domain.Template{ID: 10, BaseURL: "https://api.openai.com", CredentialType: credential.TypeCodexOAuth},
			UpstreamKey: "sk", LifecycleRevision: 1, IdentityRevision: 1, Ext: &domain.AccountExt{CredentialType: credential.TypeCodexOAuth},
		}},
		failSeq: []error{errors.New("transient")},
		groups:  map[int64][]int64{100: {10}},
	}
	latch2 := newRetryFakeLatch()
	pubCh2 := make(chan struct{}, 1)
	pub2 := &retryFakePublisher{ch: pubCh2}
	deps2 := FailureDeps{Store: store2, Failer: &retryFakeFailer{}, Latch: latch2, Publisher: pub2}
	require.Error(t, HandleFailure(context.Background(), deps2, 100, errors.New("fatal")))
	select {
	case <-pubCh2:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "retry after reset did not succeed, lifecycle broken")
	}
	require.Equal(t, int64(2), store2.accounts[100].LifecycleRevision)
	require.Empty(t, latch2.m)

	// Ensure the stale requeue from first task (99) did not panic and did not CAS after reset.
	require.Equal(t, int64(1), store.accounts[99].LifecycleRevision, "stale requeue must not have CASed after reset")
}
