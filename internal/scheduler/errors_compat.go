// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import "github.com/is7qin/c3api/internal/api"

// 迁移期兼容层（P3c I-B）：五条运行期选号哨兵的 canonical 定义已下沉到
// internal/api/errors.go（生产方与判定方共同依赖的那一层）。本文件只剩**别名**，
// 一个 `errors.New` / `fmt.Errorf` 都不许有。
//
// 为什么必须是别名而不是重新构造：`errors.Is` 判的是 error 的**值身份**。若这里写成
// `errors.New("scheduler: …")`，数据面（proxy/forward.go 的 404/429/503 映射面、
// proxy/caller.go 的协议转换判定）判到的就是另一枚值——未命中的哨兵会落进
// `statusFor` 的 default（429 + "group not found"），HTTP 语义静默改变。
// 写成 `fmt.Errorf("%w", api.ErrX)` 更隐蔽：双向 `errors.Is` 都成立、现有 75 条
// 端到端断言全绿，但 `==` 与 `%v` 文案都变了。故这五条必须是**同一 error 值**。
//
// 判据（都在本单内，不是口头约定）：
//   - internal/scheduler/errors_compat_test.go：`scheduler.ErrX == api.ErrX`、
//     双向 `errors.Is`、逐字文案、无 wrap（`errors.Unwrap` 必须为 nil）。
//   - internal/proxy/runtime_sentinel_scope_gate_test.go：数据面**生产**代码零
//     `scheduler.ErrX` 引用（返回点与判定点一律 api.*，不许半搬）。
//
// ErrEgressNotWired **不在这里**：它是装配违例（启动期事实，见 scheduler.go），
// 不是运行期选号分类，数据面从不判它。
//
// 移除条件（可执行；条件未满足前**禁止删除**）：
//
//	rg -n "scheduler\.(ErrGroupNotFound|ErrFormatUnavailable|ErrPlanNotReady|ErrNoAvailable|ErrAttemptsExhausted)" --glob '!internal/scheduler/**'
//
// 生产代码此刻已全部改指 api；残留命中只在 internal/proxy 的测试里（它们用旧拼写
// 喂映射函数，语义等价）——那正是本别名要继续存在的理由。等测试也改指 api 后，
// 删除本文件即可（数据面的 import 不会因此变化）。
var (
	ErrGroupNotFound     = api.ErrGroupNotFound
	ErrFormatUnavailable = api.ErrFormatUnavailable
	ErrPlanNotReady      = api.ErrPlanNotReady
	ErrNoAvailable       = api.ErrNoAvailable
	ErrAttemptsExhausted = api.ErrAttemptsExhausted
)
