// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import "time"

// 供应商业务面领域类型（spec 2026-10-09 §6.1/§6.2/§6.3）。

// SupplierSettlementKind 结算单来源：供应商自申请 / 管理员代申请（复用同一通道）。
type SupplierSettlementKind string

const (
	SettlementSupplierRequest SupplierSettlementKind = "supplier_request"
	SettlementAdminRequest    SupplierSettlementKind = "admin_request"
)

// SupplierSettlementStatus 五态。
type SupplierSettlementStatus string

const (
	SettlementPending  SupplierSettlementStatus = "pending"
	SettlementApproved SupplierSettlementStatus = "approved"
	SettlementPaying   SupplierSettlementStatus = "paying"
	SettlementPaid     SupplierSettlementStatus = "paid"
	SettlementRejected SupplierSettlementStatus = "rejected"
)

// SupplierOverview 供应商收益概览（§6.1）。
type SupplierOverview struct {
	Available         int64
	FrozenAmount      int64
	LifetimeCredited  int64
	LifetimePaid      int64
	ShareBp           int
	FreezeHours       int
	LatestAvailableAt *time.Time
	BucketRows        int64
}

// SupplierChunk 冻结中桶。
type SupplierChunk struct {
	AvailableAt time.Time
	Amount      int64
}

// SupplierEarning 区间收益明细行（usage_logs 投影）。
type SupplierEarning struct {
	CreatedAt  time.Time
	Cost       int64
	EarnMillis int64
	Model      *string
}

// SupplierSettlement 结算单（§3.6 投影）。
type SupplierSettlement struct {
	ID                   int64
	SupplierUserID       int64
	Kind                 SupplierSettlementKind
	AmountMillis         int64
	PeriodStart          time.Time
	PeriodEnd            time.Time
	Status               SupplierSettlementStatus
	Revision             int64
	RequestKey           string
	RequestedAt          time.Time
	RequestedOperator    int64
	ReviewedAt           *time.Time
	ReviewerUserID       *int64
	PayoutOperatorUserID *int64
	PayoutStartedAt      *time.Time
	ExternalRef          *string
	PaidAt               *time.Time
	PaidOperatorUserID   *int64
	PayoutFailureReason  *string
	PayoutFailedAt       *time.Time
	PaymentKey           *string
	PayeeSnapshot        *string
	RiskReview           *string
	Note                 *string
	RejectReason         *string
}

// SupplierPayeeSnapshot 收款目标快照（§6.5 C4）：结构化固定（收款人/账号/单位）。
// 认领副作用前固定、重试/恢复复用，不一致 ⇒ 拒绝。
type SupplierPayeeSnapshot struct {
	PayeeName string
	Account   string
	Unit      string
}

// SupplierPayoutFailureConfirmation 结构化「确定未支付」核验（§6.5 C4）：具名确认人 +
// 确定未支付结论 + 证据 + 旧执行已停止确认。任一缺失/false ⇒ 失败闭合（保留 paying）。
type SupplierPayoutFailureConfirmation struct {
	Reason              string
	Evidence            string
	ConfirmedNotPaid    bool
	OldExecutionStopped bool
}

// SupplierRiskEvidence 结构化风险核对证据（§6.5 C1/I8）：reference + summary（均非空）
// + approved_revision（必须 == expected_revision）。服务端派生 operator_id/decided_at/
// scope/decision/expires_at；不得把任意自由文本当证据自动签为放行。
type SupplierRiskEvidence struct {
	Reference        string
	Summary          string
	ApprovedRevision int64
}

// ApplySettlementRequest 申请结算入参（供应商自申请 / 管理员代申请共用；§6.2）。
type ApplySettlementRequest struct {
	// OperatorUID 请求操作者（supplier_request = supplier 本人；admin_request = 管理员）。
	OperatorUID int64
	// SupplierUID 目标供应商（自申请时 = OperatorUID）。
	SupplierUID int64
	Kind        SupplierSettlementKind
	// AmountMillis 申请金额（> 0）。
	AmountMillis int64
	// RequestKey 业务幂等键（客户端生成、重试复用）。
	RequestKey string
	Note       *string
}
