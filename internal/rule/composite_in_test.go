// SPDX-License-Identifier: AGPL-3.0-or-later
package rule

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// ---- Validate 类 ----

func TestValidateWhenCompositeMutualExclusive(t *testing.T) {
	cases := []struct {
		name string
		w    domain.RuleWhen
	}{
		{"http_status exclusive", domain.RuleWhen{HTTPStatus: intPtr(500), HTTPStatusIn: []int{500}}},
		{"model exclusive", domain.RuleWhen{Model: strPtr("m1"), ModelIn: []string{"m1"}}},
		{"contains exclusive", domain.RuleWhen{ErrorMessageContains: strPtr("boom"), ErrorMessageContainsIn: []string{"boom"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateWhen(tc.w)
			require.Error(t, err)
		})
	}
}

func TestValidateWhenCompositeInNonEmpty(t *testing.T) {
	cases := []struct {
		name string
		w    domain.RuleWhen
	}{
		{"model_in empty string", domain.RuleWhen{ModelIn: []string{""}}},
		{"model_in one empty among valid", domain.RuleWhen{ModelIn: []string{"a", ""}}},
		{"error_contains_in empty", domain.RuleWhen{ErrorMessageContainsIn: []string{""}}},
		{"http_status_in zero out of range", domain.RuleWhen{HTTPStatusIn: []int{0}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateWhen(tc.w)
			require.Error(t, err)
		})
	}
}

func TestValidateWhenCompositeDuplicate(t *testing.T) {
	cases := []struct {
		name string
		w    domain.RuleWhen
	}{
		{"http_status_in dup", domain.RuleWhen{HTTPStatusIn: []int{500, 500}}},
		{"model_in dup", domain.RuleWhen{ModelIn: []string{"a", "a"}}},
		{"contains_in dup", domain.RuleWhen{ErrorMessageContainsIn: []string{"x", "x"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateWhen(tc.w)
			require.Error(t, err)
			require.Contains(t, err.Error(), "duplicate")
		})
	}
}

func TestValidateWhenCompositeOutOfRange(t *testing.T) {
	cases := []struct {
		name string
		w    domain.RuleWhen
	}{
		{"99", domain.RuleWhen{HTTPStatusIn: []int{99}}},
		{"600", domain.RuleWhen{HTTPStatusIn: []int{600}}},
		{"0", domain.RuleWhen{HTTPStatusIn: []int{0}}},
		{"399", domain.RuleWhen{HTTPStatusIn: []int{399}}},
		{"mixed valid+invalid", domain.RuleWhen{HTTPStatusIn: []int{500, 99}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateWhen(tc.w)
			require.Error(t, err)
			require.Contains(t, err.Error(), "between 400 and 599")
		})
	}
}

func TestValidateWhen_KindOK_Rejects_ContainsIn(t *testing.T) {
	w := domain.RuleWhen{Kind: strPtr("ok"), ErrorMessageContainsIn: []string{"overload"}}
	err := ValidateWhen(w)
	require.Error(t, err)
	require.Contains(t, err.Error(), "incompatible")
}

// ---- Engine / Classify 类 ----

func TestClassifyModelIn(t *testing.T) {
	e := mustEngineWithRules(t,
		domain.Rule{Name: "r1", Enabled: true, Priority: 10, When: domain.RuleWhen{ModelIn: []string{"gpt-5-0611", "gpt-4o"}}, Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy)}},
	)
	// hit
	evHit := Event{Kind: Kind5xx, Model: "gpt-5-0611"}
	then, punish := e.Classify(evHit)
	require.NotNil(t, then.Status)
	require.True(t, punish)
	// hit second element
	evHit2 := Event{Kind: Kind5xx, Model: "gpt-4o"}
	then2, _ := e.Classify(evHit2)
	require.NotNil(t, then2.Status)
	// miss
	evMiss := Event{Kind: Kind5xx, Model: "gpt-5"}
	thenMiss, punishMiss := e.Classify(evMiss)
	// no rule hit -> fallback 502 for non-ok kind
	require.NotNil(t, thenMiss.ResponseCode)
	require.Equal(t, 502, *thenMiss.ResponseCode)
	require.False(t, punishMiss)
}

func TestClassifyHTTPStatusIn_NilEvent(t *testing.T) {
	e := mustEngineWithRules(t,
		domain.Rule{Name: "r1", Enabled: true, Priority: 10, When: domain.RuleWhen{HTTPStatusIn: []int{500}}, Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy)}},
	)
	// ev.HTTPStatus == nil must not panic and must not match
	ev := Event{Kind: Kind5xx, HTTPStatus: nil}
	require.NotPanics(t, func() {
		then, punish := e.Classify(ev)
		// fallback
		require.Equal(t, 502, *then.ResponseCode)
		require.False(t, punish)
	})
	// also via Match directly
	w := domain.RuleWhen{HTTPStatusIn: []int{500}}
	require.False(t, Match(w, ev, windowSnapshot{}))
}

func TestClassifyHTTPStatusIn(t *testing.T) {
	e := mustEngineWithRules(t,
		domain.Rule{Name: "r1", Enabled: true, Priority: 10, When: domain.RuleWhen{HTTPStatusIn: []int{500, 502}}, Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy)}},
	)
	for _, code := range []int{500, 502} {
		c := code
		ev := Event{Kind: Kind5xx, HTTPStatus: &c}
		then, _ := e.Classify(ev)
		require.NotNil(t, then.Status, "code %d should hit", c)
	}
	c503 := 503
	evMiss := Event{Kind: Kind5xx, HTTPStatus: &c503}
	thenMiss, _ := e.Classify(evMiss)
	require.NotNil(t, thenMiss.ResponseCode)
	require.Equal(t, 502, *thenMiss.ResponseCode)
}

func TestClassifyErrorContainsIn(t *testing.T) {
	e := mustEngineWithRules(t,
		domain.Rule{Name: "r1", Enabled: true, Priority: 10, When: domain.RuleWhen{ErrorMessageContainsIn: []string{"overload", "busy"}}, Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy)}},
	)
	evHit := Event{Kind: Kind5xx, ErrorMessage: "system overload now"}
	then, _ := e.Classify(evHit)
	require.NotNil(t, then.Status)
	evHit2 := Event{Kind: Kind5xx, ErrorMessage: "server busy please retry"}
	then2, _ := e.Classify(evHit2)
	require.NotNil(t, then2.Status)
	evMiss := Event{Kind: Kind5xx, ErrorMessage: "timeout"}
	thenMiss, _ := e.Classify(evMiss)
	require.Equal(t, 502, *thenMiss.ResponseCode)
}

func TestModelIn_ThreeFaces(t *testing.T) {
	// 模拟 mapping: sel.Model = mapped model, ev.Model 恒为最终模型
	// 三面一致：响应/状态/日志均用最终模型判定
	mapped := "gpt-4o"
	reqModel := "gpt-4"
	e := mustEngineWithRules(t,
		domain.Rule{Name: "r1", Enabled: true, Priority: 10, When: domain.RuleWhen{ModelIn: []string{mapped}}, Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy), ResponseCode: intPtr(502), CustomMessage: strPtr("hit")}},
	)
	// final model = mapped => hit
	evFinal := Event{Kind: Kind5xx, Model: mapped}
	then, _ := e.Classify(evFinal)
	require.NotNil(t, then.Status)
	require.Equal(t, "hit", *then.CustomMessage)
	// reqModel not hit when final differs
	evReq := Event{Kind: Kind5xx, Model: reqModel}
	then2, _ := e.Classify(evReq)
	require.Equal(t, 502, *then2.ResponseCode)
	require.Equal(t, "upstream rejected request", *then2.CustomMessage)
	// also verify Match directly uses ev.Model (final)
	require.True(t, Match(domain.RuleWhen{ModelIn: []string{mapped}}, evFinal, windowSnapshot{}))
	require.False(t, Match(domain.RuleWhen{ModelIn: []string{mapped}}, evReq, windowSnapshot{}))
}

func TestClassifyCompositeAndAcrossFields(t *testing.T) {
	e := mustEngineWithRules(t,
		domain.Rule{
			Name: "r1", Enabled: true, Priority: 10,
			When: domain.RuleWhen{
				Kind: strPtr("5xx"), HTTPStatusIn: []int{500, 502}, ErrorMessageContainsIn: []string{"overload"},
			},
			Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy)},
		},
	)
	http500 := 500
	// all three dimensions hit -> match
	evHit := Event{Kind: Kind5xx, HTTPStatus: &http500, ErrorMessage: "overload"}
	then, _ := e.Classify(evHit)
	require.NotNil(t, then.Status)
	// kind mismatch -> miss
	evKindMiss := Event{Kind: Kind4xx, HTTPStatus: &http500, ErrorMessage: "overload"}
	then2, _ := e.Classify(evKindMiss)
	require.Equal(t, 502, *then2.ResponseCode)
	// http_status miss -> miss
	http503 := 503
	evStatusMiss := Event{Kind: Kind5xx, HTTPStatus: &http503, ErrorMessage: "overload"}
	then3, _ := e.Classify(evStatusMiss)
	require.Equal(t, 502, *then3.ResponseCode)
	// message miss -> miss
	evMsgMiss := Event{Kind: Kind5xx, HTTPStatus: &http500, ErrorMessage: "timeout"}
	then4, _ := e.Classify(evMsgMiss)
	require.Equal(t, 502, *then4.ResponseCode)
}

func TestClassifyCompositePriority(t *testing.T) {
	e := mustEngineWithRules(t,
		domain.Rule{Name: "r1", Enabled: true, Priority: 10, When: domain.RuleWhen{ModelIn: []string{"m1", "m2"}}, Then: domain.RuleThen{Status: statusPtr(domain.Status429), CustomMessage: strPtr("first")}},
		domain.Rule{Name: "r2", Enabled: true, Priority: 20, When: domain.RuleWhen{ModelIn: []string{"m1"}}, Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy), CustomMessage: strPtr("second")}},
	)
	ev := Event{Kind: Kind5xx, Model: "m1"}
	then, _ := e.Classify(ev)
	require.NotNil(t, then.CustomMessage)
	require.Equal(t, "first", *then.CustomMessage)
	require.Equal(t, domain.Status429, *then.Status)
	// m2 only hits first as well but second would also match if first not exist; priority still first
	ev2 := Event{Kind: Kind5xx, Model: "m2"}
	then2, _ := e.Classify(ev2)
	require.Equal(t, "first", *then2.CustomMessage)
}

func TestClassifyFallbackStill502(t *testing.T) {
	e := mustEngineWithRules(t,
		domain.Rule{Name: "r1", Enabled: true, Priority: 10, When: domain.RuleWhen{ModelIn: []string{"m1"}}, Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy)}},
	)
	ev := Event{Kind: Kind5xx, Model: "unknown"}
	then, punish := e.Classify(ev)
	require.Equal(t, 502, *then.ResponseCode)
	require.Equal(t, "upstream rejected request", *then.CustomMessage)
	require.False(t, punish)
}

func TestMatchWithInAndSingleSemantics(t *testing.T) {
	// single == nil && len(in)==0 => true (no filter)
	require.True(t, matchIntIn(nil, nil, nil))
	require.True(t, matchIntIn(intPtr(500), nil, nil))
	require.True(t, matchStringIn("any", nil, nil))
	require.True(t, matchSubstringIn("any", nil, nil))
	// single !=nil -> exact / substring
	require.True(t, matchStringIn("a", strPtr("a"), []string{"b"}))
	require.False(t, matchStringIn("a", strPtr("b"), []string{"a"})) // single path ignores in
	require.True(t, matchSubstringIn("hello world", strPtr("world"), nil))
	require.False(t, matchSubstringIn("hello", strPtr("world"), nil))
	// in path: any hit
	require.True(t, matchIntIn(intPtr(502), nil, []int{500, 502}))
	require.False(t, matchIntIn(intPtr(503), nil, []int{500, 502}))
}

func mustEngineWithRules(t *testing.T, rules ...domain.Rule) *RuleEngine {
	t.Helper()
	e, _ := newTestEngine(t, rules...)
	return e
}
