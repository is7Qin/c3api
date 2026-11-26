// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"context"

	"github.com/is7qin/c3api/internal/domain"
)

// ErrLogClassifier 用户面 err_logs **行级脱敏**要问「这条错误会命中什么规则」。
//
// 本包**不持有**规则引擎：引擎是执行面的一部分（含规则匹配、惩罚动作、窗口
// 计数与持久化），用户面只借它的一个判定结果。所以这里只声明"要问什么"，由
// 组合根（cmd/server）注入实现。
//
// 参数与返回值**只用 domain 与本包的形状**——签名里不出现 rule.*，本包才能不
// import 规则引擎而独立演化（改引擎的匹配/惩罚/持久化不会波及用户面）。
type ErrLogClassifier interface {
	// ClassifyErrLog 返回该行应改写的文案；ok=false 表示策略不干预（调用方原样
	// 透传上游正文）。实现方负责把 in 翻译成引擎事件、跑同一套判定，并套用与
	// 代理响应**同源**的统一公式 msg=CustomMessage!=nil?*CustomMessage:upstream
	// （见 internal/rule.UnifiedMessage）。
	ClassifyErrLog(ctx context.Context, in ErrLogQuery) (msg string, ok bool)
}

// ErrLogQuery 一次行级脱敏判定的入参：**只含用户面真正需要喂给规则的标量**。
//
// 为什么不是 rule.Event：那里面还有 RouteClassID / QualityClassID /
// CandidateFingerprint / ExpectedIdentityRevision 等执行面身份字段（账本记账、
// CAS 守卫用），用户面既拿不到也语义无关；把整个事件类型搬过来等于把执行面
// 形状钉进用户面契约。
//
// 字段口径（与旧 rule.Event 构造逐字一致，改这里要看 convert.go 的
// sanitizeErrLog）：
//   - Kind 总是有值：本类型没有"未知类别"表示，因为它只由 errTypeKind 的
//     ok=true 分支构造（ok=false 的行根本不会走到这里）；
//   - HTTPStatus 只有 status_code > 0 才有值（nil = 上游没给码，带
//     http_status 条件的规则恒不匹配，见 spec-stream-failure-truth §3.2）；
//   - Model 是**最终请求模型**（映射后 MappedModel，与 failoverLoop 一致）；
//   - GroupID nil = 无分组（规则 when.group_id 恒不匹配）。
type ErrLogQuery struct {
	AccountID    int64
	TemplateID   int64
	GroupID      *int64 // nil = 无分组（规则 when.group_id 恒不匹配）
	Model        string
	Kind         domain.RuleKind
	HTTPStatus   *int // nil = 上游未给状态码
	ErrorMessage string
}
