// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
package scheduler

import (
	"time"

	"github.com/is7qin/c3api/internal/domain"
)

// 路由质量观测窗口常量（spec §5.3 命名）——**同源重导出**：唯一字面量定义在
// domain（domain/routing_window.go 记录了理由：scheduler 是 repository 的下游，
// repository 无法反向 import scheduler，故两个包共用的常量必须落在更叶子的包）。
// 本包内的消费点（provider 活体截断、quality_stats 内存累计）用本名；repository
// 的落库窗读直接用 domain 同名常量，两者是同一个值。
const (
	// BaselineLookback 基线回看下界（事故判定与基线窗共用 24h）。
	BaselineLookback time.Duration = domain.BaselineLookback
	// CurrentWindowLen 当前窗长度（[M-CurrentWindowLen, M)）。
	CurrentWindowLen time.Duration = domain.CurrentWindowLen
)
