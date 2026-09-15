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
	"github.com/is7qin/c3api/internal/pricing"
	"github.com/is7qin/c3api/internal/repository"
)

func newPricingSvc(t *testing.T, fs *fakeStore) *Service {
	t.Helper()
	svc := New(fs, nil, NopInvalidator{}, nil, nil, nil, nil, ServiceDeps{EmailCodeStore: testEmailCodes})
	require.NoError(t, svc.ReloadPricingCtx(context.Background()))
	return svc
}

type fakePriceFetcher struct {
	res *pricing.FetchResult
	err error
	url string
}

func (f *fakePriceFetcher) Fetch(ctx context.Context, sourceURL string) (*pricing.FetchResult, error) {
	f.url = sourceURL
	if f.err != nil {
		return nil, f.err
	}
	return f.res, nil
}

func int64Ptr(v int64) *int64 { return &v }

func TestPriceEntrySnapshotLoad(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertPriceEntriesFromLiteLLM(context.Background(), []*domain.PriceEntry{
		{Model: "gpt-4o", Mode: domain.PriceModeToken, InputPerM: int64Ptr(250000), OutputPerM: int64Ptr(1000000), Source: domain.PricingSourceLitellm},
		{Model: "img-m", Mode: domain.PriceModeImage, PricePerImage: int64Ptr(5400), Source: domain.PricingSourceLitellm},
	})
	require.NoError(t, err)
	svc := newPricingSvc(t, fs)
	pe, err := svc.GetPriceEntry(context.Background(), "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(250000), *pe.InputPerM)
	_, err = svc.GetPriceEntry(context.Background(), "missing")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPriceEntryManualValidation(t *testing.T) {
	fs := newFakeStore()
	svc := newPricingSvc(t, fs)
	_, err := svc.UpsertPriceEntry(context.Background(), &repository.PriceEntryManual{Model: "", Mode: domain.PriceModeToken})
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.UpsertPriceEntry(context.Background(), &repository.PriceEntryManual{Model: "m", Mode: domain.PriceModeToken, InputPerM: int64Ptr(100)})
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.UpsertPriceEntry(context.Background(), &repository.PriceEntryManual{Model: "m", Mode: domain.PriceModeToken, InputPerM: int64Ptr(100), OutputPerM: int64Ptr(200)})
	require.NoError(t, err)
}

func TestResolvePricesWithVariant(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertPriceEntriesFromLiteLLM(context.Background(), []*domain.PriceEntry{
		{Model: "m", Mode: domain.PriceModeToken, InputPerM: int64Ptr(100000), OutputPerM: int64Ptr(200000), Source: domain.PricingSourceManual},
	})
	require.NoError(t, err)
	_, err = fs.ReplacePriceVariants(context.Background(), "m", []*domain.PriceVariant{
		{Model: "m", Seq: 1, ServiceTier: strPtr("priority"), SetInputPerM: int64Ptr(150000)},
	})
	require.NoError(t, err)
	svc := newPricingSvc(t, fs)
	rp, ok := svc.ResolvePrices("m", 0, "priority", time.Now())
	require.True(t, ok)
	require.Equal(t, int64(150000), *rp.InputPerM)
}

// TestPricingChangeNotifiesCompiler 是缺陷 B 价格面的回归：定价写面
// （Upsert/Delete/Variants/手动 sync）是编译道价格输入的唯一变更源——解析
// 价格真实变化必须通知编译（装配期接 scheduler.RequestCompile）；同值写
// （解析价格不变）必须静默，否则每次同值 PUT 都驱逐一次全量重编译。
func TestPricingChangeNotifiesCompiler(t *testing.T) {
	fs := newFakeStore()
	var calls int
	svc := New(fs, nil, NopInvalidator{}, nil, nil, nil, nil, ServiceDeps{EmailCodeStore: testEmailCodes, CompileNotify: func() { calls++ }})
	require.NoError(t, svc.ReloadPricingCtx(context.Background()))
	ctx := context.Background()

	_, err := svc.UpsertPriceEntry(ctx, &repository.PriceEntryManual{Model: "m", Mode: domain.PriceModeToken, InputPerM: int64Ptr(100), OutputPerM: int64Ptr(200)})
	require.NoError(t, err)
	require.Equal(t, 1, calls, "真实价格变化必须通知编译")

	_, err = svc.UpsertPriceEntry(ctx, &repository.PriceEntryManual{Model: "m", Mode: domain.PriceModeToken, InputPerM: int64Ptr(100), OutputPerM: int64Ptr(200)})
	require.NoError(t, err)
	require.Equal(t, 1, calls, "同值 PUT（解析价格不变）必须静默")

	_, err = svc.UpsertPriceEntry(ctx, &repository.PriceEntryManual{Model: "m", Mode: domain.PriceModeToken, InputPerM: int64Ptr(300), OutputPerM: int64Ptr(200)})
	require.NoError(t, err)
	require.Equal(t, 2, calls, "价格变更必须再次通知编译")

	require.NoError(t, svc.DeletePriceEntry(ctx, "m"))
	require.Equal(t, 3, calls, "价格删除必须通知编译")
}

// TestResolvedPricesByModel pins the compile-lane price source: full-snapshot
// base resolution (tier "", promptTokens 0), tier-scoped variants excluded,
// and nil before the snapshot is loaded.
func TestResolvedPricesByModel(t *testing.T) {
	fs := newFakeStore()
	svc := New(fs, nil, NopInvalidator{}, nil, nil, nil, nil, ServiceDeps{EmailCodeStore: testEmailCodes})
	require.Nil(t, svc.ResolvedPricesByModel(time.Now()), "unloaded snapshot = nil (compile lane treats as no prices)")
	_, err := fs.UpsertPriceEntriesFromLiteLLM(context.Background(), []*domain.PriceEntry{
		{Model: "m", Mode: domain.PriceModeToken, InputPerM: int64Ptr(100000), OutputPerM: int64Ptr(200000), Source: domain.PricingSourceManual},
	})
	require.NoError(t, err)
	_, err = fs.ReplacePriceVariants(context.Background(), "m", []*domain.PriceVariant{
		{Model: "m", Seq: 1, ServiceTier: strPtr("priority"), SetInputPerM: int64Ptr(150000)},
	})
	require.NoError(t, err)
	require.NoError(t, svc.ReloadPricingCtx(context.Background()))

	prices := svc.ResolvedPricesByModel(time.Now())
	require.Len(t, prices, 1)
	rp, ok := prices["m"]
	require.True(t, ok)
	require.Equal(t, int64(100000), *rp.InputPerM, "base entry price: tier-scoped variant must not apply")
	require.Equal(t, int64(200000), *rp.OutputPerM)
}

func TestReplacePriceVariants_MultBPValidation(t *testing.T) {
	fs := newFakeStore()
	svc := newPricingSvc(t, fs)
	// MaxInt rejected
	maxInt := 1 << 30
	_, err := svc.ReplacePriceVariants(context.Background(), "m", []*domain.PriceVariant{{Model: "m", Seq: 1, MultBP: &maxInt}})
	require.ErrorIs(t, err, ErrInvalidInput)
	// negative rejected
	neg := -1
	_, err = svc.ReplacePriceVariants(context.Background(), "m", []*domain.PriceVariant{{Model: "m", Seq: 1, MultBP: &neg}})
	require.ErrorIs(t, err, ErrInvalidInput)
	// boundary 100000 allowed, 100001 rejected
	v100k := 100000
	_, err = svc.ReplacePriceVariants(context.Background(), "m", []*domain.PriceVariant{{Model: "m", Seq: 1, MultBP: &v100k}})
	require.NoError(t, err)
	v100001 := 100001
	_, err = svc.ReplacePriceVariants(context.Background(), "m", []*domain.PriceVariant{{Model: "m", Seq: 1, MultBP: &v100001}})
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestReplacePriceVariants_CallSetPricePerCall(t *testing.T) {
	fs := newFakeStore()
	svc := newPricingSvc(t, fs)
	callPrice := int64(2000)
	_, err := svc.ReplacePriceVariants(context.Background(), "m-call", []*domain.PriceVariant{{Model: "m-call", Seq: 1, SetPricePerCall: &callPrice}})
	require.NoError(t, err)
	// also resolver yields that price
	_, err = fs.UpsertPriceEntriesFromLiteLLM(context.Background(), []*domain.PriceEntry{
		{Model: "m-call", Mode: domain.PriceModeCall, PricePerCall: int64Ptr(1000), Source: domain.PricingSourceManual},
	})
	require.NoError(t, err)
	require.NoError(t, svc.ReloadPricingCtx(context.Background()))
	rp, ok := svc.ResolvePrices("m-call", 0, "auto", time.Now())
	require.True(t, ok)
	require.NotNil(t, rp.PricePerCall)
	require.Equal(t, callPrice, *rp.PricePerCall)
	// empty effect should be rejected
	_, err = svc.ReplacePriceVariants(context.Background(), "m-call", []*domain.PriceVariant{{Model: "m-call", Seq: 2}})
	require.ErrorIs(t, err, ErrInvalidInput)
}

// TestPricingWritePublishesPricingChange D1 定价跨实例失效：定价写面经统一
// 出口 reloadPricingAndNotifyCompiler 发布 Change{Pricing:true}（编译通知装配
// 与否均发布——跨实例传播不依赖变化检测）；ReloadPricingCtx（启动/FullRefresh
// 路径）保持 publish-free。
func TestPricingWritePublishesPricingChange(t *testing.T) {
	ctx := context.Background()
	manual := &repository.PriceEntryManual{Model: "m", Mode: domain.PriceModeToken, InputPerM: int64Ptr(100), OutputPerM: int64Ptr(200)}

	t.Run("降级路径（compileNotify nil）仍发布", func(t *testing.T) {
		svc, _, pr := newPubSvc()
		_, err := svc.UpsertPriceEntry(ctx, manual)
		require.NoError(t, err)
		got := pr.last()
		require.NotNil(t, got)
		require.True(t, got.Pricing, "定价写面 → Pricing:true")
		require.Equal(t, 1, pr.total(), "一次写面一条 NOTIFY")
	})

	t.Run("装配路径（compileNotify 非 nil）仍发布", func(t *testing.T) {
		svc, _, pr := newPubSvc()
		svc.compileNotify = func() {}
		_, err := svc.UpsertPriceEntry(ctx, manual)
		require.NoError(t, err)
		got := pr.last()
		require.NotNil(t, got)
		require.True(t, got.Pricing, "装配路径同样发布（不依赖变化检测）")
	})

	t.Run("DeletePriceEntry 发布", func(t *testing.T) {
		svc, _, pr := newPubSvc()
		_, err := svc.UpsertPriceEntry(ctx, manual)
		require.NoError(t, err)
		require.NoError(t, svc.DeletePriceEntry(ctx, "m"))
		got := pr.last()
		require.NotNil(t, got)
		require.True(t, got.Pricing, "删除写面 → Pricing:true")
	})

	t.Run("ReloadPricingCtx 不发布", func(t *testing.T) {
		svc, _, pr := newPubSvc()
		require.NoError(t, svc.ReloadPricingCtx(ctx))
		require.Equal(t, 0, pr.total(), "启动/FullRefresh 路径保持 publish-free")
	})
}
