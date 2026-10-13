// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 用户面模型目录纯逻辑用例（零依赖）。运行：`node --test`。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { filterCatalog, priceRows, variantCondition, variantEffect } from './model-catalog.ts'
import type { CatalogGroup, CatalogModel, CatalogVariant } from './model-catalog.ts'

const group = (name: string, models: string[]): CatalogGroup => ({
  Group: { ID: 1, Name: name },
  Models: models.map(m => ({ Model: m, Variants: [] })),
})

describe('filterCatalog', () => {
  const groups = [group('A', ['gpt-4o', 'claude-3']), group('B', ['gpt-4o-mini'])]

  it('大小写不敏感子串，跨组；命中 0 的组隐藏', () => {
    const r = filterCatalog(groups, 'GPT-4O')
    assert.equal(r.length, 2)
    assert.deepEqual(r[0].Models.map(m => m.Model), ['gpt-4o'])
    assert.deepEqual(r[1].Models.map(m => m.Model), ['gpt-4o-mini'])
  })

  it('命中 0 的组整体隐藏', () => {
    const r = filterCatalog(groups, 'claude')
    assert.equal(r.length, 1)
    assert.equal(r[0].Group.Name, 'A')
  })

  it('空白查询 → 原样返回（不过滤）；无命中 → 空数组', () => {
    assert.equal(filterCatalog(groups, '   ').length, 2)
    assert.deepEqual(filterCatalog(groups, 'nope'), [])
  })
})

describe('priceRows', () => {
  it('token → 输入/输出/缓存读/缓存写（$/M）', () => {
    const m: CatalogModel = { Model: 'x', Mode: 'token', InputPerM: 1, OutputPerM: 2, Variants: [] }
    assert.deepEqual(priceRows(m).map(r => [r.key, r.unit]), [
      ['input', 'perM'],
      ['output', 'perM'],
      ['cacheRead', 'perM'],
      ['cacheWrite', 'perM'],
    ])
  })

  it('call → 按次（$/次）', () => {
    const m: CatalogModel = { Model: 'x', Mode: 'call', PricePerCall: 0.01, Variants: [] }
    assert.deepEqual(priceRows(m).map(r => [r.key, r.unit, r.value]), [['perCall', 'perCall', 0.01]])
  })

  it('image → 图像 in/out token（$/M）+ 按张（$/张）', () => {
    const m: CatalogModel = { Model: 'x', Mode: 'image', ImgInTokPerM: 1, ImgOutTokPerM: 2, PricePerImage: 0.04, Variants: [] }
    assert.deepEqual(priceRows(m).map(r => [r.key, r.unit]), [
      ['imgIn', 'perM'],
      ['imgOut', 'perM'],
      ['perImage', 'perImage'],
    ])
  })

  it('Mode=null（无价）/未知 → 空数组（调用方显示 —）', () => {
    assert.deepEqual(priceRows({ Model: 'x', Variants: [] }), [])
    assert.deepEqual(priceRows({ Model: 'x', Mode: 'weird', Variants: [] }), [])
  })
})

describe('variant adapters', () => {
  const v: CatalogVariant = {
    Seq: 1,
    ServiceTier: 'priority',
    CtxMin: 100,
    CtxMax: 200,
    TimeStart: '09:00',
    TimeEnd: '18:00',
    DowMask: 0b0000010,
    multiplier: 1.5,
    SetInputPerM: 1,
    SetCacheCreationPerM: 2,
  }

  it('CatalogVariant（PascalCase）→ 条件摘要输入（snake_case）', () => {
    assert.deepEqual(variantCondition(v), {
      service_tier: 'priority',
      ctx_min: 100,
      ctx_max: 200,
      time_start: '09:00',
      time_end: '18:00',
      dow_mask: 0b0000010,
    })
  })

  it('CatalogVariant → 效果摘要输入（SetCacheCreationPerM → set_cache_creation_per_m）', () => {
    const e = variantEffect(v)
    assert.equal(e.multiplier, 1.5)
    assert.equal(e.set_input_per_m, 1)
    assert.equal(e.set_cache_creation_per_m, 2)
  })
})
