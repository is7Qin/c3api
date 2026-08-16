// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"

	"github.com/is7qin/c3api/internal/handler/httpface"
)

// GetAdminSettings 全部设置（默认值 + DB 覆盖；ServerInterface）。
func (h *AdminAPI) GetAdminSettings(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.GetSettings(r.Context())
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]Setting, 0, len(rows))
	for _, s := range rows {
		out = append(out, toAPISetting(s))
	}
	httpface.WriteJSON(w, http.StatusOK, out)
}

// PutAdminSettings 更新设置（类型化校验在 service；返回更新后全部设置，
// ServerInterface）。
func (h *AdminAPI) PutAdminSettings(w http.ResponseWriter, r *http.Request) {
	var in SettingUpdate
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if _, err := h.svc.UpdateSetting(r.Context(), in.Key, in.Value); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	h.GetAdminSettings(w, r)
}
