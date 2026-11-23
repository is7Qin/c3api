// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestHashCredentialPairEncodingPinned 是 hashCredentialPair 的**字节级 KAT**
// （known-answer test）：固定向量 + 写死的十六进制摘要，钉住编码
// 「8 字节大端长度前缀 + 内容，两段依次为上游键、PAT，再 sha256」。
//
// 它**不是**等价类测试——等价类（哪些凭据对进同一枚键）由
// static_key_credential_test.go 的 1125 组参照测试负责，那条判据只约束"相等关系"，
// 对摘要值本身的算法/宽度无感：换哈希、改前缀宽度、调换分段顺序都能保持等价关系
// 不变却让所有在途计划与已编译叶子静默换代（跨版本不可复现）。本测试补的正是
// 那一维：**字节**。
//
// 期望值按上述编码手工推得（例：("", "") 的前像 = 16 个零字节；
// ("sk-abc", "pat-abc") 的前像 = 00..06 00..06 "sk-abc" 00..07 "pat-abc"），
// 与实现无关地由独立工具复算确认过。
func TestHashCredentialPairEncodingPinned(t *testing.T) {
	const changed = "编码变了：这不仅是等价类问题（摘要值本身被钉住，换编码/换哈希/改前缀宽度都会红）"

	vectors := []struct {
		name     string
		upstream string
		pat      string
		want     string // hashCredentialPair 输出的 sha256，小写 hex
	}{
		{
			name:     "empty_pair",
			upstream: "",
			pat:      "",
			want:     "374708fff7719dd5979ec875d56cd2286f6d3cf7ec317a3b25632aab28ec37bb",
		},
		{
			name:     "typical_pair",
			upstream: "sk-abc",
			pat:      "pat-abc",
			want:     "7dc717be191887bf7ed5e681c4eff563537df6abf6f48b7822c7db142bf34d76",
		},
		{
			// ("ab","c") 与 ("a","bc") 裸拼接后同为 "abc"：两条向量各自钉住自己的
			// 字节，合起来证明长度前缀确实写进了前像（而不是靠分段顺序碰巧分开）。
			name:     "split_ab_c",
			upstream: "ab",
			pat:      "c",
			want:     "601d5476e2ccfe2c87a2bba7a322659734a05749d5b5aa781f513e4912db0d5f",
		},
		{
			name:     "split_a_bc",
			upstream: "a",
			pat:      "bc",
			want:     "3fafa1cf2f19a7c1129beb20cf0983f73a489a221fc0dd2f16d1be292d089205",
		},
	}

	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			got := hashCredentialPair(v.upstream, v.pat)
			require.Equal(t, v.want, hex.EncodeToString(got[:]), changed)
		})
	}

	// 长度前缀生效（不是"顺序碰巧分开"）：两对拼接相同的碎片必须落到不同摘要。
	abC := hashCredentialPair("ab", "c")
	aBC := hashCredentialPair("a", "bc")
	require.NotEqual(t, hex.EncodeToString(abC[:]), hex.EncodeToString(aBC[:]),
		"长度前缀失效：(\"ab\",\"c\") 与 (\"a\",\"bc\") 被折成同一摘要")
}
