// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

//go:build race

// Package racetest 暴露「当前二进制是否带 -race 插桩」这一个事实，给性能判据用。
//
// 为什么需要它：`-race` 会给部分分配加上影子元数据，于是**分配字节数**（B/op）与
// 计时（ns/op）在 race 下必然变化；而**分配次数**（allocs/op）不变。任何"精确
// 字节数/耗时"的断言因此在 race 下是假红，除非显式区分模式。
//
// 纪律（P5b r2）：精确 allocs/B/op 与 ns 判据必须写成
//
//	if racetest.Enabled {
//	    // race 插桩改变分配计数与耗时 ⇒ 只留功能断言，数值只 log
//	} else {
//	    require.Equal(t, want, got)  // 非 race 模式原判据照旧
//	}
//
// 两份文件靠 build tag 二选一（`race_on.go` / `race_off.go`），不在每个包里各写一套
// `//go:build race` 断言——否则"哪些判据区分模式"就散落在几十处、无人能对账。
package racetest

// Enabled 报告当前测试二进制是否由 `-race` 构建。
const Enabled = true
