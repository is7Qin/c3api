// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"

	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/repository"
)

// GetUsageLogs 用量明细分页查询（/usage_logs；消费面改名裁决：/logs → /usage_logs
// 与表名一致）。keyset 游标分页（用户裁决：from/to 生成层必填 400，cursor
// 缺失/≤0 = 首页；limit 上限 200 超限裁剪——游标语义下 Total 已从契约移除）。
// error_type 过滤保留——usage_logs 只剩 abort/failover 半异常标记（错误审计
// 面在 /err_logs）。
//
// limit 缺省/钳制走 httpface.LogLimit，limit+1 探测走 httpface.ClipLogPage
// （与 4 个 usage/errlog handler 同源语义，C2 消重）。
func (h *AdminAPI) GetUsageLogs(w http.ResponseWriter, r *http.Request, params GetUsageLogsParams) {
	lq := repository.UsageQuery{
		Limit:     httpface.LogLimit(httpface.Deref(params.Limit)),
		Cursor:    httpface.Deref(params.Cursor),
		From:      &params.From,
		To:        &params.To,
		GroupID:   httpface.Deref(params.GroupId),
		AccountID: httpface.Deref(params.AccountId),
		UserID:    httpface.Deref(params.UserId),
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
	out := make([]UsageLog, 0, len(rows))
	for _, l := range rows { // service.QueryUsages 直透 []*domain.UsageLog（spec 2026-08-17）
		out = append(out, toAPIUsageLog(l))
	}
	// limit+1 探测（repo 多取 1 行）：行数 > limit = 还有下一页，
	// next_cursor = 本页最后一条 id（下一页以其为游标，WHERE id < cursor）。
	out, next := httpface.ClipLogPage(out, lq.Limit, func(l UsageLog) *int64 { return l.ID })
	httpface.WriteJSON(w, http.StatusOK, LogsResponse{Rows: out, NextCursor: next})
}
