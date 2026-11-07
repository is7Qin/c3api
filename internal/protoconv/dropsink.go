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
// 设计对齐 toolschema 的观测口径（toolschema.go 的 SetSchemaSink/observeSchema）：
// 包级计数 + 可选 sink。与那一处的差别是**计数器键用结构体而不是拼接字符串**：
// 转换是逐块调用的热路径，`string + "|" + string` 每次都要分配一段临时字节，
// 而 dropKey{dir, field} 是可比较的值类型，sync.Map 直接用它当键，零分配。
//
// 使用约定：
//   - 计数键在 DropCounts 里才拼成 "<方向>|<字段>"（冷路径）；
//   - sink 的告警由调用方（cmd/server）按 (方向, 字段) 去重，**每对只 WARN 一次**；
//   - 测试用 ResetDropCounts 复位（包级状态，测试必须串行独占，禁 t.Parallel）。
const dropKeySep = "|"

// dropKey 计数键：方向 + 字段。值类型、可比较 ⇒ 可直接作 map 键，无分配。
type dropKey struct {
	dir   domain.ProtocolConvert
	field string
}

// dropSink 观测出口（nil = 关闭）。第一个参数是方向，第二个是字段名。
var dropSink atomic.Pointer[func(domain.ProtocolConvert, string)]

// SetDropSink 安装丢弃观测出口（传 nil 关闭）。由 cmd/server 在启动时调用一次。
func SetDropSink(f func(domain.ProtocolConvert, string)) {
	if f == nil {
		dropSink.Store(nil)
		return
	}
	dropSink.Store(&f)
}

// dropCounters 全局计数（dropKey → *atomic.Int64）。
var dropCounters sync.Map

// noteDrop 记一次丢弃。热路径调用：命中已有键时只做一次 map 读 + 原子加。
func noteDrop(dir domain.ProtocolConvert, field string) {
	k := dropKey{dir: dir, field: field}
	if v, ok := dropCounters.Load(k); ok {
		v.(*atomic.Int64).Add(1)
	} else {
		n := new(atomic.Int64)
		n.Store(1)
		dropCounters.Store(k, n)
	}
	if f := dropSink.Load(); f != nil {
		(*f)(dir, field)
	}
}

// DropCounts 快照（key = "<方向>|<字段>"，如 "mess_to_chat|document"）。
// 测试与运维读数用；字符串只在这里拼，且只在被读时拼。
func DropCounts() map[string]int64 {
	out := make(map[string]int64)
	dropCounters.Range(func(k, v any) bool {
		dk := k.(dropKey)
		out[string(dk.dir)+dropKeySep+dk.field] = v.(*atomic.Int64).Load()
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
