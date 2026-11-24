// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package identityfence 是候选身份指纹（I）与三个身份围栏哨兵的权威定义——**本轴**
// （scheduler / rulepersist / 后续其它控制面组件）的唯一一份。
//
// 为什么单独成包（P5-1，R2 的 D5）：`candidateFingerprint` 与三个哨兵原先住在
// internal/scheduler，而 internal/rulepersist（规则判定落库）要判它们。留在 scheduler
// 会让"谁是权威"变成"谁刚好 import 了谁"；搬进一个只依赖 domain 的叶子包之后，
// 这条轴上的权威只有一处，消费方各自 import 它。
//
// **第二套已消除（P3c I-A）**：internal/sdkbridge/failure.go 原先自持一套同名三哨兵
// （`sdkbridge:` 前缀，各一枚独立的 errors.New），其 isTransientFailure 用自己那三枚
// 判等——跨轴（scheduler 家族的 error × sdkbridge 的判定）会静默 false。现在那三个导出
// 名是本包 canonical 的**变量别名**（同一 error 值），跨包 errors.Is 成立。反向断言
// （sdkbridge 只许别名、不许自建）钉在两处：ownership_gate_test.go 的
// TestSdkbridgeAliasesIdentityfenceSentinels（AST，扫整包）与 sdkbridge 的
// TestIdentitySentinelsAreSharedWithIdentityfence（值身份 + 双向 errors.Is）。
//
// 边界：本包只依赖 internal/domain（指纹由 domain.AccountCandidateFingerprint /
// domain.CandidateFPHex 派生）。**不**依赖 repository（repository.ErrStaleIdentityRevision
// 是仓储 CAS 的哨兵，归 repository，不搬）、**不**依赖 scheduler（否则等于把哨兵挪了个寂寞）。
//
// 计数与语义由 identityfence_test.go 的归属门禁机械钉住：三个 errors.New 与
// CandidateFingerprint 的实现体必须在本包，scheduler / sdkbridge 两侧只许留别名
// （scheduler 另有一行转发）。
package identityfence

import (
	"errors"
	"fmt"

	"github.com/is7qin/c3api/internal/domain"
)

var (
	// ErrMissingCandidateFingerprint 缺少候选身份指纹 I：
	// 事件没带 I，或账号侧根本算不出 I（身份字段缺失）。判决无法与身份绑定。
	ErrMissingCandidateFingerprint = errors.New("identityfence: candidate fingerprint required")
	// ErrCandidateFingerprintMismatch 事件携带的 I 与账号当前 I 不一致：
	// 判决针对的身份已不存在。
	ErrCandidateFingerprintMismatch = errors.New("identityfence: candidate fingerprint mismatch")
	// ErrMissingExpectedRevision 事件缺期望身份代际 K：
	// 没有来源版本就没有可施加的账号级事实。
	ErrMissingExpectedRevision = errors.New("identityfence: expected revision required")
)

// CandidateFingerprint 是候选身份指纹 I 的 canonical 实现。
//
// 单一事实源：domain.AccountCandidateFingerprint（身份字段缺失 ⇒ wrap
// ErrMissingCandidateFingerprint，分类与"事件没带 I"相同：判决无法与身份绑定）。
// 返回十六进制串（domain.CandidateFPHex）——它就是键结构、latch、continuation 绑定与
// 尝试身份里那个 I 的线格式。
func CandidateFingerprint(a *domain.Account) (string, error) {
	fp, err := domain.AccountCandidateFingerprint(a)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrMissingCandidateFingerprint, err)
	}
	return domain.CandidateFPHex(fp), nil
}
