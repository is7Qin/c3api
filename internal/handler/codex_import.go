// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"net/http"

	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/service"
)

// PostAccountsImportCodexOauth imports Codex OAuth JSON without returning any
// credential material.
func (h *AdminAPI) PostAccountsImportCodexOauth(w http.ResponseWriter, r *http.Request) {
	var in CodexOAuthImportRequest
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	credentials, err := json.Marshal(in.Credentials)
	if err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid credentials")
		return
	}
	result, err := h.svc.ImportCodexOAuth(r.Context(), credentials, service.CodexOAuthImportOptions{
		TemplateID:     in.TemplateId,
		GroupIDs:       append([]int64(nil), deref(in.GroupIds)...),
		NamePrefix:     deref(in.NamePrefix),
		Weight:         deref(in.Weight),
		MaxConcurrency: deref(in.MaxConcurrency),
	})
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}

	items := make([]CodexOAuthImportItem, 0, len(result.Items))
	for _, item := range result.Items {
		out := CodexOAuthImportItem{
			AccountId: item.AccountID,
			Index:     item.Index,
			Status:    CodexOAuthImportItemStatus(item.Status),
		}
		if item.SpaceID != "" {
			out.SpaceId = ptr(item.SpaceID)
		}
		if item.Email != "" {
			out.Email = ptr(item.Email)
		}
		if item.Message != "" {
			out.Message = ptr(item.Message)
		}
		items = append(items, out)
	}
	httpface.WriteJSON(w, http.StatusOK, CodexOAuthImportResponse{
		Imported: result.Imported,
		Updated:  result.Updated,
		Skipped:  result.Skipped,
		Failed:   result.Failed,
		Items:    items,
	})
}
