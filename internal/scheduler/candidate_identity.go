// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/identityfence"
)

// 迁移期兼容层（P5-1）：候选身份指纹与三个身份围栏哨兵的 canonical 定义已下沉到
// internal/identityfence（只依赖 domain）。本文件只剩**别名 + 一行转发**。
//
// 为什么是别名而不是重新构造：`errors.Is` 判的是 error 值身份。若这里写成
// `errors.New("scheduler: …")` 或另行 wrap，调用方（sink.go:82,129,133,152、
// scheduler.go:504,590,1140,1219、routing_compiler_candidates.go:109、本包的
// revisionVocabulary）判到的就是另一枚值——语义会**静默**改变。故三个 var 必须是
// 同一 error 值。归属由 internal/identityfence/ownership_gate_test.go 机械钉住
// （它同时钉住 sdkbridge 那套独立哨兵的存在——见该文件的 TestSdkbridgeStillOwnsSecondSentinelSet）。
//
// 移除条件（可执行；条件未满足前**禁止删除**）：
//
//	rg -n "scheduler\.(ErrMissingCandidateFingerprint|ErrCandidateFingerprintMismatch|ErrMissingExpectedRevision|CandidateFingerprint|candidateFingerprint)" --glob '!internal/scheduler/**'
//
// 只剩注释，且 `rg` 确认 scheduler 生产代码对这 5 个名字的引用已全部改指
// identityfence 时，删除别名与转发，并把 generation_symbol_gate_test.go 的
// revisionVocabulary 口径改成 canonical 使用。
var (
	ErrMissingCandidateFingerprint  = identityfence.ErrMissingCandidateFingerprint
	ErrCandidateFingerprintMismatch = identityfence.ErrCandidateFingerprintMismatch
	ErrMissingExpectedRevision      = identityfence.ErrMissingExpectedRevision
)

func candidateFingerprint(a *domain.Account) (string, error) {
	return identityfence.CandidateFingerprint(a)
}

// CandidateFingerprint 是本包的历史入口，现转发到 identityfence（本轴的权威实现）。
// 外部消费者只剩 internal/proxy 的三个测试（caller_images_codex_test.go、
// failover_plan_test.go、search_endpoint_test.go）——sdkbridge 并不 import 它（也 import 不了：
// 那会与 scheduler → sdkbridge 成环）。
//
// **注意**：internal/sdkbridge/failure.go 另有一套同名三哨兵（`sdkbridge:` 前缀），
// 本单未合并；本包这条轴上的"单一事实源"只覆盖 scheduler / rulepersist。
func CandidateFingerprint(a *domain.Account) (string, error) {
	return identityfence.CandidateFingerprint(a)
}
