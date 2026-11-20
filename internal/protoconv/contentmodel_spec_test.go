// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 内容模型的 spec 侧（test-only）：方向 role 期望集、成员名表、目标词表。
//
// 这些 decl 于 2026-11-21（U2）从 contentmodel.go **原样搬来**，生产零引用：
//
//   - dirRoles：方向 → 该方向必须覆盖的源 role 集（(a3) 的期望集）；
//   - members* / membersOf：shape ↔ 成员名（权威副本是 testdata/members.tsv，
//     (a) 断言两者一一对应）；
//   - 词表与 partVocab / eventVocabOf / inVocab：(b2)/(d′) 的唯一依据。
//
// 生产要的是**行数据**（shape → name/pol 的 lookupTable），它经 rowsOf/lookup
// 进入 mapsTo/emits，故 rows* 留在 contentmodel.go；本文件只装"只有测试读"的那半。
// 搬动边界由本文件末尾的 TestContentModelProdSurface 钉住。

// dirRoles 方向 → 该方向必须覆盖的源 role 集（(a3) 的期望集）。排除 anyRole
// 哨兵行后，各行掩码的并集必须包含它；每行的掩码又必须落在它之内。
var dirRoles = [dirCount]uint8{
	dirChatToResp:     rmSystem | rmDeveloper | rmUser | rmAsst | rmTool,
	dirChatToMess:     rmSystem | rmDeveloper | rmUser | rmAsst | rmTool,
	dirMessToResp:     rmUser | rmAsst,
	dirRespToMess:     rmSystem | rmDeveloper | rmUser | rmAsst,
	dirRespToChat:     rmAsst,
	dirRespToMessResp: rmAsst,
	dirMessToChatResp: rmAsst,
	// T8 请求面：Mess 的 message 只有 user/assistant 两种 role（system 在顶层，
	// 由 shMCTopSystem 的 anyRole 行承载）。
	dirMessToChatReq: rmUser | rmAsst,
	// T10 请求面：resp 的 item 没有 role 字段（role 在 message item 内部），
	// 但 message item 可以是 user/system/developer/assistant 四种。
	dirRespToChatReq: rmSystem | rmDeveloper | rmUser | rmAsst,
	// T9/T11 响应面：chat 的 choices[0].message 只有 assistant 一侧。
	dirChatToMessResp: rmAsst,
	dirChatToRespResp: rmAsst,
}

// member shape ↔ 成员名（成员名与 spec §3.2 逐字一致；权威副本是
// testdata/members.tsv，(a) 断言三者一一对应）。
type member struct {
	sh   shape
	name string
}

func membersChatToResp() []member {
	return []member{
		{shCSysdevText, "c_sysdev_text"},
		{shCUserText, "c_user_text"},
		{shCUserImgURL, "c_user_img_url"},
		{shCUserImgData, "c_user_img_data"},
		{shCUserImgBare64, "c_user_img_bare64"},
		{shCUserAudio, "c_user_audio"},
		{shCUserFileData, "c_user_file_data"},
		{shCUserFileID, "c_user_file_id"},
		{shCAsstText, "c_asst_text"},
		{shCAsstRefusal, "c_asst_refusal"},
		{shCAsstOtherParts, "c_asst_other_parts"},
		{shCAsstToolFunction, "c_asst_tool_function"},
		{shCAsstToolCustom, "c_asst_tool_custom"},
		{shCToolMsg, "c_tool_msg"},
		{shCFunctionMsg, "c_function_msg"},
		{shCTail, "c_tail"},
	}
}

func membersChatToMess() []member {
	return []member{
		{shCMSysdevText, "cm_sysdev_text"},
		{shCMUserText, "cm_user_text"},
		{shCMUserImgURL, "cm_user_img_url"},
		{shCMUserImgData, "cm_user_img_data"},
		{shCMUserImgBare64, "cm_user_img_bare64"},
		{shCMUserAudio, "cm_user_audio"},
		{shCMUserFileData, "cm_user_file_data"},
		{shCMUserFileID, "cm_user_file_id"},
		{shCMAsstText, "cm_asst_text"},
		{shCMAsstRefusal, "cm_asst_refusal"},
		{shCMAsstOtherParts, "cm_asst_other_parts"},
		{shCMAsstToolFunction, "cm_asst_tool_function"},
		{shCMAsstToolCustom, "cm_asst_tool_custom"},
		{shCMToolMsg, "cm_tool_msg"},
		{shCMFunctionMsg, "cm_function_msg"},
		{shCMTail, "cm_tail"},
	}
}

func membersMessToResp() []member {
	return []member{
		{shMUserText, "m_user_text"},
		{shMAsstText, "m_asst_text"},
		{shMUserImgBase64, "m_user_img_base64"},
		{shMUserImgURL, "m_user_img_url"},
		{shMUserImgFileID, "m_user_img_file_id"},
		{shMAsstImgBase64, "m_asst_img_base64"},
		{shMAsstImgURL, "m_asst_img_url"},
		{shMAsstImgFileID, "m_asst_img_file_id"},
		{shMUserDocB64TextContent, "m_user_doc_b64textcontent"},
		{shMUserDocFileID, "m_user_doc_file_id"},
		{shMUserDocURL, "m_user_doc_url"},
		{shMUserSearchResult, "m_user_search_result"},
		{shMAsstSearchResult, "m_asst_search_result"},
		{shMAsstToolUse, "m_asst_tool_use"},
		{shMToolResultString, "m_tool_result_string"},
		{shMToolResultArrayText, "m_tool_result_array_text"},
		{shMToolResultArrayNoText, "m_tool_result_array_notext"},
		{shMToolResultNoContent, "m_tool_result_no_content"},
		{shMToolResultNestedMedia, "m_tool_result_nested_media"},
		{shMThinking, "m_thinking"},
		{shMRedactedThinking, "m_redacted_thinking"},
		{shMTail, "m_tail"},
		{shMTopSystem, "m_top_system"},
	}
}

func membersRespToMess() []member {
	return []member{
		{shRMsgUserInputText, "r_msg_user_input_text"},
		{shRMsgUserOutputText, "r_msg_user_output_text"},
		{shRMsgAsstInputText, "r_msg_asst_input_text"},
		{shRMsgUserImgURL, "r_msg_user_img_url"},
		{shRMsgUserImgDataURL, "r_msg_user_img_data_url"},
		{shRMsgUserImgOther, "r_msg_user_img_other"},
		{shRMsgUserImgFileID, "r_msg_user_img_file_id"},
		{shRMsgUserFileURL, "r_msg_user_file_url"},
		{shRMsgUserFileID, "r_msg_user_file_id"},
		{shRMsgUserFileData, "r_msg_user_file_data"},
		{shRMsgUserInputAudio, "r_msg_user_input_audio"},
		{shRMsgSysdevText, "r_msg_sysdev_text"},
		{shRMsgAsstOutputText, "r_msg_asst_output_text"},
		{shRMsgAsstRefusal, "r_msg_asst_refusal"},
		{shRMsgAsstMedia, "r_msg_asst_media"},
		{shRFunctionCall, "r_function_call"},
		{shRReasoning, "r_reasoning"},
		{shRItemReference, "r_item_reference"},
		{shRTail, "r_tail"},
		{shRFcoString, "r_fco_string"},
		{shRFcoArrayText, "r_fco_array_text"},
		{shRFcoArrayNoText, "r_fco_array_notext"},
	}
}

func membersRespToChat() []member {
	return []member{
		{shXMessageOutputText, "x_message_output_text"},
		{shXMessageRefusal, "x_message_refusal"},
		{shXFunctionCall, "x_function_call"},
		{shXReasoning, "x_reasoning"},
		{shXMessageMedia, "x_message_media"},
		{shXTail, "x_tail"},
	}
}

func membersRespToMessResp() []member {
	return []member{
		{shYOutputText, "y_output_text"},
		{shYRefusal, "y_refusal"},
		{shYFunctionCall, "y_function_call"},
		{shYReasoning, "y_reasoning"},
		{shYTail, "y_tail"},
	}
}

func membersMessToChatResp() []member {
	return []member{
		{shZText, "z_text"},
		{shZThinking, "z_thinking"},
		{shZToolUse, "z_tool_use"},
		{shZMedia, "z_media"},
		{shZTail, "z_tail"},
	}
}

func membersMessToChatReq() []member {
	return []member{
		{shMCSysText, "mc_sys_text"},
		{shMCSysBlock, "mc_sys_block"},
		{shMCUserText, "mc_user_text"},
		{shMCUserImgB64, "mc_user_img_b64"},
		{shMCUserImgURL, "mc_user_img_url"},
		{shMCUserDoc, "mc_user_doc"},
		{shMCUserTR, "mc_user_tr"},
		{shMCUserTRErr, "mc_user_tr_err"},
		{shMCUserTRImg, "mc_user_tr_img"},
		{shMCAsstText, "mc_asst_text"},
		{shMCAsstToolUse, "mc_asst_tooluse"},
		{shMCAsstThinking, "mc_asst_thinking"},
		{shMCAsstRedacted, "mc_asst_redacted"},
		{shMCTools, "mc_tools"},
		{shMCToolStrict, "mc_tool_strict"},
		{shMCToolChoiceStr, "mc_tool_choice_str"},
		{shMCToolChoiceTool, "mc_tool_choice_tool"},
		{shMCToolChoiceParallel, "mc_tool_choice_parallel"},
		{shMCMaxTokens, "mc_max_tokens"},
		{shMCStop, "mc_stop"},
		{shMCMetaUser, "mc_meta_user"},
	}
}

func membersRespToChatReq() []member {
	return []member{
		{shRCInstructions, "rc_instructions"},
		{shRCInputStr, "rc_input_str"},
		{shRCMsgText, "rc_msg_text"},
		{shRCMsgImg, "rc_msg_img"},
		{shRCMsgFile, "rc_msg_file"},
		{shRCFunctionCall, "rc_fc"},
		{shRCFunctionCallOutput, "rc_fco"},
		{shRCReasoning, "rc_reasoning"},
		{shRCEncrypted, "rc_encrypted"},
		{shRCMaxOut, "rc_max_out"},
		{shRCTools, "rc_tools"},
		{shRCToolsStrict, "rc_tools_strict"},
		{shRCToolChoice, "rc_tool_choice"},
		{shRCTextFormat, "rc_text_format"},
		{shRCEffort, "rc_effort"},
		{shRCPrevID, "rc_prev_id"},
	}
}

func membersChatToMessResp() []member {
	return []member{
		{shCM2Text, "cm2_text"},
		{shCM2Tool, "cm2_tool"},
		{shCM2ToolBadArgs, "cm2_tool_badargs"},
		{shCM2Reasoning, "cm2_reasoning"},
		{shCM2Finish, "cm2_finish"},
		{shCM2UsageNet, "cm2_usage_net"},
		{shCM2UsageCached, "cm2_usage_cached"},
		{shCM2UsageWrite, "cm2_usage_write"},
		{shCM2UsageOut, "cm2_usage_out"},
		{shCM2DropReasoningTokens, "cm2_drop_reasoning_tokens"},
		{shCM2DropLogprobs, "cm2_drop_logprobs"},
	}
}

func membersChatToRespResp() []member {
	return []member{
		{shCCText, "cc_text"},
		{shCCFunctionCall, "cc_fc"},
		{shCCReasoning, "cc_reasoning"},
		{shCCStatus, "cc_status"},
		{shCCIncomplete, "cc_incomplete"},
		{shCCUsageNet, "cc_usage_net"},
		{shCCUsageCached, "cc_usage_cached"},
		{shCCUsageWrite, "cc_usage_write"},
		{shCCUsageOut, "cc_usage_out"},
		{shCCReasoningTokens, "cc_reasoning_tokens"},
	}
}

// membersOf 方向 → 成员清单。
func membersOf(d direction) []member {
	switch d {
	case dirChatToResp:
		return membersChatToResp()
	case dirChatToMess:
		return membersChatToMess()
	case dirMessToResp:
		return membersMessToResp()
	case dirRespToMess:
		return membersRespToMess()
	case dirRespToChat:
		return membersRespToChat()
	case dirRespToMessResp:
		return membersRespToMessResp()
	case dirMessToChatResp:
		return membersMessToChatResp()
	case dirMessToChatReq:
		return membersMessToChatReq()
	case dirRespToChatReq:
		return membersRespToChatReq()
	case dirChatToMessResp:
		return membersChatToMessResp()
	case dirChatToRespResp:
		return membersChatToRespResp()
	}
	return nil
}

// --- 词表（§3.5）：(b2) 与 (d′) 的唯一依据 ---

// partVocab 目标端部件/块类型词表（按 (目标协议, role)）。chat 目标没有部件
// 类型（content 是字符串，SSE 帧键另表），故返回 nil。
func partVocab(t target, r role) []string {
	switch t {
	case tgtResp:
		switch r {
		case roleUser:
			return []string{"input_text", "input_image", "input_file"}
		case roleSystem, roleDeveloper:
			return []string{"input_text"}
		case roleAssistant:
			return []string{"output_text", "refusal"}
		}
	case tgtMess:
		switch r {
		case roleUser:
			return []string{"text", "image", "tool_result", "document"}
		case roleAssistant:
			return []string{"text", "thinking", "redacted_thinking", "tool_use"}
		}
	}
	return nil
}

// itemVocabIn 请求向项类型（本仓会写出）。
var itemVocabIn = []string{"message", "function_call", "function_call_output", "reasoning"}

// itemVocabOut 响应向项类型。function_call_output 只属请求向（resp.txt:1078），
// 不得与 itemVocabIn 混用。
var itemVocabOut = []string{"message", "function_call", "reasoning"}

// chatKeyVocab chat 消息体键（行级）。
var chatKeyVocab = []string{"content", "tool_calls", "reasoning_content", "tool_call_id"}

// chatFrameKeys chat SSE **顶层**帧键（帧级；choices[].delta 的键仍查
// chatKeyVocab——两张表不得混用）。
var chatFrameKeys = []string{
	"id", "object", "created", "model", "choices", "index", "delta", "role",
	"finish_reason", "usage",
}

// fieldVocab 请求字段名。mess 目标的顶层 system 是 chat/resp 的
// system 折叠落点，resp 目标的对应字段是 instructions。
var fieldVocab = []string{"instructions", "system", "messages"}

// chatReqVocab **chat 请求**顶层键（T8 会写出的那些）。chat 的参数名与
// Anthropic 完全不同（max_completion_tokens / stop / 嵌套 tools[].function），
// 故不能与 fieldVocab 混用一张表：混了就等于两套语义互相背书，谁都查不出错。
//
// 只登记**有成员行**的键。model / stream / stream_options / temperature / top_p /
// reasoning_effort 直传直写、无成员行，故不入表（口径与既有四方向一致：内容模型
// 只覆盖"需要判断怎么落"的东西，不覆盖直传参数）。
var chatReqVocab = []string{
	"messages", "tools", "tool_choice", "parallel_tool_calls",
	"max_completion_tokens", "stop", "user",
	// 只有 resp→chat（T10）会写的两项。
	"response_format", "reasoning_effort",
}

// usageVocab 用量键（mess 与 resp 两个目标的并集；T9/T11 的 usage 行查这张表）。
// 目标用量对象的键由协议固定（不是"选词"问题），但"到底写了哪几个键"仍必须显式
// 登记——账务口径就是靠这几个键分别计价的（billing/cost.go 按 net + cr + cc）。
var usageVocab = []string{
	"input_tokens", "output_tokens", "total_tokens",
	"cache_read_input_tokens", "cache_creation_input_tokens",
	"input_tokens_details", "output_tokens_details",
}

// respEventVocab resp 目标的 SSE 事件名域（本仓会产出的事件）。
var respEventVocab = []string{
	"response.created",
	"response.output_item.added",
	"response.output_item.done",
	"response.output_text.delta",
	"response.reasoning_summary_text.delta",
	"response.function_call_arguments.delta",
	"response.completed",
	"response.failed",
	"error",
}

// messEventVocab mess 目标的 SSE 事件名域。
var messEventVocab = []string{
	"message_start",
	"content_block_start",
	"content_block_delta",
	"content_block_stop",
	"message_delta",
	"message_stop",
	"error",
}

// eventVocabOf 目标协议 → SSE 事件名域（chat 帧为 data-only，无事件名）。
func eventVocabOf(t target) []string {
	switch t {
	case tgtResp:
		return respEventVocab
	case tgtMess:
		return messEventVocab
	}
	return nil
}

// inVocab 名词是否在词表内（冷路径，线性扫可接受）。
func inVocab(vocab []string, name string) bool {
	for _, v := range vocab {
		if v == name {
			return true
		}
	}
	return false
}

// --- 搬动边界门禁 ---

// movedOutFuncs 从 contentmodel.go 搬到本文件的 15 个函数（生产零引用）。
var movedOutFuncs = []string{
	"membersChatToResp", "membersChatToMess", "membersMessToResp",
	"membersRespToMess", "membersRespToChat", "membersRespToMessResp",
	"membersMessToChatResp", "membersMessToChatReq", "membersRespToChatReq",
	"membersChatToMessResp", "membersChatToRespResp", "membersOf",
	"partVocab", "eventVocabOf", "inVocab",
}

// TestContentModelProdSurface contentmodel.go 的非方法函数集合**恰好等于**下面
// 列出的 16 项：集合相等，不是"至少包含"。
//
// 为什么不是只有"生产直接调用的 lookup/mapsTo/emits"：生产除了这三个入口，
// 还要它们脚下那张表。lookupTable 的包级初始化器 range rowsOf(d)，lookup 用
// roleBit(r) 造 role 掩码 —— 这 13 个函数是 shape → name/pol 行表的**生产侧写法**，
// 搬进 _test.go 会当场编译失败（undefined: rowsOf，见 contentmodel.go 的
// lookupTable）。所以要再收缩这个集合，得先改 lookupTable 的数据来源，那是"改数据
// 表示"，不属于"把只被测试读的 spec 表搬走"。
//
// 反向也钉住：往 contentmodel.go 里加第 17 个非方法函数就红 —— 防止把刚搬走的
// spec 逻辑以"再包一层 helper"的形式搬回来。
func TestContentModelProdSurface(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "contentmodel.go", nil, 0)
	require.NoError(t, err)

	var got []string
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv != nil {
			continue
		}
		got = append(got, fd.Name.Name)
	}
	sort.Strings(got)

	want := []string{
		"lookup", "mapsTo", "emits", // 生产直接调用的 3 个入口
		"roleBit", "rowsOf", // lookup 的掩码 / lookupTable 初始化器
		// 11 个方向的行表（shape → name/pol），lookupTable 的唯一数据源。
		"rowsChatToResp", "rowsChatToMess", "rowsMessToResp", "rowsRespToMess",
		"rowsRespToChat", "rowsRespToMessResp", "rowsMessToChatResp",
		"rowsMessToChatReq", "rowsRespToChatReq", "rowsChatToMessResp",
		"rowsChatToRespResp",
	}
	sort.Strings(want)
	require.Equal(t, want, got,
		"contentmodel.go 的非方法函数集合变了：生产只许留 lookup/mapsTo/emits + 行表数据源")

	// 搬走的 15 个不许换个生产文件搬回来 —— 只在 _test.go 里合法的东西，
	// 放进任何非测试文件都是一样的膨胀。
	prodOwner := map[string]string{}
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		pf, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		for _, d := range pf.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv != nil {
				continue
			}
			prodOwner[fd.Name.Name] = name
		}
	}
	for _, n := range movedOutFuncs {
		file, dup := prodOwner[n]
		require.False(t, dup, "%s 已搬进 contentmodel_spec_test.go，不许再出现在生产文件 %s", n, file)
	}
}
