// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// #18 ent IN 列表 >65,535 参数崩溃修复——真实 PG 集成测试。
//
// 事故（O3 压测实证）：fill 数据累计 847k 组 → ent eager-load（WithAccounts/
// QueryAccounts/WithUser）生成 `WHERE id IN (…847k 参数…)` 超 PostgreSQL 参数
// 上限 65535（错误 54001 "too many parameters"）——启动 fatal、运行中静默失败
// 返回空/部分结果。本文件验证修复后：
//   - LoadGroupsAccounts（组侧全表扫描 + 成员关系全表扫描，零 IN 参数）在
//     组/账号都 >65,535 时结果完整；
//   - LoadGroupAccounts（EXISTS 子查询，无账号 IN 跳）在单组账号 >65,535 时
//     结果完整；
//   - LoadKeys（key id 分片）跨分片边界加载完整；
//   - DeactivateCodes（分片 UPDATE）>65,535 ids 全量失效成功。
//
// 边界设计：>65,535（66,000，修复前必崩）与临界 65,500（修复前恰好通过，
// 防回归）。

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/account"
	"github.com/is7qin/c3api/internal/ent/group"
	"github.com/is7qin/c3api/internal/ent/key"
	"github.com/is7qin/c3api/internal/ent/user"
	"github.com/is7qin/c3api/internal/repository"
)

// pgExecPool 额外连接池（成员关系/断言 raw SQL 用；与 newPGRepos 同一 DSN——
// 测试库每测试重建 schema，语句互不依赖连接）。
func pgExecPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	pool, err := repository.OpenPG(context.Background(), dsn, 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// fillBatchSize CreateBulk 分批大小：单条多行 INSERT 的列数 × 行数必须留出
// 65535 参数上限余量（组 2 列 → 8192 行 16k 参数；账号 7 列 → 4096 行 29k
// 参数；4096 对两者都安全）。
const fillBatchSize = 4096

// fillGroupsAccounts 填充 n 组 + n 账号：账号 i 属组 i（1:1），另全部账号加入
// 专用 hub 组（1:1 范围外，避免成员行重复主键）。返回 (组 id 起值, 账号 id
// 起值, hub 组 id)——id 为自增连续（每测试重建 schema），成员关系经 raw SQL
// `SELECT id, (id-起值)+偏移` 一次写入。
// 注意：CreateBulk 自身也受 65535 参数上限约束（组 2 列、账号 ~7 列），故按
// fillBatchSize 分批——这正说明生产中任何"全量单语句"构造都会触顶，仓库层
// 加载必须零 IN/分片。
func fillGroupsAccounts(t *testing.T, repos *repository.Repository, tplID int64, n int) (firstGroupID, firstAccountID, hubID int64) {
	t.Helper()
	ctx := context.Background()
	var groups []*ent.Group
	for lo := 0; lo < n; lo += fillBatchSize {
		hi := min(lo+fillBatchSize, n)
		builders := make([]*ent.GroupCreate, 0, hi-lo)
		for i := lo; i < hi; i++ {
			builders = append(builders, repos.Client.Group.Create().
				SetName(fmt.Sprintf("pool-%d", i)).
				SetVisibility(group.VisibilityPublic))
		}
		rows, err := repos.Client.Group.CreateBulk(builders...).Save(ctx)
		require.NoError(t, err)
		groups = append(groups, rows...)
	}
	firstGroupID = groups[0].ID

	var accs []*ent.Account
	for lo := 0; lo < n; lo += fillBatchSize {
		hi := min(lo+fillBatchSize, n)
		builders := make([]*ent.AccountCreate, 0, hi-lo)
		for i := lo; i < hi; i++ {
			builders = append(builders, repos.Client.Account.Create().
				SetName(fmt.Sprintf("acc-%d", i)).
				SetTemplateID(tplID).
				SetUpstreamKey("sk-upstream").
				SetWeight(100).
				SetMaxConcurrency(100000).
				SetStatus(account.StatusActive))
		}
		rows, err := repos.Client.Account.CreateBulk(builders...).Save(ctx)
		require.NoError(t, err)
		accs = append(accs, rows...)
	}
	firstAccountID = accs[0].ID

	pool := pgExecPool(t)
	// 账号 i → 组 i（自增 id 连续：account_id - firstAccountID + firstGroupID）
	_, err := pool.Exec(ctx,
		`INSERT INTO account_groups (account_id, group_id) SELECT id, $1 + (id - $2) FROM accounts`,
		firstGroupID, firstAccountID)
	require.NoError(t, err)
	// 全部账号 → hub 组（专用组，不在 1:1 范围内——避免与 1:1 行重复主键）
	hub, err := repos.Client.Group.Create().
		SetName("hub").
		SetVisibility(group.VisibilityPublic).
		Save(ctx)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO account_groups (account_id, group_id) SELECT id, $1 FROM accounts`, hub.ID)
	require.NoError(t, err)
	return firstGroupID, firstAccountID, hub.ID
}

func TestPGLoadGroupsAccountsBeyondLimit(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)

	const n = 66000 // > PG 参数上限 65,535（修复前 LoadGroupsAccounts 必崩）
	firstGroupID, firstAccountID, hubID := fillGroupsAccounts(t, repos, tpl.ID, n)
	require.NotEqual(t, firstGroupID, hubID, "hub = 专用组（1:1 范围外）")
	// 无账号组：必须在快照中保留空条目（调度器 Select 区分"组不存在"与
	// "组无账号"——旧 eager-load 语义）
	emptyG, err := repos.Client.Group.Create().
		SetName("empty").SetVisibility(group.VisibilityPublic).Save(ctx)
	require.NoError(t, err)

	// 全量加载：组数、账号数双双超限，结果必须完整（count 断言）
	m, err := repos.Groups.LoadGroupsAccounts(ctx)
	require.NoError(t, err)
	require.Len(t, m, n+2, "组数 = 66002（旧实现此处 IN 66k 参数崩溃）")
	require.Contains(t, m, emptyG.ID, "无账号组在快照中（map 命中而非缺失）")
	require.Empty(t, m[emptyG.ID], "无账号组空条目")
	var total int
	for _, accs := range m {
		total += len(accs)
	}
	require.Equal(t, 2*n, total, "成员关系完整：每账号在 1:1 组 + hub 组")
	require.Len(t, m[hubID], n, "hub 组 = 全部账号")
	require.Len(t, m[firstGroupID+1], 1, "非 hub 组各 1 个账号")

	// 账号带模板（eager-load 语义保持）
	acc := m[hubID][0]
	require.NotNil(t, acc.Template, "账号必须带模板")
	require.Equal(t, tpl.BaseURL, acc.Template.BaseURL)

	// 单组加载：hub 组 66,000 账号 > 65,535（旧实现 QueryAccounts 邻接跳
	// `WHERE id IN (66k)` 崩溃）；结果无序（与旧实现同语义），按集合断言
	accs, err := repos.Groups.LoadGroupAccounts(ctx, hubID)
	require.NoError(t, err)
	require.Len(t, accs, n)
	ids := make(map[int64]struct{}, len(accs))
	for _, a := range accs {
		ids[a.ID] = struct{}{}
	}
	require.Len(t, ids, n, "无重复")
	require.Contains(t, ids, firstAccountID)
	require.Contains(t, ids, firstAccountID+int64(n)-1)

	// 空组语义：不存在的组 → 空集而非错误
	empty, err := repos.Groups.LoadGroupAccounts(ctx, firstGroupID-1)
	require.NoError(t, err)
	require.Empty(t, empty)
}

func TestPGLoadGroupsAccountsNearLimit(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)

	const n = 65500 // 临界值：恰好低于 65,535（修复前通过；防分片/扫描回归）
	firstGroupID, _, hubID := fillGroupsAccounts(t, repos, tpl.ID, n)

	m, err := repos.Groups.LoadGroupsAccounts(ctx)
	require.NoError(t, err)
	require.Len(t, m, n+1)
	require.Len(t, m[hubID], n)
	require.Len(t, m[firstGroupID+1], 1, "非 hub 组各 1 个账号")
}

func TestPGLoadKeysChunked(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	g := seedPGGroup(t, repos, "keys-group")

	const n = 9000 // > 单分片 8192：跨 2 块加载
	var users []*ent.User
	for lo := 0; lo < n; lo += fillBatchSize {
		hi := min(lo+fillBatchSize, n)
		builders := make([]*ent.UserCreate, 0, hi-lo)
		for i := lo; i < hi; i++ {
			builders = append(builders, repos.Client.User.Create().
				SetEmail(fmt.Sprintf("u%d@loadtest.test", i)).
				SetPasswordHash("x").
				SetRole(user.RoleUser).
				SetStatus(user.StatusActive).
				SetMaxConcurrency(8).
				SetBalance(0))
		}
		rows, err := repos.Client.User.CreateBulk(builders...).Save(ctx)
		require.NoError(t, err)
		users = append(users, rows...)
	}

	var keys []*ent.Key
	for lo := 0; lo < n; lo += fillBatchSize {
		hi := min(lo+fillBatchSize, n)
		builders := make([]*ent.KeyCreate, 0, hi-lo)
		for i := lo; i < hi; i++ {
			builders = append(builders, repos.Client.Key.Create().
				SetUserID(users[i].ID).
				SetGroupID(g.ID).
				SetName(fmt.Sprintf("load-%d", i)).
				SetKeyRaw(fmt.Sprintf("ck-%d", i)).
				SetStatus(key.StatusActive).
				SetMaxConcurrency(4).
				SetQuota(0).
				SetQuotaUsed(0))
		}
		rows, err := repos.Client.Key.CreateBulk(builders...).Save(ctx)
		require.NoError(t, err)
		keys = append(keys, rows...)
	}

	m, err := repos.Keys.LoadKeys(ctx)
	require.NoError(t, err)
	require.Len(t, m, n, "分片合并结果完整（9000 keys → 2 块）")
	// 抽查跨块边界的两条：归属用户状态/并发已 eager 填充
	first, last := keys[0], keys[n-1]
	for _, k := range []*ent.Key{first, last} {
		meta, ok := m[k.KeyRaw]
		require.True(t, ok)
		require.Equal(t, k.ID, meta.KeyID)
		require.Equal(t, k.UserID, meta.UserID)
		require.Equal(t, domain.UserStatusActive, meta.UserStatus, "用户状态经 m2o 边填充")
		require.Equal(t, 8, meta.UserMaxConc)
		require.Equal(t, 4, meta.KeyMaxConc)
	}
}

func TestPGDeactivateCodesChunked(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	const n = 66000 // > PG 参数上限：分片 UPDATE 必须全量失效
	codes := make([]*domain.RedemptionCode, 0, n)
	for i := 0; i < n; i++ {
		codes = append(codes, &domain.RedemptionCode{
			Code: fmt.Sprintf("CODE-%06d", i), Type: domain.RedemptionTypeBalance,
			Value: 1000, MaxUses: 1, UsedCount: 0,
			Status: domain.RedemptionStatusActive, CreatedBy: 0,
		})
	}
	// CreateCodes 单事务多行 INSERT 同样受 65535 参数上限约束，分批填充
	//（恰是本缺陷的同类形状；仓库层批量读修复不依赖写入侧）。
	for lo := 0; lo < n; lo += fillBatchSize {
		hi := min(lo+fillBatchSize, n)
		require.NoError(t, repos.CreateCodes(ctx, codes[lo:hi]))
	}

	ids := make([]int64, 0, n)
	for _, c := range codes {
		ids = append(ids, c.ID)
	}
	affected, err := repos.DeactivateCodes(ctx, ids)
	require.NoError(t, err)
	require.Equal(t, int64(n), affected, "分片失效受影响数 = 全部")
	// 幂等重放：全部已 disabled → 0
	affected, err = repos.DeactivateCodes(ctx, ids)
	require.NoError(t, err)
	require.Zero(t, affected, "重复失效 no-op")
}
