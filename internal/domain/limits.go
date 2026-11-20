// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

// DefaultFailoverAttempts 是 proxy.failover_attempts 的默认值（总尝试次数，
// 含首次尝试）：0 会让 failover 循环零次执行，首次选号占用的并发槽永不释放。
const DefaultFailoverAttempts = 3

// MaxFailoverAttempts 是 proxy.failover_attempts 的硬上限（合法域
// 1..MaxFailoverAttempts），与 scheduler 的 attempted-ID 去重槽位数一致：
// AttemptPlan 用固定 [MaxFailoverAttempts] 集做跨账号去重，超过它无法保证
// 不重复选号，故配置校验与运行期归一都必须收在这个上界上。
const MaxFailoverAttempts = 8
