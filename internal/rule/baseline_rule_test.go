// SPDX-License-Identifier: AGPL-3.0-or-later
package rule

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestRuleBaseline characterizes enqueue/window/first-match/response-shaping behavior
// on typed Throttle/FailAccount actions.
func TestRuleBaseline(t *testing.T) {
	// Enqueue bounded channel still best-effort admission (dropped when full)
	e := New(Config{EventQueueSize: 1}, newFakeRuleStore(), nil, nil, nil)
	e.Enqueue(Event{AccountID: 1, Kind: Kind429, OccurredAt: at(0)})
	e.Enqueue(Event{AccountID: 1, Kind: Kind429, OccurredAt: at(1)})
	require.Equal(t, int64(1), e.AdmissionDropped())
	require.Equal(t, 1, len(e.ch))

	// Window first-match: two rules same kind, priority decides
	sink2 := newFakeSink(10)
	e2, _ := newTestEngineWithSink(t, sink2, nil,
		domain.Rule{Name: "p10", Enabled: true, Priority: 10, When: domain.RuleWhen{Kind: strPtr("5xx")}, Then: domain.RuleThen{Throttle: openThrottleMs(5000)}},
		domain.Rule{Name: "p20", Enabled: true, Priority: 20, When: domain.RuleWhen{Kind: strPtr("5xx")}, Then: domain.RuleThen{Throttle: openThrottleMs(30000)}},
	)
	e2.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind5xx, OccurredAt: at(0)})
	require.Equal(t, 1, sink2.countThrottle(), "首中即停，只执行一次")
	require.Equal(t, int64(5000), *sink2.lastThrottle().DurationMs, "priority 低者先命中")

	// Window threshold: count 429 >=2 in 60s
	sink3 := newFakeSink(10)
	e3, _ := newTestEngineWithSink(t, sink3, nil, domain.Rule{
		Name: "win", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("429"), Count429GE: intPtr(2), WindowSeconds: intPtr(60)},
		Then: domain.RuleThen{Throttle: openThrottle()},
	})
	e3.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(0)})
	require.Equal(t, 0, sink3.countThrottle(), "below threshold should not apply")
	e3.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(1)})
	require.Equal(t, 1, sink3.countThrottle(), "threshold hit should apply")

	// Response shaping unchanged: passthrough vs custom via Classify (punish false for pure shaping)
	e4, _ := newTestEngine(t, domain.Rule{
		Name: "shape", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("4xx"), HTTPStatus: intPtr(400)},
		Then: domain.RuleThen{ResponseCode: intPtr(502), CustomMessage: strPtr("Upstream request failed")},
	})
	then, punish := e4.Classify(Event{AccountID: 1, Kind: Kind4xx, HTTPStatus: intPtr(400), ErrorMessage: "x"})
	require.False(t, punish, "pure shaping rule has no punish")
	require.Equal(t, 502, *then.ResponseCode)
	require.Equal(t, "Upstream request failed", *then.CustomMessage)
	msg, ok := UnifiedMessage(then, "x")
	require.True(t, ok)
	require.Equal(t, "Upstream request failed", msg)

	// Raw outcome Enqueue remains same channel semantics (time window decay still holds)
	var wm windowMap
	wm.reset(30*time.Second, true)
	wm.Add(Event{AccountID: 1, Kind: Kind5xx, OccurredAt: at(0)})
	require.Equal(t, 1, wm.Snapshot(1, 30, at(0)).failure)
}
