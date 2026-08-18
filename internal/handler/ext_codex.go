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

// —— 账号类型化鉴权扩展（account_ext 1:1；codex 专用——codex_identity jsonb
// 四元组 + codex_oauth_* 组 + codex_pat_key 组；未来 claude oauth 等新类型 →
// 新增 ext_claude.go 同构） ——

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
// 身份四元组（codex_identity 对象——installation/session/thread/window）首次
// 写入缺省 → service 自动生成并持久化（账号存在期间稳定）；类型-列组约束与
// 账号缺 id → 400/404（service 校验）。
//
// 请求体空值语义（显式空串统一——breaking 内契约）：
//   - 凭据/管理列（codex_oauth_* / codex_pat_key / codex_email /
//     codex_account_id）：null/缺省 = NULL 清空落库；空串 = 字面空值写入
//     （两者区分——空串是合法值，不归一 NULL）；
//   - codex_identity 对象字段：null 与空串同语义 = 未提供（handler 归一空串，
//     service 自动生成/沿用——identity 无清空路径，恒等约束由 service 维护）。
func (h *AdminAPI) PutAccountsIdExt(w http.ResponseWriter, r *http.Request, id int64) {
	var in AccountExt
	if err := decodeLenient(r, &in); err != nil { // ext 专用宽松解码（忽略未知字段，见 ext.go）
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	e := &domain.AccountExt{
		AccountID:              id, // 路径 {id} 为准（请求体 account_id 忽略）
		CredentialType:         credential.Type(in.CredentialType),
		CodexOAuthToken:        in.CodexOauthToken,
		CodexOAuthRefreshToken: in.CodexOauthRefreshToken,
		CodexOAuthExpiresAt:    in.CodexOauthExpiresAt,
		CodexPATKey:            in.CodexPatKey,
		CodexEmail:             in.CodexEmail, // 管理面标识（人工/上游导入提供，可空）
		CodexAccountID:         in.CodexAccountId,
	}
	if in.CodexIdentity != nil {
		// 身份对象缺省/空字段 → 空串 = 未提供 → service 自动生成/沿用（语义保持）
		e.CodexIdentity = &domain.CodexIdentity{
			InstallationID: deref(in.CodexIdentity.InstallationId),
			SessionID:      deref(in.CodexIdentity.SessionId),
			ThreadID:       deref(in.CodexIdentity.ThreadId),
			WindowID:       deref(in.CodexIdentity.WindowId),
		}
	}
	saved, err := h.svc.UpsertAccountExt(r.Context(), e)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIAccountExt(saved))
}

// toAPIAccountExt 账号 ext 领域对象 → 契约类型（account_id 只读，响应带）。
// 响应 null/省略形态（与生成契约 json tag 实际行为对齐）：
//   - codex_identity 对象：nil → **省略**（omitempty，不输出 null）；非 nil →
//     对象投影（installation_id 恒输出；session/thread/window 空 → **null**——
//     无 omitempty，与契约 nullable: true 一致）；
//   - 凭据/管理列（codex_oauth_* 等）：nil → **null**（无 omitempty）。
func toAPIAccountExt(e *domain.AccountExt) AccountExt {
	out := AccountExt{
		AccountId:              &e.AccountID,
		CredentialType:         AccountExtCredentialType(e.CredentialType),
		CodexOauthToken:        e.CodexOAuthToken,
		CodexOauthRefreshToken: e.CodexOAuthRefreshToken,
		CodexOauthExpiresAt:    e.CodexOAuthExpiresAt,
		CodexPatKey:            e.CodexPATKey,
		CodexEmail:             e.CodexEmail,
		CodexAccountId:         e.CodexAccountID,
	}
	if e.CodexIdentity != nil {
		id := e.CodexIdentity
		out.CodexIdentity = &CodexIdentity{InstallationId: &id.InstallationID}
		if id.SessionID != "" {
			out.CodexIdentity.SessionId = &id.SessionID
		}
		if id.ThreadID != "" {
			out.CodexIdentity.ThreadId = &id.ThreadID
		}
		if id.WindowID != "" {
			out.CodexIdentity.WindowId = &id.WindowID
		}
	}
	return out
}
