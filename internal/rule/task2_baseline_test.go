// SPDX-License-Identifier: AGPL-3.0-or-later
package rule

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestRuleBaseline_Task2 characterizes existing enqueue/window/first-match/response-shaping behavior
// before typed Throttle/FailAccount. This is the passing baseline referenced in MUST DO 1.
func TestRuleBaseline_Task2(t *testing.T) {
	// Enqueue bounded channel still best-effort admission (dropped when full)
	e := New(Config{EventQueueSize: 1}, newFakeRuleStore(), nil)
	e.Enqueue(Event{AccountID: 1, Kind: Kind429, OccurredAt: at(0)})
	e.Enqueue(Event{AccountID: 1, Kind: Kind429, OccurredAt: at(1)})
	require.Equal(t, int64(1), e.AdmissionDropped())
	require.Equal(t, 1, len(e.ch))

	// Window first-match: two rules same kind, priority decides
	e2, _ := newTestEngine(t,
		domain.Rule{Name: "p10", Enabled: true, Priority: 10, When: domain.RuleWhen{Kind: strPtr("5xx")}, Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy), Cooldown: strPtr("5s")}},
		domain.Rule{Name: "p20", Enabled: true, Priority: 20, When: domain.RuleWhen{Kind: strPtr("5xx")}, Then: domain.RuleThen{Status: statusPtr(domain.Status429), Cooldown: strPtr("30s")}},
	)
	var rec recorder
	e2.SetApply(rec.fn)
	e2.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind5xx, OccurredAt: at(0)})
	got := rec.get()
	require.Len(t, got, 1)
	require.Equal(t, domain.StatusUnhealthy, *got[0].status)
	require.Equal(t, at(5), *got[0].cooldown)

	// Window threshold: count 429 >=2 in 60s
	e3, _ := newTestEngine(t, domain.Rule{
		Name: "win", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("429"), Count429GE: intPtr(2), WindowSeconds: intPtr(60)},
		Then: domain.RuleThen{Status: statusPtr(domain.Status429), Cooldown: strPtr("30s")},
	})
	var rec3 recorder
	e3.SetApply(rec3.fn)
	e3.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(0)})
	require.Empty(t, rec3.get(), "below threshold should not apply")
	e3.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind429, OccurredAt: at(1)})
	require.Len(t, rec3.get(), 1, "threshold hit should apply")

	// Response shaping unchanged: passthrough vs custom via Classify (punish false for pure shaping per legacy)
	e4, _ := newTestEngine(t, domain.Rule{
		Name: "shape", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("4xx"), HTTPStatus: intPtr(400)},
		Then: domain.RuleThen{ResponseCode: intPtr(502), CustomMessage: strPtr("Upstream request failed")},
	})
	then, punish := e4.Classify(Event{AccountID: 1, Kind: Kind4xx, HTTPStatus: intPtr(400), ErrorMessage: "x"})
	require.False(t, punish, "pure shaping rule has no punish (legacy semantics)")
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
