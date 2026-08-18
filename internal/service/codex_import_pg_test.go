// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"os"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// ---------------------------------------------------------------------------
// Task B codex 批量导入 service 层真实 PG（幂等矩阵/事务回滚/缺省断言/快照生效
// ——基座同 signup_bootstrap_pg_test.go：独立 schema，不 DROP public）。
// ---------------------------------------------------------------------------

// codexImportPGTestSchema 本文件 PG 测试专用 schema。
const codexImportPGTestSchema = "codex_import_test"

// newCodexImportPG 独立 schema 上的服务（模板/组种子齐备）。
func newCodexImportPG(t *testing.T) (*Service, *repository.Repository) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + codexImportPGTestSchema
	} else {
		dsn += "?search_path=" + codexImportPGTestSchema
	}
	ctx := context.Background()
	pool, err := repository.OpenPG(ctx, dsn, 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+codexImportPGTestSchema+` CASCADE; CREATE SCHEMA `+codexImportPGTestSchema+`;`)
	require.NoError(t, err)
	repos, err := repository.NewWithPG(t.Context(), entsql.OpenDB(dialect.Postgres, db), true, pool)
	require.NoError(t, err)
	svc := New(repos, nil, NopInvalidator{}, nil, nil, nil, nil)
	return svc, repos
}

func seedCodexImportTemplates(t *testing.T, repos *repository.Repository) (oauthTpl, patTpl int64, groupID int64) {
	t.Helper()
	ctx := context.Background()
	ot, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "codex-oauth-tpl", CredentialType: credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
	})
	require.NoError(t, err)
	pt, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "codex-pat-tpl", CredentialType: credential.TypeCodexPAT,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
	})
	require.NoError(t, err)
	g, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "codex-import-g", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	return ot.ID, pt.ID, g.ID
}

// TestImportCodexAccountsPG 幂等矩阵（真实 PG）：新建 → imported（身份生成 +
// 缺省 25/100 落库）；同键重导 → updated（凭据更新、身份沿用、并发/权重不动）；
// 不同键共存；批内同键后者胜；跨类型同键 → 行级 failed 零混写。
func TestImportCodexAccountsPG(t *testing.T) {
	svc, repos := newCodexImportPG(t)
	ctx := context.Background()
	oauthTpl, patTpl, gid := seedCodexImportTemplates(t, repos)

	t.Run("new key imported with defaults", func(t *testing.T) {
		res, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
			{CodexEmail: "pg1@example.com", CodexAccountID: "pg-1",
				CodexOAuthToken: "at-1", CodexOAuthRefreshToken: "rt-1",
				CodexOAuthExpiresAt: pgStrPtr(t, "2026-12-31T23:59:59Z")},
		}, &oauthTpl, &gid)
		require.NoError(t, err)
		require.Equal(t, 1, res.Imported)
		require.Empty(t, res.Failed)

		ext, err := repos.AccountExts.FindAccountExtByCodexKey(ctx, "pg1@example.com", "pg-1")
		require.NoError(t, err)
		require.Equal(t, "at-1", *ext.CodexOAuthToken)
		require.Equal(t, "rt-1", *ext.CodexOAuthRefreshToken)
		require.NotNil(t, ext.CodexOAuthExpiresAt, "RFC3339 expires roundtrip")
		require.NotNil(t, ext.CodexIdentity)
		require.NotEmpty(t, ext.CodexIdentity.InstallationID, "身份自动生成持久化")

		acc, err := repos.Accounts.GetAccount(ctx, ext.AccountID)
		require.NoError(t, err)
		require.Equal(t, 25, acc.MaxConcurrency, "max_concurrency 缺省 25（非表默认 8——用户裁决红绿）")
		require.Equal(t, 100, acc.Weight, "weight 缺省 100")
		require.Equal(t, "pg1@example.com", acc.Name)
		gs, err := repos.Accounts.GetAccountGroups(ctx, acc.ID)
		require.NoError(t, err)
		require.Equal(t, []int64{gid}, gs, "group_id 传入 → 归组")
	})

	t.Run("same key re-import updated credentials only", func(t *testing.T) {
		res, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
			{CodexEmail: "pg1@example.com", CodexAccountID: "pg-1",
				CodexOAuthToken: "at-2", CodexOAuthRefreshToken: "rt-2"},
		}, &oauthTpl, nil)
		require.NoError(t, err)
		require.Equal(t, 1, res.Updated)
		require.Empty(t, res.Failed)

		ext, err := repos.AccountExts.FindAccountExtByCodexKey(ctx, "pg1@example.com", "pg-1")
		require.NoError(t, err)
		require.Equal(t, "at-2", *ext.CodexOAuthToken, "凭据更新")
		require.Equal(t, "rt-2", *ext.CodexOAuthRefreshToken)
		require.Nil(t, ext.CodexOAuthExpiresAt, "updated 不传 expires → 清 NULL（保旧语义）")
		require.NotNil(t, ext.CodexIdentity, "身份沿用")
		acc, err := repos.Accounts.GetAccount(ctx, ext.AccountID)
		require.NoError(t, err)
		require.Equal(t, 25, acc.MaxConcurrency, "并发不动")
		require.Equal(t, 100, acc.Weight, "权重不动")
		gs, err := repos.Accounts.GetAccountGroups(ctx, acc.ID)
		require.NoError(t, err)
		require.Equal(t, []int64{gid}, gs, "归属不动")
	})

	t.Run("different keys coexist", func(t *testing.T) {
		res, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
			{CodexEmail: "pg1@example.com", CodexAccountID: "pg-2",
				CodexOAuthToken: "at", CodexOAuthRefreshToken: "rt"},
			{CodexEmail: "pg2@example.com", CodexAccountID: "pg-1",
				CodexOAuthToken: "at", CodexOAuthRefreshToken: "rt"},
		}, &oauthTpl, nil)
		require.NoError(t, err)
		require.Equal(t, 2, res.Imported)
		require.Empty(t, res.Failed)
		for _, key := range [][2]string{{"pg1@example.com", "pg-2"}, {"pg2@example.com", "pg-1"}} {
			_, err := repos.AccountExts.FindAccountExtByCodexKey(ctx, key[0], key[1])
			require.NoError(t, err, "组合键语义共存")
		}
	})

	t.Run("batch duplicate keys last wins", func(t *testing.T) {
		res, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
			{CodexEmail: "pg3@example.com", CodexAccountID: "pg-3",
				CodexOAuthToken: "at-first", CodexOAuthRefreshToken: "rt-first"},
			{CodexEmail: "pg3@example.com", CodexAccountID: "pg-3",
				CodexOAuthToken: "at-last", CodexOAuthRefreshToken: "rt-last"},
		}, &oauthTpl, nil)
		require.NoError(t, err)
		require.Equal(t, 1, res.Imported)
		require.Equal(t, 1, res.Updated)
		require.Empty(t, res.Failed)
		ext, err := repos.AccountExts.FindAccountExtByCodexKey(ctx, "pg3@example.com", "pg-3")
		require.NoError(t, err)
		require.Equal(t, "at-last", *ext.CodexOAuthToken, "批内同键逐行顺序应用后者胜")
	})

	t.Run("cross type same key row failed no mixed write", func(t *testing.T) {
		// pat 端点先导 pg-4
		res, err := svc.ImportCodexPATAccounts(ctx, []domain.CodexPATImportItem{
			{CodexEmail: "pg4@example.com", CodexAccountID: "pg-4", CodexPATKey: "pat-1"},
		}, &patTpl, nil)
		require.NoError(t, err)
		require.Equal(t, 1, res.Imported)

		// oauth 端点命中 pat 行 → 行级 failed（不跨类型混写）
		res, err = svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
			{CodexEmail: "pg4@example.com", CodexAccountID: "pg-4",
				CodexOAuthToken: "at-x", CodexOAuthRefreshToken: "rt-x"},
		}, &oauthTpl, nil)
		require.NoError(t, err)
		require.Equal(t, 0, res.Imported)
		require.Equal(t, 0, res.Updated)
		require.Len(t, res.Failed, 1)
		require.Equal(t, 0, res.Failed[0].Index)
		require.Contains(t, res.Failed[0].Error, "凭据类型不匹配")
		ext, err := repos.AccountExts.FindAccountExtByCodexKey(ctx, "pg4@example.com", "pg-4")
		require.NoError(t, err)
		require.Equal(t, credential.TypeCodexPAT, ext.CredentialType, "跨类型不混写")
		require.Nil(t, ext.CodexOAuthToken, "oauth 列零写入")
		require.Equal(t, "pat-1", *ext.CodexPATKey, "pat 凭据保持")
	})
}

// TestImportCodexTemplateTypeMismatchPG 模板类型错配（task review Important 1）：
// oauth 端点配 codex-pat 模板 → 400 整批拒绝，零写入（防违反 ext 类型 ==
// 模板类型硬不变量）。
func TestImportCodexTemplateTypeMismatchPG(t *testing.T) {
	svc, repos := newCodexImportPG(t)
	ctx := context.Background()
	oauthTpl, patTpl, _ := seedCodexImportTemplates(t, repos)

	_, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
		{CodexEmail: "tm@example.com", CodexAccountID: "tm",
			CodexOAuthToken: "at", CodexOAuthRefreshToken: "rt"},
	}, &patTpl, nil)
	require.ErrorIs(t, err, ErrInvalidInput)
	require.Contains(t, err.Error(), "模板类型不匹配")

	_, err = svc.ImportCodexPATAccounts(ctx, []domain.CodexPATImportItem{
		{CodexEmail: "tm@example.com", CodexAccountID: "tm", CodexPATKey: "pat"},
	}, &oauthTpl, nil)
	require.ErrorIs(t, err, ErrInvalidInput)

	// 整批拒绝零写入
	_, err = repos.AccountExts.FindAccountExtByCodexKey(ctx, "tm@example.com", "tm")
	require.ErrorIs(t, err, repository.ErrNotFound, "错配批零写入")
}

// TestImportCodexSoftDeletedPG 软删账号重导入（task review Minor 1）：命中已
// 软删账号 → 行级 failed + 账号仍软删 + 凭据不更新（updated 只对存活账号）。
func TestImportCodexSoftDeletedPG(t *testing.T) {
	svc, repos := newCodexImportPG(t)
	ctx := context.Background()
	oauthTpl, _, _ := seedCodexImportTemplates(t, repos)

	res, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
		{CodexEmail: "sd@example.com", CodexAccountID: "sd",
			CodexOAuthToken: "at-1", CodexOAuthRefreshToken: "rt-1"},
	}, &oauthTpl, nil)
	require.NoError(t, err)
	require.Equal(t, 1, res.Imported)
	ext, err := repos.AccountExts.FindAccountExtByCodexKey(ctx, "sd@example.com", "sd")
	require.NoError(t, err)

	// 真实软删（deleted_at 置值）
	require.NoError(t, repos.Accounts.DeleteAccount(ctx, ext.AccountID))

	res, err = svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
		{CodexEmail: "sd@example.com", CodexAccountID: "sd",
			CodexOAuthToken: "at-2", CodexOAuthRefreshToken: "rt-2"},
	}, &oauthTpl, nil)
	require.NoError(t, err)
	require.Equal(t, 0, res.Imported)
	require.Equal(t, 0, res.Updated)
	require.Len(t, res.Failed, 1)
	require.Contains(t, res.Failed[0].Error, "账号已删除")

	acc, err := repos.Accounts.GetAccount(ctx, ext.AccountID)
	require.NoError(t, err)
	require.NotNil(t, acc.DeletedAt, "账号仍软删（不自动复活）")
	ext2, err := repos.AccountExts.FindAccountExtByCodexKey(ctx, "sd@example.com", "sd")
	require.NoError(t, err)
	require.Equal(t, "at-1", *ext2.CodexOAuthToken, "凭据未更新（updated 只对存活账号）")
}

// TestImportCodexTxRollbackPG 单行事务回滚（真实 PG）：group_id 不存在 →
// SetAccountGroups 失败 → account+ext 整体回滚（无孤儿断言——回滚后无
// account 行无 ext 行）。
func TestImportCodexTxRollbackPG(t *testing.T) {
	svc, repos := newCodexImportPG(t)
	ctx := context.Background()
	oauthTpl, _, _ := seedCodexImportTemplates(t, repos)

	missingGid := int64(999999)
	res, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
		{CodexEmail: "rbpg@example.com", CodexAccountID: "rbpg",
			CodexOAuthToken: "at", CodexOAuthRefreshToken: "rt"},
	}, &oauthTpl, &missingGid)
	require.NoError(t, err)
	require.Equal(t, 0, res.Imported)
	require.Len(t, res.Failed, 1)
	require.Contains(t, res.Failed[0].Error, "missing")

	// 无孤儿：无 ext 行 + 无 account 行
	_, err = repos.AccountExts.FindAccountExtByCodexKey(ctx, "rbpg@example.com", "rbpg")
	require.ErrorIs(t, err, repository.ErrNotFound, "ext 行回滚零残留")
	accs, _, err := repos.Accounts.ListAccounts(ctx, repository.ListQuery{Name: "rbpg@example.com"})
	require.NoError(t, err)
	require.Empty(t, accs, "account 行回滚零残留")
}

// TestImportCodexSnapshotPG 快照生效：导入后调度器数据源（LoadGroupsAccounts）
// 反映新凭据（updated 后重载同样反映——sig 变化重建的 DB 侧数据源）。
func TestImportCodexSnapshotPG(t *testing.T) {
	svc, repos := newCodexImportPG(t)
	ctx := context.Background()
	oauthTpl, _, gid := seedCodexImportTemplates(t, repos)

	res, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
		{CodexEmail: "snap@example.com", CodexAccountID: "snap",
			CodexOAuthToken: "at-1", CodexOAuthRefreshToken: "rt-1"},
	}, &oauthTpl, &gid)
	require.NoError(t, err)
	require.Equal(t, 1, res.Imported)

	members, err := repos.Groups.LoadGroupAccounts(ctx, gid)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.NotNil(t, members[0].Ext, "导入账号 ext 合并进调度器快照")
	require.Equal(t, "at-1", *members[0].Ext.CodexOAuthToken, "新凭据进快照（调度器重载后即生效）")

	// updated 后快照反映新凭据（导入即生效——sig 变化重建的数据源）
	_, err = svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
		{CodexEmail: "snap@example.com", CodexAccountID: "snap",
			CodexOAuthToken: "at-2", CodexOAuthRefreshToken: "rt-2"},
	}, &oauthTpl, nil)
	require.NoError(t, err)
	members, err = repos.Groups.LoadGroupAccounts(ctx, gid)
	require.NoError(t, err)
	require.Equal(t, "at-2", *members[0].Ext.CodexOAuthToken, "updated 后重载快照反映新凭据")
}

// pgStrPtr RFC3339 字符串 → *string（测试构造用）。
func pgStrPtr(t *testing.T, s string) *string {
	t.Helper()
	return &s
}
