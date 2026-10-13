// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 条件变体展示纯逻辑用例（零依赖，注入假 t）。运行：`node --test`。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import type { TFunction } from 'i18next'
import { boolsToDowMask, condSummary, dowMaskToBools, effectSummary, fmtMult } from './price-variant.ts'

// 假翻译：返回 key 末段（dowSun..Sat），验证星期解码走 i18n 键。
const t = ((key: string) => key.split('.').pop() ?? key) as unknown as TFunction

describe('dowMaskToBools / boolsToDowMask', () => {
  it('按位解码 bit i = 星期 i（Sun=0..Sat=6）', () => {
    assert.deepEqual(dowMaskToBools(0b0000001), [true, false, false, false, false, false, false])
    assert.deepEqual(dowMaskToBools(1 << 6), [false, false, false, false, false, false, true])
    assert.deepEqual(dowMaskToBools(null), Array(7).fill(false))
    assert.deepEqual(dowMaskToBools(undefined), Array(7).fill(false))
  })

  it('编码往返：全 false → undefined（无该条件）', () => {
    assert.equal(boolsToDowMask(Array(7).fill(false)), undefined)
    assert.equal(boolsToDowMask(dowMaskToBools(0b0110110)), 0b0110110)
  })
})

describe('fmtMult', () => {
  it('整数省略小数、非整数去尾零；null/undefined → 空串', () => {
    assert.equal(fmtMult(1), '×1')
    assert.equal(fmtMult(0), '×0')
    assert.equal(fmtMult(1.5), '×1.5')
    assert.equal(fmtMult(1.25), '×1.25')
    assert.equal(fmtMult(null), '')
    assert.equal(fmtMult(undefined), '')
  })
})

describe('condSummary', () => {
  it('组合 tier · 区间 · 星期名（经 t 解析），` · ` 分隔', () => {
    assert.equal(
      condSummary(t, { service_tier: 'priority', ctx_min: 100, ctx_max: 200, dow_mask: 0b0000010 }),
      'priority · 100–200 · dowMon'
    )
  })

  it('单边界 / 时间窗 / 无条件', () => {
    assert.equal(condSummary(t, { ctx_min: 128000 }), '≥128000')
    assert.equal(condSummary(t, { ctx_max: 128000 }), '≤128000')
    assert.equal(condSummary(t, { time_start: '09:00', time_end: '18:00' }), '09:00–18:00')
    assert.equal(condSummary(t, {}), '—')
  })
})

describe('effectSummary', () => {
  it('倍率 + set_* 覆盖价（注意 set_cache_creation_per_m 挂 cacheWrite）', () => {
    assert.equal(
      effectSummary({ multiplier: 1.5, set_input_per_m: 1, set_cache_creation_per_m: 2, set_price_per_image: 0.05 }),
      '×1.5 · in $1/M · cacheWrite $2/M · perImg $0.05'
    )
    assert.equal(effectSummary({}), '—')
  })
})
