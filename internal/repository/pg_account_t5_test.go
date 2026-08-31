// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// ---------------------------------------------------------------------------
// SDK 接入 T5：轮转回写（WriteOAuthRotation 部分更新 upsert）真实 PG 测试 +
// 失效恢复（status→active 隐含清 failed_at + last_error）。基座见
// pg_account_groups_test.go 的 newPGRepos（DROP SCHEMA 重建）。
// ---------------------------------------------------------------------------

// seedPGOAuthExt 建 codex-oauth 模板 + 账号 + 带身份四元组与 oauth 凭据的
// ext 行（写入面全列：codex_identity/codex_pat_key/codex_email 断言不动）。
func seedPGOAuthExt(t *testing.T, repos *repository.Repository, name string, at, rt string, expires *time.Time) (*domain.Account, *domain.AccountExt) {
	t.Helper()
	ctx := context.Background()
	tpl, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "t-codex-" + name, BaseURL: "",
		CredentialType:   credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		ModelMapping:     domain.ModelMapping{},
	})
	require.NoError(t, err)
	acc, err := repos.Accounts.CreateAccount(ctx, &domain.Account{
		Name: name, TemplateID: tpl.ID, UpstreamKey: "sk-" + name, MaxConcurrency: 8,
	})
	require.NoError(t, err)
	ext := &domain.AccountExt{
		AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
		CodexIdentity: &domain.CodexIdentity{
			InstallationID: "inst-" + name,
			SessionID:      "sess-" + name, ThreadID: "th-" + name, WindowID: "th-" + name + ":0",
		},
		CodexOAuthToken: strPtrPG(at), CodexOAuthRefreshToken: strPtrPG(rt), CodexOAuthExpiresAt: expires,
		CodexEmail: strPtrPG("a@" + name + ".x"),
	}
	_, err = repos.AccountExts.UpsertAccountExt(ctx, ext)
	require.NoError(t, err)
	return acc, ext
}

// TestWriteOAuthRotationPG 轮转回写 roundtrip：部分更新 upsert 仅动
// codex_oauth_* 三列（codex_oauth_token/codex_oauth_refresh_token/
// codex_oauth_expires_at 保旧），其余列（codex_identity/codex_pat_key/
// codex_email）原样保留（防 UpsertAccountExt 全量 upsert 的 ClearX 清空
// 回归——P3-4 expiry 保旧断言）；幂等（重复回调收敛）；行缺失 → 报错。
func TestWriteOAuthRotationPG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	expires := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	acc, ext := seedPGOAuthExt(t, repos, "rot1", "at-old", "rt-old", &expires)

	// 回写（携带旧 expiry 保旧）
	require.NoError(t, repos.AccountExts.WriteOAuthRotation(ctx, acc.ID, "at-new", "rt-new", &expires))

	got, err := repos.AccountExts.GetAccountExt(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, "at-new", *got.CodexOAuthToken)
	require.Equal(t, "rt-new", *got.CodexOAuthRefreshToken)
	require.NotNil(t, got.CodexOAuthExpiresAt, "expiry 保旧——不得被 ClearX 清空")
	require.True(t, got.CodexOAuthExpiresAt.Equal(expires), "codex_oauth_expires_at 写回后不变（防 ClearX 清空回归）")
	// 其余列原样（部分更新不触碰）
	require.Equal(t, ext.CodexIdentity.InstallationID, got.CodexIdentity.InstallationID)
	require.Equal(t, ext.CodexIdentity.SessionID, got.CodexIdentity.SessionID)
	require.Equal(t, ext.CodexIdentity.ThreadID, got.CodexIdentity.ThreadID)
	require.Equal(t, ext.CodexIdentity.WindowID, got.CodexIdentity.WindowID)
	require.Equal(t, *ext.CodexEmail, *got.CodexEmail)

	// 幂等：重复回调（D4 重试投递同一 (at, rt)）→ 收敛不报错
	require.NoError(t, repos.AccountExts.WriteOAuthRotation(ctx, acc.ID, "at-new", "rt-new", &expires))
	got2, err := repos.AccountExts.GetAccountExt(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, "at-new", *got2.CodexOAuthToken)
	require.Equal(t, "rt-new", *got2.CodexOAuthRefreshToken)
	require.True(t, got2.CodexOAuthExpiresAt.Equal(expires))
}

// TestWriteOAuthRotationNilExpiryPG 保旧语义的 nil 形态：携带 nil expiry 回写
// → codex_oauth_expires_at 写 NULL（与"未知过期"语义一致）。
func TestWriteOAuthRotationNilExpiryPG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	expires := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	acc, _ := seedPGOAuthExt(t, repos, "rot2", "at-old", "rt-old", &expires)

	require.NoError(t, repos.AccountExts.WriteOAuthRotation(ctx, acc.ID, "at-new", "rt-new", nil))
	got, err := repos.AccountExts.GetAccountExt(ctx, acc.ID)
	require.NoError(t, err)
	require.Nil(t, got.CodexOAuthExpiresAt, "nil expiry → 写 NULL（保旧携带值语义）")
	require.Equal(t, "at-new", *got.CodexOAuthToken)
}

// TestWriteOAuthRotationMissingRowPG 行缺失（配置损坏——codex 账号必有 ext
// 行）→ 报错（INSERT 路径缺必填列）——D4 回调重试链接管（fail-closed）。
func TestWriteOAuthRotationMissingRowPG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	tpl, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "t-rot3", BaseURL: "", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		ModelMapping: domain.ModelMapping{},
	})
	require.NoError(t, err)
	acc, err := repos.Accounts.CreateAccount(ctx, &domain.Account{
		Name: "rot3", TemplateID: tpl.ID, UpstreamKey: "sk-rot3", MaxConcurrency: 8,
	})
	require.NoError(t, err)
	// 无 ext 行
	require.Error(t, repos.AccountExts.WriteOAuthRotation(ctx, acc.ID, "at", "rt", nil),
		"行缺失必须报错——令牌无法持久化 = D4 失败信号")
}
