// SPDX-License-Identifier: AGPL-3.0-or-later
package rule

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestRulePersist_PanicContainedAndNextProcessed(t *testing.T) {
	th := &domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: int64Ptr(1000), UseReset: false}
	sink := newFakeSink(10)
	var calls atomic.Int32
	e := New(Config{EventQueueSize: 16, PersistQueueSize: 4}, newFakeRuleStore(), nil, sink, func(ctx context.Context, item PersistItem) error {
		calls.Add(1)
		panic("injected panic for test")
	})
	e.rulesMu.Lock()
	e.rules = []compiledRule{{Rule: domain.Rule{Name: "typed", Enabled: true, Priority: 10, When: domain.RuleWhen{Kind: strPtr("429")}, Then: domain.RuleThen{Throttle: th}}}}
	e.rulesMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, e.Start(ctx))

	e.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(0)})

	// Pending must be released via defer even though callback panicked (barrier, no sleep)
	require.Eventually(t, func() bool { return e.PersistQueued() == 0 }, 2*time.Second, 5*time.Millisecond)
	require.Equal(t, int32(1), calls.Load())
	require.GreaterOrEqual(t, e.PersistQueued(), 0)
	require.Equal(t, int64(1), e.MatchedActions())

	// Close must cancel and join in-flight/supervised loop without hanging
	cancel()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	require.NoError(t, e.Close(closeCtx))
	require.Equal(t, 0, e.PersistQueued())
}

func TestRulePersist_NeverNegativeUnderLoad(t *testing.T) {
	th := &domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: int64Ptr(1000), UseReset: false}
	sink := newFakeSink(64)
	e := New(Config{EventQueueSize: 64, PersistQueueSize: 8}, newFakeRuleStore(), nil, sink, func(ctx context.Context, item PersistItem) error { return nil })
	e.rulesMu.Lock()
	e.rules = []compiledRule{{Rule: domain.Rule{Name: "typed", Enabled: true, Priority: 10, When: domain.RuleWhen{Kind: strPtr("429")}, Then: domain.RuleThen{Throttle: th}}}}
	e.rulesMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, e.Start(ctx))
	t.Cleanup(func() {
		cancel()
		_ = e.Close(context.Background())
	})

	var wg sync.WaitGroup
	wg.Add(2)
	// Producer
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			e.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(i)})
		}
	}()
	// Sampler barrier (no sleep)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if q := e.PersistQueued(); q < 0 {
				t.Errorf("PersistQueued negative: %d", q)
			}
			// Also sample channel vs pending not negative
			if e.persistPending.Load() < 0 {
				t.Errorf("pending atomic negative")
			}
		}
	}()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "load test hung")
	}
	require.Eventually(t, func() bool { return e.PersistQueued() == 0 }, 2*time.Second, 5*time.Millisecond)
	require.GreaterOrEqual(t, e.PersistQueued(), 0)
}

func TestRulePersist_QueueFullRollbackExact(t *testing.T) {
	th := &domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: int64Ptr(1000), UseReset: false}
	sink := newFakeSink(10)
	e := New(Config{EventQueueSize: 16, PersistQueueSize: 1}, newFakeRuleStore(), nil, sink, nil)
	e.rulesMu.Lock()
	e.rules = []compiledRule{{Rule: domain.Rule{Name: "typed", Enabled: true, Priority: 10, When: domain.RuleWhen{Kind: strPtr("429")}, Then: domain.RuleThen{Throttle: th}}}}
	e.rulesMu.Unlock()
	// No Start, so persist not consumed; pending reflects queued only
	e.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(0)})
	t.Logf("after first pending=%d chlen=%d", e.PersistQueued(), e.PersistQueuedChannelLen())
	require.Equal(t, 1, e.PersistQueued())
	require.Equal(t, 1, e.PersistQueuedChannelLen())
	// Second enqueue should reserve then rollback, pending stays exactly 1, dropped increments
	e.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(1)})
	t.Logf("after second pending=%d chlen=%d dropped=%d", e.PersistQueued(), e.PersistQueuedChannelLen(), e.PersistDropped())
	require.Equal(t, 1, e.PersistQueued(), "rollback must leave pending exact")
	require.Equal(t, int64(1), e.PersistDropped())
	require.Equal(t, 1, e.PersistQueuedChannelLen())
	// After flush, pending 0
	require.NoError(t, e.Close(context.Background()))
	t.Logf("after close pending=%d chlen=%d", e.PersistQueued(), e.PersistQueuedChannelLen())
	require.Equal(t, 0, e.PersistQueued())
	require.GreaterOrEqual(t, e.PersistQueued(), 0)
}
