// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

// 内容模型：源形状（shape）→ 目标名 / 不可表达策略的唯一存放处（spec §3.1）。
//
// 四轴里两轴住在本文件，且**只住数据不写逻辑**：
//
//	A1 类型投影    rows 的 name（目标部件/项/字段/帧键名）
//	A2 不可表达    rows 的 pol（map/drop/omit/empty/text-degrade）
//	A4 停因        stopMapIncomplete + 本包既有的三个映射函数（测试钉住二者一致）
//
// A3（顺序与配对）在 assemble.go。散文不声明任何行数：成员清单的权威副本是
// testdata/members.tsv，行数据在下面的 rowsXXX 函数里，(a) 断言两者一一对应。
//
// 只被测试读的那半（members.tsv 的成员名表、方向 role 期望集、目标词表）在
// contentmodel_spec_test.go —— 生产不需要它们，就不进生产编译单元。
//
// 每个成员一个 shape，shape 全局唯一且 7 段两两不相交（(a2)），因此
// lookup 可以直接把 (dir, shape) 当下标用——热路径零扫描零分配。

// role 消息角色。row.roles 用位掩码表达「该行适用的 role 集合」。
type role uint8

const (
	roleInvalid role = iota
	roleSystem
	roleDeveloper
	roleUser
	roleAssistant
	roleTool
)

// roleBit role 在 row.roles 里的位。
func roleBit(r role) uint8 { return 1 << r }

const (
	rmSystem    uint8 = 1 << roleSystem
	rmDeveloper uint8 = 1 << roleDeveloper
	rmUser      uint8 = 1 << roleUser
	rmAsst      uint8 = 1 << roleAssistant
	rmTool      uint8 = 1 << roleTool
)

// part 目标端内容部件/块类型的枚举。词表本身是字符串（§3.5），part 只为
// 「同 shape 不同部件」的判据留位。
//
// 为 spec §3.1 类型集预留，**当前零消费者**（行数据用字符串 name 承载部件类型，
// 见 §2.0-A1）——不是漏接线的 bug，不要当死代码删。
type part uint8

const (
	partNone part = iota
	partText
	partImage
	partFile
	partAudio
	partRefusal
)

// kind row.name 的名词种类。四类名词混装会让一条词表断言盖住四种语义，
// 故 (b2) 按 kind 选词表（§3.5）。
type kind uint8

const (
	kindInvalid kind = iota
	kindPart         // 目标端内容部件/块类型（input_text / text / tool_use …）
	kindItem         // 项类型（message / function_call / function_call_output / reasoning）
	kindField        // 请求字段名（instructions / system）
	kindChatKey      // chat 消息体键（content / tool_calls / reasoning_content）
	kindUsage        // 用量键（mess / resp 响应的 usage 成员）
)

// policy 不可表达策略（A2）。**零值 = polUnknown**：未填表或 role 掩码外
// 一律显形为未定义策略，不得被当成「合法丢弃」。
type policy uint8

const (
	polUnknown     policy = iota
	polMap                // 目标有等价承载 → 按 name 写出
	polDrop               // 整块丢弃（不写任何字节）
	polOmit               // 已折叠到别处，不在此处单独写出
	polEmpty              // 目标必填容器写空值（L16：output 必填 → ""）；为 §3.1 预留，当前零行使用
	polTextDegrade        // 降级为 text 块（L12：mess 目标 refusal）
	polReject             // 目标无法承载且**不可静默丢弃** → 转换入口整请求 400
	//                       （T10 的 previous_response_id：丢了等于丢多轮历史，
	//                       客户端只会看到"模型变笨"，比 400 难排查得多）
)

// shape 源内容形状。全局唯一，与成员常量一一对应；7 段值域两两不相交。
type shape uint16

// 段基值：每方向一段，段内不连续也不与邻段交叠（(a2) 断言值域不相交）。
const (
	shapeNone shape = 0

	shapeBaseChatToResp   shape = 1    // T1  c_*
	shapeBaseChatToMess   shape = 101  // T2  cm_*
	shapeBaseMessToResp   shape = 201  // T3  m_*
	shapeBaseRespToMess   shape = 301  // T4  r_*
	shapeBaseRespToChat   shape = 401  // T5  x_*
	shapeBaseRespMess     shape = 501  // T6  y_*
	shapeBaseMessChat     shape = 601  // T7  z_*
	shapeBaseMessChatReq  shape = 701  // T8  mc_*
	shapeBaseChatMessResp shape = 801  // T9  cm2_*
	shapeBaseRespChatReq  shape = 901  // T10 rc_*
	shapeBaseChatRespResp shape = 1001 // T11 cc_*

	shapeCount shape = 1024 // 全部 shape 的值域上界（shape 直接作下标，见 shapeName）
)

// segWidth lookup 表的段内宽度：各段成员数的最大值（T3 = 23）。段内偏移从 0 起
// （第一个成员就是 segBase），"该槽位没有行"由回读 rw.sh != sh 判定，故没有哨兵
// 槽位。新增成员超出时 init 会带消息 panic。
const segWidth = 23

// segBase 方向 → 该方向 shape 段的基值。lookup 用段内偏移 sh-segBase[d] 作下标，
// 故表只需 [dirCount][segWidth]（7×23 行）而非 [dirCount][shapeCount]（7×608 行）
// 的宽表——仍然是一次下标 + 一次比较，无扫描。
var segBase = [dirCount]shape{
	dirChatToResp:     shapeBaseChatToResp,
	dirChatToMess:     shapeBaseChatToMess,
	dirMessToResp:     shapeBaseMessToResp,
	dirRespToMess:     shapeBaseRespToMess,
	dirRespToChat:     shapeBaseRespToChat,
	dirRespToMessResp: shapeBaseRespMess,
	dirMessToChatResp: shapeBaseMessChat,
	dirMessToChatReq:  shapeBaseMessChatReq,
	dirRespToChatReq:  shapeBaseRespChatReq,
	dirChatToMessResp: shapeBaseChatMessResp,
	dirChatToRespResp: shapeBaseChatRespResp,
}

// T1 chat→resp 成员（spec §3.2，全部 kindItem）。
const (
	shCSysdevText shape = shapeBaseChatToResp + iota
	shCUserText
	shCUserImgURL
	shCUserImgData
	shCUserImgBare64
	shCUserAudio
	shCUserFileData
	shCUserFileID
	shCAsstText
	shCAsstRefusal
	shCAsstOtherParts
	shCAsstToolFunction
	shCAsstToolCustom
	shCToolMsg
	shCFunctionMsg
	shCTail
)

// T2 chat→mess 成员（独立常量段，与 T1 同构成但不共享 shape）。
const (
	shCMSysdevText shape = shapeBaseChatToMess + iota
	shCMUserText
	shCMUserImgURL
	shCMUserImgData
	shCMUserImgBare64
	shCMUserAudio
	shCMUserFileData
	shCMUserFileID
	shCMAsstText
	shCMAsstRefusal
	shCMAsstOtherParts
	shCMAsstToolFunction
	shCMAsstToolCustom
	shCMToolMsg
	shCMFunctionMsg
	shCMTail
)

// T3 mess→resp 成员。
const (
	shMUserText shape = shapeBaseMessToResp + iota
	shMAsstText
	shMUserImgBase64
	shMUserImgURL
	shMUserImgFileID
	shMAsstImgBase64
	shMAsstImgURL
	shMAsstImgFileID
	shMUserDocB64TextContent
	shMUserDocFileID
	shMUserDocURL
	shMUserSearchResult
	shMAsstSearchResult
	shMAsstToolUse
	shMToolResultString
	shMToolResultArrayText
	shMToolResultArrayNoText
	shMToolResultNoContent
	shMToolResultNestedMedia
	shMThinking
	shMRedactedThinking
	shMTail
	shMTopSystem
)

// T4 resp→mess 成员（请求面）。
const (
	shRMsgUserInputText shape = shapeBaseRespToMess + iota
	shRMsgUserOutputText
	shRMsgAsstInputText
	shRMsgUserImgURL
	shRMsgUserImgDataURL
	shRMsgUserImgOther
	shRMsgUserImgFileID
	shRMsgUserFileURL
	shRMsgUserFileID
	shRMsgUserFileData
	shRMsgUserInputAudio
	shRMsgSysdevText
	shRMsgAsstOutputText
	shRMsgAsstRefusal
	shRMsgAsstMedia
	shRFunctionCall
	shRReasoning
	shRItemReference
	shRTail
	shRFcoString
	shRFcoArrayText
	shRFcoArrayNoText
)

// T5 resp→chat 成员（响应面）。
const (
	shXMessageOutputText shape = shapeBaseRespToChat + iota
	shXMessageRefusal
	shXFunctionCall
	shXReasoning
	shXMessageMedia
	shXTail
)

// T6 resp→mess 成员（响应面）。
const (
	shYOutputText shape = shapeBaseRespMess + iota
	shYRefusal
	shYFunctionCall
	shYReasoning
	shYTail
)

// T7 mess→chat 成员（响应面）。
const (
	shZText shape = shapeBaseMessChat + iota
	shZThinking
	shZToolUse
	shZMedia
	shZTail
)

// T8 mess→chat 成员（请求面，spec §6.2 T8 的 21 个，逐名对齐）。源形状与 T3
// 同构（Mess 内容块），但目标策略按 chat 的能力重排：文档类只有纯文本有载体、
// tool_result 的图片要迁到随后的 user 消息、签名无处承载。
//
// 与既有四方向不同，T8 有**顶层参数**成员（tools / tool_choice / stop / …）：
// chat 是唯一"参数名与 Anthropic 完全不同"的目标（`max_completion_tokens`、
// `stop`、嵌套 `tools[].function`），不建模就等于这些字段没有 anchor 可查。
const (
	shMCSysText shape = shapeBaseMessChatReq + iota
	shMCSysBlock
	shMCUserText
	shMCUserImgB64
	shMCUserImgURL
	shMCUserDoc
	shMCUserTR
	shMCUserTRErr
	shMCUserTRImg
	shMCAsstText
	shMCAsstToolUse
	shMCAsstThinking
	shMCAsstRedacted
	shMCTools
	shMCToolStrict
	shMCToolChoiceStr
	shMCToolChoiceTool
	shMCToolChoiceParallel
	shMCMaxTokens
	shMCStop
	shMCMetaUser
)

// T10 resp→chat 成员（请求面，spec §6.2 T10 的 16 个，逐名对齐）。
//
// 与 T8 同构：内容侧只有"文本 / 图片 / 文件(丢) / 工具对 / 思考 / 加密思考(丢)"
// 六类，其余是顶层参数。独有两点：`text.format` 要**重排结构**成 chat 的
// `response_format`，`previous_response_id` 等有状态字段**不可静默丢弃**（见
// polReject 与 rejectRespStateful）。
const (
	shRCInstructions shape = shapeBaseRespChatReq + iota
	shRCInputStr
	shRCMsgText
	shRCMsgImg
	shRCMsgFile
	shRCFunctionCall
	shRCFunctionCallOutput
	shRCReasoning
	shRCEncrypted
	shRCMaxOut
	shRCTools
	shRCToolsStrict
	shRCToolChoice
	shRCTextFormat
	shRCEffort
	shRCPrevID
)

// T9 chat→mess（**响应面**）成员（spec §6.2 T9 的 11 个）。
// 上游是 chat，客户端要 Messages：文本/工具/思考三块 + 停因 + 四项用量 + 两项
// "chat 有、mess 无"的丢弃（reasoning_tokens 明细、logprobs）。
const (
	shCM2Text shape = shapeBaseChatMessResp + iota
	shCM2Tool
	shCM2ToolBadArgs
	shCM2Reasoning
	shCM2Finish
	shCM2UsageNet
	shCM2UsageCached
	shCM2UsageWrite
	shCM2UsageOut
	shCM2DropReasoningTokens
	shCM2DropLogprobs
)

// T11 chat→resp（**响应面**）成员（spec §6.2 T11 的 10 个）。
// 与 T9 的差别：resp 的文本是 output[] 里的 message 项、工具是 function_call 项、
// 思考是 reasoning 项，且状态/不完整原因要写进 status/incomplete_details。
const (
	shCCText shape = shapeBaseChatRespResp + iota
	shCCFunctionCall
	shCCReasoning
	shCCStatus
	shCCIncomplete
	shCCUsageNet
	shCCUsageCached
	shCCUsageWrite
	shCCUsageOut
	shCCReasoningTokens
)

// direction 内容模型的方向（7 个方向各自一张表）。
type direction uint8

const (
	dirChatToResp direction = iota
	dirChatToMess
	dirMessToResp
	dirRespToMess
	dirRespToChat
	dirRespToMessResp
	dirMessToChatResp
	dirMessToChatReq
	dirRespToChatReq
	dirChatToMessResp
	dirChatToRespResp
	dirCount
)

// target 目标协议（A2 的落点；assemble 与 (b2) 的词表按它分派）。
type target uint8

const (
	tgtResp target = iota
	tgtMess
	tgtChat
)

// row 一行内容模型：一个 (direction, member) 的 A1/A2 取值。
//
// anyRole 是**独立哨兵**（不用 roles 位掩码表达）：true = 该行不受 role 限制
// （item/field/tail 类）；false 时按 roles 判。用 roleAny 全置位会让 (a3) 恒真，
// 且让未知 role 从「静默丢弃」升级为未定义策略。
type row struct {
	sh      shape
	anyRole bool
	roles   uint8
	kind    kind
	name    string
	pol     policy
	ann     bool // 目标部件是否附 annotations:[]（仅 resp assistant text）
}

// T1 chat→resp：按现状行为填表（assistant 文本已经修正为 output_text，
// assistant 的 image/audio/file 已经修正为 drop）。
func rowsChatToResp() []row {
	return []row{
		{sh: shCSysdevText, roles: rmSystem | rmDeveloper, kind: kindItem, name: "message", pol: polMap},
		{sh: shCUserText, roles: rmUser, kind: kindItem, name: "message", pol: polMap},
		{sh: shCUserImgURL, roles: rmUser, kind: kindItem, name: "message", pol: polMap},
		{sh: shCUserImgData, roles: rmUser, kind: kindItem, name: "message", pol: polMap},
		{sh: shCUserImgBare64, roles: rmUser, kind: kindItem, name: "message", pol: polMap},
		{sh: shCUserAudio, roles: rmUser, kind: kindItem, pol: polDrop},
		{sh: shCUserFileData, roles: rmUser, kind: kindItem, name: "message", pol: polMap},
		{sh: shCUserFileID, roles: rmUser, kind: kindItem, pol: polDrop},
		{sh: shCAsstText, roles: rmAsst, kind: kindItem, name: "message", pol: polMap, ann: true},
		{sh: shCAsstRefusal, roles: rmAsst, kind: kindItem, name: "message", pol: polMap},
		{sh: shCAsstOtherParts, roles: rmAsst, kind: kindItem, pol: polDrop},
		{sh: shCAsstToolFunction, roles: rmAsst, kind: kindItem, name: "function_call", pol: polMap},
		{sh: shCAsstToolCustom, roles: rmAsst, kind: kindItem, pol: polDrop},
		{sh: shCToolMsg, roles: rmTool, kind: kindItem, name: "function_call_output", pol: polMap},
		{sh: shCFunctionMsg, roles: rmTool, kind: kindItem, name: "function_call_output", pol: polMap},
		{sh: shCTail, anyRole: true, kind: kindItem, pol: polDrop},
	}
}

// T2 chat→mess：system/developer 折叠进顶层 system 字段，故 item 级为 omit。
func rowsChatToMess() []row {
	return []row{
		{sh: shCMSysdevText, roles: rmSystem | rmDeveloper, kind: kindItem, pol: polOmit},
		{sh: shCMUserText, roles: rmUser, kind: kindItem, name: "message", pol: polMap},
		{sh: shCMUserImgURL, roles: rmUser, kind: kindItem, name: "message", pol: polMap},
		{sh: shCMUserImgData, roles: rmUser, kind: kindItem, name: "message", pol: polMap},
		{sh: shCMUserImgBare64, roles: rmUser, kind: kindItem, pol: polDrop},
		{sh: shCMUserAudio, roles: rmUser, kind: kindItem, pol: polDrop},
		{sh: shCMUserFileData, roles: rmUser, kind: kindItem, pol: polDrop},
		{sh: shCMUserFileID, roles: rmUser, kind: kindItem, pol: polDrop},
		{sh: shCMAsstText, roles: rmAsst, kind: kindItem, name: "message", pol: polMap},
		{sh: shCMAsstRefusal, roles: rmAsst, kind: kindItem, name: "message", pol: polTextDegrade},
		{sh: shCMAsstOtherParts, roles: rmAsst, kind: kindItem, pol: polDrop},
		{sh: shCMAsstToolFunction, roles: rmAsst, kind: kindItem, name: "message", pol: polMap},
		{sh: shCMAsstToolCustom, roles: rmAsst, kind: kindItem, pol: polDrop},
		{sh: shCMToolMsg, roles: rmTool, kind: kindItem, name: "message", pol: polMap},
		{sh: shCMFunctionMsg, roles: rmTool, kind: kindItem, name: "message", pol: polMap},
		{sh: shCMTail, anyRole: true, kind: kindItem, pol: polDrop},
	}
}

// T3 mess→resp：顶层 system 折叠进 instructions（字段级，无 role 维度）。
// 非文本输入部件（image/document/search_result）当前无 resp 承载 → drop，
// 逐行纠错在 M2。
func rowsMessToResp() []row {
	return []row{
		{sh: shMUserText, roles: rmUser, kind: kindItem, name: "message", pol: polMap},
		{sh: shMAsstText, roles: rmAsst, kind: kindItem, name: "message", pol: polMap},
		{sh: shMUserImgBase64, roles: rmUser, kind: kindItem, name: "message", pol: polMap},
		{sh: shMUserImgURL, roles: rmUser, kind: kindItem, name: "message", pol: polMap},
		{sh: shMUserImgFileID, roles: rmUser, kind: kindItem, pol: polDrop},
		{sh: shMAsstImgBase64, roles: rmAsst, kind: kindItem, pol: polDrop},
		{sh: shMAsstImgURL, roles: rmAsst, kind: kindItem, pol: polDrop},
		{sh: shMAsstImgFileID, roles: rmAsst, kind: kindItem, pol: polDrop},
		{sh: shMUserDocB64TextContent, roles: rmUser, kind: kindItem, pol: polDrop},
		{sh: shMUserDocFileID, roles: rmUser, kind: kindItem, pol: polDrop},
		{sh: shMUserDocURL, roles: rmUser, kind: kindItem, name: "message", pol: polMap},
		{sh: shMUserSearchResult, roles: rmUser, kind: kindItem, pol: polDrop},
		{sh: shMAsstSearchResult, roles: rmAsst, kind: kindItem, pol: polDrop},
		{sh: shMAsstToolUse, roles: rmAsst, kind: kindItem, name: "function_call", pol: polMap},
		{sh: shMToolResultString, roles: rmUser, kind: kindItem, name: "function_call_output", pol: polMap},
		{sh: shMToolResultArrayText, roles: rmUser, kind: kindItem, name: "function_call_output", pol: polMap},
		{sh: shMToolResultArrayNoText, roles: rmUser, kind: kindItem, name: "function_call_output", pol: polMap},
		{sh: shMToolResultNoContent, roles: rmUser, kind: kindItem, name: "function_call_output", pol: polMap},
		{sh: shMToolResultNestedMedia, roles: rmUser, kind: kindItem, name: "function_call_output", pol: polMap},
		{sh: shMThinking, roles: rmAsst, kind: kindItem, name: "reasoning", pol: polMap},
		{sh: shMRedactedThinking, roles: rmAsst, kind: kindItem, name: "reasoning", pol: polMap},
		{sh: shMTail, anyRole: true, kind: kindItem, pol: polDrop},
		{sh: shMTopSystem, anyRole: true, kind: kindField, name: "instructions", pol: polMap},
	}
}

// T4 resp→mess（请求面）。resp 的 message 部件按 type 识别（读宽容），role 只
// 决定落哪一行——`input_text` 与 `output_text` 在两种 role 下都当文本搬运。
// assistant 的图片/文件/音频一律 ∅（J2：Messages assistant 不写 image/document）。
func rowsRespToMess() []row {
	return []row{
		{sh: shRMsgUserInputText, roles: rmUser, kind: kindItem, name: "message", pol: polMap},
		{sh: shRMsgUserOutputText, roles: rmUser, kind: kindItem, name: "message", pol: polMap},
		{sh: shRMsgAsstInputText, roles: rmAsst, kind: kindItem, name: "message", pol: polMap},
		{sh: shRMsgUserImgURL, roles: rmUser, kind: kindItem, name: "message", pol: polMap},
		{sh: shRMsgUserImgDataURL, roles: rmUser, kind: kindItem, name: "message", pol: polMap},
		{sh: shRMsgUserImgOther, roles: rmUser, kind: kindItem, pol: polDrop},
		{sh: shRMsgUserImgFileID, roles: rmUser, kind: kindItem, pol: polDrop},
		{sh: shRMsgUserFileURL, roles: rmUser, kind: kindItem, name: "message", pol: polMap},
		{sh: shRMsgUserFileID, roles: rmUser, kind: kindItem, pol: polDrop},
		{sh: shRMsgUserFileData, roles: rmUser, kind: kindItem, pol: polDrop},
		{sh: shRMsgUserInputAudio, roles: rmUser, kind: kindItem, pol: polDrop},
		{sh: shRMsgSysdevText, roles: rmSystem | rmDeveloper, kind: kindItem, pol: polOmit},
		{sh: shRMsgAsstOutputText, roles: rmAsst, kind: kindItem, name: "message", pol: polMap},
		{sh: shRMsgAsstRefusal, roles: rmAsst, kind: kindItem, name: "message", pol: polTextDegrade},
		{sh: shRMsgAsstMedia, roles: rmAsst, kind: kindItem, pol: polDrop},
		{sh: shRFunctionCall, roles: rmAsst, kind: kindItem, name: "function_call", pol: polMap},
		{sh: shRReasoning, roles: rmAsst, kind: kindItem, name: "reasoning", pol: polMap},
		{sh: shRItemReference, anyRole: true, kind: kindItem, pol: polDrop},
		{sh: shRTail, anyRole: true, kind: kindItem, pol: polDrop},
		{sh: shRFcoString, roles: rmUser, kind: kindPart, name: "tool_result", pol: polMap},
		{sh: shRFcoArrayText, roles: rmUser, kind: kindPart, name: "tool_result", pol: polMap},
		{sh: shRFcoArrayNoText, roles: rmUser, kind: kindPart, name: "tool_result", pol: polMap},
	}
}

// T5 resp→chat（响应面）：chat 目标只有纯文本通道，content 恒为字符串。
func rowsRespToChat() []row {
	return []row{
		{sh: shXMessageOutputText, roles: rmAsst, kind: kindChatKey, name: "content", pol: polMap},
		{sh: shXMessageRefusal, roles: rmAsst, kind: kindChatKey, name: "content", pol: polMap},
		{sh: shXFunctionCall, roles: rmAsst, kind: kindChatKey, name: "tool_calls", pol: polMap},
		{sh: shXReasoning, roles: rmAsst, kind: kindChatKey, name: "reasoning_content", pol: polMap},
		{sh: shXMessageMedia, roles: rmAsst, kind: kindChatKey, pol: polDrop},
		{sh: shXTail, anyRole: true, kind: kindChatKey, pol: polDrop},
	}
}

// T6 resp→mess（响应面）：refusal 降级为 text 的接线在 M2。
func rowsRespToMessResp() []row {
	return []row{
		{sh: shYOutputText, roles: rmAsst, kind: kindPart, name: "text", pol: polMap},
		{sh: shYRefusal, roles: rmAsst, kind: kindPart, name: "text", pol: polTextDegrade},
		{sh: shYFunctionCall, roles: rmAsst, kind: kindPart, name: "tool_use", pol: polMap},
		{sh: shYReasoning, roles: rmAsst, kind: kindPart, name: "thinking", pol: polMap},
		{sh: shYTail, anyRole: true, kind: kindPart, pol: polDrop},
	}
}

// T7 mess→chat（响应面）。
func rowsMessToChatResp() []row {
	return []row{
		{sh: shZText, roles: rmAsst, kind: kindChatKey, name: "content", pol: polMap},
		{sh: shZThinking, roles: rmAsst, kind: kindChatKey, name: "reasoning_content", pol: polMap},
		{sh: shZToolUse, roles: rmAsst, kind: kindChatKey, name: "tool_calls", pol: polMap},
		{sh: shZMedia, roles: rmAsst, kind: kindChatKey, pol: polDrop},
		{sh: shZTail, anyRole: true, kind: kindChatKey, pol: polDrop},
	}
}

// rowsOf 方向 → 行数据（测试与装配表共用）。
func rowsOf(d direction) []row {
	switch d {
	case dirChatToResp:
		return rowsChatToResp()
	case dirChatToMess:
		return rowsChatToMess()
	case dirMessToResp:
		return rowsMessToResp()
	case dirRespToMess:
		return rowsRespToMess()
	case dirRespToChat:
		return rowsRespToChat()
	case dirRespToMessResp:
		return rowsRespToMessResp()
	case dirMessToChatResp:
		return rowsMessToChatResp()
	case dirMessToChatReq:
		return rowsMessToChatReq()
	case dirRespToChatReq:
		return rowsRespToChatReq()
	case dirChatToMessResp:
		return rowsChatToMessResp()
	case dirChatToRespResp:
		return rowsChatToRespResp()
	}
	return nil
}

// dirName 方向 → testdata 里的目录名（members.tsv / anchors.tsv 的 dir 列）。
var dirName = [dirCount]string{
	dirChatToResp:     "chat_to_resp",
	dirChatToMess:     "chat_to_mess",
	dirMessToResp:     "mess_to_resp",
	dirRespToMess:     "resp_to_mess",
	dirRespToChat:     "resp_to_chat",
	dirRespToMessResp: "resp_to_mess_resp",
	dirMessToChatResp: "mess_to_chat_resp",
	dirMessToChatReq:  "mess_to_chat_req",
	dirRespToChatReq:  "resp_to_chat_req",
	dirChatToMessResp: "chat_to_mess_resp",
	dirChatToRespResp: "chat_to_resp_resp",
}

// lookupTable (dir, shape) → 行数据的静态数组：index = sh - segBase[dir]（段基值
// 见 segBase），一次下标零扫描零分配。空槽位（该方向没有这个 shape）由回读
// rw.sh != sh 判定。
var lookupTable = func() [dirCount][segWidth]row {
	var t [dirCount][segWidth]row
	for d := direction(0); d < dirCount; d++ {
		for _, rw := range rowsOf(d) {
			off := int(rw.sh) - int(segBase[d])
			if off < 0 || off >= segWidth {
				// 静态数据自检：新增成员却没抬 segWidth 时，这里给出比
				// 「index out of range」更可读的失败（任何测试跑一次即暴露）。
				panic("protoconv: shape 段内偏移越界，请抬 segWidth")
			}
			t[d][off] = rw
		}
	}
	return t
}()

// lookup 取 (dir, shape, role) 的策略行。ok=false 表示该 role 不适用该行
// （或该方向没有这个成员），此时返回零值行 + polUnknown：**掩码外恒为
// polUnknown，不得返回 polDrop**——漏列 role 必须显形为未定义策略，而不是
// 被当成合法丢弃。anyRole 行永不踩空（不受 role 限制）。
func lookup(d direction, sh shape, r role) (row, bool) {
	off := int(sh) - int(segBase[d])
	if off < 0 || off >= segWidth {
		return row{}, false
	}
	rw := lookupTable[d][off]
	// 段内偏移会与其他段的 shape 撞值（如 T4 方向收到 T7 的 305 → off=4），
	// 故必须回读 sh 判同，不能只看非零。
	if rw.sh != sh {
		return row{}, false
	}
	if rw.anyRole {
		return rw, true
	}
	if rw.roles&roleBit(r) == 0 {
		return row{sh: sh, pol: polUnknown}, false
	}
	return rw, true
}

// mapsTo 该 (dir, shape, role) 是否按 name 原样写出（唯一的「要不要写」判据）。
func mapsTo(d direction, sh shape, r role) (row, bool) {
	rw, ok := lookup(d, sh, r)
	if !ok || rw.pol != polMap {
		return rw, false
	}
	return rw, true
}

// emits 该行是否**写出内容**：polMap（按 name 原样）与 polTextDegrade（降级为
// text 块）都写出，只是形态不同；其余策略（drop/omit/empty/unknown）不写。
func emits(d direction, sh shape, r role) (row, bool) {
	rw, ok := lookup(d, sh, r)
	if !ok || (rw.pol != polMap && rw.pol != polTextDegrade) {
		return rw, false
	}
	return rw, true
}

// --- A4：停止原因 / 状态（§2.4）---

// stopRow resp 的 incomplete_details.reason → (Messages stop_reason,
// Chat finish_reason)。resp 侧自身保留 incomplete + reason 原值。
type stopRow struct {
	incomplete string
	mess       string
	chat       string
}

// stopMapIncomplete 三个目标的停因换算（唯一真相；既有换算函数由测试钉住与
// 本表一致）。
var stopMapIncomplete = []stopRow{
	{incomplete: "content_filter", mess: "refusal", chat: "content_filter"},
	{incomplete: "max_output_tokens", mess: "max_tokens", chat: "length"},
	{incomplete: "max_messages", mess: "max_tokens", chat: "length"},
	{incomplete: "steered", mess: "end_turn", chat: "stop"},
}

// stopDefaultIncomplete 未登记的 reason 的落点（max_output_tokens 同口径）。
var stopDefaultIncomplete = stopRow{incomplete: "", mess: "max_tokens", chat: "length"}

// messStopToChat 表：Messages stop_reason → Chat finish_reason。
var messStopToChat = []struct{ mess, chat string }{
	{"tool_use", "tool_calls"},
	// F4：`function_call` 是 chat 官方 deprecated 的 finish_reason。它排在
	// tool_calls **之后**：正向（mess→chat）首命中仍是 tool_calls，行为不变；
	// 反向（chat→mess，messStopFromChat）才能查到它 —— 否则 chat 全集里这个
	// 值会掉进 default 得 end_turn，客户端看到"正常结束"而实际是工具调用。
	{"tool_use", "function_call"},
	{"max_tokens", "length"},
	{"model_context_window_exceeded", "length"},
	{"refusal", "content_filter"},
	// 显式登记 end_turn：值不变（与 default 同），但"这个值有定义"从兜底变成
	// 表命中 —— messStopFromChat 反向扫得到它，(e) 的像集断言才有意义。
	{"end_turn", "stop"},
}

// messStopToChatDefault end_turn / stop_sequence / pause_turn / compaction / 缺省。
const messStopToChatDefault = "stop"

// T8 mess→chat（请求面）：源是 Mess 内容块，目标策略按 chat 能力定。与 T3 的
// 差别集中在四处：document 只有纯文本有载体、tool_result 的图片要迁走、
// assistant 图片无对应位置、thinking 只能保留纯文本（chat 无签名通道）。
func rowsMessToChatReq() []row {
	return []row{
		// 顶层 system（字符串或块数组）折叠成首条 role:"system" 消息。
		{sh: shMCSysText, anyRole: true, kind: kindField, name: "messages", pol: polMap},
		{sh: shMCSysBlock, anyRole: true, kind: kindField, name: "messages", pol: polMap},
		// user 内容块。
		{sh: shMCUserText, roles: rmUser, kind: kindChatKey, name: "content", pol: polMap},
		{sh: shMCUserImgB64, roles: rmUser, kind: kindChatKey, name: "content", pol: polMap},
		{sh: shMCUserImgURL, roles: rmUser, kind: kindChatKey, name: "content", pol: polMap},
		{sh: shMCUserDoc, roles: rmUser, kind: kindChatKey, pol: polDrop},
		// tool_result：只有 tool 消息有载体（tool_call_id + 文本 content）。
		{sh: shMCUserTR, roles: rmUser, kind: kindChatKey, name: "tool_call_id", pol: polMap},
		{sh: shMCUserTRErr, roles: rmUser, kind: kindChatKey, name: "tool_call_id", pol: polMap},
		{sh: shMCUserTRImg, roles: rmUser, kind: kindChatKey, name: "tool_call_id", pol: polMap},
		// assistant 内容块。
		{sh: shMCAsstText, roles: rmAsst, kind: kindChatKey, name: "content", pol: polMap},
		{sh: shMCAsstToolUse, roles: rmAsst, kind: kindChatKey, name: "tool_calls", pol: polMap},
		{sh: shMCAsstThinking, roles: rmAsst, kind: kindChatKey, name: "reasoning_content", pol: polMap},
		{sh: shMCAsstRedacted, roles: rmAsst, kind: kindChatKey, pol: polDrop},
		// 顶层参数（字段级，无 role 维度）。
		{sh: shMCTools, anyRole: true, kind: kindField, name: "tools", pol: polMap},
		{sh: shMCToolStrict, anyRole: true, kind: kindField, name: "tools", pol: polMap},
		{sh: shMCToolChoiceStr, anyRole: true, kind: kindField, name: "tool_choice", pol: polMap},
		{sh: shMCToolChoiceTool, anyRole: true, kind: kindField, name: "tool_choice", pol: polMap},
		{sh: shMCToolChoiceParallel, anyRole: true, kind: kindField, name: "parallel_tool_calls", pol: polMap},
		{sh: shMCMaxTokens, anyRole: true, kind: kindField, name: "max_completion_tokens", pol: polMap},
		{sh: shMCStop, anyRole: true, kind: kindField, name: "stop", pol: polMap},
		{sh: shMCMetaUser, anyRole: true, kind: kindField, name: "user", pol: polMap},
	}
}

// T10 resp→chat：resp 的 item 是开放联合，能搬过来的只有文本/图片/工具对/思考；
// 文件与加密思考无载体（丢弃），有状态字段整请求拒绝。
func rowsRespToChatReq() []row {
	return []row{
		{sh: shRCInstructions, anyRole: true, kind: kindField, name: "messages", pol: polMap},
		{sh: shRCInputStr, anyRole: true, kind: kindField, name: "messages", pol: polMap},
		{sh: shRCMsgText, roles: rmSystem | rmDeveloper | rmUser, kind: kindChatKey, name: "content", pol: polMap},
		{sh: shRCMsgImg, roles: rmUser, kind: kindChatKey, name: "content", pol: polMap},
		{sh: shRCMsgFile, roles: rmUser, kind: kindChatKey, pol: polDrop},
		{sh: shRCFunctionCall, roles: rmAsst, kind: kindChatKey, name: "tool_calls", pol: polMap},
		{sh: shRCFunctionCallOutput, anyRole: true, kind: kindChatKey, name: "tool_call_id", pol: polMap},
		{sh: shRCReasoning, roles: rmAsst, kind: kindChatKey, name: "reasoning_content", pol: polMap},
		{sh: shRCEncrypted, roles: rmAsst, kind: kindChatKey, pol: polDrop},
		{sh: shRCMaxOut, anyRole: true, kind: kindField, name: "max_completion_tokens", pol: polMap},
		{sh: shRCTools, anyRole: true, kind: kindField, name: "tools", pol: polMap},
		{sh: shRCToolsStrict, anyRole: true, kind: kindField, name: "tools", pol: polMap},
		{sh: shRCToolChoice, anyRole: true, kind: kindField, name: "tool_choice", pol: polMap},
		{sh: shRCTextFormat, anyRole: true, kind: kindField, name: "response_format", pol: polMap},
		{sh: shRCEffort, anyRole: true, kind: kindField, name: "reasoning_effort", pol: polMap},
		// 有状态字段的代表行：策略是 polReject（整请求 400），不是 polDrop。
		// 另外三个同族字段（conversation / prompt / context_management）行为相同，
		// 由 rejectRespStateful 的列表承载，不各占一行。
		{sh: shRCPrevID, anyRole: true, kind: kindField, name: "previous_response_id", pol: polReject},
	}
}

// T9 chat→mess（响应面）：assistant 只有文本/工具/思考三种块可搬；停因与四项用量
// 恒写；chat 的 reasoning_tokens 明细与 logprobs 在 Messages 里没有承载位。
func rowsChatToMessResp() []row {
	return []row{
		{sh: shCM2Text, roles: rmAsst, kind: kindPart, name: "text", pol: polMap},
		{sh: shCM2Tool, roles: rmAsst, kind: kindPart, name: "tool_use", pol: polMap},
		{sh: shCM2ToolBadArgs, roles: rmAsst, kind: kindPart, name: "tool_use", pol: polMap},
		{sh: shCM2Reasoning, roles: rmAsst, kind: kindPart, name: "thinking", pol: polMap},
		// 停因与状态是**取值映射**（值域由 messStopFromChat / chatRespStatus 的表
		// 钉住，(e) 门禁覆盖），故 name 为空：它们不选键，只选值。
		{sh: shCM2Finish, anyRole: true, kind: kindField, pol: polMap},
		{sh: shCM2UsageNet, anyRole: true, kind: kindUsage, name: "input_tokens", pol: polMap},
		{sh: shCM2UsageCached, anyRole: true, kind: kindUsage, name: "cache_read_input_tokens", pol: polMap},
		{sh: shCM2UsageWrite, anyRole: true, kind: kindUsage, name: "cache_creation_input_tokens", pol: polMap},
		{sh: shCM2UsageOut, anyRole: true, kind: kindUsage, name: "output_tokens", pol: polMap},
		{sh: shCM2DropReasoningTokens, anyRole: true, kind: kindUsage, pol: polDrop},
		{sh: shCM2DropLogprobs, anyRole: true, kind: kindUsage, pol: polDrop},
	}
}

// T11 chat→resp（响应面）：resp 把内容装进 output[] 的**项**里（message /
// function_call / reasoning），并额外要求 status 与 incomplete_details。
func rowsChatToRespResp() []row {
	return []row{
		{sh: shCCText, roles: rmAsst, kind: kindPart, name: "output_text", pol: polMap},
		{sh: shCCFunctionCall, roles: rmAsst, kind: kindItem, name: "function_call", pol: polMap},
		{sh: shCCReasoning, roles: rmAsst, kind: kindItem, name: "reasoning", pol: polMap},
		{sh: shCCStatus, anyRole: true, kind: kindField, pol: polMap},
		{sh: shCCIncomplete, anyRole: true, kind: kindField, pol: polMap},
		{sh: shCCUsageNet, anyRole: true, kind: kindUsage, name: "input_tokens", pol: polMap},
		{sh: shCCUsageCached, anyRole: true, kind: kindUsage, name: "input_tokens_details", pol: polMap},
		{sh: shCCUsageWrite, anyRole: true, kind: kindUsage, name: "input_tokens_details", pol: polMap},
		{sh: shCCUsageOut, anyRole: true, kind: kindUsage, name: "output_tokens", pol: polMap},
		{sh: shCCReasoningTokens, anyRole: true, kind: kindUsage, name: "output_tokens_details", pol: polMap},
	}
}
