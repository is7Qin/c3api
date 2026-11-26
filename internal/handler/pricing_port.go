// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"

	"github.com/is7qin/c3api/internal/pricingresult"
)

// PricingSync 价格手动同步/预览的两个编排动作——**本包声明的消费方接口**。
//
// 只用两个方法。`*pricing.SyncWorker` 的其余方法（Name/Start/Close/Sync/Stats）
// 是 worker 生命周期与 cron 面，管理面一个都不需要；把整个具体类型当作字段类型，
// 等于让本包的类型系统认识 pricing 的实现面，那正是这条债要消的东西。
//
// 返回值来自 internal/pricingresult（中立结果值）——本包只读
// Rows/Skipped/Updated 与 ToAdd/ToUpdate/Skipped/VariantsChanged 四个 int，
// 从不读 Preview.Entries（该字段在 pricing 侧也只写不读；把它接进响应是一次
// 独立的特性补全，不是本接口的职责）。
//
// 错误谓词：同步失败只用 errors.As 判 `*pricingresult.FetchError`，**不要**退回
// 判哨兵变量、更不要判通用的 serviceerr.ErrUpstream——那个哨兵被多处共享生产，
// 会把无关的上游故障误标成「价格同步失败」（见 pricing_port_test.go 的门禁）。
//
// 实现在 internal/pricing（方法集是超集 ⇒ 自动满足），由组合根 cmd/server 注入。
type PricingSync interface {
	// SyncNow 执行一次手动同步，返回落库统计。
	SyncNow(ctx context.Context) (*pricingresult.SyncStats, error)
	// Preview 只计算不落库，返回将要发生的变更。
	Preview(ctx context.Context) (*pricingresult.Preview, error)
}
