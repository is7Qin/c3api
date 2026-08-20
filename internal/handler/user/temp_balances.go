// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
)

// 我的临时额度（/api/user/temp-balances，spec 2026-08-15）：仅有效额度（未过期且
// 正余额），FEFO 同序展示；amount 毫分 → USD 在本包 convert 边界换算（内部
// 毫分不动）。

// GetUserTempBalances 我的临时额度（强制 user_id = 当前用户，无 user_id 参数
// ——对齐 /api/user/stats 模式防越权，ServerInterface）。
func (h *UserAPI) GetUserTempBalances(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListUserTempBalances(r.Context(), currentUserID(r))
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	// 合计先以毫分整数 Σ 再一次性 /1e5（避免逐行浮点累加误差）。
	totalMillis := int64(0)
	out := make([]TempBalanceRow, 0, len(rows))
	for _, tb := range rows {
		totalMillis += tb.Amount
		out = append(out, toAPITempBalance(tb))
	}
	httpface.WriteJSON(w, http.StatusOK, TempBalancesResponse{
		TotalUsd: float64(totalMillis) / 1e5,
		Rows:     out,
	})
}

// toAPITempBalance 临时额度领域对象 → 契约类型（Amount 毫分 → USD /1e5；
// ExpiresAt nil = 永久额度）。
func toAPITempBalance(tb *domain.TempBalance) TempBalanceRow {
	return TempBalanceRow{
		Id:        tb.ID,
		AmountUsd: float64(tb.Amount) / 1e5,
		ExpiresAt: tb.ExpiresAt,
		Note:      tb.Note,
	}
}
