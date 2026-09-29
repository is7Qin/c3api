// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func int64Ptr(v int64) *int64 { return &v }

func TestCallCostFromResolved_ZeroPrice_NoPanic(t *testing.T) {
	rp := domain.ResolvedPrices{PricePerCall: int64Ptr(0)}
	require.NotPanics(t, func() {
		require.Equal(t, int64(0), CallCostFromResolved(rp, 10))
		require.Equal(t, int64(0), CallCostFromResolved(rp, 0))
		require.Equal(t, int64(0), CallCostFromResolved(rp, 1<<60))
	})
	// nil price also 0
	require.Equal(t, int64(0), CallCostFromResolved(domain.ResolvedPrices{PricePerCall: nil}, 10))
}

func TestCallCostFromResolved_NegativePrice_Defense(t *testing.T) {
	rp := domain.ResolvedPrices{PricePerCall: int64Ptr(-5)}
	require.Equal(t, int64(0), CallCostFromResolved(rp, 10))
	rp2 := domain.ResolvedPrices{PricePerCall: int64Ptr(-1 << 60)}
	require.Equal(t, int64(0), CallCostFromResolved(rp2, 1<<30))
}

func TestImageCostFromResolved_ZeroPrice_NoPanic(t *testing.T) {
	rp := domain.ResolvedPrices{PricePerImage: int64Ptr(0), ImgInTokPerM: int64Ptr(0), ImgOutTokPerM: int64Ptr(0)}
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
