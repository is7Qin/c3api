// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"
)

// ---------------------------------------------------------------------------
// codex 批量导入 service 层真实 PG（幂等矩阵/事务回滚/缺省断言/快照生效
// ——基座同 signup_bootstrap_pg_test.go：测试私有 clone）。
// ---------------------------------------------------------------------------

// newCodexImportPG 测试私有 clone 上的服务（模板/组种子齐备）。
func newCodexImportPG(t *testing.T) (*Service, *repository.Repository) {
	t.Helper()
	dsn := pgtest.Clone(t)
	pool := pgtest.OpenPool(t, dsn)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	repos, err := repository.NewWithPG(t.Context(), entsql.OpenDB(dialect.Postgres, db), false, pool)
	require.NoError(t, err)
	svc := New(Deps{Store: repos, Scheduler: nil, Invalidate: NopInvalidator{}, Publisher: nil, RuleReload: nil, Auth: nil, Log: nil, EmailCodeStore: testEmailCodes})
	return svc, repos
}

func seedCodexImportTemplates(t *testing.T, repos *repository.Repository) (oauthTpl, patTpl int64, groupID int64) {
	t.Helper()
	ctx := context.Background()
	ot, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "codex-oauth-tpl", CredentialType: credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
		ModelMapping:     domain.ModelMapping{},
	})
	require.NoError(t, err)
	pt, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "codex-pat-tpl", CredentialType: credential.TypeCodexPAT,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
		ModelMapping:     domain.ModelMapping{},
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
		}, &oauthTpl, &gid, domain.CodexImportConfig{})
		require.NoError(t, err)
		require.Equal(t, 1, res.Imported)
		require.Empty(t, res.Failed)

		ext, err := repos.AccountExts.FindAccountExtByCodexKey(ctx, "pg1@example.com", "pg-1", credential.TypeCodexOAuth)
		require.NoError(t, err)
		require.Equal(t, "at-1", *ext.CodexOAuthToken)
		require.Equal(t, "rt-1", *ext.CodexOAuthRefreshToken)
		require.NotNil(t, ext.CodexOAuthExpiresAt, "RFC3339 expires roundtrip")
		require.NotNil(t, ext.CodexIdentity)
		require.NotEmpty(t, ext.CodexIdentity.InstallationID, "身份自动生成持久化")

		acc, err := repos.Accounts.GetAccount(ctx, ext.AccountID)
		require.NoError(t, err)
		require.Equal(t, 25, acc.MaxConcurrency, "max_concurrency 缺省 25（非表默认 8红绿）")
		require.Equal(t, "pg1@example.com", acc.Name)
		gs, err := repos.Accounts.GetAccountGroups(ctx, acc.ID)
		require.NoError(t, err)
		require.Equal(t, []int64{gid}, gs, "group_id 传入 → 归组")
	})

	t.Run("same key re-import updated credentials only", func(t *testing.T) {
		res, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
			{CodexEmail: "pg1@example.com", CodexAccountID: "pg-1",
				CodexOAuthToken: "at-2", CodexOAuthRefreshToken: "rt-2"},
		}, &oauthTpl, nil, domain.CodexImportConfig{})
		require.NoError(t, err)
		require.Equal(t, 1, res.Updated)
		require.Empty(t, res.Failed)

		ext, err := repos.AccountExts.FindAccountExtByCodexKey(ctx, "pg1@example.com", "pg-1", credential.TypeCodexOAuth)
		require.NoError(t, err)
		require.Equal(t, "at-2", *ext.CodexOAuthToken, "凭据更新")
		require.Equal(t, "rt-2", *ext.CodexOAuthRefreshToken)
		require.Nil(t, ext.CodexOAuthExpiresAt, "updated 不传 expires → 清 NULL（保旧语义）")
		require.NotNil(t, ext.CodexIdentity, "身份沿用")
		acc, err := repos.Accounts.GetAccount(ctx, ext.AccountID)
		require.NoError(t, err)
		require.Equal(t, 25, acc.MaxConcurrency, "并发不动")
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
		}, &oauthTpl, nil, domain.CodexImportConfig{})
		require.NoError(t, err)
		require.Equal(t, 2, res.Imported)
		require.Empty(t, res.Failed)
		for _, key := range [][2]string{{"pg1@example.com", "pg-2"}, {"pg2@example.com", "pg-1"}} {
			_, err := repos.AccountExts.FindAccountExtByCodexKey(ctx, key[0], key[1], credential.TypeCodexOAuth)
			require.NoError(t, err, "组合键语义共存")
		}
	})

	t.Run("batch duplicate keys last wins", func(t *testing.T) {
		res, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
			{CodexEmail: "pg3@example.com", CodexAccountID: "pg-3",
				CodexOAuthToken: "at-first", CodexOAuthRefreshToken: "rt-first"},
			{CodexEmail: "pg3@example.com", CodexAccountID: "pg-3",
				CodexOAuthToken: "at-last", CodexOAuthRefreshToken: "rt-last"},
		}, &oauthTpl, nil, domain.CodexImportConfig{})
		require.NoError(t, err)
		require.Equal(t, 1, res.Imported)
		require.Equal(t, 1, res.Updated)
		require.Empty(t, res.Failed)
		ext, err := repos.AccountExts.FindAccountExtByCodexKey(ctx, "pg3@example.com", "pg-3", credential.TypeCodexOAuth)
		require.NoError(t, err)
		require.Equal(t, "at-last", *ext.CodexOAuthToken, "批内同键逐行顺序应用后者胜")
	})

	t.Run("cross type same key row failed no mixed write", func(t *testing.T) {
		// pat 端点先导 pg-4
		res, err := svc.ImportCodexPATAccounts(ctx, []domain.CodexPATImportItem{
			{CodexEmail: "pg4@example.com", CodexAccountID: "pg-4", CodexPATKey: "pat-1"},
		}, &patTpl, nil, domain.CodexImportConfig{})
		require.NoError(t, err)
		require.Equal(t, 1, res.Imported)

		// oauth 端点命中 pat 行 → 行级 failed（不跨类型混写）
		res, err = svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
			{CodexEmail: "pg4@example.com", CodexAccountID: "pg-4",
				CodexOAuthToken: "at-x", CodexOAuthRefreshToken: "rt-x"},
		}, &oauthTpl, nil, domain.CodexImportConfig{})
		require.NoError(t, err)
		require.Equal(t, 0, res.Imported)
		require.Equal(t, 0, res.Updated)
		require.Len(t, res.Failed, 1)
		require.Equal(t, 0, res.Failed[0].Index)
		require.Contains(t, res.Failed[0].Error, "凭据类型不匹配")
		ext, err := repos.AccountExts.FindAccountExtByCodexKey(ctx, "pg4@example.com", "pg-4", credential.TypeCodexOAuth)
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
	}, &patTpl, nil, domain.CodexImportConfig{})
	require.ErrorIs(t, err, ErrInvalidInput)
	require.Contains(t, err.Error(), "模板类型不匹配")

	_, err = svc.ImportCodexPATAccounts(ctx, []domain.CodexPATImportItem{
		{CodexEmail: "tm@example.com", CodexAccountID: "tm", CodexPATKey: "pat"},
	}, &oauthTpl, nil, domain.CodexImportConfig{})
	require.ErrorIs(t, err, ErrInvalidInput)

	// 整批拒绝零写入
	_, err = repos.AccountExts.FindAccountExtByCodexKey(ctx, "tm@example.com", "tm", credential.TypeCodexOAuth)
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
	}, &oauthTpl, nil, domain.CodexImportConfig{})
	require.NoError(t, err)
	require.Equal(t, 1, res.Imported)
	ext, err := repos.AccountExts.FindAccountExtByCodexKey(ctx, "sd@example.com", "sd", credential.TypeCodexOAuth)
	require.NoError(t, err)

	// 真实软删（deleted_at 置值）
	require.NoError(t, repos.Accounts.DeleteAccount(ctx, ext.AccountID))

	res, err = svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
		{CodexEmail: "sd@example.com", CodexAccountID: "sd",
			CodexOAuthToken: "at-2", CodexOAuthRefreshToken: "rt-2"},
	}, &oauthTpl, nil, domain.CodexImportConfig{})
	require.NoError(t, err)
	require.Equal(t, 0, res.Imported)
	require.Equal(t, 0, res.Updated)
	require.Len(t, res.Failed, 1)
	require.Contains(t, res.Failed[0].Error, "账号已删除")

	acc, err := repos.Accounts.GetAccount(ctx, ext.AccountID)
	require.NoError(t, err)
	require.NotNil(t, acc.DeletedAt, "账号仍软删（不自动复活）")
	ext2, err := repos.AccountExts.FindAccountExtByCodexKey(ctx, "sd@example.com", "sd", credential.TypeCodexOAuth)
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
	}, &oauthTpl, &missingGid, domain.CodexImportConfig{})
	require.NoError(t, err)
	require.Equal(t, 0, res.Imported)
	require.Len(t, res.Failed, 1)
	require.Contains(t, res.Failed[0].Error, "missing")

	// 无孤儿：无 ext 行 + 无 account 行
	_, err = repos.AccountExts.FindAccountExtByCodexKey(ctx, "rbpg@example.com", "rbpg", credential.TypeCodexOAuth)
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
	}, &oauthTpl, &gid, domain.CodexImportConfig{})
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
	}, &oauthTpl, nil, domain.CodexImportConfig{})
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

// TestBPSImportCoexistsWithCodexAccountPG 同一个 ChatGPT 身份（email + 上游
// account id）先作为 codex-oauth 存在时，BPS 登录必须新建独立的 BPS 账号，而不是
// 撞 0001 的两列唯一索引后报「凭据类型不匹配」。
//
// 0002 迁移把导入幂等键拆成两条部分唯一索引（credential_type <> 'bps' 与 = 'bps'）；
// 本用例跑在真实 PG 上，因此同时是那条迁移的回归：没有 0002，第二次插入会 23505。
func TestBPSImportCoexistsWithCodexAccountPG(t *testing.T) {
	svc, repos := newCodexImportPG(t)
	ctx := context.Background()

	oauthTpl, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "bps-coexist-codex", CredentialType: credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
		ModelMapping:     domain.ModelMapping{},
	})
	require.NoError(t, err)
	bpsTpl, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "bps-coexist-bps", CredentialType: credential.TypeBPS,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
		ModelMapping:     domain.ModelMapping{},
	})
	require.NoError(t, err)

	const email, upstream = "coexist@example.com", "acct-shared-1"
	codexRes, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{{
		CodexEmail: email, CodexAccountID: upstream,
		CodexOAuthToken: "at-codex", CodexOAuthRefreshToken: "rt-codex",
	}}, &oauthTpl.ID, nil, domain.CodexImportConfig{})
	require.NoError(t, err)
	require.Equal(t, 1, codexRes.Imported)

	svc.bpsLogin = &fakeBPSLoginProvider{fakeCodexLoginProvider: fakeCodexLoginProvider{
		status: domain.CodexLoginAuthorized,
		tokens: &domain.CodexLoginTokens{
			Email: email, AccountID: upstream, AccessToken: "at-bps", RefreshToken: "rt-bps",
		},
		code: domain.CodexDeviceCode{AuthID: "auth-coexist", UserCode: "code"},
	}}
	bpsRes, err := svc.CompleteDeviceLogin(ctx, domain.DeviceLoginBPS, "auth-coexist", "code", bpsTpl.ID, nil)
	require.NoError(t, err, "同一上游身份不得阻塞 BPS 登录")
	require.Equal(t, domain.CodexLoginAuthorized, bpsRes.Status)
	require.True(t, bpsRes.Imported, "BPS 必须新建独立账号行")
	require.NotZero(t, bpsRes.AccountID)

	codexExt, err := repos.AccountExts.FindAccountExtByCodexKey(ctx, email, upstream, credential.TypeCodexOAuth)
	require.NoError(t, err)
	require.Equal(t, credential.TypeCodexOAuth, codexExt.CredentialType)
	require.Equal(t, "at-codex", *codexExt.CodexOAuthToken, "Codex 行凭据零触碰")

	bpsExt, err := repos.AccountExts.FindAccountExtByCodexKey(ctx, email, upstream, credential.TypeBPS)
	require.NoError(t, err)
	require.Equal(t, credential.TypeBPS, bpsExt.CredentialType)
	require.Equal(t, "at-bps", *bpsExt.CodexOAuthToken)
	require.NotEqual(t, codexExt.AccountID, bpsExt.AccountID, "两个 provider 各自独立账号行")
}

// TestCodexIdentityGroupStillExclusivePG 0002 拆索引后，Codex 组内部仍必须互斥：
// 同一 (codex_email, codex_account_id) 再写一条 codex-pat 行要撞部分唯一索引
// （23505）。放宽 BPS 与 Codex 的并存，不能顺手放宽 oauth/pat 混写。
func TestCodexIdentityGroupStillExclusivePG(t *testing.T) {
	svc, repos := newCodexImportPG(t)
	ctx := context.Background()

	oauthTpl, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "grp-codex-oauth", CredentialType: credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
		ModelMapping:     domain.ModelMapping{},
	})
	require.NoError(t, err)
	patTpl, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "grp-codex-pat", CredentialType: credential.TypeCodexPAT,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
		ModelMapping:     domain.ModelMapping{},
	})
	require.NoError(t, err)

	const email, upstream = "group@example.com", "acct-group-1"
	res, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{{
		CodexEmail: email, CodexAccountID: upstream,
		CodexOAuthToken: "at", CodexOAuthRefreshToken: "rt",
	}}, &oauthTpl.ID, nil, domain.CodexImportConfig{})
	require.NoError(t, err)
	require.Equal(t, 1, res.Imported, "base oauth row must exist")

	// 直接由 repo 写入第二条同组（不同 Codex 类型）行：import 层的跨类型检查
	// 之外，DB 的部分唯一索引也必须是兜底，而不是只有应用层记得判。
	acc, err := repos.Accounts.CreateAccount(ctx, &domain.Account{
		Name: "pat-duplicate", TemplateID: patTpl.ID, Enabled: true, MaxConcurrency: 1,
	})
	require.NoError(t, err)
	_, err = repos.AccountExts.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: acc.ID, CredentialType: credential.TypeCodexPAT,
		CodexEmail: pgStrPtr(t, email), CodexAccountID: pgStrPtr(t, upstream),
		CodexPATKey: pgStrPtr(t, "pat-key"),
	})
	require.Error(t, err, "oauth 与 pat 共用同一身份组，第二行必须被唯一索引拒绝")
	require.Contains(t, err.Error(), "23505")
}
