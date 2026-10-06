// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// `fetchAllRoutingPlanRoutes` 的纯逻辑用例：注入假 fetchPage，不触网。
// 运行：`node --test`（Node 内置运行器原生跑 TS，零新依赖）。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { fetchAllRoutingPlanRoutes, OBSERVED_WINDOW_MAX_SECONDS, observedWindowQuery, routingContentState, ROUTING_PLAN_PAGE_MAX } from './routing-plan.ts'

type Route = { id: number }

// fake 服务端：routes 为 1..total，按 offset/limit 切片。
function fakeServer(total: number, generation = 7) {
  const calls: Array<{ offset: number; limit: number }> = []
  const fetchPage = async (p: { offset: number; limit: number }) => {
    calls.push(p)
    const all: Route[] = Array.from({ length: total }, (_, i) => ({ id: i + 1 }))
    return { generation, routes: all.slice(p.offset, p.offset + p.limit), total_routes: total }
  }
  return { fetchPage, calls }
}

describe('fetchAllRoutingPlanRoutes', () => {
  it('单页计划 → 只发一次请求，路由原样返回', async () => {
    const { fetchPage, calls } = fakeServer(60)
    const page = await fetchAllRoutingPlanRoutes<Route>(fetchPage)
    assert.equal(page.routes.length, 60)
    assert.deepEqual(calls, [{ offset: 0, limit: ROUTING_PLAN_PAGE_MAX }])
  })

  it('空计划 → generation 0、routes []，不空转', async () => {
    const { fetchPage, calls } = fakeServer(0, 0)
    const page = await fetchAllRoutingPlanRoutes<Route>(fetchPage)
    assert.deepEqual(page.routes, [])
    assert.equal(page.generation, 0)
    assert.equal(calls.length, 1)
  })

  it('多页计划 → 按 total_routes 取全，无重复无缺口', async () => {
    const n = ROUTING_PLAN_PAGE_MAX * 2 + 5
    const { fetchPage, calls } = fakeServer(n)
    const page = await fetchAllRoutingPlanRoutes<Route>(fetchPage)
    assert.equal(page.routes.length, n)
    assert.deepEqual(
      calls.map(c => c.offset),
      [0, ROUTING_PLAN_PAGE_MAX, ROUTING_PLAN_PAGE_MAX * 2],
    )
    assert.deepEqual(
      page.routes.map(r => r.id),
      Array.from({ length: n }, (_, i) => i + 1),
    )
  })

  it('页间代际变化 → 停止拼接，返回首帧元数据 + 已取部分（不产出现实不存在的组合）', async () => {
    const all: Route[] = Array.from({ length: 250 }, (_, i) => ({ id: i + 1 }))
    let call = 0
    const fetchPage = async (p: { offset: number; limit: number }) => {
      call++
      // 第二页起报告新代际：模拟取页期间重编译。
      const generation = call === 1 ? 7 : 8
      return { generation, routes: all.slice(p.offset, p.offset + p.limit), total_routes: all.length }
    }
    const page = await fetchAllRoutingPlanRoutes<Route>(fetchPage)
    assert.equal(page.generation, 7)
    assert.equal(page.routes.length, ROUTING_PLAN_PAGE_MAX)
    assert.deepEqual(
      page.routes.map(r => r.id),
      Array.from({ length: ROUTING_PLAN_PAGE_MAX }, (_, i) => i + 1),
    )
  })

  it('计划在取页期间收缩 → 空页即停，不空转', async () => {
    let call = 0
    const fetchPage = async (p: { offset: number; limit: number }) => {
      call++
      if (call === 1) {
        return { generation: 7, routes: [{ id: 1 }, { id: 2 }], total_routes: 300 }
      }
      return { generation: 7, routes: [], total_routes: 2 }
    }
    const page = await fetchAllRoutingPlanRoutes<Route>(fetchPage)
    assert.equal(page.routes.length, 2)
    assert.equal(call, 2)
  })
})

describe('observedWindowQuery', () => {
  const local = (min: number) => {
    const d = new Date(2026, 0, 1, 0, 0, 0)
    d.setMinutes(d.getMinutes() + min)
    const pad = (n: number) => String(n).padStart(2, '0')
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
  }

  it('合法窗口 → 成对 RFC3339（本地串按浏览器时区解析）', () => {
    const q = observedWindowQuery({ from: local(0), to: local(60) })
    assert.equal(q.from, new Date(2026, 0, 1, 0, 0, 0).toISOString())
    assert.equal(q.to, new Date(2026, 0, 1, 1, 0, 0).toISOString())
  })

  it('恰 90d → 给对；>90d → 两端都省略（绝不发半窗口）', () => {
    const spanMinutes = (OBSERVED_WINDOW_MAX_SECONDS / 60)
    assert.deepEqual(
      Object.keys(observedWindowQuery({ from: local(0), to: local(spanMinutes) })).sort(),
      ['from', 'to'],
    )
    const over = observedWindowQuery({ from: local(0), to: local(spanMinutes + 1) })
    assert.deepEqual(over, {})
    assert.equal(over.from, undefined)
    assert.equal(over.to, undefined)
  })

  it('from ≥ to（含相等）→ 两端都省略', () => {
    assert.deepEqual(observedWindowQuery({ from: local(60), to: local(0) }), {})
    assert.deepEqual(observedWindowQuery({ from: local(30), to: local(30) }), {})
  })

  it('任一端非法/空 → 两端都省略（不发半窗口）', () => {
    assert.deepEqual(observedWindowQuery({ from: '', to: local(60) }), {})
    assert.deepEqual(observedWindowQuery({ from: local(0), to: 'not-a-date' }), {})
    assert.deepEqual(observedWindowQuery({ from: 'not-a-date', to: local(60) }), {})
  })
})

describe('routingContentState', () => {
  const base = { loading: false, error: false, planTotal: 3, observedCount: 2, search: '' }

  it('loading 优先于一切', () => {
    assert.equal(routingContentState({ ...base, loading: true }), 'loading')
    assert.equal(routingContentState({ ...base, loading: true, error: true, planTotal: 0 }), 'loading')
  })

  it('error 次之（顶部 Card 仍由页面保留）', () => {
    assert.equal(routingContentState({ ...base, error: true }), 'error')
    assert.equal(routingContentState({ ...base, error: true, planTotal: 0 }), 'error')
  })

  it('planTotal=0 → empty-plan（不分有无搜索）', () => {
    assert.equal(routingContentState({ ...base, planTotal: 0, observedCount: 0, search: 'x' }), 'empty-plan')
    assert.equal(routingContentState({ ...base, planTotal: 0, observedCount: 0, search: '' }), 'empty-plan')
  })

  it('planTotal>0 且窗口内 0 有流量：有搜索 → no-match，无搜索 → no-traffic', () => {
    assert.equal(routingContentState({ ...base, observedCount: 0, search: 'gpt' }), 'no-match')
    assert.equal(routingContentState({ ...base, observedCount: 0, search: '' }), 'no-traffic')
  })

  it('observedCount>0 → cards', () => {
    assert.equal(routingContentState({ ...base, observedCount: 1, search: 'gpt' }), 'cards')
    assert.equal(routingContentState({ ...base, observedCount: 5, search: '' }), 'cards')
  })
})
