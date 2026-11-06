// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

// 内容模型门禁（spec §3.6）：(a) 成员↔行↔anchor、(a2) 段不相交、(a3) 掩码完整性、
// (b) 结构等价、(b2) 语义一致、(d′) 输出词表、(e) 分配门。
//
// 这些断言的价值全部来自**期望值独立于实现手写**：(b) 的期望切片不是从
// contentmodel.go 复制的，成员清单的权威副本是 testdata/members.tsv，
// anchor 的权威副本是 testdata/spec/{anchors,excerpts}.tsv。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/stretchr/testify/require"
)

// --- TSV 读取 ---

// tsvRows 读取 TSV（首行不是表头：文件本身逐行即数据），返回非空行。
func tsvRows(t *testing.T, path string) [][]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var out [][]string
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		out = append(out, fields)
	}
	return out
}

func kindName(k kind) string {
	switch k {
	case kindPart:
		return "part"
	case kindItem:
		return "item"
	case kindField:
		return "field"
	case kindChatKey:
		return "key"
	}
	return "?"
}

func (r row) anyRoleName() string {
	if r.anyRole {
		return "true"
	}
	return "false"
}

// --- (a) 成员 ↔ 行 ↔ anchor（双向）---

// TestMembersTSVInSync (a)①：testdata/members.tsv ↔ rowsXXX 逐方向一一对应。
// 更新方式：TSV_UPDATE=1 go test ./internal/protoconv -run TestMembersTSVInSync
func TestMembersTSVInSync(t *testing.T) {
	const path = "testdata/members.tsv"
	if os.Getenv("TSV_UPDATE") == "1" {
		var b strings.Builder
		for d := direction(0); d < dirCount; d++ {
			for _, m := range membersOf(d) {
				rw, ok := lookup(d, m.sh, roleInvalid)
				require.True(t, ok || !rw.anyRole, "每个成员必须有行：%s", m.name)
			}
			for _, rw := range rowsOf(d) {
				b.WriteString(strings.Join([]string{
					dirName[d], shapeName[rw.sh], kindName(rw.kind), rw.anyRoleName(),
					dirName[d] + "/" + shapeName[rw.sh],
				}, "\t"))
				b.WriteString("\n")
			}
		}
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o644))
		return
	}

	rows := tsvRows(t, path)
	byDir := map[string][][]string{}
	for _, f := range rows {
		require.Len(t, f, 5, "members.tsv 每行五列：dir/member/kind/anyRole/AnchorRef")
		byDir[f[0]] = append(byDir[f[0]], f)
	}

	seenMember := map[string]bool{}
	seenAnchor := map[string]bool{}
	for d := direction(0); d < dirCount; d++ {
		name := dirName[d]
		want := byDir[name]
		require.NotEmpty(t, want, "方向 %s 在 members.tsv 里没有行", name)

		var got [][]string
		for _, rw := range rowsOf(d) {
			member := shapeName[rw.sh]
			require.NotEmpty(t, member, "行 %d 没有对应成员名", rw.sh)
			key := name + "/" + member
			require.False(t, seenMember[key], "成员重复：%s", key)
			seenMember[key] = true
			got = append(got, []string{name, member, kindName(rw.kind), rw.anyRoleName(), key})
		}
		require.ElementsMatch(t, want, got, "方向 %s 的 members.tsv 与 rows 必须一一对应", name)
	}

	// (a)③ members.tsv 的 AnchorRef ↔ anchors.tsv 的 (dir, member) 双向 ElementsMatch
	var fromMembers, fromAnchors []string
	for _, f := range rows {
		fromMembers = append(fromMembers, f[4])
	}
	for _, f := range tsvRows(t, "testdata/spec/anchors.tsv") {
		require.Len(t, f, 5, "anchors.tsv 每行五列：dir/member/spec_file/line/excerpt")
		key := f[0] + "/" + f[1]
		require.False(t, seenAnchor[key], "anchor 重复：%s", key)
		seenAnchor[key] = true
		fromAnchors = append(fromAnchors, key)
	}
	require.ElementsMatch(t, fromMembers, fromAnchors, "每个成员恰好一条 anchor（野 anchor / 缺 anchor 都要红）")

	// (a)④ anchors ⊆ excerpts（逐三元组）⑤ excerpts 每条至少被用一次 ⑥ 片段 ≤120B
	used := map[string]bool{}
	for _, f := range tsvRows(t, "testdata/spec/anchors.tsv") {
		require.Contains(t, []string{"resp", "mess", "chat"}, f[2], "spec_file 只能是三份规范之一")
		triple := f[2] + "\t" + f[3] + "\t" + f[4]
		used[triple] = true
		require.LessOrEqual(t, len(f[4]), 120, "anchor 片段必须 ≤120B：%s", f[1])
		require.NotEmpty(t, f[4])
	}
	unused := 0
	for _, f := range tsvRows(t, "testdata/spec/excerpts.tsv") {
		require.Len(t, f, 3, "excerpts.tsv 每行三列：spec_file/line/excerpt")
		require.LessOrEqual(t, len(f[2]), 120, "excerpt 片段必须 ≤120B")
		if !used[f[0]+"\t"+f[1]+"\t"+f[2]] {
			unused++
		}
	}
	require.Zero(t, unused, "excerpts.tsv 每条摘录至少要被一条 anchor 使用")
}

// --- (b) 结构等价（期望切片独立手写）---

func rowRole(sh shape, roles uint8, k kind, name string, pol policy) row {
	return row{sh: sh, roles: roles, kind: k, name: name, pol: pol}
}

func rowAnn(sh shape, roles uint8, k kind, name string, pol policy) row {
	return row{sh: sh, roles: roles, kind: k, name: name, pol: pol, ann: true}
}

func rowAny(sh shape, k kind, name string, pol policy) row {
	return row{sh: sh, anyRole: true, kind: k, name: name, pol: pol}
}

// TestRowsStructuralEquality (b)：[]row 与手写期望切片逐项相等（含 ann），
// 且每行必须满足 anyRole || roles != 0（哨兵行的 roles==0 合法）。
func TestRowsStructuralEquality(t *testing.T) {
	cases := []struct {
		dir  direction
		want []row
	}{
		{dirChatToResp, []row{
			rowRole(shCSysdevText, rmSystem|rmDeveloper, kindItem, "message", polMap),
			rowRole(shCUserText, rmUser, kindItem, "message", polMap),
			rowRole(shCUserImgURL, rmUser, kindItem, "message", polMap),
			rowRole(shCUserImgData, rmUser, kindItem, "message", polMap),
			rowRole(shCUserImgBare64, rmUser, kindItem, "message", polMap),
			rowRole(shCUserAudio, rmUser, kindItem, "", polDrop),
			rowRole(shCUserFileData, rmUser, kindItem, "message", polMap),
			rowRole(shCUserFileID, rmUser, kindItem, "", polDrop),
			rowAnn(shCAsstText, rmAsst, kindItem, "message", polMap),
			rowRole(shCAsstRefusal, rmAsst, kindItem, "message", polMap),
			rowRole(shCAsstOtherParts, rmAsst, kindItem, "", polDrop),
			rowRole(shCAsstToolFunction, rmAsst, kindItem, "function_call", polMap),
			rowRole(shCAsstToolCustom, rmAsst, kindItem, "", polDrop),
			rowRole(shCToolMsg, rmTool, kindItem, "function_call_output", polMap),
			rowRole(shCFunctionMsg, rmTool, kindItem, "function_call_output", polMap),
			rowAny(shCTail, kindItem, "", polDrop),
		}},
		{dirChatToMess, []row{
			rowRole(shCMSysdevText, rmSystem|rmDeveloper, kindItem, "", polOmit),
			rowRole(shCMUserText, rmUser, kindItem, "message", polMap),
			rowRole(shCMUserImgURL, rmUser, kindItem, "message", polMap),
			rowRole(shCMUserImgData, rmUser, kindItem, "message", polMap),
			rowRole(shCMUserImgBare64, rmUser, kindItem, "", polDrop),
			rowRole(shCMUserAudio, rmUser, kindItem, "", polDrop),
			rowRole(shCMUserFileData, rmUser, kindItem, "", polDrop),
			rowRole(shCMUserFileID, rmUser, kindItem, "", polDrop),
			rowRole(shCMAsstText, rmAsst, kindItem, "message", polMap),
			rowRole(shCMAsstRefusal, rmAsst, kindItem, "message", polTextDegrade),
			rowRole(shCMAsstOtherParts, rmAsst, kindItem, "", polDrop),
			rowRole(shCMAsstToolFunction, rmAsst, kindItem, "message", polMap),
			rowRole(shCMAsstToolCustom, rmAsst, kindItem, "", polDrop),
			rowRole(shCMToolMsg, rmTool, kindItem, "message", polMap),
			rowRole(shCMFunctionMsg, rmTool, kindItem, "message", polMap),
			rowAny(shCMTail, kindItem, "", polDrop),
		}},
		{dirMessToResp, []row{
			rowRole(shMUserText, rmUser, kindItem, "message", polMap),
			rowRole(shMAsstText, rmAsst, kindItem, "message", polMap),
			rowRole(shMUserImgBase64, rmUser, kindItem, "message", polMap),
			rowRole(shMUserImgURL, rmUser, kindItem, "message", polMap),
			rowRole(shMUserImgFileID, rmUser, kindItem, "", polDrop),
			rowRole(shMAsstImgBase64, rmAsst, kindItem, "", polDrop),
			rowRole(shMAsstImgURL, rmAsst, kindItem, "", polDrop),
			rowRole(shMAsstImgFileID, rmAsst, kindItem, "", polDrop),
			rowRole(shMUserDocB64TextContent, rmUser, kindItem, "", polDrop),
			rowRole(shMUserDocFileID, rmUser, kindItem, "", polDrop),
			rowRole(shMUserDocURL, rmUser, kindItem, "message", polMap),
			rowRole(shMUserSearchResult, rmUser, kindItem, "message", polDrop),
			rowRole(shMAsstSearchResult, rmAsst, kindItem, "", polDrop),
			rowRole(shMAsstToolUse, rmAsst, kindItem, "function_call", polMap),
			rowRole(shMToolResultString, rmUser, kindItem, "function_call_output", polMap),
			rowRole(shMToolResultArrayText, rmUser, kindItem, "function_call_output", polMap),
			rowRole(shMToolResultArrayNoText, rmUser, kindItem, "function_call_output", polMap),
			rowRole(shMToolResultNoContent, rmUser, kindItem, "function_call_output", polMap),
			rowRole(shMToolResultNestedMedia, rmUser, kindItem, "function_call_output", polMap),
			rowRole(shMThinking, rmAsst, kindItem, "reasoning", polMap),
			rowRole(shMRedactedThinking, rmAsst, kindItem, "reasoning", polMap),
			rowAny(shMTail, kindItem, "", polDrop),
			rowAny(shMTopSystem, kindField, "instructions", polMap),
		}},
		{dirRespToMess, []row{
			rowRole(shRMsgUserInputText, rmUser, kindItem, "message", polMap),
			rowRole(shRMsgUserOutputText, rmUser, kindItem, "message", polMap),
			rowRole(shRMsgAsstInputText, rmAsst, kindItem, "message", polMap),
			rowRole(shRMsgUserImgURL, rmUser, kindItem, "message", polMap),
			rowRole(shRMsgUserImgDataURL, rmUser, kindItem, "message", polMap),
			rowRole(shRMsgUserImgOther, rmUser, kindItem, "", polDrop),
			rowRole(shRMsgUserImgFileID, rmUser, kindItem, "", polDrop),
			rowRole(shRMsgUserFileURL, rmUser, kindItem, "message", polMap),
			rowRole(shRMsgUserFileID, rmUser, kindItem, "", polDrop),
			rowRole(shRMsgUserFileData, rmUser, kindItem, "", polDrop),
			rowRole(shRMsgUserInputAudio, rmUser, kindItem, "", polDrop),
			rowRole(shRMsgSysdevText, rmSystem|rmDeveloper, kindItem, "", polOmit),
			rowRole(shRMsgAsstOutputText, rmAsst, kindItem, "message", polMap),
			rowRole(shRMsgAsstRefusal, rmAsst, kindItem, "message", polTextDegrade),
			rowRole(shRMsgAsstMedia, rmAsst, kindItem, "", polDrop),
			rowRole(shRFunctionCall, rmAsst, kindItem, "function_call", polMap),
			rowRole(shRReasoning, rmAsst, kindItem, "reasoning", polMap),
			rowAny(shRItemReference, kindItem, "", polDrop),
			rowAny(shRTail, kindItem, "", polDrop),
			rowRole(shRFcoString, rmUser, kindPart, "tool_result", polMap),
			rowRole(shRFcoArrayText, rmUser, kindPart, "tool_result", polMap),
			rowRole(shRFcoArrayNoText, rmUser, kindPart, "tool_result", polMap),
		}},
		{dirRespToChat, []row{
			rowRole(shXMessageOutputText, rmAsst, kindChatKey, "content", polMap),
			rowRole(shXMessageRefusal, rmAsst, kindChatKey, "content", polMap),
			rowRole(shXFunctionCall, rmAsst, kindChatKey, "tool_calls", polMap),
			rowRole(shXReasoning, rmAsst, kindChatKey, "reasoning_content", polMap),
			rowRole(shXMessageMedia, rmAsst, kindChatKey, "", polDrop),
			rowAny(shXTail, kindChatKey, "", polDrop),
		}},
		{dirRespToMessResp, []row{
			rowRole(shYOutputText, rmAsst, kindPart, "text", polMap),
			rowRole(shYRefusal, rmAsst, kindPart, "text", polTextDegrade),
			rowRole(shYFunctionCall, rmAsst, kindPart, "tool_use", polMap),
			rowRole(shYReasoning, rmAsst, kindPart, "thinking", polMap),
			rowAny(shYTail, kindPart, "", polDrop),
		}},
		{dirMessToChatResp, []row{
			rowRole(shZText, rmAsst, kindChatKey, "content", polMap),
			rowRole(shZThinking, rmAsst, kindChatKey, "reasoning_content", polMap),
			rowRole(shZToolUse, rmAsst, kindChatKey, "tool_calls", polMap),
			rowRole(shZMedia, rmAsst, kindChatKey, "", polDrop),
			rowAny(shZTail, kindChatKey, "", polDrop),
		}},
	}
	covered := map[direction]bool{}
	for _, c := range cases {
		covered[c.dir] = true
		got := rowsOf(c.dir)
		require.Equal(t, c.want, got, "方向 %s 的行数据与手写期望不一致", dirName[c.dir])
		for _, rw := range got {
			require.True(t, rw.anyRole || rw.roles != 0, "行 %s 既不是哨兵也没写 role 掩码", shapeName[rw.sh])
			require.NotZero(t, rw.pol, "行 %s 的策略必须是显式值（零值是 polUnknown）", shapeName[rw.sh])
		}
	}
	require.Len(t, covered, int(dirCount), "七个方向必须都有期望切片")
}

// --- (a2) 段不相交 ---

func TestShapeSegmentsDisjoint(t *testing.T) {
	segs := make([][]shape, 0, dirCount)
	for d := direction(0); d < dirCount; d++ {
		var seg []shape
		for _, m := range membersOf(d) {
			seg = append(seg, m.sh)
		}
		require.NotEmpty(t, seg, "方向 %s 没有成员", dirName[d])
		segs = append(segs, seg)
	}
	for i := 0; i < len(segs); i++ {
		for j := i + 1; j < len(segs); j++ {
			in := map[shape]bool{}
			for _, sh := range segs[i] {
				in[sh] = true
			}
			for _, sh := range segs[j] {
				require.False(t, in[sh], "方向 %s 与 %s 的 shape 段相交（shape=%d）", dirName[direction(i)], dirName[direction(j)], sh)
			}
		}
	}
	// 成员名全局唯一（同一 shape 不得两义）
	names := map[string]shape{}
	for d := direction(0); d < dirCount; d++ {
		for _, m := range membersOf(d) {
			if prev, ok := names[m.name]; ok {
				require.Equal(t, prev, m.sh, "成员名 %s 绑定了两个 shape", m.name)
			}
			names[m.name] = m.sh
		}
	}
}

// --- (a3) 掩码完整性 ---

func TestRoleMaskCoverage(t *testing.T) {
	allRoles := []role{roleSystem, roleDeveloper, roleUser, roleAssistant, roleTool}
	for d := direction(0); d < dirCount; d++ {
		expected := dirRoles[d]
		require.NotZero(t, expected, "方向 %s 必须声明期望 role 集", dirName[d])

		var union uint8
		for _, rw := range rowsOf(d) {
			if rw.anyRole {
				continue // 哨兵行不参与 role 覆盖判定
			}
			// ② 非哨兵行 roles ⊆ expectedRoles(dir)
			require.Zero(t, rw.roles&^expected, "方向 %s 的行 %s 掩码含期望集外的 role", dirName[d], shapeName[rw.sh])
			union |= rw.roles
		}
		// ① 排除哨兵行后 union(roles) ⊇ expectedRoles(dir)
		require.Zero(t, expected&^union, "方向 %s 的期望 role 未被任何行覆盖", dirName[d])

		// ③ 掩码外 polUnknown；哨兵行永不踩空
		for _, rw := range rowsOf(d) {
			for _, r := range allRoles {
				got, ok := lookup(d, rw.sh, r)
				if rw.anyRole || rw.roles&roleBit(r) != 0 {
					require.True(t, ok, "方向 %s 行 %s 对 role %s 应命中", dirName[d], shapeName[rw.sh], roleName(r))
					require.Equal(t, rw.pol, got.pol, "命中的行策略必须与表一致")
					continue
				}
				require.False(t, ok, "方向 %s 行 %s 对 role %s 不应命中", dirName[d], shapeName[rw.sh], roleName(r))
				require.Equal(t, polUnknown, got.pol, "掩码外必须是 polUnknown（不得是 polDrop）")
			}
		}
	}
	// 未知 role 与掩码外同口径
	got, ok := lookup(dirChatToResp, shCUserText, roleInvalid)
	require.False(t, ok)
	require.Equal(t, polUnknown, got.pol)
}

// --- (b2) 语义一致（按 kind 选词表）---

func TestRowNamesInVocab(t *testing.T) {
	requestDirs := map[direction]bool{
		dirChatToResp: true, dirChatToMess: true, dirMessToResp: true, dirRespToMess: true,
	}
	for d := direction(0); d < dirCount; d++ {
		tgt := dirTarget(d)
		for _, rw := range rowsOf(d) {
			if rw.name == "" {
				continue // ∅ 行不参与名词判定
			}
			switch rw.kind {
			case kindPart:
				require.False(t, rw.anyRole, "部件行的 name 必须按 role 判定，不能是哨兵行：%s", shapeName[rw.sh])
				for _, r := range []role{roleSystem, roleDeveloper, roleUser, roleAssistant, roleTool} {
					if rw.roles&roleBit(r) == 0 {
						continue
					}
					require.True(t, inVocab(partVocab(tgt, r), rw.name),
						"方向 %s 行 %s 的 name=%q 不在 partVocab(%s) 内", dirName[d], shapeName[rw.sh], rw.name, roleName(r))
				}
			case kindItem:
				vocab := itemVocabOut
				if requestDirs[d] {
					vocab = itemVocabIn
				}
				require.True(t, inVocab(vocab, rw.name), "方向 %s 行 %s 的 name=%q 不在项类型词表内", dirName[d], shapeName[rw.sh], rw.name)
			case kindChatKey:
				require.True(t, inVocab(chatKeyVocab, rw.name), "方向 %s 行 %s 的 name=%q 不在 chat 键词表内", dirName[d], shapeName[rw.sh], rw.name)
			case kindField:
				require.True(t, inVocab(fieldVocab, rw.name), "方向 %s 行 %s 的 name=%q 不在字段词表内", dirName[d], shapeName[rw.sh], rw.name)
			default:
				require.Fail(t, "行的 kind 未定义", shapeName[rw.sh])
			}
		}
	}
}

// TestVocabSelfComplete §3.5 自完整性：词表逐元素硬编码比对（新增名词必须同时
// 改词表与期望）。
func TestVocabSelfComplete(t *testing.T) {
	require.ElementsMatch(t, []string{"input_text", "input_image", "input_file"}, partVocab(tgtResp, roleUser))
	require.ElementsMatch(t, []string{"input_text"}, partVocab(tgtResp, roleSystem))
	require.ElementsMatch(t, []string{"input_text"}, partVocab(tgtResp, roleDeveloper))
	require.ElementsMatch(t, []string{"output_text", "refusal"}, partVocab(tgtResp, roleAssistant))
	require.ElementsMatch(t, []string{"text", "image", "tool_result", "document"}, partVocab(tgtMess, roleUser))
	require.ElementsMatch(t, []string{"text", "thinking", "redacted_thinking", "tool_use"}, partVocab(tgtMess, roleAssistant))
	require.Nil(t, partVocab(tgtChat, roleAssistant), "chat 目标不适用 partVocab（content 是字符串）")

	require.ElementsMatch(t, []string{"message", "function_call", "function_call_output", "reasoning"}, itemVocabIn)
	require.ElementsMatch(t, []string{"message", "function_call", "reasoning"}, itemVocabOut)
	require.ElementsMatch(t, []string{"content", "tool_calls", "reasoning_content"}, chatKeyVocab)
	require.ElementsMatch(t, []string{"id", "object", "created", "model", "choices", "index", "delta", "role", "finish_reason", "usage"}, chatFrameKeys)
	require.ElementsMatch(t, []string{"instructions", "system"}, fieldVocab)
	require.ElementsMatch(t, []string{
		"response.created", "response.output_item.added", "response.output_item.done",
		"response.output_text.delta", "response.reasoning_summary_text.delta",
		"response.function_call_arguments.delta", "response.completed", "error",
	}, respEventVocab)
	require.ElementsMatch(t, []string{
		"message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop", "error",
	}, messEventVocab)
	require.NotContains(t, itemVocabOut, "function_call_output", "function_call_output 只属请求向")
}

// --- A4：停因表与既有换算函数一致 ---

func TestStopMapMatchesLiveMapping(t *testing.T) {
	for _, r := range stopMapIncomplete {
		require.Equal(t, r.chat, chatFinishFromIncomplete(r.incomplete), "chat finish_reason 与停因表不一致：%s", r.incomplete)
		require.Equal(t, r.mess, messStopFromIncomplete(r.incomplete), "mess stop_reason 与停因表不一致：%s", r.incomplete)
	}
	require.Equal(t, stopDefaultIncomplete.chat, chatFinishFromIncomplete("max_output_tokens"))
	require.Equal(t, stopDefaultIncomplete.mess, messStopFromIncomplete("max_output_tokens"))
	for _, r := range messStopToChat {
		require.Equal(t, r.chat, messStopToChatFinish(r.mess), "mess→chat 停因表不一致：%s", r.mess)
	}
	require.Equal(t, messStopToChatDefault, messStopToChatFinish("end_turn"))
}

// --- (e) 分配门 ---

// TestHotPathAllocs (e)：主路径分配数不得超过基线（chat→resp 6 allocs/op）。
// 输入形状与 BenchmarkConvertRequestChatToResp 相同（无图片）。
func TestHotPathAllocs(t *testing.T) {
	body := []byte(benchChatReqBody)
	allocs := testing.AllocsPerRun(200, func() {
		out, err := ConvertRequest(body, domain.ProtocolConvertChatToResp)
		if err != nil {
			t.Fatal(err)
		}
		benchSink = out
	})
	require.LessOrEqual(t, allocs, float64(6), "热路径分配回归：%.1f allocs/op > 6", allocs)
}

// --- (d′) 输出词表（两套，带 role 维度）---

// roleFromName 解码后的 role 字符串 → role。
func roleFromName(s string) role {
	switch s {
	case "system":
		return roleSystem
	case "developer":
		return roleDeveloper
	case "user":
		return roleUser
	case "assistant":
		return roleAssistant
	case "tool":
		return roleTool
	}
	return roleInvalid
}

// checkPartList 校验 content/内容块数组的部件类型过词表。
func checkPartList(t *testing.T, tgt target, r role, v any) {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		return
	}
	vocab := partVocab(tgt, r)
	for _, p := range list {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := pm["type"].(string)
		require.True(t, inVocab(vocab, typ), "部件类型 %q 不在 partVocab(%s, %s) 内", typ, targetName(tgt), roleName(r))
	}
}

func targetName(tg target) string {
	switch tg {
	case tgtResp:
		return "resp"
	case tgtMess:
		return "mess"
	}
	return "chat"
}

// checkItems 校验项数组的项类型与 message 项内容部件。
func checkItems(t *testing.T, tgt target, items []any, vocab []string) {
	t.Helper()
	for _, it := range items {
		item, ok := it.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := item["type"].(string)
		require.True(t, inVocab(vocab, typ), "项类型 %q 不在词表内", typ)
		r := roleFromName(stringOf(item["role"]))
		if r == roleInvalid {
			// 无 role 的项（function_call_output）不做 role 维度的部件判定
			continue
		}
		checkPartList(t, tgt, r, item["content"])
	}
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// checkMessages 校验 mess messages[] 的块类型与 chat message 的键。
func checkMessages(t *testing.T, msgs []any) {
	t.Helper()
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		checkPartList(t, tgtMess, roleFromName(stringOf(msg["role"])), msg["content"])
	}
}

// checkChatMessage chat 目标的 assistant message：content 必须是字符串或 null
// （chat 目标不适用 partVocab），其余键必须来自 chat 键词表。
func checkChatMessage(t *testing.T, msg map[string]any) {
	t.Helper()
	for k := range msg {
		if k == "role" {
			continue
		}
		require.True(t, inVocab(chatKeyVocab, k), "chat message 出现词表外的键 %q", k)
	}
	if v, ok := msg["content"]; ok && v != nil {
		_, isStr := v.(string)
		require.True(t, isStr, "chat 目标的 content 必须是字符串（不适用 partVocab）")
	}
}

// checkFrame 校验一帧 SSE：顶层 type ∈ 事件名域、项类型 ∈ 项词表、帧内部件按
// role 过 partVocab；chat 帧校验顶层键与 delta 键。
func checkFrame(t *testing.T, tgt target, payload []byte) {
	t.Helper()
	if !json.Valid(payload) {
		return // [DONE] 等非 JSON 载荷
	}
	var m map[string]any
	require.NoError(t, json.Unmarshal(payload, &m))
	if tgt == tgtChat {
		for k := range m {
			require.True(t, inVocab(chatFrameKeys, k), "chat 帧顶层出现词表外的键 %q", k)
		}
		if choices, ok := m["choices"].([]any); ok {
			for _, c := range choices {
				cm, ok := c.(map[string]any)
				if !ok {
					continue
				}
				if delta, ok := cm["delta"].(map[string]any); ok {
					for k := range delta {
						// delta.role 是 chat 流式的角色前导（属帧级 role 声明，
						// 见 chatFrameKeys），不是消息体内容键。
						if k == "role" {
							continue
						}
						require.True(t, inVocab(chatKeyVocab, k), "chat delta 出现词表外的键 %q", k)
					}
				}
			}
		}
		return
	}
	vocab := eventVocabOf(tgt)
	if typ, ok := m["type"].(string); ok {
		require.True(t, inVocab(vocab, typ), "事件类型 %q 不在 %s 事件域内", typ, targetName(tgt))
	}
	if tgt != tgtResp {
		return
	}
	if item, ok := m["item"].(map[string]any); ok {
		r := roleFromName(stringOf(item["role"]))
		if r == roleInvalid {
			r = roleAssistant // item 缺 role → 回落 assistant
		}
		require.True(t, inVocab(itemVocabOut, stringOf(item["type"])), "帧内 item 类型 %q 不在项词表内", stringOf(item["type"]))
		checkPartList(t, tgtResp, r, item["content"])
	}
	if resp, ok := m["response"].(map[string]any); ok {
		if output, ok := resp["output"].([]any); ok {
			checkItems(t, tgtResp, output, itemVocabOut)
		}
	}
}

// checkGoldenOutput (d′) 主体：按用例的目标协议选校验路径（每个用例自带
// vocab 方向与是否流式，不做形态嗅探）。
func checkGoldenOutput(t *testing.T, c goldenCase, out string) {
	t.Helper()
	tgt := dirTarget(c.vocab)
	if c.stream {
		for _, frame := range strings.Split(out, "\n---\n") {
			for _, line := range strings.Split(frame, "\n") {
				if p, ok := strings.CutPrefix(line, "data: "); ok {
					checkFrame(t, tgt, []byte(p))
				}
			}
		}
		return
	}
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m))
	switch tgt {
	case tgtResp:
		if items, ok := m["input"].([]any); ok { // resp 请求
			checkItems(t, tgtResp, items, itemVocabIn)
			return
		}
		items, ok := m["output"].([]any) // resp 响应
		require.True(t, ok, "resp 目标必须带 input 或 output")
		checkItems(t, tgtResp, items, itemVocabOut)
	case tgtMess:
		if msgs, ok := m["messages"].([]any); ok { // mess 请求
			checkMessages(t, msgs)
			return
		}
		checkPartList(t, tgtMess, roleAssistant, m["content"]) // mess 响应
	default: // chat
		if choices, ok := m["choices"].([]any); ok { // chat 响应
			for _, c := range choices {
				cm, ok := c.(map[string]any)
				if !ok {
					continue
				}
				if msg, ok := cm["message"].(map[string]any); ok {
					checkChatMessage(t, msg)
				}
			}
			return
		}
		// chat 请求：assistant message 的 content 必须是字符串
		if msgs, ok := m["messages"].([]any); ok {
			for _, mv := range msgs {
				if mm, ok := mv.(map[string]any); ok && stringOf(mm["role"]) == "assistant" {
					checkChatMessage(t, mm)
				}
			}
		}
	}
}

// TestOutputStreamVocab (d′)：全部黄金用例的输出过词表（含 SSE 帧）。
// RT1（assistant 项只收 output_text/refusal）在这里才有唯一门禁。
func TestOutputStreamVocab(t *testing.T) {
	for _, c := range goldenCases() {
		t.Run(c.name, func(t *testing.T) {
			out := c.run(t)
			checkGoldenOutput(t, c, out)
		})
	}
}

// --- M1 三处缺陷修正的判别力（缺一即 FAIL）---

// TestM1AssistantTextIsOutputText §4 修正 1：chat assistant 历史文本 →
// resp output_text + annotations:[]（字节级，键序不得变）。
func TestM1AssistantTextIsOutputText(t *testing.T) {
	out, err := ConvertRequest([]byte(`{"model":"m","messages":[{"role":"assistant","content":"hello"}]}`), domain.ProtocolConvertChatToResp)
	require.NoError(t, err)
	require.Contains(t, string(out),
		`{"content":[{"annotations":[],"text":"hello","type":"output_text"}],"role":"assistant","type":"message"}`,
		"assistant 文本必须是 output_text 且带 annotations:[]（§1.1 的 400 根因）")
	require.NotContains(t, string(out), `"input_text"`, "assistant 历史不得写 input_text")
}

// TestM1ChatToRespAssistantMediaDropped §4 修正 2：assistant 的 image/audio/file
// 部件不得进 upstream，其余部件保留。
func TestM1ChatToRespAssistantMediaDropped(t *testing.T) {
	out, err := ConvertRequest([]byte(`{"model":"m","messages":[{"role":"assistant","content":[
		{"type":"text","text":"kept"},
		{"type":"image_url","image_url":"https://e.com/i.png"},
		{"type":"input_audio","input_audio":{"data":"AA","format":"wav"}},
		{"type":"file","file":{"file_data":"QUJD"}}
	]}]}`), domain.ProtocolConvertChatToResp)
	require.NoError(t, err)
	s := string(out)
	require.NotContains(t, s, "input_image", "assistant 图片不得进 resp input")
	require.NotContains(t, s, "input_audio")
	require.NotContains(t, s, "input_file")
	require.Contains(t, s, `{"annotations":[],"text":"kept","type":"output_text"}`, "文本部件必须保留")
}

// TestM1ChatToMessAssistantMediaDropped §4 修正 3：assistant 消息里不得出现
// image 块；image-only → 整条消息消失。
func TestM1ChatToMessAssistantMediaDropped(t *testing.T) {
	out, err := ConvertRequest([]byte(`{"model":"m","messages":[
		{"role":"assistant","content":[{"type":"image_url","image_url":"https://e.com/i.png"}]},
		{"role":"assistant","content":[{"type":"text","text":"kept"},{"type":"image_url","image_url":"data:image/png;base64,QUJD"}]}
	]}`), domain.ProtocolConvertChatToMess)
	require.NoError(t, err)
	s := string(out)
	require.NotContains(t, s, `"type":"image"`, "assistant 消息不得出现 image 块（J2）")
	require.Contains(t, s, `{"content":[{"text":"kept","type":"text"}],"role":"assistant"}`, "文本块保留")
	require.NotContains(t, s, `"source"`, "image-only 的 assistant 消息必须整条消失")
}
