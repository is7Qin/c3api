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

// 判据常量。
//
// 两个闸的取值都是 4 KiB（spec r16 §3-1 ②／§3-5）：**要省的是"扫多少字节"，不是
// "扫几遍"**——`bytes.Index` 是 SIMD（实测 ~5.5 GB/s），手写逐字节状态机（~0.5–1 GB/s）
// 更慢，把两次扫描融合成一次自定义遍历只会更糟。所以给判据加**长度闸**：>4 KiB 的
// data-only 帧一律不判，让每帧成本与帧长解耦。真实上游的错误载荷整体 ≤300 B
// （13× 余量）。v1 的"每帧两次 bytes.Contains"在 16 KiB 帧上要 5.9 µs/帧、
// 整条 3.2 MiB 流 +382%——这是本次重塑的原因。
const (
	// payloadScanMax 是 data-only 帧的扫判闸：len > 4 KiB ⇒ 不判 ⇒ nil。
	payloadScanMax = 4 << 10
	// payloadParseMax 是 `event: error` 的解析闸：超过只判命中、不解析文案。
	payloadParseMax = 4 << 10
)

// 判据字节串（包级不可变——零锁零全局状态，编译期静态引用，零分配）。
var (
	// payloadEventName 是 `event: error` 的折叠比较目标（针①）。
	payloadEventName = []byte("error")
	// payloadAnchor 是两个形状的**共同子串**（含首尾引号，7 B）：`"error":{` 以它开头，
	// `"type":"error"` 里它紧随 `"type":` ⇒ **同一个 bytes.Index 循环覆盖两种形态**
	// （每次从一个出现位置推进到下一个），命中后再 O(1) 判形。
	payloadAnchor = []byte(`"error"`)
	// payloadTypeKey 是 responses 形状的就近前缀（7 B）。
	payloadTypeKey = []byte(`"type":`)
)

// AsPayloadError 判定一帧是否是「上游在声明失败」，并把它归一成 PayloadError
// （spec §3-1，五处消费者共用的唯一判据）。
//
// 形状（r16：单锚点 + 4 KiB 扫判闸 + 命中才解析）：
//
//	① ev.Event 折叠等于 "error" ⇒ 直接进解析（零预筛——`event: error` 本身就是声明）；
//	   len > 4 KiB 时只判 hit、不解析文案。
//	② 否则 len > 4 KiB ⇒ 不判 ⇒ nil（data-only 大帧，§6-1 申报）。
//	③ 否则（≤4 KiB）：全帧单遍、单锚点 `"error"` + O(1) 就近判形（payloadPrefilter），
//	   **逐个出现位置继续扫**（首个不合格不代表整帧不合格）；未命中 ⇒ nil（零解析、
//	   零分配），命中 ⇒ 进解析。判 nil 后不回头再判。
//	④ 解析（①/③ 共用同一次，parsePayload）：顶层 type == "error"，或顶层 error 为
//	   非空对象（内层 type/message 至少一个非空）⇒ hit。
//
// 语义要点（与 v1 逐条保持不变）：
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
	if bytes.EqualFold(ev.Event, payloadEventName) {
		if len(ev.Data) > payloadParseMax {
			return &PayloadError{} // hit 但不解析：不为文案付整帧解析
		}
		return parsePayload(ev.Data, true)
	}
	if len(ev.Data) > payloadScanMax {
		return nil // data-only 大帧：不判（每帧成本与帧长解耦）
	}
	if !payloadPrefilter(ev.Data) {
		return nil // 普通帧：单锚点单遍、零解析、零分配
	}
	return parsePayload(ev.Data, false)
}

// payloadPrefilter 是 O(1) 判形的单锚点预筛：锚点 `"error"`（含首尾引号）是两种形态的
// 共同子串，任一出现位置满足下列之一即命中：
//
//	紧随 `:{`          ⇒ chat 形状（`"error":{…}`）
//	前 7 B 恰为 `"type":` ⇒ responses 形状（`"type":"error"`）
//
// **逐个出现位置继续扫**：同一帧里"`error` 键但值非对象"可能排在合格键之前
// （`{"error":null,"type":"error"}`、`{"error":"boom","error":{"message":"m"}}`），
// 只判首个出现位置会漏（T-multi）。`"error":null` / `"error":"boom"` 两处都不满足
// ⇒ **不进解析**（保住 T-null 的 0 alloc）。
func payloadPrefilter(data []byte) bool {
	off := 0
	for off < len(data) {
		i := bytes.Index(data[off:], payloadAnchor)
		if i < 0 {
			return false
		}
		i += off
		j := i + len(payloadAnchor)
		if j+1 < len(data) && data[j] == ':' && data[j+1] == '{' {
			return true
		}
		if i >= len(payloadTypeKey) && bytes.Equal(data[i-len(payloadTypeKey):i], payloadTypeKey) {
			return true
		}
		off = j
	}
	return false
}

// parsePayload 是命中后的**唯一一次**解析（spec §3-1 ④）。hit 的初值由调用方给定：
// ①（`event: error`）为 true，③ 为 false。
//
// 只看顶层键、不看返回值、不早退；内层 error 对象只在首字节是 '{' 时解析
// （null/字符串/缺失一律不算命中），且内层 type/message **至少一个非空**才算命中
// （OR 不是 AND）。取内层优先、缺失回退顶层。
func parsePayload(data []byte, hit bool) *PayloadError {
	var probe struct {
		Type    string          `json:"type"`
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(data, &probe)

	hit = hit || probe.Type == "error"
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
