// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"errors"
	"fmt"
	"strings"
)

// 供应商结算输入的**纯值层**规则单一实现（spec 2026-10-09 §6.5 C1/C2/C4）。
//
// 这里只承载「输入形状」的规范化与校验（金额、收款目标、风险证据、失败确认、
// 付款凭证），不做任何 DB/锁/CAS 判定：锁内金额与单据匹配、锁后 DB 时间、操作者
// 状态/token_version、状态/revision CAS 留在 repository 事务内。service 与
// repository 入口各自调用同一函数，避免同一规则出现多份不完全相同的表达式。
//
// ErrSupplierInputInvalid 是纯值层校验失败哨兵；service 映射到其 ErrInvalidInput
// (400)，repository 映射到其 ErrInvalidInput。

// ErrSupplierInputInvalid 结算输入纯值层校验失败（调用方各自映射到 400）。
var ErrSupplierInputInvalid = errors.New("supplier input invalid")

// SupplierClaimCommand 认领（approved → paying）的纯输入命令（§6.5 C1/C2/C4）。
// 只承载输入规则：金额、收款目标、风险证据。
type SupplierClaimCommand struct {
	AmountMillis     int64
	ExpectedRevision int64
	Payee            SupplierPayeeSnapshot
	Risk             SupplierRiskEvidence
}

// NormalizeAndValidate 规范化并校验认领命令（纯函数，无副作用）：
//   - AmountMillis > 0；
//   - 收款目标结构化非空（经 Payee.NormalizeAndValidate trim）；
//   - 风险证据 reference/summary 非空（trim）且 ApprovedRevision == ExpectedRevision。
//
// 返回规范化后的命令副本。金额与单据匹配等**事务内**判定不在此处。
func (c SupplierClaimCommand) NormalizeAndValidate() (SupplierClaimCommand, error) {
	if c.AmountMillis <= 0 {
		return c, fmt.Errorf("%w: amount_millis must be > 0", ErrSupplierInputInvalid)
	}
	payee, err := c.Payee.NormalizeAndValidate()
	if err != nil {
		return c, err
	}
	risk, err := c.Risk.NormalizeAndValidate(c.ExpectedRevision)
	if err != nil {
		return c, err
	}
	c.Payee = payee
	c.Risk = risk
	return c, nil
}

// NormalizeAndValidate 规范化收款目标快照：收款人/账号/单位 trim 后均非空
// （§6.5 C4；不得把自由文本当收款目标）。
func (p SupplierPayeeSnapshot) NormalizeAndValidate() (SupplierPayeeSnapshot, error) {
	p.PayeeName = strings.TrimSpace(p.PayeeName)
	p.Account = strings.TrimSpace(p.Account)
	p.Unit = strings.TrimSpace(p.Unit)
	if p.PayeeName == "" || p.Account == "" || p.Unit == "" {
		return p, fmt.Errorf("%w: payee_snapshot requires payee_name/account/unit", ErrSupplierInputInvalid)
	}
	return p, nil
}

// NormalizeAndValidate 规范化风险核对证据：reference/summary trim 后非空，且
// ApprovedRevision 必须 == expectedRevision（§6.5 C1/I8；错 revision ⇒ 拒绝）。
func (r SupplierRiskEvidence) NormalizeAndValidate(expectedRevision int64) (SupplierRiskEvidence, error) {
	r.Reference = strings.TrimSpace(r.Reference)
	r.Summary = strings.TrimSpace(r.Summary)
	if r.Reference == "" || r.Summary == "" {
		return r, fmt.Errorf("%w: risk evidence reference and summary are required", ErrSupplierInputInvalid)
	}
	if r.ApprovedRevision != expectedRevision {
		return r, fmt.Errorf("%w: risk approved_revision %d != expected %d", ErrSupplierInputInvalid, r.ApprovedRevision, expectedRevision)
	}
	return r, nil
}

// Validate 规范化并校验结构化「确定未支付」核验（§6.5 C4）：reason/evidence trim 后
// 非空，且 ConfirmedNotPaid、OldExecutionStopped 均为 true；任一不符 ⇒ 失败闭合
// （保持 paying）。返回规范化后的核验副本。
func (f SupplierPayoutFailureConfirmation) Validate() (SupplierPayoutFailureConfirmation, error) {
	f.Reason = strings.TrimSpace(f.Reason)
	f.Evidence = strings.TrimSpace(f.Evidence)
	if f.Reason == "" || f.Evidence == "" || !f.ConfirmedNotPaid || !f.OldExecutionStopped {
		return f, fmt.Errorf("%w: confirm-failed requires reason, evidence and confirmed-not-paid/old-execution-stopped", ErrSupplierInputInvalid)
	}
	return f, nil
}

// NormalizeExternalRef 规范化付款凭证：trim 后非空（§6.3：paid 必须携带 external_ref）。
func NormalizeExternalRef(ref string) (string, error) {
	out := strings.TrimSpace(ref)
	if out == "" {
		return "", fmt.Errorf("%w: external_ref is required to confirm paid", ErrSupplierInputInvalid)
	}
	return out, nil
}
