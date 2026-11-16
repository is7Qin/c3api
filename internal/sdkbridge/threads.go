// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import codexsdk "github.com/is7Qin/codex-sdk"

// 线程身份（伪装四元组的来源）的**唯一持有者**。
//
// 判据：`Turns` / `WindowN` / `NextWindowAt` / `WMax` 这些量怎么变、什么时候换线程，
// 是 SDK 的 `IdentityState` 与 `Step` 定义的协议事实；网关不复制任何一条算法，也不
// 读它的字段——宿主只搬运一个**不透明句柄**，并在调用时把它交回来。
//
// 为什么不是值类型快照：`InstallationID / Session / Window / Turn` 是 `WithConversation`
// 与握手 routing-hint 的 **wire 输入**。把它们摊成宿主字段，宿主就有能力自己拼一个
// "看起来合法"的身份，而这正是这条边界要防的事。所以对外只有：
//   - ThreadHandle：拿不到任何字段（零值 = 匿名身份，等价 SDK 的零值 Identity）
//   - Threads.Step：唯一的状态转移入口
//   - Threads.Key：连接复用的线程键（宿主只当字符串用，不解析）

// RotatePolicy 轮换策略：每线程最多活几个窗口（开线程时在 [WMaxLo, WMaxHi] 内均匀抽；
// WMaxHi == 0 表示不退休）。
//
// 这是**部署策略**而不是 wire 语义：宿主有权选"线程活多久"，窗口跨度怎么算仍然在 SDK。
// 值类型可直接比较——resize 靠它判断"要不要换新会话"。
type RotatePolicy struct {
	WMaxLo uint64
	WMaxHi uint64
}

// DefaultRotatePolicy 默认策略（每线程 16–48 个窗口，与 SDK 默认一致）。
func DefaultRotatePolicy() RotatePolicy { return RotatePolicy{WMaxLo: 16, WMaxHi: 48} }

// toSDK 归一成 SDK 策略。唯一转换点。
func (p RotatePolicy) toSDK() codexsdk.RotatePolicy {
	return codexsdk.RotatePolicy{WMaxLo: p.WMaxLo, WMaxHi: p.WMaxHi}
}

// threadState 一条被租借的线程的全部协议状态。字段不导出：只有本包能读。
type threadState struct {
	st       codexsdk.IdentityState
	windowID string // 预计算（= st.WindowID()），避免每请求重算
	key      string // 预计算（= installation|thread|thread），连接复用键
}

// ThreadHandle 线程句柄（值语义、可拷贝、不透明）。零值 = 匿名身份。
type ThreadHandle struct{ s *threadState }

// Threads 线程工厂与状态转移的唯一入口（无状态；策略是构造期参数）。
type Threads struct{ policy RotatePolicy }

// NewThreads 按策略构造工厂。所有账号的池共用同一个实例（策略是池的不可变构造参数）。
func NewThreads(p RotatePolicy) *Threads { return &Threads{policy: p} }

// Policy 本工厂的策略（值比较即可判断"策略是否变了"）。
func (t *Threads) Policy() RotatePolicy { return t.policy }

// Open 用账号级 installation 开一条新线程（新线程 UUIDv7、WMax 按策略抽样、
// turns/window 归零），并预计算窗口 ID 与线程键。
func (t *Threads) Open(installationID string) ThreadHandle {
	st := codexsdk.NewIdentityState(installationID, t.policy.toSDK())
	return ThreadHandle{s: &threadState{
		st:       st,
		windowID: st.WindowID(),
		key:      threadKeyOf(installationID, st.ThreadID),
	}}
}

// Step 推进一次**完成轮**——唯一的状态转移入口。返回被退休的旧线程键（"" = 未退休）。
//
// 调用纪律由会话池保证：只在成功取到 usage、且上游调用已归还之后调用一次；
// 中途超时/断流的请求不得推进（否则窗口计数漂移）。
func (t *Threads) Step(h ThreadHandle) string {
	if h.s == nil {
		return ""
	}
	prev := h.s.st
	h.s.st = codexsdk.Step(prev, t.policy.toSDK())
	switch {
	case h.s.st.ThreadID != prev.ThreadID:
		old := h.s.key
		h.s.windowID = h.s.st.WindowID()
		h.s.key = threadKeyOf(h.s.st.InstallationID, h.s.st.ThreadID)
		return old
	case h.s.st.WindowN != prev.WindowN:
		h.s.windowID = h.s.st.WindowID()
	}
	return ""
}

// Key 连接复用键（installation|thread|thread，与 SDK 同构）。宿主只当不透明字符串用：
// 它进 ClientTable 的 idle 索引，解析它没有意义，拼它更是错的。
func (t *Threads) Key(h ThreadHandle) string {
	if h.s == nil {
		return ""
	}
	return h.s.key
}

// identity 把句柄组装成一次调用的身份。payload 决定 `Turn.First`——必须是**本次真正
// 发给上游的那一份字节**（与 SDK 用同一份，所以两处判定永远一致）。
func (h ThreadHandle) identity(payload []byte) codexsdk.Identity {
	if h.s == nil {
		return codexsdk.Identity{}
	}
	return codexsdk.Identity{
		InstallationID: h.s.st.InstallationID,
		Session: codexsdk.Session{
			SessionID: h.s.st.ThreadID,
			ThreadID:  h.s.st.ThreadID,
			WindowID:  h.s.windowID,
		},
		Window: codexsdk.Window{Number: h.s.st.WindowN, ContextID: h.s.st.ContextWindowID},
		Turn:   codexsdk.Turn{First: !codexsdk.AnalyzeConversationInput(payload).FollowUp()},
	}
}

// threadKeyOf 连接复用键，与 SDK 同构：installation|session|thread。槽位路径的
// session == thread（根线程语义），所以是 installation|thread|thread。
func threadKeyOf(installationID, threadID string) string {
	return installationID + "|" + threadID + "|" + threadID
}
