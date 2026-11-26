// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package pricingresult 价格手动同步/预览的**结果值**。
//
// 为什么单独一个包：`internal/handler` 要驱动一次价格同步并读它的结果，
// 但**不该认识** `internal/pricing` 的实现面（fetcher / repo / settings / cron /
// worker 生命周期）。结果值是双方唯一共享的东西，把它放在双方都能依赖的
// 中立叶子里，`handler` 的消费方接口与 `pricing` 的实现就能各自独立演化。
// 与 `internal/modelmap` / `internal/identityenv` 同范式。
//
// 本包只放**纯数据**（三个结构体）与**一个错误类型**（FetchError，见 errors.go）。
// 加任何函数/方法进来之前先问：它是不是纯数据？不是就别放。
// **零内部依赖**（只 import stdlib）——这正是本包存在的意义。
package pricingresult

// SyncStats 一次手动同步的结果（文本价行 + 解析跳过数 + 落库数 + 拉取变体数）。
type SyncStats struct {
	Rows     int
	Skipped  int
	Updated  int
	Variants int
}

// Preview 一次预览的结果（只读比较，零写库）。
type Preview struct {
	ToAdd           int
	ToUpdate        int
	Skipped         int
	Entries         []PreviewEntry
	VariantsChanged int
}

// PreviewEntry 单模型 add/update 意向。
type PreviewEntry struct {
	Model  string
	Mode   string
	Action string // add/update
}
