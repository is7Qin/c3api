// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/invalidate"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/proxy"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/service"
	"github.com/is7qin/c3api/internal/snapshot"
)

// 启动就绪时序（快照注册表）真实 PG 集成基座（与 repository/pricing 包同款
// 约定）：
//
//	TEST_DATABASE_URL=postgres://postgres:c3api@localhost:15432/c3api_test_snap \
//	  go test ./cmd/server/ -run TestStartupReloadAllPG -v
//
// 独立测试库 c3api_test_snap（避开与其它包测试的 DB 竞争）；本测试另用独立
// schema（snapshot_test）与同库其它 schema 隔离。未设置 TEST_DATABASE_URL →
// t.Skip。

// snapshotTestSchema 本测试专用 schema（同一数据库内隔离命名空间）。
const snapshotTestSchema = "snapshot_test"

func newSnapshotPGRepos(t *testing.T) *repository.Repository {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + snapshotTestSchema
	} else {
		dsn += "?search_path=" + snapshotTestSchema
	}
	ctx := context.Background()
	pool, err := repository.OpenPG(ctx, dsn, 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+snapshotTestSchema+` CASCADE; CREATE SCHEMA `+snapshotTestSchema+`;`)
	require.NoError(t, err)
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), true)
	require.NoError(t, err)
	return repos
}

// TestStartupReloadAllPG 启动就绪时序：构造链完成后 registry.ReloadAll 全量
// 首刷（并行）→ 五路快照全部可用——不依赖任何周期 ticker（scheduler 从未
// Start，SyncInterval 小时级；Select 立即可用 = 90s/首 tick 窗口消灭断言）；
// 各快照错误独立（全部成功 → 空错误 map）；Status 记录 5 条加载状态。
func TestStartupReloadAllPG(t *testing.T) {
	repos := newSnapshotPGRepos(t)
	ctx := context.Background()

	// --- 种子数据（构造链完成后注册表首刷应全部可见） ---
	u, err := repos.CreateUser(ctx, &domain.User{
		Email: "startup@example.com", PasswordHash: "bcrypt-hash",
		Role: domain.RoleUser, Status: domain.UserStatusActive, MaxConcurrency: 8,
		Balance: 123_456, // 毫分
	})
	require.NoError(t, err)
	g, err := repos.Groups.CreateGroup(ctx, &domain.Group{
		Name: "g1", Visibility: domain.GroupVisibilityPublic, PriceMultiplier: 10000,
	})
	require.NoError(t, err)
	tpl, err := repos.CreateTemplate(ctx, &domain.Template{
		Name: "t1", BaseURL: "http://upstream.example.com", CredentialType: credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		Models:           []string{"gpt-4o"},
	})
	require.NoError(t, err)
	acc, err := repos.CreateAccount(ctx, &domain.Account{
		Name: "acc-1", TemplateID: tpl.ID, UpstreamKey: "sk-upstream",
		Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4,
	})
	require.NoError(t, err)
	require.NoError(t, repos.SetAccountGroups(ctx, acc.ID, []int64{g.ID})) // 成员关系独立写入（CreateAccount 不落 m2m）
	_, err = repos.CreateKey(ctx, &domain.Key{
		UserID: u.ID, GroupID: g.ID, Name: "k-startup",
		KeyRaw: "ck-startup-1",
		Status: domain.KeyStatusActive, MaxConcurrency: 8, Quota: 1_000_000,
	})
	require.NoError(t, err)
	_, err = repos.UpsertManual(ctx, &repository.PricingManual{
		Model: "gpt-4o", PromptPricePerMillion: 250_000, CompletionPricePerMillion: 1_000_000,
	})
	require.NoError(t, err)

	// --- 构造链（与 main 装配序一致：模块构造零 reload——单一入口） ---
	ruleEngine := rule.New(rule.Config{}, repos.Rules, nil)
	sched := scheduler.New(scheduler.Config{
		// 测试不 Start（零 ticker）：全部依赖注册表首刷——SyncInterval 给小时级
		// 兜底值，防误 Start 时 0 间隔 ticker panic。
		DefaultMaxConcurrency: 4, SyncInterval: time.Hour,
	}, repos.Groups, ruleEngine, nil)
	auth := proxy.NewAuth(repos.Keys, repos.Users, nil)
	balances := billing.NewBalances(repos, nil)
	svc := service.New(repos, sched, service.NopInvalidator{}, nil, ruleEngine, auth, nil)

	reg := snapshot.New()
	for _, s := range []snapshot.Snapshot{
		authSnapshot{auth}, schedSnapshot{sched}, ruleSnapshot{ruleEngine},
		pricingSnapshot{svc}, balanceSnapshot{balances},
	} {
		require.NoError(t, reg.Register(s))
	}

	// --- 统一启动就绪：ReloadAll（并行全量首刷） ---
	require.Empty(t, reg.ReloadAll(ctx), "五路快照首刷全部成功")

	// --- 全部快照已加载（不依赖各自 ticker） ---
	// auth：key 鉴权命中。
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer ck-startup-1")
	meta, ok := auth.Authenticate(req)
	require.True(t, ok, "auth 首刷后 key 鉴权立即可用")
	require.Equal(t, u.ID, meta.UserID)

	// scheduler：启动后立即转换请求可用（Select 不 panic、命中种子账号）。
	sel, err := sched.Select(g.ID, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err, "scheduler 首刷后 Select 立即可用（无 ticker）")
	require.Equal(t, acc.ID, sel.AccountID)
	require.Equal(t, tpl.ID, sel.TemplateID)
	sched.Release(acc.ID) // 归还并发槽

	// rules：空表种子已写入（状态管理唯一路径）。
	require.True(t, ruleEngine.NeedsOKEvents(), "规则表首刷含种子（seed-ok）")

	// balances：余额快照命中。
	bal, ok := balances.BalanceOf(u.ID)
	require.True(t, ok)
	require.Equal(t, int64(123_456), bal)

	// pricing：价格快照命中（计费读零 DB）。
	p, err := svc.GetPrice("gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(250_000), p.PromptPricePerMillion)

	// Status 可观测：5 条、全部已加载、无错误。
	st := reg.Status()
	require.Len(t, st, 5)
	for _, s := range st {
		require.False(t, s.LastReload.IsZero(), "%s 已首刷", s.Name)
		require.NoError(t, s.LastError, "%s 首刷无错误", s.Name)
	}
}

// observingKeyRepo 包装 key loader：在加载时刻记录 settings 快照 N——观测
// gate.reload → allocBudget 现读 ClusterInstances() 读到的 N（auth.Reload 内
// LoadKeys/LoadUsers 之后即 gate.reload，期间 settings 快照不被改动，观测等价）。
// svc 构造后回填（构造环：svc 需要 auth、auth 需要 loader、loader 需要 svc 观测）。
type observingKeyRepo struct {
	*repository.KeyRepo
	svc  *service.Service
	seen *atomic.Int64
}

func (r *observingKeyRepo) LoadKeys(ctx context.Context) (map[string]domain.KeyMeta, error) {
	r.seen.Store(int64(r.svc.ClusterInstances()))
	return r.KeyRepo.LoadKeys(ctx)
}

// nopClients invalidate 装配用 aiclient 工厂桩（settings 时序测试不触发 clients
// 分支，仅满足 Config 必填）。
type nopClients struct{}

func (nopClients) InvalidateAll() {}

// TestSettingsTimingPG #36 即时重算时序（R2 M-1，真实 PG 全链路）：settings
// 旧 N → 变更新 N → auth.Reload（注册表 scope 分发）必须读到新 N——顺序保证
// budget 按新 N 重算，而非"重算了个寂寞"。分两段：
//
//   - 远端路径：其他实例落库 N=2 → 本实例 Apply(Change{Settings:true}) →
//     settings 快照先同步刷新、auth 后重载。修复前本段红：Apply 仅 Mark 去抖
//     （200ms 后 flush 才 ReloadSettings），reloadScopes 同步 auth.Reload 读到
//     旧 N=1，新 N 落地后再无 gate.reload 触发（observedN 恒 1）。
//   - 本地路径（#36 本地缺口）：UpdateSetting("cluster.instances","2") → 本地
//     直连分发器触发 auth.Reload（自播 NOTIFY 被 Listener Src 跳过，本地实例
//     不能依赖 NOTIFY 回环）。
//
// inv 不 Start（settings 分支 sync 后不依赖去抖器；不 Start 使修复前形态确定性
// 红——flush 永不执行，快照保持旧 N）。
func TestSettingsTimingPG(t *testing.T) {
	repos := newSnapshotPGRepos(t)
	ctx := context.Background()

	// --- 种子：用户 + 组 + key（gate 预算所需）+ settings 旧 N=1 ---
	u, err := repos.CreateUser(ctx, &domain.User{
		Email: "timing@example.com", PasswordHash: "bcrypt-hash",
		Role: domain.RoleUser, Status: domain.UserStatusActive, MaxConcurrency: 8,
	})
	require.NoError(t, err)
	g, err := repos.Groups.CreateGroup(ctx, &domain.Group{
		Name: "g-timing", Visibility: domain.GroupVisibilityPublic, PriceMultiplier: 10000,
	})
	require.NoError(t, err)
	_, err = repos.CreateKey(ctx, &domain.Key{
		UserID: u.ID, GroupID: g.ID, Name: "k-timing",
		KeyRaw: "ck-timing-1",
		Status: domain.KeyStatusActive, MaxConcurrency: 8, Quota: 1_000_000,
	})
	require.NoError(t, err)
	_, err = repos.SetSetting(ctx, "cluster.instances", domain.SettingTypeNumber, "1")
	require.NoError(t, err)

	// --- 构造链（与 main 装配序一致：模块构造零 reload——单一入口） ---
	ruleEngine := rule.New(rule.Config{}, repos.Rules, nil)
	sched := scheduler.New(scheduler.Config{
		DefaultMaxConcurrency: 4, SyncInterval: time.Hour,
	}, repos.Groups, ruleEngine, nil)
	var seenN atomic.Int64
	obs := &observingKeyRepo{KeyRepo: repos.Keys, seen: &seenN}
	auth := proxy.NewAuth(obs, repos.Users, nil)
	svc := service.New(repos, sched, service.NopInvalidator{}, nil, ruleEngine, auth, nil)
	obs.svc = svc // 回填（首次 LoadKeys 在注册表 ReloadAll 时）
	auth.SetInstancesProvider(svc)

	reg := snapshot.New()
	require.NoError(t, reg.Register(authSnapshot{auth}))
	inv := invalidate.New(invalidate.Config{
		Window: time.Millisecond, Sched: sched, Clients: nopClients{},
		Auth: auth, Rules: ruleEngine,
	})
	disp := &dispatcher{inv: inv, svc: svc, snapshots: reg, log: nil}

	// 启动首刷：auth reload 一次，观测到旧 N=1（基线）。
	require.Empty(t, reg.ReloadAll(ctx), "auth 快照首刷成功")
	require.Eventually(t, func() bool { return seenN.Load() == 1 }, time.Second, 5*time.Millisecond)

	// --- 远端路径：其他实例落库 N=2（本实例不经 UpdateSetting）---
	_, err = repos.SetSetting(ctx, "cluster.instances", domain.SettingTypeNumber, "2")
	require.NoError(t, err)
	disp.Apply(ctx, notify.Change{Settings: true})
	require.Eventually(t, func() bool { return seenN.Load() == 2 }, 2*time.Second, 5*time.Millisecond,
		"远端 settings 变更：快照先同步刷新、auth.Reload 读到新 N（预算按新 N 重算）")

	// --- 本地路径（#36 本地缺口）：UpdateSetting 直连本地分发器（自播 NOTIFY
	// 被 Listener Src 跳过，本地不能依赖回环）——本地实例亦即时重算预算。
	// 修复前本段红：UpdateSetting 后 auth.Reload 从不触发，seenN 恒 2（上次
	// 远端 reload 的观测值）。---
	svc.SetLocalDispatcher(disp)
	_, err = svc.UpdateSetting(ctx, "cluster.instances", "3")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return seenN.Load() == 3 }, 2*time.Second, 5*time.Millisecond,
		"本地 UpdateSetting：直连分发器 → auth.Reload 读到新 N=3（本地实例预算即时重算）")
}
