// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
package rule

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func strPtrP(s string) *string { return &s }
func intPtrP(v int) *int { return &v }

func TestPassthrough429CodeBodySplit(t *testing.T) {
	e, _ := newTestEngine(t)
	ev := Event{AccountID: 1, Kind: Kind429, HTTPStatus: intPtrP(429), ErrorMessage: "upstream 429 detail"}
	then, _ := e.Classify(ev)
	require.Nil(t, then.ResponseCode, "429 码透")
	require.NotNil(t, then.CustomMessage)
	require.Equal(t, "rate limited", *then.CustomMessage, "429 文不透")
}

func TestPassthrough400Full(t *testing.T) {
	e, _ := newTestEngine(t)
	ev := Event{AccountID: 1, Kind: Kind4xx, HTTPStatus: intPtrP(400), ErrorMessage: "bad request"}
	then, _ := e.Classify(ev)
	require.Nil(t, then.ResponseCode)
	require.Nil(t, then.CustomMessage, "400 全透")
}

func TestPassthrough5xxNormalized(t *testing.T) {
	e, _ := newTestEngine(t)
	ev := Event{AccountID: 1, Kind: Kind5xx, HTTPStatus: intPtrP(500), ErrorMessage: "boom"}
	then, _ := e.Classify(ev)
	require.NotNil(t, then.ResponseCode)
	require.Equal(t, 502, *then.ResponseCode)
	require.NotNil(t, then.CustomMessage)
	require.Equal(t, "Upstream request failed", *then.CustomMessage)
}

func TestPassthroughCustomCode(t *testing.T) {
	e, _ := newTestEngine(t, domain.Rule{Name: "custom-403", Enabled: true, Priority: 10, When: domain.RuleWhen{Kind: strPtrP("4xx"), HTTPStatus: intPtrP(403)}, Then: domain.RuleThen{ResponseCode: intPtrP(404), CustomMessage: strPtrP("custom msg")}})
	ev := Event{AccountID: 1, Kind: Kind4xx, HTTPStatus: intPtrP(403), ErrorMessage: "original"}
	then, _ := e.Classify(ev)
	require.NotNil(t, then.ResponseCode)
	require.Equal(t, 404, *then.ResponseCode)
	require.Equal(t, "custom msg", *then.CustomMessage)
}

func TestPassthroughHeaderWithCodePassthrough(t *testing.T) {
	// 覆写码时不透头（ResponseCode != nil 已覆写，头不透）
	e, _ := newTestEngine(t, domain.Rule{Name: "overwrite-429", Enabled: true, Priority: 10, When: domain.RuleWhen{Kind: strPtrP("429")}, Then: domain.RuleThen{ResponseCode: intPtrP(502), CustomMessage: strPtrP("Upstream request failed")}})
	ev := Event{AccountID: 1, Kind: Kind429, HTTPStatus: intPtrP(429), ErrorMessage: "rate limited"}
	then, _ := e.Classify(ev)
	require.NotNil(t, then.ResponseCode)
	require.Equal(t, 502, *then.ResponseCode, "覆写码")
	// 透码时透头（ResponseCode nil）
	e2, _ := newTestEngine(t)
	ev2 := Event{AccountID: 1, Kind: Kind429, HTTPStatus: intPtrP(429), ErrorMessage: "rate limited"}
	then2, _ := e2.Classify(ev2)
	require.Nil(t, then2.ResponseCode, "透码")
}

func TestPassthroughWindowRulePassthroughDirect(t *testing.T) {
	ws := 60
	cnt := 1
	e, _ := newTestEngine(t, domain.Rule{Name: "window-400", Enabled: true, Priority: 10, When: domain.RuleWhen{Kind: strPtrP("4xx"), HTTPStatus: intPtrP(400), WindowSeconds: &ws, CountFailureGE: &cnt}, Then: domain.RuleThen{}})
	ev := Event{AccountID: 1, Kind: Kind4xx, HTTPStatus: intPtrP(400), ErrorMessage: "window bad"}
	then, _ := e.Classify(ev)
	require.Nil(t, then.ResponseCode)
	require.Nil(t, then.CustomMessage, "窗口规则全透")
}
