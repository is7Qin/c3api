// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 线程状态的**唯一**验收面（T3 从 scheduler 搬来）。
//
// 为什么必须在这里：`Turns` / `WindowN` / `NextWindowAt` / `WMax` 是 SDK 的协议状态，
// 宿主看不到也不该看到。改造前这些断言写在 scheduler 的用例里（它们读 session.st），
// 那是"测试把边界凿穿"的典型——搬进来之后，宿主的用例只能观测两件事：线程键变没变、
// 退休键交没交给池。这两件事恰好就是它们真正要保护的行为。

func testThreads(p RotatePolicy) *Threads { return NewThreads(p) }

// TestThreadOpenFieldsIdentity 新线程的四元组：session == thread（根线程语义）、
// 窗口从 0 起、first 由 payload 判定、不带 turn-state。
func TestThreadOpenFieldsIdentity(t *testing.T) {
	th := testThreads(RotatePolicy{})
	h := th.Open("inst-a")
	require.NotEmpty(t, th.Key(h))

	payload := []byte(`{"model":"m","input":[{"role":"user","content":"hi"}]}`)
	id := h.identity(payload)
	require.Equal(t, "inst-a", id.InstallationID, "installation 取会话（账号级稳定项）")
	require.Equal(t, id.Session.ThreadID, id.Session.SessionID, "session == thread")
	require.NotEmpty(t, id.Session.ThreadID)
	require.Equal(t, id.Session.ThreadID+":0", id.Session.WindowID, "windowID = thread:0")
	require.Equal(t, uint64(0), id.Window.Number)
	require.NotEmpty(t, id.Window.ContextID)
	require.True(t, id.Turn.First, "首轮请求")
	require.Empty(t, id.Turn.State, "槽位路径不带 turn-state")
	require.Nil(t, id.Sink, "槽位路径不捕获 turn-state")

	// 同一线程、同一 payload ⇒ 除 Turn.First 外完全一致（字段拷贝，零重算）。
	require.Equal(t, !h.identity([]byte(`{"model":"m","input":[{"role":"user","content":"hi"},{"role":"assistant","content":"a"},{"role":"user","content":"next"}]}`)).Turn.First,
		false, "续轮不是 first")
}

// TestThreadStepAdvancesWindowWithoutRetire 窗口内 step 不换线程；跨窗口换 windowID
// 但线程键不变；达到 WMax 才退休并换线程。
func TestThreadStepAdvancesWindowWithoutRetire(t *testing.T) {
	// WMax=2：每线程活 2 个窗口。windowSpan 由 (threadID, windowIndex) 确定性派生，
	// 所以"多少次 step 跨一个窗口"可算但不必写死——用"窗口号是否变"驱动循环。
	th := testThreads(RotatePolicy{WMaxLo: 2, WMaxHi: 2})
	h := th.Open("inst-b")
	key0 := th.Key(h)

	window0 := h.identity(nil).Session.WindowID
	seenWindowAdvance := false
	retired := ""
	for i := 0; i < 200 && retired == ""; i++ {
		retired = th.Step(h)
		if !seenWindowAdvance && th.Key(h) == key0 && h.identity(nil).Session.WindowID != window0 {
			seenWindowAdvance = true
		}
	}
	require.True(t, seenWindowAdvance, "窗口推进必须换 windowID 但不换线程")
	require.NotEmpty(t, retired, "WMax=2 ⇒ 200 次 step 内必然退休")
	require.Equal(t, key0, retired, "退休返回的是**旧**线程键")
	require.NotEqual(t, key0, th.Key(h), "退休后线程键换新")
	require.Equal(t, h.identity(nil).Session.ThreadID+":0", h.identity(nil).Session.WindowID,
		"退休后窗口从 0 起")
	require.True(t, h.identity(nil).Turn.First, "新线程上仍然是首轮标记（由 payload 决定，与线程无关）")
}

// TestThreadNoRetireWhenWMaxZero WMaxHi=0 ⇒ 永不退休（线程不换），step 只推进窗口。
func TestThreadNoRetireWhenWMaxZero(t *testing.T) {
	th := testThreads(RotatePolicy{})
	h := th.Open("inst-c")
	key := th.Key(h)
	for i := 0; i < 500; i++ {
		require.Empty(t, th.Step(h), "WMaxHi=0 时不得退休")
	}
	require.Equal(t, key, th.Key(h))
}

// TestThreadHandleZeroValueIsAnonymous 零值句柄 = 匿名身份（无会话的 codex 请求）。
func TestThreadHandleZeroValueIsAnonymous(t *testing.T) {
	var h ThreadHandle
	require.Zero(t, h.identity(nil))
	require.Empty(t, testThreads(RotatePolicy{}).Key(h))
	require.Empty(t, testThreads(RotatePolicy{}).Step(h), "零值句柄 step 是 no-op")
}

// TestThreadsPolicyRoundTrip 策略是值类型：相等比较即"策略没变"（resize 判据）。
//
// 本包**不提供默认值**（P5-2b）：策略的所有者是会话池，默认值随所有权一起搬走，
// 曾在这里的那条默认值断言也随之搬到 identitypool——默认值只有一处可读，本文件
// 不再替它作证。
func TestThreadsPolicyRoundTrip(t *testing.T) {
	p := RotatePolicy{WMaxLo: 3, WMaxHi: 9}
	require.Equal(t, p, NewThreads(p).Policy())
	require.NotEqual(t, p, NewThreads(RotatePolicy{WMaxLo: 3, WMaxHi: 8}).Policy())
}
