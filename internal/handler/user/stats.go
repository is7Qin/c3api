// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"net/http"
	"time"

	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/service"
)

// resolveStatsZone 用户面 `timezone` 参数边界解析（与 admin handler 同一
// service.ResolveTimeZone 单一实现）：缺省/空 → UTC；非法 IANA → 400。
// 结果仅请求内使用——无进程级时区。
func resolveStatsZone(raw *string) (*time.Location, error) {
	if raw == nil {
		return time.UTC, nil
	}
	return service.ResolveTimeZone(*raw)
}

// GetUserStats 我的用量统计（强制 user_id = 当前用户；granularity 缺省 day；
// timezone = 浏览器时区，缺省 UTC——分组路由见 repository）。
func (h *UserAPI) GetUserStats(w http.ResponseWriter, r *http.Request, params GetUserStatsParams) {
	zone, err := resolveStatsZone(params.Timezone)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	// 窗口两形态（from+to / window 单独，恰择一）在共享的请求边界解码（与 admin
	// 面同一份 httpface.ResolveStatsWindow）；时钟复用同一套可注入机制
	// （UserAPI.now，与 AdminAPI.now 同款）——不新造时钟源。
	from, to, err := httpface.ResolveStatsWindow(params.From, params.To, params.Window, h.now())
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	// 非法 granularity 直透 service 哨兵（同 GetStatsTrend）。
	granularity := ""
	if params.Granularity != nil {
		granularity = string(*params.Granularity)
	}
	q := service.EntityTrendQuery{
		From:        from,
		To:          to,
		Granularity: granularity,
		Zone:        zone,
	}
	if params.Model != nil {
		q.Model = *params.Model
	}
	rows, err := h.svc.UserStats(r.Context(), currentUserID(r), q)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	// 与 /stats/trend、/stats/entity-trend 同形：200 体裸数组 ⇒ 生效窗口与
	// 实际存储走回显头（spec §4.4(b)），值取自 service 判定回传的 Exec。
	httpface.WriteStatsEcho(w, rows.Exec)
	out := make([]StatTrendPoint, 0, len(rows.Buckets))
	for _, b := range rows.Buckets {
		out = append(out, toAPIEntityStatTrendPoint(b))
	}
	httpface.WriteJSON(w, http.StatusOK, out)
}

// GetUserStatsCapabilities 用户台的统计能力（与 /api/admin/stats/capabilities
// 同一份服务端投影——service.StatsCapabilities 是唯一来源，本层只做形状映射，
// 零字面量）。存在的理由：用户台的选择器裁剪与 TTFT 窗口上限同样需要按部署
// 取值，而 /api/admin/* 只接受 platform_admin 凭据（普通用户拿不到）。
func (h *UserAPI) GetUserStatsCapabilities(w http.ResponseWriter, r *http.Request) {
	httpface.WriteJSON(w, http.StatusOK, toAPIStatsCapabilities(h.svc.StatsCapabilities()))
}

// GetUserStatsTTFT 我的 TTFT 聚合（强制 user_id = 当前用户）。数值与
// 时区无关；`timezone` 仅接受并校验（非法 400），不进查询。窗口形态解码/
// 时钟同 GetUserStats。
func (h *UserAPI) GetUserStatsTTFT(w http.ResponseWriter, r *http.Request, params GetUserStatsTTFTParams) {
	if _, err := resolveStatsZone(params.Timezone); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	from, to, err := httpface.ResolveStatsWindow(params.From, params.To, params.Window, h.now())
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	q := service.TTFTQuery{
		From: from,
		To:   to,
	}
	if params.Model != nil {
		q.Model = *params.Model
	}
	sum, err := h.svc.UserStatsTTFT(r.Context(), currentUserID(r), q)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIStatTTFTSummary(sum))
}
