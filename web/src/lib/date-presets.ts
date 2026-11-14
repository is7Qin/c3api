// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 时间范围快捷预设的纯逻辑：UI 只负责渲染按钮，日期算术全部收在这里。
//
// 三条硬约束（任一条放松都会在真实时区里落地即错，2026-11-15 设计实测）：
//  1. 只用日历加法 `new Date(y, m, d + n)` / `new Date(y, m + 1, 1)`——跨月跨年交给
//     Date 归一，不手写 12 月分支。禁用 `±86400e3`：它加的是 24h 绝对时长，而日界是
//     墙钟概念，DST 切换日会漂 1h（TZ=America/New_York 实测：now − 86400e3 在 2026-11-01
//     fall-back 落到墙钟 13:00，now + 86400e3 在 2026-03-08 spring-forward 落到 12:00）。
//  2. 字面量由**归一后的日历字段 + 常量 `T00:00`** 拼出，禁止读 getHours()/getMinutes()。
//     America/Santiago 2026-09-06 本地不存在 00:00（当天从 01:00 起），
//     `new Date(2026, 8, 6).getHours()` 实测为 1 —— 按小时分量落字面量会得到 `T01:00`。
//     这也是**不复用 fmt.ts 的 local()** 的原因：那个函数按"此刻的时分"落字面量，
//     本模块要的是"整日界"，两者不是同一规则的两次调用（自带 3 行 pad2 更便宜）。
//  3. 禁止 toISOString() 往返：它输出 UTC 表示（本地 00:00 → 前一天 16:00），而线缆格式
//     只有一套——无偏移、无秒的本地墙钟 `YYYY-MM-DDTHH:mm`，绝对化只在 toRFC3339 一处。

// 顺序即按钮顺序，也是键集与顺序的唯一来源（不在别处手写第二份）。
export const DATE_PRESET_KEYS = ['today', 'last7d', 'thisWeek', 'last30d', 'thisMonth'] as const
export type DatePresetKey = (typeof DATE_PRESET_KEYS)[number]

const pad2 = (n: number) => String(n).padStart(2, '0')

// 归一后的 Date → 该日历日的日界字面量。只读日历字段（y/m/d），时分恒取常量。
function dayStartLiteral(d: Date): string {
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}T00:00`
}

// 每个预设只声明"两端日界是哪两个日历日"（用日历加法表达，跨月跨年由 Date 归一），
// 落字面量统一走 dayStartLiteral——DST 缺口的两种表现只在这一处被吸收掉。
// 表驱动而非 switch：`Record<DatePresetKey, …>` 少一个键 tsc 直接报错（switch 缺
// return 只会得到 undefined，编译期更弱），新增 key 也不必回头改 presetRange。
const PRESET_BOUNDS: Record<DatePresetKey, (now: Date) => { from: Date; to: Date }> = {
  // 今天：[当天 00:00, 次日 00:00)
  today: now => {
    const y = now.getFullYear()
    const m = now.getMonth()
    const d = now.getDate()
    return { from: new Date(y, m, d), to: new Date(y, m, d + 1) }
  },
  // 近 7 天：[当天 − 6 天 00:00, 次日 00:00)——含今天共 7 个日历日（不是 7×24h）
  last7d: now => {
    const y = now.getFullYear()
    const m = now.getMonth()
    const d = now.getDate()
    return { from: new Date(y, m, d - 6), to: new Date(y, m, d + 1) }
  },
  // 本周：[本周一 00:00, 下周一 00:00)。周首日 = 周一：getDay() 里周日是 0，
  // 加 6 再取模把它映射到偏移 6（周一 → 0、周日 → 6）。
  thisWeek: now => {
    const y = now.getFullYear()
    const m = now.getMonth()
    const d = now.getDate()
    const offset = (now.getDay() + 6) % 7
    return { from: new Date(y, m, d - offset), to: new Date(y, m, d - offset + 7) }
  },
  // 近 30 天：[当天 − 29 天 00:00, 次日 00:00)——含今天共 30 个日历日
  last30d: now => {
    const y = now.getFullYear()
    const m = now.getMonth()
    const d = now.getDate()
    return { from: new Date(y, m, d - 29), to: new Date(y, m, d + 1) }
  },
  // 本月：[本月 1 日 00:00, 下月 1 日 00:00)——下月由 m + 1 归一（12 月 → 次年 1 月）
  thisMonth: now => {
    const y = now.getFullYear()
    const m = now.getMonth()
    return { from: new Date(y, m, 1), to: new Date(y, m + 1, 1) }
  },
}

/**
 * 预设区间（本地墙钟，含头不含尾，两端时分恒 00:00）。
 * `now` 供测试与"渲染期不读时钟"的调用点注入确定性时刻；缺省取此刻。
 */
export function presetRange(key: DatePresetKey, now: Date = new Date()): { from: string; to: string } {
  const { from, to } = PRESET_BOUNDS[key](now)
  return { from: dayStartLiteral(from), to: dayStartLiteral(to) }
}
