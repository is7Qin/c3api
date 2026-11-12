// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sserelay

import (
	"bytes"
	"encoding/json"
)

// PayloadError 是「上游在流内投递了错误载荷」这一类失败的唯一表示
// （spec-inband-error-truth r10 §3-1）。它只承载两个字段：上游 error 对象的
// type 与 message——**不含 status**，因为这类帧没有 HTTP 状态可言
// （statusOf 靠"结构体没有 StatusCode 字段"返 0，落 pipeline 的 code 0 分支）。
//
// 生命周期：由 AsPayloadError 在帧旁路判定时构造（命中即返回，每个命中帧一次），
// 随后作为 Relay 的返回错误向上传播；`Type`/`Message` 是解析结果的字符串拷贝，
// 不引用 relay 的复用缓冲。
type PayloadError struct{ Type, Message string }

// Error 实现 error：Message 非空 ⇒ 原样返回（让上游原文进入 last_error /
// err_logs）；否则给一句自述文案（`event: error` + 空 data 这类无 payload 形态）。
func (e *PayloadError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "upstream error payload"
}

// RawJSON 合成 `{"error":{"type":…,"message":…}}`，供**已提交**路径的客户端文案
// 使用（caller_images_stream.go 的 `interface{ RawJSON() string }` 鸭子类型 →
// streamErrMessage → buildErrorFrame）。没有它，已提交路径的客户端只会看到
// "upstream connection error"，比修复前（至少能看到上游原文）更差。
//
// 只合成 type/message：上游的 param/code 等字段按 spec §6-2 舍弃。json.Marshal
// 生成（逃逸一次），空字段省略。
func (e *PayloadError) RawJSON() string {
	var out struct {
		Error struct {
			Type    string `json:"type,omitempty"`
			Message string `json:"message,omitempty"`
		} `json:"error"`
	}
	out.Error.Type = e.Type
	out.Error.Message = e.Message
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}

// 判据常量（包级不可变字节串——零锁零全局状态，编译期静态引用，零分配）。
var (
	// needleTypeError / needleErrorObject 是两条预筛针（spec §3-1 ②③）：只有
	// 命中才解析，普通 chunk 两针、零解析、零分配。
	needleTypeError   = []byte(`"type":"error"`)
	needleErrorObject = []byte(`"error":{`)
	// payloadEventName 是 `event: error` 的折叠比较目标（针①）。
	payloadEventName = []byte("error")
)

// AsPayloadError 判定一帧是否是「上游在声明失败」，并把它归一成 PayloadError
// （spec §3-1，五处消费者共用的唯一判据）。
//
// 命中条件（帧**顶层**投递错误载荷，与 status 无关）：
//
//	① ev.Event 折叠等于 "error"（零预筛——`event: error` 本身就是声明）
//	② 预筛 `"type":"error"` 或 ③ 预筛 `"error":{` 命中，且
//	   - data 顶层 type == "error"，或
//	   - data 顶层 error 为非空对象（内层 type/message 至少一个非空）
//
// 语义要点：
//   - **① 也要解析**：Type/Message 只能来自 data，`data: {"message":"failed"}`
//     只有①命中，不解析就拿不到上游文案（S5b）。
//   - **解析不看返回值、不早退**：单字段类型不匹配时 encoding/json 会跳过该字段
//     并继续填充其余字段（`{"type":123,"error":{"message":"boom"}}` 仍可判 hit），
//     所以是 `_ = json.Unmarshal(...)`。`if err != nil { return nil }` 会把真错误帧
//     静默判成 nil。
//   - **顶层确认是结构体解析的副产品**：json.Unmarshal 只认顶层键 ⇒
//     `item.error` 这类嵌套字段天然不命中（T-nested）。
//   - **OR 不是 AND**：内层 type 与 message 都为空才算空对象（Azure 风格
//     `{"error":{"message":…,"code":…}}` 无 type 也要命中）。
//   - **取值**：内层 error.type/error.message 优先，缺失回退顶层；不取 error.code。
//   - 重复键按 encoding/json 后值语义（`{"type":"error","type":"message"}` ⇒ nil）。
//
// 不复制整帧：只有 probe.Error（error 对象大小，受 Config.MaxLineBytes 约束）
// 与两个 string 逃逸。
func AsPayloadError(ev Event) *PayloadError {
	eventIsError := bytes.EqualFold(ev.Event, payloadEventName)
	if !eventIsError &&
		!bytes.Contains(ev.Data, needleTypeError) &&
		!bytes.Contains(ev.Data, needleErrorObject) {
		return nil // 普通帧：两针、零解析、零分配
	}
	// A（①）与 B（②③）共用同一次解析：只看顶层键，不看返回值、不早退。
	var probe struct {
		Type    string          `json:"type"`
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(ev.Data, &probe)

	hit := eventIsError || probe.Type == "error"
	// 内层 error 对象：仅当首字节是 '{' 才解析（null/字符串/缺失一律不算命中）。
	// 空对象（内层 type 与 message 都空）不算命中。
	var inner struct{ Type, Message string }
	innerHoldsFields := false
	if len(probe.Error) > 0 && probe.Error[0] == '{' {
		_ = json.Unmarshal(probe.Error, &inner)
		innerHoldsFields = inner.Type != "" || inner.Message != "" // OR 不是 AND
	}
	if !hit && !innerHoldsFields {
		return nil
	}
	pe := &PayloadError{Type: probe.Type, Message: probe.Message}
	if innerHoldsFields { // 内层优先，缺失回退顶层
		if inner.Type != "" {
			pe.Type = inner.Type
		}
		if inner.Message != "" {
			pe.Message = inner.Message
		}
	}
	return pe
}
