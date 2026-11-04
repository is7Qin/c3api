// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

// reasoning / thinking 语义的载体换算（issue 46；spec:
// .omo/plans/spec-protoconv-reasoning.md，v7）。
//
// 两条硬约束：
//   - 约束 A（只针对上游）：不把无合法签名 / 不可回放的 thinking 块发给
//     Anthropic 上游（真 Anthropic 校验签名，缺失或错误 → 400）。
//   - 约束 B（只针对 mess 客户端）：给 Anthropic 客户端发 thinking 块时即使无
//     签名也带 "signature":""（客户端只展示，不校验；字段缺失才会被拒）。
//
// 两类信封（都以 sealPrefix 开头，靠解码后的 type 区分）：
//   - thinking / redacted_thinking：Anthropic 块往返（resp_to_mess 方向），放
//     reasoning item 的 encrypted_content；
//   - reasoning_ref：Responses 的不透明 encrypted_content 往返（mess_to_resp
//     方向），放在 thinking 块的 signature 里。
//
// 单一谓词 canReplay 决定「能否往返」，产出侧与回放侧必须调同一个函数。

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

const (
	// sealPrefix 信封前缀：客户端（codex 等）对 encrypted_content 原样回传，
	// 前缀让我们区分自有信封与别家不透明 blob。
	sealPrefix = "c3a1:"
	// maxThinkingBytes 单块原文上限（thinking 文本 / redacted data）。
	maxThinkingBytes = 256 << 10
	// maxEnvelopeBytes 信封总长上限（seal 之后的字节）。
	maxEnvelopeBytes = 512 << 10
	// reasoningRefType 第二类信封臂的类型标记。
	reasoningRefType = "reasoning_ref"
)

// thinkingBlock Anthropic thinking / redacted_thinking 块的可往返编码形态
// （字段与 Messages API 同名同义）。
type thinkingBlock struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
}

// reasoningRef Responses 的 reasoning 载体（第二类信封臂）：把上游不透明的
// encrypted_content 借 thinking 块的 signature 字段往返（mess_to_resp 方向）。
type reasoningRef struct {
	Type             string `json:"type"`
	EncryptedContent string `json:"encrypted_content"`
	ID               string `json:"id,omitempty"`
}

// sealBytes 块 → 信封字符串。超长 → ""（不可往返）。
func sealBytes(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	if len(raw) > maxEnvelopeBytes {
		return ""
	}
	out := sealPrefix + base64.RawURLEncoding.EncodeToString(raw)
	if len(out) > maxEnvelopeBytes {
		return ""
	}
	return out
}

// sealThinking thinking 块 → 信封（不可往返时返回 ""）。
func sealThinking(b thinkingBlock) string {
	if !canReplay(b) {
		return ""
	}
	return sealBytes(b)
}

// canReplay 块能否经信封往返：thinking 需非空签名、redacted 需非空 data，原文
// 与信封总长都在上限内。产出侧与回放侧共用本函数（禁止两套口径——否则会出现
// 「产出成功、回放被拒」导致的永久思考关闭）。
func canReplay(b thinkingBlock) bool {
	switch b.Type {
	case "thinking":
		if b.Signature == "" || len(b.Thinking) > maxThinkingBytes {
			return false
		}
	case "redacted_thinking":
		if b.Data == "" || len(b.Data) > maxThinkingBytes {
			return false
		}
	default:
		return false
	}
	return sealBytes(b) != ""
}

// openThinking 信封 → thinking 块。非本包 / 解码失败 / 不可往返 → false
// （按解码后的 type 判别：reasoning_ref 臂不在此处理）。
func openThinking(s string) (thinkingBlock, bool) {
	if !strings.HasPrefix(s, sealPrefix) || len(s) > maxEnvelopeBytes {
		return thinkingBlock{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(s[len(sealPrefix):])
	if err != nil {
		return thinkingBlock{}, false
	}
	var b thinkingBlock
	if err := json.Unmarshal(raw, &b); err != nil {
		return thinkingBlock{}, false
	}
	if !canReplay(b) {
		return thinkingBlock{}, false
	}
	return b, true
}

// sealReasoningRef reasoning_ref 臂 → signature 字符串（encrypted_content 为空
// 或超长 → ""，调用方须退化为 signature:""）。
func sealReasoningRef(r reasoningRef) string {
	if r.EncryptedContent == "" {
		return ""
	}
	r.Type = reasoningRefType
	return sealBytes(r)
}

// openReasoningRef signature → reasoning_ref 臂。判别键是解码后的 type，不是
// 前缀：thinking 型旧信封必须在这里被拒（否则会把 Anthropic 签名当
// encrypted_content 发给 Responses 上游）。
func openReasoningRef(s string) (reasoningRef, bool) {
	if !strings.HasPrefix(s, sealPrefix) || len(s) > maxEnvelopeBytes {
		return reasoningRef{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(s[len(sealPrefix):])
	if err != nil {
		return reasoningRef{}, false
	}
	var r reasoningRef
	if err := json.Unmarshal(raw, &r); err != nil {
		return reasoningRef{}, false
	}
	if r.Type != reasoningRefType || r.EncryptedContent == "" {
		return reasoningRef{}, false
	}
	return r, true
}

// thinkingBudgetByEffort effort → budget_tokens（用户拍板的表；none/minimal/
// 未知值不在表内 = 不开启）。
var thinkingBudgetByEffort = map[string]int64{
	"low":    2048,
	"medium": 8192,
	"high":   16384,
	"xhigh":  16384,
	"max":    16384,
}

// budgetForEffort effort → 表值（不开启 → 0）。
func budgetForEffort(effort string) int64 { return thinkingBudgetByEffort[effort] }

// effortForBudget budget_tokens → effort 反查（无 budget 的 adaptive → ""）。
func effortForBudget(n int64) string {
	switch {
	case n <= 0:
		return ""
	case n <= 2048:
		return "low"
	case n <= 8192:
		return "medium"
	default:
		return "high"
	}
}

// thinkingBudgetFor 生效 budget（no_raise：只按客户端上限压缩，绝不抬高）。
//
//	budget = min(表值, maxTokens-1024)，budget < 1024 → 不开启
func thinkingBudgetFor(effort string, maxTokens int64) (int64, bool) {
	b := budgetForEffort(effort)
	if b <= 0 || maxTokens <= 0 {
		return 0, false
	}
	b = min(b, maxTokens-1024)
	if b < 1024 {
		return 0, false
	}
	return b, true
}

// thinkingValue Anthropic 请求的 thinking 字段值字节（"" = 不写该键）。
func thinkingValue(effort string, maxTokens int64) string {
	b, ok := thinkingBudgetFor(effort, maxTokens)
	if !ok {
		return ""
	}
	return `{"type":"enabled","budget_tokens":` + strconv.FormatInt(b, 10) + `}`
}

// validMaxTokens 合法输出上限：JSON Number、1..1e9，且 Raw 不含小数点/指数。
// 小数（4096.9）、指数（1e20）、下划线分组（1_000）、JSON 字符串（"4096"）一律
// 非法，由调用方按「该字段不参与」处理（§3.1 逐字段判定）。
//
// 判据落在 strconv.ParseInt(raw, 10, 64)：它只接受可选正负号 + 十进制数字，
// 但它**容忍前导零与前导 '+'**，所以 `04096` / `+4096`（两者都不是合法 JSON
// 数字字面量）当前会被接受并归一为 4096。若要严格到 `-?[0-9]+`，需在此额外
// 做首字符 / 前导零校验——改判据属语义变更，未做。
//
// 不复用 gjsonNumInt：它会把 4096.9 截成 4096，导致「写出值」与「budget 基数」
// 不同源。
func validMaxTokens(v gjson.Result) (int64, bool) {
	if v.Type != gjson.Number {
		return 0, false
	}
	raw := v.Raw
	if raw == "" || strings.ContainsAny(raw, ".eE") {
		return 0, false
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1 || n > 1_000_000_000 {
		return 0, false
	}
	return n, true
}

// maxTokensField 按优先级取第一个合法上限（spec §3.1：非法/缺失 → 该字段不参与，
// 继续下一字段；全部不合法 → 默认 4096）。返回值**同时**用于写出与 budget。
func maxTokensField(vals ...gjson.Result) (int64, bool) {
	for _, v := range vals {
		if n, ok := validMaxTokens(v); ok {
			return n, true
		}
	}
	return defaultMaxTokens, false
}

// defaultMaxTokens 转换器写出 Anthropic 必填 max_tokens 的缺省值。
const defaultMaxTokens = 4096

// chatEffort chat 侧 reasoning_effort 取值（枚举白名单同 reasoningEffortRaw）。
func chatEffort(v gjson.Result) string {
	if reasoningEffortRaw(v) == "" {
		return ""
	}
	return v.Str
}

// messToolChoiceForces Messages tool_choice 是否强制调用工具（any / tool）：
// Anthropic 不接受 forced tool use 与 thinking 同时开启，冲突时保留强制意图。
func messToolChoiceForces(tc string) bool {
	if tc == "" {
		return false
	}
	return strings.Contains(tc, `"type":"any"`) || strings.HasPrefix(tc, `{"name":`)
}

// reasoningValue Anthropic thinking 参数（+ output_config.effort 覆盖）→
// Responses 目标的 reasoning 字段值（`{"effort":…,"summary":"auto"}`）；thinking
// 未开启 → ""。summary:"auto" 必带，否则上游只回 encrypted_content。
func reasoningValue(thinking, outputEffort gjson.Result) string {
	if !thinking.IsObject() {
		return ""
	}
	typ := thinking.Get("type")
	if !rawStrEq(typ.Raw, "enabled") && !rawStrEq(typ.Raw, "adaptive") {
		return ""
	}
	effort := effortForBudget(gjsonNumInt(thinking.Get("budget_tokens")))
	if outputEffort.IsObject() {
		if v := outputEffort.Get("effort"); reasoningEffortRaw(v) != "" {
			effort = v.Str
		}
	}
	if effort == "" {
		return `{"summary":"auto"}`
	}
	return `{"effort":` + strconv.Quote(effort) + `,"summary":"auto"}`
}

// reasoningEffortFromBody resp 请求 reasoning.effort 取值（枚举外 → ""）。
func reasoningEffortFromBody(reasoning gjson.Result) string {
	if !reasoning.IsObject() {
		return ""
	}
	return chatEffort(reasoning.Get("effort"))
}

// chatHistoryHasToolCalls chat 历史是否有带 tool_calls 的 assistant 轮：chat 无
// 签名通道，无法把思考回放给「要求回放」的 Anthropic 端点（否则该轮必 422）。
func chatHistoryHasToolCalls(msgs gjson.Result) bool {
	if !msgs.IsArray() {
		return false
	}
	found := false
	msgs.ForEach(func(_, m gjson.Result) bool {
		if !m.IsObject() || !rawStrEq(m.Get("role").Raw, "assistant") {
			return true
		}
		if tcs := m.Get("tool_calls"); tcs.IsArray() && len(tcs.Array()) > 0 {
			found = true
			return false
		}
		return true
	})
	return found
}

// isTurnOutputItem resp input 里是否「切轮」的 item：user message 或任意
// type 以 _output 结尾的项（function_call_output / custom_tool_call_output /
// local_shell_call_output / computer_call_output …）。
func isTurnOutputItem(item gjson.Result) bool {
	if !item.IsObject() {
		return false
	}
	if rawStrEq(item.Get("type").Raw, "message") && rawStrEq(item.Get("role").Raw, "user") {
		return true
	}
	return strings.HasSuffix(item.Get("type").Str, "_output")
}

// isContentItem 该 item 是否算「同轮正文」——只有 assistant message 与
// function_call 才算（spec §2.3）。
//
// 其余 item 既不切轮也不记正文：web_search_call / item_reference /
// computer_call / mcp_* 之类是 Responses 的内建工具或引用项，把它们当正文会让
// 「reasoning 在正文之前」的判据误命中，进而把整个请求降级成不带思考
// （一次降级还会自锁后续轮）。轮边界另有 isTurnOutputItem 负责。
func isContentItem(item gjson.Result) bool {
	typ := item.Get("type").Raw
	if rawStrEq(typ, "function_call") {
		return true
	}
	return rawStrEq(typ, "message") && rawStrEq(item.Get("role").Raw, "assistant")
}

// scanRespThinking spec §2.3 的「全有或全无」预扫：input 里每个 reasoning item
// 都能回放、且都出现在所属轮的工具调用/正文之前，才允许回放 thinking。
//
// 轮边界 = user message 或任意 *_output 项；正文 = assistant message 或
// function_call（isContentItem）；其余未知 item 忽略。单遍扫描，不改变写出顺序。
func scanRespThinking(input gjson.Result) bool {
	if !input.IsArray() {
		return true
	}
	replayable := true
	seenContent := false // 当前轮已出现正文（assistant message / function_call）
	input.ForEach(func(_, item gjson.Result) bool {
		if !item.IsObject() {
			return true
		}
		if isTurnOutputItem(item) {
			seenContent = false
			return true
		}
		if rawStrEq(item.Get("type").Raw, "reasoning") {
			if seenContent {
				// 同轮 reasoning 出现在正文/工具调用之后：顺序不可修复。
				replayable = false
				return false
			}
			enc := item.Get("encrypted_content")
			if enc.Type != gjson.String {
				replayable = false
				return false
			}
			if _, ok := openThinking(enc.Str); !ok {
				replayable = false
				return false
			}
			return true
		}
		if isContentItem(item) {
			seenContent = true
		}
		return true
	})
	return replayable
}

// respReasoningTextOf Responses reasoning item → 文本：summary[].text 优先，
// 否则 content[].type=="reasoning_text"。
func respReasoningTextOf(item map[string]any) string {
	var parts []string
	if s, ok := arr(item, "summary"); ok {
		for _, p := range s {
			if pm, ok := p.(map[string]any); ok {
				if t, ok := str(pm, "text"); ok && t != "" {
					parts = append(parts, t)
				}
			}
		}
	}
	if len(parts) == 0 {
		if c, ok := arr(item, "content"); ok {
			for _, p := range c {
				pm, ok := p.(map[string]any)
				if !ok || pm["type"] != "reasoning_text" {
					continue
				}
				if t, ok := str(pm, "text"); ok && t != "" {
					parts = append(parts, t)
				}
			}
		}
	}
	return joinStrings(parts, "")
}

// thinkingBlockMap thinking 块的 map 形态（map 组装路径用；键序无关）。
func thinkingBlockMap(b thinkingBlock) map[string]any {
	if b.Type == "redacted_thinking" {
		return map[string]any{"type": "redacted_thinking", "data": b.Data}
	}
	return map[string]any{"type": "thinking", "thinking": b.Thinking, "signature": b.Signature}
}

// appendThinkingBlock 把 thinking 块追加为 Messages 内容块字面量：thinking 恒
// 输出 signature（未签名 → 空串），redacted_thinking 只带不透明 data。
func appendThinkingBlock(out []byte, b thinkingBlock) []byte {
	if b.Type == "redacted_thinking" {
		out = append(out, `{"data":`...)
		out = appendJSONString(out, b.Data)
		return append(out, `,"type":"redacted_thinking"}`...)
	}
	out = append(out, `{"signature":`...)
	out = appendJSONString(out, b.Signature)
	out = append(out, `,"thinking":`...)
	out = appendJSONString(out, b.Thinking)
	return append(out, `,"type":"thinking"}`...)
}

// appendReasoningRefItem 把 reasoning_ref 臂还原成 Responses reasoning item
// 字面量（id 非空才写；不带 summary——臂里没有）。
func appendReasoningRefItem(out []byte, r reasoningRef) []byte {
	out = append(out, `{"encrypted_content":`...)
	out = appendJSONString(out, r.EncryptedContent)
	if r.ID != "" {
		out = append(out, `,"id":`...)
		out = appendJSONString(out, r.ID)
	}
	return append(out, `,"type":"reasoning"}`...)
}

// appendReasoningStripes Responses reasoning item 的文本以「去引号片段」形式
// 追加（chat 字符串拼接风格）。summary[].text 优先，空则 content[].reasoning_text。
func appendReasoningStripes(buf []byte, item gjson.Result) []byte {
	got := false
	if s := item.Get("summary"); s.IsArray() {
		s.ForEach(func(_, p gjson.Result) bool {
			if t := p.Get("text"); t.Type == gjson.String && len(t.Raw) > 2 {
				buf = append(buf, t.Raw[1:len(t.Raw)-1]...)
				got = true
			}
			return true
		})
	}
	if got {
		return buf
	}
	if c := item.Get("content"); c.IsArray() {
		c.ForEach(func(_, p gjson.Result) bool {
			if !rawStrEq(p.Get("type").Raw, "reasoning_text") {
				return true
			}
			if t := p.Get("text"); t.Type == gjson.String && len(t.Raw) > 2 {
				buf = append(buf, t.Raw[1:len(t.Raw)-1]...)
			}
			return true
		})
	}
	return buf
}
