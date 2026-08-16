// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package httpface 管理面/用户面 HTTP 响应书写：WriteJSON/WriteErr/
// WriteServiceErr（service 错误→HTTP 状态映射表唯一一份）。
//
// 依赖仅 internal/service/errors 叶子包（零内部依赖），不反向依赖
// handler/service/auth/server 任何上层包——依赖图无环。
package httpface

import (
	"encoding/json"
	"errors"
	"net/http"

	serviceerr "github.com/is7qin/c3api/internal/service/errors"
)

// WriteJSON 写 JSON 响应（Content-Type application/json + encoder 编码，
// 含尾换行——与各包历史副本逐字节一致）。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteErr 写 {"error": msg} JSON 信封。
func WriteErr(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]any{"error": msg})
}

// WriteServiceErr 统一把 service 错误映射为 HTTP 状态（映射表唯一一份）。
// 404 输出 err.Error()（service 层已把缺失 id 详情包装进 ErrNotFound，与
// 批量 404 同语义）；未命中哨兵 → 500 "internal error"（不泄露内部细节）。
func WriteServiceErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, serviceerr.ErrNotFound):
		WriteErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, serviceerr.ErrInvalidInput):
		WriteErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, serviceerr.ErrConflict):
		WriteErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, serviceerr.ErrInvalidCredentials):
		WriteErr(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, serviceerr.ErrSignupDisabled):
		WriteErr(w, http.StatusForbidden, err.Error())
	default:
		WriteErr(w, http.StatusInternalServerError, "internal error")
	}
}
