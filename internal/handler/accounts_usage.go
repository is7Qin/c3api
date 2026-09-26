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
// account_ids 逗号分隔必填（非数字/空/去重后 >100 → 400）；窗口**两形态恰择一**
// （P3，spec §7.3）：from+to 绝对窗口，或 window 时长串相对窗口（服务端自持
// `h.now`：to = 整点向上取整、from = to − window）。两态都给 / 只给一端 / 都不给
// → 400 `window_ambiguous`；`window` 不是时长串（如 `7d`）→ 400 `window_invalid`。
// **本端点不再有"缺省 = 当天"**（那正是"两态都不给"的第三种形态，双路径按裁决
// 删除而非兼容；调用方必须显式说出它要哪一段）。响应 items 恒 = account_ids
// 去重后全量（无记录账号 gateway 全 0——前端免补零），顺序 = 去重后顺序。
//
// **窗口上限与覆盖率在 service 层判定**（domain.Admit，KindUsageAgg：raw cost
// 90d + usage_logs 覆盖率）——本 handler 只把线上形态解码成一对绝对时刻，
// 倒序（from 不早于 to）也由 Admit step 1 报 400（window_invalid），不再有第二
// 份形状校验（spec §4.5：全区间 GROUP BY 无 LIMIT 的真实成本洞在 service 补掉）。
func (h *AdminAPI) GetAccountsUsage(w http.ResponseWriter, r *http.Request, params GetAccountsUsageParams) {
	if _, err := resolveStatsZone(params.Timezone); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	ids, err := parseAccountIDs(params.AccountIds)
	if err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	from, to, err := httpface.ResolveStatsWindow(params.From, params.To, params.Window, h.now())
	if err != nil {
		httpface.WriteServiceErr(w, err)
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

// assembleUpstream upstream 栏装配（原 service.AccountsUsage 后半段
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
