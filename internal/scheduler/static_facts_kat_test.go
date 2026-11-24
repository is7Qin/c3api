// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
)

// factsDigestPreimage 是 KAT 专用的前像入口：按生产编码器算出的**精确长度**分配
// 一块缓冲，再交给同一条 factsDigestEncodeInto。
//
// 它本身只存在于测试面（生产侧只有 FactsDigest，零堆分配）；之所以不在生产文件里
// 放一个返回 []byte 的版本，是因为那条路径会恒分配 512B——而且没有生产读者。
//
// 它只负责"给编码器一块可读的缓冲"，**不重写编码**：段序/前缀宽度/哈希的任何改动
// 都会让下面钉住的前像与密文同时变红（M6 的存在性证明就在这）。
func factsDigestPreimage(f ProviderFacts, upstreamKey, pat string) []byte {
	buf := make([]byte, factsDigestPreimageLen(f, upstreamKey, pat))
	factsDigestEncodeInto(buf, f, upstreamKey, pat)
	return buf
}

// factsDigestVector 是一条字节级 KAT 向量。
type factsDigestVector struct {
	name        string
	facts       ProviderFacts
	upstreamKey string
	pat         string
	// wantPreimageHex 由 **独立实现**（testdata/facts_digest_vectors.py，Python）
	// 算出：一行一段 = 8 字节大端长度前缀 + 内容。段序被交换时这里逐行肉眼可见
	// （密文断言抓不到段序——交换两段只改变密文，而密文是"钉住的值"，只要同步
	// 改就能过；前像钉的是**结构**）。
	wantPreimageHex string
	// wantDigestHex 是同一份脚本算出的 sha256。
	wantDigestHex string
	// overCap 标记"前像跨过 inlineCap"的向量：它必须真的走 make 分支，其余必须
	// 留在栈上缓冲内——否则两条分支的覆盖会静默互换（覆盖是判据的一部分）。
	overCap bool
}

// TestFactsDigestKnownAnswer 是 FactsDigest 的**字节级** KAT（known-answer test）。
//
// 它补的是等价类测试补不到的那一维：static_key_credential_test.go 的 1125 组只约束
// 「哪些输入对进同一枚键」，对摘要值本身的算法/宽度/段序无感——换哈希、改前缀宽度、
// 交换两段都能保持等价关系不变，却让全部在途计划与已编译叶子静默换代（跨版本不可
// 复现）。本测试把**前像**与**密文**一起钉住。
//
// 每条向量**三条**断言，缺一条就漏一种变异：
//  1. 钉住的前像 hex == factsDigestPreimage(...)（抓段序/前缀/段内容）；
//  2. sha256(钉住的前像) == FactsDigest(...)（把前像与编码器连起来，防"helper 与
//     生产各走一套"）；
//  3. FactsDigest(...) 的密文 == 钉住的密文（抓哈希算法本身）。
//
// 向量 6 组覆盖 5 类：codex 典型 / BPS 典型 / 空 pat / >512B 溢出 / 槽位歧义对
// （("ab","c") vs ("a","bc")，两条各钉一份字节，合起来证明长度前缀真的进了前像）。
//
// 它同时继任 static_key_hash_kat_test.go：那枚 2 段 pair 摘要（hashCredentialPair）
// 已被 U40 折叠进本摘要，原来的 ("sk-abc","pat-abc") 一类向量的语义在这里以
// "段 4/段 5" 的形式继续被钉住。
func TestFactsDigestKnownAnswer(t *testing.T) {
	const changed = "factsDigest 编码变了：这不仅是等价类问题（前像/摘要都被钉住，交换段序、改前缀宽度、换哈希都会红）"

	// 溢出向量的形状由 factsDigestOverflowShape 提供（与分配断言/基准同一份输入）。
	overflowFacts, overflowUpstream, overflowPat := factsDigestOverflowShape()

	vectors := []factsDigestVector{
		{
			name: "codex_typical",
			facts: ProviderFacts{
				Channel:          credential.ChannelCodex,
				CodexAccountID:   "acct_9f2c41d8e7b3",
				InstallationID:   "3f1c9a24-8b6d-4e07-9c1a-2d5b8e7f0a13",
				BPSProfileDigest: "",
			},
			upstreamKey: "sk-live-codex-0001",
			pat:         "pat-codex-0001",
			// 前像由 testdata/facts_digest_vectors.py 独立算出；一行一段 = 8B 大端长度前缀 + 内容。
			wantPreimageHex: "000000000000000a6368616e3a636f646578" + // 段 0 channel
				"0000000000000011616363745f396632633431643865376233" + // 段 1 codexAccountID
				"000000000000002433663163396132342d386236642d346530372d396331612d326435623865376630613133" + // 段 2 installationID
				"0000000000000000" + // 段 3 bpsProfileDigest
				"0000000000000012736b2d6c6976652d636f6465782d30303031" + // 段 4 upstreamKey
				"000000000000000e7061742d636f6465782d30303031" + // 段 5 pat
				"",
			wantDigestHex: "11c7ffc93837a0b5575b31ed303b2141fc6ad676dd6c45a41bcdc1920ca7c531",
			// codex 真实形状：channel + 账号 + UUID installation + 上游键 + pat（无 BPS profile）
		},
		{
			name: "bps_typical",
			facts: ProviderFacts{
				Channel:          credential.ChannelBPS,
				CodexAccountID:   "acct_7c1d5e93b204",
				InstallationID:   "",
				BPSProfileDigest: "9f2d3b1c0a7e5648d1c2b3a495f60718293a4b5c6d7e8f90a1b2c3d4e5f60718",
			},
			upstreamKey: "sk-live-bps-0001",
			pat:         "",
			// 前像由 testdata/facts_digest_vectors.py 独立算出；一行一段 = 8B 大端长度前缀 + 内容。
			wantPreimageHex: "00000000000000086368616e3a627073" + // 段 0 channel
				"0000000000000011616363745f376331643565393362323034" + // 段 1 codexAccountID
				"0000000000000000" + // 段 2 installationID
				"000000000000004039663264336231633061376535363438643163326233613439356636303731383239336134623563366437653866393061316232633364346535663630373138" + // 段 3 bpsProfileDigest
				"0000000000000010736b2d6c6976652d6270732d30303031" + // 段 4 upstreamKey
				"0000000000000000" + // 段 5 pat
				"",
			wantDigestHex: "12a04733ba523d8d8986499b8455d0a50803cced8974bfa69d244c9aa6846f62",
			// BPS 典型：installation 恒空、pat 恒空（BPS 分支从来不读 PAT）、profile 摘要有值
		},
		{
			name: "codex_empty_pat",
			facts: ProviderFacts{
				Channel:          credential.ChannelCodex,
				CodexAccountID:   "acct_9f2c41d8e7b3",
				InstallationID:   "3f1c9a24-8b6d-4e07-9c1a-2d5b8e7f0a13",
				BPSProfileDigest: "",
			},
			upstreamKey: "sk-live-codex-0001",
			pat:         "",
			// 前像由 testdata/facts_digest_vectors.py 独立算出；一行一段 = 8B 大端长度前缀 + 内容。
			wantPreimageHex: "000000000000000a6368616e3a636f646578" + // 段 0 channel
				"0000000000000011616363745f396632633431643865376233" + // 段 1 codexAccountID
				"000000000000002433663163396132342d386236642d346530372d396331612d326435623865376630613133" + // 段 2 installationID
				"0000000000000000" + // 段 3 bpsProfileDigest
				"0000000000000012736b2d6c6976652d636f6465782d30303031" + // 段 4 upstreamKey
				"0000000000000000" + // 段 5 pat
				"",
			wantDigestHex: "a136fa96731ad4d5ce65c2786b20b4f5fce0694b0e6b9840481acde9a5ff2185",
			// 空 pat：codex 通道但凭据没有 PAT（缺失 = ""，与改前的空 string 字段同义）
		},
		{
			name:        "overflow_547",
			facts:       overflowFacts,
			upstreamKey: overflowUpstream,
			pat:         overflowPat,
			overCap:     true,
			// 前像由 testdata/facts_digest_vectors.py 独立算出；一行一段 = 8B 大端长度前缀 + 内容。
			wantPreimageHex: "000000000000000a6368616e3a636f646578" + // 段 0 channel
				"0000000000000011616363745f396632633431643865376233" + // 段 1 codexAccountID
				"000000000000002433663163396132342d386236642d346530372d396331612d326435623865376630613133" + // 段 2 installationID
				"0000000000000000" + // 段 3 bpsProfileDigest
				"00000000000001a8736b2d6c6976652d3031323334353637383961626364656630313233343536373839616263646566303132333435363738396162636465663031323334353637383961626364656630313233343536373839616263646566303132333435363738396162636465663031323334353637383961626364656630313233343536373839616263646566303132333435363738396162636465663031323334353637383961626364656630313233343536373839616263646566303132333435363738396162636465663031323334353637383961626364656630313233343536373839616263646566303132333435363738396162636465663031323334353637383961626364656630313233343536373839616263646566303132333435363738396162636465663031323334353637383961626364656630313233343536373839616263646566303132333435363738396162636465663031323334353637383961626364656630313233343536373839616263646566303132333435363738396162636465663031323334353637383961626364656630313233343536373839616263646566" + // 段 4 upstreamKey
				"000000000000000c7061742d6666666666666666" + // 段 5 pat
				"",
			wantDigestHex: "bd6fdbdddc1641803a290cbd9b362b2dafd1d8c4df957b583fa59bb9fc8f9783",
			// >512B 溢出：四段合计 547B ⇒ Go 侧退化为一次 make（前像仍逐字节可复算）
		},
		{
			name: "slot_ab_c",
			facts: ProviderFacts{
				Channel:          credential.ChannelCodex,
				CodexAccountID:   "acct_9f2c41d8e7b3",
				InstallationID:   "",
				BPSProfileDigest: "",
			},
			upstreamKey: "ab",
			pat:         "c",
			// 前像由 testdata/facts_digest_vectors.py 独立算出；一行一段 = 8B 大端长度前缀 + 内容。
			wantPreimageHex: "000000000000000a6368616e3a636f646578" + // 段 0 channel
				"0000000000000011616363745f396632633431643865376233" + // 段 1 codexAccountID
				"0000000000000000" + // 段 2 installationID
				"0000000000000000" + // 段 3 bpsProfileDigest
				"00000000000000026162" + // 段 4 upstreamKey
				"000000000000000163" + // 段 5 pat
				"",
			wantDigestHex: "8e84db301de1f40f42d3e6e454259db9b0c2e1eabbbf0e29ee1edb79e600ae6e",
			// 槽位歧义对 A：("ab","c") —— 裸拼接与下一组同为 "abc"，长度前缀必须分开
		},
		{
			name: "slot_a_bc",
			facts: ProviderFacts{
				Channel:          credential.ChannelCodex,
				CodexAccountID:   "acct_9f2c41d8e7b3",
				InstallationID:   "",
				BPSProfileDigest: "",
			},
			upstreamKey: "a",
			pat:         "bc",
			// 前像由 testdata/facts_digest_vectors.py 独立算出；一行一段 = 8B 大端长度前缀 + 内容。
			wantPreimageHex: "000000000000000a6368616e3a636f646578" + // 段 0 channel
				"0000000000000011616363745f396632633431643865376233" + // 段 1 codexAccountID
				"0000000000000000" + // 段 2 installationID
				"0000000000000000" + // 段 3 bpsProfileDigest
				"000000000000000161" + // 段 4 upstreamKey
				"00000000000000026263" + // 段 5 pat
				"",
			wantDigestHex: "90a4cae5e2031699359997d0687b4c75a6612c95a40d9eb0de8420404e946fc8",
			// 槽位歧义对 B：("a","bc")
		},
	}

	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			pre := factsDigestPreimage(v.facts, v.upstreamKey, v.pat)
			// 长度口径：溢出向量必须真的跨过栈上上界，其余必须留在界内——否则
			// "溢出分支"和"零分配分支"的覆盖会静默互换。
			require.Equal(t, factsDigestPreimageLen(v.facts, v.upstreamKey, v.pat), len(pre),
				"前像长度必须与 factsDigestPreimageLen 一致（编码不做二次扩容）")
			if v.overCap {
				require.Greater(t, len(pre), factsDigestInlineCap,
					"溢出向量必须真的跨过 inlineCap：否则它证明不了 make 分支")
			} else {
				require.LessOrEqual(t, len(pre), factsDigestInlineCap,
					"这条向量必须留在栈上缓冲内（零堆分配形状）")
			}

			// 断言 1：钉住的前像 hex == helper 的输出。
			require.Equal(t, v.wantPreimageHex, hex.EncodeToString(pre), changed)

			// 断言 2：sha256(钉住的前像) == FactsDigest(...)。
			pinned, err := hex.DecodeString(v.wantPreimageHex)
			require.NoError(t, err, "钉住的前像必须是合法 hex")
			pinnedSum := sha256.Sum256(pinned)
			got := FactsDigest(v.facts, v.upstreamKey, v.pat)
			require.Equal(t, hex.EncodeToString(pinnedSum[:]), hex.EncodeToString(got[:]),
				"前像与编码器分叉：sha256(钉住的前像) 必须等于 FactsDigest"+changed)

			// 断言 3：密文 == 钉住的密文。
			require.Equal(t, v.wantDigestHex, hex.EncodeToString(got[:]), changed)
		})
	}

	// 槽位歧义对：两对碎片裸拼接同为 "abc"，长度前缀必须把它们分开。密文断言已
	// 各自钉住；这里再显式断言两者不等（否则"两条向量都钉对"可能被误读成"它们
	// 本来就该相等"）。
	abC := FactsDigest(ProviderFacts{Channel: credential.ChannelCodex, CodexAccountID: "acct_9f2c41d8e7b3"}, "ab", "c")
	aBC := FactsDigest(ProviderFacts{Channel: credential.ChannelCodex, CodexAccountID: "acct_9f2c41d8e7b3"}, "a", "bc")
	require.NotEqual(t, hex.EncodeToString(abC[:]), hex.EncodeToString(aBC[:]),
		`长度前缀失效：("ab","c") 与 ("a","bc") 被折成同一摘要`)
}
