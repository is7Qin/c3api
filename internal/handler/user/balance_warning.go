// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"net/http"

	"github.com/is7qin/c3api/internal/handler/httpface"
)

func (h *UserAPI) GetUserBalanceWarningThreshold(w http.ResponseWriter, r *http.Request) {
	millis, err := h.svc.GetBalanceWarningThreshold(r.Context(), currentUserID(r))
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, BalanceWarningThresholdResponse{
		BalanceWarningThreshold: float64(millis) / 1e5,
	})
}

func (h *UserAPI) PutUserBalanceWarningThreshold(w http.ResponseWriter, r *http.Request) {
	var in BalanceWarningThresholdUpdate
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	updated, err := h.svc.UpdateBalanceWarningThreshold(r.Context(), currentUserID(r), in.BalanceWarningThreshold)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, BalanceWarningThresholdResponse{
		BalanceWarningThreshold: float64(updated.BalanceWarningThreshold) / 1e5,
	})
}
