// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 计划读面的「取全」helper。
//
// `/routing/plan` 的 `routes` 是服务端**一页**（缺省 20 条、上限 200），而需要「整个
// 当前计划」的调用方只读第一页会**静默漏掉**第 20 条之后路由上的内容——Ops 事故面板
// 正是这种调用方：它筛 `incident.active` 列活跃事故，漏页即漏报事故。
//
// 计划是**不可变快照且带 `generation`**，故页间代际不一致意味着两页不属于同一版本
// （拼接会得到现实中不存在的路由组合）。此时停止拼接并返回已取部分：调用方按
// `refetchInterval` 轮询，下一次会拿到自洽快照。该降级路径只在计划超过一页上限
// （>200 条路由）且恰逢重编译时才会走到。

/** 与 openapi 契约一致：三只读端点的 `limit` 上限均为 200。 */
export const ROUTING_PLAN_PAGE_MAX = 200

export type RoutingPlanPage<T> = {
  generation: number
  routes: T[]
  total_routes: number
}

/**
 * 逐页取全计划路由。返回首帧的元数据（`generation`）与拼接后的 `routes`。
 *
 * 传入的 `fetchPage` 必须只按 `offset`/`limit` 取页（其余参数由调用方闭包固定）。
 */
export async function fetchAllRoutingPlanRoutes<T>(
  fetchPage: (p: { offset: number; limit: number }) => Promise<RoutingPlanPage<T>>,
): Promise<RoutingPlanPage<T>> {
  const first = await fetchPage({ offset: 0, limit: ROUTING_PLAN_PAGE_MAX })
  const routes = [...first.routes]
  while (routes.length < first.total_routes) {
    const next = await fetchPage({ offset: routes.length, limit: ROUTING_PLAN_PAGE_MAX })
    // 代际变了 = 取页期间计划重编译，两页不属于同一版本。
    if (next.generation !== first.generation) break
    // 空页而 total 未达 = 计划在取页期间收缩；以已取为准，不空转。
    if (next.routes.length === 0) break
    routes.push(...next.routes)
  }
  return { ...first, routes }
}
