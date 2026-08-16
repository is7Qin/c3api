// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
)

// —— 账号类型化鉴权扩展（account_ext 1:1；codex 专用——installation_id/
// session_id/thread_id/window_id + oauth 组 + pat 组；未来 claude oauth 等
// 新类型 → 新增 ext_claude.go 同构） ——

// GetAccountsIdExt 账号类型化鉴权扩展（编辑回显；仅 codex-oauth/codex-pat
// 账号有 ext 行，ServerInterface）。账号缺 id / 无 ext 行 → 404。
func (h *AdminAPI) GetAccountsIdExt(w http.ResponseWriter, r *http.Request, id int64) {
	e, err := h.svc.GetAccountExt(r.Context(), id)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIAccountExt(e))
}

// PutAccountsIdExt 幂等写入账号类型化鉴权扩展（Create/Update 合一；全列更新
// 含 NULL 清空，ServerInterface）。credential_type ∈ {codex-oauth, codex-pat}；
// 身份四元组（installation/session/thread/window）首次写入缺省 → service 自动
// 生成并持久化（账号存在期间稳定）；类型-列组约束与账号缺 id → 400/404
// （service 校验）。
func (h *AdminAPI) PutAccountsIdExt(w http.ResponseWriter, r *http.Request, id int64) {
	var in AccountExt
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	e := &domain.AccountExt{
		AccountID:         id, // 路径 {id} 为准（请求体 account_id 忽略）
		CredentialType:    credential.Type(in.CredentialType),
		InstallationID:    deref(in.InstallationId), // 缺省 → service 自动生成
		SessionID:         in.SessionId,
		ThreadID:          in.ThreadId,
		WindowID:          in.WindowId,
		OAuthToken:        in.OauthToken,
		OAuthRefreshToken: in.OauthRefreshToken,
		OAuthExpiresAt:    in.OauthExpiresAt,
		PATKey:            in.PatKey,
		Email:             in.Email, // 管理面标识（人工/上游导入提供，可空）
	}
	saved, err := h.svc.UpsertAccountExt(r.Context(), e)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIAccountExt(saved))
}

// toAPIAccountExt 账号 ext 领域对象 → 契约类型（account_id 只读，响应带）。
func toAPIAccountExt(e *domain.AccountExt) AccountExt {
	return AccountExt{
		AccountId:         &e.AccountID,
		CredentialType:    AccountExtCredentialType(e.CredentialType),
		InstallationId:    ptr(e.InstallationID),
		SessionId:         e.SessionID,
		ThreadId:          e.ThreadID,
		WindowId:          e.WindowID,
		OauthToken:        e.OAuthToken,
		OauthRefreshToken: e.OAuthRefreshToken,
		OauthExpiresAt:    e.OAuthExpiresAt,
		PatKey:            e.PATKey,
		Email:             e.Email,
	}
}
