// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"
	"time"

	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/repository"
)

// GetStats 用量统计聚合（ServerInterface）。from/to 缺省 now-24h..now，
// granularity 非法值回落 day。
func (h *AdminAPI) GetStats(w http.ResponseWriter, r *http.Request, params GetStatsParams) {
	sq := repository.StatQuery{From: time.Now().Add(-24 * time.Hour), To: time.Now()}
	if params.From != nil {
		sq.From = *params.From
	}
	if params.To != nil {
		sq.To = *params.To
	}
	if params.GroupId != nil {
		sq.GroupID = *params.GroupId
	}
	if params.AccountId != nil {
		sq.AccountID = *params.AccountId
	}
	if params.TemplateId != nil {
		sq.TemplateID = *params.TemplateId
	}
	if params.UserId != nil {
		sq.UserID = *params.UserId
	}
	if params.Model != nil {
		sq.Model = *params.Model
	}
	granularity := "day"
	if params.Granularity != nil {
		granularity = string(*params.Granularity)
		if granularity != "hour" && granularity != "day" {
			granularity = "day"
		}
	}
	rows, err := h.svc.QueryStats(r.Context(), sq, granularity)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]StatBucket, 0, len(rows))
	for _, b := range rows {
		out = append(out, toAPIStatBucket(b))
	}
	httpface.WriteJSON(w, http.StatusOK, out)
}
