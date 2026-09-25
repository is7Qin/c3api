// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 能力换算的纯逻辑用例：固定能力对象注入，不触网。
// 运行：`node --test`（Node 内置运行器原生跑 TS，零新依赖——与 routing-plan.test.ts 同款）。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { compositeMaxSpanSeconds, kindCostCapSeconds, kindMaxSpanSeconds } from './stats-capabilities.ts'
import type { StatsCapabilities } from './stats-capabilities.ts'

// 证据部署（spec §4.4(a) 示例）：log=2 / errlog=7 / stats=180。
const EVIDENCE: StatsCapabilities = {
  bucket_grid_seconds: 3600,
  kinds: {
    trend: {
      grouping: 'zoned',
      storages: ['cube', 'raw'],
      cost_cap_seconds: { cube: 7776000, raw: 691200 },
      coverage_days: { cube: 180, raw: 2 },
    },
    entity_trend: {
      grouping: 'zoned',
      storages: ['cube', 'raw'],
      cost_cap_seconds: { cube: 7776000, raw: 691200 },
      coverage_days: { cube: 180, raw: 2 },
    },
    usage_agg: {
      grouping: 'none',
      storages: ['raw'],
      cost_cap_seconds: { raw: 7776000 },
      coverage_days: { raw: 2 },
    },
    ttft_exact: {
      grouping: 'none',
      storages: ['raw'],
      cost_cap_seconds: { raw: 604800 },
      coverage_days: { raw: 2 },
    },
    usage_list: {
      grouping: 'none',
      storages: ['raw'],
      cost_cap_seconds: { raw: 0 },
      coverage_days: { raw: 2 },
    },
  },
}

// 缺省部署（log=30 / errlog=7 / stats=180）。
const DEF: StatsCapabilities = {
  ...EVIDENCE,
  kinds: {
    ...EVIDENCE.kinds,
    trend: {
      grouping: 'zoned',
      storages: ['cube', 'raw'],
      cost_cap_seconds: { cube: 7776000, raw: 691200 },
      coverage_days: { cube: 180, raw: 7 },
    },
    entity_trend: {
      grouping: 'zoned',
      storages: ['cube', 'raw'],
      cost_cap_seconds: { cube: 7776000, raw: 691200 },
      coverage_days: { cube: 180, raw: 7 },
    },
    usage_agg: {
      grouping: 'none',
      storages: ['raw'],
      cost_cap_seconds: { raw: 7776000 },
      coverage_days: { raw: 30 },
    },
  },
}

describe('kindMaxSpanSeconds', () => {
  it('zoned kind 取候选存储的 max(min(上限, 覆盖))——不把 cube 能力压成原始行', () => {
    assert.equal(kindMaxSpanSeconds(EVIDENCE.kinds.trend), 7776000)
    assert.equal(kindMaxSpanSeconds(DEF.kinds.trend), 7776000)
  })

  it('单存储 kind 受覆盖上界与成本上界的较小者约束', () => {
    // 证据部署：usage_agg 上限 90d 但 usage_logs 只留 2 天。
    assert.equal(kindMaxSpanSeconds(EVIDENCE.kinds.usage_agg), 2 * 86400)
    assert.equal(kindMaxSpanSeconds(DEF.kinds.usage_agg), 30 * 86400)
  })

  it('0 = 无界（上限 0 表示 keyset 分页无上限；覆盖 0 表示守卫关闭）', () => {
    // usage_list：上限 0（无界）、覆盖 2 天 ⇒ 由覆盖决定。
    assert.equal(kindMaxSpanSeconds(EVIDENCE.kinds.usage_list), 2 * 86400)
    // 只关闭覆盖而保留 0 上限 ⇒ 无界。
    assert.equal(
      kindMaxSpanSeconds({ grouping: 'none', storages: ['raw'], cost_cap_seconds: { raw: 0 }, coverage_days: { raw: 0 } }),
      Number.MAX_SAFE_INTEGER
    )
  })

  it('未知 kind / 空 storages ⇒ undefined（调用方不裁剪）', () => {
    assert.equal(kindMaxSpanSeconds(undefined), undefined)
    assert.equal(kindMaxSpanSeconds({ grouping: 'none', storages: [], cost_cap_seconds: {}, coverage_days: {} }), undefined)
  })
})

describe('compositeMaxSpanSeconds', () => {
  it('多 kind 共用查询面 ⇒ 取下界（一次调用被拒整块就报错）', () => {
    // 账号用量明细 = usage_agg + entity_trend：证据部署下 2 天（usage_logs 只留 2 天）。
    assert.equal(compositeMaxSpanSeconds(EVIDENCE, ['usage_agg', 'entity_trend']), 2 * 86400)
    // 缺省部署下 30 天（usage_agg coverage=30）。
    assert.equal(compositeMaxSpanSeconds(DEF, ['usage_agg', 'entity_trend']), 30 * 86400)
  })

  it('未知能力 / 未登记的 kind ⇒ undefined 或忽略（不得把选择器清空）', () => {
    assert.equal(compositeMaxSpanSeconds(undefined, ['usage_agg']), undefined)
    assert.equal(compositeMaxSpanSeconds(EVIDENCE, ['usage_agg', 'not_a_kind']), 2 * 86400)
  })
})

describe('kindCostCapSeconds', () => {
  it('ttft_exact 的首选（唯一）存储 raw 上限 = 168h', () => {
    assert.equal(kindCostCapSeconds(EVIDENCE, 'ttft_exact'), 604800)
  })

  it('未知能力 ⇒ undefined（不假装有上限）', () => {
    assert.equal(kindCostCapSeconds(undefined, 'ttft_exact'), undefined)
    assert.equal(kindCostCapSeconds(EVIDENCE, 'nope'), undefined)
  })
})
