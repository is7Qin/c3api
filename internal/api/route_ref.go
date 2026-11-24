// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package api

// RouteRef identifies a compiled route: group + format + model + operation + canonical RouteClassID.
//
// 只搬结构体，不搬构造/归一函数（P3c II）：RouteRefFor / RouteRefForOp /
// normRouteRef / canonicalRouteRefWithOp 留在 internal/scheduler —— 它们依赖
// operationTagForFormat / routeKey 这类**路由编译器事实**，搬进来就等于让契约层
// 反向知道路由怎么编译。scheduler 侧另有 `type RouteRef = api.RouteRef` 别名，
// 故 RouteRefFor(...) 的返回类型与本结构体是**同一个类型**（不是第二份身份），
// 调用点 `scheduler.RouteRefFor(...)` 与 `api.RouteRef{...}` 混写在类型层面无歧义。
type RouteRef struct {
	GroupID      int64
	Format       string
	Model        string
	OperationTag string
	RouteClassID string
}
