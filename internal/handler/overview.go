// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"fmt"
	"net/http"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/service"
)

// GET /api/admin/overview 管理端总览（spec 2026-08-14）：一站式聚合端点——
// summary（今日 UTC 日界，cost 毫分 /1e5 → USD）+ trend（近 N 天日桶，SQL
// 侧 GROUP BY）+ accounts（调度器快照健康分布/并发水位）+ resources（三表
// 冷面 count）+ err_top（账号维度 EWMA Top5）+ alerts（billing 水线注入面）。
// 全冷面（聚合查询 + 快照遍历）；内部 TTL 30s 缓存，键含 days/group_id +
// UTC 日界（summary"今日"跨午夜滚转——缓存键带日界分量）；无 singleflight
// （dashboard 单消费者，P3 声明接受）。
func (h *AdminAPI) GetAdminOverview(w http.ResponseWriter, r *http.Request, params GetAdminOverviewParams) {
	days := deref(params.Days)
	if days < 1 {
		days = 7 // 缺省 7（契约 default；nil/非法回落缺省）
	}
	if days > 30 {
		days = 30
	}
	groupID := deref(params.GroupId)
	utcDay := h.now().UTC().Truncate(24 * time.Hour) // 缓存键与聚合区间同一日界源
	key := fmt.Sprintf("o:%d:%d:%s", days, groupID, utcDay.Format("2006-01-02"))
	if v, ok := h.overviewCache.get(key); ok {
		httpface.WriteJSON(w, http.StatusOK, v)
		return
	}
	data, err := h.svc.Overview(r.Context(), utcDay, days, groupID)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	resp := overviewResponse(data, h.ops.BillingAlerts)
	h.overviewCache.set(key, resp)
	httpface.WriteJSON(w, http.StatusOK, resp)
}

// overviewResponse 服务端聚合结果 → 契约类型（cost 毫分 /1e5 → USD——API
// 边界换算，内部毫分不动；err_rate = errors/requests，无请求 = 0；TTFT 指标 =
// 查询侧 Go 除/直方图插值——SQL 只 sum/count/max，spec 2026-08-14 §4）。
func overviewResponse(d *service.OverviewData, alerts func() BillingAlerts) OverviewResponse {
	summary := OverviewSummary{
		Requests:        d.Summary.Requests,
		Errors:          d.Summary.Errors,
		ErrRate:         errRateOf(d.Summary.Requests, d.Summary.Errors),
		CostUsd:         millisToUSD(d.Summary.Cost),
		RawCostUsd:      millisToUSD(d.Summary.RawCost),
		InputTokens:     d.Summary.InputTokens,
		OutputTokens:    d.Summary.OutputTokens,
		TotalTokens:     d.Summary.TotalTokens,
		CacheReadTokens: d.Summary.CacheReadTokens,
		CallCount:       d.Summary.CallCount,
		TtftAvgMs:       d.Summary.TTFTAvgMS(),
		TtftMaxMs:       d.Summary.TTFTMaxMS,
		TtftP50Ms:       d.Summary.TTFTPercentileMS(0.50),
		TtftP90Ms:       d.Summary.TTFTPercentileMS(0.90),
		TtftP95Ms:       d.Summary.TTFTPercentileMS(0.95),
		TtftP99Ms:       d.Summary.TTFTPercentileMS(0.99),
	}
	trend := make([]OverviewTrend, 0, len(d.Trend))
	for _, t := range d.Trend {
		trend = append(trend, OverviewTrend{
			Date:      openapiDate(t.Date),
			Requests:  t.Requests,
			Errors:    t.Errors,
			CostUsd:   millisToUSD(t.Cost),
			RawCostUsd: millisToUSD(t.RawCost),
			Tokens:    t.Tokens,
			CallCount: t.CallCount,
			TtftAvgMs: t.TTFTAvgMS(),
			TtftMaxMs: t.TTFTMaxMS,
			TtftP50Ms: t.TTFTPercentileMS(0.50),
			TtftP90Ms: t.TTFTPercentileMS(0.90),
			TtftP95Ms: t.TTFTPercentileMS(0.95),
			TtftP99Ms: t.TTFTPercentileMS(0.99),
		})
	}
	errTop := make([]OverviewErrTop, 0, len(d.ErrTop))
	for _, e := range d.ErrTop {
		errTop = append(errTop, OverviewErrTop{Name: e.Name, ErrRate: e.ErrRate, ErrCount: e.ErrCount})
	}
	var a BillingAlerts
	if alerts != nil {
		a = alerts()
	}
	return OverviewResponse{
		Summary: summary,
		Trend:   trend,
		Accounts: OverviewAccounts{
			Active:         d.Accounts.Active,
			Unhealthy:      d.Accounts.Unhealthy,
			N429:           d.Accounts.N429,
			Disabled:       d.Accounts.Disabled,
			Concurrency:    d.Accounts.Concurrency,
			MaxConcurrency: int(d.Accounts.MaxConcurrency),
		},
		Resources: OverviewResources{
			Templates: d.Resources.Templates,
			Groups:    d.Resources.Groups,
			Users:     d.Resources.Users,
		},
		ErrTop: errTop,
		Alerts: OverviewAlerts{
			BillingPending:          a.Pending,
			BillingPendingWaterline: a.PendingWaterline,
			BillingWarned:           a.Warned,
		},
	}
}

// errRateOf 错误率（errors / requests；无请求 = 0）。
func errRateOf(requests, errors int64) float64 {
	if requests <= 0 {
		return 0
	}
	return float64(errors) / float64(requests)
}

// openapiDate time.Time → 契约 Date（UTC 日桶序列化为 YYYY-MM-DD）。
func openapiDate(t time.Time) openapi_types.Date {
	return openapi_types.Date{Time: t}
}
