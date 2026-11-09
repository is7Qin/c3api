// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"sync/atomic"

	codexsdk "github.com/is7Qin/codex-sdk"
)

// sessionKeyOf 会话键——turn-state 槽的作用域键（G3）：`installation|session|thread`。
//
// 为什么不是账号级：同一个账号上并存多个 thread 时，账号级单值会被 A 会话的签发值
// 覆盖，B 会话下一次请求就把别人的 ts 当自己的带上（跨会话串台）。取这三段是因为
// 三者与 identityKey 同源且**不随槽窗口推进而变**：windowID 每次换槽都换，把它算进
// 键里会让 ts 在换槽瞬间丢失（同一个 thread 的轮内续传因此断掉）。nil sess/meta ⇒
// 空段 ⇒ 键 "||"（无会话标识时退化为一条共享槽，见 spec §7-2）。
func sessionKeyOf(sess *codexsdk.Session, meta *codexsdk.CodexMeta) string {
	var installation, session, thread string
	if meta != nil {
		installation, session, thread = meta.InstallationID, meta.SessionID, meta.ThreadID
	}
	return installation + "|" + session + "|" + thread
}

// turnStateSlots 会话级 turn-state 槽表（G3）：键 = sessionKey，值 = 该会话当前
// 生效的 ts。
//
// 读路径是**每请求**的（SDK 的 WithTurnStateProvider 在热路径上调它），所以不能
// 用互斥锁——这里是一份写时复制的只读快照：
//   - 读：一次 atomic.Load + 一次 map 查，零锁；
//   - 写：拷一份新 map 再 CAS 换指针（写只在握手/响应头/事件/轮结束这几个低频点
//     发生，拷贝成本可忽略）。CAS 失败即有人并发写过，重读重试——最终一致，不会
//     丢写（谁最后成功谁的值生效，与"最后签发值胜出"的语义一致）。
//
// 零值可用（原子指针 nil ⇒ 空表）。
type turnStateSlots struct {
	m atomic.Pointer[map[string]string]
}

// load 读该会话当前 ts（空串 = 本会话尚未签发/已清除）。
func (s *turnStateSlots) load(key string) string {
	if m := s.m.Load(); m != nil {
		return (*m)[key]
	}
	return ""
}

// store 写该会话当前 ts（空串 = 清除该键）。
func (s *turnStateSlots) store(key, ts string) {
	for {
		old := s.m.Load()
		cur := map[string]string(nil)
		if old != nil {
			cur = *old
		}
		if cur[key] == ts {
			return // 幂等：同值不复制（响应未再签发时重复回写走这一条）
		}
		next := make(map[string]string, len(cur)+1)
		for k, v := range cur {
			next[k] = v
		}
		if ts == "" {
			delete(next, key)
		} else {
			next[key] = ts
		}
		if s.m.CompareAndSwap(old, &next) {
			return
		}
	}
}

// closeWSIdleConns 关闭一批客户端各自的 **WS** 空闲连接（G4）。
//
// 调用纪律：**锁外**调用。SDK 的 clear() 会逐条 terminate，可能在写闸上等待；
// 在 a.mu 内调用会把整个 client 表卡住（spec §3.1-S4）。
//
// 只用 CloseWSIdleConnections 而不是 CloseIdleConnections：后者会对
// `client.Transport.(*http.Transport)` 断言，然后清空其空闲池——网关出口是
// `*httpx.ShardedTransport`（断言失败 ⇒ 静默 no-op，丢了 client 却关不掉连接），
// 而一旦哪天出口是裸 `*http.Transport` 又会打掉全站 h2 连接池（spec §7-5）。
func closeWSIdleConns(clients []*codexsdk.HTTPClient) {
	for _, c := range clients {
		if c != nil {
			c.CloseWSIdleConnections()
		}
	}
}
