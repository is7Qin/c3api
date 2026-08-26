// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func strPtrTZ(v string) *string { return &v }

func TestResolvePrices_TimeZone_Differential(t *testing.T) {
	// Fixed UTC instant crossing date boundary: 2026-08-23T23:30:00Z
	// In Asia/Shanghai (UTC+8) => 2026-08-24 07:30 Monday
	// In UTC               => 2026-08-23 23:30 Sunday
	atUTC := time.Date(2026, 8, 23, 23, 30, 0, 0, time.UTC)
	locShanghai, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)

	fs := newFakeStore()
	_, err = fs.UpsertPriceEntriesFromLiteLLM(context.Background(), []*domain.PriceEntry{
		{Model: "tz-model", Mode: domain.PriceModeToken, InputPerM: int64Ptr(100000), OutputPerM: int64Ptr(200000), Source: domain.PricingSourceManual},
	})
	require.NoError(t, err)

	equal := 5000 // mult 0.5 for matching variant
	other := 10000
	// Variant seq 1: matches only when time 07:00-08:00 AND Monday
	dowMonday := 1 << 1 // Monday = weekday 1 (Sun=0)
	_, err = fs.ReplacePriceVariants(context.Background(), "tz-model", []*domain.PriceVariant{
		{Model: "tz-model", Seq: 1, TimeStart: strPtrTZ("07:00"), TimeEnd: strPtrTZ("08:00"), DowMask: &dowMonday, MultBP: &equal},
		{Model: "tz-model", Seq: 2, MultBP: &other}, // fallback catch-all
	})
	require.NoError(t, err)

	svc := newPricingSvc(t, fs)

	// tzLoc=nil => at stays UTC => 23:30 outside window + Sunday not Monday => fallback seq2
	rp, ok := svc.ResolvePrices("tz-model", 0, "", atUTC)
	require.True(t, ok)
	require.NotNil(t, rp.VariantSeq)
	require.Equal(t, 2, *rp.VariantSeq, "nil tzLoc should not match Monday 07:30 window")

	// tzLoc=Asia/Shanghai => 07:30 Monday inside window => seq1
	svc.SetTimeLocation(locShanghai)
	rp2, ok := svc.ResolvePrices("tz-model", 0, "", atUTC)
	require.True(t, ok)
	require.NotNil(t, rp2.VariantSeq)
	require.Equal(t, 1, *rp2.VariantSeq, "Asia/Shanghai should match Monday 07:30 window")

	// also verify UTC explicit does NOT match (same as nil but via location)
	svc.SetTimeLocation(time.UTC)
	rp3, ok := svc.ResolvePrices("tz-model", 0, "", atUTC)
	require.True(t, ok)
	require.NotNil(t, rp3.VariantSeq)
	require.Equal(t, 2, *rp3.VariantSeq, "UTC should not match Monday window")

	// reset to nil again = process-local fallback (same as initial)
	svc.SetTimeLocation(nil)
	rp4, ok := svc.ResolvePrices("tz-model", 0, "", atUTC)
	require.True(t, ok)
	require.NotNil(t, rp4.VariantSeq)
	require.Equal(t, 2, *rp4.VariantSeq)
}

func TestResolvePrices_TimeZone_TimeOnly(t *testing.T) {
	// Same instant but test time-only window (no dow)
	atUTC := time.Date(2026, 8, 23, 23, 30, 0, 0, time.UTC)
	locShanghai, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	fs := newFakeStore()
	_, err = fs.UpsertPriceEntriesFromLiteLLM(context.Background(), []*domain.PriceEntry{
		{Model: "tz-time-model", Mode: domain.PriceModeToken, InputPerM: int64Ptr(100000), OutputPerM: int64Ptr(200000), Source: domain.PricingSourceManual},
	})
	require.NoError(t, err)
	m1 := 20000
	m2 := 10000
	_, err = fs.ReplacePriceVariants(context.Background(), "tz-time-model", []*domain.PriceVariant{
		{Model: "tz-time-model", Seq: 1, TimeStart: strPtrTZ("07:00"), TimeEnd: strPtrTZ("08:00"), MultBP: &m1},
		{Model: "tz-time-model", Seq: 2, MultBP: &m2},
	})
	require.NoError(t, err)
	svc := newPricingSvc(t, fs)
	// nil => UTC 23:30 not in window
	rp, _ := svc.ResolvePrices("tz-time-model", 0, "", atUTC)
	require.Equal(t, 2, *rp.VariantSeq)
	// Shanghai => 07:30 in window
	svc.SetTimeLocation(locShanghai)
	rp2, _ := svc.ResolvePrices("tz-time-model", 0, "", atUTC)
	require.Equal(t, 1, *rp2.VariantSeq)
}
