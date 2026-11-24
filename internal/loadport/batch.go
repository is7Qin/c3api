// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package loadport 是**装载端口**：控制面（internal/scheduler）向存储实现
// （internal/repository）索要账号快照时使用的中立契约。
//
// 为什么单独成包、而不与 internal/api 同居（P5-1 裁决 A）：
//   - 语义不同向。api 是"数据面 ↔ 控制面"之间的契约（Selection / SelectionHandle /
//     AttemptPlan）；本包是"控制面 ↔ 存储实现"之间的契约。两者方向不同，不该共用
//     一个包名去解释。
//   - 硬约束：api 自己 import internal/sdkbridge，而 sdkbridge import internal/repository
//     ⇒ `repository → api` 是 Go 导入环（编译期直接失败）。装载端口的实现方必须能
//     import 契约，故契约必须住在**叶子包**里。
//
// 边界：只依赖 internal/domain 与 internal/credential。**不**依赖 repository
// （否则环回来）、**不**依赖 scheduler（能力由消费方描述，实现方无需 import 它——
// 同 AccountUniverseLoader / AccountExtMismatchSource 的方向）。
package loadport

import (
	"context"

	"github.com/is7qin/c3api/internal/domain"
)

// GroupAccountBatch 是一次**载入批次**的会话：同一个 template_id 在整批内只转换一枚
// *domain.Template（U22 §2.1），生命周期由调用方按批次持有。
//
// 为什么是"会话"而不是"把作用域传进来"：模板指针必须**跨组唯一**，而"跨组唯一"只有
// 在"一张表贯穿整批"时才成立。把表的所有权交给会话对象（而不是让调用方拿着作用域
// 逐个组传参），使"同一模板一枚指针"由实现方保证——调用方写不出"每组新建一张表"的
// 退化形态（它手上根本没有建表的入口）。
//
// 没有 Close/Release：批次背后只有一张 map（repository.TemplateLoadScope 无资源、
// 无 goroutine、无 fd），词法寿命即足够。加显式关闭只会制造一个"忘了关"的协议。
// 非并发安全：调用方保证同一批次串行使用（生产上 reload / invalidate 都在调度器锁内）。
type GroupAccountBatch interface {
	// LoadGroup 装载一个组的账号。同一批次内重复调用必须复用同一枚 *domain.Template
	// （同 template_id ⇒ 同一指针）；批次的模板表跨组累积，不随单组载入重置。
	LoadGroup(ctx context.Context, groupID int64) ([]*domain.Account, error)
}

// BatchedGroupLoader 是装载器的**可选能力**。
//
// **不支持批次的装载源不要实现本接口。** "不支持"的唯一合法信号是**类型断言失败**
// （`loader.(loadport.BatchedGroupLoader)` 的 ok 为 false），不是"实现了但返回 nil"
// ——后者把一次静态可判的能力缺失变成运行期分支，而那个分支在实现侧没有任何回报。
//
// 实现约束（两条都是违约，不是"建议"）：
//  1. NewBatch 必须返回**非 nil**且**非 typed-nil** 的批次。调用方的探针只检查
//     `b == nil`，抓不到 `var p *repoBatch; var b loadport.GroupAccountBatch = p` 这种
//     typed-nil（接口非 nil、底层指针 nil）；typed-nil 只会在第一次 LoadGroup 时
//     panic。故"不得返回 typed-nil"由**实现侧用例**钉住，不在调用方加反射判空
//     （那是把实现违约的开销摊到每次失效）。
//  2. 每次 NewBatch 必须返回**独立**的批次（独立模板表）。共享表会让上一个批次的
//     模板对象漏进下一个批次：跨批次复用同一张表 ⇒ 模板内容改动不生效，且旧对象
//     存活期被无限拉长。
type BatchedGroupLoader interface {
	NewBatch() GroupAccountBatch
}
