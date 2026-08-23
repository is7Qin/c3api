// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"net/http"

	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/service"
)

// GetUserStats 我的用量统计（强制 user_id = 当前用户；granularity 缺省 day）。
func (h *UserAPI) GetUserStats(w http.ResponseWriter, r *http.Request, params GetUserStatsParams) {
	granularity := "day"
	if params.Granularity != nil {
		granularity = string(*params.Granularity)
		if granularity != "hour" && granularity != "day" {
			granularity = "day"
		}
	}
	q := service.EntityTrendQuery{
		From:        params.From,
		To:          params.To,
		Granularity: granularity,
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

// GetUserStatsTTFT 我的 TTFT 聚合（强制 user_id = 当前用户）。
func (h *UserAPI) GetUserStatsTTFT(w http.ResponseWriter, r *http.Request, params GetUserStatsTTFTParams) {
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
