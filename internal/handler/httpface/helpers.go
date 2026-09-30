// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package httpface

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// Decode 管理面/用户面共享的严格 JSON 解码：未知字段 → 错误（拼错字段名从
// 200 静默不生效变 400 显式）；二次 Decode 拒尾随数据（io.EOF 才算完）。
//
// 实现从 handler/user 两份逐字节相同的本地副本下沉到本包（两面包共享的请求
// 参数边界，与 ClampLimit 同款先例）——不再两处各留一份。
func Decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing data after JSON body")
		}
		return err
	}
	return nil
}

// Deref 返回指针指向的值；nil 时返回零值。
func Deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

// Ptr 返回指向 v 的指针（响应契约字段赋值用）。
func Ptr[T any](v T) *T { return &v }

// LogLimit 明细（usage_logs/err_logs）分页 limit 归一：≤0 → 缺省 20；>200 →
// 上限 200（走 ClampLimit——分页上限钳制唯一来源）。四处明细 handler 共用。
func LogLimit(limit int) int {
	if limit <= 0 {
		limit = 20
	}
	return ClampLimit(limit)
}

// ClipLogPage 明细 keyset 分页的 limit+1 探测（repo 多取 1 行）：len(page) >
// limit 时截到 limit，next_cursor = 本页最后一条 id；否则 next = nil（无下一页）。
// id 取行主键指针（调用方注入——usage/errlog 四 handler 的行类型不同；契约面
// ID 为 *int64）。
func ClipLogPage[T any](page []T, limit int, id func(T) *int64) ([]T, *int64) {
	if len(page) > limit {
		return page[:limit], id(page[limit-1])
	}
	return page, nil
}
