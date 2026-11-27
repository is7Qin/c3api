// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package supplier 供应商收益记账链 / 解冻链的纯逻辑与 worker 契约
// （spec 2026-10-09 §5）。持久 SQL 由 repository 承载（绑定真实 PG）；本包保持
// 无 DB 依赖，纯函数可单测（冻结参数、桶时刻、聚合法定/守恒、重试分类）。
package supplier

import "github.com/is7qin/c3api/internal/domain"

// freeze.go 冻结参数与桶时刻的**别名**：canonical 实现已下沉 internal/domain
// 的 supplier_rules.go（持久化实现与 worker 编排都要用这些纯函数，
// 而 repository 不该反向依赖 worker 编排包）。此处保留别名，既有调用点零改动。

// EffectiveFreezeHours 生效冻结小时（全局开关优先）。
var EffectiveFreezeHours = domain.EffectiveFreezeHours

// BucketAvailableAt 桶时刻（写时算定）。
var BucketAvailableAt = domain.BucketAvailableAt

// MaxActiveBuckets 活跃桶数紧上界。
var MaxActiveBuckets = domain.MaxActiveBuckets
