// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// GetUserUsageLogs 我的用量明细（/user/usage_logs；强制 user_id = 当前用户——
// 越权过滤在 service/repo 层，请求侧不可指定他人，ServerInterface）。
// keyset 游标分页：cursor 透传仅作本人行内 id 下界（跨页注入他人 id 仍被
// user_id 过滤钳制），next_cursor 组装与 admin 侧同构。
func (h *UserAPI) GetUserUsageLogs(w http.ResponseWriter, r *http.Request, params GetUserUsageLogsParams) {
	lq := repository.UsageQuery{Limit: 20, From: &params.From, To: &params.To, UserID: currentUserID(r)}
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
	if params.ErrorType != nil {
		lq.ErrorType = *params.ErrorType
	}
	rows, err := h.svc.QueryUsages(r.Context(), lq)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out := make([]UserUsageLog, 0, len(rows))
	for _, item := range rows { // service.QueryUsages 返回 []any（元素为 *domain.UsageLog）
		l, ok := item.(*domain.UsageLog)
		if !ok { // 类型不符是内部错误：不能静默丢数据，返回 500
			writeErr(w, http.StatusInternalServerError, "internal error: unexpected usage log row type")
			return
		}
		out = append(out, toAPIUsageLog(l))
	}
	// limit+1 探测（与 admin 侧同语义）：next_cursor = 本页最后一条 id。
	var next *int64
	if len(out) > lq.Limit {
		next = out[lq.Limit-1].ID
		out = out[:lq.Limit]
	}
	writeJSON(w, http.StatusOK, UserLogsResponse{Rows: out, NextCursor: next})
}
