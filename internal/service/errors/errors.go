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
	ErrNotFound           = errors.New("service: not found")
	ErrInvalidInput       = errors.New("service: invalid input")
	ErrConflict           = errors.New("service: conflict")
	ErrInvalidCredentials = errors.New("service: invalid email or password")
	ErrSignupDisabled     = errors.New("service: signup disabled")
)
