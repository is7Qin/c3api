// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 展示格式化工具（页面共享，放 components/ 以便与提交范围一致）。
// 后端 err_rate 为比率（0~1），按 brief 以百分比展示。
export function formatPercent(v?: number): string {
  return v == null ? '—' : `${(v * 100).toFixed(1)}%`
}

export function formatDateTime(iso?: string): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleString('zh-CN', { hour12: false })
}

// 超长文本截断（title/末尾省略），适合表格单元格。
export function truncate(s: string | undefined | null, n = 16): string {
  if (!s) return '—'
  return s.length > n ? `${s.slice(0, n)}…` : s
}

// datetime-local → RFC3339（本地时区输入 → UTC ISO），非法/空值返回 undefined。
export function toRFC3339(v: string): string | undefined {
  if (!v) return undefined
  const d = new Date(v)
  return Number.isNaN(d.getTime()) ? undefined : d.toISOString()
}

// datetime-local 本地时间补零（YYYY-MM-DDTHH:mm）。
const pad2 = (n: number) => String(n).padStart(2, '0')

// 默认近 24h 日志范围（datetime-local 本地时区字面量；调用方在组件挂载时固定一次，
// 避免渲染期时间漂移；from/to 契约必填）。
export function defaultLogRange(): { from: string; to: string } {
  const to = new Date()
  const from = new Date(to.getTime() - 24 * 3600 * 1000)
  const local = (d: Date) =>
    `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}T${pad2(d.getHours())}:${pad2(d.getMinutes())}`
  return { from: local(from), to: local(to) }
}

// 逗号列表截断展示，完整内容放 title。
export function commaList(items: string[] | undefined, max = 3): { text: string; full: string } {
  const full = items?.join(', ') ?? ''
  if (!full) return { text: '—', full: '' }
  const head = items!.slice(0, max).join(', ')
  const text = items!.length > max ? `${head} +${items!.length - max}` : head
  return { text, full }
}

// 金额格式化（毫分）：后端计费以毫分为单位，1 USD = 100,000 毫分。
// 空/0 → null，调用方统一显示 —（未计费路径 Cost 为 0/空，与真实 $0.0000 无法区分，直接省略）。
const MILLI_CENTS_PER_USD = 100_000
function usdText(c?: number | null): string | null {
  if (c == null || c <= 0) return null
  return `$${(c / MILLI_CENTS_PER_USD).toFixed(5)}`
}

// 计费成本：毫分 → USD 字符串，如 $3.25000 / $0.00001（5 位保 1 毫分可见）；空值或 0 显示 —。
export function formatCost(c?: number | null): string {
  return usdText(c) ?? '—'
}

// USD 金额直接展示（API 已换算的统计面值，如 /stats、/overview 的 Cost/cost_usd）：
// $3.25000；空值或 0 显示 —。与 formatCost（毫分语义）并存——两单位各有明确消费面。
export function formatUSD(c?: number | null): string {
  if (c == null || c <= 0) return '—'
  return `$${c.toFixed(5)}`
}

// 每百万 token 价格：USD/1M tokens 正常值直接展示（API 边界已换算，内部存储毫分），
// 如 3.5 → $3.5000/M；空值显示 —（0 = 免费价，照常展示 $0.0000/M）。
export function formatPricePerMillion(c?: number | null): string {
  return c == null ? '—' : `$${c.toFixed(4)}/M`
}
