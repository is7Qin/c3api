// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package snapshot

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fnSnap 函数型 fake 快照（panic 隔离测试用）：Reload 行为由闭包注入。
type fnSnap struct {
	name   string
	scopes []string
	calls  atomic.Int64
	reload func(ctx context.Context) error
}

func (f *fnSnap) Name() string     { return f.name }
func (f *fnSnap) Scopes() []string { return f.scopes }
func (f *fnSnap) Reload(ctx context.Context) error {
	f.calls.Add(1)
	return f.reload(ctx)
}
func (f *fnSnap) count() int64 { return f.calls.Load() }

// statusByName Status 列表按名称索引（断言便利）。
func statusByName(reg *Registry) map[string]Status {
	out := make(map[string]Status)
	for _, st := range reg.Status() {
		out[st.Name] = st
	}
	return out
}

// TestReloadPanicDoesNotKillProcess 修复前回归（RED）：任一 Snapshot.Reload 的
// panic 沿 goroutine 栈展开直接终止整个进程——注册表必须把它隔离为该快照的
// 一次 reload 失败（兄弟照常执行、触发正常返回）。
func TestReloadPanicDoesNotKillProcess(t *testing.T) {
	reg := New()
	boom := &fnSnap{name: "boom", reload: func(ctx context.Context) error {
		panic("reload exploded")
	}}
	ok := &fnSnap{name: "ok", reload: func(ctx context.Context) error { return nil }}
	require.NoError(t, reg.Register(boom))
	require.NoError(t, reg.Register(ok))

	errs := reg.ReloadAll(context.Background()) // 修复前：进程在此崩溃
	require.Equal(t, int64(1), boom.count())
	require.Equal(t, int64(1), ok.count(), "panic 兄弟照常执行")
	require.Contains(t, errs, "boom")
	require.NotContains(t, errs, "ok")
}

// TestReloadPanicIsolationFull 全量 panic 隔离（spec §7.1.1）：一个 panic、一个
// 普通 error、一个 success 同批并行——进程不崩、兄弟各自执行、返回 map 只含
// 失败者、Status/LastReload 按 snapshot 独立记录。
func TestReloadPanicIsolationFull(t *testing.T) {
	reg := New()
	boom := &fnSnap{name: "boom", reload: func(ctx context.Context) error { panic("boom-value") }}
	fail := &fnSnap{name: "fail", reload: func(ctx context.Context) error { return errors.New("plain-fail") }}
	ok := &fnSnap{name: "ok", reload: func(ctx context.Context) error { return nil }}
	for _, s := range []Snapshot{boom, fail, ok} {
		require.NoError(t, reg.Register(s))
	}

	errs := reg.ReloadAll(context.Background())
	require.Len(t, errs, 2, "成功快照不入 map")
	require.Contains(t, errs, "boom")
	require.Contains(t, errs, "fail")
	require.EqualError(t, errs["fail"], "plain-fail")
	require.Equal(t, int64(1), boom.count())
	require.Equal(t, int64(1), ok.count())

	st := statusByName(reg)
	require.False(t, st["boom"].LastReload.IsZero(), "panic 也更新 LastReload")
	require.True(t, errors.Is(st["boom"].LastError, ErrReloadPanic))
	require.EqualError(t, st["fail"].LastError, "plain-fail")
	require.NoError(t, st["ok"].LastError)
}

// TestPanicErrorIdentity 错误身份（spec §7.1.2）：返回 panic 满足 errors.As 与
// errors.Is(ErrReloadPanic)；Status 只满足 errors.Is（不持有 stack——固定摘要
// 哨兵），stack 只在返回 map 错误上经 PanicStackForLog 可读。
func TestPanicErrorIdentity(t *testing.T) {
	reg := New()
	boom := &fnSnap{name: "boom", reload: func(ctx context.Context) error { panic("identity-boom") }}
	require.NoError(t, reg.Register(boom))

	errs := reg.ReloadAll(context.Background())
	err := errs["boom"]
	require.True(t, errors.Is(err, ErrReloadPanic))
	var pe *panicError
	require.True(t, errors.As(err, &pe), "返回错误为 Registry 私有 panicError")
	require.Equal(t, "boom", pe.snapshot, "按注册表名称归因")
	stack, ok := PanicStackForLog(err)
	require.True(t, ok)
	require.NotEmpty(t, stack)

	st := statusByName(reg)
	require.True(t, errors.Is(st["boom"].LastError, ErrReloadPanic))
	_, ok = PanicStackForLog(st["boom"].LastError)
	require.False(t, ok, "Status 不持有 stack")
}

// TestPanicRedaction 脱敏（spec §7.1.3）：panic value 含 secret-like 文本时，
// Error() 固定摘要、Status、管理面 projection（走 Status.LastError.Error()）
// 与日志 stack 均不出现该文本。
func TestPanicRedaction(t *testing.T) {
	reg := New()
	boom := &fnSnap{name: "boom", reload: func(ctx context.Context) error {
		panic("TOPSECRET-panic-payload")
	}}
	require.NoError(t, reg.Register(boom))

	errs := reg.ReloadAll(context.Background())
	err := errs["boom"]
	require.Equal(t, "snapshot reload panicked", err.Error(), "Error() 固定摘要")
	require.NotContains(t, err.Error(), "TOPSECRET-panic-payload")
	stack, ok := PanicStackForLog(err)
	require.True(t, ok)
	require.NotContains(t, stack, "TOPSECRET-panic-payload")

	st := statusByName(reg)
	require.Equal(t, "snapshot reload panicked", st["boom"].LastError.Error())
	require.NotContains(t, st["boom"].LastError.Error(), "TOPSECRET-panic-payload")
}

// nameOnceSnap Name 只允许注册时调用一次的快照：reload 归因必须使用注册表
// 名称，任何 reload 后的 s.Name() 调用都会 panic 暴露（spec §7.1.4）。
type nameOnceSnap struct {
	name  string
	calls atomic.Int64
}

func (n *nameOnceSnap) Name() string {
	if n.calls.Add(1) > 1 {
		panic("Name() called after registration")
	}
	return n.name
}
func (n *nameOnceSnap) Scopes() []string                 { return nil }
func (n *nameOnceSnap) Reload(ctx context.Context) error { panic("reload boom") }

// TestReloadPanicRegisteredNameAttribution 注册名归因：注册后再次调用 Name()
// 会 panic；reload 仍按注册表名称归因且不触发第二次 Name()。
func TestReloadPanicRegisteredNameAttribution(t *testing.T) {
	reg := New()
	n := &nameOnceSnap{name: "registered-name"}
	require.NoError(t, reg.Register(n))
	require.Equal(t, int64(1), n.calls.Load(), "Register 恰好调用一次 Name")

	errs := reg.ReloadAll(context.Background())
	require.Contains(t, errs, "registered-name", "按注册表名称归因")
	require.True(t, errors.Is(errs["registered-name"], ErrReloadPanic))
	require.Equal(t, int64(1), n.calls.Load(), "reload 路径不调用第二次 Name")

	st := statusByName(reg)
	require.True(t, errors.Is(st["registered-name"].LastError, ErrReloadPanic))
}

// TestReloadScopePanicIsolation scope 分发（spec §7.1.5）：只命中目标 scope，
// 未命中快照不调用；panic 快照与成功兄弟各自记录。
func TestReloadScopePanicIsolation(t *testing.T) {
	reg := New()
	boom := &fnSnap{name: "boom", scopes: []string{"x"}, reload: func(ctx context.Context) error {
		panic("scope-boom")
	}}
	ok := &fnSnap{name: "ok", scopes: []string{"x"}, reload: func(ctx context.Context) error { return nil }}
	other := &fnSnap{name: "other", scopes: []string{"y"}, reload: func(ctx context.Context) error { return nil }}
	for _, s := range []Snapshot{boom, ok, other} {
		require.NoError(t, reg.Register(s))
	}

	errs := reg.Reload(context.Background(), "x")
	require.Len(t, errs, 1)
	require.True(t, errors.Is(errs["boom"], ErrReloadPanic))
	require.Equal(t, int64(1), boom.count())
	require.Equal(t, int64(1), ok.count(), "panic 兄弟照常执行")
	require.Equal(t, int64(0), other.count(), "scope 未命中不调用")
}

// TestReloadPanicSelfHeal 自愈（spec §7.1.6）：首次 panic → 二次成功（Status
// 清除）→ 三次 panic 仍隔离；stack 一次性预算不因中间 success 重置。
func TestReloadPanicSelfHeal(t *testing.T) {
	reg := New()
	var calls atomic.Int64
	flap := &fnSnap{name: "flap", reload: func(ctx context.Context) error {
		if calls.Add(1) != 2 {
			panic("flap-boom")
		}
		return nil
	}}
	require.NoError(t, reg.Register(flap))

	errs := reg.ReloadAll(context.Background())
	require.True(t, errors.Is(errs["flap"], ErrReloadPanic))
	stack1, ok := PanicStackForLog(errs["flap"])
	require.True(t, ok, "首次 panic 捕获 stack")
	require.NotEmpty(t, stack1)

	require.Empty(t, reg.ReloadAll(context.Background()), "第二次成功：空 map")
	st := statusByName(reg)
	require.NoError(t, st["flap"].LastError, "成功清除 LastError")

	errs = reg.ReloadAll(context.Background())
	require.True(t, errors.Is(errs["flap"], ErrReloadPanic))
	_, ok = PanicStackForLog(errs["flap"])
	require.False(t, ok, "stack 一次性预算不因中间 success 重置")
}

// TestPanicStackBudgetOneShot 一次性 budget（spec §7.1.7）：首次 panic 有
// stack；中间普通 error 或连续 panic 都不能重新消耗 stack 预算（success 情形
// 见 TestReloadPanicSelfHeal）。
func TestPanicStackBudgetOneShot(t *testing.T) {
	t.Run("panicThenPanic", func(t *testing.T) {
		reg := New()
		p := &fnSnap{name: "p", reload: func(ctx context.Context) error { panic("repeat-boom") }}
		require.NoError(t, reg.Register(p))

		errs := reg.ReloadAll(context.Background())
		stack, ok := PanicStackForLog(errs["p"])
		require.True(t, ok, "首次 panic 有 stack")
		require.NotEmpty(t, stack)

		errs = reg.ReloadAll(context.Background())
		require.True(t, errors.Is(errs["p"], ErrReloadPanic), "后续 panic 仍隔离")
		_, ok = PanicStackForLog(errs["p"])
		require.False(t, ok, "后续 panic 无 stack")
	})

	t.Run("panicThenErrorThenPanic", func(t *testing.T) {
		reg := New()
		var calls atomic.Int64
		p := &fnSnap{name: "p", reload: func(ctx context.Context) error {
			switch calls.Add(1) {
			case 1, 3:
				panic("boom")
			default:
				return errors.New("plain-error")
			}
		}}
		require.NoError(t, reg.Register(p))

		errs := reg.ReloadAll(context.Background())
		_, ok := PanicStackForLog(errs["p"])
		require.True(t, ok, "首次 panic 有 stack")

		errs = reg.ReloadAll(context.Background())
		require.EqualError(t, errs["p"], "plain-error", "普通 error 行为不变")

		errs = reg.ReloadAll(context.Background())
		require.True(t, errors.Is(errs["p"], ErrReloadPanic))
		_, ok = PanicStackForLog(errs["p"])
		require.False(t, ok, "普通 error 不重置 stack 预算")
	})
}

// TestReloadAllSerializedAcrossTriggersWithPanic 触发串行（spec §7.1.8）：一批
// 阻塞（且含 panic）时第二次 ReloadAll 不进入；放行后依次完成，panic 不破坏
// execMu。无 time.Sleep——require.Never 作有界 watchdog。
func TestReloadAllSerializedAcrossTriggersWithPanic(t *testing.T) {
	reg := New()
	gate := make(chan struct{})
	var calls atomic.Int64
	a := &fnSnap{name: "a", reload: func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			<-gate // 阻塞第一批
			panic("a-boom")
		}
		return nil
	}}
	b := &fnSnap{name: "b", reload: func(ctx context.Context) error { panic("b-boom") }}
	require.NoError(t, reg.Register(a))
	require.NoError(t, reg.Register(b))

	done1 := make(chan map[string]error, 1)
	go func() { done1 <- reg.ReloadAll(context.Background()) }()
	require.Eventually(t, func() bool { return calls.Load() == 1 }, time.Second, time.Millisecond,
		"第一批已进入 a 的 Reload")

	done2 := make(chan map[string]error, 1)
	go func() { done2 <- reg.ReloadAll(context.Background()) }()
	require.Never(t, func() bool { return calls.Load() > 1 }, 50*time.Millisecond, time.Millisecond,
		"第二个触发在第一个完成前不进入 a（execMu 串行）")

	close(gate) // 放行第一批
	errs1 := <-done1
	require.True(t, errors.Is(errs1["a"], ErrReloadPanic))
	require.True(t, errors.Is(errs1["b"], ErrReloadPanic))

	errs2 := <-done2 // panic 不破坏 execMu：第二批正常完成
	require.NotContains(t, errs2, "a", "第二批 a 成功")
	require.True(t, errors.Is(errs2["b"], ErrReloadPanic))
	require.Equal(t, int64(2), calls.Load())
	require.Equal(t, int64(2), b.count())
}

// TestReloadPanicCancellationOrthogonal 取消正交（spec §7.1.9）：一个快照返回
// context.Canceled、另一个 panic——两者分别独立记录，互不吞并。
func TestReloadPanicCancellationOrthogonal(t *testing.T) {
	reg := New()
	canceled := &fnSnap{name: "canceled", reload: func(ctx context.Context) error {
		return context.Canceled
	}}
	boom := &fnSnap{name: "boom", reload: func(ctx context.Context) error { panic("cancel-boom") }}
	require.NoError(t, reg.Register(canceled))
	require.NoError(t, reg.Register(boom))

	errs := reg.ReloadAll(context.Background())
	require.ErrorIs(t, errs["canceled"], context.Canceled)
	require.True(t, errors.Is(errs["boom"], ErrReloadPanic))
	require.Len(t, errs, 2)
}

// TestReloadPanicNil Go 1.26 默认 panic(nil)（spec §7.1.10）：恢复为非 nil
// runtime panic 值，按普通 panic 隔离——只断言隔离、失败与 stack 非空，不断言
// runtime-specific value。
func TestReloadPanicNil(t *testing.T) {
	reg := New()
	pn := &fnSnap{name: "pn", reload: func(ctx context.Context) error {
		panic(nil) // Go 1.21+ 默认：recover 得到非 nil runtime panic 值
	}}
	require.NoError(t, reg.Register(pn))

	errs := reg.ReloadAll(context.Background())
	require.True(t, errors.Is(errs["pn"], ErrReloadPanic), "panic(nil) 被隔离为 reload 失败")
	stack, ok := PanicStackForLog(errs["pn"])
	require.True(t, ok)
	require.NotEmpty(t, stack, "首次 panic stack 非空")

	st := statusByName(reg)
	require.True(t, errors.Is(st["pn"].LastError, ErrReloadPanic))
}

// TestSanitizePanicStack sanitizer（spec §7.1.11）：合成 stack 覆盖 Windows/
// Unix 路径、空格、反斜杠、控制字符与超长内容——验证 basename（/ 与 \）、
// 控制字符替换（\n/\t 保留）与最终字节上限。
func TestSanitizePanicStack(t *testing.T) {
	require.Empty(t, sanitizePanicStack(""), "空输入原样返回")

	in := "goroutine 7 [running]:\n" +
		"example.com/mod@v1.2.3/dep.(*Thing).do(0xc0001)\n" +
		"\tC:/Users/i/project/go-proxy-mini/internal/snapshot/snapshot.go:99 +0x1a\n" +
		"\t/home/ci/project/internal/deep/file name.go:12 +0x5\n" +
		"\tC:\\Users\\i\\proj\\win path.go:7 +0x3\n" +
		"keep\ttab and\x01\x02ctl\x7fhere\n"
	out := sanitizePanicStack(in)
	// 路径全部 basename（/ 与 \ 同支持）：任何路径分隔符不残留
	require.NotContains(t, out, "C:/Users")
	require.NotContains(t, out, "C:\\Users")
	require.NotContains(t, out, "/home/ci")
	require.NotContains(t, out, "go-proxy-mini")
	require.NotContains(t, out, "/")
	require.NotContains(t, out, "\\")
	require.Contains(t, out, "snapshot.go:99 +0x1a", "Unix 路径 basename")
	require.Contains(t, out, "file name.go:12 +0x5", "空格保留")
	require.Contains(t, out, "win path.go:7 +0x3", "Windows 反斜杠路径 basename")
	// 控制字符替换（\n/\t 除外）
	require.NotContains(t, out, "\x01")
	require.NotContains(t, out, "\x02")
	require.NotContains(t, out, "\x7f")
	require.Contains(t, out, "keep\ttab and??ctl?here", "tab 保留、其余控制字符 → ?")
	require.Contains(t, out, "\n", "换行保留")

	// 超长内容：最终字节上限生效（basename 收缩后仍超限时按字节截断）。
	long := strings.Repeat(strings.Repeat("x", 100)+"/"+strings.Repeat("y", 200)+"\n", 400)
	got := sanitizePanicStack(long)
	require.Equal(t, panicStackMaxBytes, len(got), "最终输出恰为字节上限")
	require.LessOrEqual(t, len(got), panicStackMaxBytes)
}
