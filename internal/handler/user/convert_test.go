// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
)

// fakeRuleStore 最小 RuleStore（行级脱敏测试——种子写入 + 列表）。
type fakeRuleStore struct {
	rules []domain.Rule
}

func (f *fakeRuleStore) ListRules(ctx context.Context, enabled *bool) ([]domain.Rule, error) {
	out := make([]domain.Rule, 0, len(f.rules))
	for _, r := range f.rules {
		if enabled != nil && r.Enabled != *enabled {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeRuleStore) CreateRule(ctx context.Context, r domain.Rule) (int64, error) {
	f.rules = append(f.rules, r)
	return int64(len(f.rules)), nil
}

func (f *fakeRuleStore) CountRules(ctx context.Context) (int64, error) {
	return int64(len(f.rules)), nil
}

func (f *fakeRuleStore) UpdateRule(ctx context.Context, r domain.Rule) error { return nil }
func (f *fakeRuleStore) DeleteRule(ctx context.Context, id int64) error      { return nil }
func (f *fakeRuleStore) DeleteRulesBatch(ctx context.Context, ids []int64) error {
	return nil
}

func errRow(et domain.ErrorType, code int, msg string) *domain.UsageLog {
	return &domain.UsageLog{
		ID: 1, GroupID: 10, AccountID: 1, TemplateID: 1, UserID: 1, KeyID: 1,
		Model: "gpt-4o", StatusCode: code, ErrorType: et, ErrorMessage: &msg,
	}
}

func strPtrS(s string) *string { return &s }
func intPtrS(v int) *int       { return &v }

// newTestUserAPI 规则引擎注入版 UserAPI（种子表；rules 非 nil → 行级脱敏生效）。
func newTestUserAPI(t *testing.T, rules ...domain.Rule) *UserAPI {
	t.Helper()
	st := &fakeRuleStore{}
	for _, r := range rules {
		_, err := st.CreateRule(context.Background(), r)
		require.NoError(t, err)
	}
	e := rule.New(rule.Config{}, st, nil)
	require.NoError(t, e.Reload(context.Background()))
	return &UserAPI{rules: e}
}

// TestUserErrLogSanitize 用户面 err_logs 行级脱敏（gate Major 2 全函数映射）：
// 401 原文行 → 固定文案（无命中 → 默认归一）；400 行 → 原文
// （seed-4xx-400 ResponseCode nil + CustomMessage nil 全透）；ErrAuth/ErrBilling 本地拒绝行 → 原样（无
// kind 不参与策略）；5xx/network/abort 行 → 固定文案（seed 命中 CustomMessage）。
func TestUserErrLogSanitize(t *testing.T) {
	h := newTestUserAPI(t) // 空表 → 种子 5 条

	cases := []struct {
		name string
		row  *domain.UsageLog
		want string
	}{
		{"401 原文行 → 固定文案", errRow(domain.Err4xx, 401, "Insufficient balance, workspace ws_1, https://example.com/billing"),
			upstreamRejectedMsg},
		{"403 原文行 → 固定文案", errRow(domain.Err4xx, 403, "forbidden upstream detail"), upstreamRejectedMsg},
		{"400 行 → 原文（seed-4xx-400 透传）", errRow(domain.Err4xx, 400, "bad request"), "bad request"},
		{"429 行 → 固定文案（seed-429）", errRow(domain.Err429, 429, "upstream 429 detail"), "rate limited"},
		{"5xx 行 → 固定文案（seed-5xx）", errRow(domain.Err5xx, 500, "upstream internal detail"), "Upstream request failed"},
		{"network 行 → 固定文案（seed-network）", errRow(domain.ErrNetwork, 0, "dial tcp: refused"), "Upstream request failed"},
		{"abort 行 → 固定文案（保守映射 5xx）", errRow(domain.ErrAbort, 499, "client closed before response"), "Upstream request failed"},
		{"ErrAuth 本地拒绝行 → 原样", errRow(domain.ErrAuth, 401, "invalid gateway key"), "invalid gateway key"},
		{"ErrBilling 本地拒绝行 → 原样", errRow(domain.ErrBilling, 402, "insufficient balance"), "insufficient balance"},
		{"ErrNoAccount 本地拒绝行 → 原样", errRow(domain.ErrNoAccount, 404, "no available account"), "no available account"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := h.toAPIErrLog(tc.row)
			require.NotNil(t, e.ErrorMessage)
			require.Equal(t, tc.want, *e.ErrorMessage)
		})
	}
}

// TestUserErrLogSanitizeTransmitRule 用户全透规则命中 → 原文保留
// （指针意图：CustomMessage nil = 透传原文，响应与日志同步原文）。
func TestUserErrLogSanitizeTransmitRule(t *testing.T) {
	h := newTestUserAPI(t,
		domain.Rule{Name: "tx-401", Enabled: true, Priority: 10,
			When: domain.RuleWhen{Kind: strPtrS("4xx"), HTTPStatus: intPtrS(401)},
			Then: domain.RuleThen{}},
	)
	e := h.toAPIErrLog(errRow(domain.Err4xx, 401, "balance detail for user debugging"))
	require.NotNil(t, e.ErrorMessage)
	require.Equal(t, "balance detail for user debugging", *e.ErrorMessage)
}

// TestUserErrLogSanitizeAccountScoped 行级脱敏与响应归一同一策略：账号条件
// 规则只命中该账号行（其余账号 401 行仍归一）。
func TestUserErrLogSanitizeAccountScoped(t *testing.T) {
	aid := int64(1)
	h := newTestUserAPI(t,
		domain.Rule{Name: "acc-1-tx", Enabled: true, Priority: 10,
			When: domain.RuleWhen{Kind: strPtrS("4xx"), HTTPStatus: intPtrS(401), AccountID: &aid},
			Then: domain.RuleThen{}},
	)
	// 账号 1 的 401 行 → 全透规则命中 → 原文
	e := h.toAPIErrLog(errRow(domain.Err4xx, 401, "acc1 raw"))
	require.Equal(t, "acc1 raw", *e.ErrorMessage)
	// 账号 2 的 401 行 → 不命中 → 归一
	row := errRow(domain.Err4xx, 401, "acc2 raw")
	row.AccountID = 2
	e = h.toAPIErrLog(row)
	require.Equal(t, upstreamRejectedMsg, *e.ErrorMessage)
}

// TestUserErrLogNoRulesNil 规则引擎未注入（测试/未装配）→ 恒原文（零回归）。
func TestUserErrLogNoRulesNil(t *testing.T) {
	h := &UserAPI{}
	e := h.toAPIErrLog(errRow(domain.Err4xx, 401, "raw detail"))
	require.NotNil(t, e.ErrorMessage)
	require.Equal(t, "raw detail", *e.ErrorMessage)
}
