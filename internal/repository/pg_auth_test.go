// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent/group"
	"github.com/is7qin/c3api/internal/ent/key"
	"github.com/is7qin/c3api/internal/ent/user"
	"github.com/is7qin/c3api/internal/repository"
)

// ---------------------------------------------------------------------------
// 真实 PostgreSQL 测试基座（评审延续：新表/新语义一律真实 PG）。
// 启动方式：
//   docker compose -f deploy/test-compose.yml up -d
//   TEST_DATABASE_URL=postgres://postgres:c3api@localhost:15432/c3api_test \
//     go test ./internal/repository/ -run PG -v
// 未设置 TEST_DATABASE_URL → t.Skip。
// ---------------------------------------------------------------------------

// seedPGUser 建用户（role 缺省 user；返回创建的 domain.User）。
func seedPGUser(t *testing.T, repos *repository.Repository, email string) *domain.User {
	t.Helper()
	u, err := repos.CreateUser(context.Background(), &domain.User{
		Email: email, PasswordHash: "bcrypt-hash-" + email,
		Role: domain.RoleUser, Status: domain.UserStatusActive, MaxConcurrency: 0,
	})
	require.NoError(t, err)
	return u
}

func TestPGUserCRUD(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	u := seedPGUser(t, repos, "a@example.com")
	require.True(t, u.ID > 0)
	require.Equal(t, domain.RoleUser, u.Role, "role 缺省 user")
	require.Equal(t, domain.UserStatusActive, u.Status, "status 缺省 active")
	require.Zero(t, u.Balance, "balance 默认 0")

	// GetByEmail（未找到 → nil,nil；找到 → 完整回读）
	got, err := repos.GetUserByEmail(ctx, "a@example.com")
	require.NoError(t, err)
	require.Equal(t, u.ID, got.ID)
	missing, err := repos.GetUserByEmail(ctx, "nope@example.com")
	require.NoError(t, err)
	require.Nil(t, missing)

	// Update（patch：role/status/max_concurrency/balance 全字段显式设置；
	// 旧值条件 = 创建快照——0 行/误判覆盖并发增量的回归网）
	oldMC, oldBal := u.MaxConcurrency, u.Balance
	role, st := domain.RolePlatformAdmin, domain.UserStatusDisabled
	mc, bal := 3, int64(12345)
	updated, err := repos.UpdateUser(ctx, &repository.UserPatch{
		ID: u.ID, Role: &role, Status: &st,
		MaxConcurrency: &mc, OldMaxConcurrency: &oldMC,
		Balance: &bal, OldBalance: &oldBal,
	})
	require.NoError(t, err)
	require.Equal(t, domain.RolePlatformAdmin, updated.Role)
	require.Equal(t, domain.UserStatusDisabled, updated.Status)
	require.Equal(t, 3, updated.MaxConcurrency)
	require.Equal(t, int64(12345), updated.Balance)

	// UpdateUserPassword（独立路径；不影响其他字段；单语句原子递增
	// token_version——改密即撤销该用户全部 JWT，spec 2026-08-25-jwt-password-
	// revocation §3/§5.3）
	require.NoError(t, repos.UpdateUserPassword(ctx, u.ID, "new-hash"))
	got2, err := repos.GetUser(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, "new-hash", got2.PasswordHash)
	require.Equal(t, domain.RolePlatformAdmin, got2.Role, "改密不动其他字段")
	require.Equal(t, int64(1), got2.TokenVersion, "改密原子递增撤销版本（创建默认 0 → 1）")
	// 二次改密：证明是 +1 递增而非 SET 固定值（并发双改密版本不回退）
	require.NoError(t, repos.UpdateUserPassword(ctx, u.ID, "newer-hash"))
	got3, err := repos.GetUser(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, int64(2), got3.TokenVersion, "二次改密继续 +1")
	require.Equal(t, "newer-hash", got3.PasswordHash)

	// ListUsers（email 过滤 + 分页 + sort 白名单）
	seedPGUser(t, repos, "b@example.com")
	rows, total, err := repos.ListUsers(ctx, repository.ListQuery{Email: "b@", Limit: 10})
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, rows, 1)
	require.Equal(t, "b@example.com", rows[0].Email)

	// LoadUsers 快照（status+role+token_version 单次查找：RequireJWT 状态校验
	// + 撤销版本比对 + adminAuth 快照 role 覆盖 claims 的数据源）
	states, err := repos.Users.LoadUsers(ctx)
	require.NoError(t, err)
	require.Contains(t, states, u.ID)
	require.Equal(t, domain.UserSnapshot{Status: domain.UserStatusDisabled, Role: domain.RolePlatformAdmin, TokenVersion: 2},
		states[u.ID], "用户禁用后快照反映（status/role/token_version 一并携带）")
}

// TestPGListUsersRoleAndSurfaceFilters 真实 PG：/users 的 role / supplier_surface
// 过滤矩阵（spec 2026-10-09 §4 #1–#9）——每行同时锁定 total 与 rows 身份（ID 集合 +
// 角色多重集）、surface 覆盖 role、未知值宽松空列表、大小写子串、筛选后分页的具体
// ID 与页间不重叠。
func TestPGListUsersRoleAndSurfaceFilters(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	// 三角色样本 + 大小写邮箱（含大小写不同的域名，验证 ContainsFold）。
	// 供应商 s2 置为 disabled：证明 role/surface 过滤不看 active（spec §4 #1）。
	s1 := seedPGUserRole(t, repos, "s1@Example.com", domain.RoleSupplier)
	s2 := seedPGUserRole(t, repos, "s2@example.com", domain.RoleSupplier)
	a1 := seedPGUserRole(t, repos, "a1@example.com", domain.RolePlatformAdmin)
	u1 := seedPGUserRole(t, repos, "u1@example.com", domain.RoleUser)
	u2 := seedPGUserRole(t, repos, "u2@example.com", domain.RoleUser)
	disabled := domain.UserStatusDisabled
	_, err := repos.UpdateUser(ctx, &repository.UserPatch{ID: s2.ID, Status: &disabled})
	require.NoError(t, err)

	// repo 默认排序 id desc；据此给出各筛选集的期望 ID（用于 rows 身份与分页断言）。
	allIDs := []int64{u2.ID, u1.ID, a1.ID, s2.ID, s1.ID}
	surfaceIDs := []int64{a1.ID, s2.ID, s1.ID} // {supplier, platform_admin}
	supplierIDs := []int64{s2.ID, s1.ID}
	userIDs := []int64{u2.ID, u1.ID}

	rolesOf := func(rows []*domain.User) []domain.Role {
		out := make([]domain.Role, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.Role)
		}
		return out
	}
	idsOf := func(rows []*domain.User) []int64 {
		out := make([]int64, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return out
	}
	// 每行同时断言 total、rows 的 Len、精确 ID 集合与角色多重集。
	check := func(name string, q repository.ListQuery, wantTotal int64, wantIDs []int64, wantRoles []domain.Role) {
		t.Helper()
		rows, total, err := repos.ListUsers(ctx, q)
		require.NoError(t, err, name)
		require.Equal(t, wantTotal, total, "%s: total", name)
		require.Len(t, rows, int(wantTotal), "%s: rows len", name)
		require.ElementsMatch(t, wantIDs, idsOf(rows), "%s: rows ids", name)
		require.ElementsMatch(t, wantRoles, rolesOf(rows), "%s: rows roles", name)
	}

	// #1 各有效 role 精确过滤（含禁用状态的供应商亦计入——role 过滤不看 active）
	check("role=supplier", repository.ListQuery{Role: "supplier", Limit: 20}, 2, supplierIDs,
		[]domain.Role{domain.RoleSupplier, domain.RoleSupplier})
	check("role=platform_admin", repository.ListQuery{Role: "platform_admin", Limit: 20}, 1, []int64{a1.ID},
		[]domain.Role{domain.RolePlatformAdmin})
	check("role=user", repository.ListQuery{Role: "user", Limit: 20}, 2, userIDs,
		[]domain.Role{domain.RoleUser, domain.RoleUser})

	// #2 两参数缺省、role="" → 无角色过滤（5 名全量）
	check("缺省不过滤", repository.ListQuery{Limit: 20}, 5, allIDs,
		[]domain.Role{domain.RoleSupplier, domain.RoleSupplier, domain.RolePlatformAdmin, domain.RoleUser, domain.RoleUser})
	check("role= 空", repository.ListQuery{Role: "", Limit: 20}, 5, allIDs,
		[]domain.Role{domain.RoleSupplier, domain.RoleSupplier, domain.RolePlatformAdmin, domain.RoleUser, domain.RoleUser})

	// #3 role=bogus（未知非空）+ surface 缺省/false → 空、total=0、rows 空
	check("role=bogus", repository.ListQuery{Role: "bogus", Limit: 20}, 0, []int64{}, []domain.Role{})
	check("role=bogus&surface=false", repository.ListQuery{Role: "bogus", SupplierSurface: false, Limit: 20}, 0,
		[]int64{}, []domain.Role{})

	// #4 supplier_surface=true → {supplier, platform_admin}，排除 user
	check("surface=true", repository.ListQuery{SupplierSurface: true, Limit: 20}, 3, surfaceIDs,
		[]domain.Role{domain.RoleSupplier, domain.RoleSupplier, domain.RolePlatformAdmin})

	// #5 role=user & surface=true → 供应商面集（证明 surface 覆盖 role）
	check("role=user&surface=true", repository.ListQuery{Role: "user", SupplierSurface: true, Limit: 20}, 3, surfaceIDs,
		[]domain.Role{domain.RoleSupplier, domain.RoleSupplier, domain.RolePlatformAdmin})

	// #6 role=bogus & surface=true → 供应商面集（同上，未知值不生效）
	check("role=bogus&surface=true", repository.ListQuery{Role: "bogus", SupplierSurface: true, Limit: 20}, 3, surfaceIDs,
		[]domain.Role{domain.RoleSupplier, domain.RoleSupplier, domain.RolePlatformAdmin})

	// #7 role=supplier & surface=false → 仅 supplier
	check("role=supplier&surface=false", repository.ListQuery{Role: "supplier", SupplierSurface: false, Limit: 20}, 2, supplierIDs,
		[]domain.Role{domain.RoleSupplier, domain.RoleSupplier})

	// #8 大小写不同邮箱子串组合、匹配/无匹配（等价 repo EmailContainsFold）
	check("email=S1@", repository.ListQuery{Email: "S1@", Limit: 20}, 1, []int64{s1.ID},
		[]domain.Role{domain.RoleSupplier})
	check("email=S&role=supplier", repository.ListQuery{Email: "S", Role: "supplier", Limit: 20}, 2, supplierIDs,
		[]domain.Role{domain.RoleSupplier, domain.RoleSupplier})
	check("email=U&role=user", repository.ListQuery{Email: "U", Role: "user", Limit: 20}, 2, userIDs,
		[]domain.Role{domain.RoleUser, domain.RoleUser})
	check("email=U&role=supplier 无匹配", repository.ListQuery{Email: "U", Role: "supplier", Limit: 20}, 0,
		[]int64{}, []domain.Role{})
	check("email=S&surface=true", repository.ListQuery{Email: "S", SupplierSurface: true, Limit: 20}, 2, supplierIDs,
		[]domain.Role{domain.RoleSupplier, domain.RoleSupplier})
	check("email=A1&surface=true", repository.ListQuery{Email: "A1", SupplierSurface: true, Limit: 20}, 1, []int64{a1.ID},
		[]domain.Role{domain.RolePlatformAdmin})
	check("email=U&surface=true 无匹配（user 被排除）", repository.ListQuery{Email: "U", SupplierSurface: true, Limit: 20}, 0,
		[]int64{}, []domain.Role{})
	check("email=no-such@ 无匹配", repository.ListQuery{Email: "no-such@", Limit: 20}, 0, []int64{}, []domain.Role{})

	// #9 筛选后 total 为全集，limit/offset 每页具体 ID（id desc）且页间不重叠。
	p0, tot0, err := repos.ListUsers(ctx, repository.ListQuery{SupplierSurface: true, Limit: 1, Offset: 0})
	require.NoError(t, err)
	require.Equal(t, int64(3), tot0, "surface total = 筛选全集")
	require.Equal(t, []int64{a1.ID}, idsOf(p0), "page0 = id desc 首行")
	p1, tot1, err := repos.ListUsers(ctx, repository.ListQuery{SupplierSurface: true, Limit: 1, Offset: 1})
	require.NoError(t, err)
	require.Equal(t, int64(3), tot1)
	require.Equal(t, []int64{s2.ID}, idsOf(p1), "page1")
	p2, tot2, err := repos.ListUsers(ctx, repository.ListQuery{SupplierSurface: true, Limit: 1, Offset: 2})
	require.NoError(t, err)
	require.Equal(t, int64(3), tot2)
	require.Equal(t, []int64{s1.ID}, idsOf(p2), "page2")
	p3, tot3, err := repos.ListUsers(ctx, repository.ListQuery{SupplierSurface: true, Limit: 1, Offset: 3})
	require.NoError(t, err)
	require.Equal(t, int64(3), tot3, "越界 total 仍为全集")
	require.Empty(t, p3, "越界空")
	// 页间不重叠 + 并集 = 筛选全集。
	seen := map[int64]int{}
	for _, id := range append(append(append([]int64{}, idsOf(p0)...), idsOf(p1)...), idsOf(p2)...) {
		seen[id]++
	}
	require.Len(t, seen, 3, "各页具体 ID 互不重复")
	for _, id := range surfaceIDs {
		require.Equal(t, 1, seen[id], "id %d 恰出现一次", id)
	}

	// 全量分页（limit=2）同样按 id desc 切页，total 恒为 5。
	a0, totA0, err := repos.ListUsers(ctx, repository.ListQuery{Limit: 2, Offset: 0})
	require.NoError(t, err)
	require.Equal(t, int64(5), totA0)
	require.Equal(t, []int64{u2.ID, u1.ID}, idsOf(a0), "all page0")
	a2, totA2, err := repos.ListUsers(ctx, repository.ListQuery{Limit: 2, Offset: 2})
	require.NoError(t, err)
	require.Equal(t, int64(5), totA2)
	require.Equal(t, []int64{a1.ID, s2.ID}, idsOf(a2), "all page1")
}

func TestPGKeyLifecycle(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	u := seedPGUser(t, repos, "keys@example.com")
	g, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "kg", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)

	k, err := repos.CreateKey(ctx, &domain.Key{
		UserID: u.ID, GroupID: g.ID, Name: "k1",
		KeyRaw: "ck-k1-plain",
		Status: domain.KeyStatusActive, MaxConcurrency: 0,
		Quota: 100, QuotaUsed: 10,
	})
	require.NoError(t, err)
	require.True(t, k.ID > 0)

	// GetKeyByRaw（明文等值回显；未找到 → nil,nil）
	got, err := repos.GetKeyByRaw(ctx, "ck-k1-plain")
	require.NoError(t, err)
	require.Equal(t, k.ID, got.ID)
	require.Equal(t, "ck-k1-plain", got.KeyRaw, "明文等值回显")
	require.Equal(t, int64(10), got.QuotaUsed)
	missing, err := repos.GetKeyByRaw(ctx, "ck-nope")
	require.NoError(t, err)
	require.Nil(t, missing)

	// ListKeysByUser
	rows, total, err := repos.ListKeysByUser(ctx, u.ID, repository.ListQuery{Limit: 10})
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Equal(t, "k1", rows[0].Name)

	// UpdateKey（patch：status/并发/额度；nil = 不改）
	name := "k1-renamed"
	st := domain.KeyStatusDisabled
	mc := 2
	q := int64(200)
	updated, err := repos.UpdateKey(ctx, &repository.KeyPatch{
		ID: k.ID, Name: &name, Status: &st, MaxConcurrency: &mc, Quota: &q,
	})
	require.NoError(t, err)
	require.Equal(t, domain.KeyStatusDisabled, updated.Status)
	require.Equal(t, 2, updated.MaxConcurrency)
	require.Equal(t, int64(200), updated.Quota)
	require.Equal(t, int64(10), updated.QuotaUsed, "返回行 QuotaUsed = DB 新鲜值（快照 15 不落库）")

	// RotateKey（明文换新单参）
	rotated, err := repos.RotateKey(ctx, k.ID, "ck-k1-new")
	require.NoError(t, err)
	require.Equal(t, "ck-k1-new", rotated.KeyRaw)

	// AddQuotaUsed 增量回写（Recorder 节奏）：基数 10（UpdateKey 未覆盖）→ 15
	require.NoError(t, repos.Keys.AddQuotaUsed(ctx, map[int64]int64{k.ID: 5, 99999: 3}))
	got2, err := repos.GetKey(ctx, k.ID)
	require.NoError(t, err)
	require.Equal(t, int64(15), got2.QuotaUsed, "5 增量生效；缺失 key 静默跳过（UpdateKey 不再覆盖基数）")

	// DeleteKey（软删）：GET 单个仍可查（含 deleted_at）；鉴权路径不可见
	require.NoError(t, repos.DeleteKey(ctx, k.ID))
	got3, err := repos.GetKey(ctx, k.ID)
	require.NoError(t, err, "软删后 GET 单个仍可查（审计可见）")
	require.NotNil(t, got3.DeletedAt, "软删行带 deleted_at")
	gone, err := repos.GetKeyByRaw(ctx, k.KeyRaw)
	require.NoError(t, err)
	require.Nil(t, gone, "已软删 key 按未找到处理（鉴权拒绝路径）；旧明文轮换后亦查不到")

	// DeleteKeysByGroup（组删除前置清理；返回本次被软删明文——已软删的 k1 过滤）
	_, err = repos.CreateKey(ctx, &domain.Key{
		UserID: u.ID, GroupID: g.ID, Name: "k2",
		KeyRaw: "ck-k2", Status: domain.KeyStatusActive,
	})
	require.NoError(t, err)
	raws, err := repos.DeleteKeysByGroup(ctx, g.ID)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"ck-k2"}, raws)
	// 级联软删后：组内全部 key 已删（GET 单个可查已删项）
	got4, err := repos.GetKey(ctx, k.ID)
	require.NoError(t, err)
	require.NotNil(t, got4.DeletedAt)
}

func TestPGLoadKeysSnapshot(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	u := seedPGUser(t, repos, "snap@example.com")
	g, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "sg", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	// 用户禁用 + key 禁用各一个，验证快照携带状态
	_, err = repos.CreateKey(ctx, &domain.Key{
		UserID: u.ID, GroupID: g.ID, Name: "active", KeyRaw: "ck-a",
		Status: domain.KeyStatusActive, MaxConcurrency: 4, Quota: 1000, QuotaUsed: 77,
	})
	require.NoError(t, err)
	_, err = repos.CreateKey(ctx, &domain.Key{
		UserID: u.ID, GroupID: g.ID, Name: "disabled", KeyRaw: "ck-d",
		Status: domain.KeyStatusDisabled,
	})
	require.NoError(t, err)

	m, err := repos.Keys.LoadKeys(ctx)
	require.NoError(t, err)
	require.Len(t, m, 2)
	a, ok := m["ck-a"]
	require.True(t, ok)
	require.Equal(t, u.ID, a.UserID, "KeyMeta 携带 userID")
	require.Equal(t, g.ID, a.GroupID)
	require.Equal(t, domain.KeyStatusActive, a.KeyStatus)
	require.Equal(t, 4, a.KeyMaxConc)
	require.True(t, a.HasQuota, "quota>0 → HasQuota")
	require.Equal(t, int64(1000), a.Quota)
	require.Equal(t, int64(77), a.QuotaUsed)
	require.Equal(t, domain.UserStatusActive, a.UserStatus)
	require.Equal(t, 0, a.UserMaxConc, "用户 max_concurrency 快照")
	d, ok := m["ck-d"]
	require.True(t, ok)
	require.Equal(t, domain.KeyStatusDisabled, d.KeyStatus)
	require.False(t, d.HasQuota, "quota=0 → HasQuota false")

	// 用户禁用后快照同步（invalidate → Reload 的数据源；patch 只改 status）
	st := domain.UserStatusDisabled
	_, err = repos.UpdateUser(ctx, &repository.UserPatch{ID: u.ID, Status: &st})
	require.NoError(t, err)
	m2, err := repos.Keys.LoadKeys(ctx)
	require.NoError(t, err)
	require.Equal(t, domain.UserStatusDisabled, m2["ck-a"].UserStatus, "用户禁用随快照下发")

	// 组 protocol_convert 变更随快照下发（热路径分支数据源；多值方向集合）
	_, err = repos.Groups.UpdateGroup(ctx, &domain.Group{
		ID: g.ID, Name: g.Name, Visibility: g.Visibility,
		PriceMultiplier: 10000,
		ProtocolConverts: []domain.ProtocolConvert{
			domain.ProtocolConvertChatToResp, domain.ProtocolConvertMessToResp,
		},
	})
	require.NoError(t, err)
	m3, err := repos.Keys.LoadKeys(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp, domain.ProtocolConvertMessToResp},
		m3["ck-a"].ProtocolConverts, "组 protocol_convert 随快照下发")
}

func TestPGGroupAssignments(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	u1 := seedPGUser(t, repos, "u1@example.com")
	u2 := seedPGUser(t, repos, "u2@example.com")
	pub, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "pub", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	priv, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "priv", Visibility: domain.GroupVisibilityPrivate})
	require.NoError(t, err)

	// Grant/Revoke（重复授予幂等——联合唯一兜底）
	require.NoError(t, repos.GrantGroup(ctx, priv.ID, u1.ID))
	require.NoError(t, repos.GrantGroup(ctx, priv.ID, u1.ID))
	list, err := repos.Assignments.ListByGroup(ctx, priv.ID)
	require.NoError(t, err)
	require.Len(t, list, 1, "重复授予幂等")
	require.Equal(t, u1.ID, list[0].UserID)

	// ListByUser
	assigns, err := repos.ListAssignmentsByUser(ctx, u1.ID)
	require.NoError(t, err)
	require.Len(t, assigns, 1)

	// ListGroupsForUser：public 全部 + 已授予 private
	groupsFor, err := repos.ListGroupsForUser(ctx, u1.ID)
	require.NoError(t, err)
	ids := make([]int64, 0, len(groupsFor))
	for _, g := range groupsFor {
		ids = append(ids, g.ID)
	}
	require.Contains(t, ids, pub.ID, "public 全部可见")
	require.Contains(t, ids, priv.ID, "已授予 private 可见")
	groupsFor2, err := repos.ListGroupsForUser(ctx, u2.ID)
	require.NoError(t, err)
	require.Len(t, groupsFor2, 1, "未授予用户只见 public")
	require.Equal(t, pub.ID, groupsFor2[0].ID)

	// Revoke
	require.NoError(t, repos.RevokeGroup(ctx, priv.ID, u1.ID))
	groupsFor3, err := repos.ListGroupsForUser(ctx, u1.ID)
	require.NoError(t, err)
	require.Len(t, groupsFor3, 1, "撤销后 private 不可见")
}

func TestPGGroupVisibilityRoundTrip(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	g, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "vis", Visibility: domain.GroupVisibilityPrivate})
	require.NoError(t, err)
	require.Equal(t, domain.GroupVisibilityPrivate, g.Visibility)
	got, err := repos.Groups.GetGroup(ctx, g.ID)
	require.NoError(t, err)
	require.Equal(t, domain.GroupVisibilityPrivate, got.Visibility)
	g.Visibility = domain.GroupVisibilityPublic
	updated, err := repos.Groups.UpdateGroup(ctx, g)
	require.NoError(t, err)
	require.Equal(t, domain.GroupVisibilityPublic, updated.Visibility)
}

func TestPGSettingDefaultsAndSet(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	// DB 无行 → 默认（signup_enabled=true）
	s, err := repos.GetSetting(ctx, "signup_enabled")
	require.NoError(t, err)
	require.Equal(t, domain.SettingTypeSwitch, s.Type)
	require.Equal(t, "true", s.Value)

	// Set 建行（upsert）
	set, err := repos.SetSetting(ctx, "signup_enabled", domain.SettingTypeSwitch, "false")
	require.NoError(t, err)
	require.Equal(t, "false", set.Value)
	s2, err := repos.GetSetting(ctx, "signup_enabled")
	require.NoError(t, err)
	require.Equal(t, "false", s2.Value)

	// GetAll：默认 + DB 覆盖（注册表逐项返回；signup_enabled 为 DB 覆盖值）
	all, err := repos.GetAllSettings(ctx)
	require.NoError(t, err)
	require.Len(t, all, len(domain.DefaultSettings))
	require.Equal(t, "false", all[0].Value)

	// 新用户初始资源 4 key 默认值（DB 无行即默认）
	for _, key := range []string{"default_user_max_concurrency", "default_user_balance", "default_user_temp_balance", "default_user_temp_balance_ttl_days"} {
		d := domain.DefaultSetting(key)
		require.NotNil(t, d, "内置注册表必须含 %s", key)
		require.Equal(t, domain.SettingTypeNumber, d.Type)
		got, err := repos.GetSetting(ctx, key)
		require.NoError(t, err)
		require.Equal(t, d.Value, got.Value, "DB 无行 → 默认 %s=%s", key, d.Value)
	}
	// 新 key 经 Set 落库覆盖默认
	set, err = repos.SetSetting(ctx, "default_user_balance", domain.SettingTypeNumber, "500")
	require.NoError(t, err)
	require.Equal(t, "500", set.Value)
	got, err := repos.GetSetting(ctx, "default_user_balance")
	require.NoError(t, err)
	require.Equal(t, "500", got.Value)

	// 重复 Set 覆盖（upsert 更新）
	_, err = repos.SetSetting(ctx, "signup_enabled", domain.SettingTypeSwitch, "true")
	require.NoError(t, err)
	s3, _ := repos.GetSetting(ctx, "signup_enabled")
	require.Equal(t, "true", s3.Value)
}

func TestPGUsageLogUserKeyRoundTrip(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	u := seedPGUser(t, repos, "log@example.com")
	err := repos.Usages.InsertBatch(ctx, []*domain.UsageLog{
		{RequestID: "r-user", GroupID: 1, AccountID: 2, UserID: u.ID, KeyID: 7,
			Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone,
			LatencyMS: 1, TotalTokens: 100, CreatedAt: time.Now()},
		{RequestID: "r-nouser", GroupID: 1, AccountID: 2,
			Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone,
			LatencyMS: 1, TotalTokens: 50, CreatedAt: time.Now()},
	})
	require.NoError(t, err)

	// user_id 过滤（/api/user/logs 语义：只看到自己的）
	rows, err := repos.Usages.QueryUsages(ctx, repository.UsageQuery{UserID: u.ID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, int64(7), rows[0].KeyID, "key_id 回读")

	// key_id 过滤
	rows2, err := repos.Usages.QueryUsages(ctx, repository.UsageQuery{KeyID: 7, Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows2, 1)
	require.Equal(t, u.ID, rows2[0].UserID)

	// 无 user 的日志 user_id 为 NULL → 0
	rows3, err := repos.Usages.QueryUsages(ctx, repository.UsageQuery{Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows3, 2)
	for _, l := range rows3 {
		if l.RequestID == "r-nouser" {
			require.Zero(t, l.UserID)
			require.Zero(t, l.KeyID)
		}
	}
}

// 编译期钉：ent 生成的枚举类型名（字段）。
var _ = group.VisibilityPublic
var _ = user.RoleUser
var _ = key.StatusActive
