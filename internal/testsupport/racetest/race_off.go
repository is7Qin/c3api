// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

//go:build !race

// Package racetest 暴露「当前二进制是否带 -race 插桩」这一个事实，给性能判据用。
// 本文件是**非** race 构建的那一半（`race_on.go` 是另一半）；两者只差一个常量，
// 见 race_on.go 的包注释与纪律。
package racetest

// Enabled 报告当前测试二进制是否由 `-race` 构建。
const Enabled = false
