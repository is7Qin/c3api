// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package httpface 管理面/用户面 HTTP 边界：响应书写（WriteJSON/WriteErr/
// WriteServiceErr，service 错误→HTTP 状态映射表唯一一份）+ 请求参数边界
// （ClampLimit——两面包共享的分页上限钳制）。
//
// 依赖仅 internal/service/errors 叶子包（零内部依赖），不反向依赖
// handler/service/auth/server 任何上层包——依赖图无环。
package httpface

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/is7qin/c3api/internal/domain"
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

// 统计响应回显头（spec-stats-window-plan-2026-09-25 §4.4(b)）：三个 200 体是
// **裸数组**（加字段即破契约）⇒ 生效窗口与实际存储只能走响应头。
const (
	// HeaderStatsEffectiveFrom 本次读取真正使用的窗口下界（RFC3339，UTC）。
	HeaderStatsEffectiveFrom = "X-Stats-Effective-From"
	// HeaderStatsEffectiveTo 本次读取真正使用的窗口上界（RFC3339，UTC，半开）。
	HeaderStatsEffectiveTo = "X-Stats-Effective-To"
	// HeaderStatsStorage 本次读取实际使用的存储（cube|raw）。
	HeaderStatsStorage = "X-Stats-Storage"
)

// WriteStatsEcho 写三个回显头，值**直接取自 service 判定后回传的 Exec**。
// 调用方绝不重算窗口判定：判定 owner 只有 domain.Admit 一处（重算即第二份
// 判定，而两份判定的 now 不同还可能给出不同窗口，回显就会撒谎）。
//
// 无跨源部署（管理台/用户台同源由本进程服务），故不需要
// Access-Control-Expose-Headers；若将来跨源，这三个头必须先暴露。
func WriteStatsEcho(w http.ResponseWriter, exec domain.Exec) {
	h := w.Header()
	h.Set(HeaderStatsEffectiveFrom, exec.From.UTC().Format(time.RFC3339))
	h.Set(HeaderStatsEffectiveTo, exec.To.UTC().Format(time.RFC3339))
	h.Set(HeaderStatsStorage, exec.Storage.String())
}

// ClampLimit 分页 limit 统一钳制（上限 200，与 /usage_logs 同语义）：超限
// 裁剪到 200；≤0 原样透传（repo 归一下限 ≤0→20）。管理面误操作
// limit=2147483647 不再全表物化（spec 2026-08-17 边界收敛，6 端点共享）。
func ClampLimit(l int) int {
	if l > 200 {
		return 200
	}
	return l
}

// WriteServiceErr 统一把 service 错误映射为 HTTP 状态（映射表唯一一份）。
// 404 输出 err.Error()（service 层已把缺失 id 详情包装进 ErrNotFound，与
// 批量 404 同语义）；未命中哨兵 → 500 "internal error"（不泄露内部细节）。
// 400 额外序列化统计窗口拒绝的机读字段（spec §4.4(c)）。
func WriteServiceErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, serviceerr.ErrNotFound):
		WriteErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, serviceerr.ErrInvalidInput):
		writeInvalidInput(w, err)
	case errors.Is(err, serviceerr.ErrConflict):
		WriteErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, serviceerr.ErrPreconditionFailed):
		WriteErr(w, http.StatusPreconditionFailed, err.Error())
	case errors.Is(err, serviceerr.ErrInvalidCredentials):
		WriteErr(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, serviceerr.ErrSignupDisabled):
		WriteErr(w, http.StatusForbidden, err.Error())
	case errors.Is(err, serviceerr.ErrTooManyRequests):
		WriteErr(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, serviceerr.ErrMailNotConfigured):
		WriteErr(w, http.StatusInternalServerError, err.Error())
	case errors.Is(err, serviceerr.ErrMailChannelTestFailed):
		WriteErr(w, http.StatusInternalServerError, err.Error())
	default:
		WriteErr(w, http.StatusInternalServerError, "internal error")
	}
}

// errBody 错误信封（`{"error": ...}` + 统计窗口拒绝时的可选机读字段）。
// 除 error 外**全部可选**（省略而非零值）：既有客户端不受影响。
type errBody struct {
	Error string `json:"error"`
	// Reason 线缆原因串（§4.4(d) 映射表的唯一来源 = domain.StatsWindowReject.String）。
	Reason string `json:"reason,omitempty"`
	// Storage 判定所依据的实际存储（`window_invalid` 省略——J2b）。
	Storage string `json:"storage,omitempty"`
	// LimitSeconds 成本上限秒数（仅 `window_too_long`；上限 0 = 无上限，故不出现）。
	LimitSeconds int64 `json:"limit_seconds,omitempty"`
	// EffectiveFrom/EffectiveTo 判定后的生效窗口（`window_invalid` 省略——回显
	// 一个"非法的请求窗口"会假装它是生效窗口）。
	EffectiveFrom string `json:"effective_from,omitempty"`
	EffectiveTo   string `json:"effective_to,omitempty"`
	// RetentionDays 覆盖判定所依据的最保守保留天数（仅 `*_horizon`）。
	RetentionDays int `json:"retention_days,omitempty"`
}

// writeInvalidInput 400 路径：先按哨兵判（不变），再以 errors.As 取统计窗口
// 载体补机读字段——**禁字符串嗅探**（spec §4.4(d) 指定的唯一取值手段）。
// 非统计窗口的校验错误（绝大多数 400）走同一信封且零可选字段，字节输出与
// 引入前逐字节一致。
func writeInvalidInput(w http.ResponseWriter, err error) {
	body := errBody{Error: err.Error()}
	var werr *serviceerr.StatsWindowError
	if errors.As(err, &werr) {
		// reason 词汇表以 §4.4(d) 映射表为唯一来源，此处不重复枚举。
		body.Reason = werr.Reject.String()
		if werr.CarriesWindowFields() {
			body.Storage = werr.Storage.String()
			body.LimitSeconds = werr.LimitSeconds
			body.RetentionDays = werr.RetentionDays
			body.EffectiveFrom = rfc3339OrEmpty(werr.EffectiveFrom)
			body.EffectiveTo = rfc3339OrEmpty(werr.EffectiveTo)
		}
	}
	WriteJSON(w, http.StatusBadRequest, body)
}

// rfc3339OrEmpty 零值时刻 → 空串（omitempty 省略该字段）。
func rfc3339OrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
