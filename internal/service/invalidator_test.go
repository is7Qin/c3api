// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// invCall 一次 Invalidator 调用记录。
type invCall struct {
	kind string // users / templates / accounts / multipliers
	gids []int64
	key  bool
}

// invRecorder 记录 Mark 调用的测试假件（O2 接线矩阵断言：各实体走各自的
// 重载方式标记；兼作旧的 "invalidate: func() { invalidated++ }" 计数替代）。
type invRecorder struct {
	mu    sync.Mutex
	calls []invCall
}

func (r *invRecorder) Users()     { r.record("users", nil, false) }
func (r *invRecorder) Templates() { r.record("templates", nil, false) }
func (r *invRecorder) Multipliers() {
	r.record("multipliers", nil, false)
}
func (r *invRecorder) Accounts(gids []int64, keyChanged bool) {
	r.record("accounts", gids, keyChanged)
}
func (r *invRecorder) record(kind string, gids []int64, key bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, invCall{kind: kind, gids: append([]int64(nil), gids...), key: key})
}

// total 总调用次数（旧 invalidated 计数语义）。
func (r *invRecorder) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// last 最近一次调用（nil = 无）。
func (r *invRecorder) last() *invCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return nil
	}
	c := r.calls[len(r.calls)-1]
	return &c
}

// countKind 指定类型调用次数。
func (r *invRecorder) countKind(kind string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if c.kind == kind {
			n++
		}
	}
	return n
}

// --- O2 接线矩阵逐实体断言（评审 M-1） ---

func TestInvalidatorMatrix(t *testing.T) {
	ctx := context.Background()

	t.Run("用户创建/更新 → Users()", func(t *testing.T) {
		fs := newFakeStore()
		rec := &invRecorder{}
		svc := &Service{store: fs, inv: rec, log: nil}
		u, err := svc.CreateUser(ctx, "u1@example.com", "pw12345678", domain.RoleUser,
			domain.UserStatusActive, 8, 1000)
		require.NoError(t, err)
		require.Equal(t, 1, rec.countKind("users"), "创建用户 → Users()")
		// patch 形态（条件写）：旧值条件 = 创建快照（MaxConcurrency 8 / Balance 1000）
		role, st := domain.RoleUser, domain.UserStatusActive
		mc, oldMC := 4, 8
		bal, oldBal := int64(900), int64(1000)
		_, err = svc.UpdateUser(ctx, &repository.UserPatch{
			ID: u.ID, Role: &role, Status: &st,
			MaxConcurrency: &mc, OldMaxConcurrency: &oldMC,
			Balance: &bal, OldBalance: &oldBal,
		})
		require.NoError(t, err)
		require.Equal(t, 2, rec.countKind("users"), "更新用户 → Users()")
		require.Zero(t, rec.countKind("templates"))
		require.Zero(t, rec.countKind("accounts"))
		require.Zero(t, rec.countKind("multipliers"))
	})

	t.Run("模板 CRUD → Templates()", func(t *testing.T) {
		fs := newFakeStore()
		rec := &invRecorder{}
		svc := &Service{store: fs, inv: rec, log: nil}
		_, err := svc.CreateTemplate(ctx, &domain.Template{
			Name: "t", BaseURL: "https://t.example.com",
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		})
		require.NoError(t, err)
		require.Equal(t, 1, rec.countKind("templates"), "创建模板 → Templates()")
	})

	t.Run("账号创建/更新/删除 → Accounts(gids, keyChanged)", func(t *testing.T) {
		fs := newFakeStore()
		rec := &invRecorder{}
		svc := &Service{store: fs, inv: rec, log: nil}
		tpl, err := svc.CreateTemplate(ctx, &domain.Template{
			Name: "t", BaseURL: "https://t.example.com",
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		})
		require.NoError(t, err)
		g1, err := svc.CreateGroup(ctx, "g1", domain.GroupVisibilityPublic, nil, nil)
		require.NoError(t, err)
		g2, err := svc.CreateGroup(ctx, "g2", domain.GroupVisibilityPublic, nil, nil)
		require.NoError(t, err)

		// 创建带组：gids = 新建分组；keyChanged=false（新 key 无既有客户端）
		acc, err := svc.CreateAccount(ctx, &domain.Account{
			Name: "a1", TemplateID: tpl.ID, UpstreamKey: "sk-1", GroupIDs: &[]int64{g1.ID, g2.ID},
		})
		require.NoError(t, err)
		got := rec.last()
		require.Equal(t, "accounts", got.kind)
		require.ElementsMatch(t, []int64{g1.ID, g2.ID}, got.gids, "创建 → 全部分组组级定向")
		require.False(t, got.key)

		// 更新移组 g1→g2：旧组 ∪ 新组都重载；upstream_key 变更 → keyChanged
		_, err = svc.UpdateAccount(ctx, &domain.Account{
			ID: acc.ID, Name: "a1", TemplateID: tpl.ID, UpstreamKey: "sk-2", GroupIDs: &[]int64{g2.ID},
		})
		require.NoError(t, err)
		got = rec.last()
		require.Equal(t, "accounts", got.kind)
		// 旧组 [g1,g2] + 目标组 g2（重复由去抖器 map 去重）
		require.ElementsMatch(t, []int64{g1.ID, g2.ID, g2.ID}, got.gids, "移组 A→B：旧组+新组都重载")
		require.True(t, got.key, "upstream_key 变更 → keyChanged")

		// 删除：删除前查组 → 组级定向（账号当前在 g2）
		require.NoError(t, svc.DeleteAccount(ctx, acc.ID))
		got = rec.last()
		require.Equal(t, "accounts", got.kind)
		require.ElementsMatch(t, []int64{g2.ID}, got.gids, "删除 → 原分组定向")
		require.False(t, got.key)

		require.Zero(t, rec.countKind("users"), "账号变更不得触发用户全量")
		require.Equal(t, 1, rec.countKind("templates"), "模板仅 setup 创建触发一次，账号变更不得触发模板全量")
	})

	t.Run("批量账号 → 旧组并集 + 目标组；keyChanged 按 patch", func(t *testing.T) {
		fs := newFakeStore()
		rec := &invRecorder{}
		svc := &Service{store: fs, inv: rec, log: nil}
		tpl, err := svc.CreateTemplate(ctx, &domain.Template{
			Name: "t", BaseURL: "https://t.example.com",
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		})
		require.NoError(t, err)
		g1, err := svc.CreateGroup(ctx, "g1", domain.GroupVisibilityPublic, nil, nil)
		require.NoError(t, err)
		g2, err := svc.CreateGroup(ctx, "g2", domain.GroupVisibilityPublic, nil, nil)
		require.NoError(t, err)
		a1, err := svc.CreateAccount(ctx, &domain.Account{
			Name: "a1", TemplateID: tpl.ID, UpstreamKey: "sk-1", GroupIDs: &[]int64{g1.ID},
		})
		require.NoError(t, err)
		a2, err := svc.CreateAccount(ctx, &domain.Account{
			Name: "a2", TemplateID: tpl.ID, UpstreamKey: "sk-1", GroupIDs: &[]int64{g1.ID},
		})
		require.NoError(t, err)

		// 批量：两组旧组 g1 + 目标 g2 并集；upstream_key 提供 → keyChanged
		key := "sk-batch"
		require.NoError(t, svc.UpdateAccountsBatch(ctx, []int64{a1.ID, a2.ID},
			repository.AccountPatch{GroupIDs: &[]int64{g2.ID}, UpstreamKey: &key}))
		got := rec.last()
		require.Equal(t, "accounts", got.kind)
		// 旧组（两账号 × g1）+ 目标组 g2 并集（重复由去抖器 map 去重）
		require.ElementsMatch(t, []int64{g1.ID, g1.ID, g2.ID}, got.gids)
		require.True(t, got.key, "批量 upstream_key → keyChanged")

		// 批量删除：旧组并集（两账号同组 → gids 含重复，去抖器内按 map 去重）
		require.NoError(t, svc.DeleteAccountsBatch(ctx, []int64{a1.ID, a2.ID}))
		got = rec.last()
		require.ElementsMatch(t, []int64{g2.ID, g2.ID}, got.gids, "删除后账号已在 g2")
		require.False(t, got.key)
	})

	t.Run("组倍率 → Multipliers()", func(t *testing.T) {
		fs := newFakeStore()
		rec := &invRecorder{}
		svc := &Service{store: fs, inv: rec, log: nil}
		g, err := svc.CreateGroup(ctx, "g", domain.GroupVisibilityPublic, nil, nil)
		require.NoError(t, err)
		require.Equal(t, 1, rec.countKind("multipliers"), "创建组（倍率设定）→ Multipliers()")
		_, err = svc.UpdateGroup(ctx, &domain.Group{ID: g.ID, Name: "g", PriceMultiplier: 20000, ProtocolConverts: nil})
		require.NoError(t, err)
		require.Equal(t, 2, rec.countKind("multipliers"), "更新组倍率 → Multipliers()")
		require.NoError(t, svc.DeleteGroup(ctx, g.ID))
		require.Equal(t, 3, rec.countKind("multipliers"), "删除组 → Multipliers()")
		require.Zero(t, rec.countKind("users"))
		require.Zero(t, rec.countKind("accounts"))
	})

	t.Run("组授予 + 用户-组专属倍率 → Multipliers()（T3.5 按组）", func(t *testing.T) {
		fs := newFakeStore()
		rec := &invRecorder{}
		svc := &Service{store: fs, inv: rec, log: nil}
		u, err := svc.CreateUser(ctx, "am@example.com", "pw12345678", domain.RoleUser,
			domain.UserStatusActive, 8, 1000)
		require.NoError(t, err)
		g, err := svc.CreateGroup(ctx, "g", domain.GroupVisibilityPublic, nil, nil)
		require.NoError(t, err)
		before := rec.countKind("multipliers")
		// 授予 + 设专属倍率（万分数 5000 = ×0.5）→ Multipliers()
		applied, _, err := svc.SetGroupAssignments(ctx, g.ID, []int64{u.ID}, map[int64]*int{u.ID: intPtr(5000)})
		require.NoError(t, err)
		require.Equal(t, []int64{u.ID}, applied)
		require.Equal(t, before+1, rec.countKind("multipliers"), "group_assignment CRUD → Multipliers()")
		// 清除专属倍率（nil）→ Multipliers()
		_, _, err = svc.SetGroupAssignments(ctx, g.ID, []int64{u.ID}, map[int64]*int{u.ID: nil})
		require.NoError(t, err)
		require.Equal(t, before+2, rec.countKind("multipliers"), "清除专属倍率 → Multipliers()")
		require.Equal(t, 1, rec.countKind("users"), "assignment 倍率变更不得触发用户全量 Reload（仅创建用户一次）")
	})

	t.Run("批量组更新（仅 name/visibility）→ 不触发任何失效", func(t *testing.T) {
		fs := newFakeStore()
		rec := &invRecorder{}
		svc := &Service{store: fs, inv: rec, log: nil}
		g, err := svc.CreateGroup(ctx, "g", domain.GroupVisibilityPublic, nil, nil)
		require.NoError(t, err)
		before := rec.total()
		name := "renamed"
		require.NoError(t, svc.UpdateGroupsBatch(ctx, []int64{g.ID}, repository.GroupPatch{Name: &name}))
		require.Equal(t, before, rec.total(), "GroupPatch 无倍率字段 → 不触发失效（矩阵：仅倍率变更走 Multipliers）")
	})
}
