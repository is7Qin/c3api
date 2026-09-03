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
	// 非法 granularity 直透 service 哨兵（同 GetStatsTrend，RG-BE M-1）。
	granularity := ""
	if params.Granularity != nil {
		granularity = string(*params.Granularity)
	}
	q := service.EntityTrendQuery{
		From:        params.From,
		To:          params.To,
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
	out := make([]StatTrendPoint, 0, len(rows))
	for _, b := range rows {
		out = append(out, toAPIEntityStatTrendPoint(b))
	}
	httpface.WriteJSON(w, http.StatusOK, out)
}

// GetUserStatsTTFT 我的 TTFT 聚合（强制 user_id = 当前用户）。数值与
// 时区无关；`timezone` 仅接受并校验（非法 400），不进查询。
func (h *UserAPI) GetUserStatsTTFT(w http.ResponseWriter, r *http.Request, params GetUserStatsTTFTParams) {
	if _, err := resolveStatsZone(params.Timezone); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	q := service.TTFTQuery{
		From: params.From,
		To:   params.To,
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
