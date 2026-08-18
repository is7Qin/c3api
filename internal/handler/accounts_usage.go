// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
)

// GetAccountsUsage 账号用量聚合（/admin/accounts/usage——统一 usage API 查询
// 面 spec 2026-08-18，ServerInterface）。参数解析与校验在 handler 层：
// account_ids 逗号分隔必填（非数字/空/去重后 >100 → 400）；from/to RFC3339
// 可选——缺省 = 当天（from=UTC 当日零点、to=now，"当天"语义单点，经 h.now
// 可注入时钟）；from > to → 400。响应 items 恒 = account_ids 去重后全量
// （无记录账号 gateway 全 0——前端免补零），顺序 = 去重后顺序。
func (h *AdminAPI) GetAccountsUsage(w http.ResponseWriter, r *http.Request, params GetAccountsUsageParams) {
	ids, err := parseAccountIDs(params.AccountIds)
	if err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	now := h.now()
	from := now.UTC().Truncate(24 * time.Hour) // UTC 当日零点（"当天"缺省单点）
	to := now
	if params.From != nil {
		from = *params.From
	}
	if params.To != nil {
		to = *params.To
	}
	if !from.Before(to) {
		httpface.WriteErr(w, http.StatusBadRequest, "from must be before to")
		return
	}
	items, err := h.svc.AccountsUsage(r.Context(), ids, from, to)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]AccountUsageItem, 0, len(items))
	for _, it := range items {
		out = append(out, toAPIAccountUsageItem(it))
	}
	httpface.WriteJSON(w, http.StatusOK, AccountsUsageResponse{Items: out})
}

// parseAccountIDs 解析逗号分隔 account_ids（非数字 → 错误）；1-100 条校验 +
// 去重走 normalizeIDs 既有惯例（原始条数检查在前；重复 id 响应 item 唯一，
// 顺序 = 首次出现序）。
func parseAccountIDs(s string) ([]int64, error) {
	if s == "" {
		return nil, errors.New("account_ids is required")
	}
	parts := strings.Split(s, ",")
	ids := make([]int64, 0, len(parts))
	for _, p := range parts {
		id, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
		if err != nil {
			return nil, errors.New("account_ids must be comma-separated integers")
		}
		ids = append(ids, id)
	}
	return normalizeIDs(ids)
}

// toAPIAccountUsageItem 领域 item → 契约类型（毫分 → USD /1e5 展示换算——
// temp-balances 先例；upstream 快照/错误标记直透）。
func toAPIAccountUsageItem(it domain.AccountUsage) AccountUsageItem {
	item := AccountUsageItem{
		AccountId: it.AccountID,
		Gateway: UsageGatewayStats{
			RawCostUsd:  millisToUSD(it.Gateway.RawCost),
			CostUsd:     millisToUSD(it.Gateway.Cost),
			Requests:    it.Gateway.Requests,
			TotalTokens: it.Gateway.TotalTokens,
		},
	}
	if it.UpstreamError != nil {
		e := AccountUsageItemUpstreamError(*it.UpstreamError)
		item.UpstreamError = &e
	}
	item.Upstream = toAPICodexSnapshot(it.Upstream)
	return item
}

// toAPICodexSnapshot 领域快照 → 契约类型（逐字段拷贝——生成类型独立于 domain，
// 对齐 convert.go 全字段映射惯例；nil 透传）。
func toAPICodexSnapshot(s *domain.CodexUsageSnapshot) *CodexUsageSnapshot {
	if s == nil {
		return nil
	}
	out := &CodexUsageSnapshot{}
	if s.PlanType != "" {
		out.PlanType = &s.PlanType
	}
	if s.RateLimit != nil {
		out.RateLimit = &CodexRateLimit{UsedPercent: s.RateLimit.UsedPercent, ResetAt: s.RateLimit.ResetAt}
	}
	if s.Credits != nil {
		out.Credits = &CodexCredits{Balance: s.Credits.Balance}
	}
	if s.SpendControl != nil {
		out.SpendControl = &CodexSpendControl{
			Limit: s.SpendControl.Limit, Used: s.SpendControl.Used, Remaining: s.SpendControl.Remaining,
			UsedPercent: s.SpendControl.UsedPercent, RemainingPercent: s.SpendControl.RemainingPercent,
		}
	}
	return out
}
