// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"sync"
	"sync/atomic"

	"github.com/is7qin/c3api/internal/domain"
)

// 丢弃计数（spec §7 I3）：转换把"目标协议无载体"的字段丢掉时必须可见，
// 否则线上只会表现为"模型好像没看到那段内容"，无从排查。
//
// 一次丢弃是**三元组**：转换方向 + 协议面 + 字段。面不能省：`domain.ProtocolConvert`
// 表达的是协议对（`mess_to_chat` 既是「客户端 mess 请求 → chat 请求」，也是
// 「chat 响应 → mess 响应」），请求面与响应面共用同一个值。少了面这一维，
// "mess_to_chat 丢了 12 次 document" 就分不清是客户端传的文档被丢、还是上游
// 返回的文档被丢；补救办法只剩"两面原因字符串永不同名"这种命名纪律，而那会把
// 内部实现约束泄漏到对外可见的字段名上。
//
// 面的归属由**调用点所在文件的职责**决定：请求转换文件（mess_chat_req.go /
// resp_chat_req.go）调 noteReqDrop，响应转换文件（chat_stream.go /
// chat_upstream_resp.go）调 noteRespDrop。两个入口同形，编译器只能强制"必须
// 二选一"、拦不住选错面 —— 真正的守卫是文件归属 + 落点测试
// （dropsink_test.go 的 `|req|` / usageclamp_test.go 的 `|resp|` 断言）。
//
// 设计对齐 toolschema.go 的观测口径（SetSchemaSink/observeSchema）：包级计数 +
// 可选 sink。差别是计数器键用**可比较的事件值**而不是拼接字符串：转换是逐块
// 调用的热路径，`string + "|" + string` 每次都要分配一段临时字节。
//
// 使用约定：
//   - 计数键在 DropCounts 里才拼成 "<方向>|<面>|<字段>"（冷路径）；
//   - sink 的告警由调用方（cmd/server）按事件去重，**每个三元组只 WARN 一次**；
//   - 测试用 ResetDropCounts 复位（包级状态，测试必须串行独占，禁 t.Parallel）。
const dropKeySep = "|"

// DropFace 丢弃发生在转换的哪一面。
type DropFace uint8

const (
	// DropFaceReq 请求面：客户端请求 → 上游请求。
	DropFaceReq DropFace = iota + 1
	// DropFaceResp 响应面：上游响应 → 客户端响应。
	DropFaceResp
)

func (f DropFace) String() string {
	if f == DropFaceResp {
		return "resp"
	}
	return "req"
}

// DropEvent 一次丢弃事件。值类型且各字段可比较 ⇒ 可直接作 map 键，零分配。
type DropEvent struct {
	Dir   domain.ProtocolConvert
	Face  DropFace
	Field string
}

// dropSink 观测出口（nil = 关闭）。
var dropSink atomic.Pointer[func(DropEvent)]

// SetDropSink 安装丢弃观测出口（传 nil 关闭）。由 cmd/server 在启动时调用一次。
func SetDropSink(f func(DropEvent)) {
	if f == nil {
		dropSink.Store(nil)
		return
	}
	dropSink.Store(&f)
}

// dropCounters 全局计数（DropEvent → *atomic.Int64）。
var dropCounters sync.Map

// noteReqDrop 记一次**请求面**丢弃（客户端请求 → 上游请求的转换）。
func noteReqDrop(dir domain.ProtocolConvert, field string) {
	noteDropAt(DropEvent{Dir: dir, Face: DropFaceReq, Field: field})
}

// noteRespDrop 记一次**响应面**丢弃（上游响应 → 客户端响应的转换）。
func noteRespDrop(dir domain.ProtocolConvert, field string) {
	noteDropAt(DropEvent{Dir: dir, Face: DropFaceResp, Field: field})
}

// noteDropAt 记一次丢弃。热路径调用：命中已有键时只做一次 map 读 + 原子加
// （**零分配**——所以先 Load 再 LoadOrStore：写成 LoadOrStore(k, new(...)) 会让
// 每次调用都多分配一个立刻被丢弃的计数器，基准门禁按 allocs/op 卡死）。
//
// 首次命中必须用 LoadOrStore：旧写法 `Load` miss → `new` → `Store`，两个协程
// 同时首次命中同一个键时会互相覆盖（各自从 1 起算），计数被抹掉。
// 竞争失败方的那个 *atomic.Int64 成为垃圾，但每键最多浪费一次。
func noteDropAt(ev DropEvent) {
	if v, ok := dropCounters.Load(ev); ok {
		v.(*atomic.Int64).Add(1)
	} else {
		v, _ := dropCounters.LoadOrStore(ev, new(atomic.Int64))
		v.(*atomic.Int64).Add(1)
	}
	if f := dropSink.Load(); f != nil {
		(*f)(ev)
	}
}

// DropCounts 快照（key = "<方向>|<面>|<字段>"，如 "mess_to_chat|req|document"）。
// 测试与运维读数用；字符串只在这里拼，且只在被读时拼。
func DropCounts() map[string]int64 {
	out := make(map[string]int64)
	dropCounters.Range(func(k, v any) bool {
		ev := k.(DropEvent)
		out[string(ev.Dir)+dropKeySep+ev.Face.String()+dropKeySep+ev.Field] = v.(*atomic.Int64).Load()
		return true
	})
	return out
}

// ResetDropCounts 清空计数（测试用；会影响并发中的计数，测试须串行独占）。
func ResetDropCounts() {
	dropCounters.Range(func(k, _ any) bool {
		dropCounters.Delete(k)
		return true
	})
}
