// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/testsupport/racetest"
)

// 本文件是 payloadDigest（P3a）的**字节**与**成本**判据：
//
//   - KAT（6 组）：把前像与密文一起钉住。等价类测试（payload_key_test.go 的 H3/H4、
//     static_key_test.go 的轮转敏感度）管不到算法本身——换哈希、改前缀宽度、交换
//     token/refresh 段、把 expires 挪出前像，都能保持"哪些输入同键"的关系不变，却
//     让全部在途计划与已编译叶子静默换代（跨版本不可复现）。期望值由独立实现
//     testdata/payload_digest_vectors.py 算出。
//   - 等价类：摘要相等 ⟺ 三元组逐项相等（两两比较，双向）。
//   - 成本（H8）：真实形状（前像 2336B）零堆分配；跨过 4096 的形状恰一次 make。
//   - H7（构造期 / 比较期成本）：在**真实 codex 形状**上钉 allocs/op、B/op。
//
// 字节由本文件的 KAT 钉住，形状由 payload_digest_gate_test.go 的 H1/H2 钉住，
// 行为由 payload_key_test.go 的 H3/H4 钉住——四份判据互不重叠。

// payloadDigestPreimage 是 KAT 专用的前像入口：按生产编码器算出的**精确长度**分配
// 一块缓冲，再交给同一条 payloadDigestEncodeInto。
//
// 它只负责"给编码器一块可读的缓冲"，**不重写编码**：段序/前缀宽度/哈希的任何改动
// 都会让下面钉住的前像与密文同时变红。
func payloadDigestPreimage(token, refresh string, expiresUnixNano int64) []byte {
	buf := make([]byte, payloadDigestPreimageLen(token, refresh))
	payloadDigestEncodeInto(buf, token, refresh, expiresUnixNano)
	return buf
}

// payloadDigestVector 是一条字节级 KAT 向量。
type payloadDigestVector struct {
	name    string
	token   string
	refresh string
	expires int64
	// wantPreimageHex 由**独立实现**（testdata/payload_digest_vectors.py，Python）
	// 算出：三段 TLV，每段 8 字节大端长度前缀 + 内容；seg64 的内容是 8 字节二补数。
	// 段序被交换时这里逐行肉眼可见（密文断言抓不到段序——交换两段只改变密文，而
	// 密文是"钉住的值"，只要同步改就能过；前像钉的是**结构**）。
	wantPreimageHex string
	// wantDigestHex 是同一份脚本算出的 sha256。
	wantDigestHex string
}

// TestPayloadDigestKnownAnswer 是 payloadDigestOf 的**字节级** KAT。
//
// 六组向量，第 6 组是"长度前缀对"（两个成员）：
//
//	① 典型 ("at","rt",2030-01-01Z)   ② 只变 token   ③ 只变 refresh
//	④ **只变 expires**               ⑤ 空三元组     ⑥ ("ab","c",1) vs ("a","bc",1)
//
// ④ 是"摘要漏掉 expires"的**唯一机械抓点**：改前 expires 只出现在声明表里，全仓零条
// 敏感度断言——把它从编码里删掉时，其余五组全绿（前两组只动 token/refresh），只有 ④
// 会红。⑥ 是"用裸拼接代替长度前缀"的抓点：两组的裸拼接同为 "abc1"，前像必须不同。
//
// 每条向量三条断言，缺一条就漏一种变异：
//  1. 钉住的前像 hex == payloadDigestPreimage(...)（抓段序/前缀/段内容）；
//  2. sha256(钉住的前像) == payloadDigestOf(...)（把前像与编码器连起来，防"helper
//     与生产各走一套"）；
//  3. payloadDigestOf(...) 的密文 == 钉住的密文（抓哈希算法本身）。
func TestPayloadDigestKnownAnswer(t *testing.T) {
	const changed = "payloadDigest 编码变了：这不仅是等价类问题（前像/摘要都被钉住，交换段序、改前缀宽度、换哈希、把 expires 挪出前像都会红）"

	vectors := []payloadDigestVector{
		{
			name:    "typical",
			token:   "at",
			refresh: "rt",
			expires: payloadFixtureExpires,
			// 前像由 testdata/payload_digest_vectors.py 独立算出；一段一行 = 8B 大端长度前缀 + 内容。
			wantPreimageHex: "00000000000000026174" + // 段 0 token（len=2）
				"00000000000000027274" + // 段 1 refresh（len=2）
				"00000000000000081a46e83335d50000" + // 段 2 expires（seg64：len=8 + 1893456000000000000）
				"",
			wantDigestHex: "f5ecca2cf6262e8333748433c94eff4f631594960aadd7842f78ede12097c231",
		},
		{
			name:    "token_only",
			token:   "at2",
			refresh: "rt",
			expires: payloadFixtureExpires,
			wantPreimageHex: "0000000000000003617432" + // 段 0 token
				"00000000000000027274" + // 段 1 refresh
				"00000000000000081a46e83335d50000" + // 段 2 expires
				"",
			wantDigestHex: "c49ec2fe1ac2c8fcbf1a3b7989256b7b018f8fa9393058ee28f1b7a48296928d",
		},
		{
			name:    "refresh_only",
			token:   "at",
			refresh: "rt2",
			expires: payloadFixtureExpires,
			wantPreimageHex: "00000000000000026174" + // 段 0 token
				"0000000000000003727432" + // 段 1 refresh
				"00000000000000081a46e83335d50000" + // 段 2 expires
				"",
			wantDigestHex: "fa57da6477fba0083f703d89b97864c4614ced5b4529dafaae60cc192e15e0b5",
		},
		{
			// ④ 只变 expires：token/refresh 与 ① 完全相同，expires +1ns。
			name:    "expires_only",
			token:   "at",
			refresh: "rt",
			expires: payloadFixtureExpires + 1,
			wantPreimageHex: "00000000000000026174" + // 段 0 token
				"00000000000000027274" + // 段 1 refresh
				"00000000000000081a46e83335d50001" + // 段 2 expires（只差最低位 1ns）
				"",
			wantDigestHex: "079e4736eddfe57ddaf55d7097b476429f6ca3c828391f10da3da77c04ec755d",
		},
		{
			// ⑤ 空三元组：nil ext 与全空字段都落在这里（derefString 抹平 nil 与 ""）。
			name:    "empty",
			token:   "",
			refresh: "",
			expires: 0,
			wantPreimageHex: "0000000000000000" + // 段 0 token（空）
				"0000000000000000" + // 段 1 refresh（空）
				"00000000000000080000000000000000" + // 段 2 expires（seg64(0)）
				"",
			wantDigestHex: "32f59d1c8e62ee49bedccf59007194eebbc698b888050723c5325a746463e3c2",
		},
		{
			// ⑥ 长度前缀对 A：("ab","c",1)
			name:    "slot_ab_c",
			token:   "ab",
			refresh: "c",
			expires: 1,
			wantPreimageHex: "00000000000000026162" + // 段 0 token
				"000000000000000163" + // 段 1 refresh
				"00000000000000080000000000000001" + // 段 2 expires
				"",
			wantDigestHex: "b194b34116edfc856219b4fcd644db4016da7a0fc63af34fde796288964111af",
		},
		{
			// ⑥ 长度前缀对 B：("a","bc",1)
			name:    "slot_a_bc",
			token:   "a",
			refresh: "bc",
			expires: 1,
			wantPreimageHex: "000000000000000161" + // 段 0 token
				"00000000000000026263" + // 段 1 refresh
				"00000000000000080000000000000001" + // 段 2 expires
				"",
			wantDigestHex: "f87fa95e19e547239ad8509bff136984d71bd75394b771a0d1f0829db4f553e9",
		},
	}
	require.Len(t, vectors, 7, "6 组（第 6 组是长度前缀对，两个成员各钉一份字节）")

	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			pre := payloadDigestPreimage(v.token, v.refresh, v.expires)
			require.Equal(t, payloadDigestPreimageLen(v.token, v.refresh), len(pre),
				"前像长度必须与 payloadDigestPreimageLen 一致（编码不做二次扩容）")

			// 断言 1：钉住的前像 hex == helper 的输出。
			require.Equal(t, v.wantPreimageHex, hex.EncodeToString(pre), changed)

			// 断言 2：sha256(钉住的前像) == payloadDigestOf(...)。
			pinned, err := hex.DecodeString(v.wantPreimageHex)
			require.NoError(t, err, "钉住的前像必须是合法 hex")
			pinnedSum := sha256.Sum256(pinned)
			got := payloadDigestOf(v.token, v.refresh, v.expires)
			require.Equal(t, hex.EncodeToString(pinnedSum[:]), hex.EncodeToString(got[:]),
				"前像与编码器分叉：sha256(钉住的前像) 必须等于 payloadDigestOf"+changed)

			// 断言 3：密文 == 钉住的密文。
			require.Equal(t, v.wantDigestHex, hex.EncodeToString(got[:]), changed)
		})
	}

	// "只变 expires"的显式对照（④ vs ①）：钉住值之外再钉一条**关系**，让"两边被
	// 谁顺手同步改了"这种事故也无法把 ④ 变成空转。
	require.NotEqual(t,
		payloadDigestOf("at", "rt", payloadFixtureExpires),
		payloadDigestOf("at", "rt", payloadFixtureExpires+1),
		"只变 expires 必须换摘要：它是过期时刻进编码的唯一证据")

	// 长度前缀对的显式不等：裸拼接会让两者都是 "abc1"。
	require.NotEqual(t, payloadDigestOf("ab", "c", 1), payloadDigestOf("a", "bc", 1),
		"长度前缀失效：(\"ab\",\"c\",1) 与 (\"a\",\"bc\",1) 被折成同一摘要")
}

// TestPayloadDigestSensitivity 钉住"摘要相等 ⟺ 三元组逐项相等"。
//
// 与 KAT 互补：KAT 钉字节，这里钉**关系**（双向——不得合并今天不同的三元组，也不得
// 拆分今天相同的三元组）。三枚分量各取 3 值 ⇒ 9 份摘要、36 次两两比较，跨维允许与
// 基线重复（那是正确语义）。
func TestPayloadDigestSensitivity(t *testing.T) {
	type triple struct {
		token, refresh string
		expires        int64
	}
	dims := []struct {
		name  string
		cases []triple
	}{
		{"token", []triple{
			{"at", "rt", payloadFixtureExpires},
			{"at2", "rt", payloadFixtureExpires},
			{"", "rt", payloadFixtureExpires},
		}},
		{"refresh", []triple{
			{"at", "rt", payloadFixtureExpires},
			{"at", "rt2", payloadFixtureExpires},
			{"at", "", payloadFixtureExpires},
		}},
		{"expires", []triple{
			{"at", "rt", payloadFixtureExpires},
			{"at", "rt", payloadFixtureExpires + 1},
			{"at", "rt", 0},
		}},
	}
	require.Len(t, dims, 3, "三枚分量")

	type sample struct {
		label  string
		triple triple
		digest [32]byte
	}
	var all []sample
	withinDim := 0
	for _, dim := range dims {
		require.Len(t, dim.cases, 3, "%s 维必须取 3 个值", dim.name)
		got := make([]sample, 0, 3)
		for i, tr := range dim.cases {
			got = append(got, sample{
				label:  dim.name + "#" + itoa(i),
				triple: tr,
				digest: payloadDigestOf(tr.token, tr.refresh, tr.expires),
			})
		}
		for i := range got {
			for j := i + 1; j < len(got); j++ {
				require.NotEqual(t, got[i].digest, got[j].digest,
					"同维两值必须不同摘要：%s", dim.name)
				withinDim++
			}
		}
		all = append(all, got...)
	}
	require.Equal(t, 9, len(all), "3 维 × 3 值")
	require.Equal(t, 9, withinDim, "同维两两比较次数")

	equalPairs, unequalPairs := 0, 0
	for i := range all {
		for j := i + 1; j < len(all); j++ {
			wantEq := all[i].triple == all[j].triple
			gotEq := all[i].digest == all[j].digest
			require.Equal(t, wantEq, gotEq,
				"摘要相等必须 ⟺ 三元组相等：%s vs %s", all[i].label, all[j].label)
			if wantEq {
				equalPairs++
			} else {
				unequalPairs++
			}
		}
	}
	require.Positive(t, equalPairs, "跨维与基线重复是**正确**语义：必须真的存在相等的类")
	require.Positive(t, unequalPairs, "判据不能靠恒相等通过")

	// int64 全序单射（uint64 双射）：负数按二补数编码，-1 / 0 / 1 两两不同。
	// 十进制串编码会在这里留下漏洞（"-1" 与 "1" 尚可，但 "0" 与 "-0" 不行）。
	neg := payloadDigestOf("a", "b", -1)
	zero := payloadDigestOf("a", "b", 0)
	pos := payloadDigestOf("a", "b", 1)
	require.NotEqual(t, neg, zero, "-1 与 0 不得同摘要")
	require.NotEqual(t, zero, pos, "0 与 1 不得同摘要")
	require.NotEqual(t, neg, pos, "-1 与 1 不得同摘要")

	// 定长段 vs 变长段的边界：expires 段长度前缀恒为 8，故"令牌里塞了相同字节"也
	// 不会与整数段混淆（两段的长度前缀不同——token 段的 len 是 |token|）。
	require.NotEqual(t, payloadDigestOf("\x00\x00\x00\x00\x00\x00\x00\x08", "", 0),
		payloadDigestOf("", "", 0),
		"字符串内容与整数段不得同摘要（长度前缀把两者分开）")
}

// payloadDigestRealShape 是真实 codex 载荷形状：token 2048B / refresh 256B ⇒ 前像
// 24 + 2048 + 256 + 8 = 2336B（落在 4096 的栈上缓冲内）。
//
// 与基准（bench_alloc_test.go 的 digestCodexOAuthFixture）用同一组 helper——"KAT 钉
// 的字节、基准测的形状、分配断言用的输入"是**同一份**（factsDigest 的 overflow 形状
// 也是这个规矩）。
func payloadDigestRealShape() (token, refresh string, expires int64) {
	return codexOAuthTokenShape(codexOAuthTokenBytes), codexOAuthRefreshShape(codexOAuthRefreshBytes), payloadFixtureExpires
}

// payloadDigestOverflowShape 是跨过 4096 上界的形状：token 8192B / refresh 256B ⇒
// 前像 8480B ⇒ 恰好一次 make（不二次扩容）。取 8192 而不是"刚好 4097"：这是真实世界
// 里可能出现的上界形状（超大 JWT），不是贴着阈值的构造。
func payloadDigestOverflowShape() (token, refresh string, expires int64) {
	return codexOAuthTokenShape(8192), codexOAuthRefreshShape(codexOAuthRefreshBytes), payloadFixtureExpires
}

// payloadDigestSink 是分配断言的汇：把结果存到包级变量，防编译器把整轮调用消掉。
var payloadDigestSink [32]byte

// TestPayloadDigestAllocationShapes 是 H8：inline 上界的两侧各钉一条。
//
//   - 真实形状（2336B ≤ 4096）**零堆分配**。这不是性能优化验收，是编码形状的硬约束：
//     一旦有人改用 `sha256.New()` + `h.Sum(nil)`（domain.hashFields 的路子，恒 1 次
//     32B 分配）或把 `[]byte(s)` 漏出去，本断言立刻红。
//   - 8192 级形状**恰好一次** make：多于一次说明长度算错（做了二次扩容）。
func TestPayloadDigestAllocationShapes(t *testing.T) {
	token, refresh, expires := payloadDigestRealShape()
	require.LessOrEqual(t, payloadDigestPreimageLen(token, refresh), payloadDigestInlineCap,
		"真实形状必须落在栈上缓冲内（上界 = 4096）")
	allocs := testing.AllocsPerRun(200, func() {
		payloadDigestSink = payloadDigestOf(token, refresh, expires)
	})
	require.Zero(t, allocs, "真实形状必须零堆分配（inlineCap 4096 ≥ 2336 + sha256.Sum256 不逃逸）")

	t.Run("越过上界恰好一次分配", func(t *testing.T) {
		ot, or, oe := payloadDigestOverflowShape()
		require.Greater(t, payloadDigestPreimageLen(ot, or), payloadDigestInlineCap,
			"溢出形状必须真的跨过 inline 上界")
		allocs := testing.AllocsPerRun(100, func() {
			payloadDigestSink = payloadDigestOf(ot, or, oe)
		})
		require.Equal(t, 1.0, allocs, "超出 4096 只允许一次 make（多于一次说明长度算错或发生了扩容）")
	})

	// 4096 的阈值本身：前像刚好等于上界时仍走栈上缓冲（不是"≥ 上界即 make"）。
	t.Run("恰好等于上界仍零分配", func(t *testing.T) {
		// 24 + |token| + |refresh| + 8 == 4096 ⇒ |token| = 4096 - 32 - |refresh|
		const refresh = ""
		tok := codexOAuthTokenShape(payloadDigestInlineCap - 32 - len(refresh))
		require.Equal(t, payloadDigestInlineCap, payloadDigestPreimageLen(tok, refresh),
			"夹具必须恰好落在上界上")
		allocs := testing.AllocsPerRun(200, func() {
			payloadDigestSink = payloadDigestOf(tok, refresh, payloadFixtureExpires)
		})
		require.Zero(t, allocs, "前像长度 == inlineCap 时必须仍走栈上缓冲（判据是 > 而不是 ≥）")
	})
}

// benchCost 是 testing.Benchmark 的两列读数（allocs/op、B/op）——与 `-benchmem`
// 打印的两列同源同算法，故这里钉的数值就是基准输出里那两列。
type benchCost struct {
	allocsPerOp int64
	bytesPerOp  int64
}

// measureBenchCost 跑一次 testing.Benchmark 并读回两列。
func measureBenchCost(f func(b *testing.B)) benchCost {
	res := testing.Benchmark(f)
	return benchCost{allocsPerOp: res.AllocsPerOp(), bytesPerOp: res.AllocedBytesPerOp()}
}

// TestStaticDigestBuildCodexOAuthShapeCost 是 H7 的构造期半边：**真实形状夹具**
// （token 2048B / refresh 256B）上 allocs/op == 10、B/op == 920。
//
// 为什么必须用真实形状：旧夹具 OAuth 全空，`payloadDigestOf` 只对 24B 前像取摘要，
// 测不到 2KB 令牌的成本；而"4096 够不够"正是这次要钉的事——所以在真实形状上断言
// 分配数与空载荷形状**逐项相同**（摘要零堆分配 + 缓存位点不抬分配类）。
//
// 920 B/op 的构成（改前改后逐项不变）：snapshotStatic 760B → 768 分配类 + groupIDs
// 拷贝 16B + 其余构造期小对象；10 allocs 与改前相同。
func TestStaticDigestBuildCodexOAuthShapeCost(t *testing.T) {
	build := func(a domain.Account, tp *domain.Template, gids []int64) func(b *testing.B) {
		return func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				benchmarkSnapshotStatic = newSnapshotStatic(a, tp, gids, directEgress(), nil)
			}
		}
	}
	a, tp, gids := digestCodexOAuthFixture()
	got := measureBenchCost(build(a, tp, gids))
	t.Logf("真实 codex 形状（token %dB / refresh %dB ⇒ 前像 %dB）：%d allocs/op, %d B/op",
		codexOAuthTokenBytes, codexOAuthRefreshBytes,
		payloadDigestPreimageLen(*a.Ext.CodexOAuthToken, *a.Ext.CodexOAuthRefreshToken),
		got.allocsPerOp, got.bytesPerOp)
	// 分配**次数**与 -race 无关，两种模式都断言。
	require.Equal(t, int64(10), got.allocsPerOp,
		"真实载荷形状不得增加分配：摘要必须留在 4096 的栈上缓冲内")
	if racetest.Enabled {
		// P3a 遗留：这里的两个判据是**精确字节数**，而 race 插桩会给部分分配加影子
		// 元数据（本用例基线 -race 下 920 → 944 B/op）。精确字节数只在非 race 模式成立；
		// race 模式保留分配次数断言（上面那条）与下面的日志，功能面不受影响。
		t.Logf("race 模式：跳过精确 B/op 判据（插桩改变分配字节数，实测 920→944）")
		return
	}
	require.Equal(t, int64(920), got.bytesPerOp,
		"B/op 不得变：snapshotStatic 仍在 768 分配类（缓存位点 32B 不换档）")

	// 对照：空载荷夹具与真实形状**逐项相同** ⇒ "真实形状补测"不是空谈，它证明的是
	// 摘要成本与令牌长度无关（只与是否跨过 inlineCap 有关）。
	ea, etp, egids := digestFixture()
	empty := measureBenchCost(build(ea, etp, egids))
	require.Equal(t, empty, got, "空载荷与真实载荷的构造期成本必须逐项相同")
}

// TestStaticKeyOfCost 是 H7 的比较期半边：staticKeyOf ⇒ allocs/op == 1、B/op == 32
// （叶子复用判据的唯一路径；改前 261–267 ns/op，改后载荷侧从"逐次读三枚明文字段"
// 退化成"读回一枚 [32]byte"）。
func TestStaticKeyOfCost(t *testing.T) {
	a, tp, gids := digestCodexOAuthFixture()
	av := newSnapshotStatic(a, tp, gids, directEgress(), nil)
	got := measureBenchCost(func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			benchmarkStaticKey = staticKeyOf(av)
		}
	})
	t.Logf("staticKeyOf：%d allocs/op，%d B/op（race=%v）", got.allocsPerOp, got.bytesPerOp, racetest.Enabled)
	require.Equal(t, int64(1), got.allocsPerOp, "staticKeyOf 只允许 1 次分配（返回值的堆槽）")
	if racetest.Enabled {
		// 同 P3a 遗留：精确字节数在 race 插桩下不成立（本用例本机恰好没漂，但同类
		// 断言没有契约保证，按 racetest 的同一规则处理）：分配次数仍断言，字节数只 log。
		return
	}
	require.Equal(t, int64(32), got.bytesPerOp, "staticKeyOf 的 B/op 是 32（现状值，与改前逐项相同）")
}

// TestConstructorPayloadEquivalenceClasses 钉住折叠**没有**拆分改前相同的载荷键：
//
//   - ptr(nil) 与 ptr("") 同键（沿用 derefString）；
//   - nil expires 与 UnixNano 0 同键（沿用改前语义；time.Time{} 的 UnixNano 是一个
//     巨大的负值，不是 0，故用 time.Unix(0,0) 表达"零时刻"）；
//   - ext 整体为 nil 与"ext 存在但三元组全空"同**载荷**键（构造器无条件取摘要，
//     两者都落到空三元组的摘要上；改前 payloadKeyOf 的 ext-nil 分支同样产出零值三元组）。
//
// 判据只看 payloadKey：整枚 staticKey 在"ext 整体为 nil"那一例里仍会不同——那是
// factsDigest 的账（CodexAccountID 从 "acct-up" 变 ""），与折叠无关，改前也一样。
//
// 反方向也钉一条：真的不同就必须不同（否则上面的"同键"可能是"永远同键"）。
func TestConstructorPayloadEquivalenceClasses(t *testing.T) {
	leaf := func(mut func(a *domain.Account, ext *domain.AccountExt)) *snapshotStatic {
		a, tp, gids := payloadFixture()
		ext := *a.Ext
		a.Ext = &ext
		mut(&a, a.Ext)
		return newSnapshotStatic(a, tp, gids, directEgress(), nil)
	}
	payloadOf := func(mut func(a *domain.Account, ext *domain.AccountExt)) payloadKey {
		return payloadKeyOf(leaf(mut))
	}
	same := func(t *testing.T, name string, left, right func(a *domain.Account, ext *domain.AccountExt)) {
		t.Helper()
		l, r := payloadOf(left), payloadOf(right)
		require.True(t, l == r, "%s：改前同键的形态，折叠后也必须同键", name)
	}

	// 每对都显式构造两侧：拿夹具（token "at-0"、expires 2030）当基线比"清空"是错的
	// ——那测的是"非空 vs 空"，当然不等。
	same(t, `ptr(nil) 与 ptr("")`,
		func(_ *domain.Account, ext *domain.AccountExt) { ext.CodexOAuthToken = nil },
		func(_ *domain.Account, ext *domain.AccountExt) { ext.CodexOAuthToken = new("") })
	same(t, "nil expires 与 UnixNano 0",
		func(_ *domain.Account, ext *domain.AccountExt) { ext.CodexOAuthExpiresAt = nil },
		func(_ *domain.Account, ext *domain.AccountExt) {
			zero := time.Unix(0, 0)
			ext.CodexOAuthExpiresAt = &zero
		})
	same(t, "ext 整体为 nil 与全空三元组",
		func(a *domain.Account, _ *domain.AccountExt) { a.Ext = nil },
		func(_ *domain.Account, ext *domain.AccountExt) {
			ext.CodexOAuthToken = nil
			ext.CodexOAuthRefreshToken = nil
			ext.CodexOAuthExpiresAt = nil
		})

	// 非空转：真的变了就必须换载荷键（每维一条，覆盖三枚分量），且不动 planKey。
	base := leaf(func(*domain.Account, *domain.AccountExt) {})
	for _, tc := range []struct {
		name string
		mut  func(a *domain.Account, ext *domain.AccountExt)
	}{
		{"token 变化", func(_ *domain.Account, ext *domain.AccountExt) { ext.CodexOAuthToken = new("at-changed") }},
		{"refresh 变化", func(_ *domain.Account, ext *domain.AccountExt) { ext.CodexOAuthRefreshToken = new("rt-changed") }},
		{"expires 变化", func(_ *domain.Account, ext *domain.AccountExt) {
			exp := time.Unix(0, payloadFixtureExpires+1)
			ext.CodexOAuthExpiresAt = &exp
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := leaf(tc.mut)
			require.False(t, payloadKeyOf(base) == payloadKeyOf(got),
				"%s 必须换载荷键（否则上面的同键断言可能是恒同键）", tc.name)
			require.True(t, planKeyOf(base) == planKeyOf(got),
				"%s 不得动 planKey（载荷只换叶子内容，不作废在途计划）", tc.name)
			require.False(t, staticKeyOf(base) == staticKeyOf(got), "%s 必须换叶（staticKey 不等）", tc.name)
		})
	}
}
