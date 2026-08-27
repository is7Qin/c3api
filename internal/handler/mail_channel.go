// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"

	"github.com/is7qin/c3api/internal/handler/httpface"
)

func (h *AdminAPI) PostMailChannelTest(w http.ResponseWriter, r *http.Request) {
	var in MailChannelTestRequest
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if err := h.svc.SendMailChannelTest(r.Context(), string(in.Email)); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, MailChannelTestResponse{Sent: true})
}
