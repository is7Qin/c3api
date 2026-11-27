// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package supplier

import "github.com/is7qin/c3api/internal/domain"

// risk.go 风险核对记录的**别名**：canonical 实现与校验逻辑已下沉 internal/domain
// 的 supplier_rules.go（repository 落库前要 round-trip 校验，不该依赖 worker 编排包）。

type (
	// Evidence 非空证据结构。
	Evidence = domain.Evidence
	// RiskReview 结构化风险核对记录。
	RiskReview = domain.RiskReview
)

const (
	// RiskReviewScope 固定 scope。
	RiskReviewScope = domain.RiskReviewScope
	// RiskReviewDecision 唯一允许的结论。
	RiskReviewDecision = domain.RiskReviewDecision
)

var (
	// ErrRiskReview risk_review 非法。
	ErrRiskReview = domain.ErrRiskReview
)

var (
	// ParseRiskReview 严格反序列化。
	ParseRiskReview = domain.ParseRiskReview
	// BuildRiskReview 构造记录。
	BuildRiskReview = domain.BuildRiskReview
	// MarshalRiskReview 序列化为紧凑 JSON。
	MarshalRiskReview = domain.MarshalRiskReview
	// ValidateRiskReview 校验字段面。
	ValidateRiskReview = domain.ValidateRiskReview
	// ParseAndValidateRiskReview 反序列化 + 校验。
	ParseAndValidateRiskReview = domain.ParseAndValidateRiskReview
)
