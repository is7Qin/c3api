// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

//go:fix inline
func int64Ptr(v int64) *int64 { return new(v) }

func TestCallCostFromResolved_ZeroPrice_NoPanic(t *testing.T) {
	rp := domain.ResolvedPrices{PricePerCall: new(int64(0))}
	require.NotPanics(t, func() {
		require.Equal(t, int64(0), CallCostFromResolved(rp, 10))
		require.Equal(t, int64(0), CallCostFromResolved(rp, 0))
		require.Equal(t, int64(0), CallCostFromResolved(rp, 1<<60))
	})
	// nil price also 0
	require.Equal(t, int64(0), CallCostFromResolved(domain.ResolvedPrices{PricePerCall: nil}, 10))
}

func TestCallCostFromResolved_NegativePrice_Defense(t *testing.T) {
	rp := domain.ResolvedPrices{PricePerCall: new(int64(-5))}
	require.Equal(t, int64(0), CallCostFromResolved(rp, 10))
	rp2 := domain.ResolvedPrices{PricePerCall: new(int64(-1 << 60))}
	require.Equal(t, int64(0), CallCostFromResolved(rp2, 1<<30))
}

func TestImageCostFromResolved_ZeroPrice_NoPanic(t *testing.T) {
	rp := domain.ResolvedPrices{PricePerImage: new(int64(0)), ImgInTokPerM: new(int64(0)), ImgOutTokPerM: new(int64(0))}
	require.Equal(t, int64(0), ImageCostFromResolved(rp, 100, 100, 10))
}

func TestCostFromResolved_ZeroPrice(t *testing.T) {
	zero := int64(0)
	rp := domain.ResolvedPrices{InputPerM: &zero, OutputPerM: &zero, CacheReadPerM: &zero, CacheWritePerM: &zero}
	require.Equal(t, int64(0), CostFromResolved(rp, 100, 200, 300, 400))
}

// TestCostFromResolved_CacheLanesDisjoint 计费四分量不相交（P0-2）：pt 为 net
// （uncached），缓存读/写各走独立单价——缓存部分绝不再按 input 价重复计费。
func TestCostFromResolved_CacheLanesDisjoint(t *testing.T) {
	in, out, cr, cw := int64(1e7), int64(2e7), int64(1e6), int64(3e6)
	rp := domain.ResolvedPrices{InputPerM: &in, OutputPerM: &out, CacheReadPerM: &cr, CacheWritePerM: &cw}
	// gross input 100 = uncached 0 + cache_read 40 + cache_write 60（cached/cc ⊆ input）。
	// 计费 = 0×1e7 + 10×2e7 + 40×1e6 + 60×3e6 = 420,000,000 毫分 / 1e6 = 420。
	require.Equal(t, int64(420), CostFromResolved(rp, 0, 10, 40, 60))
	// 旧的双重计费会把 gross(100) 当 pt：100×1e7 + … = 1420，故断言必须失败。
	require.NotEqual(t, int64(1420), CostFromResolved(rp, 0, 10, 40, 60), "缓存不得再按 input 价计费")
}

// TestImageCostFromResolved_PerImageWinsOverTokens 分量互斥、按张优先（用户
// 裁决）：同一行同时配了图像 token 价与按张价时，只收按张——**不再相加**。
// 旧实现是 tokenCost + count×perImage，这条断言会看到 5600 而不是 600。
func TestImageCostFromResolved_PerImageWinsOverTokens(t *testing.T) {
	in, out := int64(1_000_000), int64(2_000_000) // 毫分/1M image tokens
	per := int64(300)                             // 毫分/张
	rp := domain.ResolvedPrices{ImgInTokPerM: &in, ImgOutTokPerM: &out, PricePerImage: &per}

	// token 口径本应是 (1000×1e6 + 2000×2e6)/1e6 = 5000 毫分；按张 2 张 = 600。
	require.Equal(t, int64(600), ImageCostFromResolved(rp, 1000, 2000, 2), "按张优先，不得叠加 token")
	require.NotEqual(t, int64(5600), ImageCostFromResolved(rp, 1000, 2000, 2), "不得再相加")
}

// TestImageCostFromResolved_TokensOnly token 分量仍在没有按张价时生效。
func TestImageCostFromResolved_TokensOnly(t *testing.T) {
	in, out := int64(1_000_000), int64(2_000_000)
	rp := domain.ResolvedPrices{ImgInTokPerM: &in, ImgOutTokPerM: &out}
	require.Equal(t, int64(5000), ImageCostFromResolved(rp, 1000, 2000, 2))
}

// TestImageCostFromResolved_ExplicitFreeBeatsTokens 显式配按张 0（免费）同样
// 压过 token 价：判据是「列非 NULL」而不是「> 0」——否则「设成 0 免费」会被
// 静默改写成按 token 收费。
func TestImageCostFromResolved_ExplicitFreeBeatsTokens(t *testing.T) {
	in, out := int64(1_000_000), int64(2_000_000)
	zero := int64(0)
	rp := domain.ResolvedPrices{ImgInTokPerM: &in, ImgOutTokPerM: &out, PricePerImage: &zero}
	require.Equal(t, int64(0), ImageCostFromResolved(rp, 1000, 2000, 2), "按张 0 = 免费，不得回落 token")
}

// TestImageCostFromResolved_NoCountFallsBackToTokens 没有张数就没有按张乘数，
// 回落到图像 token（唯一有数据支撑的口径）。
func TestImageCostFromResolved_NoCountFallsBackToTokens(t *testing.T) {
	in, out := int64(1_000_000), int64(2_000_000)
	per := int64(300)
	rp := domain.ResolvedPrices{ImgInTokPerM: &in, ImgOutTokPerM: &out, PricePerImage: &per}
	require.Equal(t, int64(5000), ImageCostFromResolved(rp, 1000, 2000, 0))
}

// TestImageCostFromResolved_TextPathPerImageLane 文本格式的「视觉旁路」调用形
// 态（in/out 传 0，只按张数计）保持原样：只有按张分量。
func TestImageCostFromResolved_TextPathPerImageLane(t *testing.T) {
	per := int64(300)
	rp := domain.ResolvedPrices{PricePerImage: &per}
	require.Equal(t, int64(600), ImageCostFromResolved(rp, 0, 0, 2))
}
