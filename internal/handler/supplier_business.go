// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// supplier_business.go 供应商业务面 handler（spec 2026-10-09 §6.1/§6.2）：
// overview/earnings/chunks/settlements + 申请结算。作用域 = JWT 本人 uid（从
// 已验证 claims 取；门控已由 RequireJWT+RequireRole 担保）。实现生成面
// supplier.ServerInterface；路由由 openapi（tag `supplier`）生成，经
// supplierSurface 适配器接到本实现（见 supplier_surface.go）。

import (
	"net/http"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/handler/supplier"
)

// supplierUIDFrom 取已验证 JWT 的 user_id（门控后必存在）。
func supplierUIDFrom(r *http.Request) (int64, bool) {
	claims, ok := auth.ClaimsFrom(r.Context())
	if !ok {
		return 0, false
	}
	return claims.UserID, true
}

// supplierFundsActor 从已验证 JWT claims 构造具名资金操作者（I5）：携带
// user_id + 签名请求的 token_version（claims.ver）——写事务内复核 status/role/
// token_version。RequireJWT 已注入 claims，故门控后必 ok。
func supplierFundsActor(r *http.Request) (domain.FundsActor, bool) {
	claims, ok := auth.ClaimsFrom(r.Context())
	if !ok {
		return domain.FundsActor{}, false
	}
	return domain.FundsActor{UserID: claims.UserID, TokenVersion: claims.Ver}, true
}

// GetSupplierOverview GET /api/user/supplier/overview。
func (h *AdminAPI) GetSupplierOverview(w http.ResponseWriter, r *http.Request) {
	uid, ok := supplierUIDFrom(r)
	if !ok {
		httpface.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ov, err := h.svc.SupplierOverview(r.Context(), uid)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, supplier.SupplierOverview{
		Available:         ov.Available,
		FrozenAmount:      ov.FrozenAmount,
		LifetimeCredited:  ov.LifetimeCredited,
		LifetimePaid:      ov.LifetimePaid,
		ShareBp:           ov.ShareBp,
		FreezeHours:       ov.FreezeHours,
		LatestAvailableAt: ov.LatestAvailableAt,
		BucketRows:        ov.BucketRows,
	})
}

// GetSupplierEarnings GET /api/user/supplier/earnings。
func (h *AdminAPI) GetSupplierEarnings(w http.ResponseWriter, r *http.Request, params supplier.GetSupplierEarningsParams) {
	uid, ok := supplierUIDFrom(r)
	if !ok {
		httpface.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	limit, offset := pageParams(params.Limit, params.Offset)
	items, total, err := h.svc.SupplierEarnings(r.Context(), uid, limit, offset)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]supplier.SupplierEarning, 0, len(items))
	for _, e := range items {
		out = append(out, supplier.SupplierEarning{CreatedAt: e.CreatedAt, Cost: e.Cost, EarnMillis: e.EarnMillis, Model: e.Model})
	}
	httpface.WriteJSON(w, http.StatusOK, supplier.SupplierEarningList{Items: out, Total: total})
}

// GetSupplierChunks GET /api/user/supplier/chunks。
func (h *AdminAPI) GetSupplierChunks(w http.ResponseWriter, r *http.Request, params supplier.GetSupplierChunksParams) {
	uid, ok := supplierUIDFrom(r)
	if !ok {
		httpface.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	limit, offset := pageParams(params.Limit, params.Offset)
	items, total, err := h.svc.SupplierChunks(r.Context(), uid, limit, offset)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]supplier.SupplierChunk, 0, len(items))
	for _, c := range items {
		out = append(out, supplier.SupplierChunk{AvailableAt: c.AvailableAt, Amount: c.Amount})
	}
	httpface.WriteJSON(w, http.StatusOK, supplier.SupplierChunkList{Items: out, Total: total})
}

// GetSupplierSettlements GET /api/user/supplier/settlements。
func (h *AdminAPI) GetSupplierSettlements(w http.ResponseWriter, r *http.Request, params supplier.GetSupplierSettlementsParams) {
	uid, ok := supplierUIDFrom(r)
	if !ok {
		httpface.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	limit, offset := pageParams(params.Limit, params.Offset)
	items, total, err := h.svc.SupplierSettlements(r.Context(), uid, limit, offset)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]supplier.SupplierSettlement, 0, len(items))
	for _, s := range items {
		out = append(out, toSupplierSettlement(s))
	}
	httpface.WriteJSON(w, http.StatusOK, supplier.SupplierSettlementList{Items: out, Total: total})
}

// PostSupplierSettlement POST /api/user/supplier/settlements（申请结算 §6.2）。
// 具名 JWT 操作者（I5）：actor 从已验证 claims 构造（缺 ⇒ 403，fail-closed）。
func (h *AdminAPI) PostSupplierSettlement(w http.ResponseWriter, r *http.Request) {
	uid, ok := supplierUIDFrom(r)
	if !ok {
		httpface.WriteErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	actor, ok := supplierFundsActor(r)
	if !ok {
		httpface.WriteErr(w, http.StatusForbidden, "funds commands require a named JWT operator")
		return
	}
	var body supplier.PostSupplierSettlementJSONRequestBody
	if err := httpface.Decode(r, &body); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	s, err := h.svc.ApplySupplierSettlement(r.Context(), domain.ApplySettlementRequest{
		OperatorUID:  uid,
		SupplierUID:  uid,
		Kind:         domain.SettlementSupplierRequest,
		AmountMillis: body.AmountMillis,
		RequestKey:   body.RequestKey,
		Note:         body.Note,
	}, actor)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toSupplierSettlement(s))
}

func toSupplierSettlement(s *domain.SupplierSettlement) supplier.SupplierSettlement {
	return supplier.SupplierSettlement{
		Id:                   s.ID,
		SupplierUserId:       s.SupplierUserID,
		Kind:                 supplier.SupplierSettlementKind(s.Kind),
		AmountMillis:         s.AmountMillis,
		PeriodStart:          s.PeriodStart,
		PeriodEnd:            s.PeriodEnd,
		Status:               supplier.SupplierSettlementStatus(s.Status),
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
		Note:                 s.Note,
		RejectReason:         s.RejectReason,
	}
}

func pageParams(limit, offset *int) (int, int) {
	return httpface.ClampLimit(httpface.Deref(limit)), httpface.Deref(offset)
}
