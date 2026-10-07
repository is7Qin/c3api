// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package supplier 供应商收益记账链 / 解冻链的纯逻辑与 worker 契约
// （spec 2026-10-09 §5）。持久 SQL 由 repository 承载（绑定真实 PG）；本包保持
// 无 DB 依赖，纯函数可单测（冻结参数、桶时刻、聚合法定/守恒、重试分类）。
package supplier

import "time"

// EffectiveFreezeHours 生效冻结小时（全局开关优先，§2.4）：
//
//	生效 = freeze_enabled ? COALESCE(row.freeze_hours, globalDefault) : 0
//
// rowOverride = nil ⇒ 继承 globalDefault；= &0 ⇒ 不冻结；> 0 ⇒ 覆盖。
func EffectiveFreezeHours(freezeEnabled bool, rowOverride *int, globalDefault int) int {
	if !freezeEnabled {
		return 0
	}
	if rowOverride != nil {
		return *rowOverride
	}
	return globalDefault
}

// BucketAvailableAt 桶时刻（写时算定，纯整数，单位统一到秒；§2.4）：
//
//	available_at = ceil_to(credited, g) + H×3600
//	ceil_to(t, g) = (t + g − 1) / g × g   （整数运算；不用 time.Truncate）
//
// credited 取 DB clock_timestamp()（M1：不由各实例时钟决定）。g<=0 ⇒ 兜底 1s。
func BucketAvailableAt(credited time.Time, g time.Duration, freezeHours int) time.Time {
	gs := int64(g / time.Second)
	if gs <= 0 {
		gs = 1
	}
	t := credited.Unix()
	ceil := ((t + gs - 1) / gs) * gs
	return time.Unix(ceil+int64(freezeHours)*3600, 0).UTC()
}

// MaxActiveBuckets 数学紧上界 N(x) = ceil(x/g) − floor((x−H)/g) 的闭式上界
// （§2.4/G7 第一层）：`g ∤ H` 与 `g | H` 均为 `ceil(H/g)+1`。默认 H=24,g=2h ⇒ 13。
// 物理行数（G7 第二层）停摆/多代可超此界，不承诺无条件容量上界。
func MaxActiveBuckets(g time.Duration, freezeHours int) int64 {
	gs := int64(g / time.Second)
	if gs <= 0 {
		gs = 1
	}
	h := int64(freezeHours) * 3600
	if h < 0 {
		h = 0
	}
	return (h+gs-1)/gs + 1
}
