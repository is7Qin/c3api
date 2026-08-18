// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// ---------------------------------------------------------------------------
// W1 数据模型真实 PG 测试：template_ext / account_ext 2 张子表 CRUD roundtrip
// （幂等 upsert + NULL 清空 + FK 约束）+ groups.protocol_convert roundtrip。
// 基座见 pg_account_groups_test.go 的 newPGRepos（DROP SCHEMA 重建）。
// ---------------------------------------------------------------------------

func boolPtrPG(b bool) *bool { return &b }

func strPtrPG(s string) *string { return &s }

// TestTemplateExtPG 模板 ext：strip_image_tools（三类型公共能力开关）读写 +
// 幂等 upsert（同父 id 再写 = 单行覆盖，NULL 清空）+ FK（父模板缺失报错）+
// 缺行 404。模板 ext 无凭据列（oauth/pat 一律在 account_ext）。
func TestTemplateExtPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)

	t.Run("strip_image_tools roundtrip", func(t *testing.T) {
		saved, err := repos.TemplateExts.UpsertTemplateExt(ctx, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeResponsesSpecial,
			StripImageTools: boolPtrPG(true),
		})
		require.NoError(t, err)
		require.Equal(t, tpl.ID, saved.TemplateID)
		require.Equal(t, credential.TypeResponsesSpecial, saved.CredentialType)
		require.NotNil(t, saved.StripImageTools)
		require.True(t, *saved.StripImageTools)

		got, err := repos.TemplateExts.GetTemplateExt(ctx, tpl.ID)
		require.NoError(t, err)
		require.Equal(t, credential.TypeResponsesSpecial, got.CredentialType)
		require.True(t, *got.StripImageTools)

		// 幂等 upsert：再写（改值）→ 仍单行、值更新
		saved, err = repos.TemplateExts.UpsertTemplateExt(ctx, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeResponsesSpecial,
			StripImageTools: boolPtrPG(false),
		})
		require.NoError(t, err)
		require.False(t, *saved.StripImageTools)
		rows, err := repos.Client.TemplateExt.Query().Count(ctx)
		require.NoError(t, err)
		require.Equal(t, 1, rows, "同 template_id 幂等 upsert 恒单行")

		// NULL 清空：显式 nil → 落 NULL
		saved, err = repos.TemplateExts.UpsertTemplateExt(ctx, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeResponsesSpecial,
		})
		require.NoError(t, err)
		require.Nil(t, saved.StripImageTools, "nil 显式清列（NULL 落库）")
		got, err = repos.TemplateExts.GetTemplateExt(ctx, tpl.ID)
		require.NoError(t, err)
		require.Nil(t, got.StripImageTools)
	})

	t.Run("strip common across types", func(t *testing.T) {
		// oauth/pat 类型模板同可配 strip（三类型公共能力开关；仓库层不校验
		// 类型一致性——service 层负责）
		for _, ct := range []credential.Type{credential.TypeCodexOAuth, credential.TypeCodexPAT} {
			saved, err := repos.TemplateExts.UpsertTemplateExt(ctx, &domain.TemplateExt{
				TemplateID: tpl.ID, CredentialType: ct, StripImageTools: boolPtrPG(true),
			})
			require.NoError(t, err)
			require.Equal(t, ct, saved.CredentialType)
			require.True(t, *saved.StripImageTools, "type %s strip roundtrip", ct)
		}
	})

	t.Run("missing parent template FK", func(t *testing.T) {
		_, err := repos.TemplateExts.UpsertTemplateExt(ctx, &domain.TemplateExt{
			TemplateID: 999999, CredentialType: credential.TypeCodexOAuth,
		})
		require.Error(t, err, "FK：父模板缺失必须报错（service 层先查父行，仓库层约束兜底）")
	})

	t.Run("get missing 404", func(t *testing.T) {
		tpl2, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
			Name: "t2", BaseURL: "https://u/v1", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		})
		require.NoError(t, err)
		_, err = repos.TemplateExts.GetTemplateExt(ctx, tpl2.ID)
		require.Error(t, err)
		require.True(t, errors.Is(err, repository.ErrNotFound), "缺行 → ErrNotFound: %v", err)
	})
}

// TestAccountExtPG 账号 ext 两种 codex 类型各自读写 + FK + 缺行 404。
func TestAccountExtPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	acc := seedPGAccount(t, repos, tpl.ID, "a1")

	const iid = "11111111-2222-3333-4444-555555555555"

	t.Run("oauth roundtrip", func(t *testing.T) {
		exp := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
		saved, err := repos.AccountExts.UpsertAccountExt(ctx, &domain.AccountExt{
			AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
			CodexIdentity: &domain.CodexIdentity{InstallationID: iid}, CodexEmail: strPtrPG("user@example.com"),
			CodexOAuthToken: strPtrPG("at"), CodexOAuthRefreshToken: strPtrPG("rt"), CodexOAuthExpiresAt: &exp,
		})
		require.NoError(t, err)
		require.Equal(t, acc.ID, saved.AccountID)
		got, err := repos.AccountExts.GetAccountExt(ctx, acc.ID)
		require.NoError(t, err)
		require.Equal(t, credential.TypeCodexOAuth, got.CredentialType)
		require.Equal(t, iid, got.CodexIdentity.InstallationID, "installation_id 账号级恒稳 roundtrip")
		require.Equal(t, "user@example.com", *got.CodexEmail, "email roundtrip（人工/上游导入，非自动生成）")
		require.Equal(t, "at", *got.CodexOAuthToken)
		require.Equal(t, "rt", *got.CodexOAuthRefreshToken)
		require.True(t, exp.Equal(*got.CodexOAuthExpiresAt), "timestamptz roundtrip（时区无关比较）")
		require.Nil(t, got.CodexPATKey)
	})

	t.Run("pat roundtrip and oauth cleared", func(t *testing.T) {
		saved, err := repos.AccountExts.UpsertAccountExt(ctx, &domain.AccountExt{
			AccountID: acc.ID, CredentialType: credential.TypeCodexPAT,
			CodexIdentity: &domain.CodexIdentity{InstallationID: iid}, CodexPATKey: strPtrPG("pat"),
		})
		require.NoError(t, err)
		require.Equal(t, "pat", *saved.CodexPATKey)
		require.Nil(t, saved.CodexOAuthToken, "类型切换后 oauth 列组清空")
		require.Equal(t, iid, saved.CodexIdentity.InstallationID, "installation_id 不随类型切换变化")
		got, err := repos.AccountExts.GetAccountExt(ctx, acc.ID)
		require.NoError(t, err)
		require.Equal(t, credential.TypeCodexPAT, got.CredentialType)
		require.Equal(t, "pat", *got.CodexPATKey)
		require.Equal(t, iid, got.CodexIdentity.InstallationID)
	})

	t.Run("session columns roundtrip and clear", func(t *testing.T) {
		saved, err := repos.AccountExts.UpsertAccountExt(ctx, &domain.AccountExt{
			AccountID: acc.ID, CredentialType: credential.TypeCodexPAT,
			CodexIdentity: &domain.CodexIdentity{
				InstallationID: iid, SessionID: "s1", ThreadID: "t1", WindowID: "t1:0",
			},
			CodexPATKey: strPtrPG("pat"),
			CodexEmail:  strPtrPG("pat@example.com"),
		})
		require.NoError(t, err)
		require.Equal(t, "pat@example.com", *saved.CodexEmail)
		require.Equal(t, "s1", saved.CodexIdentity.SessionID)
		require.Equal(t, "t1", saved.CodexIdentity.ThreadID)
		require.Equal(t, "t1:0", saved.CodexIdentity.WindowID)
		// 会话轮换：写新会话 → 旧值清空（nil 显式清列）
		saved, err = repos.AccountExts.UpsertAccountExt(ctx, &domain.AccountExt{
			AccountID: acc.ID, CredentialType: credential.TypeCodexPAT,
			CodexIdentity: &domain.CodexIdentity{
				InstallationID: iid, SessionID: "s2", ThreadID: "t2", WindowID: "t2:0",
			},
			CodexPATKey: strPtrPG("pat"),
		})
		require.NoError(t, err)
		require.Equal(t, "s2", saved.CodexIdentity.SessionID)
		require.Equal(t, "t2", saved.CodexIdentity.ThreadID)
		got, err := repos.AccountExts.GetAccountExt(ctx, acc.ID)
		require.NoError(t, err)
		require.Equal(t, "t2:0", got.CodexIdentity.WindowID)
	})

	t.Run("missing parent account FK", func(t *testing.T) {
		_, err := repos.AccountExts.UpsertAccountExt(ctx, &domain.AccountExt{
			AccountID: 999999, CredentialType: credential.TypeCodexOAuth, CodexIdentity: &domain.CodexIdentity{InstallationID: iid},
		})
		require.Error(t, err, "FK：父账号缺失必须报错")
	})

	t.Run("get missing 404", func(t *testing.T) {
		acc2 := seedPGAccount(t, repos, tpl.ID, "a2")
		_, err := repos.AccountExts.GetAccountExt(ctx, acc2.ID)
		require.Error(t, err)
		require.True(t, errors.Is(err, repository.ErrNotFound), "缺行 → ErrNotFound: %v", err)
	})
}

// TestAccountExtTryInsertPG TryInsertAccountExt 首写原子性（I2）：先写者胜——
// 缺失 → 插入（true）；已存在 → 跳过不覆盖（false）；并发双首写 → 单份身份、
// 不报错、不覆盖。
func TestAccountExtTryInsertPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	acc := seedPGAccount(t, repos, tpl.ID, "a-try")

	const iidA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const iidB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

	// 首次插入 → true
	inserted, err := repos.AccountExts.TryInsertAccountExt(ctx, &domain.AccountExt{
		AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
		CodexIdentity: &domain.CodexIdentity{InstallationID: iidA}, CodexOAuthToken: strPtrPG("at"),
	})
	require.NoError(t, err)
	require.True(t, inserted, "首次插入成功")

	// 已存在 → false，身份保持先写者（不覆盖）
	inserted, err = repos.AccountExts.TryInsertAccountExt(ctx, &domain.AccountExt{
		AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
		CodexIdentity: &domain.CodexIdentity{InstallationID: iidB}, CodexOAuthToken: strPtrPG("at2"),
	})
	require.NoError(t, err)
	require.False(t, inserted, "冲突跳过（先写者胜）")
	got, err := repos.AccountExts.GetAccountExt(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, iidA, got.CodexIdentity.InstallationID, "先写者身份不覆盖")
	require.Equal(t, "at", *got.CodexOAuthToken, "先写者凭据不覆盖")

	// 并发双首写（不同生成身份）→ 单份身份、不报错
	acc2 := seedPGAccount(t, repos, tpl.ID, "a-try2")
	const iidC = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	const iidD = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	var wg sync.WaitGroup
	ins := make([]bool, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ins[i], errs[i] = repos.AccountExts.TryInsertAccountExt(ctx, &domain.AccountExt{
				AccountID: acc2.ID, CredentialType: credential.TypeCodexOAuth,
				CodexIdentity:   &domain.CodexIdentity{InstallationID: map[int]string{0: iidC, 1: iidD}[i]},
				CodexOAuthToken: strPtrPG("at"),
			})
		}(i)
	}
	wg.Wait()
	for i := 0; i < 2; i++ {
		require.NoError(t, errs[i], "并发首写不报错")
	}
	require.True(t, ins[0] != ins[1], "恰好一个成功一个幂等跳过（ins=%v）", ins)
	got, err = repos.AccountExts.GetAccountExt(ctx, acc2.ID)
	require.NoError(t, err)
	require.Contains(t, []string{iidC, iidD}, got.CodexIdentity.InstallationID, "单份身份（先写者之一）")
	rows, err := repos.Client.AccountExt.Query().Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, rows, "两账号各单行")
}

// TestGetTemplatesByIDsPG 批量取模板（I2：UpdateTemplatesBatch 类型-格式校验
// 用，替代逐 id N+1）：一次 IN 返回全部目标；缺失 id 不报错（数量 < 请求数）。
func TestGetTemplatesByIDsPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	a := seedPGTemplate(t, repos)
	b, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "t-b", BaseURL: "https://u/v1", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
	})
	require.NoError(t, err)

	got, err := repos.Templates.GetTemplatesByIDs(ctx, []int64{a.ID, b.ID})
	require.NoError(t, err)
	require.Len(t, got, 2, "一次 IN 返回全部目标模板")
	ids := map[int64]string{}
	for _, t := range got {
		ids[t.ID] = t.Name
	}
	require.Equal(t, a.Name, ids[a.ID])
	require.Equal(t, b.Name, ids[b.ID])

	// 缺失 id（含不存在）→ 不报错、数量 < 请求数（调用方按需对比）
	got, err = repos.Templates.GetTemplatesByIDs(ctx, []int64{a.ID, 999999})
	require.NoError(t, err)
	require.Len(t, got, 1, "缺失 id 不报错（数量对比由调用方做）")
	require.Equal(t, a.ID, got[0].ID)
}

// TestGroupProtocolConvertPG groups.protocol_convert 方向集合：JSON 数组存取
// roundtrip（多方向）+ 缺省 = 空数组（off）+ 更新覆盖/显式空数组清空。
func TestGroupProtocolConvertPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	// 缺省（nil）→ 空数组（off；repo 恒写入）
	g, err := repos.Groups.CreateGroup(ctx, &domain.Group{
		Name: "g-default", Visibility: domain.GroupVisibilityPublic,
		PriceMultiplier: 10000,
	})
	require.NoError(t, err)
	got, err := repos.Groups.GetGroup(ctx, g.ID)
	require.NoError(t, err)
	require.Empty(t, got.ProtocolConverts, "缺省 = 空数组（off）")

	// 四方向 JSON 数组 roundtrip
	all := []domain.ProtocolConvert{
		domain.ProtocolConvertChatToResp, domain.ProtocolConvertMessToResp,
		domain.ProtocolConvertRespToMess, domain.ProtocolConvertChatToMess,
	}
	g2, err := repos.Groups.CreateGroup(ctx, &domain.Group{
		Name: "g-multi", Visibility: domain.GroupVisibilityPublic,
		PriceMultiplier: 10000, ProtocolConverts: all,
	})
	require.NoError(t, err)
	got2, err := repos.Groups.GetGroup(ctx, g2.ID)
	require.NoError(t, err)
	require.Equal(t, all, got2.ProtocolConverts, "多方向 roundtrip（JSON 数组存取）")

	// 更新覆盖（单方向）
	g2.ProtocolConverts = []domain.ProtocolConvert{domain.ProtocolConvertRespToMess}
	updated, err := repos.Groups.UpdateGroup(ctx, g2)
	require.NoError(t, err)
	require.Equal(t, []domain.ProtocolConvert{domain.ProtocolConvertRespToMess}, updated.ProtocolConverts)
	got, err = repos.Groups.GetGroup(ctx, g2.ID)
	require.NoError(t, err)
	require.Equal(t, []domain.ProtocolConvert{domain.ProtocolConvertRespToMess}, got.ProtocolConverts, "更新生效")

	// 显式空数组 = 清空既有方向
	g2.ProtocolConverts = []domain.ProtocolConvert{}
	updated2, err := repos.Groups.UpdateGroup(ctx, g2)
	require.NoError(t, err)
	require.Empty(t, updated2.ProtocolConverts, "显式空数组 = 清空")
	got, err = repos.Groups.GetGroup(ctx, g2.ID)
	require.NoError(t, err)
	require.Empty(t, got.ProtocolConverts)
}

// ---------------------------------------------------------------------------
// T4 §3：账号 ext → 调度器快照 eager-load（Selection 扩展路线——热路径零 DB
// 的数据源；与 pg_strip_test.go 的 template_ext 快照合并同构）
// ---------------------------------------------------------------------------

// snapshotExtOf 从 LoadGroupsAccounts 全量快照取账号的 Ext。
func snapshotExtOf(t *testing.T, repos *repository.Repository, groupID, accountID int64) *domain.AccountExt {
	t.Helper()
	m, err := repos.Groups.LoadGroupsAccounts(context.Background())
	require.NoError(t, err)
	for _, a := range m[groupID] {
		if a.ID == accountID {
			return a.Ext
		}
	}
	t.Fatalf("快照必须含账号 %d（组 %d）", accountID, groupID)
	return nil
}

// TestPGAccountExtSnapshotLoad account_ext → 调度器快照 Account.Ext 合并：
// 全量（LoadGroupsAccounts）与组级（LoadGroupAccounts）两条数据源一致；无
// ext 行 → nil；ext 更新后重载反映新值（配置经快照重载生效，请求期零 DB）。
func TestPGAccountExtSnapshotLoad(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	g := seedPGGroup(t, repos, "g")
	acc := seedPGAccount(t, repos, tpl.ID, "a1")
	require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{g.ID}))

	const iid = "11111111-2222-3333-4444-555555555555"
	sess, thread, win := "s1", "t1", "t1:0"

	t.Run("no ext row is nil", func(t *testing.T) {
		require.Nil(t, snapshotExtOf(t, repos, g.ID, acc.ID), "无 ext 行 → Ext nil")
		members, err := repos.Groups.LoadGroupAccounts(ctx, g.ID)
		require.NoError(t, err)
		require.Len(t, members, 1)
		require.Nil(t, members[0].Ext, "组级重载数据源同语义")
	})

	t.Run("ext merged into snapshot", func(t *testing.T) {
		exp := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
		_, err := repos.AccountExts.UpsertAccountExt(ctx, &domain.AccountExt{
			AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
			CodexIdentity: &domain.CodexIdentity{
				InstallationID: iid, SessionID: sess, ThreadID: thread, WindowID: win,
			},
			CodexEmail:      strPtrPG("user@example.com"),
			CodexOAuthToken: strPtrPG("at"), CodexOAuthRefreshToken: strPtrPG("rt"), CodexOAuthExpiresAt: &exp,
		})
		require.NoError(t, err)
		got := snapshotExtOf(t, repos, g.ID, acc.ID)
		require.NotNil(t, got, "ext 行必须合并进全量快照")
		require.Equal(t, credential.TypeCodexOAuth, got.CredentialType)
		require.Equal(t, iid, got.CodexIdentity.InstallationID, "身份四元组落快照")
		require.Equal(t, sess, got.CodexIdentity.SessionID)
		require.Equal(t, thread, got.CodexIdentity.ThreadID)
		require.Equal(t, win, got.CodexIdentity.WindowID)
		require.Equal(t, "at", *got.CodexOAuthToken, "凭据材料落快照（热路径零 DB 数据源）")
		require.Equal(t, "rt", *got.CodexOAuthRefreshToken)
		require.True(t, exp.Equal(*got.CodexOAuthExpiresAt))
		members, err := repos.Groups.LoadGroupAccounts(ctx, g.ID)
		require.NoError(t, err)
		require.Equal(t, "at", *members[0].Ext.CodexOAuthToken, "组级重载数据源同语义")
	})

	t.Run("ext update reflected after reload", func(t *testing.T) {
		_, err := repos.AccountExts.UpsertAccountExt(ctx, &domain.AccountExt{
			AccountID: acc.ID, CredentialType: credential.TypeCodexPAT,
			CodexIdentity: &domain.CodexIdentity{InstallationID: iid}, CodexPATKey: strPtrPG("pat-new"),
		})
		require.NoError(t, err)
		got := snapshotExtOf(t, repos, g.ID, acc.ID)
		require.Equal(t, credential.TypeCodexPAT, got.CredentialType)
		require.Equal(t, "pat-new", *got.CodexPATKey, "ext 更新后重载快照反映新值")
		require.Nil(t, got.CodexOAuthToken, "类型切换 oauth 列组清空同语义")
	})
}

// TestAccountExtIdentityJSONBPG codex_identity jsonb 存取（本 task 存储形态）：
// 四元组 roundtrip 等值（序列化/解包双向一致）+ nil 身份（NULL → nil，upsert
// 冲突路径 ClearX 清空）+ 坏 json（手工 SQL 注入——应用路径不可达）→ ent 扫描
// 器 Unmarshal 报错原样透传（loud failure，非 nil 静默）。
func TestAccountExtIdentityJSONBPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	acc := seedPGAccount(t, repos, tpl.ID, "a-jsonb")

	const iid = "11111111-2222-3333-4444-555555555555"
	sess, thread, win := "s-jsonb", "t-jsonb", "t-jsonb:0"

	t.Run("four-value roundtrip", func(t *testing.T) {
		saved, err := repos.AccountExts.UpsertAccountExt(ctx, &domain.AccountExt{
			AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
			CodexIdentity: &domain.CodexIdentity{
				InstallationID: iid, SessionID: sess, ThreadID: thread, WindowID: win,
			},
			CodexOAuthToken: strPtrPG("at"),
		})
		require.NoError(t, err)
		require.Equal(t, iid, saved.CodexIdentity.InstallationID, "jsonb 落库后回读等值（installation）")
		got, err := repos.AccountExts.GetAccountExt(ctx, acc.ID)
		require.NoError(t, err)
		require.NotNil(t, got.CodexIdentity)
		require.Equal(t, &domain.CodexIdentity{InstallationID: iid, SessionID: sess, ThreadID: thread, WindowID: win},
			got.CodexIdentity, "身份四元组 jsonb roundtrip 全等（四值）")
	})

	t.Run("nil identity roundtrip", func(t *testing.T) {
		// 全量 upsert 写 nil 身份 → 冲突路径 ClearX 清 NULL；回读 nil
		saved, err := repos.AccountExts.UpsertAccountExt(ctx, &domain.AccountExt{
			AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
			CodexOAuthToken: strPtrPG("at2"),
		})
		require.NoError(t, err)
		require.Nil(t, saved.CodexIdentity, "nil 身份 → jsonb NULL")
		got, err := repos.AccountExts.GetAccountExt(ctx, acc.ID)
		require.NoError(t, err)
		require.Nil(t, got.CodexIdentity, "NULL → nil（生成层扫描器跳过 unmarshal）")
	})

	t.Run("bad json loud error", func(t *testing.T) {
		// 坏 jsonb 不可达：PG jsonb 列写入即校验（语法非法 → 写路径 loud 拒绝），
		// 应用路径 ent 恒写合法序列化——双保险。可注入的损坏形态 = 合法 json
		// 错类型（手工 SQL 场景，外部工具写库）→ ent 扫描器 json.Unmarshal
		// 报错 → 查询 error 原样透传（非 nil 静默）
		db := pgTestDB(t)
		_, err := db.ExecContext(ctx, `UPDATE account_exts SET codex_identity = '{"installation_id": 123}' WHERE account_id = $1`, acc.ID)
		require.NoError(t, err, "合法 json 错类型注入成功（测试前提）")
		_, err = repos.AccountExts.GetAccountExt(ctx, acc.ID)
		require.Error(t, err, "错类型 jsonb → 查询报错（loud failure）")
		require.NotErrorIs(t, err, repository.ErrNotFound, "必须是解包错误而非缺行")
		// 语法非法 json → PG 写路径直接拒绝（jsonb 列级校验）
		_, err = db.ExecContext(ctx, `UPDATE account_exts SET codex_identity = '{"installation_id": 123' WHERE account_id = $1`, acc.ID)
		require.Error(t, err, "语法非法 jsonb → PG 写路径 loud 拒绝")
	})
}

// TestAccountExtCodexAccountIDUniquePG 组合唯一索引 (codex_email,
// codex_account_id)：同键第二行失败（唯一约束）；不同 codex_account_id 共存；
// NULL 不参与唯一（同 email 双 NULL 行共存——存量管理面写入形态零回归）。
func TestAccountExtCodexAccountIDUniquePG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)

	seed := func(name, email string, accID *string) *domain.Account {
		t.Helper()
		acc := seedPGAccount(t, repos, tpl.ID, name)
		_, err := repos.AccountExts.UpsertAccountExt(ctx, &domain.AccountExt{
			AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
			CodexIdentity:   &domain.CodexIdentity{InstallationID: "i-" + name},
			CodexOAuthToken: strPtrPG("at"),
			CodexEmail:      strPtrPG(email),
			CodexAccountID:  accID,
		})
		require.NoError(t, err)
		return acc
	}

	// 不同键共存：同 email 不同 codex_account_id → 两行并存
	seed("u-a1", "a@example.com", strPtrPG("acc-1"))
	seed("u-a2", "a@example.com", strPtrPG("acc-2"))
	rows, err := repos.Client.AccountExt.Query().Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, rows, "同 email 不同 codex_account_id 共存")

	// 同键冲突：同 (email, codex_account_id) 不同账号第二行 → 唯一索引拒绝
	//（同账号重复写 = 幂等 upsert 收敛，不触发——见既有 upsert 测试）
	seed("u-b1", "b@example.com", strPtrPG("acc-1"))
	accB2 := seedPGAccount(t, repos, tpl.ID, "u-b2")
	_, err = repos.AccountExts.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: accB2.ID, CredentialType: credential.TypeCodexOAuth,
		CodexIdentity:   &domain.CodexIdentity{InstallationID: "i-b2"},
		CodexOAuthToken: strPtrPG("at"),
		CodexEmail:      strPtrPG("b@example.com"),
		CodexAccountID:  strPtrPG("acc-1"),
	})
	require.Error(t, err, "同 (codex_email, codex_account_id) 不同账号第二行必须唯一冲突")
	require.NotErrorIs(t, err, repository.ErrNotFound)
	// 冲突不落行（原行保持）
	_, err = repos.AccountExts.GetAccountExt(ctx, accB2.ID)
	require.ErrorIs(t, err, repository.ErrNotFound, "冲突写入零残留")

	// NULL 不参与唯一：同 email 双 codex_account_id NULL 共存（管理面既有形态）
	seed("u-c1", "c@example.com", nil)
	seed("u-c2", "c@example.com", nil)
	rows, err = repos.Client.AccountExt.Query().Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 5, rows, "同 email 双 NULL 键共存（NULL 不参与唯一）")
}
