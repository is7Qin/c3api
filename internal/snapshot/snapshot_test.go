// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package snapshot

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// recSnap 记录型 fake 快照：并发计数 + 可注入错误/阻塞（scope 分发与并行断言用）。
type recSnap struct {
	name   string
	scopes []string
	mu     sync.Mutex
	n      int
	err    error
	// block 非 nil 时 Reload 等待其关闭再返回（触发串行断言用）。
	block <-chan struct{}
}

func (r *recSnap) Name() string     { return r.name }
func (r *recSnap) Scopes() []string { return r.scopes }
func (r *recSnap) Reload(ctx context.Context) error {
	r.mu.Lock()
	r.n++
	err := r.err
	block := r.block
	r.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}
func (r *recSnap) calls() int { r.mu.Lock(); defer r.mu.Unlock(); return r.n }

func newRec(name string, scopes ...string) *recSnap {
	return &recSnap{name: name, scopes: scopes}
}

// TestRegister 注册：名称/scope 进 Status；重复名拒绝；空名拒绝。
func TestRegister(t *testing.T) {
	reg := New()
	require.NoError(t, reg.Register(newRec("auth", ScopeSettings)))
	require.NoError(t, reg.Register(newRec("sched")))
	require.Error(t, reg.Register(newRec("auth")), "重复名拒绝")
	require.Error(t, reg.Register(newRec("")), "空名拒绝")

	st := reg.Status()
	require.Len(t, st, 2)
	require.Equal(t, "auth", st[0].Name)
	require.Equal(t, []string{ScopeSettings}, st[0].Scopes)
	require.True(t, st[0].LastReload.IsZero(), "未触发前 LastReload 为零值")
	require.NoError(t, st[0].LastError)
	require.Equal(t, "sched", st[1].Name)
	require.Empty(t, st[1].Scopes)
}

// TestReloadAllParallel ReloadAll 并行执行全部快照一次；成功更新状态。
func TestReloadAllParallel(t *testing.T) {
	reg := New()
	a, b, c := newRec("a"), newRec("b"), newRec("c")
	require.NoError(t, reg.Register(a))
	require.NoError(t, reg.Register(b))
	require.NoError(t, reg.Register(c))

	require.Empty(t, reg.ReloadAll(context.Background()))
	require.Equal(t, 1, a.calls())
	require.Equal(t, 1, b.calls())
	require.Equal(t, 1, c.calls())

	st := reg.Status()
	for _, s := range st {
		require.False(t, s.LastReload.IsZero(), "%s 已触发", s.Name)
		require.NoError(t, s.LastError, "%s 无错误", s.Name)
	}
}

// TestReloadAllErrorsIndependent 错误独立收集：失败者进返回 map + Status 记录
// 错误；成功者不受影响（并行执行，非整体失败）。
func TestReloadAllErrorsIndependent(t *testing.T) {
	reg := New()
	ok := newRec("ok")
	failA := newRec("fail-a")
	failA.err = errors.New("boom-a")
	failB := newRec("fail-b")
	failB.err = errors.New("boom-b")
	require.NoError(t, reg.Register(ok))
	require.NoError(t, reg.Register(failA))
	require.NoError(t, reg.Register(failB))

	errs := reg.ReloadAll(context.Background())
	require.Equal(t, 2, len(errs))
	require.EqualError(t, errs["fail-a"], "boom-a")
	require.EqualError(t, errs["fail-b"], "boom-b")
	require.NotContains(t, errs, "ok")
	require.Equal(t, 1, ok.calls(), "失败者不影响成功者执行")

	st := reg.Status()
	for _, s := range st {
		require.False(t, s.LastReload.IsZero())
		if s.Name == "fail-a" || s.Name == "fail-b" {
			require.Error(t, s.LastError)
		} else {
			require.NoError(t, s.LastError)
		}
	}

	// 再次全刷（成功）：LastError 清空（状态反映最近一次结果）。
	failA.err = nil
	failB.err = nil
	require.Empty(t, reg.ReloadAll(context.Background()))
	for _, s := range reg.Status() {
		require.NoError(t, s.LastError, "%s 失败已清除", s.Name)
	}
}

// TestReloadByScope scope 分发精确性：变更 scope x 只重载声明 x 的快照，声明
// y 的未命中不动（脏标记语义）；同快照多 scope 命中去重一次。
func TestReloadByScope(t *testing.T) {
	reg := New()
	a := newRec("a", "x")
	b := newRec("b", "y")
	c := newRec("c", "x", "y")
	d := newRec("d") // 无 scope：纯启动/状态快照，永不响应 scope 分发
	for _, s := range []Snapshot{a, b, c, d} {
		require.NoError(t, reg.Register(s))
	}

	errs := reg.Reload(context.Background(), "x")
	require.Empty(t, errs)
	require.Equal(t, 1, a.calls(), "scope x → a 重载")
	require.Equal(t, 0, b.calls(), "scope x → b（声明 y）不动")
	require.Equal(t, 1, c.calls(), "scope x → c（含 x）重载")
	require.Equal(t, 0, d.calls(), "无 scope 快照不响应分发")

	// 多 scope 同批：命中并集；c 双命中只执行一次。
	errs = reg.Reload(context.Background(), "x", "y")
	require.Empty(t, errs)
	require.Equal(t, 2, a.calls())
	require.Equal(t, 1, b.calls())
	require.Equal(t, 2, c.calls(), "多 scope 命中去重为一次")
	require.Equal(t, 0, d.calls())

	// 未命中 scope / 空 scopes：no-op。
	require.Empty(t, reg.Reload(context.Background(), "unknown"))
	require.Empty(t, reg.Reload(context.Background()))
	require.Equal(t, 2, a.calls(), "未命中不触发")
}

// TestReloadEmptyScopesNoLock 空 scopes 前置 return：不取
// execMu——并发触发 ReloadAll 阻塞中（execMu 被持有）时 Reload() 立即返回
// （修复前排队等触发完成，零状态读取无此必要）。
func TestReloadEmptyScopesNoLock(t *testing.T) {
	reg := New()
	gate := make(chan struct{})
	a := newRec("a")
	a.block = gate
	require.NoError(t, reg.Register(a))
	require.NoError(t, reg.Register(newRec("b")))

	done := make(chan struct{})
	go func() {
		reg.ReloadAll(context.Background())
		close(done)
	}()
	// 等 a 进入 Reload（阻塞中）——ReloadAll 已持 execMu。
	require.Eventually(t, func() bool { return a.calls() == 1 }, time.Second, time.Millisecond)

	start := time.Now()
	require.Empty(t, reg.Reload(context.Background()))
	require.Less(t, time.Since(start), 200*time.Millisecond, "空 scopes 前置 return：不等待 execMu（零状态读取）")

	close(gate)
	require.Empty(t, <-done)
}

// TestReloadScopeErrors scope 分发同样错误独立收集。
func TestReloadScopeErrors(t *testing.T) {
	reg := New()
	a := newRec("a", "x")
	a.err = errors.New("x-fail")
	b := newRec("b", "x")
	require.NoError(t, reg.Register(a))
	require.NoError(t, reg.Register(b))

	errs := reg.Reload(context.Background(), "x")
	require.Equal(t, map[string]error{"a": errors.New("x-fail")}, errs)
	require.Equal(t, 1, a.calls())
	require.Equal(t, 1, b.calls())
}

// TestReloadAllSerializedAcrossTriggers 触发串行：并发两次 ReloadAll，第二次
// 等第一次全部完成（execMu——事件不重叠执行；阻塞快照上断言无并行双刷）。
func TestReloadAllSerializedAcrossTriggers(t *testing.T) {
	reg := New()
	gate := make(chan struct{})
	a := newRec("a")
	a.block = gate
	require.NoError(t, reg.Register(a))
	require.NoError(t, reg.Register(newRec("b")))

	done1 := make(chan map[string]error)
	go func() { done1 <- reg.ReloadAll(context.Background()) }()
	// 等 a 进入 Reload（阻塞中）——若第二个触发并行执行，a.calls 会 > 1。
	require.Eventually(t, func() bool { return a.calls() == 1 }, time.Second, time.Millisecond)
	done2 := make(chan map[string]error)
	go func() { done2 <- reg.ReloadAll(context.Background()) }()
	time.Sleep(50 * time.Millisecond) // 第二个触发应被 execMu 挡住
	require.Equal(t, 1, a.calls(), "第二个触发在第一个完成前不执行")

	close(gate) // 放行第一个
	require.Empty(t, <-done1)
	require.Empty(t, <-done2)
	require.Equal(t, 2, a.calls(), "串行执行完成后两个触发都完成")
}

// TestConcurrentRegisterAndReload 注册与触发并发（-race 下无竞态）：注册/全刷/
// scope 分发/状态读并发打。
func TestConcurrentRegisterAndReload(t *testing.T) {
	reg := New()
	var next atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() { // 并发注册
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				id := next.Add(1)
				_ = reg.Register(newRec("s"+strconv.FormatInt(id, 10), "x"))
			}
		}
	}()
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = reg.ReloadAll(context.Background())
					_ = reg.Reload(context.Background(), "x")
					_ = reg.Status()
				}
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}
