// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// mapWithConcurrency 的性质断言（spec batch-probe §1.3.1）。全部用**计数探针 + 微任务让出**
// 制造交错，不靠 sleep 计时（计时断言在 CI 上会抖，也证明不了并发上限）。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { mapWithConcurrency } from './concurrency.ts'
import { PROBE_CONCURRENCY } from './proxy-form.ts'

describe('mapWithConcurrency（有界并发映射）', () => {
  it('limit 非正数是调用方 bug：抛错（不静默当 1，也不返回空）', async () => {
    for (const bad of [0, -1, -100, Number.NaN]) {
      await assert.rejects(
        () => mapWithConcurrency([1, 2, 3], bad, async n => n),
        /limit must be >= 1/,
        `limit=${String(bad)} 必须抛错`,
      )
    }
  })

  it('空输入 ⇒ []，且一个 fn 都不调', async () => {
    let calls = 0
    const out = await mapWithConcurrency([], 4, async () => {
      calls++
      return 1
    })
    assert.deepEqual(out, [])
    assert.equal(calls, 0)
  })

  it('结果与输入同序：完成顺序刻意倒过来，输出仍按原下标', async () => {
    const release: Array<() => void> = []
    const pending = mapWithConcurrency([0, 1, 2, 3], 4, async n => {
      // 每个 worker 先挂起，由测试按**倒序**放行 ⇒ 若按完成顺序收集，结果会反过来。
      await new Promise<void>(done => { release[n] = done })
      return n * 10
    })
    // worker 池在 Promise.all 之前同步建好 ⇒ 此刻四个放行句柄都已就位（无需等待/计时）。
    assert.equal(release.length, 4)
    for (let i = release.length - 1; i >= 0; i--) release[i]()
    assert.deepEqual(await pending, [0, 10, 20, 30])
  })

  it('★ 并发峰值：100 项 + limit=4 ⇒ 恰为 4（不是 1，也不是 100）', async () => {
    const items = Array.from({ length: 100 }, (_, i) => i)
    let active = 0
    let peak = 0
    const out = await mapWithConcurrency(items, PROBE_CONCURRENCY, async n => {
      active++
      peak = Math.max(peak, active)
      await Promise.resolve() // 只让出微任务制造交错，不计时
      active--
      return n
    })
    // 两条断言各有靶子：上限被压成 1 或放开到 100 ⇒ 第一条红；
    // 常量本身被改（4 不再是 4）⇒ 第二条红。
    assert.equal(peak, PROBE_CONCURRENCY, '峰值必须等于上限')
    assert.equal(peak, 4, 'PROBE_CONCURRENCY 恒为 4')
    assert.deepEqual(out, items)
  })

  it('单条 fn 抛错 ⇒ 整体拒绝（错误处理属于调用方，本函数不包装）', async () => {
    await assert.rejects(
      () => mapWithConcurrency([1, 2, 3], 2, async n => {
        if (n === 2) throw new Error('boom')
        return n
      }),
      /boom/,
    )
  })
})
