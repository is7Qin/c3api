// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 快捷预设的纯逻辑用例：全部用**本地构造**的 Date 注入 now（不用 toISOString 造时刻，
// 那会把断言绑到进程时区之外的一个绝对量上），因此同一份期望在任意 TZ 下都成立。
// 运行：`node --test`（Node 内置运行器原生跑 TS，零新依赖——与 stats-capabilities.test.ts 同款）。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import { DATE_PRESET_KEYS, presetRange } from './date-presets.ts'

// —— TZ 硬门禁（进程时区无关）——
// 两个 DST 错误类的**唯一可观测条件**就是"进程跑在那个时区里"：
//   · `±86400e3` 变体只在切换日跨过跳变时漂 1h → 时间轴对齐的时区（UTC/+08）全绿；
//   · 读 `getHours()` 落字面量只在"本地当天没有 00:00"的时区错（Santiago 型缺口日）。
// 父进程时区由跑测试的人决定（CI 多为 UTC），所以下面两组断言各自**开子进程**并显式
// 指定 TZ——把它们焊在测试里，而不是靠一条没人记得跑的 `TZ=… node --test`。
const MODULE_URL = new URL('./date-presets.ts', import.meta.url).href

function presetInTZ(tz: string, key: string, localWallClock: string) {
  // `new Date('YYYY-MM-DDTHH:mm')` 无偏移 ⇒ 按**子进程**的本地时间解释，正是要的语义。
  const script = `const { presetRange } = await import(${JSON.stringify(MODULE_URL)})
process.stdout.write(JSON.stringify(presetRange(${JSON.stringify(key)}, new Date(${JSON.stringify(localWallClock)}))))`
  const out = execFileSync(process.execPath, ['--input-type=module', '-e', script], {
    env: { ...process.env, TZ: tz },
    encoding: 'utf8',
  })
  return JSON.parse(out)
}

// 两端形态：日期 + 常量 T00:00。读 getHours()/getMinutes() 的实现会在 DST 缺口日
// （America/Santiago 2026-09-06）落成 T01:00，这条正则直接挑出来。
const DAY_START = /^\d{4}-\d{2}-\d{2}T00:00$/

type Range = { from: string; to: string }

// 五个 key 的全量期望，省得每个用例重复写 map。
function allRanges(now: Date): Record<string, Range> {
  return Object.fromEntries(DATE_PRESET_KEYS.map(k => [k, presetRange(k, now)]))
}

describe('date-presets', () => {
  it('DATE_PRESET_KEYS 的顺序即按钮顺序（today/last7d/thisWeek/last30d/thisMonth）', () => {
    assert.deepEqual([...DATE_PRESET_KEYS], ['today', 'last7d', 'thisWeek', 'last30d', 'thisMonth'])
  })

  it('now = 2026-11-15T13:47（周日）五个 key 全量区间', () => {
    // 周日的关键陷阱：thisWeek 必须回退 6 天到本周一 11-09，而不是从"明天"起算。
    assert.deepEqual(allRanges(new Date(2026, 10, 15, 13, 47)), {
      today: { from: '2026-11-15T00:00', to: '2026-11-16T00:00' },
      last7d: { from: '2026-11-09T00:00', to: '2026-11-16T00:00' },
      thisWeek: { from: '2026-11-09T00:00', to: '2026-11-16T00:00' },
      last30d: { from: '2026-10-17T00:00', to: '2026-11-16T00:00' },
      thisMonth: { from: '2026-11-01T00:00', to: '2026-12-01T00:00' },
    })
  })

  it('周首日是周一：周一 00:30 与周日 23:59 给出同一个本周区间', () => {
    const monday = presetRange('thisWeek', new Date(2026, 10, 9, 0, 30))
    const sunday = presetRange('thisWeek', new Date(2026, 10, 15, 23, 59))
    assert.deepEqual(monday, { from: '2026-11-09T00:00', to: '2026-11-16T00:00' })
    assert.deepEqual(sunday, monday)
  })

  it('跨月/跨年由日历加法归一（now = 2026-12-31）', () => {
    assert.deepEqual(allRanges(new Date(2026, 11, 31, 9, 5)), {
      today: { from: '2026-12-31T00:00', to: '2027-01-01T00:00' },
      last7d: { from: '2026-12-25T00:00', to: '2027-01-01T00:00' },
      // 周四 → 偏移 3 天 → 本周一 12-28，下周一落到 2027-01-04。
      thisWeek: { from: '2026-12-28T00:00', to: '2027-01-04T00:00' },
      last30d: { from: '2026-12-02T00:00', to: '2027-01-01T00:00' },
      thisMonth: { from: '2026-12-01T00:00', to: '2027-01-01T00:00' },
    })
  })

  it('时分/毫秒不参与：同日首尾两刻给出同一组字面量', () => {
    assert.deepEqual(
      allRanges(new Date(2026, 10, 15, 0, 0, 0, 0)),
      allRanges(new Date(2026, 10, 15, 23, 59, 59, 999))
    )
  })

  it('两端形态恒为 YYYY-MM-DDTHH:mm 且时分恒 00:00', () => {
    for (const now of [
      new Date(2026, 0, 1, 0, 0),
      new Date(2026, 10, 15, 13, 47),
      new Date(2026, 5, 30, 23, 59),
      // DST 切换日也必须在形态门内（缺口日读时分分量的实现在此落地即红）。
      new Date(2026, 2, 8, 12, 0),
      new Date(2026, 10, 1, 12, 0),
      new Date(2026, 8, 6, 12, 0),
    ]) {
      for (const key of DATE_PRESET_KEYS) {
        const r = presetRange(key, now)
        assert.match(r.from, DAY_START, `${key}.from @ ${now.toDateString()}`)
        assert.match(r.to, DAY_START, `${key}.to @ ${now.toDateString()}`)
      }
    }
  })

  it('DST 切换日的日界：日期分量 = 日历算术结果，时分仍恒 00:00', () => {
    // 三个本地 DST 切换日：美东 spring-forward / fall-back、以及 America/Santiago 的
    // 00:00 缺口日（当天第一个存在时刻是 01:00）。日历加法与常量 T00:00 的口径下，
    // 三者都只是普通日历日；`±86400e3` 与 getHours() 变体分别在 ①/② 与 ③ 落地即红。
    const cases: Array<[Date, Record<string, Range>]> = [
      [
        // 2026-03-08（周日，美东 +1h）
        new Date(2026, 2, 8, 12, 0),
        {
          today: { from: '2026-03-08T00:00', to: '2026-03-09T00:00' },
          last7d: { from: '2026-03-02T00:00', to: '2026-03-09T00:00' },
          thisWeek: { from: '2026-03-02T00:00', to: '2026-03-09T00:00' },
          last30d: { from: '2026-02-07T00:00', to: '2026-03-09T00:00' },
          thisMonth: { from: '2026-03-01T00:00', to: '2026-04-01T00:00' },
        },
      ],
      [
        // 2026-11-01（周日，美东 −1h，01:00 出现两次）
        new Date(2026, 10, 1, 12, 0),
        {
          today: { from: '2026-11-01T00:00', to: '2026-11-02T00:00' },
          last7d: { from: '2026-10-26T00:00', to: '2026-11-02T00:00' },
          thisWeek: { from: '2026-10-26T00:00', to: '2026-11-02T00:00' },
          last30d: { from: '2026-10-03T00:00', to: '2026-11-02T00:00' },
          thisMonth: { from: '2026-11-01T00:00', to: '2026-12-01T00:00' },
        },
      ],
      [
        // 2026-09-06（周日，America/Santiago 本地无 00:00，当日从 01:00 起）
        new Date(2026, 8, 6, 12, 0),
        {
          today: { from: '2026-09-06T00:00', to: '2026-09-07T00:00' },
          last7d: { from: '2026-08-31T00:00', to: '2026-09-07T00:00' },
          thisWeek: { from: '2026-08-31T00:00', to: '2026-09-07T00:00' },
          last30d: { from: '2026-08-08T00:00', to: '2026-09-07T00:00' },
          thisMonth: { from: '2026-09-01T00:00', to: '2026-10-01T00:00' },
        },
      ],
    ]
    for (const [now, expected] of cases) assert.deepEqual(allRanges(now), expected)

    // 不变量（与时区无关）：'...T00:00' 按 ECMAScript 本地时间解释，缺口日落在当天
    // 第一个存在的瞬间，与 new Date(y, m, d) 同一时刻——即"日界 = 当天第一个瞬间"。
    assert.equal(new Date('2026-09-06T00:00').getTime(), new Date(2026, 8, 6).getTime())
  })

  // 上面第 7 例的真值依赖父进程时区：UTC / +08 下 `±86400e3` 与 `getHours()` 两个错误
  // 实现都能全绿（实测）。下面两例把时区固定进子进程，父进程跑在哪个时区都必须红/绿一致。
  it('TZ 硬门禁①：America/New_York 的 fall-back 日——`±86400e3` 变体落地即红', () => {
    // 2026-11-01 美东 fall-back：把"次日日界"算成 base + 86400e3 会得到当日 23:00，
    // 日期分量退回 11-01（to 塌成 from）——正是这条断言要挡的窗口错位。
    assert.deepEqual(presetInTZ('America/New_York', 'today', '2026-11-01T12:00'), {
      from: '2026-11-01T00:00',
      to: '2026-11-02T00:00',
    })
  })

  it('TZ 硬门禁②：America/Santiago 的 00:00 缺口日——`getHours()` 变体落地即红', () => {
    // 2026-09-06 本地没有 00:00（当天从 01:00 起）：按小时分量落字面量会写出 T01:00。
    assert.deepEqual(presetInTZ('America/Santiago', 'today', '2026-09-06T12:00'), {
      from: '2026-09-06T00:00',
      to: '2026-09-07T00:00',
    })
  })

  it('TZ 硬门禁③：Pacific/Apia 整日被跳过（2011-12-30 不存在）——不产出零宽窗口', () => {
    // 本地 2011-12-30 整天不存在：`new Date(2011, 11, 30)` 归一为 12-31，因此 y/m/d
    // 一律从**归一后**的 now 读取，区间变成 [12-31, 2012-01-01) 而不是 [12-31, 12-31)。
    assert.deepEqual(presetInTZ('Pacific/Apia', 'today', '2011-12-30T12:00'), {
      from: '2011-12-31T00:00',
      to: '2012-01-01T00:00',
    })
  })
})
