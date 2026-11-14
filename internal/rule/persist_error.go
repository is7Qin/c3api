// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package rule

import (
	"errors"
	"fmt"
)

// persist 失败分类（spec 2026-11-14 §2）：`persist_failures` 总数保留原语义，
// 四个分类是**额外**字段——把「四种原因混在一个总数里」拆成可判读的观测面
// （缺 K / 缺 I / I 不一致 / K 已陈旧）。
//
// 为什么枚举定义在 rule 侧：观测面（RuleEngineStats）与计数器都在 rule，
// 而包依赖方向只能是 scheduler → rule。所以由**产生错误的一侧**（scheduler 的
// NewRulePersistFunc）把失败原因投影成 rule.PersistError{Kind}，rule 侧只用
// errors.As 取 Kind 累加——反过来（rule import scheduler 认哨兵）是方向错误，
// 也会把 rule 包拖进调度器依赖。原始错误仍经 Unwrap 保留，errors.Is 语义
// 不变（scheduler/repository 的哨兵照旧可判）。
type PersistErrorKind uint8

const (
	// PersistErrMissingRevision 事件缺期望身份代际 K（scheduler.ErrMissingExpectedRevision）：
	// 没有来源版本就没有可施加的账号级事实。
	PersistErrMissingRevision PersistErrorKind = iota + 1
	// PersistErrMissingFingerprint 事件缺候选身份指纹 I，或账号侧算不出 I
	// （scheduler.ErrMissingCandidateFingerprint）：判决无法与身份绑定。
	PersistErrMissingFingerprint
	// PersistErrFingerprintMismatch 事件携带的 I 与账号当前 I 不一致
	// （scheduler.ErrCandidateFingerprintMismatch）：判决针对的身份已不存在。
	PersistErrFingerprintMismatch
	// PersistErrStaleRevision 落库 CAS 发现 K 已被管理面推进
	// （repository.ErrStaleIdentityRevision）：陈旧事件，丢弃是正确结果。
	PersistErrStaleRevision
	// PersistErrOther 未分类失败（含 health sink 失败与仓储 IO 故障）。
	// 只累加总数，不进四个分类字段。
	PersistErrOther
)

func (k PersistErrorKind) String() string {
	switch k {
	case PersistErrMissingRevision:
		return "missing_revision"
	case PersistErrMissingFingerprint:
		return "missing_fingerprint"
	case PersistErrFingerprintMismatch:
		return "fingerprint_mismatch"
	case PersistErrStaleRevision:
		return "stale_revision"
	default:
		return "other"
	}
}

// PersistError 给一次持久化失败打上分类标签。Err 保留原始错误
// （Unwrap ⇒ 调用方仍可用 errors.Is 判仓储/调度器哨兵）。
type PersistError struct {
	Kind PersistErrorKind
	Err  error
}

func (e *PersistError) Error() string {
	if e == nil {
		return "<nil persist error>"
	}
	if e.Err == nil {
		return "rule persist: " + e.Kind.String()
	}
	return fmt.Sprintf("rule persist: %s: %v", e.Kind, e.Err)
}

func (e *PersistError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// ClassifyPersistError 取错误的持久化分类；未包装（nil/Kind 零值）⇒ PersistErrOther。
func ClassifyPersistError(err error) PersistErrorKind {
	var pe *PersistError
	if errors.As(err, &pe) && pe.Kind != 0 {
		return pe.Kind
	}
	return PersistErrOther
}
