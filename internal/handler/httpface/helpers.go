// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package httpface

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Decode 管理面/用户面共享的严格 JSON 解码：未知字段 → 错误（拼错字段名从
// 200 静默不生效变 400 显式）；二次 Decode 拒尾随数据（io.EOF 才算完）。
// 请求体上限 1 MiB；需要更大上限的端点（探测 8 MiB）走 DecodeLimit。
//
// 实现从 handler/user 两份逐字节相同的本地副本下沉到本包（两面包共享的请求
// 参数边界，与 ClampLimit 同款先例）——不再两处各留一份。
func Decode(r *http.Request, v any) error {
	return DecodeLimit(r, v, 1<<20)
}

// DecodeLimit 是 Decode 的可配置上限版本：语义逐字节一致
// （DisallowUnknownFields + trailing-data 检查），仅请求体上限由调用方给定
// （如探测端点 domain.ProbeMaxRequestBytes=8 MiB）。上限是**字节**；超过时
// json.Decoder 返回 http.MaxBytesError。
func DecodeLimit(r *http.Request, v any, limit int64) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, limit))
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

// HasJSONKey 报告原始 JSON **对象顶层**是否出现该键（**含显式 null**）。
// 与「解码到 &T」的区别：普通指针字段把「键缺席」与「键为 null」都归零值，而
// 部分契约要求「出现即拒」（如供应商面导入体出现 supplier_user_id ⇒ 400，
// 见 spec 2026-10-09 §2.5）——那种判据只能看原始键存在性。
//
// body 为已读入内存的原始请求体。顶层非对象（数组/标量）⇒ 返回 false（不做
// 结构校验；Decode 到结构体的路径自会拒绝，不在此重复判定）。非法 JSON ⇒ 错误。
func HasJSONKey(body []byte, key string) (bool, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return false, err
	}
	_, ok := raw[key]
	return ok, nil
}

// DecodeRaw 读入原始请求体（有界）并恢复 r.Body，供需要「键存在性」判据的
// 端点先看原始 JSON、再走 Decode 的严格结构校验。
func DecodeRaw(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
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
//
//go:fix inline
func Ptr[T any](v T) *T { return new(v) }

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

// FormatMinor 最小单位（渠道币种分 / 余额毫分）→ 两位小数定点串（`%d.%02d`）。
//
// 支付网关契约面（spec 支付网关 §7-1）**禁止**让钱经过 float：服务端全程整数
// 换算，只在响应边界格式化。管理面与用户面同款输出，故下沉到本包而不是各面
// 各留一份（三份实现必然分叉出 "1.5" vs "1.50" 这类差异）。
func FormatMinor(minor int64) string {
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	return fmt.Sprintf("%s%d.%02d", sign, minor/100, minor%100)
}

// FormatRateMicro 汇率 ×1e6 → 六位小数定点串（`%d.%06d`，如 7200000 → "7.200000"）。
// 与 FormatMinor 同一条纪律：不经 float，不用 strconv.FormatFloat。
func FormatRateMicro(micro int64) string {
	sign := ""
	if micro < 0 {
		sign, micro = "-", -micro
	}
	return fmt.Sprintf("%s%d.%06d", sign, micro/1_000_000, micro%1_000_000)
}

// FormatUSDFromMilli 余额毫分 → USD 定点串。1 USD = 100,000 毫分 = 100 分，
// 故先整除 1000 折成"分"再共用 FormatMinor（支付订单的 credit_milli 恒是
// usd_cents*1000，整除无损；全程整数，绝不经 float）。
func FormatUSDFromMilli(creditMilli int64) string {
	return FormatMinor(creditMilli / 1000)
}
