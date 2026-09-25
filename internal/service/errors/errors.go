// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package serviceerr 定义 service 层错误哨兵（单一真相）。
//
// 叶子包（零内部依赖）：不 import internal/service 或任何上层包，否则形成
// service → 本包 → service 的 import 环。service 包以别名 re-export
// （var ErrNotFound = serviceerr.ErrNotFound 等）保持既有引用
// （errors.Is(err, service.ErrXxx)）同一哨兵实例语义——80+ 调用点零改动。
package serviceerr

import (
	"errors"

	"github.com/is7qin/c3api/internal/domain"
)

var (
	ErrNotFound     = errors.New("service: not found")
	ErrInvalidInput = errors.New("service: invalid input")
	ErrConflict     = errors.New("service: conflict")
	// ErrPreconditionFailed 乐观锁前置条件不满足（PATCH 的 If-Match 陈旧 → 412）。
	// 与 ErrConflict（body-CAS 陈旧 → 409，仅 recover 的 expected_revision）刻意
	// 分码：header 前置条件与 body-CAS 是两类不同的陈旧判据。
	ErrPreconditionFailed    = errors.New("service: precondition failed")
	ErrInvalidCredentials    = errors.New("service: invalid email or password")
	ErrSignupDisabled        = errors.New("service: signup disabled")
	ErrTooManyRequests       = errors.New("service: too many requests")
	ErrMailNotConfigured     = errors.New("service: mail not configured")
	ErrMailQueueFull         = errors.New("service: mail queue full")
	ErrMailChannelTestFailed = errors.New("service: mail channel test failed")
)

// StatsWindowError 统计窗口拒绝的**线缆载体**（spec §4.4(d)：一个哨兵 + 一个
// 载体类型）：判定属 domain.Admit（本类型不做任何判定），它只把该判定接到既有
// 校验哨兵上——httpface 的 400 映射按哨兵判，语义与引入前完全一致——并携带
// §4.4(c) 的机读字段（httpface 用 errors.As 取值，**禁字符串嗅探**）。
//
// **位置**：本类型必须同时对生产方（internal/service）与序列化方
// （internal/handler/httpface）可见，而 httpface **不能** import
// internal/service：service → internal/auth → handler/httpface 已成环
// （internal/auth/middleware.go 用 httpface 写错误响应）。故载体落在本叶子包，
// service 以类型别名 re-export（与 ErrXxx 哨兵同一手法、同一理由），调用点仍
// 写 service.StatsWindowError；这是位置选择，不是第二套类型。
type StatsWindowError struct {
	domain.StatsWindowError
}

// Unwrap 拒绝一律映射既有校验哨兵：errors.Is(err, ErrInvalidInput) 恒成立
// （400 映射的唯一依据，语义与此前各校验 helper 的哨兵一致）。
func (e *StatsWindowError) Unwrap() error { return ErrInvalidInput }

// CarriesWindowFields 该拒绝是否携带 storage / limit_seconds / retention_days /
// effective_* 字段。`window_invalid` 是参数本身非法（必填/倒序），**没有**可报
// 的存储、上限或保留截止，故这些字段必须省略（J2b）——否则客户端会读到
// `"storage":"cube"`（StatsStorage 零值）与 `limit_seconds:0` 这类伪造事实。
func (e *StatsWindowError) CarriesWindowFields() bool {
	return e.Reject != domain.StatsRejectWindowInvalid
}
