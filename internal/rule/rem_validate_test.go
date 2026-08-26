// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package rule

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestValidateWhen_EmptyInRejected(t *testing.T) {
	cases := []struct {
		name string
		w    domain.RuleWhen
		msg  string
	}{
		{"http_status_in empty", domain.RuleWhen{HTTPStatusIn: []int{}}, "http_status_in must not be empty"},
		{"model_in empty", domain.RuleWhen{ModelIn: []string{}}, "model_in must not be empty"},
		{"error_message_contains_in empty", domain.RuleWhen{ErrorMessageContainsIn: []string{}}, "error_message_contains_in must not be empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateWhen(tc.w)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.msg)
		})
	}
}

func TestValidateWhen_NilInPasses(t *testing.T) {
	// nil means dimension not filtered → valid
	require.NoError(t, ValidateWhen(domain.RuleWhen{}))
	require.NoError(t, ValidateWhen(domain.RuleWhen{Kind: strPtr("4xx")}))
}

func TestValidateWhen_EmptyIn_JSONDistinguishesNilVsEmpty(t *testing.T) {
	// Standard encoding/json distinguishes nil vs [] — rely on it.
	// Present [] → non-nil empty → rejected; absent → nil → passes.
	var w1 domain.RuleWhen
	require.NoError(t, json.Unmarshal([]byte(`{"http_status_in":[]}`), &w1))
	require.NotNil(t, w1.HTTPStatusIn)
	require.Error(t, ValidateWhen(w1))
	require.Contains(t, ValidateWhen(w1).Error(), "http_status_in must not be empty")

	var w2 domain.RuleWhen
	require.NoError(t, json.Unmarshal([]byte(`{}`), &w2))
	require.Nil(t, w2.HTTPStatusIn)
	require.NoError(t, ValidateWhen(w2))

	var w3 domain.RuleWhen
	require.NoError(t, json.Unmarshal([]byte(`{"model_in":[]}`), &w3))
	require.NotNil(t, w3.ModelIn)
	require.Error(t, ValidateWhen(w3))

	var w4 domain.RuleWhen
	require.NoError(t, json.Unmarshal([]byte(`{"error_message_contains_in":[]}`), &w4))
	require.NotNil(t, w4.ErrorMessageContainsIn)
	require.Error(t, ValidateWhen(w4))
}

func TestValidateThen_PurePassthroughValid(t *testing.T) {
	require.NoError(t, ValidateThen(domain.RuleThen{}))
}

func TestValidateThen_PurePassthroughJSONRoundTrip(t *testing.T) {
	// Simulate service/handler JSON path: map → json.Marshal → json.Decode with DisallowUnknownFields
	// All-empty Then{} round-trips unchanged.
	raw := map[string]any{}
	b, err := json.Marshal(raw)
	require.NoError(t, err)
	var dec domain.RuleThen
	// service uses decodeStrict which is Marshal(map)+DisallowUnknownFields Decode;
	// for Then{} empty, round-trip must stay empty and valid.
	require.NoError(t, json.Unmarshal(b, &dec))
	require.Nil(t, dec.Status)
	require.Nil(t, dec.Cooldown)
	require.Nil(t, dec.Weight)
	require.Nil(t, dec.ResponseCode)
	require.Nil(t, dec.CustomMessage)
	require.NoError(t, ValidateThen(dec))

	// Also via JSON object {} for Then
	var dec2 domain.RuleThen
	require.NoError(t, json.Unmarshal([]byte(`{}`), &dec2))
	require.NoError(t, ValidateThen(dec2))

	// Verify Marshal of empty Then is {} (omitempty) and survives
	marshaled, err := json.Marshal(domain.RuleThen{})
	require.NoError(t, err)
	require.Equal(t, "{}", string(marshaled))

	// Full domain.Rule round-trip with Then{} empty
	rule := domain.Rule{Name: "passthrough", Enabled: true, Priority: 10, When: domain.RuleWhen{Kind: strPtr("4xx"), HTTPStatus: intPtr(400)}, Then: domain.RuleThen{}}
	b2, err := json.Marshal(rule)
	require.NoError(t, err)
	var rule2 domain.Rule
	require.NoError(t, json.Unmarshal(b2, &rule2))
	require.Nil(t, rule2.Then.Status)
	require.Nil(t, rule2.Then.ResponseCode)
	require.Nil(t, rule2.Then.CustomMessage)
	require.NoError(t, ValidateThen(rule2.Then))
}

func TestClassify_PurePassthrough_EquivalenceWithSeed(t *testing.T) {
	// User rule Then{} must behave identically to seed-4xx-400: empty actions + punish=false
	e, _ := newTestEngine(t, domain.Rule{
		Name: "user-passthrough", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("4xx"), HTTPStatus: intPtr(400)},
		Then: domain.RuleThen{},
	})
	ev := Event{AccountID: 1, Kind: Kind4xx, HTTPStatus: intPtr(400), ErrorMessage: "bad request"}
	then, punish := e.Classify(ev)
	require.Nil(t, then.Status)
	require.Nil(t, then.Cooldown)
	require.Nil(t, then.Weight)
	require.Nil(t, then.ResponseCode)
	require.Nil(t, then.CustomMessage)
	require.False(t, punish)

	// Seed engine for equivalence
	seedEngine, _ := newTestEngine(t)
	thenSeed, punishSeed := seedEngine.Classify(ev)
	require.Nil(t, thenSeed.Status)
	require.Nil(t, thenSeed.Cooldown)
	require.Nil(t, thenSeed.Weight)
	require.Nil(t, thenSeed.ResponseCode)
	require.Nil(t, thenSeed.CustomMessage)
	require.False(t, punishSeed)

	// Assert equivalence
	require.Equal(t, then, thenSeed)
	require.Equal(t, punish, punishSeed)

	// Non-matching event should fallback to default 502, not passthrough
	evOther := Event{AccountID: 1, Kind: Kind4xx, HTTPStatus: intPtr(403), ErrorMessage: "forbidden"}
	thenOther, punishOther := e.Classify(evOther)
	require.NotNil(t, thenOther.ResponseCode)
	require.Equal(t, 502, *thenOther.ResponseCode)
	require.False(t, punishOther)
}

func TestClassify_PurePassthrough_HandleEventNoPunish(t *testing.T) {
	e, _ := newTestEngine(t, domain.Rule{
		Name: "user-passthrough", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("4xx"), HTTPStatus: intPtr(400)},
		Then: domain.RuleThen{},
	})
	var rec recorder
	e.SetApply(rec.fn)
	ev := Event{AccountID: 1, Kind: Kind4xx, HTTPStatus: intPtr(400), OccurredAt: at(0), ErrorMessage: "bad"}
	e.HandleEvent(nil, ev)
	// Then{} has no status/cooldown/weight → Apply returns nils; punish=false in proxy path.
	app := rec.get()
	require.Len(t, app, 1)
	require.Nil(t, app[0].status)
	require.Nil(t, app[0].cooldown)
	require.Nil(t, app[0].weight)
}
