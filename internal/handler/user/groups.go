// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
)

// GetUserGroups 可选组列表（public 全部 + 已授予 private；只读——组管理是
// 平台面职责，key 创建时在此选组，ServerInterface）。逐组附该用户在该组的
// **有效倍率**（UserGroup.EffectiveMultiplier = 专属覆盖 ?? 组默认）。
func (h *UserAPI) GetUserGroups(w http.ResponseWriter, r *http.Request) {
	uid := currentUserID(r)
	rows, err := h.svc.ListGroupsForUser(r.Context(), uid)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	overrides, err := h.svc.GroupMultiplierOverrides(r.Context(), uid)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]UserGroup, 0, len(rows))
	for _, g := range rows {
		out = append(out, toAPIUserGroup(g, effectiveMultiplier(g, overrides)))
	}
	httpface.WriteJSON(w, http.StatusOK, out)
}

// effectiveMultiplier 该用户在该组的有效倍率（正常值 *float64，万分数 → 倍率）：
// 专属覆盖（group_assignments.price_multiplier）优先，否则组默认
// Group.PriceMultiplier。
func effectiveMultiplier(g *domain.Group, overrides map[int64]int) *float64 {
	mult := g.PriceMultiplier
	if o, ok := overrides[g.ID]; ok {
		mult = o
	}
	return httpface.MultBPToNormal(&mult)
}
