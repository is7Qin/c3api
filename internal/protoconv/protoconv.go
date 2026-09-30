// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package protoconv 提供网关级协议转换（只补差语义）：客户端协议请求体
// → 模板协议请求体（ConvertRequest）、模板协议响应 → 客户端协议响应
// （ConvertResponse 非流式 JSON / StreamMapper 流式 SSE 事件映射）。
//
// 边界（用户拍板）：转换器是网关能力，标准库实现（encoding/json），与
// OpenAI/Anthropic SDK 零耦合（缺名帧事件名推断与 sserelay 共用
// InferEventName——热路径零分配锚定 + 回退全量解码）；四方向分派按
// groups.protocol_convert 快照值（off 不经过本包——热路径分支在
// internal/proxy 判定）；WS 帧流转换不做（resp-ws 1:1 透传 范围）。
package protoconv

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/sserelay"
)

// defaultMaxTokens 是 Messages 必填 max_tokens 在客户端未给上限时的常量，
// 不是模型上限查询。
const defaultMaxTokens = 4096

// ConvertRequest 把客户端协议请求体转换为 dir 指向的模板协议请求体（返回的
// 字节即模板协议上游请求体，可直接转发）。转换器按目标协议规范映射字段；
// 目标协议无对应参数的字段（如 chat 的 frequency_penalty → resp 无此参数）
// 按规范丢弃。model 恒保持（调度器 ModelMapping 在转换后改写）。
func ConvertRequest(body []byte, dir domain.ProtocolConvert) ([]byte, error) {
	switch dir {
	case domain.ProtocolConvertChatToResp:
		return chatToRespRequest(body)
	case domain.ProtocolConvertMessToResp:
		return messToRespRequest(body)
	case domain.ProtocolConvertRespToMess:
		return respToMessRequest(body)
	case domain.ProtocolConvertChatToMess:
		return chatToMessRequest(body)
	}
	return nil, fmt.Errorf("protoconv: unsupported direction %q", dir)
}

// ConvertResponse 把模板协议的非流式响应 JSON 转换为客户端协议响应 JSON。
func ConvertResponse(body []byte, dir domain.ProtocolConvert) ([]byte, error) {
	switch dir {
	case domain.ProtocolConvertChatToResp:
		return respToChatResponse(body)
	case domain.ProtocolConvertMessToResp:
		return respToMessResponse(body)
	case domain.ProtocolConvertRespToMess:
		return messToRespResponse(body)
	case domain.ProtocolConvertChatToMess:
		return messToChatResponse(body)
	}
	return nil, fmt.Errorf("protoconv: unsupported direction %q", dir)
}

// NewStreamMapper 构造有状态的流式响应事件映射器（每 SSE 流一个实例；
// 跨事件状态：id/model、用量累积、mess→resp 的块级输出累积）。块级累积
// map（blockStarted 等）懒初始化（ensureBlocks）——chat→resp 等
// 方向从不使用，免每流 6 个 map 分配。
func NewStreamMapper(dir domain.ProtocolConvert) *StreamMapper {
	return &StreamMapper{dir: dir}
}

// ensureBlocks 懒初始化 mess→resp / resp→mess 方向的内容块累积状态
// （首个块级事件到达时分配）。
func (m *StreamMapper) ensureBlocks() {
	if m.blockStarted == nil {
		m.blockStarted = make(map[int64]bool)
		m.blockStopped = make(map[int64]bool)
		m.textByIndex = make(map[int64]string)
		m.argsByIndex = make(map[int64]string)
		m.fcNames = make(map[int64]string)
		m.fcIDs = make(map[int64]string)
		m.itemIndex = make(map[int64]int64)
		m.owner = make(map[int64]int64)
	}
}

// contentIndex 把 resp output_index 分配成 anthropic content index。
// 同一 output_index 只分配一次：优先用 output_index 本身，已被占用则用
// max(已占用)+1。output_index 缺省为 0（调用方 intOr0）。
func (m *StreamMapper) contentIndex(outputIndex int64) int64 {
	if m.itemIndex == nil {
		m.itemIndex = make(map[int64]int64)
		m.owner = make(map[int64]int64)
	}
	if idx, ok := m.itemIndex[outputIndex]; ok {
		return idx
	}
	idx := outputIndex
	if holder, taken := m.owner[idx]; taken && holder != outputIndex {
		idx = m.nextIndex
	}
	for {
		if holder, taken := m.owner[idx]; !taken || holder == outputIndex {
			break
		}
		idx++
	}
	m.itemIndex[outputIndex] = idx
	m.owner[idx] = outputIndex
	if idx >= m.nextIndex {
		m.nextIndex = idx + 1
	}
	return idx
}

// --- Responses / Anthropic usage 口径（唯一真相；改动前必读）---
//
// 网关内部 Responses（resp）侧 usage.input_tokens 恒为**总量（gross）**：
//
//	input_tokens = uncached_input + cache_read + cache_write
//
// input_tokens_details.cached_tokens（= cache_read）与
// input_tokens_details.cache_write_tokens（= cache_write）都是 input_tokens 的
// **子集**（cached ⊆ input、cache_write ⊆ input）。该包含关系由 codex 源码
// 证实：codex-rs/protocol/src/protocol.rs:2426-2432 令
// non_cached_input() = (input_tokens - cached_input_tokens).max(0)（cached ⊆
// input）；codex-rs/codex-api/src/sse/responses.rs:128-143 把
// input_tokens_details.cached_tokens / cache_write_tokens 作为 input_tokens 的
// 明细子字段（其测试用例 input_tokens=100 配 cached=40 / cache_write=60）。
//
// Anthropic Messages（mess）侧相反：input_tokens 是**净额（net，不含缓存）**，
// cache_read_input_tokens / cache_creation_input_tokens 是独立字段。故 resp↔mess
// 必须换算（此前 resp→mess 不减、mess→resp 相加，往返把 input_tokens 从 10
// 膨胀到 17——P0-1）：
//
//	resp→mess：net = input_tokens - cache_read - cache_write（下限 0）
//	mess→resp：gross = input_tokens + cache_read + cache_write
//
// 计费侧同此口径（internal/proxy/usage_extract.go、internal/billing/cost.go）：
// 按 net（uncached）计 input 单价，cache_read / cache_write 各自单价——消除
// cache_write 被 input 价与 cache-write 价双重计费（P0-2）。

// respInputGross 由 Anthropic 侧净额 + 缓存分量重建 resp（gross）input_tokens。
func respInputGross(net, cacheRead, cacheWrite int64) int64 {
	return net + cacheRead + cacheWrite
}

// respInputNet 由 resp（gross）input_tokens 反推 Anthropic 侧净额（下限 0——
// 病态上游 input_tokens < cache_read + cache_write 时钳 0 防负值；口径见上）。
func respInputNet(gross, cacheRead, cacheWrite int64) int64 {
	net := gross - cacheRead - cacheWrite
	if net < 0 {
		return 0
	}
	return net
}

// messInputTotal Messages 净额 + cache_creation + cache_read → resp（gross）
// input_tokens（口径见本文件上方「Responses / Anthropic usage 口径」）。
func (m *StreamMapper) messInputTotal() int64 {
	return respInputGross(m.it, m.cached, m.cacheCreate)
}

// setMessUsage 全量记录 Messages usage（message_start 首帧）：三项都写入
// （缺失为 0）。
func (m *StreamMapper) setMessUsage(u map[string]any) {
	m.it = intOr0(u, "input_tokens")
	m.cached = intOr0(u, "cache_read_input_tokens")
	m.cacheCreate = intOr0(u, "cache_creation_input_tokens")
}

// mergeMessUsage 合并记录 Messages usage（message_delta 覆盖 message_start）：
// 只覆盖出现的累计字段。
func (m *StreamMapper) mergeMessUsage(u map[string]any) {
	if _, ok := u["input_tokens"]; ok {
		m.it = intOr0(u, "input_tokens")
	}
	if _, ok := u["cache_read_input_tokens"]; ok {
		m.cached = intOr0(u, "cache_read_input_tokens")
	}
	if _, ok := u["cache_creation_input_tokens"]; ok {
		m.cacheCreate = intOr0(u, "cache_creation_input_tokens")
	}
}

// StreamMapper 按方向把模板协议的 SSE 事件映射为客户端协议帧（[]byte 即完整
// 帧字节，含 event/data 行与结尾空行）。映射帧复用 mapper 内缓冲（buf/dbuf），
// 生命周期仅限本次 Map 调用——调用方（sserelay Mapper 契约）立即写出后丢弃，
// 下一帧复用同一缓冲，逐帧零分配。chat→resp 方向为字节级组装，其余方向仍
// 为 map 组装（EncodeFrame）。
type StreamMapper struct {
	dir domain.ProtocolConvert

	started bool // 已发出首事件（防御重复的 created/start）
	done    bool // 已发出终止帧（防御重复的 completed/stop）
	id      string
	model   string
	created int64
	it, ot      int64 // 用量（input/output tokens；mess 侧 it 是未含缓存的 input_tokens）
	cached      int64 // cache_read / cached_tokens
	cacheCreate int64 // mess cache_creation_input_tokens（不计入 cached_tokens）
	reason      string

	// resp→mess：output_index → content index，以及 content index → 占用它的 output_index。
	itemIndex map[int64]int64
	owner     map[int64]int64
	nextIndex int64

	// 字节级帧组装复用缓冲（chat→resp 流式路径）：buf = 输出帧；dbuf =
	// delta/usage 预组装；ubuf = completed 帧上从 dbuf 拷出的 usage，
	// 避免 chatChunkFrame 覆盖 dbuf 前再分配一次。帧返回后下一帧覆盖，
	// 调用方不得跨帧保留。
	buf  []byte
	dbuf []byte
	ubuf []byte

	// mess→resp（RespToMess）方向：块级累积状态。
	blockStarted map[int64]bool
	blockStopped map[int64]bool
	textByIndex  map[int64]string
	argsByIndex  map[int64]string
	fcNames      map[int64]string
	fcIDs        map[int64]string
	blockOrder   []int64 // 内容块出现顺序（output items 保持顺序）
}

// Map 把一个模板协议 SSE 事件映射为客户端协议帧；drop=true 丢弃该帧。
// 缺 event: 名（data-only）帧不丢：data 为 JSON 对象且含字符串 type
// 字段时按该值推断事件名（resp/messages 帧 type 与事件名同值约定，非规范
// 上游如仓库 fakeupstream /v1/responses 缺 event: 行），推断出 → 与具名帧
// 同分派；无法推断（非 JSON / 无 type 字段）→ 原样透传 data 帧保留字节。
// 空帧（无字节可透传）→ 丢弃。chat 的 [DONE] 等终止帧由映射器自行产出，
// 透传仅兜底非规范上游。
func (m *StreamMapper) Map(name string, data []byte) ([]byte, bool) {
	if name == "" {
		name = string(sserelay.InferEventName(data))
		if name == "" {
			if len(data) == 0 {
				return nil, true
			}
			return encodeDataFrame(data), false
		}
	}
	switch m.dir {
	case domain.ProtocolConvertChatToResp:
		return m.mapRespToChat(name, data)
	case domain.ProtocolConvertMessToResp:
		return m.mapRespToMess(name, data)
	case domain.ProtocolConvertRespToMess:
		return m.mapMessToResp(name, data)
	case domain.ProtocolConvertChatToMess:
		return m.mapMessToChat(name, data)
	}
	return nil, true
}

// itemID 合成 resp output item id（message item 专用；确定性，无需随机源）。
func (m *StreamMapper) itemID(index int64) string {
	return "msg_" + m.id + "_" + fmt.Sprint(index)
}

// EncodeFrame 组装 SSE 帧字节：name 非空 → "event: name\n" 行；data 为 JSON
// 载荷（marshal 为单行 data）。载荷不可 marshal（转换器仅产出 map/string 等
// 可序列化值）→ 返回 nil，调用方按丢弃处理。
func EncodeFrame(name string, data any) []byte {
	payload, err := json.Marshal(data)
	if err != nil {
		return nil
	}
	out := make([]byte, 0, len(name)+len(payload)+16)
	if name != "" {
		out = append(out, "event: "...)
		out = append(out, name...)
		out = append(out, '\n')
	}
	out = append(out, "data: "...)
	out = append(out, payload...)
	out = append(out, '\n', '\n')
	return out
}

// encodeDataFrame 把 data 载荷编码为 data-only SSE 帧（缺名帧透传用）。
// 多行 payload 逐行重建 data: 前缀——SSE 规范连续 data: 行以 \n 连接，
// 单行内嵌换行会变成无冒号行被客户端忽略（语义丢失）。
func encodeDataFrame(data []byte) []byte {
	out := make([]byte, 0, len(data)+len(data)/2+8)
	for start := 0; start < len(data); {
		out = append(out, "data: "...)
		i := bytes.IndexByte(data[start:], '\n')
		if i < 0 {
			out = append(out, data[start:]...)
			break
		}
		out = append(out, data[start:start+i]...)
		out = append(out, '\n')
		start += i + 1
	}
	// 帧尾：末行换行 + 空行（帧终止）
	out = append(out, '\n', '\n')
	return out
}

// decodeObj 解析 JSON 对象（转换器输入恒为对象）。
func decodeObj(body []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// str 读字符串字段。
func str(m map[string]any, key string) (string, bool) {
	if v, ok := m[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s, true
		}
	}
	return "", false
}

// arr 读数组字段。
func arr(m map[string]any, key string) ([]any, bool) {
	if v, ok := m[key]; ok {
		if a, ok := v.([]any); ok {
			return a, true
		}
	}
	return nil, false
}

// num 读 JSON number 字段（float64 解码）。
func num(m map[string]any, key string) (float64, bool) {
	if v, ok := m[key]; ok && v != nil {
		if f, ok := v.(float64); ok {
			return f, true
		}
	}
	return 0, false
}

// intOr0 读整数字段（缺失/类型异常 → 0）。
func intOr0(m map[string]any, key string) int64 {
	if f, ok := num(m, key); ok {
		return int64(f)
	}
	return 0
}

// pass 按表透传字段：dst[key] = src[key]（仅当存在且非 nil）。
func pass(dst, src map[string]any, keys ...string) {
	for _, k := range keys {
		if v, ok := src[k]; ok && v != nil {
			dst[k] = v
		}
	}
}

// messStopToChatFinish Messages stop_reason → Chat finish_reason。
func messStopToChatFinish(reason string) string {
	switch reason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "refusal":
		return "content_filter"
	default:
		// end_turn / stop_sequence / pause_turn / compaction / 缺省
		return "stop"
	}
}

// chatFinishFromIncomplete Responses incomplete_details.reason → Chat finish_reason。
func chatFinishFromIncomplete(reason string) string {
	switch reason {
	case "content_filter":
		return "content_filter"
	case "steered":
		return "stop"
	default:
		// max_output_tokens / max_messages / 缺 reason
		return "length"
	}
}

// messStopFromIncomplete Responses incomplete_details.reason → Messages stop_reason。
func messStopFromIncomplete(reason string) string {
	switch reason {
	case "content_filter":
		return "refusal"
	case "steered":
		return "end_turn"
	default:
		return "max_tokens"
	}
}

// marshalAny 任意值 → JSON 字符串（失败 → "{}"）；function_call arguments
// 恒为字符串，这是其唯一合法表达。
func marshalAny(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// toolCallID 工具调用匹配键（修复）：Responses 规范中 function_call 项含
// 两个 ID——id（item id，fc_ 格式）与 call_id（工具调用匹配键，call_ 格式，
// function_call_output 必须按它回匹配）。对外暴露/内部合成的工具调用 ID 一律
// call_id 优先、item id 兜底（缺失防御），否则客户端回传的匹配键与上游不符，
// 多轮工具调用链断裂（上游 400 或静默断链）。
func toolCallID(im map[string]any) string {
	if id, ok := str(im, "call_id"); ok && id != "" {
		return id
	}
	id, _ := str(im, "id")
	return id
}

// parseJSON 解析 JSON 字符串 → any；解析失败/非字符串 → 空对象。
func parseJSON(v any) any {
	if s, ok := v.(string); ok && s != "" {
		var out any
		if err := json.Unmarshal([]byte(s), &out); err == nil {
			return out
		}
	}
	return map[string]any{}
}

// joinStrings 字符串数组拼接。
func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += sep + p
	}
	return out
}
