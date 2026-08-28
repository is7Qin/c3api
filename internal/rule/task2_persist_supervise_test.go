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
	"github.com/is7qin/c3api/internal/worker"
)

func TestRulePersist_PanicContainedAndNextProcessed(t *testing.T) {
	oldDelay := workerLoopRestartDelay()
	worker.SetLoopRestartDelayForTest(10 * time.Millisecond)
	t.Cleanup(func() { worker.SetLoopRestartDelayForTest(oldDelay) })

	th := &domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: int64Ptr(1000), UseReset: false}
	e := New(Config{EventQueueSize: 16, PersistQueueSize: 4}, newFakeRuleStore(), nil)
	e.rulesMu.Lock()
	e.rules = []compiledRule{{Rule: domain.Rule{Name: "typed", Enabled: true, Priority: 10, When: domain.RuleWhen{Kind: strPtr("429")}, Then: domain.RuleThen{Throttle: th}}}}
	e.rulesMu.Unlock()
	sink := newFakeSink(10)
	e.SetHealthSink(sink)

	var calls atomic.Int32
	secondDone := make(chan struct{}, 1)
	e.SetPersistFunc(func(ctx context.Context, item PersistItem) error {
		n := calls.Add(1)
		if n == 1 {
			panic("injected panic for test")
		}
		select {
		case secondDone <- struct{}{}:
		default:
		}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, e.Start(ctx))
	t.Cleanup(func() {
		cancel()
		_ = e.Close(context.Background())
	})

	e.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(0)})
	e.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(1)})

	// Wait for second item to be processed after supervisor restart (deterministic barrier, no sleep)
	select {
	case <-secondDone:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "second persist callback not processed after panic restart")
	}
	// Pending must return to zero (both items released, first via panic defer)
	require.Eventually(t, func() bool { return e.PersistQueued() == 0 }, 2*time.Second, 5*time.Millisecond)
	require.Equal(t, int32(2), calls.Load())
	// No negative pending
	require.GreaterOrEqual(t, e.PersistQueued(), 0)
	// Ensure engine still functional (no crash)
	require.Equal(t, int64(2), e.MatchedActions())
}

func TestRulePersist_NeverNegativeUnderLoad(t *testing.T) {
	th := &domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: int64Ptr(1000), UseReset: false}
	e := New(Config{EventQueueSize: 64, PersistQueueSize: 8}, newFakeRuleStore(), nil)
	e.rulesMu.Lock()
	e.rules = []compiledRule{{Rule: domain.Rule{Name: "typed", Enabled: true, Priority: 10, When: domain.RuleWhen{Kind: strPtr("429")}, Then: domain.RuleThen{Throttle: th}}}}
	e.rulesMu.Unlock()
	sink := newFakeSink(64)
	e.SetHealthSink(sink)
	e.SetPersistFunc(func(ctx context.Context, item PersistItem) error { return nil })
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
	e := New(Config{EventQueueSize: 16, PersistQueueSize: 1}, newFakeRuleStore(), nil)
	e.rulesMu.Lock()
	e.rules = []compiledRule{{Rule: domain.Rule{Name: "typed", Enabled: true, Priority: 10, When: domain.RuleWhen{Kind: strPtr("429")}, Then: domain.RuleThen{Throttle: th}}}}
	e.rulesMu.Unlock()
	sink := newFakeSink(10)
	e.SetHealthSink(sink)
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

// helpers to access worker loopRestartDelay for test (exposed via small shim)
func workerLoopRestartDelay() time.Duration { return worker.LoopRestartDelayForTest() }
