// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// TestSelectionCredentialCarriesSourceRevision 请求侧凭据投影必须与**来源身份代际 K**
// 同源（§3.3b-2）：K 在认领租借时冻结，凭据内容与它来自同一份快照读。事件回流
// （fatal / 构造失败 / 轮转回写）以这个 K 作 expectedRevision——SDK 侧因此不再需要
// "在施加前回读当前 K"。
func TestSelectionCredentialCarriesSourceRevision(t *testing.T) {
	tplOAuth := tpl(10, domain.FormatOpenAIResponses, []string{"m"})
	tplOAuth.CredentialType = credential.TypeCodexOAuth
	a := acc(1, tplOAuth, 2)
	a.IdentityRevision = 7 // 管理面已做过若干身份写入
	a.Ext = &domain.AccountExt{
		AccountID:              1,
		CredentialType:         credential.TypeCodexOAuth,
		CodexIdentity:          &domain.CodexIdentity{InstallationID: "inst"},
		CodexAccountID:         new("upstream-acct"),
		CodexOAuthToken:        new("at-1"),
		CodexOAuthRefreshToken: new("rt-1"),
	}
	s := newTestScheduler(t, []*domain.Account{a})

	sel, err := s.Select(10, domain.FormatOpenAIResponses, "m")
	require.NoError(t, err)
	defer sel.Release()

	cred := sel.Credential()
	require.Equal(t, int64(7), cred.IdentityRevision, "凭据投影携带来源 K")
	require.Equal(t, int64(1), cred.AccountID)
	require.Equal(t, "at-1", cred.OAuthToken)
	require.Equal(t, "rt-1", cred.OAuthRefreshToken)
	require.Equal(t, "upstream-acct", cred.CodexAccountID)
	require.Equal(t, "inst", cred.InstallationID)
	require.Equal(t, cred.IdentityRevision, sel.IdentityRevision())

	// 未取得租借的 Selection（非选号调用面）没有来源版本 ⇒ 0（"来源未知"）。
	require.Zero(t, (&Selection{}).IdentityRevision())
	require.Zero(t, (&Selection{}).Credential().IdentityRevision)
}
