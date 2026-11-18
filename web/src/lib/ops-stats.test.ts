// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 运维页 stats 单元格格式化：**永不输出 JSON 括号/引号**（对象展平成「标签 值」，
// 数组拼接或折叠成条数），原始 JSON 只留给 title（statTooltip）。
// 运行：`node --test`。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { formatAverage, formatRate, formatStatValue, statTooltip } from './ops-stats.ts'

// supplier-view 的 snapshot（internal/supplier/view.go:114-121 的四字段形态）。
const snapshot = {
  loaded: true,
  revision: 42,
  last_success_unix_ms: 1_763_000_000_000,
  stale_age_ms: 1_500,
}

const FIELDS: Record<string, string> = {
  loaded: '已装载',
  revision: '版本',
  last_success_unix_ms: '最近成功',
  stale_age_ms: '距今',
}
const zhField = (k: string): string => FIELDS[k] ?? k

describe('formatStatValue：对象（快照三态）', () => {
  it('逐字段展平为「标签 值」，且不含 JSON 的括号与引号', () => {
    const text = formatStatValue(snapshot, 'snapshot', { field: zhField })
    assert.ok(text.includes('已装载'), text)
    assert.ok(text.includes('版本'), text)
    assert.ok(text.includes('最近成功'), text)
    assert.ok(text.includes('距今'), text)
    for (const ch of ['{', '}', '[', ']', '"']) {
      assert.equal(text.includes(ch), false, `不得出现 ${ch}: ${text}`)
    }
  })

  it('未知子键原样兜底（statField 缺省 = 键名本身）', () => {
    assert.equal(formatStatValue({ unknown_field: 7 }, 'x'), 'unknown_field 7')
    assert.equal(formatStatValue({ unknown_field: 7 }, 'x', { field: k => k }), 'unknown_field 7')
  })

  it('空对象 → —', () => {
    assert.equal(formatStatValue({}, 'x'), '—')
  })

  it('嵌套对象再降一层；超过两层只给条数', () => {
    const text = formatStatValue({ snapshot: { loaded: true, inner: { a: 1, b: 2 } } }, 'outer')
    assert.equal(text, 'snapshot loaded true · inner 2')
  })

  it('展平后总长 > 80 ⇒ 截断并以 … 结尾', () => {
    const wide: Record<string, string> = {}
    for (let i = 0; i < 12; i++) wide[`field_${i}`] = 'x'.repeat(10)
    const text = formatStatValue(wide, 'wide')
    assert.ok(text.endsWith('…'), text)
    assert.equal(text.length, 81, '80 字符 + 省略号')
  })
})

describe('formatStatValue：数组', () => {
  it('原始值前 4 项以 · 连接，多出的记为 +N', () => {
    assert.equal(formatStatValue(['a', 'b', 'c'], 'x'), 'a · b · c')
    const five = formatStatValue(['a', 'b', 'c', 'd', 'e'], 'x')
    assert.equal(five, 'a · b · c · d +1')
    assert.ok(five.endsWith(' +1'))
  })

  it('含对象 → 只报条数', () => {
    assert.equal(formatStatValue([{ a: 1 }, { b: 2 }], 'x'), '2')
  })

  it('条数文本走注入的 items（缺省纯数字，不夹带任何界面文案）', () => {
    const en = formatStatValue([{ a: 1 }, { b: 2 }], 'x', { items: n => `${n} items` })
    assert.equal(en, '2 items')
    assert.equal(/[\u4e00-\u9fff]/.test(en), false, 'en 文案下不得出现中日韩字符')
    const bare = formatStatValue([{ a: 1 }, { b: 2 }], 'x')
    assert.equal(bare, '2')
    assert.equal(/[\u4e00-\u9fff]/.test(bare), false, '缺省文案不得夹带中文')
  })

  it('深层对象同样走 items 文本', () => {
    assert.equal(formatStatValue({ a: { b: { c: 1 } } }, 'x', { items: n => `${n} items` }), 'a b 1 items')
  })

  it('空数组 → —', () => {
    assert.equal(formatStatValue([], 'x'), '—')
  })
})

describe('formatStatValue：标量', () => {
  it('unix_ms 正值 → 本地时间串（不是数字）', () => {
    const ms = 1_763_000_000_000
    const text = formatStatValue(ms, 'last_success_unix_ms')
    assert.equal(text, new Date(ms).toLocaleString())
    assert.notEqual(text, String(ms))
    // 契约遗留命名：last_redis_attempt_ms 同样是时刻（非时长）
    assert.equal(formatStatValue(ms, 'last_redis_attempt_ms'), new Date(ms).toLocaleString())
  })

  it('unix_ms 的 0 = 从未触发（不当 1970 显示）', () => {
    assert.equal(formatStatValue(0, 'last_success_unix_ms'), '0')
  })

  it('普通数字千分位；布尔小写；空串/null/undefined → —', () => {
    assert.equal(formatStatValue(1234567, 'rows'), (1234567).toLocaleString())
    assert.equal(formatStatValue(false, 'running'), 'false')
    assert.equal(formatStatValue('', 'last_error'), '—')
    assert.equal(formatStatValue(null, 'x'), '—')
    assert.equal(formatStatValue(undefined, 'x'), '—')
  })
})

describe('statTooltip：原始值只进 title', () => {
  it('对象/数组返回合法 JSON（可 JSON.parse）', () => {
    assert.deepEqual(JSON.parse(statTooltip(snapshot, 'snapshot')), snapshot)
    assert.deepEqual(JSON.parse(statTooltip(['a', { b: 1 }], 'x')), ['a', { b: 1 }])
  })

  it('原始值直接字符串化；空值 → —', () => {
    assert.equal(statTooltip(42, 'x'), '42')
    assert.equal(statTooltip(true, 'x'), 'true')
    assert.equal(statTooltip(null, 'x'), '—')
    assert.equal(statTooltip('', 'x'), '—')
  })
})

describe('formatRate / formatAverage：重试卡的比值单元格', () => {
  it('比值 → 百分比（一位小数）', () => {
    assert.equal(formatRate(1), '100.0%')
    assert.equal(formatRate(0), '0.0%')
    assert.equal(formatRate(0.125), '12.5%')
    assert.equal(formatRate(1 / 3), '33.3%')
  })

  it('null = 服务端「分母为 0」⇒ —（不是 0%）', () => {
    assert.equal(formatRate(null), '—')
    assert.equal(formatRate(undefined), '—')
    assert.equal(formatAverage(null), '—')
    assert.equal(formatAverage(undefined), '—')
  })

  it('NaN/Infinity 不是比值 ⇒ —（算式残渣永不进界面）', () => {
    assert.equal(formatRate(Number.NaN), '—')
    assert.equal(formatRate(Number.POSITIVE_INFINITY), '—')
    assert.equal(formatAverage(Number.NaN), '—')
  })

  it('均值保留一位小数（平均尝试数 3.0 不是 3）', () => {
    assert.equal(formatAverage(3), '3.0')
    assert.equal(formatAverage(1.25), '1.3')
  })
})
