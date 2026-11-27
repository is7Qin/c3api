// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/httpface"
)

// —— codex 凭据批量导入（batch-import-codex-oauth / batch-import-codex-pat；
// 契约层——结构校验（items ≤100 原始条数 / template_id 必填 → 400）+ 响应组装；
// 行级校验/落库在 service 共享核心） ——

// PostAccountsBatchImportCodexOauth 批量导入 codex-oauth 凭据（ServerInterface）。
// 结构错误（items 空/超 100、template_id 缺）→ 400；行级失败归 failed（HTTP 恒
// 200——行级语义，全部失败也 200）。
func (h *AdminAPI) PostAccountsBatchImportCodexOauth(w http.ResponseWriter, r *http.Request) {
	raw, err := httpface.DecodeRaw(r)
	if err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	var in CodexOAuthImportBody
	if err := httpface.Decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if len(in.Items) == 0 || len(in.Items) > 100 {
		httpface.WriteErr(w, http.StatusBadRequest, "items must contain 1-100 entries")
		return
	}
	// 归属（§2.5）：供应商面导入体出现该字段（**含显式 null**）⇒ 400；归属恒为
	// JWT 本人。普通指针把 null 与缺席都归零值，故判据取**原始键存在性**（不是
	// `!= nil`——那会让 `"supplier_user_id": null` 漏过）。
	if err := rejectSupplierUserIDOnSupplierSurface(r.Context(), raw, "supplier_user_id is not writable on the supplier surface"); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	items := make([]domain.CodexOAuthImportItem, len(in.Items))
	for i, it := range in.Items {
		accountID := it.CodexAccountId
		if accountID == "" {
			// account id 缺省补全：离线解 JWT claims（派生策略唯一实现在
			// codex_account_id.go 的 deriveAccountID；空 token 由纯函数短路返回空，
			// 故此处不必前置判空。仍空 → service 行级必填校验照常 failed）。
			accountID = h.deriveAccountID(r.Context(), credential.TypeCodexOAuth, it.CodexOauthToken, "")
		}
		items[i] = domain.CodexOAuthImportItem{
			CodexEmail:             it.CodexEmail,
			CodexAccountID:         accountID,
			CodexOAuthToken:        it.CodexOauthToken,
			CodexOAuthRefreshToken: it.CodexOauthRefreshToken,
			CodexOAuthExpiresAt:    it.CodexOauthExpiresAt,
			MaxConcurrency:         it.MaxConcurrency,
		}
	}
	res, err := h.svc.ImportCodexOAuthAccounts(r.Context(), items, &in.TemplateId, in.GroupId, codexImportConfigFromBody(in.Enabled, in.CacheDomain, in.UpstreamCostMultiplier, in.ProxyId))
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIImportResult(res))
}

// PostAccountsBatchImportCodexPat 批量导入 codex-pat 凭据（ServerInterface；
// 结构校验与响应组装同 oauth 端点）。
func (h *AdminAPI) PostAccountsBatchImportCodexPat(w http.ResponseWriter, r *http.Request) {
	raw, err := httpface.DecodeRaw(r)
	if err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	var in CodexPATImportBody
	if err := httpface.Decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if len(in.Items) == 0 || len(in.Items) > 100 {
		httpface.WriteErr(w, http.StatusBadRequest, "items must contain 1-100 entries")
		return
	}
	// 归属（§2.5）：同 oauth 端点——出现（含显式 null）⇒ 400。
	if err := rejectSupplierUserIDOnSupplierSurface(r.Context(), raw, "supplier_user_id is not writable on the supplier surface"); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	items := make([]domain.CodexPATImportItem, len(in.Items))
	for i, it := range in.Items {
		accountID := it.CodexAccountId
		if accountID == "" {
			// account id 缺省补全：在线 whoami（同一个 deriveAccountID；resolver 未装配 /
			// 空 key → 留空，不阻塞导入）。失败/仍空 → service 行级必填校验照常 failed）。
			accountID = h.deriveAccountID(r.Context(), credential.TypeCodexPAT, "", it.CodexPatKey)
		}
		items[i] = domain.CodexPATImportItem{
			CodexEmail:     it.CodexEmail,
			CodexAccountID: accountID,
			CodexPATKey:    it.CodexPatKey,
			MaxConcurrency: it.MaxConcurrency,
		}
	}
	res, err := h.svc.ImportCodexPATAccounts(r.Context(), items, &in.TemplateId, in.GroupId, codexImportConfigFromBody(in.Enabled, in.CacheDomain, in.UpstreamCostMultiplier, in.ProxyId))
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIImportResult(res))
}

// rejectSupplierUserIDOnSupplierSurface 供应商面导入体的归属字段拒绝判据
// （§2.5 行「导入体出现 ⇒ 400」）：**原始键存在性**判据（`has`），不是值判据
// ——生成类型 `*int64` 无法区分「键缺席」与「键为 null」，`!= nil` 会让显式
// null 漏过（spec I4）。管理面（无作用域）不受此判据约束：该字段在那里是
// 「保留但无效」，归属照旧由管理面单建/批改分配。
func rejectSupplierUserIDOnSupplierSurface(ctx context.Context, raw []byte, msg string) error {
	if !domain.AccountScopeFrom(ctx).Set {
		return nil
	}
	has, err := httpface.HasJSONKey(raw, "supplier_user_id")
	if err != nil {
		return errors.New("invalid json: " + err.Error())
	}
	if has {
		return errors.New(msg)
	}
	return nil
}

// codexImportConfigFromBody body 级账号配置 → 领域配置（倍率走 normalToMult
// 换算为 basis points；nil 透传 = 取创建默认）。语义判定（边界/域名语法/proxy_id
// 值域）不在此处——唯一权威是 service.validateCodexImportConfig。
func codexImportConfigFromBody(enabled *bool, cacheDomain *string, mult *float64, proxyID *int64) domain.CodexImportConfig {
	cfg := domain.CodexImportConfig{Enabled: enabled, CacheDomain: cacheDomain, ProxyID: proxyID}
	if mult != nil {
		bp := normalToMult(*mult)
		cfg.UpstreamCostMultiplierBp = &bp
	}
	return cfg
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
