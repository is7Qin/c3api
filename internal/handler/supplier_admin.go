// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// supplier_admin.go 管理面结算审批 handler（spec 2026-10-09 §6.3/§6.4/§6.5）：
// 列表 + 五态 CAS（approve/reject/claim/confirm-failed/paid）+ 代申请 + 余额列表/
// PATCH。**资金命令一律要求具名操作者（I5）**：platform_admin JWT 与 platform_admin
// 管理 key 均注入 FundsActor；缺 FundsActor ⇒ 403。handler 只做线格式投影与操作者
// 提取；CAS/reject 退还/lifetime_paid/风控门在 repository 写事务内完成。

import (
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
)

// fundsActorFrom 读取资金操作者（管理面鉴权路径注入：platform_admin JWT 与管理 key
// 均经 FundsActor）；缺省（无具名操作者 / 无鉴权）⇒ ok=false（handler 返回 403，§6.5 I5）。
func fundsActorFrom(r *http.Request) (domain.FundsActor, bool) {
	return domain.FundsActorFrom(r.Context())
}

// requireFundsActor 提取资金操作者；缺失 ⇒ 403（无具名操作者明确拒绝）。
func requireFundsActor(w http.ResponseWriter, r *http.Request) (domain.FundsActor, bool) {
	actor, ok := fundsActorFrom(r)
	if !ok {
		httpface.WriteErr(w, http.StatusForbidden, "funds commands require a named operator")
		return domain.FundsActor{}, false
	}
	return actor, true
}

func toAPISupplierBalance(b domain.SupplierBalance) SupplierBalance {
	return SupplierBalance{
		SupplierUserId:    b.SupplierUserID,
		Available:         b.Available,
		LifetimeCredited:  b.LifetimeCredited,
		LifetimePaid:      b.LifetimePaid,
		ShareBp:           b.ShareBp,
		FreezeHours:       b.FreezeHours,
		LatestAvailableAt: b.LatestAvailableAt,
		BucketRows:        b.BucketRows,
	}
}

// GetAdminSupplierSettlements GET /api/admin/supplier/settlements（§6.3）。
func (h *AdminAPI) GetAdminSupplierSettlements(w http.ResponseWriter, r *http.Request, params GetAdminSupplierSettlementsParams) {
	var filter domain.SupplierSettlementFilter
	if params.Status != nil {
		s := domain.SupplierSettlementStatus(*params.Status)
		filter.Status = &s
	}
	if params.Kind != nil {
		k := domain.SupplierSettlementKind(*params.Kind)
		filter.Kind = &k
	}
	limit, offset := pageParams(params.Limit, params.Offset)
	items, total, err := h.svc.AdminListSupplierSettlements(r.Context(), filter, limit, offset)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]SupplierSettlement, 0, len(items))
	for _, s := range items {
		out = append(out, toAdminSupplierSettlement(s))
	}
	httpface.WriteJSON(w, http.StatusOK, SupplierSettlementList{Items: out, Total: total})
}

// GetAdminSupplierBalances GET /api/admin/supplier/balances（§6.3）。
func (h *AdminAPI) GetAdminSupplierBalances(w http.ResponseWriter, r *http.Request, params GetAdminSupplierBalancesParams) {
	limit, offset := pageParams(params.Limit, params.Offset)
	items, total, err := h.svc.AdminListSupplierBalances(r.Context(), limit, offset)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]SupplierBalance, 0, len(items))
	for _, b := range items {
		out = append(out, toAPISupplierBalance(b))
	}
	httpface.WriteJSON(w, http.StatusOK, SupplierBalanceList{Items: out, Total: total})
}

// PatchAdminSupplierBalancesUid PATCH /api/admin/supplier/balances/{uid}（§6.3/§6.4
// A16⑤）：仅 share_bp/freeze_hours（不接受金额）。
func (h *AdminAPI) PatchAdminSupplierBalancesUid(w http.ResponseWriter, r *http.Request, uid int64) {
	var body SupplierBalancePatchBody
	if err := httpface.Decode(r, &body); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	var patch domain.SupplierBalancePatch
	if body.ShareBp.IsSpecified() {
		if body.ShareBp.IsNull() {
			patch.ClearShareBp = true
		} else {
			v := body.ShareBp.MustGet()
			patch.ShareBp = &v
		}
	}
	if body.FreezeHours.IsSpecified() {
		if body.FreezeHours.IsNull() {
			patch.ClearFreezeHours = true
		} else {
			v := body.FreezeHours.MustGet()
			patch.FreezeHours = &v
		}
	}
	out, err := h.svc.AdminPatchSupplierBalance(r.Context(), uid, patch)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPISupplierBalance(*out))
}

// PostAdminSupplierSettlementsIdApprove pending → approved（§6.3）。
func (h *AdminAPI) PostAdminSupplierSettlementsIdApprove(w http.ResponseWriter, r *http.Request, id int64) {
	actor, ok := requireFundsActor(w, r)
	if !ok {
		return
	}
	var body SettlementTransitionBody
	if err := httpface.Decode(r, &body); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if body.SettlementRev < 1 {
		httpface.WriteErr(w, http.StatusBadRequest, "expected_revision is required")
		return
	}
	s, err := h.svc.AdminApproveSettlement(r.Context(), id, body.SettlementRev, actor)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAdminSupplierSettlement(s))
}

// PostAdminSupplierSettlementsIdReject pending|approved → rejected（退还 available）。
func (h *AdminAPI) PostAdminSupplierSettlementsIdReject(w http.ResponseWriter, r *http.Request, id int64) {
	actor, ok := requireFundsActor(w, r)
	if !ok {
		return
	}
	var body SettlementRejectBody
	if err := httpface.Decode(r, &body); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if body.SettlementRev < 1 {
		httpface.WriteErr(w, http.StatusBadRequest, "expected_revision is required")
		return
	}
	s, err := h.svc.AdminRejectSettlement(r.Context(), id, body.SettlementRev, body.Reason, actor)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAdminSupplierSettlement(s))
}

// PostAdminSupplierSettlementsIdClaim approved → paying（认领 + 风控门；§6.5）。
func (h *AdminAPI) PostAdminSupplierSettlementsIdClaim(w http.ResponseWriter, r *http.Request, id int64) {
	actor, ok := requireFundsActor(w, r)
	if !ok {
		return
	}
	var body SettlementClaimBody
	if err := httpface.Decode(r, &body); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if body.SettlementRev < 1 {
		httpface.WriteErr(w, http.StatusBadRequest, "expected_revision is required")
		return
	}
	s, err := h.svc.AdminClaimSettlement(r.Context(), id, body.SettlementRev, body.AmountMillis, domain.SupplierPayeeSnapshot{
		PayeeName: body.PayeeSnapshot.PayeeName,
		Account:   body.PayeeSnapshot.Account,
		Unit:      body.PayeeSnapshot.Unit,
	}, domain.SupplierRiskEvidence{
		Reference:        body.RiskEvidence.Reference,
		Summary:          body.RiskEvidence.Summary,
		ApprovedRevision: body.RiskEvidence.ApprovedRevision,
	}, actor)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAdminSupplierSettlement(s))
}

// PostAdminSupplierSettlementsIdConfirmFailed paying → approved（仅结构化「确定未支付」
// +「旧执行已停止」核验；未知 ⇒ 失败闭合保留 paying）。
func (h *AdminAPI) PostAdminSupplierSettlementsIdConfirmFailed(w http.ResponseWriter, r *http.Request, id int64) {
	actor, ok := requireFundsActor(w, r)
	if !ok {
		return
	}
	var body SettlementConfirmFailedBody
	if err := httpface.Decode(r, &body); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if body.SettlementRev < 1 {
		httpface.WriteErr(w, http.StatusBadRequest, "expected_revision is required")
		return
	}
	s, err := h.svc.AdminConfirmFailedSettlement(r.Context(), id, body.SettlementRev, domain.SupplierPayoutFailureConfirmation{
		Reason:              body.Reason,
		Evidence:            body.Evidence,
		ConfirmedNotPaid:    body.ConfirmedNotPaid,
		OldExecutionStopped: body.OldExecutionStopped,
	}, actor)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAdminSupplierSettlement(s))
}

// PostAdminSupplierSettlementsIdPaid 仅 paying → paid（lifetime_paid 累加；external_ref
// 必须非空）。
func (h *AdminAPI) PostAdminSupplierSettlementsIdPaid(w http.ResponseWriter, r *http.Request, id int64) {
	actor, ok := requireFundsActor(w, r)
	if !ok {
		return
	}
	var body SettlementPaidBody
	if err := httpface.Decode(r, &body); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if body.SettlementRev < 1 {
		httpface.WriteErr(w, http.StatusBadRequest, "expected_revision is required")
		return
	}
	s, err := h.svc.AdminPaidSettlement(r.Context(), id, body.SettlementRev, body.ExternalRef, actor)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAdminSupplierSettlement(s))
}

// PostAdminSupplierSettlementsAdminRequest 代申请（kind=admin_request；§6.3）。
func (h *AdminAPI) PostAdminSupplierSettlementsAdminRequest(w http.ResponseWriter, r *http.Request) {
	actor, ok := requireFundsActor(w, r)
	if !ok {
		return
	}
	var body AdminRequestSettlementBody
	if err := httpface.Decode(r, &body); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if body.SupplierUserId <= 0 || body.AmountMillis <= 0 || body.RequestKey == "" {
		httpface.WriteErr(w, http.StatusBadRequest, "supplier_user_id, amount_millis (>0) and request_key are required")
		return
	}
	s, err := h.svc.AdminRequestSettlement(r.Context(), domain.ApplySettlementRequest{
		OperatorUID:  actor.UserID,
		SupplierUID:  body.SupplierUserId,
		Kind:         domain.SettlementAdminRequest,
		AmountMillis: body.AmountMillis,
		RequestKey:   body.RequestKey,
		Note:         body.Note,
	}, actor)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAdminSupplierSettlement(s))
}

// toAdminSupplierSettlement 领域结算单 → 契约类型（管理面）。
func toAdminSupplierSettlement(s *domain.SupplierSettlement) SupplierSettlement {
	return SupplierSettlement{
		Id:                   s.ID,
		SupplierUserId:       s.SupplierUserID,
		Kind:                 SupplierSettlementKind(s.Kind),
		AmountMillis:         s.AmountMillis,
		PeriodStart:          s.PeriodStart,
		PeriodEnd:            s.PeriodEnd,
		Status:               SupplierSettlementStatus(s.Status),
		Revision:             s.Revision,
		RequestKey:           s.RequestKey,
		RequestedAt:          s.RequestedAt,
		RequestedOperator:    s.RequestedOperator,
		ReviewedAt:           s.ReviewedAt,
		ReviewerUserId:       s.ReviewerUserID,
		PayoutOperatorUserId: s.PayoutOperatorUserID,
		PayoutStartedAt:      s.PayoutStartedAt,
		ExternalRef:          s.ExternalRef,
		PaidAt:               s.PaidAt,
		PaidOperatorUserId:   s.PaidOperatorUserID,
		PayoutFailureReason:  s.PayoutFailureReason,
		PayoutFailedAt:       s.PayoutFailedAt,
		PaymentKey:           s.PaymentKey,
		PayeeSnapshot:        s.PayeeSnapshot,
		RiskReview:           s.RiskReview,
		Note:                 s.Note,
		RejectReason:         s.RejectReason,
	}
}
