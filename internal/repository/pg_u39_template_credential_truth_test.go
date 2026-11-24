// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// 本文件是 U39 的 PG 汇测。三条纪律（spec J7，判据不许放宽）：
//
//  1. **脏行只用裸 SQL 造**：`UPDATE account_exts SET credential_type = $1
//     WHERE account_id = $2`。禁止用 UpsertAccountExt 造脏——写面（service/
//     ext_codex.go:176-182 与 repository.validateAccountExtTemplate）强制两列同
//     BPS 位，走它写出的夹具是同源同值，把实现改回"按 ext 列分支"也不会红。
//  2. 断言对象是**汇**：一律从 LoadGroupsAccountsFull / LoadGroupAccounts 返回的
//     *domain.Account.Ext 取值，用例不得自己构造期望对象。
//  3. 规模：≥3 账号 × ≥2 模板 × ≥2 类型，且必须含一对 BPS↔codex 跨通道错配；
//     逐 account_id 断言 + 观测类型集合 len ≥ 2（防夹具退化）。

// u39ExtRow 一条 ext 行夹具（裸 SQL 落库，完全绕开写面）。
type u39ExtRow struct {
	AccountID      int64
	Type           credential.Type
	Identity       any // jsonb 字面量；nil = SQL NULL
	OAuthToken     any
	OAuthRefresh   any
	PATKey         any
	Email          any
	CodexAccountID any
}

// u39InsertExt 直接写 account_exts：凭据内容与行类型都由用例指定。
func u39InsertExt(t *testing.T, conn *pgx.Conn, row u39ExtRow) {
	t.Helper()
	_, err := conn.Exec(context.Background(), `INSERT INTO account_exts
		(account_id, credential_type, codex_identity, codex_oauth_token,
		 codex_oauth_refresh_token, codex_oauth_expires_at, codex_pat_key,
		 codex_email, codex_account_id)
		VALUES ($1, $2, $3::jsonb, $4, $5, NULL, $6, $7, $8)`,
		row.AccountID, string(row.Type), row.Identity, row.OAuthToken, row.OAuthRefresh,
		row.PATKey, row.Email, row.CodexAccountID)
	require.NoError(t, err, "裸 SQL 写 ext 行")
}

// u39RetypeExt 是**唯一**的造脏形状（spec J7.1）：只改 credential_type 一列，
// 凭据内容保持原样——这正是"行类型与模板类型不一致"的真实损坏形态。
func u39RetypeExt(t *testing.T, conn *pgx.Conn, accountID int64, typ credential.Type) {
	t.Helper()
	_, err := conn.Exec(context.Background(),
		`UPDATE account_exts SET credential_type = $1 WHERE account_id = $2`, string(typ), accountID)
	require.NoError(t, err, "裸 SQL 造脏行")
}

// u39CreateAccount 直接落一行账号（BPS 账号不走通用 CreateAccount——它必须经
// provider 事务；本文件的用例只关心读面投影）。
func u39CreateAccount(t *testing.T, repos *repository.Repository, tplID int64, name string) int64 {
	t.Helper()
	acc, err := repos.Client.Account.Create().
		SetName(name).SetTemplateID(tplID).SetUpstreamKey("").
		SetMaxConcurrency(8).SetEnabled(true).Save(context.Background())
	require.NoError(t, err)
	return acc.ID
}

// u39CreateTemplate 建模板（读面投影只关心 credential_type 与格式集合）。
func u39CreateTemplate(t *testing.T, repos *repository.Repository, name string, typ credential.Type, formats ...domain.RequestFormat) *domain.Template {
	t.Helper()
	baseURL := "https://u/v1"
	if typ.IsResponsesProvider() {
		baseURL = "" // Responses provider 模板自带固定端点：base_url 必须为空
	}
	tpl, err := repos.Templates.CreateTemplate(context.Background(), &domain.Template{
		Name: name, BaseURL: baseURL, CredentialType: typ,
		SupportedFormats: formats, ModelMapping: domain.ModelMapping{},
	})
	require.NoError(t, err)
	return tpl
}

// u39BPSIdentity 合法 BPS identity envelope（profile 是对象）。
func u39BPSIdentity(profile string) string {
	return `{"bps":{"profile_id":"` + profile + `"}}`
}

// TestPGAccountExtTemplateTruthSnapshot 是 J7.3/J7.4 的汇测：模板驱动分支 +
// 裸 SQL 脏行跳过 + 计数 + K 不被覆盖 + 遗留行豁免。
func TestPGAccountExtTemplateTruthSnapshot(t *testing.T) {
	repos := newPGReposShared(t)
	conn := pgSharedConn(t)
	ctx := context.Background()

	tplAPIKey := seedPGTemplate(t, repos) // api_key（静态通道）
	tplCustom := u39CreateTemplate(t, repos, "u39-custom", credential.TypeCustom, domain.FormatOpenAIChat)
	tplBPS := u39CreateTemplate(t, repos, "u39-bps", credential.TypeBPS, domain.FormatOpenAIResponses)
	tplCodexPAT := u39CreateTemplate(t, repos, "u39-codex-pat", credential.TypeCodexPAT, domain.FormatOpenAIResponses)

	g := seedPGGroup(t, repos, "u39-g")

	// N1：api_key 模板 + codex-pat 遗留行（写面本来就有意允许）。
	aLegacyPAT := u39CreateAccount(t, repos, tplAPIKey.ID, "u39-legacy-pat")
	u39InsertExt(t, conn, u39ExtRow{
		AccountID: aLegacyPAT, Type: credential.TypeCodexPAT,
		Identity: `{"installation_id":"inst-legacy-pat"}`,
		PATKey:   "pat-legacy",
	})
	// N1：custom 模板 + codex-oauth 遗留行。
	aLegacyOAuth := u39CreateAccount(t, repos, tplCustom.ID, "u39-legacy-oauth")
	u39InsertExt(t, conn, u39ExtRow{
		AccountID: aLegacyOAuth, Type: credential.TypeCodexOAuth,
		Identity:     `{"installation_id":"inst-legacy-oauth"}`,
		OAuthToken:   "at-legacy",
		OAuthRefresh: "rt-legacy",
	})
	// 静态通道模板 + **BPS 行**：写面不允许（BPS 位不同），只能来自历史裸 SQL；
	// v4 修正 2 明确"否则不断言、不改行为"——这类行同样是遗留行，照常合并。
	aLegacyBPSRow := u39CreateAccount(t, repos, tplAPIKey.ID, "u39-legacy-bps-row")
	u39InsertExt(t, conn, u39ExtRow{
		AccountID: aLegacyBPSRow, Type: credential.TypeBPS,
		Identity:       u39BPSIdentity("p-legacy"),
		OAuthToken:     "at-legacy",
		OAuthRefresh:   "rt-legacy",
		CodexAccountID: "acct-legacy",
	})
	// 合法 BPS 行（内容与 K 成对的阳性对照）。
	aBPS := u39CreateAccount(t, repos, tplBPS.ID, "u39-bps-ok")
	u39InsertExt(t, conn, u39ExtRow{
		AccountID: aBPS, Type: credential.TypeBPS,
		Identity:       u39BPSIdentity("p-ok"),
		OAuthToken:     "at-bps",
		OAuthRefresh:   "rt-bps",
		CodexAccountID: "acct-bps",
	})
	// 合法 codex-pat 行。
	aCodexPAT := u39CreateAccount(t, repos, tplCodexPAT.ID, "u39-codex-pat-ok")
	u39InsertExt(t, conn, u39ExtRow{
		AccountID: aCodexPAT, Type: credential.TypeCodexPAT,
		Identity: `{"installation_id":"inst-codex-pat"}`,
		PATKey:   "pat-ok",
	})
	// 跨通道错配 A：BPS 模板 + codex-pat ext 行（裸 SQL 造脏）。
	aDirtyBPS := u39CreateAccount(t, repos, tplBPS.ID, "u39-dirty-bps-tpl")
	u39InsertExt(t, conn, u39ExtRow{
		AccountID: aDirtyBPS, Type: credential.TypeBPS,
		Identity:       u39BPSIdentity("p-dirty"),
		OAuthToken:     "at-dirty",
		OAuthRefresh:   "rt-dirty",
		CodexAccountID: "acct-dirty",
	})
	u39RetypeExt(t, conn, aDirtyBPS, credential.TypeCodexPAT)
	// 跨通道错配 B：codex-pat 模板 + bps ext 行（裸 SQL 造脏，反方向）。
	aDirtyCodex := u39CreateAccount(t, repos, tplCodexPAT.ID, "u39-dirty-codex-tpl")
	u39InsertExt(t, conn, u39ExtRow{
		AccountID: aDirtyCodex, Type: credential.TypeCodexPAT,
		Identity: `{"installation_id":"inst-dirty-codex"}`,
		PATKey:   "pat-dirty",
	})
	u39RetypeExt(t, conn, aDirtyCodex, credential.TypeBPS)

	for _, id := range []int64{aLegacyPAT, aLegacyOAuth, aLegacyBPSRow, aBPS, aCodexPAT, aDirtyBPS, aDirtyCodex} {
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, id, []int64{g.ID}))
	}
	t.Logf("fixture account_ids: legacy-pat=%d legacy-oauth=%d legacy-bps-row=%d bps-ok=%d codex-pat-ok=%d dirty-bps-tpl=%d dirty-codex-tpl=%d",
		aLegacyPAT, aLegacyOAuth, aLegacyBPSRow, aBPS, aCodexPAT, aDirtyBPS, aDirtyCodex)
	// K 观测：合法账号的 K 必须随 ext 扫描成对进入快照；脏行的 K 绝不许被覆盖。
	_, err := conn.Exec(ctx, `UPDATE accounts SET identity_revision = 23 WHERE id = $1`, aBPS)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, `UPDATE accounts SET identity_revision = 41 WHERE id = $1`, aDirtyBPS)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, `UPDATE accounts SET identity_revision = 42 WHERE id = $1`, aDirtyCodex)
	require.NoError(t, err)

	groupsMap, all, err := repos.Groups.LoadGroupsAccountsFull(ctx)
	require.NoError(t, err, "脏行是账号级隔离：装载必须成功")

	// 规模断言：6 个账号 / 4 个模板 / 3 种类型；两条脏行被跳过。
	members := groupsMap[g.ID]
	require.Len(t, members, 5, "两条 BPS 位错配的账号必须被跳过（其余 5 条照常进快照）")
	byID := make(map[int64]*domain.Account, len(members))
	for _, a := range members {
		byID[a.ID] = a
	}
	require.Contains(t, byID, aLegacyPAT)
	require.Contains(t, byID, aLegacyOAuth)
	require.Contains(t, byID, aLegacyBPSRow, "静态通道模板 + BPS 行：遗留行照常合并")
	require.Contains(t, byID, aBPS)
	require.Contains(t, byID, aCodexPAT)
	require.NotContains(t, byID, aDirtyBPS, "BPS 模板 + codex ext 行：该账号不在快照")
	require.NotContains(t, byID, aDirtyCodex, "codex 模板 + BPS ext 行：该账号不在快照")

	// 观测类型集合 len ≥ 2（防夹具退化）+ 逐 account_id 断言（取值是汇，不是自造）。
	types := map[credential.Type]bool{}
	extTypes := map[int64]credential.Type{}
	for id, a := range byID {
		require.NotNil(t, a.Ext, "account_id=%d 必须在快照里带 ext 投影", id)
		types[a.Ext.CredentialType] = true
		extTypes[id] = a.Ext.CredentialType
	}
	require.GreaterOrEqual(t, len(types), 2, "观测到的凭据类型集合不得退化：%v", extTypes)
	require.Equal(t, credential.TypeCodexPAT, extTypes[aLegacyPAT],
		"遗留行保持 ext 列原值（api_key 模板不覆盖行类型）")
	require.Equal(t, credential.TypeCodexOAuth, extTypes[aLegacyOAuth],
		"custom 模板同样不覆盖遗留行类型")
	require.Equal(t, credential.TypeBPS, extTypes[aLegacyBPSRow],
		"静态通道模板不断言：BPS 位相反的遗留行也按行类型投影（v4 修正 2）")
	require.Equal(t, credential.TypeBPS, extTypes[aBPS])
	require.Equal(t, credential.TypeCodexPAT, extTypes[aCodexPAT])

	// 投影形状：BPS 行 BPSProfile != nil && CodexIdentity == nil；codex 行反之。
	require.NotNil(t, byID[aBPS].Ext.BPSProfile, "BPS 行必须投影 profile")
	require.Nil(t, byID[aBPS].Ext.CodexIdentity, "BPS 行不得产出 codex 身份")
	require.NotNil(t, byID[aCodexPAT].Ext.CodexIdentity, "codex 行必须投影 installation")
	require.Nil(t, byID[aCodexPAT].Ext.BPSProfile, "codex 行不得产出 BPS profile")
	require.NotNil(t, byID[aLegacyPAT].Ext.CodexIdentity)
	require.Nil(t, byID[aLegacyPAT].Ext.BPSProfile)
	require.NotNil(t, byID[aLegacyBPSRow].Ext.BPSProfile, "遗留 BPS 行走 BPS 分支（行类型决定）")
	require.Nil(t, byID[aLegacyBPSRow].Ext.CodexIdentity)

	// 内容与 K 同源：合法 BPS 账号的 K 来自同一次 ext 扫描（23）。
	require.Equal(t, int64(23), byID[aBPS].IdentityRevision)

	// 账号全集与 byID 同口径（配置面不得看到"已从路由摘除但仍在全集"的幽灵）。
	require.Len(t, all, 5)
	for _, a := range all {
		require.NotEqual(t, aDirtyBPS, a.ID)
		require.NotEqual(t, aDirtyCodex, a.ID)
	}

	// 计数：2 条，逐 account_id + 两侧类型。
	mismatches := repos.Groups.LastAccountExtMismatches()
	require.Len(t, mismatches, 2, "计数必须恰好覆盖两条脏行：%v", mismatches)
	require.Equal(t, aDirtyBPS, mismatches[0].AccountID, "名单按 account_id 排序")
	require.Equal(t, credential.TypeBPS, mismatches[0].TemplateType)
	require.Equal(t, credential.TypeCodexPAT, mismatches[0].ExtType)
	require.Equal(t, aDirtyCodex, mismatches[1].AccountID)
	require.Equal(t, credential.TypeCodexPAT, mismatches[1].TemplateType)
	require.Equal(t, credential.TypeBPS, mismatches[1].ExtType)

	// 脏行绝不许覆盖 K：装载后库里仍是 41/42（装载器不写库，快照里也没有它们）。
	afterBPS, err := repos.Accounts.GetAccount(ctx, aDirtyBPS)
	require.NoError(t, err)
	require.Equal(t, int64(41), afterBPS.IdentityRevision, "脏行的 K 未被装载改写")
	afterCodex, err := repos.Accounts.GetAccount(ctx, aDirtyCodex)
	require.NoError(t, err)
	require.Equal(t, int64(42), afterCodex.IdentityRevision, "脏行的 K 未被装载改写")
}

// TestPGAccountExtMismatchDecoys 钉住 J7.7 的诱饵：软删账号、非目标组账号都不
// 计数、不报错；目标组里的合法账号照常装载。
func TestPGAccountExtMismatchDecoys(t *testing.T) {
	repos := newPGReposShared(t)
	conn := pgSharedConn(t)
	ctx := context.Background()

	tplBPS := u39CreateTemplate(t, repos, "u39-bps-decoys", credential.TypeBPS, domain.FormatOpenAIResponses)
	g1 := seedPGGroup(t, repos, "u39-g1")
	g2 := seedPGGroup(t, repos, "u39-g2")

	aClean := u39CreateAccount(t, repos, tplBPS.ID, "u39-clean")
	u39InsertExt(t, conn, u39ExtRow{
		AccountID: aClean, Type: credential.TypeBPS, Identity: u39BPSIdentity("p-clean"),
		OAuthToken: "at", OAuthRefresh: "rt", CodexAccountID: "acct",
	})
	require.NoError(t, repos.Accounts.SetAccountGroups(ctx, aClean, []int64{g1.ID}))

	aSoft := u39CreateAccount(t, repos, tplBPS.ID, "u39-soft-deleted-dirty")
	u39InsertExt(t, conn, u39ExtRow{
		AccountID: aSoft, Type: credential.TypeBPS, Identity: u39BPSIdentity("p-soft"),
		OAuthToken: "at", OAuthRefresh: "rt", CodexAccountID: "acct",
	})
	u39RetypeExt(t, conn, aSoft, credential.TypeCodexPAT)
	require.NoError(t, repos.Accounts.SetAccountGroups(ctx, aSoft, []int64{g1.ID}))
	_, err := conn.Exec(ctx, `UPDATE accounts SET deleted_at = now() WHERE id = $1`, aSoft)
	require.NoError(t, err)

	aOther := u39CreateAccount(t, repos, tplBPS.ID, "u39-other-group-dirty")
	u39InsertExt(t, conn, u39ExtRow{
		AccountID: aOther, Type: credential.TypeBPS, Identity: u39BPSIdentity("p-other"),
		OAuthToken: "at", OAuthRefresh: "rt", CodexAccountID: "acct",
	})
	u39RetypeExt(t, conn, aOther, credential.TypeCodexPAT)
	require.NoError(t, repos.Accounts.SetAccountGroups(ctx, aOther, []int64{g2.ID}))

	// 目标组 g1：软删的脏账号不在账号扫描里 ⇒ 不计数；合法账号照常。
	members, err := repos.Groups.LoadGroupAccounts(ctx, g1.ID)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.Equal(t, aClean, members[0].ID)
	require.NotNil(t, members[0].Ext)
	require.Empty(t, repos.Groups.LastAccountExtMismatches(), "软删/非目标组的脏行都不许计数")

	// 非目标组 g2 单独装载：它自己的脏行才计数（每次装载覆盖）。
	members, err = repos.Groups.LoadGroupAccounts(ctx, g2.ID)
	require.NoError(t, err)
	require.Empty(t, members)
	require.Len(t, repos.Groups.LastAccountExtMismatches(), 1)
	require.Equal(t, aOther, repos.Groups.LastAccountExtMismatches()[0].AccountID)

	// 全量装载：软删账号仍不进扫描 ⇒ 只剩 g2 那一条。
	groupsMap, all, err := repos.Groups.LoadGroupsAccountsFull(ctx)
	require.NoError(t, err)
	require.Len(t, groupsMap[g1.ID], 1)
	require.Empty(t, groupsMap[g2.ID])
	require.Len(t, all, 1)
	require.Len(t, repos.Groups.LastAccountExtMismatches(), 1)
}

// TestPGOwnedAccountCredentialTemplateTruth 钉住 J7.6 + v4 修正 2：
//   - BPS 位错配行**永不产出** AccountCredential（quota/probe 的入口）；
//   - 遗留行照常产出凭据，且列布局按**行自己的类型**（不覆盖成模板类型）。
func TestPGOwnedAccountCredentialTemplateTruth(t *testing.T) {
	repos := newPGReposShared(t)
	conn := pgSharedConn(t)
	ctx := context.Background()

	tplAPIKey := seedPGTemplate(t, repos)
	tplBPS := u39CreateTemplate(t, repos, "u39-bps-cred", credential.TypeBPS, domain.FormatOpenAIResponses)
	tplCodexPAT := u39CreateTemplate(t, repos, "u39-codex-pat-cred", credential.TypeCodexPAT, domain.FormatOpenAIResponses)

	t.Run("BPS 位错配行不产出凭据", func(t *testing.T) {
		acc := u39CreateAccount(t, repos, tplBPS.ID, "u39-cred-dirty")
		u39InsertExt(t, conn, u39ExtRow{
			AccountID: acc, Type: credential.TypeBPS, Identity: u39BPSIdentity("p"),
			OAuthToken: "at", OAuthRefresh: "rt", CodexAccountID: "acct",
		})
		u39RetypeExt(t, conn, acc, credential.TypeCodexPAT)
		cred, err := repos.AccountExts.GetOwnedAccountCredential(ctx, acc)
		require.Nil(t, cred, "损坏行永不产出 AccountCredential")
		require.ErrorIs(t, err, repository.ErrCredentialTypeMismatch)
	})

	t.Run("api_key 模板 + codex-pat 遗留行照常产出 PAT 凭据", func(t *testing.T) {
		acc := u39CreateAccount(t, repos, tplAPIKey.ID, "u39-cred-legacy")
		u39InsertExt(t, conn, u39ExtRow{
			AccountID: acc, Type: credential.TypeCodexPAT,
			Identity: `{"installation_id":"inst-legacy"}`, PATKey: "pat-legacy",
		})
		_, err := conn.Exec(ctx, `UPDATE accounts SET identity_revision = 31 WHERE id = $1`, acc)
		require.NoError(t, err)
		cred, err := repos.AccountExts.GetOwnedAccountCredential(ctx, acc)
		require.NoError(t, err)
		require.NotNil(t, cred)
		require.Equal(t, credential.TypeCodexPAT, cred.CredentialType, "行类型原值，不被模板改写")
		require.Equal(t, "pat-legacy", cred.PATKey)
		require.Empty(t, cred.OAuthToken, "codex-pat 行不取 token 列组")
		require.Equal(t, int64(31), cred.IdentityRevision, "K 与凭据同一次读")
	})

	t.Run("codex-pat 模板 + codex-oauth 行：列布局按行类型", func(t *testing.T) {
		acc := u39CreateAccount(t, repos, tplCodexPAT.ID, "u39-cred-oauth-row")
		u39InsertExt(t, conn, u39ExtRow{
			AccountID: acc, Type: credential.TypeCodexOAuth,
			Identity:     `{"installation_id":"inst-oauth-row"}`,
			OAuthToken:   "at-row",
			OAuthRefresh: "rt-row",
		})
		cred, err := repos.AccountExts.GetOwnedAccountCredential(ctx, acc)
		require.NoError(t, err)
		require.Equal(t, credential.TypeCodexOAuth, cred.CredentialType,
			"同通道内 oauth/pat 的列布局由 ext 行自己的类型决定（v4 修正 2）")
		require.Equal(t, "at-row", cred.OAuthToken)
		require.Equal(t, "rt-row", cred.OAuthRefreshToken)
		require.Empty(t, cred.PATKey)
	})
}

// TestPGAccountExtMismatchErrorCarriesBothSentinels 钉住 J2：写面复用断言助手时
// 两枚 sentinel 必须同时 errors.Is 成立（写面按 ErrInvalidInput 分流，运维按
// ErrCredentialTypeMismatch 识别同一事实）。
func TestPGAccountExtMismatchErrorCarriesBothSentinels(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	tplBPS := u39CreateTemplate(t, repos, "u39-bps-write", credential.TypeBPS, domain.FormatOpenAIResponses)
	acc := u39CreateAccount(t, repos, tplBPS.ID, "u39-write-mismatch")

	pat := "pat"
	_, err := repos.AccountExts.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: acc, CredentialType: credential.TypeCodexPAT, CodexPATKey: &pat,
	})
	require.Error(t, err)
	require.ErrorIs(t, err, repository.ErrInvalidInput, "写面既有分流不变")
	require.ErrorIs(t, err, repository.ErrCredentialTypeMismatch, "同一事实的读面 sentinel")
}

// TestPGProjectAccountExtEdgeErrorSurface 钉住 v4 修正 1/4：GetAccountWithTemplate
// 是账号侧 ext 边的唯一 eager-load 调用点；BPS 位错配**上抛**（唯一允许的错误），
// 其余投影错误沿用旧 mapper 的"Ext = nil、不报错"。
func TestPGProjectAccountExtEdgeErrorSurface(t *testing.T) {
	repos := newPGReposShared(t)
	conn := pgSharedConn(t)
	ctx := context.Background()

	tplBPS := u39CreateTemplate(t, repos, "u39-bps-edge", credential.TypeBPS, domain.FormatOpenAIResponses)
	tplAPIKey := seedPGTemplate(t, repos)

	t.Run("合法 BPS 边照常投影", func(t *testing.T) {
		acc := u39CreateAccount(t, repos, tplBPS.ID, "u39-edge-bps-ok")
		u39InsertExt(t, conn, u39ExtRow{
			AccountID: acc, Type: credential.TypeBPS, Identity: u39BPSIdentity("p-edge"),
			OAuthToken: "at", OAuthRefresh: "rt", CodexAccountID: "acct",
		})
		got, err := repos.Accounts.GetAccountWithTemplate(ctx, acc)
		require.NoError(t, err)
		require.NotNil(t, got.Ext)
		require.NotNil(t, got.Ext.BPSProfile)
	})

	t.Run("api_key 模板 + codex-pat 遗留边保留行类型", func(t *testing.T) {
		acc := u39CreateAccount(t, repos, tplAPIKey.ID, "u39-edge-legacy")
		u39InsertExt(t, conn, u39ExtRow{
			AccountID: acc, Type: credential.TypeCodexPAT,
			Identity: `{"installation_id":"inst-edge"}`, PATKey: "pat-edge",
		})
		got, err := repos.Accounts.GetAccountWithTemplate(ctx, acc)
		require.NoError(t, err)
		require.NotNil(t, got.Ext)
		require.Equal(t, credential.TypeCodexPAT, got.Ext.CredentialType)
		require.NotNil(t, got.Ext.CodexIdentity)
	})

	t.Run("BPS 位错配：上抛 ErrCredentialTypeMismatch", func(t *testing.T) {
		acc := u39CreateAccount(t, repos, tplBPS.ID, "u39-edge-dirty")
		u39InsertExt(t, conn, u39ExtRow{
			AccountID: acc, Type: credential.TypeBPS, Identity: u39BPSIdentity("p"),
			OAuthToken: "at", OAuthRefresh: "rt", CodexAccountID: "acct",
		})
		u39RetypeExt(t, conn, acc, credential.TypeCodexPAT)
		_, err := repos.Accounts.GetAccountWithTemplate(ctx, acc)
		require.ErrorIs(t, err, repository.ErrCredentialTypeMismatch)
	})

	t.Run("坏 BPS profile：Ext = nil、不报错", func(t *testing.T) {
		acc := u39CreateAccount(t, repos, tplBPS.ID, "u39-edge-bad-profile")
		u39InsertExt(t, conn, u39ExtRow{
			AccountID: acc, Type: credential.TypeBPS, Identity: `{"bps":[]}`,
			OAuthToken: "at", OAuthRefresh: "rt", CodexAccountID: "acct",
		})
		got, err := repos.Accounts.GetAccountWithTemplate(ctx, acc)
		require.NoError(t, err, "投影错误不许扩大成读错误（rule_persist 的判决要照常落库）")
		require.Nil(t, got.Ext)
	})

	t.Run("validateBPSExt 失败：Ext = nil、不报错", func(t *testing.T) {
		acc := u39CreateAccount(t, repos, tplBPS.ID, "u39-edge-bad-shape")
		u39InsertExt(t, conn, u39ExtRow{
			AccountID: acc, Type: credential.TypeBPS, Identity: u39BPSIdentity("p"),
			OAuthToken: nil, OAuthRefresh: "rt", CodexAccountID: "acct",
		})
		got, err := repos.Accounts.GetAccountWithTemplate(ctx, acc)
		require.NoError(t, err)
		require.Nil(t, got.Ext)
	})
}
