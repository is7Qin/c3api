// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
)

// statusOnlyRuleStore 单规则替身（sanitizeErrLog 分类面：只关心 Classify，
// 其余写方法不可达）。
type statusOnlyRuleStore struct{ rules []domain.Rule }

func (s *statusOnlyRuleStore) ListRules(context.Context, *bool) ([]domain.Rule, error) {
	return s.rules, nil
}
func (s *statusOnlyRuleStore) CreateRule(context.Context, domain.Rule) (int64, error) { return 0, nil }
func (s *statusOnlyRuleStore) UpdateRule(context.Context, domain.Rule) error          { return nil }
func (s *statusOnlyRuleStore) DeleteRule(context.Context, int64) error                { return nil }
func (s *statusOnlyRuleStore) DeleteRulesBatch(context.Context, []int64) error        { return nil }
func (s *statusOnlyRuleStore) CountRules(context.Context) (int64, error)              { return 1, nil }

func errLogStrPtr(s string) *string { return &s }
func errLogIntPtr(v int) *int       { return &v }

// TestSanitizeErrLogInbandFailureHasNoHTTPStatus 显式复现 spec-stream-failure-truth
// §3.2 的**能力边界**（用户面同一条限制）：`error_type=5xx, status_code=0`
// （上游在流内声明失败但没给 HTTP 状态）的规则事件没有 HTTPStatus ⇒ 带
// http_status 条件的规则一律不匹配；同一规则在真 503 上必须命中（对照组证明
// 用例非空转）。后人若以为用户面能按"隐藏的健康 status"匹配，这条会红。
func TestSanitizeErrLogInbandFailureHasNoHTTPStatus(t *testing.T) {
	re := rule.New(rule.Config{}, &statusOnlyRuleStore{rules: []domain.Rule{{
		Name: "overload-503-only", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: errLogStrPtr("5xx"), HTTPStatus: errLogIntPtr(503)},
		Then: domain.RuleThen{CustomMessage: errLogStrPtr("upstream overloaded")},
	}}}, nil, nil, nil)
	require.NoError(t, re.Reload(context.Background()))
	h := &UserAPI{rules: re, now: time.Now}

	msg := errLogStrPtr("Our servers are currently overloaded. Please try again later.")

	// ① 流内失败：status_code=0（上游没给码）⇒ 503 条件规则不匹配 ⇒ 落到
	// 「无规则命中」的安全默认（502/"upstream rejected request"）。
	got, custom := h.sanitizeErrLog(&domain.UsageLog{
		ErrorType: domain.Err5xx, StatusCode: 0, ErrorMessage: msg,
	})
	require.True(t, custom)
	require.Equal(t, "upstream rejected request", got,
		"status_code=0 ⇒ 事件无 HTTPStatus ⇒ 带 http_status 的规则不匹配（§3.2）")

	// ② 对照：同一规则在真实 503 上必须命中（证明 ① 非空转）。
	got, custom = h.sanitizeErrLog(&domain.UsageLog{
		ErrorType: domain.Err5xx, StatusCode: 503, ErrorMessage: msg,
	})
	require.True(t, custom)
	require.Equal(t, "upstream overloaded", got, "真 503 命中同一规则")
}
