// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"crypto/sha256"
)

// 载荷摘要（P3a）：把 OAuth 访问令牌三元组折叠成一枚 [32]byte。
//
// 前像 = seg(token) || seg(refresh) || seg64(expires)，**段数恒为 3**：
//
//	seg(s)   = u64be(len(s)) || s                  // 复用 putFactsSegment
//	seg64(v) = u64be(8)      || u64be(uint64(v))   // 共 16B（同族前缀，不碰 putFactsSegment）
//
// 前像长度 = 24 + len(token) + len(refresh) + 8。
//
// 等价关系与改前的三枚明文字段**逐项一致**（判据见 payload_digest_test.go 与
// testdata/payload_digest_vectors.py 的独立实现）：
//
//   - 定数 3 段 TLV ⇒ 单射：("ab","c") 与 ("a","bc") 的前像不同（长度前缀消除拼接
//     歧义；裸拼接会让两者都是 "abc"）；
//   - uint64(v) 双射 ⇒ int64 单射：负数按二补数编码，-1 / 0 / 1 两两不同。
//     不用十进制串：十进制不是单射（"01" 与 "1" 是两条不同的串而同一个数），会凭空
//     多出可区分的键；
//   - nil 与 ptr("") 同键（derefString 在构造器里已抹平，摘要只吃 string）；
//   - nil expires 与 UnixNano 0 同键（沿用改前语义，已知且实际不可达）。
//
// 为什么不用 strconv/fmt：编码在构造期热路径上（每次重载每账号一次），且十进制
// 表示把"整数入键"变成"字符串入键"，单射性要额外论证——定宽二进制没有这个问题。
const (
	// payloadDigestInlineCap 是零堆分配的上界。实测（momus 与本次基准）：
	// 真实 codex 形状（token 2048B / refresh 256B ⇒ 前像 2336B）在 512 下是
	// 1 alloc/2688B，在 4096 下 0 alloc；8192 级形状恰 1 alloc（一次 make，不二次
	// 扩容——长度由 payloadDigestPreimageLen 精确算出）。
	payloadDigestInlineCap = 4096
	// payloadDigestSegments 是前像的段数。每段都带 8 字节大端长度前缀，故段数本身
	// 不必再编码；它是**常量**，所以 TLV 的单射性不依赖"段数写进前像"。
	payloadDigestSegments = 3
)

// payloadDigestPreimageLen 是前像的精确长度：24（三段前缀）+ |token| + |refresh|
// + 8（seg64 的定长内容）。编码据此一次定型缓冲，不做二次扩容。
func payloadDigestPreimageLen(token, refresh string) int {
	return payloadDigestSegments*factsDigestPrefixLen + len(token) + len(refresh) + 8
}

// payloadDigestOf 把 OAuth 令牌三元组折叠成 payloadKey 的 32 字节摘要。
//
// **纯函数**：它不吃 *domain.Account。读点必须留在唯一构造函数 newSnapshotStatic
// ——否则静态读闭包门禁看不见"谁读了凭据原文"，"调度包里只有构造函数读 OAuth 令牌"
// 就不再是机械事实（与 static_facts.go 的 FactsDigest 同理）。
func payloadDigestOf(token, refresh string, expiresUnixNano int64) [32]byte {
	var inline [payloadDigestInlineCap]byte
	n := payloadDigestPreimageLen(token, refresh)
	buf := inline[:]
	if n > payloadDigestInlineCap {
		buf = make([]byte, n)
	}
	buf = buf[:n]
	payloadDigestEncodeInto(buf, token, refresh, expiresUnixNano)
	return sha256.Sum256(buf)
}

// payloadDigestEncodeInto 把前像写进 dst（长度必须恰为 payloadDigestPreimageLen）。
//
// 抽成独立函数**不是**为了复用而抽象：KAT 必须能拿同一份编码逻辑产出前像，否则
// "钉住前像"就只是把测试自己写的那份编码再抄一遍（前像断言会退化成自证）。它是
// payloadDigestOf 的唯一编码实现，测试侧的 payloadDigestPreimage 只负责配一块可读
// 的缓冲。
func payloadDigestEncodeInto(dst []byte, token, refresh string, expiresUnixNano int64) {
	n := 0
	n += putFactsSegment(dst[n:], token)
	n += putFactsSegment(dst[n:], refresh)
	putFactsInt64(dst[n:], expiresUnixNano)
}
