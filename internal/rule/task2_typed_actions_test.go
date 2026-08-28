// SPDX-License-Identifier: AGPL-3.0-or-later
package rule

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

type fakeHealthSink struct {
	mu        sync.Mutex
	throttles []struct {
		ev domainThrottle
		th domain.ThrottleAction
	}
	fails []domainThrottle
	ch    chan struct{}
}

type domainThrottle struct {
	Event Event
}

func newFakeSink(buf int) *fakeHealthSink {
	return &fakeHealthSink{ch: make(chan struct{}, buf)}
}

func (f *fakeHealthSink) Throttle(ev Event, th domain.ThrottleAction) {
	f.mu.Lock()
	f.throttles = append(f.throttles, struct {
		ev domainThrottle
		th domain.ThrottleAction
	}{ev: domainThrottle{Event: ev}, th: th})
	f.mu.Unlock()
	select {
	case f.ch <- struct{}{}:
	default:
	}
}

func (f *fakeHealthSink) FailAccount(ev Event) error {
	f.mu.Lock()
	f.fails = append(f.fails, domainThrottle{Event: ev})
	f.mu.Unlock()
	select {
	case f.ch <- struct{}{}:
	default:
	}
	return nil
}

func (f *fakeHealthSink) countThrottle() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.throttles)
}
func (f *fakeHealthSink) countFail() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.fails)
}

// barrier helper: wait for n throttle signals with timeout.
func waitSignals(t *testing.T, ch chan struct{}, n int) {
	t.Helper()
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for i := 0; i < n; i++ {
		select {
		case <-ch:
		case <-timeout.C:
			require.FailNow(t, "timeout waiting for health sink signal")
		}
	}
}

// --- Validate malformed union and duration/use_reset truth table ---

func TestRuleThrottle_Validate_MalformedUnion(t *testing.T) {
	th := &domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: int64Ptr(1000), UseReset: false}
	cases := []struct {
		name string
		then domain.RuleThen
		ok   bool
	}{
		{"throttle+fail both", domain.RuleThen{Throttle: th, FailAccount: true}, false},
		{"throttle+legacy status", domain.RuleThen{Throttle: th, Status: statusPtr(domain.Status429)}, false},
		{"throttle+legacy cooldown", domain.RuleThen{Throttle: th, Cooldown: strPtr("30s")}, false},
		{"throttle+legacy weight", domain.RuleThen{Throttle: th, Weight: intPtr(10)}, false},
		{"fail+legacy status", domain.RuleThen{FailAccount: true, Status: statusPtr(domain.Status429)}, false},
		{"throttle retry_after valid", domain.RuleThen{Throttle: &domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeRetryAfter, UseReset: true}}, true},
		{"fail alone", domain.RuleThen{FailAccount: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateThen(tc.then)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestRuleThrottle_Validate_DurationUseResetConflicts(t *testing.T) {
	cases := []struct {
		name string
		th   domain.ThrottleAction
		ok   bool
	}{
		{"retry_after requires use_reset true", domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeRetryAfter, UseReset: false}, false},
		{"retry_after with use_reset true and nil duration ok fallback", domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeRetryAfter, UseReset: true}, true},
		{"retry_after with positive duration ok", domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeRetryAfter, DurationMs: int64Ptr(5000), UseReset: true}, true},
		{"retry_after with zero duration rejected", domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeRetryAfter, DurationMs: int64Ptr(0), UseReset: true}, false},
		{"retry_after with negative duration rejected", domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeRetryAfter, DurationMs: int64Ptr(-1), UseReset: true}, false},
		{"open requires use_reset false", domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: int64Ptr(1000), UseReset: true}, false},
		{"open requires duration>0 nil rejected", domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, UseReset: false}, false},
		{"open requires duration>0 zero rejected", domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: int64Ptr(0), UseReset: false}, false},
		{"open valid", domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: int64Ptr(30000), UseReset: false}, true},
		{"invalid scope", domain.ThrottleAction{Scope: "bad", Mode: domain.ThrottleModeOpen, DurationMs: int64Ptr(1000), UseReset: false}, false},
		{"invalid mode", domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: "bad", DurationMs: int64Ptr(1000), UseReset: false}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateThen(domain.RuleThen{Throttle: &tc.th})
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func int64Ptr(v int64) *int64 { return &v }

// --- Account route missing IDs ---

func TestRuleThrottle_AccountRouteMissingIDs(t *testing.T) {
	th := &domain.ThrottleAction{Scope: domain.ThrottleScopeAccountRoute, Mode: domain.ThrottleModeOpen, DurationMs: int64Ptr(5000), UseReset: false}
	e, _ := newTestEngine(t, domain.Rule{
		Name: "throttle-route", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("429")},
		Then: domain.RuleThen{Throttle: th},
	})
	sink := newFakeSink(10)
	e.SetHealthSink(sink)
	// missing RouteClassID
	e.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(0), RouteClassID: "", QualityClassID: "q1"})
	require.Equal(t, 0, sink.countThrottle(), "missing RouteClassID should not match")
	// missing QualityClassID
	e.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(1), RouteClassID: "r1", QualityClassID: ""})
	require.Equal(t, 0, sink.countThrottle(), "missing QualityClassID should not match")
	// both present matches
	e.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(2), RouteClassID: "r1", QualityClassID: "q1"})
	require.Equal(t, 1, sink.countThrottle())
	require.Equal(t, int64(1), e.MatchedActions())
}

func TestRuleThrottle_AccountWildcard(t *testing.T) {
	th := &domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeRetryAfter, UseReset: true, DurationMs: int64Ptr(2000)}
	e, _ := newTestEngine(t, domain.Rule{
		Name: "throttle-account", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("429")},
		Then: domain.RuleThen{Throttle: th},
	})
	sink := newFakeSink(10)
	e.SetHealthSink(sink)
	// account scope ignores route IDs (even if empty or present)
	e.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(0)})
	require.Equal(t, 1, sink.countThrottle())
	e.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(1), RouteClassID: "r1", QualityClassID: "q1"})
	require.Equal(t, 2, sink.countThrottle(), "account scope must ignore route and still match with IDs present")
}

// --- Window 429 threshold ---

func TestRuleWindow_ThrottleThresholdNotYetHit(t *testing.T) {
	th := &domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: int64Ptr(1000), UseReset: false}
	e, _ := newTestEngine(t, domain.Rule{
		Name: "window-throttle", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("429"), Count429GE: intPtr(3), WindowSeconds: intPtr(60)},
		Then: domain.RuleThen{Throttle: th},
	})
	sink := newFakeSink(10)
	e.SetHealthSink(sink)
	// 2 events below threshold -> no match, local sink not reached
	e.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(0)})
	e.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(1)})
	require.Equal(t, 0, sink.countThrottle())
	require.Equal(t, int64(0), e.MatchedActions())
	// 3rd hits threshold -> immediately reaches sink (channel barrier)
	e.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(2)})
	waitSignals(t, sink.ch, 1)
	require.Equal(t, 1, sink.countThrottle())
	require.Equal(t, int64(1), e.MatchedActions())
	// 4th still above threshold (window keeps counts) -> also hits
	e.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(3)})
	waitSignals(t, sink.ch, 1)
	require.Equal(t, 2, sink.countThrottle())
}

// --- Admission queue full (bounded admission channel) ---

func TestRuleQueueFull_AdmissionDropped(t *testing.T) {
	e := New(Config{EventQueueSize: 1, PersistQueueSize: 1}, newFakeRuleStore(), nil)
	// Fill admission queue
	e.Enqueue(Event{AccountID: 1, Kind: Kind429, OccurredAt: at(0)})
	require.Equal(t, int64(0), e.AdmissionDropped())
	// Second enqueue must be nonblocking and count dropped
	done := make(chan struct{})
	go func() {
		e.Enqueue(Event{AccountID: 1, Kind: Kind429, OccurredAt: at(1)})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "Enqueue blocked despite queue full (must be nonblocking)")
	}
	require.Equal(t, int64(1), e.AdmissionDropped())
}

// --- Persistence queue full / write failure / local before async ---

func TestRuleQueueFull_PersistDroppedAndLocalBeforePersist(t *testing.T) {
	th := &domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: int64Ptr(1000), UseReset: false}
	e, _ := newTestEngine(t, domain.Rule{
		Name: "persist-full", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("429")},
		Then: domain.RuleThen{Throttle: th},
	})
	// No Start, persistCh not drained -> bounded queue with cap 1024 initially, reduce to 1 for test
	e2 := New(Config{EventQueueSize: 16, PersistQueueSize: 1}, newFakeRuleStore(), nil)
	require.NoError(t, e2.Reload(context.Background()))
	// Manually create rule without using newTestEngine persist size
	// Instead create e with small persistCh by reinitializing
	// Use e (from newTestEngine) has cap 1024, so fill it via direct channel fill for deterministic full
	// We'll use e2 with injected rule
	_, _ = e, th // keep e for lint
	e2.rulesMu.Lock()
	e2.rules = []compiledRule{{Rule: domain.Rule{Name: "persist-full", Enabled: true, Priority: 10, When: domain.RuleWhen{Kind: strPtr("429")}, Then: domain.RuleThen{Throttle: th}}}}
	e2.rulesMu.Unlock()
	sink := newFakeSink(10)
	e2.SetHealthSink(sink)
	// First handle -> local sink immediate, persist enqueue succeeds
	e2.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(0)})
	require.Equal(t, 1, sink.countThrottle(), "local apply must happen before async persist")
	require.Equal(t, 1, e2.PersistQueued(), "first persist enqueued")
	require.Equal(t, int64(0), e2.PersistDropped())
	// Second handle -> local sink still immediate, but persist queue full -> dropped
	e2.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(1)})
	require.Equal(t, 2, sink.countThrottle(), "blocked persistence must not block second event local sink")
	require.Equal(t, int64(1), e2.PersistDropped())
	require.Equal(t, 1, e2.PersistQueued(), "queue stays at cap")
}

func TestRuleFailAccount_QueueFullWriteFailure(t *testing.T) {
	e := New(Config{EventQueueSize: 16, PersistQueueSize: 4}, newFakeRuleStore(), nil)
	// inject failing persist func
	e.SetPersistFunc(func(_ context.Context, item PersistItem) error { return fmt.Errorf("injected write failure") })
	// Need rule with FailAccount
	e.rulesMu.Lock()
	e.rules = []compiledRule{{Rule: domain.Rule{Name: "fail-acc", Enabled: true, Priority: 10, When: domain.RuleWhen{Kind: strPtr("5xx")}, Then: domain.RuleThen{FailAccount: true}}}}
	e.rulesMu.Unlock()
	sink := newFakeSink(10)
	e.SetHealthSink(sink)
	e.HandleEvent(context.Background(), Event{AccountID: 42, Kind: Kind5xx, OccurredAt: at(0), ExpectedRevision: 7})
	require.Equal(t, 1, sink.countFail(), "local FailAccount must be applied immediately")
	require.Equal(t, 1, e.PersistQueued())
	// Flush processes persist queue and counts failure
	e.Flush(context.Background())
	require.Equal(t, int64(1), e.PersistFailures())
	require.Equal(t, 0, e.PersistQueued(), "flushed")
	// Second failure increments again
	e.HandleEvent(context.Background(), Event{AccountID: 42, Kind: Kind5xx, OccurredAt: at(1), ExpectedRevision: 8})
	e.Flush(context.Background())
	require.Equal(t, int64(2), e.PersistFailures())
}

// --- Worker/request nonblocking (Enqueue and HandleEvent never block on persist full) ---

func TestRuleQueueFull_WorkerNonblocking(t *testing.T) {
	th := &domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: int64Ptr(1000), UseReset: false}
	e := New(Config{EventQueueSize: 2, PersistQueueSize: 1}, newFakeRuleStore(), nil)
	e.rulesMu.Lock()
	e.rules = []compiledRule{{Rule: domain.Rule{Name: "nb", Enabled: true, Priority: 10, When: domain.RuleWhen{Kind: strPtr("429")}, Then: domain.RuleThen{Throttle: th}}}}
	e.rulesMu.Unlock()
	sink := newFakeSink(10)
	e.SetHealthSink(sink)
	// Fill persist queue
	e.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(0)})
	require.Equal(t, 1, e.PersistQueued())
	// Next events must not block even though persist full — use barrier watchdog
	done := make(chan struct{})
	go func() {
		for i := 1; i < 5; i++ {
			e.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(i)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "HandleEvent blocked on persist full (must be nonblocking)")
	}
	require.Equal(t, 5, sink.countThrottle())
	require.Equal(t, int64(4), e.PersistDropped(), "4 additional persist enqueues dropped")
}

// --- Four metrics distinct ---

func TestRuleMetrics_FourDistinct(t *testing.T) {
	e := New(Config{EventQueueSize: 1, PersistQueueSize: 1}, newFakeRuleStore(), nil)
	e.SetPersistFunc(func(_ context.Context, item PersistItem) error { return fmt.Errorf("fail") })
	e.rulesMu.Lock()
	e.rules = []compiledRule{
		{Rule: domain.Rule{Name: "throttle", Enabled: true, Priority: 10, When: domain.RuleWhen{Kind: strPtr("429")}, Then: domain.RuleThen{Throttle: &domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: int64Ptr(1000), UseReset: false}}}},
	}
	e.rulesMu.Unlock()
	sink := newFakeSink(10)
	e.SetHealthSink(sink)
	// Admission drop (one slot filled, one dropped)
	e.Enqueue(Event{AccountID: 1, Kind: Kind429, OccurredAt: at(0)})
	e.Enqueue(Event{AccountID: 1, Kind: Kind429, OccurredAt: at(1)}) // dropped
	require.Equal(t, int64(1), e.AdmissionDropped())
	// Drain admission queue before measuring matched to keep metrics distinct
	e.Flush(context.Background())
	// After flush, the enqueued event was processed as a match -> matched 1, queue now empty
	// Reset matched for clear distinct check via fresh engine? Instead continue counting.
	// Clear counts for isolated check: create fresh engine for matched part
	e2 := New(Config{EventQueueSize: 16, PersistQueueSize: 1}, newFakeRuleStore(), nil)
	e2.SetPersistFunc(func(_ context.Context, item PersistItem) error { return fmt.Errorf("fail") })
	e2.rulesMu.Lock()
	e2.rules = []compiledRule{
		{Rule: domain.Rule{Name: "throttle", Enabled: true, Priority: 10, When: domain.RuleWhen{Kind: strPtr("429")}, Then: domain.RuleThen{Throttle: &domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: int64Ptr(1000), UseReset: false}}}},
	}
	e2.rulesMu.Unlock()
	sink2 := newFakeSink(10)
	e2.SetHealthSink(sink2)
	e2.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(2)})
	e2.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(3)}) // second persist dropped because queue cap 1
	require.Equal(t, int64(2), e2.MatchedActions())
	require.Equal(t, int64(1), e2.PersistDropped())
	e2.Flush(context.Background())
	require.Equal(t, int64(1), e2.PersistFailures(), "persist failures from injected func")
	// Verify four metrics are distinct and via Stats
	stats := e2.Stats().(RuleEngineStats)
	require.Equal(t, int64(2), stats.MatchedActions)
	require.Equal(t, int64(1), stats.PersistDropped)
	require.Equal(t, int64(1), stats.PersistFailures)
	// admission from first engine still distinct
	require.Equal(t, int64(1), e.AdmissionDropped())
}

// --- Response shaping unchanged ---

func TestRule_ResponseShapingUnchanged(t *testing.T) {
	e, _ := newTestEngine(t, domain.Rule{
		Name: "shape", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("5xx")},
		Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy), ResponseCode: intPtr(502), CustomMessage: strPtr("Upstream request failed")},
	})
	ev := Event{AccountID: 1, Kind: Kind5xx, HTTPStatus: intPtr(500), ErrorMessage: "boom"}
	then, punish := e.Classify(ev)
	require.True(t, punish)
	require.NotNil(t, then.ResponseCode)
	require.Equal(t, 502, *then.ResponseCode)
	require.Equal(t, "Upstream request failed", *then.CustomMessage)
	msg, ok := UnifiedMessage(then, "boom")
	require.True(t, ok)
	require.Equal(t, "Upstream request failed", msg)
	// typed throttle with shaping
	th := &domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: int64Ptr(1000), UseReset: false}
	e2, _ := newTestEngine(t, domain.Rule{
		Name: "typed-shape", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("429")},
		Then: domain.RuleThen{Throttle: th, ResponseCode: intPtr(429), CustomMessage: strPtr("rate limited")},
	})
	then2, punish2 := e2.Classify(Event{AccountID: 1, Kind: Kind429, HTTPStatus: intPtr(429)})
	require.True(t, punish2)
	require.NotNil(t, then2.Throttle)
	require.Equal(t, 429, *then2.ResponseCode)
	require.Equal(t, "rate limited", *then2.CustomMessage)
}

// --- FailAccount typed ---

func TestRuleFailAccount_BasicAndExpectedRevision(t *testing.T) {
	e := New(Config{EventQueueSize: 16, PersistQueueSize: 16}, newFakeRuleStore(), nil)
	e.rulesMu.Lock()
	e.rules = []compiledRule{{Rule: domain.Rule{Name: "fail", Enabled: true, Priority: 10, When: domain.RuleWhen{Kind: strPtr("5xx")}, Then: domain.RuleThen{FailAccount: true}}}}
	e.rulesMu.Unlock()
	sink := newFakeSink(10)
	e.SetHealthSink(sink)
	ev := Event{AccountID: 99, Kind: Kind5xx, OccurredAt: at(0), ExpectedRevision: 42}
	e.HandleEvent(context.Background(), ev)
	require.Equal(t, 1, sink.countFail())
	require.Equal(t, int64(1), e.MatchedActions())
	// verify sink received expected revision via event copy
	sink.mu.Lock()
	require.Equal(t, int64(42), sink.fails[0].Event.ExpectedRevision)
	sink.mu.Unlock()
}
