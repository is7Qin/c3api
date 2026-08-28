// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"time"

	"github.com/is7qin/c3api/internal/credential"
)

// AccountCredential 网关侧凭据传递形态（SDK 接入契约，T1 §4）：每次调用必传
// （调度器按请求选账号）。从 AccountExt 派生（CredentialFromExt）——codex-oauth
// → OAuthToken/OAuthRefreshToken/OAuthExpiresAt；codex-pat → PATKey。适配层
// （T2 起）把 cred 转成 SDK Auth 账号级缓存键：同账号复用、失效剔除。Codex
// 数据面 URL 归 SDK 官方默认所有，网关不再派生或传递 BaseURL。
type AccountCredential struct {
	AccountID         int64
	OAuthToken        string
	OAuthRefreshToken string
	OAuthExpiresAt    *time.Time
	PATKey            string
}

// CredentialFromExt 从账号扩展行派生每次调用必传的凭据（投影语义：按类型取
// 对应凭据列组——codex-oauth → codex_oauth_* 列组、codex-pat → codex_pat_key；
// nil 列 → 空值；不报错——调用方按类型分流，非本类型的列不触达）。nil ext →
// 全零值。
func CredentialFromExt(e *AccountExt) AccountCredential {
	if e == nil {
		return AccountCredential{}
	}
	c := AccountCredential{AccountID: e.AccountID}
	switch e.CredentialType {
	case credential.TypeCodexOAuth:
		if e.CodexOAuthToken != nil {
			c.OAuthToken = *e.CodexOAuthToken
		}
		if e.CodexOAuthRefreshToken != nil {
			c.OAuthRefreshToken = *e.CodexOAuthRefreshToken
		}
		c.OAuthExpiresAt = e.CodexOAuthExpiresAt
	case credential.TypeCodexPAT:
		if e.CodexPATKey != nil {
			c.PATKey = *e.CodexPATKey
		}
	}
	return c
}
