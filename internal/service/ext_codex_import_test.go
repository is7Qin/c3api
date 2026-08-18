// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/repository"
)

// —— codex 凭据批量导入 service 层（fakeStore——幂等矩阵/行级失败/事务回滚/
// 缺省断言/invalidate 批末一次；真实 PG 见 codex_import_pg_test.go） ——

// importFixture 组装导入用模板/组/服务（invRecorder 断言 invalidate 批末一次）。
func importFixture(t *testing.T) (*Service, *fakeStore, *invRecorder) {
	t.Helper()
	store := newFakeStore()
	tpl := &domain.Template{ID: 1, Name: "t", CredentialType: credential.TypeCodexOAuth}
	store.tpls[1] = tpl
	g := &domain.Group{ID: 7, Name: "g", Visibility: domain.GroupVisibilityPublic}
	store.groups[7] = g
	rec := &invRecorder{}
	svc := New(store, nil, rec, nil, nil, nil, nil)
	return svc, store, rec
}

func mcPtr(v int) *int { return &v }
func wPtr(v int) *int  { return &v }

// TestImportCodexOAuthAccountsIdempotent 幂等矩阵（oauth）：新建 → imported；
// 同键重导 → updated（凭据更新、身份沿用、并发/权重不动）；不同键共存；批内
// 同键后者胜。
func TestImportCodexOAuthAccountsIdempotent(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := importFixture(t)

	tplID := int64(1)
	gid := int64(7)

	t.Run("new key imported with identity and defaults", func(t *testing.T) {
		res, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
			{CodexEmail: "a@example.com", CodexAccountID: "acc-1",
				CodexOAuthToken: "at-1", CodexOAuthRefreshToken: "rt-1"},
		}, &tplID, &gid)
		require.NoError(t, err)
		require.Equal(t, 1, res.Imported)
		require.Equal(t, 0, res.Updated)
		require.Empty(t, res.Failed)

		ext, err := store.FindAccountExtByCodexKey(ctx, "a@example.com", "acc-1")
		require.NoError(t, err)
		require.Equal(t, credential.TypeCodexOAuth, ext.CredentialType)
		require.Equal(t, "at-1", *ext.CodexOAuthToken)
		require.Equal(t, "rt-1", *ext.CodexOAuthRefreshToken)
		require.NotNil(t, ext.CodexIdentity)
		require.NotEmpty(t, ext.CodexIdentity.InstallationID, "身份自动生成")
		require.Equal(t, ext.CodexIdentity.SessionID, ext.CodexIdentity.ThreadID, "thread==session 恒等")
		require.Equal(t, ext.CodexIdentity.ThreadID+":0", ext.CodexIdentity.WindowID)

		acc, err := store.GetAccount(ctx, ext.AccountID)
		require.NoError(t, err)
		require.Equal(t, "a@example.com", acc.Name, "name = codex_email")
		require.Equal(t, 25, acc.MaxConcurrency, "max_concurrency 缺省 25（非 8——用户裁决红绿）")
		require.Equal(t, 100, acc.Weight, "weight 缺省 100")
		require.Equal(t, tplID, acc.TemplateID)
		gs, err := store.GetAccountGroups(ctx, acc.ID)
		require.NoError(t, err)
		require.Equal(t, []int64{gid}, gs, "group_id 传入 → 归组")
	})

	t.Run("same key re-import updated credentials only", func(t *testing.T) {
		res, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
			{CodexEmail: "a@example.com", CodexAccountID: "acc-1",
				CodexOAuthToken: "at-2", CodexOAuthRefreshToken: "rt-2"},
		}, &tplID, nil)
		require.NoError(t, err)
		require.Equal(t, 0, res.Imported)
		require.Equal(t, 1, res.Updated)
		require.Empty(t, res.Failed)

		ext, err := store.FindAccountExtByCodexKey(ctx, "a@example.com", "acc-1")
		require.NoError(t, err)
		require.Equal(t, "at-2", *ext.CodexOAuthToken, "凭据更新")
		require.Equal(t, "rt-2", *ext.CodexOAuthRefreshToken)
		require.NotNil(t, ext.CodexIdentity, "身份沿用（部分更新零触碰）")
		acc, err := store.GetAccount(ctx, ext.AccountID)
		require.NoError(t, err)
		require.Equal(t, 25, acc.MaxConcurrency, "并发不动")
		require.Equal(t, 100, acc.Weight, "权重不动")
		gs, err := store.GetAccountGroups(ctx, acc.ID)
		require.NoError(t, err)
		require.Equal(t, []int64{gid}, gs, "归属不动（updated 不触碰）")
	})

	t.Run("different keys coexist", func(t *testing.T) {
		res, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
			{CodexEmail: "a@example.com", CodexAccountID: "acc-2",
				CodexOAuthToken: "at-3", CodexOAuthRefreshToken: "rt-3"},
			{CodexEmail: "b@example.com", CodexAccountID: "acc-1",
				CodexOAuthToken: "at-4", CodexOAuthRefreshToken: "rt-4"},
		}, &tplID, nil)
		require.NoError(t, err)
		require.Equal(t, 2, res.Imported)
		require.Equal(t, 0, res.Updated)
		require.Empty(t, res.Failed)
		for _, key := range [][2]string{{"a@example.com", "acc-2"}, {"b@example.com", "acc-1"}} {
			_, err := store.FindAccountExtByCodexKey(ctx, key[0], key[1])
			require.NoError(t, err, "组合键语义：同 email 多 account_id / 同 account_id 多 email 共存")
		}
	})

	t.Run("batch duplicate keys last wins", func(t *testing.T) {
		res, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
			{CodexEmail: "c@example.com", CodexAccountID: "acc-9",
				CodexOAuthToken: "at-first", CodexOAuthRefreshToken: "rt-first"},
			{CodexEmail: "c@example.com", CodexAccountID: "acc-9",
				CodexOAuthToken: "at-last", CodexOAuthRefreshToken: "rt-last"},
		}, &tplID, nil)
		require.NoError(t, err)
		require.Equal(t, 1, res.Imported)
		require.Equal(t, 1, res.Updated)
		require.Empty(t, res.Failed)
		ext, err := store.FindAccountExtByCodexKey(ctx, "c@example.com", "acc-9")
		require.NoError(t, err)
		require.Equal(t, "at-last", *ext.CodexOAuthToken, "批内同键逐行顺序应用后者胜")
		require.Equal(t, "rt-last", *ext.CodexOAuthRefreshToken)
	})
}

// TestImportCodexPATAccountsIdempotent pat 幂等矩阵（对称：新建 → imported /
// 同键重导 → updated 仅 pat_key / 跨类型同键 → 行级 failed）。
func TestImportCodexPATAccountsIdempotent(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := importFixture(t)
	// pat 端点模板（类型按端点——模板存在性即校验面）
	store.tpls[2] = &domain.Template{ID: 2, Name: "tp", CredentialType: credential.TypeCodexPAT}
	tplID, gid := int64(2), int64(7)

	res, err := svc.ImportCodexPATAccounts(ctx, []domain.CodexPATImportItem{
		{CodexEmail: "p@example.com", CodexAccountID: "p-1", CodexPATKey: "pat-1"},
	}, &tplID, &gid)
	require.NoError(t, err)
	require.Equal(t, 1, res.Imported)
	require.Empty(t, res.Failed)
	ext, err := store.FindAccountExtByCodexKey(ctx, "p@example.com", "p-1")
	require.NoError(t, err)
	require.Equal(t, credential.TypeCodexPAT, ext.CredentialType)
	require.Equal(t, "pat-1", *ext.CodexPATKey)
	require.Nil(t, ext.CodexOAuthToken, "pat 行零 oauth 列")
	identity := ext.CodexIdentity

	res, err = svc.ImportCodexPATAccounts(ctx, []domain.CodexPATImportItem{
		{CodexEmail: "p@example.com", CodexAccountID: "p-1", CodexPATKey: "pat-2",
			MaxConcurrency: mcPtr(3), Weight: wPtr(50)},
	}, &tplID, nil)
	require.NoError(t, err)
	require.Equal(t, 0, res.Imported)
	require.Equal(t, 1, res.Updated)
	require.Empty(t, res.Failed)
	ext, err = store.FindAccountExtByCodexKey(ctx, "p@example.com", "p-1")
	require.NoError(t, err)
	require.Equal(t, "pat-2", *ext.CodexPATKey, "pat_key 更新")
	require.Equal(t, identity, ext.CodexIdentity, "身份沿用（部分更新零触碰）")
	acc, err := store.GetAccount(ctx, ext.AccountID)
	require.NoError(t, err)
	require.Equal(t, 25, acc.MaxConcurrency, "updated 并发不动（显式传 3 也不动——配置面分离）")
	require.Equal(t, 100, acc.Weight, "updated 权重不动")

	// 跨类型同键：oauth 端点命中 pat 行 → 行级 failed（不跨类型混写）；
	// oauth 端点须配 oauth 模板（模板类型匹配校验——template 1 为 codex-oauth）
	oauthTplID := int64(1)
	res, err = svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
		{CodexEmail: "p@example.com", CodexAccountID: "p-1",
			CodexOAuthToken: "at-x", CodexOAuthRefreshToken: "rt-x"},
	}, &oauthTplID, nil)
	require.NoError(t, err)
	require.Equal(t, 0, res.Imported)
	require.Equal(t, 0, res.Updated)
	require.Len(t, res.Failed, 1)
	require.Equal(t, 0, res.Failed[0].Index, "failed index = items 原始下标")
	require.Contains(t, res.Failed[0].Error, "凭据类型不匹配")
	ext, err = store.FindAccountExtByCodexKey(ctx, "p@example.com", "p-1")
	require.NoError(t, err)
	require.Equal(t, credential.TypeCodexPAT, ext.CredentialType, "跨类型不混写（oauth 列零写入）")
	require.Nil(t, ext.CodexOAuthToken)
}

// TestImportCodexRowLevelFailures 行级失败：混合批计数正确 + failed index/error；
// 校验矩阵（必填缺失/成对/expires 格式/email 格式/weight 负值 → 行级 failed）；
// group_id 不存在 → 行级 failed（整体回滚无孤儿）。
func TestImportCodexRowLevelFailures(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := importFixture(t)
	tplID := int64(1)

	t.Run("mixed batch counts and indices", func(t *testing.T) {
		res, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
			{CodexEmail: "ok1@example.com", CodexAccountID: "a1",
				CodexOAuthToken: "at", CodexOAuthRefreshToken: "rt"},
			{CodexEmail: "bad-email", CodexAccountID: "a2", // index 1：email 非法
				CodexOAuthToken: "at", CodexOAuthRefreshToken: "rt"},
			{CodexEmail: "ok2@example.com", CodexAccountID: "a3",
				CodexOAuthToken: "at", CodexOAuthRefreshToken: "rt"},
			{CodexEmail: "ok3@example.com", CodexAccountID: "a4", // index 3：成对破坏
				CodexOAuthToken: "at"},
		}, &tplID, nil)
		require.NoError(t, err)
		require.Equal(t, 2, res.Imported)
		require.Equal(t, 0, res.Updated)
		require.Len(t, res.Failed, 2)
		require.Equal(t, 1, res.Failed[0].Index)
		require.Contains(t, res.Failed[0].Error, "codex_email")
		require.Equal(t, 3, res.Failed[1].Index)
		require.Contains(t, res.Failed[1].Error, "成对")
		// 失败行不落库；成功行落库
		_, err = store.FindAccountExtByCodexKey(ctx, "bad-email", "a2")
		require.ErrorIs(t, err, repository.ErrNotFound)
		_, err = store.FindAccountExtByCodexKey(ctx, "ok2@example.com", "a3")
		require.NoError(t, err)
	})

	t.Run("validation matrix", func(t *testing.T) {
		expiresBad := "not-a-time"
		res, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
			{CodexEmail: "v1@example.com", CodexAccountID: "v1",
				CodexOAuthToken: "at", CodexOAuthRefreshToken: "rt", CodexOAuthExpiresAt: &expiresBad},
			{CodexEmail: "v2@example.com", CodexAccountID: "", // account_id 必填
				CodexOAuthToken: "at", CodexOAuthRefreshToken: "rt"},
			{CodexEmail: "", CodexAccountID: "v3", // email 必填
				CodexOAuthToken: "at", CodexOAuthRefreshToken: "rt"},
			{CodexEmail: "v4@example.com", CodexAccountID: "v4",
				CodexOAuthToken: "at", CodexOAuthRefreshToken: "rt", Weight: wPtr(-1)},
		}, &tplID, nil)
		require.NoError(t, err)
		require.Equal(t, 0, res.Imported)
		require.Len(t, res.Failed, 4)
		require.Contains(t, res.Failed[0].Error, "RFC3339")
		require.Contains(t, res.Failed[1].Error, "codex_account_id")
		require.Contains(t, res.Failed[2].Error, "codex_email")
		require.Contains(t, res.Failed[3].Error, "weight")
	})

	t.Run("expires valid RFC3339 accepted", func(t *testing.T) {
		exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		res, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
			{CodexEmail: "v5@example.com", CodexAccountID: "v5",
				CodexOAuthToken: "at", CodexOAuthRefreshToken: "rt", CodexOAuthExpiresAt: &exp},
		}, &tplID, nil)
		require.NoError(t, err)
		require.Equal(t, 1, res.Imported)
		require.Empty(t, res.Failed)
		ext, err := store.FindAccountExtByCodexKey(ctx, "v5@example.com", "v5")
		require.NoError(t, err)
		require.NotNil(t, ext.CodexOAuthExpiresAt)
		require.Equal(t, exp, ext.CodexOAuthExpiresAt.UTC().Format(time.RFC3339))
	})

	t.Run("missing group row failed no orphan", func(t *testing.T) {
		missingGid := int64(999)
		res, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
			{CodexEmail: "g1@example.com", CodexAccountID: "g1",
				CodexOAuthToken: "at", CodexOAuthRefreshToken: "rt"},
		}, &tplID, &missingGid)
		require.NoError(t, err)
		require.Equal(t, 0, res.Imported)
		require.Len(t, res.Failed, 1)
		require.Contains(t, res.Failed[0].Error, "missing")
		// 单行事务整体回滚：无 account 行无 ext 行（无孤儿）
		_, err = store.FindAccountExtByCodexKey(ctx, "g1@example.com", "g1")
		require.ErrorIs(t, err, repository.ErrNotFound, "ext 无残留")
		for _, a := range store.accs {
			require.NotEqual(t, "g1@example.com", a.Name, "account 无残留")
		}
	})
}

// TestImportCodexTxRollback 单行事务回滚：事务内 ext 写入失败（注入）→ 无
// account 行无 ext 行（无孤儿——不搞补偿软删）。
func TestImportCodexTxRollback(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := importFixture(t)
	store.txUpsertExtErr = errors.New("injected ext failure")
	tplID := int64(1)

	res, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
		{CodexEmail: "rb@example.com", CodexAccountID: "rb",
			CodexOAuthToken: "at", CodexOAuthRefreshToken: "rt"},
	}, &tplID, nil)
	require.NoError(t, err)
	require.Equal(t, 0, res.Imported)
	require.Len(t, res.Failed, 1)
	require.Contains(t, res.Failed[0].Error, "injected")
	_, err = store.FindAccountExtByCodexKey(ctx, "rb@example.com", "rb")
	require.ErrorIs(t, err, repository.ErrNotFound, "ext 回滚")
	for _, a := range store.accs {
		require.NotEqual(t, "rb@example.com", a.Name, "account 回滚（无孤儿）")
	}
}

// TestImportCodexTemplateTypeMismatch 模板 credential_type 与端点类型错配 →
// 400 整批拒绝（task review Important 1——防违反 ext 类型 == 模板类型硬不变量）。
func TestImportCodexTemplateTypeMismatch(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := importFixture(t)
	// pat 类型模板（oauth 端点错误配用）
	store.tpls[2] = &domain.Template{ID: 2, Name: "pat-tpl", CredentialType: credential.TypeCodexPAT}
	patTpl := int64(2)

	t.Run("oauth endpoint with pat template rejected", func(t *testing.T) {
		_, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
			{CodexEmail: "m@example.com", CodexAccountID: "m",
				CodexOAuthToken: "at", CodexOAuthRefreshToken: "rt"},
		}, &patTpl, nil)
		require.ErrorIs(t, err, ErrInvalidInput)
		require.Contains(t, err.Error(), "模板类型不匹配")
	})

	t.Run("pat endpoint with oauth template rejected", func(t *testing.T) {
		oauthTpl := int64(1)
		_, err := svc.ImportCodexPATAccounts(ctx, []domain.CodexPATImportItem{
			{CodexEmail: "m@example.com", CodexAccountID: "m", CodexPATKey: "pat"},
		}, &oauthTpl, nil)
		require.ErrorIs(t, err, ErrInvalidInput)
		require.Contains(t, err.Error(), "模板类型不匹配")
	})

	t.Run("matching template accepted", func(t *testing.T) {
		oauthTpl := int64(1)
		res, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
			{CodexEmail: "m@example.com", CodexAccountID: "m",
				CodexOAuthToken: "at", CodexOAuthRefreshToken: "rt"},
		}, &oauthTpl, nil)
		require.NoError(t, err)
		require.Equal(t, 1, res.Imported)
	})
}

// TestImportCodexSoftDeletedAccount 软删账号重导入 → 行级 failed + 账号仍软删
// （task review Minor 1——删除意图权威，不自动复活；updated 只对存活账号）。
func TestImportCodexSoftDeletedAccount(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := importFixture(t)
	tplID := int64(1)

	// 先导入成功
	res, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
		{CodexEmail: "del@example.com", CodexAccountID: "del",
			CodexOAuthToken: "at-1", CodexOAuthRefreshToken: "rt-1"},
	}, &tplID, nil)
	require.NoError(t, err)
	require.Equal(t, 1, res.Imported)
	ext, err := store.FindAccountExtByCodexKey(ctx, "del@example.com", "del")
	require.NoError(t, err)

	// 软删（fake 硬删——直接置 DeletedAt 模拟真实 repo 软删形态）
	now := time.Now()
	store.mu.Lock()
	store.accs[ext.AccountID].DeletedAt = &now
	store.mu.Unlock()

	// 重导 → 行级 failed + 账号仍软删（凭据不更新）
	res, err = svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
		{CodexEmail: "del@example.com", CodexAccountID: "del",
			CodexOAuthToken: "at-2", CodexOAuthRefreshToken: "rt-2"},
	}, &tplID, nil)
	require.NoError(t, err)
	require.Equal(t, 0, res.Imported)
	require.Equal(t, 0, res.Updated)
	require.Len(t, res.Failed, 1)
	require.Contains(t, res.Failed[0].Error, "账号已删除")
	acc, err := store.GetAccount(ctx, ext.AccountID)
	require.NoError(t, err)
	require.NotNil(t, acc.DeletedAt, "账号仍软删（不自动复活）")
	ext2, err := store.FindAccountExtByCodexKey(ctx, "del@example.com", "del")
	require.NoError(t, err)
	require.Equal(t, "at-1", *ext2.CodexOAuthToken, "凭据未更新（updated 只对存活账号）")
}

// TestImportCodexTopLevelValidation template_id 缺/不存在 → 400/404；invalidate/
// publish 批末一次（成功行）或零次（全失败）。
func TestImportCodexTopLevelValidation(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := importFixture(t)

	t.Run("template id missing 400", func(t *testing.T) {
		_, err := svc.ImportCodexOAuthAccounts(ctx, nil, nil, nil)
		require.ErrorIs(t, err, ErrInvalidInput)
		_, err = svc.ImportCodexPATAccounts(ctx, nil, nil, nil)
		require.ErrorIs(t, err, ErrInvalidInput)
	})

	t.Run("template missing 404", func(t *testing.T) {
		id := int64(999)
		_, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
			{CodexEmail: "x@example.com", CodexAccountID: "x", CodexOAuthToken: "at", CodexOAuthRefreshToken: "rt"},
		}, &id, nil)
		require.ErrorIs(t, err, ErrNotFound)
	})
}

// TestImportCodexInvalidateOnce 批末 invalidate/publish 一次（成功行并集 gids；
// 全失败零次）。
func TestImportCodexInvalidateOnce(t *testing.T) {
	ctx := context.Background()
	svc, _, rec := importFixture(t)
	tplID, gid := int64(1), int64(7)

	// 成功批：imported（归组 7）+ updated（已有分组 7——updated 行取既有分组）
	res, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
		{CodexEmail: "i1@example.com", CodexAccountID: "i1", CodexOAuthToken: "at", CodexOAuthRefreshToken: "rt"},
	}, &tplID, &gid)
	require.NoError(t, err)
	require.Equal(t, 1, res.Imported)

	res, err = svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
		{CodexEmail: "i1@example.com", CodexAccountID: "i1", CodexOAuthToken: "at2", CodexOAuthRefreshToken: "rt2"},
	}, &tplID, nil)
	require.NoError(t, err)
	require.Equal(t, 1, res.Updated)

	require.Equal(t, 2, rec.total(), "每批一次（imported 批 + updated 批）")
	last := rec.last()
	require.Equal(t, "accounts", last.kind)
	require.ElementsMatch(t, []int64{gid}, last.gids, "updated 行既有分组并入 gids（凭据变更须重载调度器组快照）")
	require.False(t, last.key, "凭据变更不走 clients 失效（sig 比对重建机制）")

	// 全失败批：invalidate 零次
	store2 := newFakeStore()
	store2.tpls[1] = &domain.Template{ID: 1, Name: "t", CredentialType: credential.TypeCodexOAuth}
	rec2 := &invRecorder{}
	svc2 := New(store2, nil, rec2, nil, nil, nil, nil)
	bad := "bad"
	_, err = svc2.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
		{CodexEmail: "z@example.com", CodexAccountID: "z", CodexOAuthToken: "at", CodexOAuthRefreshToken: "rt", CodexOAuthExpiresAt: &bad},
	}, &tplID, nil)
	require.NoError(t, err)
	require.Equal(t, 0, rec2.total(), "全失败批 invalidate 零次")
}

// TestImportCodexPublishOnce publish 批末一次（Change{Groups: gids}）。
func TestImportCodexPublishOnce(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := importFixture(t)
	pub := &notifyRecorder{}
	svc.pub = pub
	tplID, gid := int64(1), int64(7)

	_, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
		{CodexEmail: "pub@example.com", CodexAccountID: "pub", CodexOAuthToken: "at", CodexOAuthRefreshToken: "rt"},
	}, &tplID, &gid)
	require.NoError(t, err)
	require.Equal(t, 1, len(pub.changes))
	require.Equal(t, []int64{gid}, pub.changes[0].Groups)

	// 全失败批 publish 零次（空 Change 判空跳过）
	pub.changes = nil
	bad := "bad"
	_, err = svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
		{CodexEmail: "pub2@example.com", CodexAccountID: "pub2", CodexOAuthToken: "at", CodexOAuthRefreshToken: "rt", CodexOAuthExpiresAt: &bad},
	}, &tplID, nil)
	require.NoError(t, err)
	require.Empty(t, pub.changes)
}

// notifyRecorder 记录 publish 调用的测试假件。
type notifyRecorder struct{ changes []notify.Change }

func (n *notifyRecorder) Publish(_ context.Context, ch notify.Change) error {
	n.changes = append(n.changes, ch)
	return nil
}
