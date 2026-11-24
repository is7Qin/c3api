// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package api

import "errors"

// 运行期选号失败的五条哨兵（P3c I-B）：**canonical 定义在本包**，控制面
// （internal/scheduler）只留 `var ErrX = api.ErrX` 别名，数据面（internal/proxy）
// 的生产返回点与判定点一律指本包。
//
// 为什么归契约层：这不是"控制面的私事"。生产方在控制面（选号 / 预留 / 计划推进），
// 判定方在数据面（HTTP 状态与是否重试），跨包判等用的是 error 的**值身份**，不是文案。
// 两枚"文案相同的独立 error"会让 `errors.Is` 静默失配，而 `statusFor` 的 default
// 恰好是 429 + "group not found"——错得最像"组不存在"，最难查。
// 故值身份只能有一个 owner，owner 必须是两头都能看见的那一层。
//
// 文案里的 `scheduler:` 前缀是**历史日志口径**，不随归属迁移更改：现有日志检索、
// `logx.Error` 输出与排障手册都按它匹配。想改文案是另一件事，别顺手做——那会让本次
// 迁移的 diff 里混进一个与错误分类无关的行为变更。
var (
	// ErrGroupNotFound 组不在静态视图里（或控制面根本没接线：scheduler 为 nil）。
	// 数据面映射 404。
	ErrGroupNotFound = errors.New("scheduler: group not found")

	// ErrFormatUnavailable 组可路由，但**没有任何账号支持该请求格式/模型**——
	// 这是"不可路由"（静态桶里根本没有），不是"暂时没空位"：不重试，映射 404。
	ErrFormatUnavailable = errors.New("scheduler: no account for request format")

	// ErrPlanNotReady is the compile-lag sentinel: the group exists in the
	// static view but no compiled decision has published yet (static staged
	// ahead of the compile lane, e.g. cold start before the first paired
	// publish). Distinct from ErrFormatUnavailable (genuinely unsupported
	// format/model — no static bucket, never routable): not-ready retries
	// after ~1s (503 + Retry-After), unroutable does not (404).
	ErrPlanNotReady = errors.New("scheduler: routing plan not ready")

	// ErrNoAvailable 组与格式都可路由，但所有候选账号此刻都不可用（并发槽满 /
	// 冷却中）。"再试可能成"，数据面映射 429（+ Retry-After）。
	ErrNoAvailable = errors.New("scheduler: no available account")

	// ErrAttemptsExhausted 本次计划的尝试次数用尽（ordinal 到顶 / 预留一律被拒）。
	// 与 ErrNoAvailable 只差成因（计划侧 vs 账号侧）：HTTP 面同为 429，
	// 但文案与日志必须可区分（数据面 selectErrorMessage 先判它）。
	ErrAttemptsExhausted = errors.New("scheduler: attempt plan exhausted")
)
