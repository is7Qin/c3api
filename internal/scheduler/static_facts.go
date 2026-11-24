// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"crypto/sha256"
	"encoding/binary"

	"github.com/is7qin/c3api/internal/credential"
)

// ProviderFacts 是「provider 身份事实」的**槽位表**：加一个新 provider 只需要往
// 这里加一格、再往 FactsDigest 的编码段里加一段，`planKey` 的字段数恒定。
//
// 它只装**已读好的值**，不含 `*domain.Account`：读点必须留在唯一构造函数
// （newSnapshotStatic），否则静态读闭包门禁的「谁读了哪个静态字段」会失去被扫描的
// 载体——把 `*domain.Account` 传进编码器等于把未来的 provider 读取搬出构造器，
// 门禁再也看不见它。
type ProviderFacts struct {
	// Channel 由 newSnapshotStatic 按模板凭据类型算好（credential.ChannelOf）。
	// 它是事实的一部分：同一个 installation ID 在 codex 通道与 BPS 通道上的含义
	// 不同，进键后必须能区分。
	Channel credential.Channel
	// CodexAccountID 上游账号归属（账号 ext 的 codex_account_id；BPS 与 codex 都读）。
	CodexAccountID string
	// InstallationID 账号级安装 ID。**由快照事实 $S.installationID 提供**：该叶标量
	// 只在 codex 通道定型（见 newSnapshotStatic），因此 BPS/静态通道上它恒为 ""
	// ——与叶上真正被预留消费的值同源，「键里的事实」不会比「叶子带着的事实」更细。
	InstallationID string
	// BPSProfileDigest BPS profile 的规范摘要（域函数 domain.BPSProfileDigest 派生，
	// 非 BPS 恒 ""）。与 InstallationID 一样，这里是**已冻结**的值，不是原始 profile。
	BPSProfileDigest string
}

// 编码参数：6 段 × (8 字节大端长度前缀 + 内容)。
const (
	factsDigestSegments  = 6
	factsDigestPrefixLen = 8
	// factsDigestInlineCap 是零堆分配的上界：`8*6 + Σ|seg| ≤ 512`（六段内容合计
	// ≤ 464B）。真实形状远低于它——codex 最坏合法形状（channel 5 + account 43 +
	// installation 36 + 上游键 128 + pat 20 + 48 前缀 = 280B）仍在栈缓冲内。
	//
	// 为什么是 512 而不是 256：上游键 128 字符 + 六段前缀就已经 297B，256 会让
	// **常见**形状掉进 make 分支；而 217B 形状在 384 与 512 下的耗时无差别（栈帧
	// 变大的代价小于一次堆分配）。
	factsDigestInlineCap = 512
)

// factsChannelTag 是通道标签的公共前缀：段内容统一成 `chan:<name>`。
//
// 为什么不用裸通道名：`"bps"` / `"codex-oauth"` 这些拼写是**凭据类型字面量**，
// 调度层的 credential_branch_gate_test.go 明令禁止（按类型字面量分支正是要消灭的
// 缺陷形状）。通道标签是另一层事实，带上 `chan:` 前缀既不会与类型字面量混淆，
// 也让读者一眼看出「这一段是通道，不是类型」。
const factsChannelTag = "chan:"

// factsChannelLabel 把通道映射为**固定标签**（段内容，进长度前缀编码）。
//
// 表外通道（未来新增、越界）走 `chan:<十进制>` 分支 ⇒ 通道维在摘要里**无条件
// 单射**，不依赖「今天只有三个通道」，也不会与三条具名标签相撞。
func factsChannelLabel(ch credential.Channel) string {
	switch ch {
	case credential.ChannelStaticKey:
		return "chan:static"
	case credential.ChannelCodex:
		return "chan:codex"
	case credential.ChannelBPS:
		return "chan:bps" // 整枚标签写全，不拆成 factsChannelTag + "bps"：裸 "bps" 是凭据类型字面量
	default:
		return factsChannelTag + itoa(int(ch))
	}
}

// factsDigestPreimageLen 是前像的精确长度（编码不做二次扩容）。
func factsDigestPreimageLen(f ProviderFacts, upstreamKey, pat string) int {
	return factsDigestSegments*factsDigestPrefixLen +
		len(factsChannelLabel(f.Channel)) + len(f.CodexAccountID) +
		len(f.InstallationID) + len(f.BPSProfileDigest) +
		len(upstreamKey) + len(pat)
}

// FactsDigest 把 provider 身份事实与凭据原文折叠成 planKey 的 32 字节摘要（U40）。
//
// 编码（**段序固定**，字节级由 static_facts_kat_test.go 与
// testdata/facts_digest_vectors.py 的独立实现双向钉住）：
//
//	段 0  channel            标签：static-key / codex / bps / ch=<十进制>
//	段 1  codexAccountID
//	段 2  installationID
//	段 3  bpsProfileDigest
//	段 4  upstreamKey
//	段 5  pat
//
// 每段 = 8 字节大端长度前缀 + 内容，六段串联后 sha256。长度前缀使
// ("ab","c") 与 ("a","bc") 不撞（裸拼接会）；段数是常量，故不必再编码段数。
//
// pat 的进键语义与改前的独立字段**逐字节一致**：BPS 通道由调用方传 ""（BPS 分支
// 从来不读 PAT），其余形状传 ext 的 codex_pat_key（缺失 = ""）。
//
// 分配：`8*6 + Σ|seg| ≤ 512` 时零堆分配（栈上 inline 缓冲 + sha256.Sum256，
// 不用 domain.hashFields——那枚函数是 `sha256.New()`+`h.Sum(nil)`，恒 1 次 32B
// 分配，且 fieldString 的 `[]byte(s)` 逃逸）；超出时才退化为一次 make。
func FactsDigest(f ProviderFacts, upstreamKey, pat string) [32]byte {
	var inline [factsDigestInlineCap]byte
	n := factsDigestPreimageLen(f, upstreamKey, pat)
	buf := inline[:]
	if n > factsDigestInlineCap {
		buf = make([]byte, n)
	}
	buf = buf[:n]
	factsDigestEncodeInto(buf, f, upstreamKey, pat)
	return sha256.Sum256(buf)
}

// factsDigestEncodeInto 把前像写进 dst（长度必须恰为 factsDigestPreimageLen）。
//
// 抽成独立函数**不是**为了复用而抽象：KAT 必须能拿同一份编码逻辑产出前像，
// 否则"钉住前像"就只是把测试自己写的那份编码再抄一遍（前像断言会变成自证）。
// 它是 FactsDigest 的唯一编码实现，测试侧的 factsDigestPreimage 只是给它配一块
// 可读的缓冲。
func factsDigestEncodeInto(dst []byte, f ProviderFacts, upstreamKey, pat string) {
	n := 0
	n += putFactsSegment(dst[n:], factsChannelLabel(f.Channel))
	n += putFactsSegment(dst[n:], f.CodexAccountID)
	n += putFactsSegment(dst[n:], f.InstallationID)
	n += putFactsSegment(dst[n:], f.BPSProfileDigest)
	n += putFactsSegment(dst[n:], upstreamKey)
	putFactsSegment(dst[n:], pat)
}

// putFactsSegment 写入「8 字节大端长度前缀 + 内容」，返回写入的字节数。
// 调用方保证 dst 至少有这么长（长度由 factsDigestPreimageLen 精确算出）。
func putFactsSegment(dst []byte, seg string) int {
	binary.BigEndian.PutUint64(dst[:factsDigestPrefixLen], uint64(len(seg)))
	return factsDigestPrefixLen + copy(dst[factsDigestPrefixLen:], seg)
}
