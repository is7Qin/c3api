// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package invalidate

import "strings"

// /ops/workers 可观测面（spec 2026-08-11）：独立 Stats 契约不改 worker.Worker
// 接口——装配侧类型断言聚合（main.go），响应 typed struct 非 map。
// 采集纪律：atomic.Pointer Load 零锁；State 发布后不可变（mark 每次新建），
// 读端无需锁。

// DebouncerStats 去抖器脏状态（当前是否有待执行的合并重载）。
type DebouncerStats struct {
	Dirty       bool   `json:"dirty"`        // 是否有待执行的重载
	DirtyKinds  string `json:"dirty_kinds"`  // 脏位名（逗号分隔；空 = 纯组级定向）
	DirtyGroups int    `json:"dirty_groups"` // 组级定向账号数
	WindowMs    int64  `json:"window_ms"`    // 去抖窗口
}

// Stats 满足 handler.StatsProvider（独立于 worker.Worker 契约；装配链路见 internal/handler/ops.go 文件头）。
func (d *Debouncer) Stats() any {
	var dirty bool
	var kinds string
	var groups int
	if st := d.state.Load(); st != nil {
		dirty = true
		kinds = kindsNames(st.Kinds)
		groups = len(st.Groups)
	}
	return DebouncerStats{
		Dirty: dirty, DirtyKinds: kinds, DirtyGroups: groups,
		WindowMs: d.cfg.Window.Milliseconds(),
	}
}

// kindsNames 脏位名逗号拼接（观测可读性；Kinds 为 bitmask）。
func kindsNames(k Kind) string {
	names := make([]string, 0, 6)
	for _, b := range []struct {
		bit  Kind
		name string
	}{
		{KindUsers, "users"},
		{KindTemplates, "templates"},
		{KindClients, "clients"},
		{KindMultipliers, "multipliers"},
		{KindKeys, "keys"},
		{KindRules, "rules"},
	} {
		if k&b.bit != 0 {
			names = append(names, b.name)
		}
	}
	return strings.Join(names, ",")
}
