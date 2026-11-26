// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

// RuleKind 规则事件类别（与服务端规则引擎的事件分类同一套值）。
//
// 为什么在 domain 而不是 rule：用户端 handler 要做「错误类型 → 类别」的映射并
// 把它交给规则判定，但它**不该 import 整个规则引擎**（引擎含匹配、惩罚、
// 持久化，属执行面）。类别本身是纯枚举，是两面共享的最小事实。
//
// 名带 Rule 前缀：domain 里已有 CallerKind / StatsKindID / ConfigFieldKind 等
// 「Kind」，裸 Kind 会与它们混淆；而 rule 侧的写法仍叫 rule.Kind（别名，见
// internal/rule/engine.go），新增读者按名字即可从两面之一找到这里。
//
// ⚠ 数值与顺序**不可变**：rule 侧有 `f.Kind == 0` 这类判据（0 表示「不投递
// 健康」），中间插入新值会让它错。新增只能追加到末尾。
type RuleKind int

const (
	RuleKindOK RuleKind = iota
	RuleKind429
	RuleKind4xx
	RuleKind5xx
	RuleKindNetwork // 连接级（code==0）事件——独立类型，不吃 5xx 冷却
	RuleKindTimeout // 上游预算内零产出；**必须留在末尾**
)

// String 线缆/日志上的稳定写法（与规则 when.kind 的字符串解析互逆：
// internal/rule 的 kindFromString 逐值对应本表的 6 个常量）。
func (k RuleKind) String() string {
	switch k {
	case RuleKindOK:
		return "ok"
	case RuleKind429:
		return "429"
	case RuleKind4xx:
		return "4xx"
	case RuleKind5xx:
		return "5xx"
	case RuleKindNetwork:
		return "network"
	case RuleKindTimeout:
		return "timeout"
	}
	return "unknown"
}
