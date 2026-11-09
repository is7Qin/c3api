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

// turnStateSlotCompactAt 是外层键表的压缩阈值：插入新键时若表已超过它，顺手丢掉
// 值为空的条目（= 已结束的轮留下的壳）。阈值以下不压缩，避免每轮结束都复制整表。
const turnStateSlotCompactAt = 4096

// turnStateSlots 会话级 turn-state 槽表（G3）：键 = sessionKey，值 = 该会话当前
// 生效的 ts。
//
// 读路径是**每请求**的（SDK 的 WithTurnStateProvider 在热路径上调它），所以不能
// 用互斥锁；写路径（握手/响应头/事件/轮结束）也不能是"复制整张表"——那样轮结束
// 的清除与每个响应头都会变成 O(会话数)，键数一多就是持续的写放大（实测 keys=10k
// 时整表复制 ~0.47ms/次、100k 时 ~6.4ms/次，分配量同阶）。
//
// 结构是两层：
//   - 外层 map：键 → 值单元，只在**新增键**时复制（每个新 thread 一次，低频）；
//   - 值单元：*atomic.Pointer[string]，更新/清除都是 O(1) 原子写，不复制表。
//
// 清除（ts = ""）保留值单元（把值置空），因为同一会话下一轮会复用这个单元；残留的
// 空单元由插入新键时的压缩回收（阈值 turnStateSlotCompactAt）。
//
// 零值可用（原子指针 nil ⇒ 空表）。读 = 一次 atomic.Load + 一次 map 查 + 一次原子
// 读，全程无锁、无分配。
type turnStateSlots struct {
	m atomic.Pointer[map[string]*atomic.Pointer[string]]
}

// load 读该会话当前 ts（空串 = 本会话尚未签发/已清除）。
func (s *turnStateSlots) load(key string) string {
	m := s.m.Load()
	if m == nil {
		return ""
	}
	cell := (*m)[key]
	if cell == nil {
		return ""
	}
	if v := cell.Load(); v != nil {
		return *v
	}
	return ""
}

// slotEmpty 报告一个值单元是否为"空"：从未写过（nil 指针）或写的是空串。
//
// 清除**保留值单元**（把值置空），所以"空壳"有两种形态，压缩与幂等判定都必须
// 同时认这两种——只看 nil 指针会把显式清空的单元当活值留着。
func slotEmpty(cell *atomic.Pointer[string]) bool {
	v := cell.Load()
	return v == nil || *v == ""
}

// store 写该会话当前 ts（空串 = 清除该键的值，保留值单元）。
//
// 并发语义：同键的并发写是"最后成功的那次原子写胜出"（与"最后签发值胜出"一致）；
// 不同键互不影响。外层表的 CAS 只用于**新增键**——失败即有人并发插入，重读重试。
// 压缩可能把本 goroutine 刚写过值的空壳摘掉，所以写完值后要复核一次自己在不在表里
// （不在就重试，重新插入）——不会丢写。
func (s *turnStateSlots) store(key, ts string) {
	for {
		old := s.m.Load()
		var cur map[string]*atomic.Pointer[string]
		if old != nil {
			cur = *old
		}
		if cell := cur[key]; cell != nil {
			if ts == "" && slotEmpty(cell) {
				return // 已经是空值：幂等（未签发/已清除时重复回写走这一条）
			}
			cell.Store(&ts)
			// 复核：并发压缩可能已把该单元从表里摘掉（摘的是空壳；而我们在摘除点
			// 之后又写了值）——那样这次写就落在了孤儿上，重试一次会重新插入。
			if now := s.m.Load(); now != nil && (*now)[key] == cell {
				return
			}
			continue
		}
		if ts == "" {
			return // 键不存在 + 值为空 = no-op
		}
		next := make(map[string]*atomic.Pointer[string], len(cur)+1)
		for k, v := range cur {
			next[k] = v
		}
		cell := new(atomic.Pointer[string])
		cell.Store(&ts)
		next[key] = cell
		if len(next) > turnStateSlotCompactAt {
			for k, v := range next {
				if slotEmpty(v) {
					delete(next, k) // 已结束的轮留下的空壳（含被显式清空的值单元）
				}
			}
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
