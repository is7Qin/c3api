// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package supplier

// risk.go 付款认领的结构化风险核对记录 `risk_review`（spec 2026-10-09 §6.5 C1）。
//
// 字段面固定：operator_id / decided_at / scope / evidence / decision /
// expires_at / approved_revision。**严格 JSON（存 TEXT）**：解析时拒绝未知键、错
// 类型、字段缺失、尾随数据；校验时拒绝任意 operator_id、错 scope、错 decision、
// 缺 evidence、未来 decided_at、超长/过期 expires_at、revision 不符。
//
// 服务端派生 operator_id（取资金 Actor，非客户端填）、decided_at（DB 时刻）、
// expires_at（decided_at + risk_review_max_age）、scope/decision（固定值）；
// 客户端只提供 evidence 文本。claim 在落库前对构建出的记录做**严格反序列化 +
// 校验**（round-trip），保证持久化记录恒良构；任何非良构记录一律拒绝（失败闭合）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// ErrRiskReview risk_review 非法（缺项/未知键/错类型/未来 decided_at/超长或过期
// expires_at/revision 不符等）⇒ 失败闭合（service 归类 400）。
var ErrRiskReview = errors.New("supplier: invalid risk_review record")

// RiskReviewScope 固定 scope（平台信用风险放行决定）。
const RiskReviewScope = "platform_credit_risk"

// RiskReviewDecision 唯一允许的结论（放行平台信用风险）。
const RiskReviewDecision = "approve_credit_risk"

// riskReviewSummary 服务端派生的核对摘要（evidence.summary）。
const riskReviewSummary = "platform credit risk approved"

// riskReviewClockSkew 未来 decided_at 的时钟偏移容忍预算（各实例与 DB 时钟差异）。
const riskReviewClockSkew = 5 * time.Second

// Evidence 非空结构（证据 reference + 核对摘要）。两字段都不得为空串。
type Evidence struct {
	Reference string `json:"reference"`
	Summary   string `json:"summary"`
}

// RiskReview 结构化风险核对记录（§6.5）。
type RiskReview struct {
	OperatorID       int64     `json:"operator_id"`
	DecidedAt        time.Time `json:"decided_at"`
	Scope            string    `json:"scope"`
	Evidence         Evidence  `json:"evidence"`
	Decision         string    `json:"decision"`
	ExpiresAt        time.Time `json:"expires_at"`
	ApprovedRevision int64     `json:"approved_revision"`
}

// ParseRiskReview 严格反序列化：拒绝未知键（DisallowUnknownFields）、错类型
// （JSON 类型与字段不符）、字段缺失（用指针探测）与尾随数据。
func ParseRiskReview(s string) (RiskReview, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	var raw struct {
		OperatorID       *int64     `json:"operator_id"`
		DecidedAt        *time.Time `json:"decided_at"`
		Scope            *string    `json:"scope"`
		Evidence         *Evidence  `json:"evidence"`
		Decision         *string    `json:"decision"`
		ExpiresAt        *time.Time `json:"expires_at"`
		ApprovedRevision *int64     `json:"approved_revision"`
	}
	if err := dec.Decode(&raw); err != nil {
		return RiskReview{}, fmt.Errorf("%w: %v", ErrRiskReview, err)
	}
	// 尾随数据 ⇒ 拒绝（合法单值 JSON 之后的任何非空白 token）。
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return RiskReview{}, fmt.Errorf("%w: trailing data after risk_review", ErrRiskReview)
	}
	var missing []string
	if raw.OperatorID == nil {
		missing = append(missing, "operator_id")
	}
	if raw.DecidedAt == nil {
		missing = append(missing, "decided_at")
	}
	if raw.Scope == nil {
		missing = append(missing, "scope")
	}
	if raw.Evidence == nil {
		missing = append(missing, "evidence")
	}
	if raw.Decision == nil {
		missing = append(missing, "decision")
	}
	if raw.ExpiresAt == nil {
		missing = append(missing, "expires_at")
	}
	if raw.ApprovedRevision == nil {
		missing = append(missing, "approved_revision")
	}
	if len(missing) > 0 {
		return RiskReview{}, fmt.Errorf("%w: missing fields %s", ErrRiskReview, strings.Join(missing, ","))
	}
	return RiskReview{
		OperatorID:       *raw.OperatorID,
		DecidedAt:        raw.DecidedAt.UTC(),
		Scope:            *raw.Scope,
		Evidence:         *raw.Evidence,
		Decision:         *raw.Decision,
		ExpiresAt:        raw.ExpiresAt.UTC(),
		ApprovedRevision: *raw.ApprovedRevision,
	}, nil
}

// BuildRiskReview 由服务端派生权威字段构造记录：operator_id 取资金 Actor、
// decided_at 取 DB 时刻、expires_at = decided_at + maxAge、scope/decision 固定；
// evidence 由客户端提供的 reference 文本 + 派生摘要拼装（reference 非空）。
func BuildRiskReview(actorID int64, evidenceRef string, expectedRevision int64, decidedAt time.Time, maxAge time.Duration) (RiskReview, error) {
	ref := strings.TrimSpace(evidenceRef)
	if ref == "" {
		return RiskReview{}, fmt.Errorf("%w: evidence reference required", ErrRiskReview)
	}
	return RiskReview{
		OperatorID:       actorID,
		DecidedAt:        decidedAt.UTC(),
		Scope:            RiskReviewScope,
		Evidence:         Evidence{Reference: ref, Summary: riskReviewSummary},
		Decision:         RiskReviewDecision,
		ExpiresAt:        decidedAt.Add(maxAge).UTC(),
		ApprovedRevision: expectedRevision,
	}, nil
}

// MarshalRiskReview 序列化记录为紧凑 JSON（落 `risk_review` TEXT）。
func MarshalRiskReview(rr RiskReview) (string, error) {
	b, err := json.Marshal(rr)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrRiskReview, err)
	}
	return string(b), nil
}

// ValidateRiskReview 校验记录字段面（§6.5 拒绝矩阵）：
//   - operator_id 必须 == 资金 Actor（非客户端任意填）；
//   - scope == platform_credit_risk；
//   - decision == approve_credit_risk；
//   - evidence.reference/summary 均非空；
//   - approved_revision == expected_revision；
//   - decided_at 不得在未来（> now + skew）；
//   - decided_at 不得早于 now - maxAge（过期）；
//   - expires_at 不得超出 decided_at + maxAge（超长）；
//   - expires_at 不得早于 now（已过期）。
func ValidateRiskReview(rr RiskReview, actorID, expectedRevision int64, now time.Time, maxAge time.Duration) error {
	if rr.OperatorID != actorID {
		return fmt.Errorf("%w: operator_id %d != actor %d", ErrRiskReview, rr.OperatorID, actorID)
	}
	if rr.Scope != RiskReviewScope {
		return fmt.Errorf("%w: scope %q", ErrRiskReview, rr.Scope)
	}
	if rr.Decision != RiskReviewDecision {
		return fmt.Errorf("%w: decision %q", ErrRiskReview, rr.Decision)
	}
	if strings.TrimSpace(rr.Evidence.Reference) == "" || strings.TrimSpace(rr.Evidence.Summary) == "" {
		return fmt.Errorf("%w: evidence incomplete", ErrRiskReview)
	}
	if rr.ApprovedRevision != expectedRevision {
		return fmt.Errorf("%w: approved_revision %d != expected %d", ErrRiskReview, rr.ApprovedRevision, expectedRevision)
	}
	if rr.DecidedAt.After(now.Add(riskReviewClockSkew)) {
		return fmt.Errorf("%w: decided_at in the future", ErrRiskReview)
	}
	if now.Sub(rr.DecidedAt) > maxAge {
		return fmt.Errorf("%w: risk review expired (decided_at too old)", ErrRiskReview)
	}
	if rr.ExpiresAt.After(rr.DecidedAt.Add(maxAge)) {
		return fmt.Errorf("%w: expires_at exceeds decided_at + max_age", ErrRiskReview)
	}
	if rr.ExpiresAt.Before(now) {
		return fmt.Errorf("%w: risk review expired (expires_at)", ErrRiskReview)
	}
	return nil
}

// ParseAndValidateRiskReview 严格反序列化 + 校验（round-trip 复核持久化记录）。
func ParseAndValidateRiskReview(s string, actorID, expectedRevision int64, now time.Time, maxAge time.Duration) error {
	rr, err := ParseRiskReview(s)
	if err != nil {
		return err
	}
	return ValidateRiskReview(rr, actorID, expectedRevision, now, maxAge)
}
