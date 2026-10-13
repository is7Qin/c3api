// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 用户面「模型 / 定价」页的可测纯逻辑（组件只做渲染）：搜索过滤、Mode→价格
// 分量映射、CatalogVariant→条件/效果摘要输入归一。零运行时依赖（仅 type 导入）
// ⇒ 可被 `node --test` 直接跑（与 stats-capabilities.test.ts 同款）。
import type { components } from '@/lib/api/schema'
import type { VariantCondition, VariantEffect } from '@/lib/price-variant'

export type CatalogGroup = components['schemas']['CatalogGroup']
export type CatalogModel = components['schemas']['CatalogModel']
export type CatalogVariant = components['schemas']['CatalogVariant']

// 价格分量键（决定 i18n label + 单位）：token/image 系 token 价 → $/M；
// perCall → $/次；perImage → $/张。
export type PriceRowKey = 'input' | 'output' | 'cacheRead' | 'cacheWrite' | 'perCall' | 'imgIn' | 'imgOut' | 'perImage'
export type PriceRowUnit = 'perM' | 'perCall' | 'perImage'
export interface PriceRow {
  key: PriceRowKey
  unit: PriceRowUnit
  value: number | null | undefined
}

// filterCatalog 跨组搜索：模型名大小写不敏感子串匹配；命中 0 的组隐藏；空白
// 查询 → 原样返回（不过滤）。返回新对象（不改入参）。
export function filterCatalog(groups: CatalogGroup[], query: string): CatalogGroup[] {
  const q = query.trim().toLowerCase()
  if (!q) return groups
  const out: CatalogGroup[] = []
  for (const g of groups) {
    const models = g.Models.filter(m => m.Model.toLowerCase().includes(q))
    if (models.length) out.push({ ...g, Models: models })
  }
  return out
}

// priceRows 按 Mode 决定展示的价格分量（顺序固定）：
//   token → 输入/输出/缓存读/缓存写（$/M）
//   call  → 按次价（$/次）
//   image → 图像 in/out token 价（$/M）+ 按张价（$/张）
// Mode=null（无价模型）或未知 → 空数组（调用方显示 —）。
export function priceRows(m: CatalogModel): PriceRow[] {
  switch (m.Mode) {
    case 'token':
      return [
        { key: 'input', unit: 'perM', value: m.InputPerM },
        { key: 'output', unit: 'perM', value: m.OutputPerM },
        { key: 'cacheRead', unit: 'perM', value: m.CacheReadPerM },
        { key: 'cacheWrite', unit: 'perM', value: m.CacheWritePerM },
      ]
    case 'call':
      return [{ key: 'perCall', unit: 'perCall', value: m.PricePerCall }]
    case 'image':
      return [
        { key: 'imgIn', unit: 'perM', value: m.ImgInTokPerM },
        { key: 'imgOut', unit: 'perM', value: m.ImgOutTokPerM },
        { key: 'perImage', unit: 'perImage', value: m.PricePerImage },
      ]
    default:
      return []
  }
}

// variantCondition CatalogVariant（PascalCase）→ 条件摘要输入（snake_case）。
export function variantCondition(v: CatalogVariant): VariantCondition {
  return {
    service_tier: v.ServiceTier,
    ctx_min: v.CtxMin,
    ctx_max: v.CtxMax,
    time_start: v.TimeStart,
    time_end: v.TimeEnd,
    dow_mask: v.DowMask,
  }
}

// variantEffect CatalogVariant → 效果摘要输入（注意 SetCacheCreationPerM 语义
// = 覆盖缓存写价，映射到 set_cache_creation_per_m）。
export function variantEffect(v: CatalogVariant): VariantEffect {
  return {
    multiplier: v.multiplier,
    set_input_per_m: v.SetInputPerM,
    set_output_per_m: v.SetOutputPerM,
    set_cache_read_per_m: v.SetCacheReadPerM,
    set_cache_creation_per_m: v.SetCacheCreationPerM,
    set_price_per_call: v.SetPricePerCall,
    set_img_in_tok_per_m: v.SetImgInTokPerM,
    set_img_out_tok_per_m: v.SetImgOutTokPerM,
    set_price_per_image: v.SetPricePerImage,
  }
}
