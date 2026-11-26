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

// ruleEngineClassifier 测试夹具：把**真实** *rule.RuleEngine 装进本包声明的
// ErrLogClassifier 端口。
//
// 为什么这不违背"handler/user 不 import rule"：生产边已断，这里只剩测试夹具
// 需求（登记在 internal/architecture 的 testLedger）。convert_test /
// errlog_status_boundary_test 要验的是「种子规则表与透传规则**真的**生效」
// 以及「status_code=0 ⇒ 带 http_status 的规则**真的**不匹配」——那是引擎的判定
// 语义，换成 fake 就变成测替身，断言随之失去意义（与 P6-1 的 handler -> pricing
// 同型）。
//
// 翻译逻辑与生产适配器 cmd/server/errlog_classifier.go 刻意保持"同一件事"：
// 本文件多 import 一个 rule 是夹具代价，翻译公式却只有一条（rule.UnifiedMessage）。
// 若生产适配器改了翻译，这里会跟着红——这正是想要的信号。
type ruleEngineClassifier struct {
	engine *rule.RuleEngine
}

// ClassifyErrLog 实现 ErrLogClassifier。
func (c ruleEngineClassifier) ClassifyErrLog(_ context.Context, in ErrLogQuery) (string, bool) {
	then, _ := c.engine.Classify(rule.Event{
		AccountID:    in.AccountID,
		TemplateID:   in.TemplateID,
		GroupID:      in.GroupID,
		Model:        in.Model,
		Kind:         in.Kind,
		HTTPStatus:   in.HTTPStatus,
		ErrorMessage: in.ErrorMessage,
	})
	return rule.UnifiedMessage(then, in.ErrorMessage)
}

// TestUnifiedMessageMatchesRule 对照测试：本包私有 unifiedMessage 与
// rule.UnifiedMessage **逐分支同结果**。
//
// 为什么需要它：两处是同一个公式的**两份实现**（用户面为了不 import 整个规则
// 引擎而私有了一份三行函数）。没有这条测试，"改一处必须改另一处"只是注释里的
// 祈使句；有了它，任一侧改公式（比如开始使用被忽略的第二个参数）就是红。
//
// 输入覆盖：CustomMessage 有值（覆写）/ nil（透传）/ 空串（空串也是"有值"，
// 指针即意图——空文案照样覆写，不是透传）/ ResponseCode 存在但无文案。
func TestUnifiedMessageMatchesRule(t *testing.T) {
	cases := []struct {
		name string
		then domain.RuleThen
	}{
		{"CustomMessage 有值 → 覆写", domain.RuleThen{CustomMessage: new("rate limited")}},
		{"CustomMessage nil → 透传", domain.RuleThen{}},
		{"CustomMessage 空串 → 仍覆写（指针即意图）", domain.RuleThen{CustomMessage: new("")}},
		{"只有 ResponseCode，无文案", domain.RuleThen{ResponseCode: new(502)}},
		{"ResponseCode + 文案", domain.RuleThen{ResponseCode: new(502), CustomMessage: new("Upstream request failed")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotMsg, gotOK := unifiedMessage(tc.then)
			wantMsg, wantOK := rule.UnifiedMessage(tc.then, "上游正文由调用方透传，两版都不读它")
			require.Equal(t, wantOK, gotOK, "ok 分支必须与 rule.UnifiedMessage 一致")
			require.Equal(t, wantMsg, gotMsg, "文案必须与 rule.UnifiedMessage 一致")
		})
	}
}
