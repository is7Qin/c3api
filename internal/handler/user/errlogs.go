// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"net/http"

	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/repository"
)

// GetUserErrLogs 我的错误明细（/api/user/err_logs：完整错误面——本地拒绝 + 半异常
// 双轨；强制 user_id = 当前用户，防越权）。keyset 游标分页与 /api/user/usage_logs
// 同语义（cursor 透传仅本人行内生效；limit 归一/探测走 httpface.LogLimit +
// ClipLogPage，C2 消重）。
func (h *UserAPI) GetUserErrLogs(w http.ResponseWriter, r *http.Request, params GetUserErrLogsParams) {
	lq := repository.ErrLogQuery{
		Limit:      httpface.LogLimit(httpface.Deref(params.Limit)),
		Cursor:     httpface.Deref(params.Cursor),
		From:       &params.From,
		To:         &params.To,
		UserID:     currentUserID(r),
		GroupID:    httpface.Deref(params.GroupId),
		KeyID:      httpface.Deref(params.KeyId),
		Model:      httpface.Deref(params.Model),
		Format:     string(httpface.Deref(params.Format)),
		StatusCode: httpface.Deref(params.StatusCode),
		ErrorType:  httpface.Deref(params.ErrorType),
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
	out, next := httpface.ClipLogPage(out, lq.Limit, func(l UserErrLog) *int64 { return l.ID })
	httpface.WriteJSON(w, http.StatusOK, UserErrLogsResponse{Rows: out, NextCursor: next})
}
