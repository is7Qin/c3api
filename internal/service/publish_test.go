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
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/repository"
)

// pubRecorder 记录 Publish 收到的 Change 的测试假件（#14 T2 发布点断言：
// 各变更路径发布对应 Change，一次操作一条 NOTIFY）。
type pubRecorder struct {
	mu        sync.Mutex
	calls     []notify.Change
	cancelled bool // 最近一次 Publish 收到的 ctx 已取消（评审 I-2 断言）
}

func (r *pubRecorder) Publish(ctx context.Context, ch notify.Change) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancelled = ctx.Err() != nil
	r.calls = append(r.calls, ch)
	return nil
}

// total 总发布条数。
func (r *pubRecorder) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// last 最近一次发布的 Change（nil = 无）。
func (r *pubRecorder) last() *notify.Change {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return nil
	}
	c := r.calls[len(r.calls)-1]
	return &c
}

// countKeys 发布过 Keys:true 的条数。
func (r *pubRecorder) countKeys() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if c.Keys {
			n++
		}
	}
	return n
}

// recLocalDispatcher 本地分发器假件（#36 本地即时重算断言）：记录 Apply 收到
// 的 Change（实现 notify.Dispatcher——与生产 dispatcher 同接口）。
type recLocalDispatcher struct {
	mu      sync.Mutex
	applied []notify.Change
}

func (r *recLocalDispatcher) Apply(ctx context.Context, ch notify.Change) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applied = append(r.applied, ch)
}
func (r *recLocalDispatcher) FullRefresh(ctx context.Context) error { return nil }
func (r *recLocalDispatcher) changes() []notify.Change {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]notify.Change(nil), r.applied...)
}

// newPubSvc 构造带 pubRecorder 的 Service（settings 快照加载默认值——
// RegisterUser 读 signup_enabled）。
func newPubSvc() (*Service, *fakeStore, *pubRecorder) {
	fs := newFakeStore()
	pr := &pubRecorder{}
	svc := &Service{store: fs, inv: &invRecorder{}, pub: pr, log: nil}
	svc.reloadSettings(context.Background())
	return svc, fs, pr
}

// TestPublishMatrix #14 T2 发布点矩阵：inv.* 调用点并排发布 + 三现状缺口。
func TestPublishMatrix(t *testing.T) {
	ctx := context.Background()

	t.Run("key CRUD 缺口 → Keys:true，一次操作一条", func(t *testing.T) {
		svc, fs, pr := newPubSvc()
		u := seedUser(t, fs, "k@example.com", 0, 0)
		g, err := svc.CreateGroup(ctx, "g", domain.GroupVisibilityPublic, nil, nil)
		require.NoError(t, err)

		// 创建（quota 门禁字段落库 + Auth 增量注册）→ Keys
		before := pr.total()
		created, raw, err := svc.CreateKey(ctx, u.ID, "k1", g.ID, 8, 1000)
		require.NoError(t, err)
		require.NotEmpty(t, raw)
		got := pr.last()
		require.NotNil(t, got)
		require.True(t, got.Keys, "key 创建 → Keys:true")
		require.Equal(t, before+1, pr.total(), "一次操作一条 NOTIFY")

		// 改额度（quota）→ Keys
		q := int64(2000)
		_, err = svc.UpdateKey(ctx, u.ID, created.ID, nil, nil, nil, &q)
		require.NoError(t, err)
		require.True(t, pr.last().Keys, "key 改额度 → Keys:true")

		// 轮换（旧 hash 失效 + 新 hash 注册）→ Keys
		_, _, err = svc.RotateKey(ctx, u.ID, created.ID)
		require.NoError(t, err)
		require.True(t, pr.last().Keys, "key 轮换 → Keys:true")

		// 删除 → Keys
		before = pr.total()
		require.NoError(t, svc.DeleteKey(ctx, u.ID, created.ID))
		require.True(t, pr.last().Keys, "key 删除 → Keys:true")
		require.Equal(t, before+1, pr.total(), "一次操作一条 NOTIFY")
		require.Equal(t, 4, pr.countKeys())
	})

	t.Run("UpdateSetting 缺口 → Settings:true", func(t *testing.T) {
		svc, _, pr := newPubSvc()
		_, err := svc.UpdateSetting(ctx, "signup_enabled", "false")
		require.NoError(t, err)
		got := pr.last()
		require.True(t, got.Settings, "UpdateSetting → Settings:true")
		require.Equal(t, 1, pr.total(), "一次操作一条 NOTIFY")
	})

	t.Run("规则 CRUD 缺口 → Rules:true", func(t *testing.T) {
		svc, _, pr := newPubSvc()
		created, err := svc.CreateRule(ctx, RuleInput{Name: "r1", Enabled: true, Priority: 10, When: validWhen(), Then: validThen()})
		require.NoError(t, err)
		require.True(t, pr.last().Rules, "规则创建 → Rules:true")

		e := false
		_, err = svc.UpdateRule(ctx, created.ID, RulePatch{Enabled: &e})
		require.NoError(t, err)
		require.True(t, pr.last().Rules, "规则更新 → Rules:true")

		require.NoError(t, svc.DeleteRule(ctx, created.ID))
		require.True(t, pr.last().Rules, "规则删除 → Rules:true")

		r2, err := svc.CreateRule(ctx, RuleInput{Name: "r2", Enabled: true, Priority: 20, When: validWhen(), Then: validThen()})
		require.NoError(t, err)
		require.NoError(t, svc.DeleteRulesBatch(ctx, []int64{r2.ID}))
		require.True(t, pr.last().Rules, "规则批量删除 → Rules:true")
		require.Equal(t, 5, pr.total(), "4 次规则 CRUD + 1 次批量删除，一次操作一条 NOTIFY")
	})

	t.Run("用户路径 → Users:true", func(t *testing.T) {
		svc, _, pr := newPubSvc()
		// 公开注册（signup_enabled 默认 true）
		_, err := svc.RegisterUser(ctx, "reg@example.com", "pw12345678")
		require.NoError(t, err)
		require.True(t, pr.last().Users, "注册 → Users:true")

		u, err := svc.CreateUser(ctx, "adm@example.com", "pw12345678", domain.RoleUser, domain.UserStatusActive, 8, 1000)
		require.NoError(t, err)
		require.True(t, pr.last().Users, "管理面创建用户 → Users:true")

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
		require.True(t, pr.last().Users, "更新用户 → Users:true")
		require.Equal(t, 3, pr.total(), "一次操作一条 NOTIFY")
	})

	t.Run("模板路径 → Templates:true", func(t *testing.T) {
		svc, _, pr := newPubSvc()
		_, err := svc.CreateTemplate(ctx, &domain.Template{
			Name: "t", BaseURL: "https://t.example.com",
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		})
		require.NoError(t, err)
		require.True(t, pr.last().Templates, "创建模板 → Templates:true")
		require.Equal(t, 1, pr.total())
	})

	t.Run("账号路径 → Groups；upstream_key 变更 → Clients 同一条", func(t *testing.T) {
		svc, _, pr := newPubSvc()
		tpl, err := svc.CreateTemplate(ctx, &domain.Template{
			Name: "t", BaseURL: "https://t.example.com",
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		})
		require.NoError(t, err)
		g1, err := svc.CreateGroup(ctx, "g1", domain.GroupVisibilityPublic, nil, nil)
		require.NoError(t, err)
		g2, err := svc.CreateGroup(ctx, "g2", domain.GroupVisibilityPublic, nil, nil)
		require.NoError(t, err)

		acc, err := svc.CreateAccount(ctx, &domain.Account{
			Name: "a1", TemplateID: tpl.ID, UpstreamKey: "sk-1", GroupIDs: &[]int64{g1.ID, g2.ID},
		})
		require.NoError(t, err)
		got := pr.last()
		require.ElementsMatch(t, []int64{g1.ID, g2.ID}, got.Groups, "创建账号 → Groups 受影响组")
		require.False(t, got.Clients, "新建 key 无既有客户端 → 不置 Clients")

		// 移组 g1→g2 + upstream_key 变更：Groups（旧+新）+ Clients 同一条
		before := pr.total()
		_, err = svc.UpdateAccount(ctx, &domain.Account{
			ID: acc.ID, Name: "a1", TemplateID: tpl.ID, UpstreamKey: "sk-2", GroupIDs: &[]int64{g2.ID},
		})
		require.NoError(t, err)
		got = pr.last()
		require.ElementsMatch(t, []int64{g1.ID, g2.ID, g2.ID}, got.Groups, "移组 A→B：旧组+新组")
		require.True(t, got.Clients, "upstream_key 变更 → Clients:true")
		require.Equal(t, before+1, pr.total(), "一次操作一条 NOTIFY（Groups+Clients 合并）")
	})

	t.Run("Redeem → Users:true（余额/并发变更）", func(t *testing.T) {
		svc, fs, pr := newPubSvc()
		c := genOne(t, svc, GenerateRequest{Type: domain.RedemptionTypeBalance, Value: 100}, 0)
		u := seedUser(t, fs, "redeem@example.com", 0, 0)
		apply, err := svc.Redeem(ctx, c.Code, u.ID)
		require.NoError(t, err)
		require.NotNil(t, apply)
		require.True(t, pr.last().Users, "Redeem → Users:true（余额已变更）")
		require.Equal(t, 1, pr.total())
	})
}

// TestPublishNilPublisher pub 未装配（T2 过渡）→ no-op 不 panic。
func TestPublishNilPublisher(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	svc := &Service{store: fs, inv: &invRecorder{}, log: nil}
	u := seedUser(t, fs, "nilpub@example.com", 0, 0)
	svc.publish(ctx, notify.Change{Users: true}) // 直接调用也不 panic（nil 容忍）
	_, err := svc.CreateUser(ctx, u.Email+"2", "pw12345678", domain.RoleUser, domain.UserStatusActive, 8, 1000)
	require.NoError(t, err, "nil publisher 下业务路径正常")
}

// TestReloadSettings 公开方法（invalidate.SettingsReloader 接口）：外部落库后
// 调用 → 快照重载生效。
func TestReloadSettings(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	svc := &Service{store: fs, log: nil}
	svc.reloadSettings(ctx)
	require.Equal(t, "true", svc.settingValue("signup_enabled"), "默认值进快照")

	// 模拟其他实例 UpdateSetting 落库（本实例不经 UpdateSetting 路径）
	_, err := fs.SetSetting(ctx, "signup_enabled", domain.SettingTypeSwitch, "false")
	require.NoError(t, err)
	require.Equal(t, "true", svc.settingValue("signup_enabled"), "外部变更未重载前快照不变")
	require.NoError(t, svc.ReloadSettings(ctx))
	require.Equal(t, "false", svc.settingValue("signup_enabled"), "ReloadSettings 后生效")
}

// TestClusterInstances cluster.instances 读取：默认 1；UpdateSetting 后生效；
// 非法值回退 1（回退路径 = DB 直写绕过校验后快照重载，见末尾用例）。
// A-P2-11 强化：PUT 0（低于注册表 Min=1）→ 400 拒绝，不再"接受回退 1"
// （护栏前置；除零安全面本就由消费端双守卫覆盖，见 spec 评审）。
func TestClusterInstances(t *testing.T) {
	ctx := context.Background()
	svc, fs, pr := newPubSvc()
	require.Equal(t, 1, svc.ClusterInstances(), "无 DB 行 → 注册表默认 1")

	_, err := svc.UpdateSetting(ctx, "cluster.instances", "3")
	require.NoError(t, err)
	require.True(t, pr.last().Settings, "cluster.instances 更新 → Settings:true")
	require.Equal(t, 3, svc.ClusterInstances(), "UpdateSetting 后快照生效")

	_, err = svc.UpdateSetting(ctx, "cluster.instances", "0")
	require.ErrorIs(t, err, ErrInvalidInput, "PUT 0 → 400 拒绝（低于 Min=1）")
	require.Equal(t, 3, svc.ClusterInstances(), "400 拒绝未落库 → 快照保持上一次合法值")
	_, err = svc.UpdateSetting(ctx, "cluster.instances", "abc")
	require.Error(t, err, "非数字被 UpdateSetting 类型化校验拒绝")
	require.Equal(t, 3, svc.ClusterInstances(), "400 拒绝未落库 → 快照保持上一次合法值")

	// 回退 1 路径（护栏不覆盖 DB 直写）：绕过 UpdateSetting 落库非法值 → 重载回退 1
	_, err = fs.SetSetting(ctx, "cluster.instances", domain.SettingTypeNumber, "0")
	require.NoError(t, err)
	require.NoError(t, svc.ReloadSettings(ctx))
	require.Equal(t, 1, svc.ClusterInstances(), "非法值（0）回退 1")
}

// TestUpdateSettingLocalScopeReload #36 本地实例即时重算（R2 M-1）：UpdateSetting
// 除广播 NOTIFY（其余实例）外，还须直连本地分发器（自播 NOTIFY 被 Listener
// Src 跳过，本地实例预算重算不能依赖 NOTIFY 回环）。修复前本测试红——本地
// 无任何 auth.Reload 触发，N 变更后 gate 预算不重算。
func TestUpdateSettingLocalScopeReload(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	ld := &recLocalDispatcher{}
	svc := &Service{store: fs, local: ld, log: nil}
	svc.reloadSettings(ctx)
	require.Equal(t, 1, svc.ClusterInstances(), "无 DB 行 → 默认 1")

	_, err := svc.UpdateSetting(ctx, "cluster.instances", "3")
	require.NoError(t, err)
	require.Equal(t, 3, svc.ClusterInstances(), "本地快照即时生效（既有行为）")
	got := ld.changes()
	require.Len(t, got, 1, "本地分发收到一次 settings 变更")
	require.True(t, got[0].Settings, "本地分发载荷 Settings:true")

	// 非 settings 变更不触发本地分发（本地直连仅限 settings scope 分发）。
	svc2 := &Service{store: fs, local: ld, inv: NopInvalidator{}, log: nil}
	svc2.reloadSettings(ctx)
	u := seedUser(t, fs, "ld@example.com", 0, 0)
	_, err = svc2.CreateUser(ctx, u.Email+"2", "pw12345678", domain.RoleUser, domain.UserStatusActive, 8, 1000)
	require.NoError(t, err)
	require.Len(t, ld.changes(), 1, "用户创建不触发本地 settings 分发")
}

// 组/assignment 倍率断言补充（避免上文中途废弃的深拷贝占位）。
func TestPublishMultipliersAndGroupDelete(t *testing.T) {
	ctx := context.Background()
	svc, fs, pr := newPubSvc()
	g, err := svc.CreateGroup(ctx, "g", domain.GroupVisibilityPublic, nil, nil)
	require.NoError(t, err)
	require.True(t, pr.last().Multipliers, "创建组 → Multipliers:true")

	_, err = svc.UpdateGroup(ctx, &domain.Group{ID: g.ID, Name: "g", PriceMultiplier: 20000, ProtocolConverts: nil})
	require.NoError(t, err)
	require.True(t, pr.last().Multipliers, "更新组倍率 → Multipliers:true")

	u := seedUser(t, fs, "am@example.com", 0, 0)
	_, _, err = svc.SetGroupAssignments(ctx, g.ID, []int64{u.ID}, map[int64]*int{u.ID: intPtr(5000)})
	require.NoError(t, err)
	require.True(t, pr.last().Multipliers, "assignment 专属倍率 → Multipliers:true")

	// 用户维度写（评审 M-1 补齐）：SetUserGroups → Multipliers:true 同组维度
	_, _, err = svc.SetUserGroups(ctx, u.ID, []int64{g.ID}, nil)
	require.NoError(t, err)
	require.True(t, pr.last().Multipliers, "用户维度分组写 → Multipliers:true")

	// 组删除：倍率 + 组内 key 清理（Auth.Delete）合并同一条 NOTIFY
	before := pr.total()
	require.NoError(t, svc.DeleteGroup(ctx, g.ID))
	got := pr.last()
	require.True(t, got.Multipliers, "组删除 → Multipliers:true")
	require.True(t, got.Keys, "组删除经 Auth.Delete 移除组内 key → Keys:true 同一条")
	require.Equal(t, before+1, pr.total(), "一次操作一条 NOTIFY（合并单条）")
}

// TestPublishEmptyChangeSkipped 评审 I-1：空 Change（全字段 false + Groups 空）
// → publish 判空跳过，Publisher 收到 0 条。CreateAccount 无 GroupIDs 的空载荷
// 即被覆盖（与 O2 inv.Accounts 空集 no-op 同语义）。
func TestPublishEmptyChangeSkipped(t *testing.T) {
	ctx := context.Background()

	t.Run("空 Change 直接调用不发布", func(t *testing.T) {
		svc, _, pr := newPubSvc()
		svc.publish(ctx, notify.Change{})
		require.Zero(t, pr.total(), "空 Change → 0 条 NOTIFY")
	})

	t.Run("CreateAccount 无 GroupIDs → 空载荷不发布", func(t *testing.T) {
		svc, _, pr := newPubSvc()
		tpl, err := svc.CreateTemplate(ctx, &domain.Template{
			Name: "t", BaseURL: "https://t.example.com",
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		})
		require.NoError(t, err)
		before := pr.total() // 上一步创建模板已发布 1 条（Templates:true）
		_, err = svc.CreateAccount(ctx, &domain.Account{
			Name: "a1", TemplateID: tpl.ID, UpstreamKey: "sk-1", // 无 GroupIDs
		})
		require.NoError(t, err)
		require.Equal(t, before, pr.total(), "无分组账号 → 空 Change 跳过，不发布")
	})

	t.Run("UpdateAccount 无变更 → 空载荷不发布", func(t *testing.T) {
		svc, _, pr := newPubSvc()
		tpl, err := svc.CreateTemplate(ctx, &domain.Template{
			Name: "t", BaseURL: "https://t.example.com",
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		})
		require.NoError(t, err)
		// 无分组账号（创建时无 GroupIDs → 发布跳过，计数不变）
		before := pr.total() // 上一步模板创建已发布 1 条（Templates:true）
		acc, err := svc.CreateAccount(ctx, &domain.Account{
			Name: "a1", TemplateID: tpl.ID, UpstreamKey: "sk-1",
		})
		require.NoError(t, err)
		require.Equal(t, before, pr.total(), "无分组账号创建 → 空 Change 跳过")

		// GroupIDs nil = 不变（账号无组 → oldGroups 空），UpstreamKey 相同
		// → keyChanged false → gids 空 → 空 Change 跳过
		before = pr.total()
		_, err = svc.UpdateAccount(ctx, &domain.Account{
			ID: acc.ID, Name: "a1", TemplateID: tpl.ID, UpstreamKey: "sk-1",
		})
		require.NoError(t, err)
		require.Equal(t, before, pr.total(), "无变更更新 → 空 Change 跳过，不发布")
	})
}

// TestPublishDetachedFromRequestCtx 评审 I-2：请求 ctx 已取消时发布仍发出——
// publish 用 context.WithoutCancel 剥离取消信号（客户端断开不吞 NOTIFY），
// Publisher 收到的 ctx 未取消（Err()==nil）。
func TestPublishDetachedFromRequestCtx(t *testing.T) {
	svc, _, pr := newPubSvc()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 模拟请求 ctx 已取消

	svc.publish(ctx, notify.Change{Users: true})
	require.Equal(t, 1, pr.total(), "取消 ctx 下发布不丢弃")
	pr.mu.Lock()
	defer pr.mu.Unlock()
	require.False(t, pr.cancelled, "Publisher 收到脱离请求 ctx（WithoutCancel）：未取消")
}
