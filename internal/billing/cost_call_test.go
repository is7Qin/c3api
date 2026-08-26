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
