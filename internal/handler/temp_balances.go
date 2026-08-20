// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
)

// 临时额度管理面（/api/admin/temp-balances，spec 2026-08-15）：全量视角分页列表
// （含过期/用尽/负扣减行——与用户侧"仅有效额度"视角分明）；amount 毫分 →
// USD 在 handler/convert 边界换算（内部毫分不动）。

// GetTempBalances 临时额度列表（增强分页范式 + user_id 筛选 + sort 白名单
// expires_at/amount/created_at + order，ServerInterface）。默认排序 expires_at
// asc（FEFO 同序——与扣费顺序一致）；ListQuery.sortOrder 缺省 sort 为 id，
// 本端点白名单不含 id，故缺省在 handler 显式归一为 expires_at。
func (h *AdminAPI) GetTempBalances(w http.ResponseWriter, r *http.Request, params GetTempBalancesParams) {
	q, err := pageToQuery(params.Page, params.PageSize)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	if params.Sort == nil {
		q.Sort = "expires_at"
	} else {
		q.Sort = *params.Sort
	}
	// order 缺省 asc（spec 默认表）：共享 ListQuery.sortOrder 缺省是 desc
	// （管理列表惯例）——本端点默认 expires_at asc 须显式归一。
	if params.Order == nil {
		q.Order = "asc"
	} else {
		q.Order = string(*params.Order)
	}
	var userID int64
	if params.UserId != nil {
		userID = *params.UserId
	}
	rows, total, err := h.svc.ListTempBalances(r.Context(), q, userID)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]AdminTempBalanceRow, 0, len(rows))
	for _, tb := range rows {
		out = append(out, toAPIAdminTempBalance(tb))
	}
	httpface.WriteJSON(w, http.StatusOK, AdminTempBalancesResponse{Total: total, Rows: out})
}

// toAPIAdminTempBalance 临时额度领域对象 → 契约类型（Amount 毫分 → USD /1e5；
// ExpiresAt nil = 永久额度）。
func toAPIAdminTempBalance(tb *domain.TempBalance) AdminTempBalanceRow {
	return AdminTempBalanceRow{
		Id:        tb.ID,
		UserId:    tb.UserID,
		AmountUsd: millisToUSD(tb.Amount),
		ExpiresAt: tb.ExpiresAt,
		Note:      tb.Note,
		CreatedAt: tb.CreatedAt,
	}
}
