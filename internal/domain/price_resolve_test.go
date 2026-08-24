// SPDX-License-Identifier: AGPL-3.0-or-later
package domain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func int64PtrD(v int64) *int64 { return &v }
func intPtrD(v int) *int       { return &v }

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
