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

// testEmailCodes 无行为 EmailCodeStore（service.New 必选依赖的测试占位：
// PG 装配测试不触验证码面；验证码行为由 service/handler 的 fake 真实现覆盖）。
var testEmailCodes service.EmailCodeStore = testEmailCodeStore{}

type testEmailCodeStore struct{}

func (testEmailCodeStore) GetEmailCode(ctx context.Context, email, purpose string) (*domain.EmailCode, error) {
	return nil, service.ErrNotFound
}

func (testEmailCodeStore) UpsertEmailCode(ctx context.Context, email, purpose, sha256 string, expiresAt time.Time) (*domain.EmailCode, error) {
	return &domain.EmailCode{Email: email}, nil
}

func (testEmailCodeStore) IncrementEmailCodeAttempts(ctx context.Context, email, purpose string) (int, error) {
	return 0, nil
}

func (testEmailCodeStore) DeleteEmailCode(ctx context.Context, email, purpose string) error {
	return nil
}

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
// 首刷（并行）→ 五路快照全部可用——数据加载不依赖周期 ticker（SyncInterval
// 小时级兜底）；Select 依赖编译道产出计划（Start 后 reload 触发信号 →
// debounce 编译），有界等待收口。各快照错误独立（全部成功 → 空错误 map）；
// Status 记录 5 条加载状态。
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
		ModelMapping:     domain.ModelMapping{},
	})
	require.NoError(t, err)
	acc, err := repos.CreateAccount(ctx, &domain.Account{
		Name: "acc-1", TemplateID: tpl.ID, UpstreamKey: "sk-upstream",
		MaxConcurrency: 4,
	})
	require.NoError(t, err)
	require.NoError(t, repos.SetAccountGroups(ctx, acc.ID, []int64{g.ID})) // 成员关系独立写入（CreateAccount 不落 m2m）
	_, err = repos.CreateKey(ctx, &domain.Key{
		UserID: u.ID, GroupID: g.ID, Name: "k-startup",
		KeyRaw: "ck-startup-1",
		Status: domain.KeyStatusActive, MaxConcurrency: 8, Quota: 1_000_000,
	})
	require.NoError(t, err)
	_, err = repos.UpsertPriceEntryManual(ctx, &repository.PriceEntryManual{
		Model: "gpt-4o", Mode: domain.PriceModeToken, InputPerM: ptrI64Startup(250_000), OutputPerM: ptrI64Startup(1_000_000),
	})
	require.NoError(t, err)

	// --- 构造链（与 main 装配序一致：模块构造零 reload——单一入口） ---
	ruleEngine := rule.New(rule.Config{}, repos.Rules, nil)
	sched := scheduler.New(scheduler.Config{
		// sync ticker 不依赖（SyncInterval 小时级兜底）；编译道必须 Start——
		// Select 执行预编译计划，0 间隔误配防 ticker 空转 panic。
		DefaultMaxConcurrency: 4, SyncInterval: time.Hour,
	}, repos.Groups, ruleEngine, nil, nil)
	sched.SetWindowedQualitySource(nil)
	sched.SetPricesSource(nil)
	schedCtx, cancelSched := context.WithCancel(ctx)
	t.Cleanup(cancelSched)
	require.NoError(t, sched.Start(schedCtx))
	auth := proxy.NewAuth(repos.Keys, repos.Users, nil, true)
	balances := billing.NewBalances(repos, nil)
	svc := service.New(repos, sched, service.NopInvalidator{}, nil, ruleEngine, auth, nil, service.ServiceDeps{EmailCodeStore: testEmailCodes})

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

	// scheduler：首刷触发编译信号，编译道产出计划后 Select 命中种子账号
	//（debounce 200ms——有界等待，非 sleep）。
	var sel *scheduler.Selection
	require.Eventually(t, func() bool {
		var serr error
		sel, serr = sched.Select(g.ID, domain.FormatOpenAIChat, "gpt-4o")
		return serr == nil
	}, 5*time.Second, 50*time.Millisecond, "scheduler 首刷+编译道收口后 Select 可用")
	require.Equal(t, acc.ID, sel.AccountID)
	require.Equal(t, tpl.ID, sel.TemplateID)
	sched.Release(acc.ID) // 归还并发槽

	// rules：空表首刷已写入 typed 种子（状态管理唯一路径）。
	seeded, err := repos.Client.Rule.Query().Count(ctx)
	require.NoError(t, err)
	require.Positive(t, seeded, "规则表首刷含种子")

	// balances：余额快照命中。
	bal, ok := balances.BalanceOf(u.ID)
	require.True(t, ok)
	require.Equal(t, int64(123_456), bal)

	// pricing：价格快照命中（计费读零 DB）。
	pe, err := svc.GetPriceEntry(context.Background(), "gpt-4o")
	require.NoError(t, err)
	require.NotNil(t, pe.InputPerM)
	require.Equal(t, int64(250_000), *pe.InputPerM)

	// Status 可观测：5 条、全部已加载、无错误。
	st := reg.Status()
	require.Len(t, st, 5)
	for _, s := range st {
		require.False(t, s.LastReload.IsZero(), "%s 已首刷", s.Name)
		require.NoError(t, s.LastError, "%s 首刷无错误", s.Name)
	}
}

// observingKeyRepo 包装 key loader：在加载时刻记录 settings 快照值（观测
// gate.reload 时机——auth.Reload 内 LoadKeys/LoadUsers 之后即 gate.reload，期间
// settings 快照不被改动，观测等价）。观测键 = price_sync_cron（导出读面
// svc.PriceSyncCron 直读快照；cluster.instances 已随 Redis 实例发现移除，spec
// 2026-08-25-redis-instance-discovery-design §2.4——"快照先刷、reload 后行"的时序
// 契约与具体键无关，换键锚定）。svc 构造后回填（构造环：svc 需要 auth、auth 需要
// loader、loader 需要 svc 观测）。
type observingKeyRepo struct {
	*repository.KeyRepo
	svc  *service.Service
	seen *atomic.Pointer[string]
}

func (r *observingKeyRepo) LoadKeys(ctx context.Context) (map[string]domain.KeyMeta, error) {
	v := r.svc.PriceSyncCron()
	r.seen.Store(&v)
	return r.KeyRepo.LoadKeys(ctx)
}

// nopClients invalidate 装配用 aiclient 工厂桩（settings 时序测试不触发 clients
// 分支，仅满足 Config 必填）。
type nopClients struct{}

func (nopClients) InvalidateAll() {}

// TestSettingsTimingPG #36 即时重算时序（R2 M-1，真实 PG 全链路）：settings 旧值 →
// 变更 → auth.Reload（注册表 scope 分发）必须读到新快照——顺序保证 reload 消费新
// 值，而非"重载了个寂寞"。观测键 price_sync_cron（registry 默认 "0 3 * * *"）。
// 分两段：
//
//   - 远端路径：其他实例落库新 cron → 本实例 Apply(Change{Settings:true}) →
//     settings 快照先同步刷新、auth 后重载。修复前本段红：Apply 仅 Mark 去抖
//     （200ms 后 flush 才 ReloadSettings），reloadScopes 同步 auth.Reload 读到
//     旧快照，新值落地后再无 gate.reload 触发（observedCron 恒旧值）。
//   - 本地路径：UpdateSetting → settings 快照同步刷新 + inv.Settings()
//     去抖 mark → auth 快照全量 Reload 读到新快照（≤200ms 窗口内收敛，
//     与其余 Kind 同窗口语义）。
//
// inv 必须 Start（本地路径经去抖器 flush 生效；窗口 1ms，时序仍由 Eventually
// 锚定，无 sleep）。
func TestSettingsTimingPG(t *testing.T) {
	repos := newSnapshotPGRepos(t)
	ctx := context.Background()

	// --- 种子：用户 + 组 + key（gate 预算所需）+ settings 旧 cron ---
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
	_, err = repos.SetSetting(ctx, "price_sync_cron", domain.SettingTypeString, "0 3 * * *")
	require.NoError(t, err)

	// --- 构造链（与 main 装配序一致：模块构造零 reload——单一入口） ---
	ruleEngine := rule.New(rule.Config{}, repos.Rules, nil)
	sched := scheduler.New(scheduler.Config{
		DefaultMaxConcurrency: 4, SyncInterval: time.Hour,
	}, repos.Groups, ruleEngine, nil, nil)
	var seenCron atomic.Pointer[string]
	obs := &observingKeyRepo{KeyRepo: repos.Keys, seen: &seenCron}
	auth := proxy.NewAuth(obs, repos.Users, nil, true)
	inv := invalidate.New(invalidate.Config{
		Window: time.Millisecond, Sched: sched, Clients: nopClients{},
		Auth: auth, Rules: ruleEngine,
	})
	invCtx, stopInv := context.WithCancel(ctx)
	t.Cleanup(stopInv)
	require.NoError(t, inv.Start(invCtx))
	svc := service.New(repos, sched, inv, nil, ruleEngine, auth, nil, service.ServiceDeps{EmailCodeStore: testEmailCodes})
	obs.svc = svc // 回填（首次 LoadKeys 在注册表 ReloadAll 时）
	auth.SetInstancesProvider(discoStub{})

	reg := snapshot.New()
	require.NoError(t, reg.Register(authSnapshot{auth}))
	disp := &dispatcher{inv: inv, svc: svc, snapshots: reg, log: nil}

	cronSeen := func(want string) bool {
		p := seenCron.Load()
		return p != nil && *p == want
	}

	// 启动首刷：auth reload 一次，观测到旧值（基线）。
	require.Empty(t, reg.ReloadAll(ctx), "auth 快照首刷成功")
	require.Eventually(t, func() bool { return cronSeen("0 3 * * *") }, time.Second, 5*time.Millisecond)

	// --- 远端路径：其他实例落库新值（本实例不经 UpdateSetting）---
	_, err = repos.SetSetting(ctx, "price_sync_cron", domain.SettingTypeString, "*/5 * * * *")
	require.NoError(t, err)
	disp.Apply(ctx, notify.Change{Settings: true})
	require.Eventually(t, func() bool { return cronSeen("*/5 * * * *") }, 2*time.Second, 5*time.Millisecond,
		"远端 settings 变更：快照先同步刷新、auth.Reload 读到新快照")

	// --- 本地路径：UpdateSetting → 快照同步刷新 + 去抖器 KindSettings →
	// auth.Reload 读到新快照（自播 NOTIFY 被 Listener Src 跳过，本地收敛不
	// 依赖 NOTIFY 回环——走与远端同目标的统一去抖通道）。---
	_, err = svc.UpdateSetting(ctx, "price_sync_cron", "0 9 * * *")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return cronSeen("0 9 * * *") }, 2*time.Second, 5*time.Millisecond,
		"本地 UpdateSetting：统一去抖通道 → auth.Reload 读到新快照")
}

// discoStub 最小 InstancesProvider 桩（gate 预算注入面；本测试只关心 settings
// 时序，N 恒 1 单实例语义）。
type discoStub struct{}

func (discoStub) ClusterInstances() int { return 1 }

func ptrI64Startup(v int64) *int64 { return &v }
