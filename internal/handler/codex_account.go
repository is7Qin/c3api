// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/service"
)

// PostAccountsImportCodexOauth imports the OAuth JSON emitted by Plus and
// Codex clients. Credential values are intentionally never returned.
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
		if item.CodexAccountID != "" {
			out.CodexAccountId = ptr(item.CodexAccountID)
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

// GetAccountsIdCodexUsage returns an at-most-five-minute-old usage snapshot.
func (h *AdminAPI) GetAccountsIdCodexUsage(w http.ResponseWriter, r *http.Request, id int64) {
	usage, err := h.svc.GetCodexUsage(r.Context(), id)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	h.writeCodexUsage(w, usage)
}

// PostAccountsIdCodexRefreshUsage bypasses the per-account usage cache.
func (h *AdminAPI) PostAccountsIdCodexRefreshUsage(w http.ResponseWriter, r *http.Request, id int64) {
	usage, err := h.svc.RefreshCodexUsage(r.Context(), id)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	h.writeCodexUsage(w, usage)
}

// PostAccountsCodexRefreshUsage refreshes a bounded set of accounts in
// parallel and reports each failure without hiding successful items.
func (h *AdminAPI) PostAccountsCodexRefreshUsage(w http.ResponseWriter, r *http.Request) {
	var in CodexUsageBatchRequest
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	ids, err := normalizeIDs(in.AccountIds)
	if err != nil || len(ids) > 100 {
		httpface.WriteErr(w, http.StatusBadRequest, "account_ids must contain 1-100 entries")
		return
	}
	for _, id := range ids {
		if id <= 0 {
			httpface.WriteErr(w, http.StatusBadRequest, "account_ids must be positive")
			return
		}
	}

	result := h.svc.RefreshCodexUsageBatch(r.Context(), ids)
	items := make([]CodexUsageBatchItem, 0, len(result.Items))
	for _, item := range result.Items {
		out := CodexUsageBatchItem{
			AccountId: item.AccountID,
			Status:    CodexUsageBatchItemStatus(item.Status),
		}
		if item.Message != "" {
			out.Message = ptr(item.Message)
		}
		if item.Usage != nil {
			converted, err := toAPICodexUsage(item.Usage)
			if err != nil {
				out.Status = CodexUsageBatchItemStatusFailed
				out.Message = ptr(err.Error())
			} else {
				out.Usage = &converted
			}
		}
		items = append(items, out)
	}
	httpface.WriteJSON(w, http.StatusOK, CodexUsageBatchResponse{Items: items})
}

func (h *AdminAPI) writeCodexUsage(w http.ResponseWriter, usage *service.CodexUsageResult) {
	out, err := toAPICodexUsage(usage)
	if err != nil {
		httpface.WriteErr(w, http.StatusBadGateway, err.Error())
		return
	}
	httpface.WriteJSON(w, http.StatusOK, out)
}

func toAPICodexUsage(in *service.CodexUsageResult) (CodexUsageResponse, error) {
	var usage any
	if err := json.Unmarshal(in.Usage, &usage); err != nil {
		return CodexUsageResponse{}, fmt.Errorf("invalid Codex usage payload: %w", err)
	}
	out := CodexUsageResponse{
		AccountId:      in.AccountID,
		CodexAccountId: in.CodexAccountID,
		Email:          in.Email,
		FetchedAt:      in.FetchedAt,
		Usage:          usage,
	}
	if len(in.ResetCredits) > 0 {
		var reset any
		if err := json.Unmarshal(in.ResetCredits, &reset); err != nil {
			return CodexUsageResponse{}, fmt.Errorf("invalid Codex reset credits payload: %w", err)
		}
		out.ResetCredits = &reset
	}
	if in.ResetCreditsError != "" {
		out.ResetCreditsError = ptr(in.ResetCreditsError)
	}
	return out, nil
}
