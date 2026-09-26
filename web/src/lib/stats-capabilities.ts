// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 统计能力（/stats/capabilities）到"能查多久"的纯换算——选择器裁剪与 TTFT 窗口
// 钳制的唯一来源。判定仍全部在服务端：本文件只回答"这个部署最长能服务多久"，
// **不回答**"这次请求会不会走 cube"（后者是逐请求事实，由 400 机读字段与 200
// 回显头承担，spec §4.4(a) 明确拒绝把逐请求事实伪装成部署常量）。
//
// 纯函数（无 React、无请求）：node --test 直接跑，见 stats-capabilities.test.ts。
import type { components } from '@/lib/api/schema'

export type StatsCapabilities = components['schemas']['StatsCapabilities']
export type StatsKindCapability = components['schemas']['StatsKindCapability']

// 无上限哨兵：`cost_cap_seconds = 0`（keyset 分页）与 `coverage_days = 0`（该表
// 未启用分区保留、覆盖守卫关闭）都是"无界"，在"能查多久"里等价于无穷大。
const UNBOUNDED = Number.MAX_SAFE_INTEGER

// 单 kind 在本部署**最长可服务**的跨度（秒）：候选存储的 max(min(成本上限, 覆盖上界))。
//
// 取 max 而非 min：候选存储里哪一条能用取决于窗口位置（界不齐的时区只能扫原始
// 行），这是逐请求事实——capabilities 只报部署常量。若取 min，"对齐时能查 90 天"
// 的正常部署会被压到原始行那个 7 天，等于把能力藏起来（与 spec R3"文档化数字在
// 部署上不可达"是同一类错的镜像）。超过本上界的请求在**任何**存储上都必然被
// 服务端拒绝，故按它裁剪是纯损失消除、零能力损失。
//
// 返回值 undefined = 能力未知（未加载或该 kind 不在响应里）⇒ 调用方不裁剪。
export function kindMaxSpanSeconds(k?: StatsKindCapability): number | undefined {
  // 空 storages = 形状不可解释（契约里恒非空）⇒ 视为未知而非"最长 0 秒"：
  // 未知的代价是不裁剪（保守），误判 0 的代价是把选择器清空（把部署变成不可用）。
  if (!k || !Array.isArray(k.storages) || k.storages.length === 0) return undefined
  let best = 0
  for (const storage of k.storages) {
    const cap = k.cost_cap_seconds?.[storage] ?? 0
    const coverageDays = k.coverage_days?.[storage] ?? 0
    const byCost = cap === 0 ? UNBOUNDED : cap
    const byCoverage = coverageDays === 0 ? UNBOUNDED : coverageDays * 86400
    const span = Math.min(byCost, byCoverage)
    if (span > best) best = span
  }
  return best
}

// 多个 kind **共用同一个查询面**时的可服务跨度 = 各 kind 的下界——面里每一次
// 调用都必须成立（例如账号用量明细 = /accounts/usage + /stats/entity-trend +
// 尾窗 /accounts/usage，任一被拒整块就报错）。缺失的 kind 视为未知并跳过
// （不因一个 unrecognized kind 把整个选择器清空）。
export function compositeMaxSpanSeconds(
  caps: StatsCapabilities | undefined,
  kinds: readonly string[]
): number | undefined {
  if (!caps) return undefined
  let bound: number | undefined
  for (const id of kinds) {
    const span = kindMaxSpanSeconds(caps.kinds[id])
    if (span === undefined) continue
    bound = bound === undefined ? span : Math.min(bound, span)
  }
  return bound
}

// kind **首选候选存储**的成本上限（秒；0 = 无上限）——实体级精确 TTFT 的窗口
// 钳制用它（该 kind 只有 raw 一种候选存储，故"首选"即唯一）。
export function kindCostCapSeconds(
  caps: StatsCapabilities | undefined,
  kindId: string
): number | undefined {
  const k = caps?.kinds[kindId]
  if (!k || !Array.isArray(k.storages) || k.storages.length === 0) return undefined
  return k.cost_cap_seconds?.[k.storages[0]]
}
