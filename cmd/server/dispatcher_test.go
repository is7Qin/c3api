// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/invalidate"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/snapshot"
)

// --- 测试重载目标（参照 internal/invalidate/invalidate_test.go 的 rec* 系列） ---

type recSched2 struct {
	mu     sync.Mutex
	full   int
	groups []int64
}

func (r *recSched2) InvalidateAll() { r.mu.Lock(); r.full++; r.mu.Unlock() }
func (r *recSched2) InvalidateGroup(g int64) {
	r.mu.Lock()
	r.groups = append(r.groups, g)
	r.mu.Unlock()
}
func (r *recSched2) InvalidateAllSyncCtx(ctx context.Context) error {
	r.mu.Lock()
	r.full++
	r.mu.Unlock()
	return nil
}
func (r *recSched2) counts() (int, []int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.full, append([]int64(nil), r.groups...)
}

type recClients2 struct {
	mu sync.Mutex
	n  int
}

func (r *recClients2) InvalidateAll() { r.mu.Lock(); r.n++; r.mu.Unlock() }
func (r *recClients2) calls() int     { r.mu.Lock(); defer r.mu.Unlock(); return r.n }

type recAuth2 struct {
	mu sync.Mutex
	n  int
}

func (r *recAuth2) Reload(ctx context.Context) error { r.mu.Lock(); r.n++; r.mu.Unlock(); return nil }
func (r *recAuth2) calls() int                       { r.mu.Lock(); defer r.mu.Unlock(); return r.n }

type recBal2 struct {
	mu   sync.Mutex
	rel  int
	mult int
}

func (r *recBal2) Reload(ctx context.Context) error { r.mu.Lock(); r.rel++; r.mu.Unlock(); return nil }
func (r *recBal2) ReloadMultipliers(ctx context.Context) error {
	r.mu.Lock()
	r.mult++
	r.mu.Unlock()
	return nil
}
func (r *recBal2) relCalls() int  { r.mu.Lock(); defer r.mu.Unlock(); return r.rel }
func (r *recBal2) multCalls() int { r.mu.Lock(); defer r.mu.Unlock(); return r.mult }

type recSettings2 struct {
	mu sync.Mutex
	n  int
}

func (r *recSettings2) ReloadSettings(ctx context.Context) error {
	r.mu.Lock()
	r.n++
	r.mu.Unlock()
	return nil
}
func (r *recSettings2) calls() int { r.mu.Lock(); defer r.mu.Unlock(); return r.n }

type recRules2 struct {
	mu sync.Mutex
	n  int
}

func (r *recRules2) ReloadRules(ctx context.Context) error {
	r.mu.Lock()
	r.n++
	r.mu.Unlock()
	return nil
}
func (r *recRules2) calls() int { r.mu.Lock(); defer r.mu.Unlock(); return r.n }

// recSnap 注册表 fake 快照（NOTIFY scope 分发与 FullRefresh 断言用）。
type recSnap struct {
	name   string
	scopes []string
	mu     sync.Mutex
	n      int
}

func (r *recSnap) Name() string     { return r.name }
func (r *recSnap) Scopes() []string { return r.scopes }
func (r *recSnap) Reload(ctx context.Context) error {
	r.mu.Lock()
	r.n++
	r.mu.Unlock()
	return nil
}
func (r *recSnap) calls() int { r.mu.Lock(); defer r.mu.Unlock(); return r.n }

// newTestDispatcher 构造真实 Debouncer（1ms 窗口，测试快刷）+ 记录 fake 的
// dispatcher 与各目标句柄；注册表含四路 fake 快照（auth 声明 ScopeSettings，
// 其余无 scope——与生产装配同形态）。
type testDispRig struct {
	d         *dispatcher
	inv       *invalidate.Debouncer
	sched     *recSched2
	clients   *recClients2
	auth      *recAuth2
	bal       *recBal2
	settings  *recSettings2
	rules     *recRules2
	snapAuth  *recSnap
	snapSched *recSnap
	snapRules *recSnap
	snapBal   *recSnap
	cancel    context.CancelFunc
}

func newTestDispatcher(t *testing.T) *testDispRig {
	t.Helper()
	sched := &recSched2{}
	clients := &recClients2{}
	auth := &recAuth2{}
	bal := &recBal2{}
	settings := &recSettings2{}
	rules := &recRules2{}
	inv := invalidate.New(invalidate.Config{
		Window:   time.Millisecond,
		Sched:    sched,
		Clients:  clients,
		Auth:     auth,
		Balances: bal,
		Rules:    rules,
	})
	inv.SetSettings(settings)

	snapAuth := &recSnap{name: "auth", scopes: []string{snapshot.ScopeSettings}}
	snapSched := &recSnap{name: "scheduler"}
	snapRules := &recSnap{name: "rules"}
	snapBal := &recSnap{name: "balances"}
	reg := snapshot.New()
	for _, s := range []snapshot.Snapshot{snapAuth, snapSched, snapRules, snapBal} {
		require.NoError(t, reg.Register(s))
	}
	d := &dispatcher{inv: inv, svc: settings, snapshots: reg}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, inv.Start(ctx))
	return &testDispRig{
		d: d, inv: inv, sched: sched, clients: clients, auth: auth, bal: bal,
		settings: settings, rules: rules,
		snapAuth: snapAuth, snapSched: snapSched, snapRules: snapRules, snapBal: snapBal,
		cancel: cancel,
	}
}

// waitFlush 等去抖窗口 flush 完成：轮询直到谓词满足或超时。
func waitFlush(t *testing.T, pred func() bool) {
	t.Helper()
	require.Eventually(t, pred, 2*time.Second, 5*time.Millisecond)
}

// TestDispatcherApplyMapping Dispatcher.Apply 映射表（设计文档 §2.2）：
// 每个 Change 位 → 对应去抖器 Mark → 一次 flush 内命中对应重载目标。
func TestDispatcherApplyMapping(t *testing.T) {
	t.Run("Users", func(t *testing.T) {
		rg := newTestDispatcher(t)
		rg.d.Apply(context.Background(), notify.Change{Users: true})
		waitFlush(t, func() bool { return rg.auth.calls() > 0 })
		require.Equal(t, 1, rg.auth.calls(), "users → auth Reload 一次")
	})

	t.Run("Templates", func(t *testing.T) {
		rg := newTestDispatcher(t)
		rg.d.Apply(context.Background(), notify.Change{Templates: true})
		waitFlush(t, func() bool { f, g := rg.sched.counts(); return f > 0 || len(g) > 0 })
		full, groups := rg.sched.counts()
		require.Equal(t, 1, full, "templates → sched 全量")
		require.Empty(t, groups)
		require.Equal(t, 1, rg.clients.calls(), "templates → clients 失效")
	})

	t.Run("Groups", func(t *testing.T) {
		rg := newTestDispatcher(t)
		rg.d.Apply(context.Background(), notify.Change{Groups: []int64{10, 20}})
		waitFlush(t, func() bool { _, g := rg.sched.counts(); return len(g) > 0 })
		_, groups := rg.sched.counts()
		require.ElementsMatch(t, []int64{10, 20}, groups, "groups → sched 组级定向")
		require.Equal(t, 0, rg.clients.calls(), "纯组级变更不碰 clients")
	})

	t.Run("GroupsWithClients", func(t *testing.T) {
		rg := newTestDispatcher(t)
		// 账号 upstream_key 变更：组级 + clients 同批（service account.go 发布点形态）
		rg.d.Apply(context.Background(), notify.Change{Groups: []int64{10}, Clients: true})
		waitFlush(t, func() bool { _, g := rg.sched.counts(); return len(g) > 0 && rg.clients.calls() > 0 })
		_, groups := rg.sched.counts()
		require.ElementsMatch(t, []int64{10}, groups)
		require.Equal(t, 1, rg.clients.calls())
	})

	t.Run("StandaloneClients", func(t *testing.T) {
		rg := newTestDispatcher(t)
		// 防御性兜底：服务端发布点恒与 Templates/Groups 并排，独立 Clients 也映射
		rg.d.Apply(context.Background(), notify.Change{Clients: true})
		waitFlush(t, func() bool { return rg.clients.calls() > 0 })
		full, groups := rg.sched.counts()
		require.Equal(t, 0, full, "独立 clients 不触发 sched 重载")
		require.Empty(t, groups)
		require.Equal(t, 1, rg.clients.calls())
	})

	t.Run("Multipliers", func(t *testing.T) {
		rg := newTestDispatcher(t)
		rg.d.Apply(context.Background(), notify.Change{Multipliers: true})
		waitFlush(t, func() bool { return rg.bal.multCalls() > 0 })
		require.Equal(t, 1, rg.bal.multCalls(), "multipliers → 余额倍率定向刷新")
	})

	t.Run("Keys", func(t *testing.T) {
		rg := newTestDispatcher(t)
		rg.d.Apply(context.Background(), notify.Change{Keys: true})
		waitFlush(t, func() bool { return rg.auth.calls() > 0 })
		require.Equal(t, 1, rg.auth.calls(), "keys → auth 快照全量 Reload")
	})

	t.Run("Settings", func(t *testing.T) {
		rg := newTestDispatcher(t)
		rg.d.Apply(context.Background(), notify.Change{Settings: true})
		// #36 时序（R2 M-1）：settings 快照同步刷新（Apply 内，非去抖 flush——
		// scope 重载必须读到新 N）。
		require.Equal(t, 1, rg.settings.calls(), "settings → settings 快照同步重载")
		// 注册表按 ScopeSettings 精确重载（auth 即时生效，不等待去抖窗口）。
		require.Eventually(t, func() bool { return rg.snapAuth.calls() > 0 }, time.Second, time.Millisecond)
		require.Equal(t, 1, rg.snapAuth.calls(), "settings → 声明 settings scope 的 auth 快照重载")
		// 同步重载已覆盖去抖 Mark：无 200ms 后的重复 ReloadSettings。
		time.Sleep(5 * time.Millisecond)
		require.Equal(t, 1, rg.settings.calls(), "settings 同步重载后无去抖重复重载")
	})

	t.Run("Rules", func(t *testing.T) {
		rg := newTestDispatcher(t)
		rg.d.Apply(context.Background(), notify.Change{Rules: true})
		waitFlush(t, func() bool { return rg.rules.calls() > 0 })
		require.Equal(t, 1, rg.rules.calls(), "rules → 规则表全量重载")
	})

	t.Run("DegradedFullWithGroups", func(t *testing.T) {
		rg := newTestDispatcher(t)
		// 载荷守卫降级形态（Groups 超限 → Templates=true，R9）：组级被全量包含
		// 跳过，语义仍正确（不重复组级重载）。
		rg.d.Apply(context.Background(), notify.Change{Templates: true, Groups: []int64{10}})
		waitFlush(t, func() bool { f, _ := rg.sched.counts(); return f > 0 })
		full, groups := rg.sched.counts()
		require.Equal(t, 1, full)
		require.Empty(t, groups, "Templates 存在时组级被包含跳过（去抖器 merge 语义）")
	})

	t.Run("EmptyChange", func(t *testing.T) {
		rg := newTestDispatcher(t)
		// 空 Change：无任何标记（service.publish 已判空跳过，双保险）
		rg.d.Apply(context.Background(), notify.Change{})
		time.Sleep(5 * time.Millisecond)
		require.Equal(t, 0, rg.auth.calls())
		full, groups := rg.sched.counts()
		require.Equal(t, 0, full)
		require.Empty(t, groups)
		require.Equal(t, 0, rg.clients.calls())
		require.Equal(t, 0, rg.bal.multCalls())
		require.Equal(t, 0, rg.settings.calls())
		require.Equal(t, 0, rg.rules.calls())
		require.Equal(t, 0, rg.snapAuth.calls(), "空变更不触发注册表 scope 重载")
	})
}

// TestDispatcherSettingsScopePrecision settings 变更按 scope 精确分发：只重载
// 声明 ScopeSettings 的快照（auth），未声明的（scheduler/rules/balances）不动
// （脏标记语义——变更只对应其快照集合）。
func TestDispatcherSettingsScopePrecision(t *testing.T) {
	rg := newTestDispatcher(t)
	rg.d.Apply(context.Background(), notify.Change{Settings: true})
	require.Eventually(t, func() bool { return rg.snapAuth.calls() > 0 }, time.Second, time.Millisecond)
	require.Equal(t, 1, rg.snapAuth.calls())
	require.Equal(t, 0, rg.snapSched.calls(), "settings 变更不重载未声明 settings scope 的 scheduler")
	require.Equal(t, 0, rg.snapRules.calls())
	require.Equal(t, 0, rg.snapBal.calls())
	// 其它变更类型不走注册表（去抖器路径已断言于映射表测试）：确认无注册表
	// 误触发——users 变更后 auth 快照仍只有 settings 那一次。
	rg.d.Apply(context.Background(), notify.Change{Users: true})
	waitFlush(t, func() bool { return rg.auth.calls() > 0 })
	require.Equal(t, 1, rg.snapAuth.calls(), "users 变更不触发注册表（去抖器矩阵覆盖）")
	require.Equal(t, 0, rg.snapSched.calls())
}

// TestDispatcherApplyMergesSingleFlush 本地+远端同窗口合并：一条 NOTIFY 与
// 本地变更落入同一去抖窗口 → 一次 flush，不重复 reload（设计文档 §2.3）。
func TestDispatcherApplyMergesSingleFlush(t *testing.T) {
	rg := newTestDispatcher(t)
	rg.d.Apply(context.Background(), notify.Change{Users: true})
	rg.inv.Users() // 模拟本地 admin 变更（同一去抖器）
	waitFlush(t, func() bool { return rg.auth.calls() > 0 })
	require.Equal(t, 1, rg.auth.calls(), "远端 + 本地同窗口合并为一次 reload")
}

// errSettings2 ReloadSettings 恒失败（G1-1 Apply 失败注入）。
type errSettings2 struct{ recSettings2 }

func (e *errSettings2) ReloadSettings(ctx context.Context) error {
	return errors.New("settings boom")
}

// errSnap2 快照 Reload 恒失败但记录调用（G1-1 Apply 失败注入）。
type errSnap2 struct {
	recSnap
}

func (e *errSnap2) Reload(ctx context.Context) error {
	e.recSnap.Reload(ctx)
	return errors.New("snapshot boom")
}

// TestDispatcherApplyFailureTolerated G1-1（p2-02/G-P2-1）：Apply 无返回值——
// 内部失败（settings 同步重载失败 + 注册表 scope 快照重载失败）独立 Warn 消
// 化，不透传：无 panic，同批其他变更位仍被吞入去抖（事件提示语义不破坏）。
func TestDispatcherApplyFailureTolerated(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	auth := &recAuth2{}
	inv := invalidate.New(invalidate.Config{
		Window:   time.Millisecond,
		Sched:    &recSched2{},
		Clients:  &recClients2{},
		Auth:     auth,
		Balances: &recBal2{},
		Rules:    &recRules2{},
	})
	settings := &errSettings2{}
	inv.SetSettings(settings)
	failSnap := &errSnap2{recSnap: recSnap{name: "auth", scopes: []string{snapshot.ScopeSettings}}}
	reg := snapshot.New()
	require.NoError(t, reg.Register(failSnap))
	d := &dispatcher{inv: inv, svc: settings, snapshots: reg, log: nil} // log nil = 静默（测试）
	require.NoError(t, inv.Start(ctx))

	// settings 同步重载失败 + scope 快照重载失败：Apply 无返回值、无 panic。
	d.Apply(ctx, notify.Change{Settings: true, Users: true})
	waitFlush(t, func() bool { return auth.calls() > 0 })
	require.Equal(t, 1, auth.calls(), "settings 分支失败不影响同批 users 变更吞入去抖")
	require.Equal(t, 1, failSnap.calls(), "scope 重载失败也尝试过（Warn 消化）")
}

// settingsNStub settings 快照桩（N 时序断言）：ReloadSettings 从 dbN 现读——
// 模拟 DB 权威值（远端实例 UpdateSetting 已落库，本实例经 NOTIFY 触发重载）；
// 快照值 snapN 供 auth 桩在 reload 时刻读取（模拟"快照先刷、reload 后行"的
// 现读语义；cluster.instances 已删，spec 2026-08-25-redis-instance-discovery-design
// §2.4，时序契约本身仍在）。
type settingsNStub struct {
	dbN   atomic.Int64 // 模拟 DB 当前值（外部变更写入）
	snapN atomic.Int64 // 快照值（ReloadSettings 后 = dbN）
}

func (s *settingsNStub) ReloadSettings(ctx context.Context) error {
	s.snapN.Store(s.dbN.Load())
	return nil
}
func (s *settingsNStub) N() int { return int(s.snapN.Load()) }

// recSnapN auth 快照桩：Reload 时刻记录 settings 桩快照 N——模拟 gate.reload 在
// auth.Reload 内（LoadKeys/LoadUsers 之后）现读 provider 的时序，期间快照不被
// 改动，观测等价。
type recSnapN struct {
	recSnap
	s     *settingsNStub
	mu    sync.Mutex
	lastN int
}

func (r *recSnapN) Reload(ctx context.Context) error {
	err := r.recSnap.Reload(ctx)
	r.mu.Lock()
	r.lastN = r.s.N()
	r.mu.Unlock()
	return err
}
func (r *recSnapN) observedN() int { r.mu.Lock(); defer r.mu.Unlock(); return r.lastN }

// TestDispatcherSettingsTiming settings 变更时序（R2 M-1 #36 即时重算）：
// settings 旧 N → 远端变更落库（dbN 新 N）→ Apply(Change{Settings:true}) →
// auth.Reload 必须读到新 N。顺序保证：settings 快照先同步刷新、scope 精确重载
// 后执行——修复前 d.inv.Settings() 仅 Mark（去抖 200ms 后才 flush
// ReloadSettings），reloadScopes 同步 auth.Reload 读到旧 N = 白重算（新 N 落地
// 后再无 gate.reload 触发）。本测试修复前红：auth 快照仅 reload 一次且读到旧 N
// （observedN 恒 1）。inv 不 Start（settings 分支 sync 后不依赖去抖器；不 Start
// 使修复前形态确定性红——flush 永不执行，快照保持旧 N）。
func TestDispatcherSettingsTiming(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	st := &settingsNStub{}
	st.dbN.Store(1) // 旧 N
	snapAuth := &recSnapN{s: st}
	snapAuth.name = "auth"
	snapAuth.scopes = []string{snapshot.ScopeSettings}

	inv := invalidate.New(invalidate.Config{
		Window: time.Millisecond, Sched: &recSched2{}, Clients: &recClients2{},
		Auth: &recAuth2{}, Balances: &recBal2{}, Rules: &recRules2{},
	})
	inv.SetSettings(st)
	reg := snapshot.New()
	require.NoError(t, reg.Register(snapAuth))
	d := &dispatcher{inv: inv, svc: st, snapshots: reg}

	// 模拟远端实例 UpdateSetting 落库（本实例 settings 快照仍是旧 N）。
	st.dbN.Store(2)
	d.Apply(ctx, notify.Change{Settings: true})

	// settings 快照同步刷新（Apply 内，非去抖 flush）→ auth reload 读到新 N。
	require.Equal(t, 2, st.N(), "Apply 后 settings 快照已是新 N（同步重载）")
	require.Eventually(t, func() bool { return snapAuth.calls() > 0 }, time.Second, time.Millisecond)
	require.Equal(t, 2, snapAuth.observedN(), "auth.Reload 时 gate 预算读到的 N = 新 N（快照先刷新、scope 后重载）")
}

// TestDispatcherFullRefresh FullRefresh 覆盖全部五路重载（设计文档 §2.3）：
// 注册表 ReloadAll（auth + sched + rules + balances）+ settings 快照；billing
// 关闭（balances 未注册）→ 跳过余额路径。
func TestDispatcherFullRefresh(t *testing.T) {
	t.Run("all", func(t *testing.T) {
		rg := newTestDispatcher(t)
		require.NoError(t, rg.d.FullRefresh(context.Background()))
		require.Equal(t, 1, rg.snapAuth.calls())
		require.Equal(t, 1, rg.snapSched.calls(), "sched 全量一次（注册表 ReloadAll）")
		require.Equal(t, 1, rg.snapRules.calls())
		require.Equal(t, 1, rg.snapBal.calls(), "balances Reload 一次")
		require.Equal(t, 1, rg.settings.calls(), "settings 快照重载一次")
		require.Equal(t, 0, rg.sched.full+len(rg.sched.groups), "FullRefresh 不再直调去抖器 sched（注册表路径）")
	})

	t.Run("billingDisabled", func(t *testing.T) {
		sched := &recSched2{}
		settings := &recSettings2{}
		inv := invalidate.New(invalidate.Config{Window: time.Millisecond, Sched: sched, Clients: &recClients2{}, Auth: &recAuth2{}, Rules: &recRules2{}})
		inv.SetSettings(settings)
		snapAuth := &recSnap{name: "auth", scopes: []string{snapshot.ScopeSettings}}
		snapSched := &recSnap{name: "scheduler"}
		reg := snapshot.New()
		for _, s := range []snapshot.Snapshot{snapAuth, snapSched} {
			require.NoError(t, reg.Register(s))
		}
		d := &dispatcher{inv: inv, svc: settings, snapshots: reg}
		require.NoError(t, d.FullRefresh(context.Background()))
		require.Equal(t, 1, snapAuth.calls())
		require.Equal(t, 1, snapSched.calls())
		require.Equal(t, 1, settings.calls())
	})

	t.Run("snapshotsUnconfigured", func(t *testing.T) {
		// 注册表未装配（nil）：FullRefresh 仍重载 settings（防御性兜底）。
		settings := &recSettings2{}
		inv := invalidate.New(invalidate.Config{Window: time.Millisecond, Sched: &recSched2{}, Clients: &recClients2{}, Auth: &recAuth2{}})
		inv.SetSettings(settings)
		d := &dispatcher{inv: inv, svc: settings}
		require.NoError(t, d.FullRefresh(context.Background()))
		require.Equal(t, 1, settings.calls())
	})
}

// TestDispatcherFullRefreshFirstConnectSkip E2 启动双刷（E-P2-4）：main 启动
// 首刷全成功（ReloadAll 返回空 map）置位 bootLoaded → 首连 FullRefresh 跳过
// 五路 ReloadAll（健康启动下第二遍纯冗余消除，ReloadAll 至多一次）、仅补
// ReloadSettings；断线重连（第二次调用）恒全量刷新不变。
func TestDispatcherFullRefreshFirstConnectSkip(t *testing.T) {
	rg := newTestDispatcher(t)
	// 模拟 main 启动序：注册表 ReloadAll 首刷（全部成功）→ 置位跳过标志。
	require.Empty(t, rg.d.snapshots.ReloadAll(context.Background()))
	rg.d.bootLoaded.Store(true)

	// 首连：跳过五路 ReloadAll（快照不再重载），仅补 ReloadSettings。
	require.NoError(t, rg.d.FullRefresh(context.Background()))
	require.Equal(t, 1, rg.snapAuth.calls(), "首连跳过注册表 ReloadAll（auth 仅首刷那一次）")
	require.Equal(t, 1, rg.snapSched.calls())
	require.Equal(t, 1, rg.snapRules.calls())
	require.Equal(t, 1, rg.snapBal.calls())
	require.Equal(t, 1, rg.settings.calls(), "首连仍补 ReloadSettings（svc 不在注册表内，保持既有语义）")

	// 断线重连：恒全量刷新（标志已消费，不再跳过）。
	require.NoError(t, rg.d.FullRefresh(context.Background()))
	require.Equal(t, 2, rg.snapAuth.calls())
	require.Equal(t, 2, rg.snapSched.calls())
	require.Equal(t, 2, rg.snapRules.calls())
	require.Equal(t, 2, rg.snapBal.calls())
	require.Equal(t, 2, rg.settings.calls())
}

// TestDispatcherFullRefreshBootFailedFallback 首刷部分失败（main 不置位标志）→
// 首连仍全量刷新（兜底收敛不破坏——DB 故障时快照由 listener FullRefresh 补齐）；
// 断线重连仍全量。
func TestDispatcherFullRefreshBootFailedFallback(t *testing.T) {
	rg := newTestDispatcher(t)
	// bootLoaded 零值 = 未置位（模拟 ReloadAll 返回错误，main 不置位）。
	require.NoError(t, rg.d.FullRefresh(context.Background()))
	require.Equal(t, 1, rg.snapAuth.calls())
	require.Equal(t, 1, rg.snapSched.calls())
	require.Equal(t, 1, rg.snapRules.calls())
	require.Equal(t, 1, rg.snapBal.calls())
	require.Equal(t, 1, rg.settings.calls())

	// 断线重连仍全量。
	require.NoError(t, rg.d.FullRefresh(context.Background()))
	require.Equal(t, 2, rg.snapAuth.calls())
	require.Equal(t, 2, rg.snapSched.calls())
	require.Equal(t, 2, rg.settings.calls())
}
