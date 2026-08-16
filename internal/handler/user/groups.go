// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"net/http"

	"github.com/is7qin/c3api/internal/handler/httpface"
)

// GetUserGroups 可选组列表（public 全部 + 已授予 private；只读——组管理是
// 平台面职责，key 创建时在此选组，ServerInterface）。
func (h *UserAPI) GetUserGroups(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListGroupsForUser(r.Context(), currentUserID(r))
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]Group, 0, len(rows))
	for _, g := range rows {
		out = append(out, toAPIGroup(g))
	}
	httpface.WriteJSON(w, http.StatusOK, out)
}
