// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"net/http"

	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/repository"
)

// GetUserErrLogs 我的错误明细（/user/err_logs：完整错误面——本地拒绝 + 半异常
// 双轨；强制 user_id = 当前用户，防越权）。keyset 游标分页与 /user/usage_logs
// 同语义（cursor 透传仅本人行内生效）。
func (h *UserAPI) GetUserErrLogs(w http.ResponseWriter, r *http.Request, params GetUserErrLogsParams) {
	lq := repository.ErrLogQuery{Limit: 20, From: &params.From, To: &params.To, UserID: currentUserID(r)}
	if params.Limit != nil {
		lq.Limit = *params.Limit
	}
	if lq.Limit <= 0 {
		lq.Limit = 20
	}
	if lq.Limit > 200 {
		lq.Limit = 200
	}
	if params.Cursor != nil {
		lq.Cursor = *params.Cursor
	}
	if params.GroupId != nil {
		lq.GroupID = *params.GroupId
	}
	if params.KeyId != nil {
		lq.KeyID = *params.KeyId
	}
	if params.Model != nil {
		lq.Model = *params.Model
	}
	if params.Format != nil {
		lq.Format = string(*params.Format)
	}
	if params.StatusCode != nil {
		lq.StatusCode = *params.StatusCode
	}
	if params.ErrorType != nil {
		lq.ErrorType = *params.ErrorType
	}
	rows, err := h.svc.QueryErrLogs(r.Context(), lq)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]UserErrLog, 0, len(rows))
	for _, l := range rows { // service.QueryErrLogs 直透 []*domain.UsageLog（spec 2026-08-17）
		out = append(out, h.toAPIErrLog(l))
	}
	// limit+1 探测（与 admin 侧同语义）：next_cursor = 本页最后一条 id。
	var next *int64
	if len(out) > lq.Limit {
		next = out[lq.Limit-1].ID
		out = out[:lq.Limit]
	}
	httpface.WriteJSON(w, http.StatusOK, UserErrLogsResponse{Rows: out, NextCursor: next})
}
