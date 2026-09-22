// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import "bytes"

// stripImageTools 剥离 response.create 帧中的图像工具（模板级开关开启时调用；
// 调用方负责开关分支——关闭 = 快照布尔读 + 不调用，零开销）。
//
// 热路径纪律（架构定稿 §5）：本函数以 "image" 子串预筛——无命中直接返回原
// 切片（零解析零分配）；命中才单遍扫描改写（scanTopLevelKeys 顶层键区间
// 定位 + scanTools 手写迭代器 + 工具级层级预筛 + 零序列化 splice 字节拼接，
// 见 strip_scan.go）。解析异常（tools 非数组等）→ 返回原帧原样转发
// （best-effort：上游按现状拒绝，行为不变）。
//
// 实现（2026-08-12-strip-optimize-spec v5，评审 PASS）：手写迭代器 + 零
// 序列化 splice 改写，替代现状 json.Unmarshal 全量 + json.Marshal(m) 全量
// 重编码（map 遍历 + 每值编码 + 分配）——改写降为 memcpy 级拼接。输出为
// splice 保留原始格式 vs 现状 Marshal 紧凑化——JSON 语义等价（现状测试
// 全部为 Unmarshal 后断言，口径不变）。先计数后分配：确认有剥除才分配
// 输出缓冲（预筛命中但无剥除路径不分配）。
//
// 边界（架构定稿）：只做 tools 数组 + tool_choice 悬挂；input 内嵌 v1 图像
// 内容不做（Responses Lite 下 tools 嵌入 input 的 AdditionalTools item——
// role developer 信封，图像工具以相同 namespace 形态嵌套其中——顶层 tools
// 剥离不触达，与现状一致）；只服务 resp 协议（chat/messages 不做； 转换
// 后为 resp 形态也覆盖）。剥离在客户端入站帧转发上游前执行（网关能力，不
// 依赖 SDK）——未来 resp-ws 帧流（response.create 帧）复用本函数。
//
// 图像工具形态（真实 Responses API / codex 客户端实证）：
//   - {"type":"image_generation_tool","namespace":"image_gen",...}（hosted
//     图像生成——线上 wire 实证（平台 hosted 形态）；codex-rs 源码无此
//     字面量，保留属防御性覆盖其他客户端）
//   - {"type":"image_edits",...}（图像编辑——同上，非 codex 产物）
//   - {"type":"namespace","name":"image_gen",...}（codex standalone namespace
//     工具 image_gen.imagegen——按 namespace/name == "image_gen" 判定）
//
// tool_choice 悬挂：对象形 tool_choice 的 type/name/namespace 指向已剥工具 →
// 删除 tool_choice 字段。Responses API 缺省 tool_choice = "auto"，移除 = 最简
// 正确语义（保留 "none"/"required"/"auto" 字符串形恒非悬挂）。
//
// 全剥语义（实证）：tools 全部被剥离 → 删除 tools 字段（缺省 =
// 无工具），不保留空数组 "tools":[]——删字段与缺省语义一致，是最稳形态
// （避免上游对空数组的兼容性不确定性；SDK 路径对 nil 切片同样省略该键）。
//
// 已知边角（裁决：接受不修，仅标注）：悬挂判定按标识集合匹配——
// 若保留的非图像工具恰与已剥工具同名（如 function 工具名恰为 "image_gen"），
// 指向它的 tool_choice 会被误判悬挂而移除。实测 codex 客户端无此形态（工具
// 名称空间由 namespace 隔离），误判影响 = tool_choice 退回 auto（仍可调用
// 全部保留工具），故接受。
//
// 已知边角（标注）：键名 \u 转义（如 "type" 键）不匹配剥离判定——
// 真实序列化器（serde_json 等）不产生转义 ASCII 键名，wire 不可达；如防
// 绕过需求则补键名 unescape（当前不做）。
func stripImageTools(body []byte) []byte {
	if !bytes.Contains(body, []byte("image")) {
		return body // 预筛：无命中零解析零分配直转
	}
	tools, toolChoice, toolsOK, toolChoiceOK := scanTopLevelKeys(body)
	if !toolsOK {
		return body // 无 tools 键：无工具可剥（input 内嵌 "image" 字样不受影响——边界）
	}
	toolsRaw := body[tools.valStart:tools.valEnd]
	if len(toolsRaw) == 0 || toolsRaw[0] != '[' {
		return body // tools 非数组（string/number/object/null）：原样转发，上游按现状拒绝
	}

	// 单遍扫描：先收集计数与保留工具区间，确认有剥除才分配输出缓冲（预筛
	// 命中但无剥除路径不分配——已剥标识栈数组、保留区间栈数组，均零分配）。
	var (
		nStripped int
		nKept     int
		sumLen    int // 保留工具裸字节数（元素间分隔符在拼接时计数）
	)
	var (
		keptBuf [16][]byte // 栈数组兜底：≤16 个保留工具零分配（50-tool 边界体溢栈转堆，实测记录）
		idsBuf  [8][]byte  // 已剥工具标识（type/name/namespace 值，body 子切片零拷贝）
	)
	kept := keptBuf[:0]
	ids := idsBuf[:0]
	for t := range scanTools(toolsRaw) {
		if isImageToolView(t) {
			ids = collectToolIDs(t, ids)
			nStripped++
			continue
		}
		kept = append(kept, t.Raw)
		nKept++
		sumLen += len(t.Raw)
	}
	if nStripped == 0 {
		return body // 无图像工具（"image" 命中在 input/描述等位置）：零改动原样直转
	}

	// 编辑区间：一次性计算全部区间（tools 替换/删除 + tool_choice 删除）后
	// 统一拼接——避免先删一处导致偏移移位。
	var delKeys [2]topLevelRange
	nDel := 0
	var toolsEdit bodyEdit
	hasToolsEdit := false
	if nKept == 0 {
		// 全剥：删除 tools 键（缺省 = 无工具，最稳语义；见函数注释）
		delKeys[nDel] = tools
		nDel++
	} else {
		// 部分剥：替换 tools 值区间为 '[' + 保留工具（',' 分隔）+ ']'
		toolsEdit = bodyEdit{start: tools.valStart, end: tools.valEnd, kind: editReplaceTools}
		hasToolsEdit = true
	}
	if toolChoiceOK {
		if toolChoiceDangles(body[toolChoice.valStart:toolChoice.valEnd], ids) {
			delKeys[nDel] = toolChoice
			nDel++
		}
	}

	// 删除区间：先按键名位置排序，再合并相邻删除 run（相邻键双双删除在共享
	// 逗号处重叠 1 字节）。run 边界规则（三位置逗号）：
	//   起点 = 首键前导逗号（run 始于对象首键则无前导逗号 → 键名起始）
	//   终点 = 末键值末；若 run 始于对象首键（起点为键名起始）且末键有后随
	//   逗号（其后仍有保留键）→ 终点含后随逗号（避免对象首键悬空前导逗号）
	var edits [3]bodyEdit
	nEdits := 0
	if nDel == 2 && delKeys[1].keyStart < delKeys[0].keyStart {
		delKeys[0], delKeys[1] = delKeys[1], delKeys[0]
	}
	for i := 0; i < nDel; {
		j := i
		for j+1 < nDel && delKeys[j+1].commaBefore == delKeys[j].commaAfter {
			j++ // 相邻删除：共享同一逗号
		}
		start := delKeys[i].commaBefore
		if start < 0 {
			start = delKeys[i].keyStart
		}
		end := delKeys[j].valEnd
		if start == delKeys[i].keyStart && delKeys[j].commaAfter >= 0 {
			end = delKeys[j].commaAfter + 1
		}
		edits[nEdits] = bodyEdit{start: start, end: end}
		nEdits++
		i = j + 1
	}
	if hasToolsEdit {
		edits[nEdits] = toolsEdit
		nEdits++
	}
	// 排序（≤3 区间）：tools 替换区间与删除区间不相交（值区间 vs 键区间），
	// 相邻（start == prev end）无碍。
	for i := 1; i < nEdits; i++ {
		for k := i; k > 0 && edits[k].start < edits[k-1].start; k-- {
			edits[k], edits[k-1] = edits[k-1], edits[k]
		}
	}

	// 精确容量预分配 + 零序列化 splice 拼接（容量 = 最终输出精确值，零增长）。
	outLen := len(body)
	for i := 0; i < nEdits; i++ {
		outLen -= edits[i].end - edits[i].start
	}
	if nKept > 0 {
		outLen += 2 + sumLen + nKept - 1 // '[' + 保留工具 + nKept-1 个 ',' + ']'
	}
	out := make([]byte, 0, outLen)
	prev := 0
	for i := 0; i < nEdits; i++ {
		e := edits[i]
		out = append(out, body[prev:e.start]...)
		if e.kind == editReplaceTools {
			out = append(out, '[')
			for j, r := range kept {
				if j > 0 {
					out = append(out, ',')
				}
				out = append(out, r...)
			}
			out = append(out, ']')
		}
		prev = e.end
	}
	out = append(out, body[prev:]...)
	return out
}

// bodyEdit 顶层编辑区间：删除 [start, end)；kind == editReplaceTools 时在该
// 位置插入 '[' + 保留工具（',' 分隔）+ ']'（tools 值区间替换）。
type bodyEdit struct {
	start, end int
	kind       editKind
}

type editKind uint8

const (
	editDelete editKind = iota
	editReplaceTools
)

// isImageToolView 判定工具对象是否为图像工具（规则原样，仅值比较载体从
// gjson String() 变为扫描提取字节——等价）：type ∈ {image_generation_tool,
// image_edits}；或 namespace 工具且 namespace/name == "image_gen"（codex
// standalone namespace 工具 image_gen.imagegen）。type 字段恒为小写——
// 预筛 "image" 子串必然命中（\u 转义形态经解码路径，与现状等价）。
func isImageToolView(t ToolView) bool {
	if bytes.Equal(t.Type, typeImageGenTool) || bytes.Equal(t.Type, typeImageEdits) {
		return true
	}
	if bytes.Equal(t.Type, typeNamespace) {
		ns := t.Namespace
		if len(ns) == 0 {
			ns = t.Name // 复刻现状 nsOf fallback（namespace 空则 name，strip_image.go 原 98-103 行）
		}
		return bytes.Equal(ns, nameImageGen)
	}
	return false
}

// collectToolIDs 收集已剥工具的标识（type/name/namespace，非空才收录）——
// tool_choice 悬挂判定集合。标识为 body 子切片（真零拷贝零分配——判定仅需
// 字节相等比较；map[string] 键需 string 拷贝（实测 1 alloc/id），热路径红线
// 换用字节集合）。值传递返回（避免指针逃逸——&ids 传非内联函数会使整个
// 栈切片逃逸堆上，无剥除路径也被迫分配，实测 1 alloc）。
func collectToolIDs(t ToolView, ids [][]byte) [][]byte {
	if len(t.Type) > 0 {
		ids = append(ids, t.Type)
	}
	if len(t.Name) > 0 {
		ids = append(ids, t.Name)
	}
	if len(t.Namespace) > 0 {
		ids = append(ids, t.Namespace)
	}
	return ids
}

// toolChoiceDangles 判定 tool_choice 是否悬挂（指向已剥工具）：仅对象形
// （首字符 '{'）——type/name/namespace 值 ∈ 已剥工具标识集合 → 悬挂。字符串
// 形（"auto"/"none"/"required"）恒非悬挂。
//
// 判定等价于现状 gjson 版（对合法客户端输入——spec：tool_choice 悬挂逻辑
// 零改动）；实现换为字节扫描（extractKeys 同款状态机，零分配）——与
// collectToolIDs 的 []byte 标识集匹配，无需 string 转换（strip 路径不再
// 调用 gjson；模块级仍 11 文件使用）。病态输入（tc 键重复等）差异方向
// 保守（不误删悬挂判定）。比较语义：tc 值经 \uXXXX 解码（对齐提取侧），
// 含其他转义的标识在两侧字节原样——与 gjson 全解码在 == 匹配上等价
// （见 extractKeys 注释）。
func toolChoiceDangles(tc []byte, ids [][]byte) bool {
	if len(tc) == 0 || tc[0] != '{' {
		return false
	}
	var tv ToolView
	extractKeys(tc, &tv)
	for _, v := range [3][]byte{tv.Type, tv.Name, tv.Namespace} {
		if len(v) == 0 {
			continue
		}
		for _, id := range ids {
			if bytes.Equal(v, id) {
				return true
			}
		}
	}
	return false
}
