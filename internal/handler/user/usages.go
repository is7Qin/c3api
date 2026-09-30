// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"net/http"

	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/repository"
)

// GetUserUsageLogs 我的用量明细（/api/user/usage_logs；强制 user_id = 当前用户——
// 越权过滤在 service/repo 层，请求侧不可指定他人，ServerInterface）。
// keyset 游标分页：cursor 透传仅作本人行内 id 下界（跨页注入他人 id 仍被
// user_id 过滤钳制），next_cursor 组装与 admin 侧同构（limit 归一/探测走
// httpface.LogLimit + ClipLogPage，C2 消重）。
func (h *UserAPI) GetUserUsageLogs(w http.ResponseWriter, r *http.Request, params GetUserUsageLogsParams) {
	lq := repository.UsageQuery{
		Limit:     httpface.LogLimit(httpface.Deref(params.Limit)),
		Cursor:    httpface.Deref(params.Cursor),
		From:      &params.From,
		To:        &params.To,
		UserID:    currentUserID(r),
		GroupID:   httpface.Deref(params.GroupId),
		KeyID:     httpface.Deref(params.KeyId),
		Model:     httpface.Deref(params.Model),
		Format:    string(httpface.Deref(params.Format)),
		ErrorType: httpface.Deref(params.ErrorType),
	}
	rows, err := h.svc.QueryUsages(r.Context(), lq)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]UserUsageLog, 0, len(rows))
	for _, l := range rows { // service.QueryUsages 直透 []*domain.UsageLog（spec 2026-08-17）
		out = append(out, toAPIUsageLog(l))
	}
	// limit+1 探测（与 admin 侧同语义）：next_cursor = 本页最后一条 id。
	out, next := httpface.ClipLogPage(out, lq.Limit, func(l UserUsageLog) *int64 { return l.ID })
	httpface.WriteJSON(w, http.StatusOK, UserLogsResponse{Rows: out, NextCursor: next})
}
