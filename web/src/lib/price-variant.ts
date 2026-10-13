// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 价格条件变体的展示纯逻辑（自 web/src/pages/pricing.tsx 下沉，管理面与用户面
// 「模型 / 定价」页共用——避免两份条件摘要/倍率格式漂移）。i18n 复用
// `pricing.variants.*`（含 dowSun..Sat）。
import type { TFunction } from 'i18next'

// 星期掩码位序：bit i = 星期 i（Sun=0..Sat=6，与后端 DowMask 一致）。
export const DOW_KEYS = ['dowSun', 'dowMon', 'dowTue', 'dowWed', 'dowThu', 'dowFri', 'dowSat'] as const

// DowMask（位掩码）→ 7 元素布尔数组（Sun..Sat）；null/undefined → 全 false。
export const dowMaskToBools = (mask: number | null | undefined): boolean[] =>
  Array.from({ length: 7 }, (_, i) => mask != null && (mask & (1 << i)) !== 0)

// 7 元素布尔数组 → DowMask；全 false → undefined（无该条件）。
export const boolsToDowMask = (bools: boolean[]): number | undefined => {
  const m = bools.reduce((acc, v, i) => (v ? acc | (1 << i) : acc), 0)
  return m === 0 ? undefined : m
}

// 倍率（正常值）→ 展示串：null/undefined → 空串；整数省略小数，非整数去尾零
// （×1 → "×1"、×1.5 → "×1.5"、×0 → "×0"）。
export const fmtMult = (m: number | null | undefined): string => {
  if (m == null) return ''
  return `×${Number.isInteger(m) ? String(m) : m.toFixed(4).replace(/\.?0+$/, '')}`
}

// 变体条件字段（snake_case，与管理面 PriceVariantUpsert 同形；用户面
// CatalogVariant 经 adapter 归一后同用）。
export interface VariantCondition {
  service_tier?: string | null
  ctx_min?: number | null
  ctx_max?: number | null
  time_start?: string | null
  time_end?: string | null
  dow_mask?: number | null
}

// 变体效果字段（snake_case；set_cache_creation_per_m = 覆盖**缓存写**价）。
export interface VariantEffect {
  multiplier?: number | null
  set_input_per_m?: number | null
  set_output_per_m?: number | null
  set_cache_read_per_m?: number | null
  set_cache_creation_per_m?: number | null
  set_price_per_call?: number | null
  set_img_in_tok_per_m?: number | null
  set_img_out_tok_per_m?: number | null
  set_price_per_image?: number | null
}

// 条件摘要（tier · ctx 区间 · 时间窗 · 星期名，` · ` 分隔）；无条件 → '—'。
// 星期名经 t 解析（pricing.variants.dowSun..Sat），与部署语言一致。
export function condSummary(t: TFunction, r: VariantCondition): string {
  const parts: string[] = []
  if (r.service_tier) parts.push(r.service_tier)
  if (r.ctx_min != null || r.ctx_max != null) {
    if (r.ctx_min != null && r.ctx_max != null) parts.push(`${r.ctx_min}–${r.ctx_max}`)
    else if (r.ctx_min != null) parts.push(`≥${r.ctx_min}`)
    else parts.push(`≤${r.ctx_max}`)
  }
  if (r.time_start || r.time_end) {
    if (r.time_start && r.time_end) parts.push(`${r.time_start}–${r.time_end}`)
    else {
      const single = r.time_start ?? r.time_end
      if (single) parts.push(single)
    }
  }
  if (r.dow_mask != null) {
    const bools = dowMaskToBools(r.dow_mask)
    const labels = bools.flatMap((v, i) => (v ? [t(`pricing.variants.${DOW_KEYS[i]}`)] : []))
    if (labels.length) parts.push(labels.join(','))
  }
  return parts.length ? parts.join(' · ') : '—'
}

// 效果摘要（倍率 · set_* 覆盖价），` · ` 分隔；无效果 → '—'。覆盖价挂正确列
// （set_cache_creation_per_m → cacheWrite）。
export function effectSummary(r: VariantEffect): string {
  const parts: string[] = []
  if (r.multiplier != null) parts.push(fmtMult(r.multiplier))
  if (r.set_input_per_m != null) parts.push(`in $${r.set_input_per_m}/M`)
  if (r.set_output_per_m != null) parts.push(`out $${r.set_output_per_m}/M`)
  if (r.set_cache_read_per_m != null) parts.push(`cacheRead $${r.set_cache_read_per_m}/M`)
  if (r.set_cache_creation_per_m != null) parts.push(`cacheWrite $${r.set_cache_creation_per_m}/M`)
  if (r.set_price_per_call != null) parts.push(`call $${r.set_price_per_call}`)
  if (r.set_img_in_tok_per_m != null) parts.push(`imgIn $${r.set_img_in_tok_per_m}/M`)
  if (r.set_img_out_tok_per_m != null) parts.push(`imgOut $${r.set_img_out_tok_per_m}/M`)
  if (r.set_price_per_image != null) parts.push(`perImg $${r.set_price_per_image}`)
  return parts.length ? parts.join(' · ') : '—'
}
