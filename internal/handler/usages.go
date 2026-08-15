// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// GetUsageLogs 用量明细分页查询（/usage_logs；消费面改名裁决：/logs → /usage_logs
// 与表名一致）。keyset 游标分页（用户裁决：from/to 生成层必填 400，cursor
// 缺失/≤0 = 首页；limit 上限 200 超限裁剪——游标语义下 Total 已从契约移除）。
// error_type 过滤保留——usage_logs 只剩 abort/failover 半异常标记（错误审计
// 面在 /err_logs）。
func (h *AdminAPI) GetUsageLogs(w http.ResponseWriter, r *http.Request, params GetUsageLogsParams) {
	lq := repository.UsageQuery{Limit: 20, From: &params.From, To: &params.To}
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
	if params.AccountId != nil {
		lq.AccountID = *params.AccountId
	}
	if params.UserId != nil {
		lq.UserID = *params.UserId
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
	out := make([]UsageLog, 0, len(rows))
	for _, item := range rows { // service.QueryUsages 返回 []any（元素为 *domain.UsageLog）
		l, ok := item.(*domain.UsageLog)
		if !ok { // 类型不符是内部错误：不能静默丢数据，返回 500
			writeErr(w, http.StatusInternalServerError, "internal error: unexpected usage log row type")
			return
		}
		out = append(out, toAPIUsageLog(l))
	}
	// limit+1 探测（repo 多取 1 行）：行数 > limit = 还有下一页，
	// next_cursor = 本页最后一条 id（下一页以其为游标，WHERE id < cursor）。
	var next *int64
	if len(out) > lq.Limit {
		next = out[lq.Limit-1].ID
		out = out[:lq.Limit]
	}
	writeJSON(w, http.StatusOK, LogsResponse{Rows: out, NextCursor: next})
}
