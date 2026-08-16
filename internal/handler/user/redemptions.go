// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"net/http"

	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
)

// 兑换码用户面（/user/redemptions，JWT 保护区内——Router 中间件强制；user_id
// 一律取当前登录用户，防越权）。

// pageToQuery 增强分页范式（page 1-based + page_size）→ repository.ListQuery
// （与 admin 面同语义：page 缺省/越下界按 1；page_size 缺省 20，越界 → 400）。
func pageToQuery(page, pageSize *int) (repository.ListQuery, error) {
	p := 1
	if page != nil && *page > 1 {
		p = *page
	}
	size := 20
	if pageSize != nil {
		size = *pageSize
	}
	if size < 1 || size > 100 {
		return repository.ListQuery{}, service.ErrInvalidInput
	}
	return repository.ListQuery{Limit: size, Offset: (p - 1) * size}, nil
}

// PostUserRedemptions 兑换码（ServerInterface）：400 invalid code（不存在/失效/
// 过期/用尽，统一不泄露细节，决策 7）、409 already redeemed（重复兑换）。
func (h *UserAPI) PostUserRedemptions(w http.ResponseWriter, r *http.Request) {
	var in RedeemRequest
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	apply, err := h.svc.Redeem(r.Context(), in.Code, currentUserID(r))
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	var resp RedeemResponse
	resp.Applied.Type = RedemptionType(apply.Type)
	resp.Applied.Value = redemptionValueToAPI(apply.Type, apply.Value)
	resp.Applied.ResourceExpiresAt = apply.ResourceExpiresAt
	httpface.WriteJSON(w, http.StatusOK, resp)
}

// GetUserRedemptions 我的兑换记录（use 快照 + 码的 type/remark 联查；强制
// user_id = 当前用户，ServerInterface）。
func (h *UserAPI) GetUserRedemptions(w http.ResponseWriter, r *http.Request, params GetUserRedemptionsParams) {
	q, err := pageToQuery(params.Page, params.PageSize)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	q.Sort = deref(params.Sort)
	q.Order = string(deref(params.Order))
	rows, total, err := h.svc.ListMyRedemptions(r.Context(), currentUserID(r), q)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]RedemptionRecord, 0, len(rows))
	for _, rec := range rows {
		out = append(out, RedemptionRecord{
			ID:                rec.ID,
			CodeID:            rec.CodeID,
			Code:              rec.Code,
			CodeType:          RedemptionType(rec.CodeType),
			Value:             redemptionValueToAPI(rec.CodeType, rec.Value),
			Remark:            rec.Remark,
			ResourceExpiresAt: rec.ResourceExpiresAt,
			CreatedAt:         rec.CreatedAt,
		})
	}
	httpface.WriteJSON(w, http.StatusOK, RedemptionRecordListResponse{Total: total, Rows: out})
}
