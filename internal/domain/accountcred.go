// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"time"

	"github.com/is7qin/c3api/internal/credential"
)

// AccountCredential 网关侧凭据传递形态（SDK 接入契约，§4）：每次调用必传
// （调度器按请求选账号）。从 AccountExt 派生（CredentialFromExt）——codex-oauth
// → OAuthToken/OAuthRefreshToken/OAuthExpiresAt；codex-pat → PATKey。适配层
// （此后）把 cred 转成 SDK Auth 账号级缓存键：同账号复用、失效剔除。Codex
// 数据面 URL 归 SDK 官方默认所有，网关不再派生或传递 BaseURL。
type AccountCredential struct {
	AccountID         int64
	OAuthToken        string
	OAuthRefreshToken string
	OAuthExpiresAt    *time.Time
	PATKey            string
	// CodexAccountID 上游账号/空间标识（ChatGPT-Account-ID 头——SDK 内部注入面；
	// 空 = 不发头，向后兼容）。落库点定值（导入行/单账号保存派生），热路径只读。
	CodexAccountID string
	// InstallationID 账号安装级稳定标识（codex_identity.installation_id 的投影）。
	// 它与会话的 installation 同源，是"这份凭据还是不是同一版账号身份"的判据之一
	// （sdkbridge.CredOwner）；不属于凭据本身，故不参与重建签名（credKey）。
	InstallationID string
	// IdentityRevision **来源身份代际 K**（account.identity_revision 的投影，与上面
	// 各列**同一次快照读**）。凭据事件（判死/构造失败/轮转回写）必须带着"产生它的
	// 那一版身份"走到持久化围栏：K 只由管理面身份类写入推进（SDK 自刷新不动），
	// 所以"来源 K = 当前 K"才是"这份判决仍然生效"的判据。
	//
	// 0 = 来源未知（装配缺陷）：拿不到来源版本的事件不得退化去读"当前"——那正是
	// "旧凭据的判决禁用新凭据"的来源。消费方（sdkbridge）对 0 拒绝账号级动作。
	IdentityRevision int64
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
	// 账号标识 codex 两类型统一投影（非 codex 类型不带——调用方按类型分流，
	// 非本类型的列不触达）。
	if e.CredentialType.IsCodex() {
		if e.CodexAccountID != nil {
			c.CodexAccountID = *e.CodexAccountID
		}
		if e.CodexIdentity != nil {
			c.InstallationID = e.CodexIdentity.InstallationID
		}
	}
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
