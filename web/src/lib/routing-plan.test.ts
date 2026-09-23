// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// `fetchAllRoutingPlanRoutes` 的纯逻辑用例：注入假 fetchPage，不触网。
// 运行：`node --test`（Node 内置运行器原生跑 TS，零新依赖）。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { fetchAllRoutingPlanRoutes, ROUTING_PLAN_PAGE_MAX } from './routing-plan.ts'

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
