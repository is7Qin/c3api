// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

// A3（顺序与配对）的唯一实现处：装配进代码，不接受散文回退（spec §3.3/J9）。
//
// 四个方向原先各写一遍同样的片段（assistant 轮收尾、tool_result 包裹、system
// 折叠），彼此漂移过（spec §1.2 R2）。这里收成三个助手，编码层只提供内容字节。
//
// 零分配约束：参数只用 (out []byte, 标量, 切片)；单趟 append；不得为组装新建
// 中间切片；热路径不得 Str()/string()。正文与 callID 因此以 string 传入（它们
// 本来就是 gjson 的零拷贝 Raw 切片），转 []byte 反而会多一次分配。

// roleName role → 协议里的角色名（常量字符串，零分配）。
func roleName(r role) string {
	switch r {
	case roleSystem:
		return "system"
	case roleDeveloper:
		return "developer"
	case roleUser:
		return "user"
	case roleAssistant:
		return "assistant"
	case roleTool:
		return "tool"
	}
	return ""
}

// appendAssistantTurn 收尾一个已打开的轮：`],"role":"<r>"}`。open=false 表示
// 当前没有打开的轮（no-op），调用方无需自己判空。
//
// assistant 轮允许以 function_call 结尾（mess.txt:154：末轮可为 assistant），
// 收尾位置由调用方按「下一个非 function_call/reasoning 项到达」决定——这一
// 配对规则只在本函数与调用点之间成立，不得在编码层重新推导。
func appendAssistantTurn(out []byte, r role, open bool) []byte {
	if !open {
		return out
	}
	out = append(out, `],"role":"`...)
	out = append(out, roleName(r)...)
	return append(out, '"', '}')
}

// appendToolResult 追加一个 tool 结果载体：
//
//	mess 目标 → 一条 user 消息里的 tool_result 块
//	resp 目标 → 顶层 function_call_output 项
//
// 不合成对应的 call（J11）：孤儿 tool_result 的存在与否由调用方判定（M2 起
// 按 §3.3 不变量 1 丢弃）。hasBody=false → mess 目标不写 content 键（resp→mess
// 的「output 数组只有图片」形态），resp 目标 output 必填故恒写（L16 由调用方
// 兜底为 `""`）。
func appendToolResult(out []byte, tgt target, callID, body string, hasBody bool) []byte {
	if tgt == tgtResp {
		out = append(out, `{"call_id":`...)
		out = append(out, callID...)
		out = append(out, `,"output":`...)
		out = append(out, body...)
		return append(out, `,"type":"function_call_output"}`...)
	}
	out = append(out, `{"content":[{`...)
	if hasBody {
		out = append(out, `"content":`...)
		out = append(out, body...)
		out = append(out, ',')
	}
	out = append(out, `"tool_use_id":`...)
	out = append(out, callID...)
	return append(out, `,"type":"tool_result"}],"role":"user"}`...)
}

// appendSystemFold 把若干文本片段折叠成一个 JSON 字符串字面量（system 折叠的
// 唯一实现）：chat→mess / resp→mess 的顶层 system 与 mess→resp 的 instructions
// 共用同一规则——按出现顺序以 \n 连接（模型对 system 只有一条通道）。分隔符按
// 目标协议选择（当前三个目标同口径）。
func appendSystemFold(out []byte, tgt target, texts []string) []byte {
	_ = tgt // 分隔符与目标无关（三个目标都是 "\n"）；形参锁定调用点的目标语义
	return appendJoinedTexts(out, texts, systemFoldSep)
}

// systemFoldSep system 多片段的连接符。三个目标同值：模型对 system 只有一条
// 文本通道，多片段只能拼成一条（mess.txt:193 无 system 角色，resp 的
// instructions 是单字符串字段）。
const systemFoldSep = "\n"

// foldSystemTexts 空片段集合 → ""（调用方据此省略字段），否则返回折叠后的
// JSON 字符串字面量。
func foldSystemTexts(tgt target, texts []string) string {
	if len(texts) == 0 {
		return ""
	}
	buf := make([]byte, 0, 64)
	buf = appendSystemFold(buf, tgt, texts)
	return string(buf)
}
