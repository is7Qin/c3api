// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package qualitymodel

import "errors"

// ErrRoutingSnapshotBeyondRetention 快照分钟早于观测保留截止（§5.4 写面守卫）。
// 与"旧序号被拒"的幂等 no-op 语义**不同**：那条路径的 payload 已经落库过
// （tx.Commit 返回 nil 正确），而超期拒绝意味着该分钟的链已被保留策略丢弃，
// 是数据丢失。故必须显式返回本哨兵，调用方不得照抄静默形状——否则合法迟到
// 实例的链会无痕消失（判据 B5）。
//
// 为什么不进 internal/storeerr（R1-b §3.1.1 裁定）：storeerr 收的是"跨域、
// 独立于业务策略的存储协议分类"（not found / conflict / stale），文案带
// "repository: " 前缀。本哨兵编码的是**保留期策略**（超期即丢），不是任何存储
// 都会有的协议分类，且基线文案无前缀 —— 搬进去会让 storeerr 的文案约定出现
// 例外。它与 consumer 数量无关，留在域内契约包。
//
// 文案是日志/调用方契约，**一个字节都不能改**。
var ErrRoutingSnapshotBeyondRetention = errors.New("routing flow snapshot beyond observation retention")
