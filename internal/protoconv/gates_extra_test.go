// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

// 本文件补 spec §8 里两条"现有测试只做了一半"的门禁：
//
//	(d2) segWidth 与最宽段的段内宽度**精确相等**。现有 TestShapeSegmentsDisjoint
//	     只证明各方向的 shape 段互不相交 —— 把 segWidth 从 23 改成 30，段照样
//	     互不重叠，那条测试全绿，但 lookupTable 的 [dirCount][segWidth] 多出 7 列
//	     永远写不到的空槽位，lookup 的 `off >= segWidth` 越界边界也从 23 漂到 30。
//	     故这里补 require.Equal(maxSegWidth(), int(segWidth))（见 TestShapeSegWidthExact）。
//
//	(l) dirName 11 项两两唯一、逐项非空、且与 rowsOf 真正覆盖的方向集合一致。
//	     现有测试只在遍历 membersOf 时把 dirName[d] 当失败信息读出来，从不校验
//	     dirName 本身：复制粘贴一行（两个方向同名）不会红，只会让 TSV 看起来
//	     "少了一行"。
//
// 期望值一律从生产成员表现算（segWidth 是 const，不能函数初始化，故写成运行期断言）。

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// allDirections 0..dirCount-1（顺序即 direction 的 iota 定义）。
func allDirections() []direction {
	out := make([]direction, 0, dirCount)
	for d := direction(0); d < dirCount; d++ {
		out = append(out, d)
	}
	return out
}

// segSpan 方向 d 的**段内宽度需求**：lookup 用 off = sh - segBase[d] 作下标
// （contentmodel.go:778），故该段至少要 max(shape)-segBase[d]+1 个槽位。
// 由 membersOf 现算 —— **不读 segWidth**，否则断言自证（拿 segWidth 算出来的
// 值去证明 segWidth 正确）。
func segSpan(d direction) int {
	ms := membersOf(d)
	if len(ms) == 0 {
		return 0
	}
	hi := ms[0].sh
	for _, m := range ms[1:] {
		if m.sh > hi {
			hi = m.sh
		}
	}
	return int(hi) - int(segBase[d]) + 1
}

// maxSegWidth 全部方向段内宽度需求的最大值 —— 这就是 segWidth 必须取的**唯一**值。
// 当前边界段是 T3 mess→resp（23 个成员，shape 501..523 ⇒ 需求 23），即 spec §8(d2)
// 记的 "T3 = 23"。
//
// **改什么会红**：把 segWidth 改成任何不等于 maxSegWidth() 的值（改大 → 空列，
// 改小 → init panic），或给最宽段增删成员而 segWidth 不跟着动。负例已用 -overlay
// 在树外副本实测。
func maxSegWidth() int {
	max := 0
	for d := direction(0); d < dirCount; d++ {
		if sp := segSpan(d); sp > max {
			max = sp
		}
	}
	return max
}

// TestShapeSegWidthExact (d2) segWidth == maxSegWidth()，且每个方向的成员/行都落在
// 自己那一段的 [segBase[d], segBase[d]+segWidth) 内（lookup 越界判定与回读 rw.sh
// 的前提），shapeCount 严格大于最大 shape（shape 直接作下标，见 shapeName）。
func TestShapeSegWidthExact(t *testing.T) {
	// ① 精确相等（spec §8(d2) 原文）。
	require.Equal(t, maxSegWidth(), int(segWidth),
		"segWidth 必须恰好等于最宽 shape 段的段内宽度（当前 23 = T3 mess→resp）；"+
			"改大 → lookupTable 多出永不使用的列，改小 → init panic")

	// ② 边界说明：必须**存在**一个跑满 segWidth 的方向（"最宽段"），否则 ① 的相等
	//    就成了"所有方向都很窄、segWidth 恰好等于某个小值"的巧合；把最宽段的成员
	//    删掉一个而 segWidth 不降，这里也会红。
	var saturated []direction
	for _, d := range allDirections() {
		if segSpan(d) == int(segWidth) {
			saturated = append(saturated, d)
		}
	}
	require.NotEmpty(t, saturated,
		"没有任何方向的段内宽度需求等于 segWidth=%d：最宽段被改动而 segWidth 没跟着降（maxSegWidth=%d）",
		int(segWidth), maxSegWidth())

	// ③ 段内不变式：membersOf 与 rowsOf 的每个 shape 都落在**自己**那一段里。
	//    越界（sh < segBase[d] 或 off >= segWidth）会让 lookup 静默返回 ok=false
	//    （丢行），故必须独立钉住，不能只靠 init 的 panic。
	maxShape := shape(0)
	for _, d := range allDirections() {
		for _, m := range membersOf(d) {
			require.GreaterOrEqual(t, int(m.sh), int(segBase[d]),
				"方向 %s 的成员 %s（shape=%d）低于本段 base=%d：段内偏移为负，lookup 恒返回 ok=false",
				dirName[d], m.name, m.sh, segBase[d])
			require.Less(t, int(m.sh), int(segBase[d])+int(segWidth),
				"方向 %s 的成员 %s（shape=%d）超出本段 base+segWidth=%d：lookup 越界",
				dirName[d], m.name, m.sh, int(segBase[d])+int(segWidth))
			if m.sh > maxShape {
				maxShape = m.sh
			}
		}
		for _, rw := range rowsOf(d) {
			require.GreaterOrEqual(t, int(rw.sh), int(segBase[d]),
				"方向 %s 的行（shape=%d）低于本段 base=%d", dirName[d], rw.sh, segBase[d])
			require.Less(t, int(rw.sh), int(segBase[d])+int(segWidth),
				"方向 %s 的行（shape=%d）超出本段 base+segWidth=%d", dirName[d], rw.sh, int(segBase[d])+int(segWidth))
			if rw.sh > maxShape {
				maxShape = rw.sh
			}
		}
	}
	require.Greater(t, int(shapeCount), int(maxShape),
		"shapeCount（%d）必须严格大于最大 shape（%d）：shape 直接作数组下标（shapeName）", shapeCount, maxShape)

	// ④ segBase 两两不同：两段共用 base 会让跨段 shape 的"回读 rw.sh != sh"
	//    变成必然发生而不是异常（且 members.tsv 的 dir 列会出现两个方向抢同一批 shape）。
	seen := make(map[shape]direction, dirCount)
	for _, d := range allDirections() {
		prev, dup := seen[segBase[d]]
		require.False(t, dup, "方向 %s 与 %s 的 segBase 相同（%d）",
			dirName[d], dirName[prev], segBase[d])
		seen[segBase[d]] = d
	}
}

// TestDirNameGate (l) dirName 是 members.tsv / anchors.tsv / 契约表的 dir 列共用词表
// （contentmodel_test.go:117-118/138 直接拿它拼 members.tsv 的 dir 与 AnchorRef 列；
// contract_client_to_chat_test.go:20 把 dir 列定义为 "= dirName"）。
// 它必须：长度 == dirCount、逐项非空、11 项**两两唯一**、且与 rowsOf 真正覆盖的方向
// 集合一致 —— 只在遍历成员表时顺便读 dirName 是不够的（那样重名/空名都不会红）。
//
// **命名陷阱（spec §2.2 第 2 条；生产代码同款说明见 dropsink.go 的“两面原因
// 不相交”约定）**：
// `dirName[dirRespToChat] == "resp_to_chat"` 是 **content-model 响应面**（T5，x_* 段：
// 上游 chat 响应 → 客户端 resp 响应）；而 **domain 值**
// `domain.ProtocolConvertRespToChat` 也写作 `"resp_to_chat"`，它命名的是
// "客户端 resp → 上游 chat"这**整个方向**（其请求面是 dirRespToChatReq /
// `"resp_to_chat_req"`，T10，rc_* 段）。同一个字符串在两个命名空间里指两件不同的事：
// 日志与丢弃计数必须带 `domain:` / `content-model:` 前缀 —— 丢弃计数的键是
// `<domain 值>|<面>|<字段>`（面由 dropsink.go 的 DropEvent 显式携带，不是靠字符串
// 避让），所以两者在观测面上不会混。Go 编译器抓不到这种词序重排，故本测试把这两个
// 字符串**钉死**：改名前必须先读这段注释。
func TestDirNameGate(t *testing.T) {
	// ① 长度 == dirCount（spec §8(l) 原文）。dirName 是 [dirCount]string，故这条
	//    由编译器保证；真正抓漏项的是 ② 的"逐项非空"（漏写的键留下零值 ""）。
	require.Len(t, dirName, int(dirCount), "dirName 必须覆盖全部内部方向")
	require.Equal(t, 11, int(dirCount),
		"spec §2.2：内部 content-model 方向 11 个（7 既有 + 4 新增）；加方向要同时补 dirName/rowsOf/members.tsv")

	// ② 逐项非空 + 两两唯一（复制粘贴重名即红）。
	seen := make(map[string]direction, dirCount)
	for _, d := range allDirections() {
		name := dirName[d]
		require.NotEmpty(t, name,
			"方向 %d（segBase=%d）没有 dirName：members.tsv/anchors.tsv/契约表的 dir 列会退化成空串",
			d, segBase[d])
		prev, dup := seen[name]
		require.False(t, dup,
			"dirName %q 重复：方向 #%d（segBase=%d）与方向 #%d（segBase=%d）同名（spec §8(l)：复制粘贴重名即红；11 项必须两两唯一）",
			name, int(prev), segBase[prev], int(d), segBase[d])
		seen[name] = d
	}

	// ③ dirName 与 rowsOf 覆盖的方向集合一致 —— 不能只遍历成员表：rowsOf 少一个
	//    case 会返回 nil（该方向"有名字但没有行数据"），此时 assemble/lookup 全部
	//    走空表，而成员表照样是满的。
	var withRows []direction
	for _, d := range allDirections() {
		if len(rowsOf(d)) > 0 {
			withRows = append(withRows, d)
		}
	}
	require.Len(t, withRows, len(dirName),
		"dirName 有 %d 项，但只有 %d 个方向有行数据（rowsOf 返回 nil = 有名字没有行表）",
		len(dirName), len(withRows))
	require.ElementsMatch(t, allDirections(), withRows,
		"dirName 覆盖的方向集合与 rowsOf 不一致（members.tsv 的 dir 列会出现无行数据的方向）")

	// ④ 命名陷阱钉死：content-model 响应面的 "resp_to_chat" 与 domain 值
	//    "resp_to_chat" 同名不同义（见上方注释）；请求面必须带 _req 后缀。
	require.Equal(t, "resp_to_chat", dirName[dirRespToChat],
		"content-model 响应面（T5 x_*）的 dirName 必须保持 resp_to_chat（与 members.tsv 的 dir 列一致）")
	require.Equal(t, "resp_to_chat_req", dirName[dirRespToChatReq],
		"domain 值 resp_to_chat 的**请求面**是 content-model 的 resp_to_chat_req（T10 rc_*）")
	require.Equal(t, "resp_to_chat", string(domain.ProtocolConvertRespToChat),
		"domain 值与 dirName 的 resp_to_chat 同名不同义（spec §2.2 命名陷阱 2）：改任一侧都要先改注释与 TSV")
}
