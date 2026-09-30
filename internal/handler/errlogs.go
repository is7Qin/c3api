// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"

	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/repository"
)

// GetErrLogs 错误明细分页查询（/err_logs：完整错误面——本地拒绝 + 半异常双轨，
// status_code/error_type 全值）。过滤面与 /usage_logs 同构 + status_code；
// keyset 游标分页与 /usage_logs 同语义（from/to 必填、cursor 透传、next_cursor 组装
// ——limit 归一/探测走 httpface.LogLimit + ClipLogPage，C2 消重）。
func (h *AdminAPI) GetErrLogs(w http.ResponseWriter, r *http.Request, params GetErrLogsParams) {
	lq := repository.ErrLogQuery{
		Limit:      httpface.LogLimit(httpface.Deref(params.Limit)),
		Cursor:     httpface.Deref(params.Cursor),
		From:       &params.From,
		To:         &params.To,
		GroupID:    httpface.Deref(params.GroupId),
		AccountID:  httpface.Deref(params.AccountId),
		UserID:     httpface.Deref(params.UserId),
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
	out := make([]ErrLog, 0, len(rows))
	for _, l := range rows { // service.QueryErrLogs 直透 []*domain.UsageLog（spec 2026-08-17）
		out = append(out, toAPIErrLog(l))
	}
	// limit+1 探测（与 GetUsageLogs 同语义）：next_cursor = 本页最后一条 id。
	out, next := httpface.ClipLogPage(out, lq.Limit, func(l ErrLog) *int64 { return l.ID })
	httpface.WriteJSON(w, http.StatusOK, ErrLogsResponse{Rows: out, NextCursor: next})
}
