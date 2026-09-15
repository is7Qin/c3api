// SPDX-License-Identifier: AGPL-3.0-or-later
package rule

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// Defect 1: matched_actions only for typed Throttle/FailAccount accepted.
func TestRuleMatched_TypedOnly(t *testing.T) {
	// Shaping-only (ResponseCode/CustomMessage) must not count.
	e2, _ := newTestEngine(t, domain.Rule{
		Name: "shaping", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("4xx"), HTTPStatus: intPtr(400)},
		Then: domain.RuleThen{ResponseCode: intPtr(502), CustomMessage: strPtr("Upstream request failed")},
	})
	e2.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind4xx, HTTPStatus: intPtr(400), OccurredAt: at(0)})
	require.Equal(t, int64(0), e2.MatchedActions(), "shaping-only must not count")

	// Typed Throttle should count exactly once when accepted.
	th := &domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: int64Ptr(1000), UseReset: false}
	e3, _ := newTestEngine(t, domain.Rule{
		Name: "typed", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("429")},
		Then: domain.RuleThen{Throttle: th},
	})
	sink := newFakeSink(10)
	e3.SetHealthSink(sink)
	e3.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(0)})
	require.Equal(t, int64(1), e3.MatchedActions())
	// Second typed hit increments again
	e3.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(1)})
	require.Equal(t, int64(2), e3.MatchedActions())

	// Typed FailAccount also counts
	e4 := New(Config{EventQueueSize: 16, PersistQueueSize: 16}, newFakeRuleStore(), nil)
	e4.rulesMu.Lock()
	e4.rules = []compiledRule{{Rule: domain.Rule{Name: "fail", Enabled: true, Priority: 10, When: domain.RuleWhen{Kind: strPtr("5xx")}, Then: domain.RuleThen{FailAccount: true}}}}
	e4.rulesMu.Unlock()
	sink4 := newFakeSink(10)
	e4.SetHealthSink(sink4)
	e4.HandleEvent(context.Background(), Event{AccountID: 99, Kind: Kind5xx, OccurredAt: at(0)})
	require.Equal(t, int64(1), e4.MatchedActions())

	// Account_route missing IDs => not matched, so not counted
	thRoute := &domain.ThrottleAction{Scope: domain.ThrottleScopeAccountRoute, Mode: domain.ThrottleModeOpen, DurationMs: int64Ptr(1000), UseReset: false}
	e5, _ := newTestEngine(t, domain.Rule{
		Name: "route", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("429")},
		Then: domain.RuleThen{Throttle: thRoute},
	})
	sink5 := newFakeSink(10)
	e5.SetHealthSink(sink5)
	e5.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(0), RouteClassID: "", QualityClassID: "q1"})
	require.Equal(t, int64(0), e5.MatchedActions(), "missing IDs => not matched => not counted")
}

// Defect 2 & 3: context-aware persist + pending + Close join.
func TestRulePersist_PendingAndJoin(t *testing.T) {
	th := &domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: int64Ptr(1000), UseReset: false}
	e := New(Config{EventQueueSize: 16, PersistQueueSize: 4}, newFakeRuleStore(), nil)
	e.rulesMu.Lock()
	e.rules = []compiledRule{
		{Rule: domain.Rule{Name: "typed", Enabled: true, Priority: 10, When: domain.RuleWhen{Kind: strPtr("429")}, Then: domain.RuleThen{Throttle: th}}},
	}
	e.rulesMu.Unlock()
	sink := newFakeSink(10)
	e.SetHealthSink(sink)

	started := make(chan struct{}, 1)
	unblock := make(chan struct{})
	e.SetPersistFunc(func(ctx context.Context, item PersistItem) error {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-unblock:
			return nil
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, e.Start(ctx))

	// Trigger typed action -> local sink immediate, persist enqueued and dequeued into in-flight
	e.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(0)})
	// Wait for callback to have started (deterministic barrier)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "persist callback did not start")
	}
	// While callback blocks, pending must remain 1 (queued+inflight), even though channel is empty.
	require.Equal(t, 1, e.PersistQueued(), "pending must include in-flight")
	require.Equal(t, 0, e.PersistQueuedChannelLen(), "channel empty while in-flight, len would lie")

	// Cancel via Close path: Close should cancel persistCtx and wait for callback.
	// Instead of canceling parent ctx directly, test Close's joining.
	// First, cancel the parent to also signal? But we want Close to drive cancellation.
	// We'll call cancel for parent and then Close; Close will cancel its own child and wait.
	cancel()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	// Unblock callback concurrently with Close wait: close will wait for callback, callback needs unblock or ctx.
	// 确定性汇合：callback 已在 started 屏障后阻塞（上文已等到启动信号），
	// Close 在独立 goroutine 走 join 路径；任一唤醒路径（unblock 放行 /
	// Close Ctx 取消）事后断言一致，直接放行，无需睡眠排序。
	closeErr := make(chan error, 1)
	startClose := time.Now()
	go func() { closeErr <- e.Close(closeCtx) }()
	close(unblock)
	require.NoError(t, <-closeErr)
	elapsed := time.Since(startClose)
	require.Less(t, elapsed, 2*time.Second, "Close must join promptly")

	require.Equal(t, 0, e.PersistQueued(), "pending becomes 0 after callback finishes")
	require.Equal(t, int64(0), e.PersistFailures(), "cancelled callback should not count as failure")
	// Ensure no goroutine remains: second Close is idempotent and pending stays 0.
	require.NoError(t, e.Close(context.Background()))
	require.Equal(t, 0, e.PersistQueued())
}

// Additional: Close without Start must not hang and pending drains synchronously.
func TestRulePersist_FlushWithoutStartPending(t *testing.T) {
	e := New(Config{EventQueueSize: 16, PersistQueueSize: 4}, newFakeRuleStore(), nil)
	e.rulesMu.Lock()
	e.rules = []compiledRule{
		{Rule: domain.Rule{Name: "typed", Enabled: true, Priority: 10, When: domain.RuleWhen{Kind: strPtr("429")}, Then: domain.RuleThen{Throttle: &domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: int64Ptr(1000), UseReset: false}}}},
	}
	e.rulesMu.Unlock()
	sink := newFakeSink(10)
	e.SetHealthSink(sink)
	e.SetPersistFunc(func(_ context.Context, _ PersistItem) error { return nil })
	e.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(0)})
	require.Equal(t, 1, e.PersistQueued())
	require.Equal(t, 1, e.PersistQueuedChannelLen())
	require.NoError(t, e.Close(context.Background()))
	require.Equal(t, 0, e.PersistQueued())
}
