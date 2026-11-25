// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/identityfence"
)

// 候选身份指纹与三个身份围栏哨兵的 canonical 定义住在 internal/identityfence
// （只依赖 domain）。本文件只剩两个**本包的取指纹入口**：`candidateFingerprint`
// （包内使用）与 `CandidateFingerprint`（历史导出名，转发）。它们不是兼容层：删它们等于
// 把 scheduler 的取指纹入口整个挪走。
//
// P5 收口：这里曾有三个哨兵别名（`var ErrMissingCandidateFingerprint =
// identityfence.Err…`），现已删——本包与数据面一律直接引用
// `identityfence.Err…`，跨包 `errors.Is` 靠的是**同一个 canonical error 值**，不是别名层。
// 归属仍由 internal/identityfence/ownership_gate_test.go 与
// internal/architecture/sentinel_single_source_gate_test.go 机械钉住。

func candidateFingerprint(a *domain.Account) (string, error) {
	return identityfence.CandidateFingerprint(a)
}

// CandidateFingerprint 是本包的历史入口，现转发到 identityfence（本轴的权威实现）。
// 包外只剩 internal/proxy 的三个测试（caller_images_codex_test.go、failover_plan_test.go、
// search_endpoint_test.go），它们同样可以直接调 identityfence.CandidateFingerprint；这条转发保留是因为它是
// scheduler 对外的取指纹名字（包内一律走 candidateFingerprint）。sdkbridge 并不 import 它（也 import
// 不了：那会与 scheduler → sdkbridge 成环）。
//
// **注意**：internal/sdkbridge/failure.go 从 P3c 起也改用本轴的别名（同一 error 值），
// 故这条轴上的"单一事实源"覆盖 scheduler / rulepersist / sdkbridge 三处消费方。
func CandidateFingerprint(a *domain.Account) (string, error) {
	return identityfence.CandidateFingerprint(a)
}
