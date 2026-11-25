// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import "github.com/is7qin/c3api/internal/api"

// 运行期选号哨兵的**包内拼写**：五条 canonical 定义在 internal/api/errors.go
// （生产方与判定方共同依赖的那一层），本文件只做同值别名。
//
// 为什么必须是别名而不是重新构造：`errors.Is` 判的是 error 的**值身份**。若这里写成
// `errors.New("scheduler: …")`，数据面（proxy 的 404/429/503 映射面、协议转换判定）
// 判到的就是另一枚值——未命中的哨兵会落进 `statusFor` 的 default（429 + "group not
// found"），HTTP 语义静默改变。写成 `fmt.Errorf("%w", api.ErrX)` 更隐蔽：双向
// `errors.Is` 都成立、端到端断言全绿，但 `==` 与 `%v` 文案都变了。
//
// P5 收口后本文件**不再是"迁移期兼容层"**：删 errors_compat.go 之前它是"scheduler 侧还有旧拼写调用点"
// 的过渡；现在旧拼写已被清零——数据面（internal/proxy）生产代码与测试一律直指
// `api.ErrX`（见 internal/proxy/runtime_sentinel_scope_gate_test.go，
// internal/proxy/error_mapping_test.go 直接拿 api 哨兵喂映射函数）。剩下这五个
// `var` 是 scheduler **包内**的生产拼写，唯一职责是让"同一 error 值"这件事有一个
// 声明点可被机械断言。
//
// 判据（都在本单内，不是口头约定）：
//   - internal/architecture/sentinel_single_source_gate_test.go：全仓每个哨兵恰好一处
//     canonical（api ×5）、别名只许 `var ErrX = api.ErrX` 形状，且总数仍钉死为 16；
//   - internal/proxy/runtime_sentinel_scope_gate_test.go：数据面生产代码零
//     `scheduler.ErrX` 引用（返回点与判定点一律 api.*，不许半搬）。
//
// ErrEgressNotWired **不在这里**：它是装配违例（启动期事实，见 scheduler.go），
// 不是运行期选号分类，数据面从不判它。
//
// 移除条件（可执行；条件未满足前**禁止删除**）：把这三个文件里的短名全部改写成
// `api.ErrX`（包内调用点一并改），并同步删除 sentinelAliasTable 里对应的 5 行、
// 把 sentinelTotalDecls 从 16 改到 11：
//
//	rg -n '(?<![\w.])Err(GroupNotFound|FormatUnavailable|PlanNotReady|NoAvailable|AttemptsExhausted)\b' \
//	  internal/scheduler/attempt_plan_reservation.go internal/scheduler/attempt_plan_exec.go \
//	  internal/scheduler/attempt_plan_*_test.go
var (
	ErrGroupNotFound     = api.ErrGroupNotFound
	ErrFormatUnavailable = api.ErrFormatUnavailable
	ErrPlanNotReady      = api.ErrPlanNotReady
	ErrNoAvailable       = api.ErrNoAvailable
	ErrAttemptsExhausted = api.ErrAttemptsExhausted
)
