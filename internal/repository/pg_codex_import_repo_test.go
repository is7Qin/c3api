// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// ---------------------------------------------------------------------------
// codex 批量导入 repo 面真实 PG：FindAccountExtByCodexKey 组合键查重
// roundtrip + 缺行 ErrNotFound。
// ---------------------------------------------------------------------------

// TestFindAccountExtByCodexKeyPG 组合键查重：双条件命中（含 credential_type
// 跨类型判定材料）；缺行 → ErrNotFound；组合键语义（同 email 多 account_id /
// 同 account_id 多 email 互不误命中）。
func TestFindAccountExtByCodexKeyPG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)

	seed := func(name, email string, accID *string, ct credential.Type) int64 {
		t.Helper()
		acc := seedPGAccount(t, repos, tpl.ID, name)
		e := &domain.AccountExt{
			AccountID: acc.ID, CredentialType: ct,
			CodexIdentity: &domain.CodexIdentity{InstallationID: "i-" + name},
			CodexEmail:    strPtrPG(email), CodexAccountID: accID,
		}
		if ct == credential.TypeCodexOAuth {
			e.CodexOAuthToken = strPtrPG("at")
		} else {
			e.CodexPATKey = strPtrPG("pat")
		}
		_, err := repos.AccountExts.UpsertAccountExt(ctx, e)
		require.NoError(t, err)
		return acc.ID
	}

	// 同 email 双 account_id（组合键语义——不误命中）
	seed("k1", "k@example.com", strPtrPG("ka-1"), credential.TypeCodexOAuth)
	seed("k2", "k@example.com", strPtrPG("ka-2"), credential.TypeCodexOAuth)

	got, err := repos.AccountExts.FindAccountExtByCodexKey(ctx, "k@example.com", "ka-1")
	require.NoError(t, err)
	require.Equal(t, "k@example.com", *got.CodexEmail)
	require.Equal(t, "ka-1", *got.CodexAccountID)
	require.Equal(t, credential.TypeCodexOAuth, got.CredentialType, "命中行含 credential_type（跨类型判定用）")
	require.Equal(t, "at", *got.CodexOAuthToken)

	got2, err := repos.AccountExts.FindAccountExtByCodexKey(ctx, "k@example.com", "ka-2")
	require.NoError(t, err)
	require.Equal(t, "ka-2", *got2.CodexAccountID, "同 email 多 account_id 精确命中各自行")

	// 缺行 → ErrNotFound
	_, err = repos.AccountExts.FindAccountExtByCodexKey(ctx, "k@example.com", "ka-3")
	require.Error(t, err)
	require.True(t, errors.Is(err, repository.ErrNotFound), "缺行 → ErrNotFound: %v", err)

	// 同 account_id 多 email（组合键另一维度）——pat 行共存
	seed("k3", "k3@example.com", strPtrPG("kb-1"), credential.TypeCodexPAT)
	got3, err := repos.AccountExts.FindAccountExtByCodexKey(ctx, "k3@example.com", "kb-1")
	require.NoError(t, err)
	require.Equal(t, credential.TypeCodexPAT, got3.CredentialType, "pat 行命中")
	_, err = repos.AccountExts.FindAccountExtByCodexKey(ctx, "k@example.com", "kb-1")
	require.True(t, errors.Is(err, repository.ErrNotFound), "email 维度不同不误命中")
}
