// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// seedCodexAccountWithExt 建一个 codex-oauth 账号 + ext 行（身份面齐备）。
func seedCodexAccountWithExt(t *testing.T, repos *repository.Repository, name string) (*domain.Account, *domain.AccountExt) {
	t.Helper()
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	acc, err := repos.Accounts.CreateAccount(ctx, &domain.Account{
		Name: name, TemplateID: tpl.ID, UpstreamKey: "sk-1", MaxConcurrency: 8, Enabled: true,
	})
	require.NoError(t, err)
	ext := &domain.AccountExt{
		AccountID:       acc.ID,
		CredentialType:  "codex-oauth",
		CodexIdentity:   &domain.CodexIdentity{InstallationID: "i1", SessionID: "s1", ThreadID: "s1", WindowID: "s1:0"},
		CodexOAuthToken: strPtr("tok-1"), CodexOAuthRefreshToken: strPtr("rt-1"),
		CodexEmail: strPtr("a@example.com"), CodexAccountID: strPtr("ca-1"),
	}
	_, err = repos.AccountExts.UpsertAccountExt(ctx, ext)
	require.NoError(t, err)
	return acc, ext
}

// TestPGAdminCredentialWriteAdvancesIdentityOnlyOnValueChange 钉住 spec §3.5 的
// ext 写面语义：管理面凭据写入**无条件**推进 C，K 只在凭据面**按值变更**时推进
// （幂等重写不推进）；SDK 自动刷新（WriteOAuthRotation）两个计数器都不动——
// "刷新同一账号的令牌"不是身份写入。
func TestPGAdminCredentialWriteAdvancesIdentityOnlyOnValueChange(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	acc, _ := seedCodexAccountWithExt(t, repos, "ext-identity")

	// 1) SDK 自动刷新：不触 accounts 行 ⇒ C/K 均不动。
	require.NoError(t, repos.AccountExts.WriteOAuthRotation(ctx, acc.ID, "tok-sdk", "rt-sdk", nil))
	afterSDK, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), afterSDK.LifecycleRevision, "SDK refresh must not advance C")
	require.Equal(t, int64(1), afterSDK.IdentityRevision, "SDK refresh must not advance K")

	// 2) 管理面 OAuth 轮转，凭据面**逐值相同**（幂等重写）：只推进 C。
	require.NoError(t, repos.AccountExts.AdminWriteOAuthRotationCAS(ctx, acc.ID, 1, "tok-sdk", "rt-sdk", nil))
	idem, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, int64(2), idem.LifecycleRevision, "admin write always advances C")
	require.Equal(t, int64(1), idem.IdentityRevision, "idempotent credential rewrite must not advance K")

	// 3) 管理面 OAuth 轮转，token 真的换了：C 与 K 同时推进。
	require.NoError(t, repos.AccountExts.AdminWriteOAuthRotationCAS(ctx, acc.ID, 2, "tok-rotated", "rt-rotated", nil))
	rotated, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, int64(3), rotated.LifecycleRevision)
	require.Equal(t, int64(2), rotated.IdentityRevision, "credential replacement is an identity write")

	// 4) 全量 PUT 逐值相同（幂等）：只推进 C。
	_, err = repos.AccountExts.AdminUpsertAccountExtCAS(ctx, &domain.AccountExt{
		AccountID: acc.ID, CredentialType: "codex-oauth",
		CodexIdentity:   &domain.CodexIdentity{InstallationID: "i1", SessionID: "s1", ThreadID: "s1", WindowID: "s1:0"},
		CodexOAuthToken: strPtr("tok-rotated"), CodexOAuthRefreshToken: strPtr("rt-rotated"),
		CodexEmail: strPtr("a@example.com"), CodexAccountID: strPtr("ca-1"),
	}, 3)
	require.NoError(t, err)
	idemUpsert, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, int64(4), idemUpsert.LifecycleRevision)
	require.Equal(t, int64(2), idemUpsert.IdentityRevision, "idempotent PUT must not advance K")

	// 5) 全量 PUT 改身份四元组：C 与 K 同时推进。
	_, err = repos.AccountExts.AdminUpsertAccountExtCAS(ctx, &domain.AccountExt{
		AccountID: acc.ID, CredentialType: "codex-oauth",
		CodexIdentity:   &domain.CodexIdentity{InstallationID: "i2", SessionID: "s2", ThreadID: "s2", WindowID: "s2:0"},
		CodexOAuthToken: strPtr("tok-rotated"), CodexOAuthRefreshToken: strPtr("rt-rotated"),
		CodexEmail: strPtr("a@example.com"), CodexAccountID: strPtr("ca-1"),
	}, 4)
	require.NoError(t, err)
	newIdentity, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, int64(5), newIdentity.LifecycleRevision)
	require.Equal(t, int64(3), newIdentity.IdentityRevision, "identity quadruple change is an identity write")

	// 6) PAT 轮转端点：值变 ⇒ K 推进；值不变 ⇒ 只推进 C。
	require.NoError(t, repos.AccountExts.AdminWritePATKeyCAS(ctx, acc.ID, 5, "pat-1"))
	afterPAT, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, int64(6), afterPAT.LifecycleRevision)
	require.Equal(t, int64(4), afterPAT.IdentityRevision)
	require.NoError(t, repos.AccountExts.AdminWritePATKeyCAS(ctx, acc.ID, 6, "pat-1"))
	afterPATSame, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, int64(7), afterPATSame.LifecycleRevision)
	require.Equal(t, int64(4), afterPATSame.IdentityRevision, "rewriting the same PAT key must not advance K")

	// 7) 只改过期时刻（token/refresh 未变）也是凭据面变更 ⇒ K 推进：过期时刻参与
	//    上游鉴权有效性，不是"无消费者"的展示字段。
	expires := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, repos.AccountExts.AdminWriteOAuthRotationCAS(ctx, acc.ID, 7, "tok-rotated", "rt-rotated", &expires))
	afterExpiry, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, int64(8), afterExpiry.LifecycleRevision)
	require.Equal(t, int64(5), afterExpiry.IdentityRevision, "expiry change is part of the credential face")
}

// TestPGAdminCredentialWriteVoidsOldIdentityVerdict 是上一条的**后果**用例：身份
// 写入推进 K 之后，以旧 K 为前提的失效判决（FailAccountCAS）必须被拒——这正是
// K 推进存在的意义（旧凭据下的判死不能钉住新凭据）。
func TestPGAdminCredentialWriteVoidsOldIdentityVerdict(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	acc, _ := seedCodexAccountWithExt(t, repos, "ext-void")

	require.NoError(t, repos.AccountExts.AdminWriteOAuthRotationCAS(ctx, acc.ID, 1, "tok-2", "rt-2", nil))
	fresh, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, int64(2), fresh.IdentityRevision)

	// 旧 K（1）下的判决必须陈旧被拒。
	err = repos.Accounts.FailAccountCAS(ctx, acc.ID, 1, "sdk", time.Now(), "old verdict")
	require.ErrorIs(t, err, repository.ErrStaleIdentityRevision, "verdict captured before the credential replacement must be voided")
	// 新 K 下的判决正常落库。
	require.NoError(t, repos.Accounts.FailAccountCAS(ctx, acc.ID, 2, "sdk", time.Now(), "fresh verdict"))
	after, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.NotNil(t, after.FailedAt)
}
