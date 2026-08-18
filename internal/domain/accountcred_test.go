// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
)

func strP(s string) *string { return &s }

// CredentialFromExt 派生单测（T1 §4：AccountExt → AccountCredential 两形态投影）。
func TestCredentialFromExt(t *testing.T) {
	exp := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

	t.Run("codex-oauth 取 oauth 列组", func(t *testing.T) {
		e := &AccountExt{
			AccountID: 7, CredentialType: credential.TypeCodexOAuth,
			CodexOAuthToken: strP("at"), CodexOAuthRefreshToken: strP("rt"), CodexOAuthExpiresAt: &exp,
			CodexPATKey: strP("patshould-not-leak"),
		}
		c := CredentialFromExt(e)
		require.Equal(t, int64(7), c.AccountID)
		require.Equal(t, "at", c.OAuthToken)
		require.Equal(t, "rt", c.OAuthRefreshToken)
		require.Equal(t, exp, *c.OAuthExpiresAt)
		require.Empty(t, c.PATKey, "oauth 类型不得投影 pat 列")
	})

	t.Run("codex-pat 取 pat 列组", func(t *testing.T) {
		e := &AccountExt{
			AccountID: 8, CredentialType: credential.TypeCodexPAT,
			CodexPATKey:     strP("pk"),
			CodexOAuthToken: strP("at-should-not-leak"),
		}
		c := CredentialFromExt(e)
		require.Equal(t, int64(8), c.AccountID)
		require.Equal(t, "pk", c.PATKey)
		require.Empty(t, c.OAuthToken, "pat 类型不得投影 oauth 列")
		require.Empty(t, c.OAuthRefreshToken)
		require.Nil(t, c.OAuthExpiresAt)
	})

	t.Run("oauth nil 列投影为空值", func(t *testing.T) {
		e := &AccountExt{AccountID: 9, CredentialType: credential.TypeCodexOAuth}
		c := CredentialFromExt(e)
		require.Equal(t, int64(9), c.AccountID)
		require.Empty(t, c.OAuthToken)
		require.Empty(t, c.OAuthRefreshToken)
		require.Nil(t, c.OAuthExpiresAt)
	})

	t.Run("nil ext → 全零值", func(t *testing.T) {
		c := CredentialFromExt(nil)
		require.Zero(t, c)
	})

	t.Run("非 codex 类型 → 全空（调用方按类型分流不触达）", func(t *testing.T) {
		e := &AccountExt{AccountID: 10, CredentialType: credential.TypeResponsesSpecial}
		c := CredentialFromExt(e)
		require.Equal(t, int64(10), c.AccountID)
		require.Empty(t, c.OAuthToken)
		require.Empty(t, c.PATKey)
	})
}
