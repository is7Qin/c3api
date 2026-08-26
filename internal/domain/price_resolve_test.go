// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func int64PtrD(v int64) *int64 { return &v }
func intPtrD(v int) *int       { return &v }

func TestResolveEntryPrices_SetPricePerCall(t *testing.T) {
	entry := &PriceEntry{Model: "search-m", Mode: PriceModeCall, PricePerCall: int64PtrD(1000)}
	custom := int64(9999)
	variants := []*PriceVariant{{Model: "search-m", Seq: 1, SetPricePerCall: &custom}}
	rp, ok := ResolveEntryPrices(entry, variants, "auto", 0, time.Now())
	require.True(t, ok)
	require.NotNil(t, rp.PricePerCall)
	require.Equal(t, custom, *rp.PricePerCall)
}

func TestResolveEntryPrices_ImageOverrides(t *testing.T) {
	entry := &PriceEntry{Model: "img-m", Mode: PriceModeImage, ImgInTokPerM: int64PtrD(100), ImgOutTokPerM: int64PtrD(200), PricePerImage: int64PtrD(300)}
	imgIn := int64(111)
	imgOut := int64(222)
	perImg := int64(333)
	variants := []*PriceVariant{{Model: "img-m", Seq: 1, SetImgInTokPerM: &imgIn, SetImgOutTokPerM: &imgOut, SetPricePerImage: &perImg}}
	rp, ok := ResolveEntryPrices(entry, variants, "auto", 0, time.Now())
	require.True(t, ok)
	require.Equal(t, imgIn, *rp.ImgInTokPerM)
	require.Equal(t, imgOut, *rp.ImgOutTokPerM)
	require.Equal(t, perImg, *rp.PricePerImage)
}

func TestResolveEntryPrices_CacheOverrides(t *testing.T) {
	entry := &PriceEntry{Model: "tok-m", Mode: PriceModeToken, InputPerM: int64PtrD(1000), OutputPerM: int64PtrD(2000), CacheReadPerM: int64PtrD(3000), CacheWritePerM: int64PtrD(4000)}
	cr := int64(999)
	cw := int64(888)
	variants := []*PriceVariant{{Model: "tok-m", Seq: 1, SetCacheReadPerM: &cr, SetCacheCreationPerM: &cw}}
	rp, ok := ResolveEntryPrices(entry, variants, "auto", 0, time.Now())
	require.True(t, ok)
	require.Equal(t, cr, *rp.CacheReadPerM)
	require.Equal(t, cw, *rp.CacheWritePerM)
	// input/output unchanged
	require.Equal(t, int64(1000), *rp.InputPerM)
}

func TestResolveEntryPrices_MultBPClamp(t *testing.T) {
	base := int64(1000000)
	entry := &PriceEntry{Model: "m", Mode: PriceModeToken, InputPerM: int64PtrD(base), OutputPerM: int64PtrD(base)}
	// large valid price with mult 100000 should stay non-negative (no overflow wrap)
	large := int64(1_000_000_000)
	entry2 := &PriceEntry{Model: "m2", Mode: PriceModeToken, InputPerM: int64PtrD(large), OutputPerM: int64PtrD(large)}
	variants := []*PriceVariant{{Model: "m2", Seq: 1, MultBP: intPtrD(100000)}}
	rp, ok := ResolveEntryPrices(entry2, variants, "auto", 0, time.Now())
	require.True(t, ok)
	require.NotNil(t, rp.InputPerM)
	require.GreaterOrEqual(t, *rp.InputPerM, int64(0))
	// mult negative should be clamped to 0 => price 0, not negative
	variantsNeg := []*PriceVariant{{Model: "m", Seq: 1, MultBP: intPtrD(-5000)}}
	rp, ok = ResolveEntryPrices(entry, variantsNeg, "auto", 0, time.Now())
	require.True(t, ok)
	require.NotNil(t, rp.InputPerM)
	require.Equal(t, int64(0), *rp.InputPerM)
	// mult >100000 should be clamped to 100000
	variantsHuge := []*PriceVariant{{Model: "m", Seq: 1, MultBP: intPtrD(999999)}}
	rp, ok = ResolveEntryPrices(entry, variantsHuge, "auto", 0, time.Now())
	require.True(t, ok)
	require.NotNil(t, rp.InputPerM)
	// 100000 means ×10: 1_000_000 *10 =10_000_000
	require.Equal(t, int64(10000000), *rp.InputPerM)
	// mult 5000 means ×0.5 (with round): 1_000_000*5000/10000=500000
	variantsHalf := []*PriceVariant{{Model: "m", Seq: 1, MultBP: intPtrD(5000)}}
	rp, ok = ResolveEntryPrices(entry, variantsHalf, "auto", 0, time.Now())
	require.True(t, ok)
	require.Equal(t, int64(500000), *rp.InputPerM)
}
