// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package invalidate

import (
	"context"
	"sync"
	"testing"
	"time"
)

// --- fake 重载目标（记录各自被调用的重载方式；后沿测试可阻塞） ---

type recSched struct {
	mu     sync.Mutex
	full   int
	groups []int64
}

func (r *recSched) InvalidateAll() {
	r.mu.Lock()
	r.full++
	r.mu.Unlock()
}
func (r *recSched) InvalidateGroup(gid int64) {
	r.mu.Lock()
	r.groups = append(r.groups, gid)
	r.mu.Unlock()
}
func (r *recSched) groupCalls() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.groups...)
}
func (r *recSched) fullCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.full
}

type recClients struct {
	mu sync.Mutex
	n  int
}

func (r *recClients) InvalidateAll() { r.mu.Lock(); r.n++; r.mu.Unlock() }
func (r *recClients) calls() int     { r.mu.Lock(); defer r.mu.Unlock(); return r.n }

// recAuth 记录 Reload；block 非 nil 时每次调用先阻塞（后沿语义测试）。
type recAuth struct {
	mu      sync.Mutex
	n       int
	started chan struct{} // 阻塞前信号（缓冲 1）
	block   chan struct{} // 非 nil：等待其关闭再完成
}

func (r *recAuth) Reload(ctx context.Context) error {
	if r.block != nil {
		select {
		case r.started <- struct{}{}:
		default:
		}
		<-r.block
	}
	r.mu.Lock()
	r.n++
	r.mu.Unlock()
	return nil
}
func (r *recAuth) calls() int { r.mu.Lock(); defer r.mu.Unlock(); return r.n }

type recBal struct {
	mu   sync.Mutex
	rel  int
	mult int
}

func (r *recBal) Reload(ctx context.Context) error { r.mu.Lock(); r.rel++; r.mu.Unlock(); return nil }
func (r *recBal) ReloadMultipliers(ctx context.Context) error {
	r.mu.Lock()
	r.mult++
	r.mu.Unlock()
	return nil
}
func (r *recBal) relCalls() int { r.mu.Lock(); defer r.mu.Unlock(); return r.rel }
func (r *recBal) multCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mult
}

// recSettings 记录 ReloadSettings 调用（#14 T1 新增分支的 fake 目标）。
type recSettings struct {
	mu sync.Mutex
	n  int
}

func (r *recSettings) ReloadSettings(ctx context.Context) error {
	r.mu.Lock()
	r.n++
	r.mu.Unlock()
	return nil
}
func (r *recSettings) calls() int { r.mu.Lock(); defer r.mu.Unlock(); return r.n }

// recRules 记录 ReloadRules 调用（#14 T1 新增分支的 fake 目标）。
type recRules struct {
	mu sync.Mutex
	n  int
}

func (r *recRules) ReloadRules(ctx context.Context) error {
	r.mu.Lock()
	r.n++
	r.mu.Unlock()
	return nil
}
func (r *recRules) calls() int { r.mu.Lock(); defer r.mu.Unlock(); return r.n }

// fakeClock 可注入定时器（fake 时钟）：newTimer 每次调用记录当前 channel 并
// 递增代数（fire 须作用于最新定时器——waitTimer 以代数同步）；fire 手动触发
// 到期。
type fakeClock struct {
	mu  sync.Mutex
	ch  chan time.Time
	gen int
}

func (c *fakeClock) newTimer(time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	c.ch = make(chan time.Time, 1)
	return c.ch
}

func (c *fakeClock) genNow() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// waitTimer 轮询直到执行 goroutine 已创建新定时器（代数 > afterGen；Mark 后、
// fire 前的同步点）。
func (c *fakeClock) waitTimer(t *testing.T, afterGen int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if c.genNow() > afterGen {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timer not created after mark")
		}
		time.Sleep(time.Millisecond)
	}
}

// fire 触发当前定时器到期（后沿立即再执行不需要 fire——drain 循环直接执行）。
func (c *fakeClock) fire() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ch <- time.Now()
}

// markAndFire 标记 → 等定时器创建 → 触发到期。
func (r *rig) markAndFire(k Kind, gids []int64) {
	gen := r.clock.genNow()
	r.d.mark(k, gids)
	r.clock.waitTimer(r.t, gen)
	r.clock.fire()
}

type rig struct {
	t        *testing.T
	d        *Debouncer
	clock    *fakeClock
	sched    *recSched
	cl       *recClients
	auth     *recAuth
	bal      *recBal
	settings *recSettings
	rules    *recRules
	cancel   context.CancelFunc
}

func newRig(t *testing.T, auth *recAuth) *rig {
	t.Helper()
	r := &rig{
		t:        t,
		clock:    &fakeClock{},
		sched:    &recSched{},
		cl:       &recClients{},
		auth:     auth,
		bal:      &recBal{},
		settings: &recSettings{},
		rules:    &recRules{},
	}
	r.d = New(Config{Window: time.Hour, Sched: r.sched, Clients: r.cl, Auth: auth, Balances: r.bal, Settings: r.settings, Rules: r.rules})
	r.d.newTimer = r.clock.newTimer
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	requireNoErr(t, r.d.Start(ctx))
	t.Cleanup(cancel)
	return r
}

// waitCalls 轮询直到 fn 返回值满足（reloadAll 在 loop goroutine 同步执行，
// 断言前需等其完成）。
func waitCalls(t *testing.T, fn func() int, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if fn() >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected %d calls, got %d", want, fn())
		}
		time.Sleep(time.Millisecond)
	}
}

func requireNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- 测试 ---

// TestDebounceMerge 同窗口多次变更（含同类型重复）= 1 次合并重载。
func TestDebounceMerge(t *testing.T) {
	r := newRig(t, &recAuth{})
	gen := r.clock.genNow() // 评审 M-1：代数读取必须放在首次 mark 前——执行 goroutine
	// 可能已在 mark 后建好定时器（代数已推进），后读会等到永不出现的新定时器。
	r.d.Users()
	r.d.Users() // 重复并入
	r.d.Templates()
	r.d.Multipliers()
	r.d.mark(0, nil) // 额外唤醒（已有脏 → 无新定时器）
	r.clock.waitTimer(t, gen)
	r.clock.fire()
	waitCalls(t, r.auth.calls, 1)
	// 一次合并重载：auth 1 次（非 2）、余额全量 1、sched 全量 1、clients 1、
	// 倍率定向 1。
	if got := r.auth.calls(); got != 1 {
		t.Fatalf("auth reloads = %d, want 1（同窗口合并）", got)
	}
	if got := r.bal.relCalls(); got != 1 {
		t.Fatalf("balance reloads = %d, want 1", got)
	}
	if got := r.sched.fullCalls(); got != 1 {
		t.Fatalf("sched full = %d, want 1", got)
	}
	if got := r.cl.calls(); got != 1 {
		t.Fatalf("clients = %d, want 1", got)
	}
	if got := r.bal.multCalls(); got != 1 {
		t.Fatalf("multiplier reloads = %d, want 1", got)
	}
}

// TestMatrixPerEntity 矩阵逐实体断言：用户/模板/账号/组倍率各走各自重载方式。
func TestMatrixPerEntity(t *testing.T) {
	t.Run("users→auth+bal全量，不动sched/clients", func(t *testing.T) {
		r := newRig(t, &recAuth{})
		r.markAndFire(KindUsers, nil)
		waitCalls(t, r.auth.calls, 1)
		if r.bal.relCalls() != 1 || r.sched.fullCalls() != 0 || r.cl.calls() != 0 || r.bal.multCalls() != 0 {
			t.Fatalf("users 只应 auth+bal 全量：auth=%d bal=%d schedFull=%d clients=%d mult=%d",
				r.auth.calls(), r.bal.relCalls(), r.sched.fullCalls(), r.cl.calls(), r.bal.multCalls())
		}
	})
	t.Run("templates→sched全量+clients，不动auth/bal", func(t *testing.T) {
		r := newRig(t, &recAuth{})
		r.markAndFire(KindTemplates, nil)
		waitCalls(t, r.sched.fullCalls, 1)
		if r.cl.calls() != 1 || r.auth.calls() != 0 || r.bal.relCalls() != 0 {
			t.Fatalf("templates 只应 sched+clients")
		}
	})
	t.Run("accounts→组级定向，不动他组/全量", func(t *testing.T) {
		r := newRig(t, &recAuth{})
		r.markAndFire(0, []int64{5, 6})
		waitCalls(t, func() int { return len(r.sched.groupCalls()) }, 2)
		if got := r.sched.fullCalls(); got != 0 {
			t.Fatalf("accounts 不应触发 sched 全量, got %d", got)
		}
		if got := r.cl.calls(); got != 0 {
			t.Fatalf("upstream_key 未变更不应失效 clients")
		}
		// 定向正确性：仅标记的组被重载
		if got := r.sched.groupCalls(); len(got) != 2 || !contains(got, 5) || !contains(got, 6) {
			t.Fatalf("组级重载 = %v, want {5,6}", got)
		}
	})
	t.Run("accounts keyChanged→额外clients失效", func(t *testing.T) {
		r := newRig(t, &recAuth{})
		r.markAndFire(KindClients, nil)
		waitCalls(t, r.cl.calls, 1)
		if r.sched.fullCalls() != 0 {
			t.Fatalf("keyChanged 不应触发 sched 全量")
		}
	})
	t.Run("multipliers→倍率定向刷新，不动余额/sched", func(t *testing.T) {
		r := newRig(t, &recAuth{})
		r.markAndFire(KindMultipliers, nil)
		waitCalls(t, r.bal.multCalls, 1)
		if r.bal.relCalls() != 0 || r.sched.fullCalls() != 0 || r.auth.calls() != 0 {
			t.Fatalf("multipliers 只应倍率定向刷新")
		}
	})
}

// TestGroupMergeAndSubsume 同窗口多组并集；与模板全量同窗口 → 组级被包含跳过。
func TestGroupMergeAndSubsume(t *testing.T) {
	t.Run("多组并集", func(t *testing.T) {
		r := newRig(t, &recAuth{})
		gen := r.clock.genNow() // 评审 M-1：代数读取在首次 mark 前（见 TestDebounceMerge）
		r.d.Accounts([]int64{5}, false)
		r.d.Accounts([]int64{6}, false) // 窗口内并入
		r.d.mark(0, nil)
		r.clock.waitTimer(t, gen)
		r.clock.fire()
		waitCalls(t, func() int { return len(r.sched.groupCalls()) }, 2)
		if got := r.sched.groupCalls(); len(got) != 2 || !contains(got, 5) || !contains(got, 6) {
			t.Fatalf("组级并集 = %v, want {5,6}", got)
		}
	})
	t.Run("模板全量包含组级", func(t *testing.T) {
		r := newRig(t, &recAuth{})
		gen := r.clock.genNow() // 评审 M-1：代数读取在首次 mark 前
		r.d.Accounts([]int64{5}, false)
		r.d.Templates() // 同窗口：full ⊇ 组级
		r.d.mark(0, nil)
		r.clock.waitTimer(t, gen)
		r.clock.fire()
		waitCalls(t, r.sched.fullCalls, 1)
		if len(r.sched.groupCalls()) != 0 {
			t.Fatalf("模板全量应包含组级重载, got groups=%v", r.sched.groupCalls())
		}
	})
}

// TestTrailingEdge 后沿语义（评审 C-6）：长 reload 期间新脏标记 → 完成后
// 立即再执行（无需重新起窗口定时器）。
func TestTrailingEdge(t *testing.T) {
	auth := &recAuth{started: make(chan struct{}, 1), block: make(chan struct{})}
	r := newRig(t, auth)
	gen := r.clock.genNow()
	r.d.Users()
	r.clock.waitTimer(t, gen)
	r.clock.fire()
	// 第一次 reload 阻塞中
	select {
	case <-auth.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first reload did not start")
	}
	// reload 期间新变更
	r.d.Users()
	close(auth.block) // 放行第一次
	// 第二次 reload 立即执行（drain 循环），无需再 fire
	waitCalls(t, auth.calls, 2)
	// 若实现退化为"窗口再等一轮"或漏掉标记，此处会超时
}

// TestWindowPerBatch 窗口重置：前一批完成后再变更 → 新窗口 → 新一次重载。
func TestWindowPerBatch(t *testing.T) {
	r := newRig(t, &recAuth{})
	gen := r.clock.genNow()
	r.d.Users()
	r.clock.waitTimer(t, gen)
	r.clock.fire()
	waitCalls(t, r.auth.calls, 1)

	gen = r.clock.genNow()
	r.d.Users() // 第二批（独立窗口）
	r.clock.waitTimer(t, gen)
	r.clock.fire()
	waitCalls(t, r.auth.calls, 2)
}

// TestNoMarkNoReload 无变更 → 不创建定时器、不重载。
func TestNoMarkNoReload(t *testing.T) {
	r := newRig(t, &recAuth{})
	time.Sleep(50 * time.Millisecond) // 给 loop 机会（不应有定时器）
	r.clock.mu.Lock()
	created := r.clock.ch != nil
	r.clock.mu.Unlock()
	if created {
		t.Fatal("无变更不应创建定时器")
	}
	if r.auth.calls() != 0 {
		t.Fatal("无变更不应重载")
	}
}

// TestBillingDisabled 余额目标 nil（billing.enabled=false）：用户/倍率变更
// 跳过余额路径，其余正常。
func TestBillingDisabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched, cl, auth, bal := &recSched{}, &recClients{}, &recAuth{}, &recBal{}
	d := New(Config{Window: time.Hour, Sched: sched, Clients: cl, Auth: auth, Log: nil})
	d.newTimer = func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	requireNoErr(t, d.Start(ctx))
	d.Users()
	waitCalls(t, auth.calls, 1)
	if bal.relCalls() != 0 {
		t.Fatalf("balances nil 时不得调用 Reload")
	}
}

// TestNewBranches #14 T1 新增三分支（keys/settings/rules）逐实体断言：
// 各自只走自己的重载目标。
func TestNewBranches(t *testing.T) {
	t.Run("keys→auth 全量，不动 balances/settings/rules", func(t *testing.T) {
		r := newRig(t, &recAuth{})
		r.markAndFire(KindKeys, nil)
		waitCalls(t, r.auth.calls, 1)
		if r.bal.relCalls() != 0 || r.settings.calls() != 0 || r.rules.calls() != 0 || r.sched.fullCalls() != 0 {
			t.Fatalf("keys 只应 auth 全量：auth=%d bal=%d settings=%d rules=%d schedFull=%d",
				r.auth.calls(), r.bal.relCalls(), r.settings.calls(), r.rules.calls(), r.sched.fullCalls())
		}
	})
	t.Run("settings→settings 重载，不动 auth/sched/rules", func(t *testing.T) {
		r := newRig(t, &recAuth{})
		r.markAndFire(KindSettings, nil)
		waitCalls(t, r.settings.calls, 1)
		if r.auth.calls() != 0 || r.sched.fullCalls() != 0 || r.rules.calls() != 0 {
			t.Fatalf("settings 只应 settings 重载")
		}
	})
	t.Run("rules→rules 重载，不动 auth/sched/settings", func(t *testing.T) {
		r := newRig(t, &recAuth{})
		r.markAndFire(KindRules, nil)
		waitCalls(t, r.rules.calls, 1)
		if r.auth.calls() != 0 || r.sched.fullCalls() != 0 || r.settings.calls() != 0 {
			t.Fatalf("rules 只应 rules 重载")
		}
	})
}

// TestNewBranchesMerge 同窗口 keys+settings+rules 合并 → 各重载目标各 1 次
// （与用户/模板等既有位的并集语义一致）。
func TestNewBranchesMerge(t *testing.T) {
	r := newRig(t, &recAuth{})
	gen := r.clock.genNow() // 评审 M-1：代数读取在首次 mark 前
	r.d.Keys()
	r.d.Settings()
	r.d.Rules()
	r.d.Keys() // 重复并入
	r.clock.waitTimer(t, gen)
	r.clock.fire()
	waitCalls(t, r.auth.calls, 1)
	if r.auth.calls() != 1 || r.settings.calls() != 1 || r.rules.calls() != 1 {
		t.Fatalf("同窗口合并各 1 次：auth=%d settings=%d rules=%d",
			r.auth.calls(), r.settings.calls(), r.rules.calls())
	}
}

// TestNewBranchesNilReloader settings/rules reloader 未注入（nil）→ 对应分支
// 跳过，不 panic，其余正常（单实例现状行为不变）。
func TestNewBranchesNilReloader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched, cl, auth := &recSched{}, &recClients{}, &recAuth{}
	d := New(Config{Window: time.Hour, Sched: sched, Clients: cl, Auth: auth, Log: nil}) // 无 Settings/Rules
	clock := &fakeClock{}
	d.newTimer = clock.newTimer
	requireNoErr(t, d.Start(ctx))
	gen := clock.genNow()
	d.Settings()
	d.Rules()
	clock.waitTimer(t, gen)
	clock.fire()
	time.Sleep(50 * time.Millisecond) // reloadAll 在 loop goroutine 同步执行（无可断言计数，等一轮）
	if auth.calls() != 0 || sched.fullCalls() != 0 {
		t.Fatalf("settings/rules nil 时不得触发 auth/sched（auth=%d sched=%d）", auth.calls(), sched.fullCalls())
	}
}

func contains(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
