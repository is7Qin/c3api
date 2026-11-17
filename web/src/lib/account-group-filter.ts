// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 账号页「分组筛选」的选项构造与值映射（spec 2026-11-17 §2）。
//
// 服务端契约是三态（`group_id`：缺席 = 不过滤；0 = 未分组；正数 = 该分组），
// 而 Select 的 value 只能是字符串——故这里把三态钉成一组**显式 sentinel**
// （`all` / `ungrouped`）而不是空串：空串在 Select 里既可能是「未选」也可能是
// 「值为空」，UI 层再也分不清三态里的哪一态。GROUP_FILTER_VALUE_TO_WIRE 是
// sentinel ⇄ wire 的唯一换算点，页面只调用 groupFilterValue()。
//
// 纯函数（无 React / 无 i18n 运行时依赖）：选项只带 i18n 键或原始分组名，
// 由调用方翻译；分组名是外部数据（用户输入），不进 locale 文件。

/** 「全部分组」sentinel：映射到 wire 的**缺席**（不过滤）。 */
export const GROUP_FILTER_ALL = 'all'
/** 「未分组」sentinel：映射到 wire 的 `0`（不是「不过滤」）。 */
export const GROUP_FILTER_UNGROUPED = 'ungrouped'

/** 固定两项的 i18n 键（分组项不带键，直接用 name）。 */
export type GroupFilterLabelKey = 'accounts.groupFilter.all' | 'accounts.groupFilter.ungrouped'

/** Select 的一项：value 是 sentinel 或 `String(groupId)`；固定两项带 labelKey。 */
export interface GroupFilterOption {
  value: string
  labelKey?: GroupFilterLabelKey
  name?: string
}

/** 选项数据源的最小形状（页面传的是 `components['schemas']['Group'][]`）。 */
export interface GroupFilterSource {
  ID?: number
  Name?: string
}

/**
 * groupFilterOptions 构造分组筛选的选项列表：
 * `[全部分组, 未分组, ...各分组名]`（顺序与分组列表一致）。
 *
 * 空列表**不隐藏控件**：仍返回「全部 / 未分组」两项（spec §2.1——供应商可能
 * 一个分组都没有，「未分组」这一档正是他要找的）。
 */
export function groupFilterOptions(groups: readonly GroupFilterSource[]): GroupFilterOption[] {
  const fixed: GroupFilterOption[] = [
    { value: GROUP_FILTER_ALL, labelKey: 'accounts.groupFilter.all' },
    { value: GROUP_FILTER_UNGROUPED, labelKey: 'accounts.groupFilter.ungrouped' },
  ]
  const rest: GroupFilterOption[] = []
  for (const g of groups) {
    if (g.ID === undefined) continue
    const value = String(g.ID)
    // sentinel 与真实分组 id 不可撞车（id 是数字串，故结构上不会；此断言防未来改 sentinel）。
    if (value === GROUP_FILTER_ALL || value === GROUP_FILTER_UNGROUPED) continue
    rest.push({ value, name: g.Name ?? `#${g.ID}` })
  }
  return [...fixed, ...rest]
}

/**
 * groupFilterValue 把 Select 的 value 翻成 wire 的三态：
 * `all → undefined`（缺席 = 不过滤）、`ungrouped → 0`（未分组）、`<id> → id`。
 * 未知/非法值按「不过滤」处理（宁可多显示，不要因一个坏 sentinel 变成空列表）。
 */
export function groupFilterValue(raw: string): number | undefined {
  if (raw === GROUP_FILTER_ALL) return undefined
  if (raw === GROUP_FILTER_UNGROUPED) return 0
  const trimmed = raw.trim()
  if (trimmed === '') return undefined
  const n = Number(trimmed)
  if (!Number.isFinite(n) || !Number.isInteger(n) || n < 0) return undefined
  return n
}
