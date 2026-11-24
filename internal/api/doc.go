// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package api 是数据面（internal/proxy）与控制面（internal/scheduler）之间的中立契约。
//
// 它只有三样东西：
//
//   - Selection：一次派发的**具体结构**，只读事实全是字段（实测 20 个；数据面直读、
//     零成本）。它不是控制面对象——私有租约/会话状态在 Selection.Handle 背后；
//     HandleOrDetached 是数据面唯一的句柄取值口（nil / 零值 Selection ⇒ 分离句柄）。
//   - SelectionHandle：控制面私有句柄，恰好 4 个方法（Release/Thread/StepIdentity/
//     Credential）。数据面只经它触碰生命周期，别的一概不暴露。
//   - AttemptPlan：计划的窄消费面，恰好 2 个方法（ContinuationIncompatible/
//     AbandonLastAttempt）。ReserveAttempt 属控制面编排，不在计划接口上。
//
// 依赖方向是单向的：本包**不 import** internal/scheduler（门禁 F2 机械强制）。
// 控制面把 Selection 别名为自己的类型并向数据面提供 handle 实现；数据面只认本包。
package api
