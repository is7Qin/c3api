// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"

	userapi "github.com/is7qin/c3api/internal/handler/user"
	"github.com/is7qin/c3api/internal/rule"
)

// errLogClassifier 把用户面的「这条错误行命中什么规则」翻译到规则引擎上。
//
// 为什么薄适配器住在组合根而不是 handler/user 里：用户面只声明它要问什么
// （userapi.ErrLogClassifier），引擎是执行面组件（匹配 + 惩罚 + 窗口计数 +
// 持久化）。让用户面 import 引擎，就是把执行面形状钉进用户面契约；让引擎
// import 用户面，则是执行面去认领一个 HTTP 面的私事。翻译属装配知识，所以
// 它住在**唯一装配点** cmd/server。
//
// 翻译内容只有两件事，且都与引擎的公共形状无关：
//  1. 用户面标量 → rule.Event（只填用户面真有的字段；执行面身份字段
//     RouteClassID/QualityClassID/CandidateFingerprint/ExpectedIdentityRevision
//     留零值——行级脱敏只问"命中哪条规则"，从不投递事件，也不应用惩罚）；
//  2. 命中结果 → 统一公式（rule.UnifiedMessage，与代理响应/用户面附加文案同源）。
type errLogClassifier struct {
	engine *rule.RuleEngine
}

// ClassifyErrLog 实现 userapi.ErrLogClassifier。
func (c errLogClassifier) ClassifyErrLog(_ context.Context, in userapi.ErrLogQuery) (string, bool) {
	then, _ := c.engine.Classify(rule.Event{
		AccountID:    in.AccountID,
		TemplateID:   in.TemplateID,
		GroupID:      in.GroupID,
		Model:        in.Model,
		Kind:         in.Kind,
		HTTPStatus:   in.HTTPStatus,
		ErrorMessage: in.ErrorMessage,
	})
	// 上游正文只作占位：rule.UnifiedMessage 的第二参数不使用（正文由调用方
	// 透传），这里保持与全仓其余 4 个调用点同形。
	return rule.UnifiedMessage(then, in.ErrorMessage)
}
