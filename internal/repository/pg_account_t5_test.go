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
		Name: "t-codex-" + name, BaseURL: "https://u/v1",
		CredentialType:   credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
	})
	require.NoError(t, err)
	acc, err := repos.Accounts.CreateAccount(ctx, &domain.Account{
		Name: name, TemplateID: tpl.ID, UpstreamKey: "sk-" + name, Weight: 1, MaxConcurrency: 8,
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
	repos := newPGRepos(t)
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
	repos := newPGRepos(t)
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
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "t-rot3", BaseURL: "https://u/v1", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
	})
	require.NoError(t, err)
	acc, err := repos.Accounts.CreateAccount(ctx, &domain.Account{
		Name: "rot3", TemplateID: tpl.ID, UpstreamKey: "sk-rot3", Weight: 1, MaxConcurrency: 8,
	})
	require.NoError(t, err)
	// 无 ext 行
	require.Error(t, repos.AccountExts.WriteOAuthRotation(ctx, acc.ID, "at", "rt", nil),
		"行缺失必须报错——令牌无法持久化 = D4 失败信号")
}

// TestAccountRecoveryClearsFailedPG 失效恢复（T5 §2，P2-2 定死方案 b——管理面
// status→active 隐含清双字段）：UpdateAccount / UpdateAccountsBatch /
// UpdateAccountStatus 三条写路径 status→active → failed_at + last_error 双清
// （P3-4 恢复断言）；非 active 状态 → 字段保留（不误清）。
func TestAccountRecoveryClearsFailedPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	acc, _ := seedPGOAuthExt(t, repos, "rec1", "at", "rt", nil)

	// 预置失效（T1 SetAccountFailed 语义：failed_at + last_error）
	when := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	require.NoError(t, repos.Accounts.SetAccountFailed(ctx, acc.ID, when, "auth permanently revoked"))
	check := func(t *testing.T, wantFailed bool) {
		t.Helper()
		got, err := repos.Accounts.GetAccount(ctx, acc.ID)
		require.NoError(t, err)
		if wantFailed {
			require.NotNil(t, got.FailedAt, "failed_at 保留")
			require.NotNil(t, got.LastError, "last_error 保留")
		} else {
			require.Nil(t, got.FailedAt, "failed_at 双清")
			require.Nil(t, got.LastError, "last_error 双清（P3-4 恢复断言）")
			require.Equal(t, domain.StatusActive, got.Status)
		}
	}
	check(t, true)

	// 路径 1：UpdateAccount（单条 PUT）status→active → 双清
	cur, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	cur.Status = domain.StatusActive
	_, err = repos.Accounts.UpdateAccount(ctx, cur)
	require.NoError(t, err)
	check(t, false)

	// 再次失效 → 非 active 状态不误清
	require.NoError(t, repos.Accounts.SetAccountFailed(ctx, acc.ID, when, "boom again"))
	cur2, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	cur2.Status = domain.StatusDisabled
	_, err = repos.Accounts.UpdateAccount(ctx, cur2)
	require.NoError(t, err)
	check2 := func(t *testing.T) {
		t.Helper()
		got, err := repos.Accounts.GetAccount(ctx, acc.ID)
		require.NoError(t, err)
		require.NotNil(t, got.FailedAt, "disabled 状态不触发恢复——failed_at 保留")
		require.NotNil(t, got.LastError, "last_error 保留")
		require.Equal(t, domain.StatusDisabled, got.Status)
	}
	check2(t)

	// 路径 2：UpdateAccountsBatch（批量 status→active）→ 双清
	st := domain.StatusActive
	require.NoError(t, repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{Status: &st}))
	check(t, false)

	// 批量非 active 不误清
	require.NoError(t, repos.Accounts.SetAccountFailed(ctx, acc.ID, when, "boom 3"))
	st2 := domain.Status429
	require.NoError(t, repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{Status: &st2}))
	gotB, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.NotNil(t, gotB.FailedAt, "批量 429 不触发恢复")
	require.Equal(t, domain.Status429, gotB.Status)

	// 路径 3：UpdateAccountStatus（调度回写路径）status→active → failed_at 清除
	require.NoError(t, repos.Accounts.UpdateAccountStatus(ctx, acc.ID, domain.StatusActive, nil, nil, nil))
	gotC, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Nil(t, gotC.FailedAt, "调度回写 active → failed_at 清除（ClearLastError 同处扩展）")
	require.Nil(t, gotC.LastError)
	require.Equal(t, domain.StatusActive, gotC.Status)
}
