// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
)

// —— codex 凭据批量导入（Task B：batch-import-codex-oauth / batch-import-codex-pat；
// 契约层——结构校验（items ≤100 原始条数 / template_id 必填 → 400）+ 响应组装；
// 行级校验/落库在 service 共享核心） ——

// PostAccountsBatchImportCodexOauth 批量导入 codex-oauth 凭据（ServerInterface）。
// 结构错误（items 空/超 100、template_id 缺）→ 400；行级失败归 failed（HTTP 恒
// 200——行级语义，全部失败也 200）。
func (h *AdminAPI) PostAccountsBatchImportCodexOauth(w http.ResponseWriter, r *http.Request) {
	var in CodexOAuthImportBody
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if len(in.Items) == 0 || len(in.Items) > 100 {
		httpface.WriteErr(w, http.StatusBadRequest, "items must contain 1-100 entries")
		return
	}
	items := make([]domain.CodexOAuthImportItem, len(in.Items))
	for i, it := range in.Items {
		items[i] = domain.CodexOAuthImportItem{
			CodexEmail:             it.CodexEmail,
			CodexAccountID:         it.CodexAccountId,
			CodexOAuthToken:        it.CodexOauthToken,
			CodexOAuthRefreshToken: it.CodexOauthRefreshToken,
			CodexOAuthExpiresAt:    it.CodexOauthExpiresAt,
			MaxConcurrency:         it.MaxConcurrency,
			Weight:                 it.Weight,
		}
	}
	res, err := h.svc.ImportCodexOAuthAccounts(r.Context(), items, &in.TemplateId, in.GroupId)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIImportResult(res))
}

// PostAccountsBatchImportCodexPat 批量导入 codex-pat 凭据（ServerInterface；
// 结构校验与响应组装同 oauth 端点）。
func (h *AdminAPI) PostAccountsBatchImportCodexPat(w http.ResponseWriter, r *http.Request) {
	var in CodexPATImportBody
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if len(in.Items) == 0 || len(in.Items) > 100 {
		httpface.WriteErr(w, http.StatusBadRequest, "items must contain 1-100 entries")
		return
	}
	items := make([]domain.CodexPATImportItem, len(in.Items))
	for i, it := range in.Items {
		items[i] = domain.CodexPATImportItem{
			CodexEmail:     it.CodexEmail,
			CodexAccountID: it.CodexAccountId,
			CodexPATKey:    it.CodexPatKey,
			MaxConcurrency: it.MaxConcurrency,
			Weight:         it.Weight,
		}
	}
	res, err := h.svc.ImportCodexPATAccounts(r.Context(), items, &in.TemplateId, in.GroupId)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIImportResult(res))
}

// toAPIImportResult 领域结果 → 契约类型（行级 failed 直透 index/error）。
func toAPIImportResult(res *domain.ImportResult) ImportResult {
	out := ImportResult{Imported: res.Imported, Updated: res.Updated}
	out.Failed = make([]ImportFailedItem, 0, len(res.Failed))
	for _, f := range res.Failed {
		out.Failed = append(out.Failed, ImportFailedItem{Index: f.Index, Error: f.Error})
	}
	return out
}
