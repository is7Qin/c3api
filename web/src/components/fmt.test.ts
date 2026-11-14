// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// defaultLogRange 的行为门禁。**形状门禁抓不到"上界该不该落在未来、下界该不该一起
// 后移"**——两种写法都编译通过、都"看起来合理"，所以只能测语义：
// ① 上界晚于此刻且不早于下一个整点；② 跨度恒 24h（下界由上界派生 ⇒ 整体后移 1h）；
// ③ 输出恒为本地墙钟字面量、窗口不塌缩。
// 运行：`node --test`（Node 内置运行器原生跑 TS，零新依赖）。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { defaultLogRange } from './fmt.ts'

// 与 internal/domain/statsplan.go 的 ceilHour 同义（整点网格向上取整；秒/毫秒分量
// 参与进位，与 Go 侧"先按纳秒分量进位再整点取整"逐位等价）。
const ceilHour = (ms: number) => Math.ceil(ms / 3_600_000) * 3_600_000

// 本地墙钟字面量 → 毫秒。无偏移的 date-time 按**本地时间**解释（ECMAScript
// Date Time String Format），与 toRFC3339 的解析口径一致。
const parseLocal = (v: string) => new Date(v).getTime()

const HOUR = 3600 * 1000
const DAY = 24 * HOUR

// 本进程时区在给定瞬间的偏移（分钟）。用来判定窗口内是否有 DST 跳变——有跳变时
// "墙钟字面量 → 绝对时刻"的往返才可能差 1h，无跳变时必须精确 24h。
const offsetMin = (ms: number) => new Date(ms).getTimezoneOffset()
const hasTransition = (t0: number) =>
  new Set([offsetMin(t0 - DAY), offsetMin(t0), offsetMin(t0 + HOUR)]).size > 1

describe('defaultLogRange', () => {
  it('上界晚于此刻，且不早于下一个整点', () => {
    const t0 = Date.now()
    const { to } = defaultLogRange(t0)
    const toMs = parseLocal(to)
    assert.ok(toMs > t0, `to=${to} 必须晚于此刻（否则当前分钟的记录被截断在窗外）`)
    // 上界 = 此刻 + 1h（分钟截断只向下取整，落点 ∈ (now+59min, now+60min]）⇒ 恒不早于
    // ⌈now⌉h，与账号「用量明细」的窗口上界落在同一小时。
    assert.ok(toMs >= ceilHour(t0), `to=${to} 必须不早于下一个整点 ${new Date(ceilHour(t0)).toISOString()}`)
    assert.ok(toMs <= t0 + HOUR, `to=${to} 不得超出 此刻+1h`)
  })

  it('跨度恒 24h：无 DST 跳变时**精确** 24h（"只把上界加一小时"的变体会给 25h）', () => {
    const t0 = Date.now()
    const { from, to } = defaultLogRange(t0)
    const fromMs = parseLocal(from)
    const span = parseLocal(to) - fromMs
    if (hasTransition(t0)) {
      // 窗口含 DST 跳变：墙钟字面量往返可能差 1h（23h/25h），这是表示法的边界。
      assert.ok(Math.abs(span - DAY) <= HOUR, `DST 窗口跨度只能是 24h±1h：from=${from} to=${to} span=${span}ms`)
    } else {
      assert.equal(span, DAY, `无 DST 跳变必须精确 24h：from=${from} to=${to} span=${span}ms`)
    }
    assert.ok(span > 0, `窗口不得塌缩或反向：from=${from} to=${to}`)
    // 整体后移：from ∈ (此刻 − 23h − 1min, 此刻 − 23h]（分钟截断最多吞掉 60s）。
    assert.ok(fromMs > t0 - 24 * HOUR, `from=${from} 必须晚于 此刻−24h（窗口整体后移）`)
    assert.ok(fromMs > t0 - 23 * HOUR - 61_000, `from=${from} 与 此刻−23h 的偏差不得超过一分钟`)
    assert.ok(fromMs <= t0 - 23 * HOUR, `from=${from} 不得晚于 此刻−23h`)
  })

  it('边界时钟：整点、整点 +1ms、分钟末都满足"上界 ∈ [⌈now⌉h, now+1h]"', () => {
    // 真实时钟撞不到这些点；注入固定 nowMs 才能把它们变成确定性用例。
    const base = new Date(2026, 10, 15, 3, 0, 0, 0).getTime() // 本地 03:00:00.000
    for (const nowMs of [base, base + 1, base + 59_999, base + 30 * 60 * 1000]) {
      const { from, to } = defaultLogRange(nowMs)
      const toMs = parseLocal(to)
      assert.ok(toMs >= ceilHour(nowMs), `now=${new Date(nowMs).toISOString()} to=${to} 必须 ≥ ⌈now⌉h`)
      assert.ok(toMs <= nowMs + HOUR, `now=${new Date(nowMs).toISOString()} to=${to} 必须 ≤ now+1h`)
      assert.ok(parseLocal(from) < toMs, `now=${new Date(nowMs).toISOString()} 窗口必须非空`)
    }
  })

  it('输出是本地墙钟字面量（无时区后缀）', () => {
    const { from, to } = defaultLogRange()
    for (const v of [from, to]) {
      assert.match(v, /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/, `${v} 必须是 datetime-local 形态`)
    }
  })
})
