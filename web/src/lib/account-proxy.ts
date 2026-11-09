// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 账号「出口代理」绑定的三态投影（单条编辑 + 批量）——**唯一实现**。
//
// 为什么单独成模块（安全性理由，spec §3.6）：
// 账号的 `proxy_id` 是 wire 三态字段：缺席 = 不变 / null = 解绑 / >0 = 绑定。
// 而读面的回显**受可见性约束**：供应商面看不到平台代理，管理员把平台代理绑到某供应商
// 账号后，该供应商读这个账号时 `proxy_id` 回显 **null**（spec §6.2 第 6 条的读面口径）。
// 于是「把整张表单原样覆盖回去」= 把 admin 绑的平台代理静默解绑——用户从未表达这个意图。
//
// 本模块因此把「未触碰」建模成**一等状态**：
// - 单条：`buildAccountPatch(initial, draft)` 里 `initial` 是读到的回显。
//   回显为 null 时**永远不会**产出 `proxy_id: null`——即使调用方传入「直连」草稿。
//   （真解绑只可能发生在「回显非 null 且用户选直连」：那说明该代理对调用方可见，
//    他确实在解除自己看得见的那条绑定。）
// - 批量：`buildBatchProxyFields(sel)` 里 `null` = 不变（不发键），`direct` = null，
//   `bind` = id。批量没有「回显」可比，故「不变」必须由调用方显式选择。
import type { components } from '@/lib/api/schema'
import { proxyOptionLabel } from './proxy-form.ts'

export type AccountConfigPatch = components['schemas']['AccountConfigPatch']
export type ProxyPickerItem = components['schemas']['Proxy']

// 单条编辑的选择（Select 的语义值）：直连 或 绑定某条代理。
export type ProxySelection = { kind: 'direct' } | { kind: 'bind'; id: number }

// Select 的空值哨兵（wrapper 契约：不允许用 '' / placeholder 表达已选值）。
export const PROXY_DIRECT_VALUE = '__direct'
// 批量面的「不变」哨兵：与 PROXY_DIRECT_VALUE 同处一张 items 映射里。
export const PROXY_UNCHANGED_VALUE = '__unchanged'

export const proxyDirect = (): ProxySelection => ({ kind: 'direct' })

// 读到的回显 → Select 值（null = 直连/未绑定；非 null = 该代理对调用方可见）。
export function proxySelectionOf(echo: number | null | undefined): ProxySelection {
  return echo != null && echo > 0 ? { kind: 'bind', id: echo } : proxyDirect()
}

export function proxySelectValue(sel: ProxySelection): string {
  return sel.kind === 'bind' ? String(sel.id) : PROXY_DIRECT_VALUE
}

export function parseProxySelectValue(value: string): ProxySelection {
  const id = Number(value)
  return Number.isInteger(id) && id > 0 ? { kind: 'bind', id } : proxyDirect()
}

// 批量三态：null = 不变（不发键）。与单条同源的两种取值。
export function parseBatchProxyValue(value: string): ProxySelection | null {
  if (value === PROXY_UNCHANGED_VALUE) return null
  return parseProxySelectValue(value)
}

// 单条 payload 投影。
// - **回显 null ⇒ 永不发 proxy_id**（看不见的绑定不得被「覆盖回 null」静默解绑）
// - 回显 == 草稿 ⇒ 不发（未变更）
// - 其余：direct ⇒ null（解绑可见的那条）；bind ⇒ id
export function buildAccountPatch(
  initial: number | null | undefined,
  draft: ProxySelection,
): Pick<AccountConfigPatch, 'proxy_id'> {
  const initialId = initial != null && initial > 0 ? initial : null
  if (draft.kind === 'bind') {
    if (initialId === draft.id) return {}
    if (draft.id <= 0) return {} // 非法 id 当作未触碰（fail-closed，不发明绑定）
    return { proxy_id: draft.id }
  }
  // direct
  if (initialId == null) return {}
  return { proxy_id: null }
}

// 批量 payload 投影：null（不变）⇒ {}；direct ⇒ null；bind ⇒ id。语义与单条同源。
export function buildBatchProxyFields(sel: ProxySelection | null): Pick<AccountConfigPatch, 'proxy_id'> {
  if (sel == null) return {}
  if (sel.kind === 'bind') return sel.id > 0 ? { proxy_id: sel.id } : {}
  return { proxy_id: null }
}

// Select items 映射：直连/不变哨兵 + 可见代理列表（label 与 SelectItem 共用同一拼法，
// 避免闭合态与弹出列表显示不一致）。
// disabledSuffix：停用的代理**仍可选**（绑定后账号不被调度，由后端 Routable 决定），
// 但必须在选项上就看得出来——否则用户会把「账号忽然不接流量」当成平台 bug。
export function proxySelectItems(
  proxies: readonly ProxyPickerItem[],
  opts?: { directLabel?: string; unchangedLabel?: string; disabledSuffix?: string },
): Record<string, string> {
  const items: Record<string, string> = {}
  if (opts?.unchangedLabel !== undefined) items[PROXY_UNCHANGED_VALUE] = opts.unchangedLabel
  items[PROXY_DIRECT_VALUE] = opts?.directLabel ?? PROXY_DIRECT_VALUE
  for (const p of proxies) {
    const suffix = p.enabled === false && opts?.disabledSuffix ? opts.disabledSuffix : ''
    items[String(p.id)] = proxyOptionLabel(p) + suffix
  }
  return items
}

// 回显不可信提示的判据：供应商面 + 回显为 null ⇒ 可能被管理员绑了平台代理（不可见）。
// 只用于 UI 文案，不参与任何写入决策。
export function echoMayHideBinding(kind: 'admin' | 'supplier', echo: number | null | undefined): boolean {
  return kind === 'supplier' && echo == null
}

// —— 分组（group_ids）的三态：**字段本身就是三态，不要再叠哨兵** ——
//
// 分组回显不随账号列表返回（GET /accounts/{id}/groups，对话框打开时才拉），
// 于是「表单里是空数组」有两种含义：用户清空了 / 回显还没到。混为一谈就会用
// `[]` 覆盖账号已有分组——这类 bug 修过两次，两次都在让「快照」变完整，
// 而不是让「写入」不依赖快照。
//
// 契约（PATCH）：缺席 = 不变、`[]` = 清空。所以草稿用 `number[] | null` 表达意图：
// null = 没表态（不发键），[] = 明确清空，[ids] = 替换。展示侧派生自回显，
// 不拷进表单——拷贝就会有两份真值，就得靠 effect 同步，同步就有窗口。
export function buildGroupIdsField(
  ids: readonly number[] | null,
): Pick<AccountConfigPatch, 'group_ids'> {
  return ids === null ? {} : { group_ids: [...ids] }
}
