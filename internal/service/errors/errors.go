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

import "errors"

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
