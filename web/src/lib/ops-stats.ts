// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 运维页 stats 值的展示格式化（纯函数：node --test 直接跑，见 ops-stats.test.ts）。
//
// stats 是契约自由 schema（unknown），各 worker 异构：数字（部分 unix_ms 是时刻）、
// 布尔、字符串，也有**嵌套对象**（如 supplier-view 的 snapshot）。单元格只允许人读
// 文本——**永不**输出 JSON 的括号/引号：对象逐字段展平为「标签 值」，数组按原始值
// 拼接或折叠为条数。原始 JSON 只走 statTooltip（title 悬浮），不进单元格。
//
// 文案由调用方注入（页面用 i18n 的 t()）：子字段标签 ops.statField.<k>，条数文本
// ops.statItems。两者都缺省时输出键名与纯数字——本文件不含任何界面文案。
// 布尔刻意保持 true/false，不本地化：单元格由页面用徽章渲染（ops.boolYes/boolNo），
// 这里的结果只服务 title 与嵌套对象展平。

const DASH = '—'
const SEP = ' · '
// 单元格一行放不下：展平后超过 80 字符即截断（完整值在 title）。
const MAX_LEN = 80
// 对象最多降两层（顶层对象 = 1）；再深只给条数，避免一条 stats 撑爆卡片。
const MAX_DEPTH = 2
// 数组原始值最多列 4 项，其余折叠为 `+N`。
const ARRAY_HEAD = 4

// 注入的界面文案。缺省：标签 = 原键名，条数 = 纯数字（无任何语言假设）。
export type StatFormat = {
  field?: (k: string) => string
  items?: (n: number) => string
}

const isObject = (v: unknown): v is Record<string, unknown> =>
  typeof v === 'object' && v !== null && !Array.isArray(v)

const clip = (s: string): string => (s.length > MAX_LEN ? `${s.slice(0, MAX_LEN)}…` : s)

const fieldLabel = (fmt: StatFormat | undefined, k: string): string => (fmt?.field ? fmt.field(k) : k)

const itemsText = (fmt: StatFormat | undefined, n: number): string => (fmt?.items ? fmt.items(n) : String(n))

function formatScalar(v: unknown, key: string): string {
  if (typeof v === 'number') {
    // last_redis_attempt_ms 是毫秒时间戳（契约命名遗留 _ms 后缀，非时长）
    if ((key.endsWith('unix_ms') || key === 'last_redis_attempt_ms') && v > 0) {
      return new Date(v).toLocaleString()
    }
    return v.toLocaleString()
  }
  if (typeof v === 'boolean') return v ? 'true' : 'false'
  if (typeof v === 'string') return v || DASH
  return DASH
}

function formatArray(v: unknown[], key: string, fmt: StatFormat | undefined, depth: number): string {
  if (v.length === 0) return DASH
  // 含对象/数组的数组无法一行说清：只报条数。
  if (v.some(x => isObject(x) || Array.isArray(x))) return itemsText(fmt, v.length)
  const head = v.slice(0, ARRAY_HEAD).map(x => formatValue(x, key, fmt, depth)).join(SEP)
  return v.length > ARRAY_HEAD ? `${head} +${v.length - ARRAY_HEAD}` : head
}

function formatObject(v: Record<string, unknown>, fmt: StatFormat | undefined, depth: number): string {
  const entries = Object.entries(v)
  if (entries.length === 0) return DASH
  const text = entries
    .map(([childKey, child]) => `${fieldLabel(fmt, childKey)} ${formatValue(child, childKey, fmt, depth + 1)}`)
    .join(SEP)
  return clip(text)
}

function formatValue(v: unknown, key: string, fmt: StatFormat | undefined, depth: number): string {
  if (Array.isArray(v)) return formatArray(v, key, fmt, depth)
  if (isObject(v)) {
    const n = Object.keys(v).length
    if (n === 0) return DASH
    if (depth > MAX_DEPTH) return itemsText(fmt, n)
    return formatObject(v, fmt, depth)
  }
  return formatScalar(v, key)
}

// 单元格文本：数字（unix_ms 正值转本地时刻）、布尔、字符串、数组、对象逐层展平。
export function formatStatValue(v: unknown, key: string, fmt?: StatFormat): string {
  return formatValue(v, key, fmt, 1)
}

// title 悬浮文本：原始值直接字符串化，对象/数组保留完整 JSON（单元格已不含 JSON）。
export function statTooltip(v: unknown, key: string): string {
  if (v === null || v === undefined) return DASH
  if (typeof v === 'object') {
    try {
      return JSON.stringify(v) ?? DASH
    } catch {
      // 循环引用（正常 JSON 响应不会出现）：退回展平文本，title 也不能是空串。
      return formatStatValue(v, key)
    }
  }
  if (typeof v === 'string') return v || DASH
  return String(v)
}
