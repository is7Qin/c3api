// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/pkg/logx"
)

// CodexUsageProber codex 额度快照数据源（*sdkbridge.Codex 满足——组合根经
// OpsOptions.UsageSnap 构造注入；接口化供测试注入）。
type CodexUsageProber interface {
	GetUsageSnapshot(ctx context.Context, cred *domain.AccountCredential) (*domain.CodexUsageSnapshot, error)
}

// GetAccountsUsage 账号用量聚合（/api/admin/accounts/usage——统一 usage API 查询
// 面 spec 2026-08-18，ServerInterface）。参数解析与校验在 handler 层：
// account_ids 逗号分隔必填（非数字/空/去重后 >100 → 400）；from/to RFC3339
// 可选——缺省 = 当天（from=请求浏览器时区当日零点（`timezone` 参数，缺省
// UTC）、to=now，"当天"语义单点，经 h.now 可注入时钟）；显式 from/to 为绝对
// 时刻直透，不做任何时区改写；from > to → 400。响应 items 恒 = account_ids
// 去重后全量（无记录账号 gateway 全 0——前端免补零），顺序 = 去重后顺序。
// 底层读 usage_logs 原始行绝对区间——本端点时区只影响缺省日界，不影响数值。
func (h *AdminAPI) GetAccountsUsage(w http.ResponseWriter, r *http.Request, params GetAccountsUsageParams) {
	zone, err := resolveStatsZone(params.Timezone)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	ids, err := parseAccountIDs(params.AccountIds)
	if err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	now := h.now()
	from := dayStart(now, zone) // 请求时区当日零点（"当天"缺省单点）
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
	items, err := h.svc.AccountsGatewayUsage(r.Context(), ids, from, to)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	h.assembleUpstream(r.Context(), items)
	out := make([]AccountUsageItem, 0, len(items))
	for _, it := range items {
		out = append(out, toAPIAccountUsageItem(it))
	}
	httpface.WriteJSON(w, http.StatusOK, AccountsUsageResponse{Items: out})
}

// assembleUpstream upstream 栏装配（W2-T3：原 service.AccountsUsage 后半段
// 整体搬迁——行为逐分支一致）：逐账号凭据组装（svc.AccountUsageCredential：
// api-key/非 codex → nil cred → null 快照/null 标记；store 故障 → 已在
// service 侧 Warn + null/null）→ codex 账号调 prober（nil prober = 未装配
// → null 快照，与旧 service nil-setter 降级一致）→ sdkbridge 哨兵映射
// upstream_error（ErrAuthExpired → auth_expired，ErrUpstream →
// upstream_unavailable；未知错误 → Warn + null/null，不误标）。
//
// 失败语义：单账号快照失败不整批失败（其余账号照常）；批内 errgroup 有界
// 并发（8——与 sdkbridge usageFetchSem 容量对齐：上游并发仍由适配层恒保
// ≤8，此处仅并行化编排的 DB 往返/调用分发）；结果按 ids 顺序（goroutine
// 按 index 写 items，保序）。
func (h *AdminAPI) assembleUpstream(ctx context.Context, items []domain.AccountUsage) {
	var g errgroup.Group
	g.SetLimit(8) // 批内并行度（与 sdkbridge usageFetchSem 语义对齐）
	for i := range items {
		i := i
		g.Go(func() error {
			cred, err := h.svc.AccountUsageCredential(ctx, items[i].AccountID)
			if err != nil {
				return nil // store 故障（service 侧已 Warn）→ null/null，不误标
			}
			if cred == nil {
				return nil // 非 codex（api-key 无凭据）→ null 快照/null 标记
			}
			if h.usageSnap == nil {
				return nil // 未装配 → null 快照（旧 nil-setter 降级语义）
			}
			snap, err := h.usageSnap.GetUsageSnapshot(ctx, cred)
			switch {
			case err == nil:
				items[i].Upstream = snap
			case errors.Is(err, sdkbridge.ErrAuthExpired):
				e := domain.UpstreamErrorAuthExpired
				items[i].UpstreamError = &e
			case errors.Is(err, sdkbridge.ErrUpstream):
				e := domain.UpstreamErrorUpstreamUnavailable
				items[i].UpstreamError = &e
			default:
				// 未知上游错误 → 不误标：null/null（ctx 取消为请求已死信号，
				// 不记 Warn）。
				if ctx.Err() == nil && h.log != nil {
					h.log.Warn("accounts usage: account upstream lookup failed", logx.Int64("account_id", items[i].AccountID), logx.Error(err))
				}
			}
			return nil
		})
	}
	// 内部分支恒 nil（单账号失败只记标记）——Wait 仅作在途屏障。
	_ = g.Wait()
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
