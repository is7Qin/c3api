// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/pricing"
	"github.com/is7qin/c3api/internal/repository"
)

// newPricingSvc 构造 Service + 显式首刷价格快照（快照注册表单一入口语义——
// New 不再自载 pricing，首刷由注册表 ReloadAll 承担；测试等价调用
// ReloadPricingCtx 断言成功）。
func newPricingSvc(t *testing.T, fs *fakeStore) *Service {
	t.Helper()
	svc := New(fs, nil, NopInvalidator{}, nil, nil, nil, nil)
	require.NoError(t, svc.ReloadPricingCtx(context.Background()))
	return svc
}

// fakePriceFetcher 测试用拉取器（记录收到的 URL + 返回注入结果/错误）。
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

func litellmRow(model string, prompt, completion int64) *domain.Pricing {
	return &domain.Pricing{
		Model: model, PromptPricePerMillion: prompt,
		CompletionPricePerMillion: completion, Source: domain.PricingSourceLitellm,
	}
}

func int64Ptr(v int64) *int64 { return &v }

// manualReq 构造 PricingManual（可选 cache 价；矩阵字段走显式设置）。
func manualReq(model string, prompt, completion int64, cache ...*int64) *repository.PricingManual {
	m := &repository.PricingManual{
		Model: model, PromptPricePerMillion: prompt, CompletionPricePerMillion: completion,
	}
	if len(cache) >= 1 {
		m.CacheReadPricePerMillion = cache[0]
	}
	if len(cache) >= 2 {
		m.CacheCreationPricePerMillion = cache[1]
	}
	return m
}

// TestPricingSnapshotLoadNew New 初始化从 DB 全量加载：GetPrice 快照命中
// （litellm + manual 行），缺失 → ErrNotFound（计费拒绝而非按 0 计价）。
func TestPricingSnapshotLoadNew(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertFromLiteLLM(context.Background(), []*domain.Pricing{
		litellmRow("gpt-4o", 250000, 1000000),
		litellmRow("claude-3-5-sonnet", 300000, 1500000),
	})
	require.NoError(t, err)
	_, err = fs.UpsertManual(context.Background(), manualReq("gpt-4o-mini", 100, 200))
	require.NoError(t, err)

	svc := newPricingSvc(t, fs)

	p, err := svc.GetPrice("gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(250000), p.PromptPricePerMillion)
	require.Equal(t, domain.PricingSourceLitellm, p.Source)

	p, err = svc.GetPrice("gpt-4o-mini")
	require.NoError(t, err)
	require.Equal(t, int64(100), p.PromptPricePerMillion)
	require.Equal(t, domain.PricingSourceManual, p.Source)

	_, err = svc.GetPrice("no-such-model")
	require.ErrorIs(t, err, ErrNotFound)
}

// TestPricingSnapshotReload ReloadPricing 后读路径即时生效（零 DB）。
func TestPricingSnapshotReload(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertFromLiteLLM(context.Background(), []*domain.Pricing{litellmRow("m-a", 1, 2)})
	require.NoError(t, err)
	svc := newPricingSvc(t, fs)

	// 库内新增行：快照未刷新前读不到
	_, err = fs.UpsertManual(context.Background(), manualReq("m-b", 3, 4))
	require.NoError(t, err)
	_, err = svc.GetPrice("m-b")
	require.ErrorIs(t, err, ErrNotFound, "快照未刷新（DB 新增不自动可见）")

	svc.ReloadPricing()
	p, err := svc.GetPrice("m-b")
	require.NoError(t, err)
	require.Equal(t, int64(3), p.PromptPricePerMillion)
}

// TestUpsertManualPricing 管理端手动设价：校验（model 空/价格负数 → 400 语义）
// + 成功后自动重载快照（读路径即时生效，无需手动 ReloadPricing）。
func TestUpsertManualPricing(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertFromLiteLLM(context.Background(), []*domain.Pricing{litellmRow("gpt-4o", 10, 20)})
	require.NoError(t, err)
	svc := newPricingSvc(t, fs)
	ctx := context.Background()

	t.Run("validation", func(t *testing.T) {
		_, err := svc.UpsertManualPricing(ctx, manualReq("", 1, 2))
		require.ErrorIs(t, err, ErrInvalidInput, "model 空 → 400")
		_, err = svc.UpsertManualPricing(ctx, manualReq("m", -1, 2))
		require.ErrorIs(t, err, ErrInvalidInput, "负价 → 400")
		_, err = svc.UpsertManualPricing(ctx, manualReq("m", 1, -2))
		require.ErrorIs(t, err, ErrInvalidInput)
		_, err = svc.UpsertManualPricing(ctx, manualReq("m", 1, 2, int64Ptr(-1)))
		require.ErrorIs(t, err, ErrInvalidInput, "负 cache 价 → 400")
		_, err = svc.UpsertManualPricing(ctx, manualReq("m", 1, 2, nil, int64Ptr(-2)))
		require.ErrorIs(t, err, ErrInvalidInput)
		_, err = svc.UpsertManualPricing(ctx, manualReq("m", 1, 2, nil, int64Ptr(-2)))
		require.ErrorIs(t, err, ErrInvalidInput, "负矩阵价 → 400（矩阵字段全量校验）")
		m := manualReq("m", 1, 2)
		m.PriorityPromptPricePerMillion = int64Ptr(-3)
		_, err = svc.UpsertManualPricing(ctx, m)
		require.ErrorIs(t, err, ErrInvalidInput, "负 priority 价 → 400")
		m = manualReq("m", 1, 2)
		m.FastMultiplier = int64Ptr(200000)
		_, err = svc.UpsertManualPricing(ctx, m)
		require.ErrorIs(t, err, ErrInvalidInput, "fast 万分数超上限（>×10.0）→ 400")
	})

	t.Run("takeover litellm row and reload snapshot", func(t *testing.T) {
		p, err := svc.UpsertManualPricing(ctx, manualReq("gpt-4o", 999, 888))
		require.NoError(t, err)
		require.Equal(t, domain.PricingSourceManual, p.Source)
		got, err := svc.GetPrice("gpt-4o")
		require.NoError(t, err)
		require.Equal(t, int64(999), got.PromptPricePerMillion, "接管 litellm 行且快照立即生效")
		require.Equal(t, domain.PricingSourceManual, got.Source)
		require.Nil(t, got.CacheReadPricePerMillion, "不设 cache 价 → nil")
	})

	t.Run("manual with cache prices", func(t *testing.T) {
		p, err := svc.UpsertManualPricing(ctx, manualReq("m-cache", 10, 20, int64Ptr(30), int64Ptr(40)))
		require.NoError(t, err)
		require.NotNil(t, p.CacheReadPricePerMillion)
		require.Equal(t, int64(30), *p.CacheReadPricePerMillion)
		require.Equal(t, int64(40), *p.CacheCreationPricePerMillion)

		got, err := svc.GetPrice("m-cache")
		require.NoError(t, err, "快照重载后读路径含 cache 价")
		require.Equal(t, int64(30), *got.CacheReadPricePerMillion)
		require.Equal(t, int64(40), *got.CacheCreationPricePerMillion)
	})

	t.Run("manual without cache prices", func(t *testing.T) {
		p, err := svc.UpsertManualPricing(ctx, manualReq("m-nocache", 10, 20))
		require.NoError(t, err)
		require.Nil(t, p.CacheReadPricePerMillion, "缺省 → nil")
		require.Nil(t, p.CacheCreationPricePerMillion)
	})

	t.Run("manual with matrix prices", func(t *testing.T) {
		m := manualReq("m-matrix", 100, 200)
		m.PriorityPromptPricePerMillion = int64Ptr(150)
		m.PriorityCompletionPricePerMillion = int64Ptr(300)
		m.FlexPromptPricePerMillion = int64Ptr(90)
		m.AboveThreshold = int64Ptr(272000)
		m.AbovePromptPricePerMillion = int64Ptr(80)
		m.AboveCompletionPricePerMillion = int64Ptr(160)
		m.AboveFlexPromptPricePerMillion = int64Ptr(70)
		m.FastMultiplier = int64Ptr(20000)
		p, err := svc.UpsertManualPricing(ctx, m)
		require.NoError(t, err)
		require.Equal(t, int64(150), *p.PriorityPromptPricePerMillion)
		require.Equal(t, int64(90), *p.FlexPromptPricePerMillion)
		require.Equal(t, int64(272000), *p.AboveThreshold)
		require.Equal(t, int64(80), *p.AbovePromptPricePerMillion)
		require.Equal(t, int64(70), *p.AboveFlexPromptPricePerMillion)
		require.Equal(t, int64(20000), *p.FastMultiplier)
		require.Nil(t, p.AbovePriorityPromptPricePerMillion, "未设置组 → nil")

		got, err := svc.GetPrice("m-matrix")
		require.NoError(t, err, "快照重载后矩阵价即时生效")
		require.Equal(t, int64(150), *got.PriorityPromptPricePerMillion)
		require.Equal(t, int64(20000), *got.FastMultiplier)
	})
}

// TestDeleteManualPricing 管理端删手动价：成功后快照同步（该 model 从快照消失）；
// 失败（litellm 行 → ErrConflict / 缺失 → ErrNotFound）不刷新快照（原行保留）。
func TestDeleteManualPricing(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertFromLiteLLM(context.Background(), []*domain.Pricing{litellmRow("m-litellm", 1, 2)})
	require.NoError(t, err)
	_, err = fs.UpsertManual(context.Background(), manualReq("m-manual", 3, 4))
	require.NoError(t, err)
	svc := newPricingSvc(t, fs)
	ctx := context.Background()

	// litellm 行 → ErrConflict（只允许删手动价），快照保留
	err = svc.DeleteManualPricing(ctx, "m-litellm")
	require.ErrorIs(t, err, ErrConflict)
	_, err = svc.GetPrice("m-litellm")
	require.NoError(t, err, "删除失败快照保留")

	// 缺失 → ErrNotFound
	err = svc.DeleteManualPricing(ctx, "m-absent")
	require.ErrorIs(t, err, ErrNotFound)

	// 手动行删除成功 → 快照同步移除
	require.NoError(t, svc.DeleteManualPricing(ctx, "m-manual"))
	_, err = svc.GetPrice("m-manual")
	require.ErrorIs(t, err, ErrNotFound, "删手动价后快照消失（缺失窗口 GetPrice → ErrNotFound）")
}

// TestBuildPricingSnapshotManualPriority 快照构建的 manual > litellm 优先级：
// 同一 model 多行（防御；DB 唯一约束下不应出现）按 source 收敛，manual 恒胜。
func TestBuildPricingSnapshotManualPriority(t *testing.T) {
	t.Run("litellm first, manual later", func(t *testing.T) {
		m := buildPricingSnapshot([]*domain.Pricing{
			litellmRow("m", 1, 2),
			{Model: "m", PromptPricePerMillion: 9, CompletionPricePerMillion: 9, Source: domain.PricingSourceManual},
		})
		require.Equal(t, domain.PricingSourceManual, m["m"].Source, "manual 覆盖 litellm")
		require.Equal(t, int64(9), m["m"].PromptPricePerMillion)
	})

	t.Run("manual first, litellm later", func(t *testing.T) {
		m := buildPricingSnapshot([]*domain.Pricing{
			{Model: "m", PromptPricePerMillion: 9, CompletionPricePerMillion: 9, Source: domain.PricingSourceManual},
			litellmRow("m", 1, 2),
		})
		require.Equal(t, int64(9), m["m"].PromptPricePerMillion, "manual 行不被 litellm 覆盖")
		require.Equal(t, domain.PricingSourceManual, m["m"].Source)
	})

	t.Run("distinct models kept", func(t *testing.T) {
		m := buildPricingSnapshot([]*domain.Pricing{
			litellmRow("a", 1, 2), litellmRow("b", 3, 4),
		})
		require.Len(t, m, 2)
	})
}

// TestPricingSnapshotPaging 快照全量加载分页（> 1000 行跨多页取全）。
func TestPricingSnapshotPaging(t *testing.T) {
	fs := newFakeStore()
	rows := make([]*domain.Pricing, 0, 2500)
	for i := 0; i < 2500; i++ {
		rows = append(rows, litellmRow("pg-model-"+strconv.Itoa(i), int64(i), int64(i)))
	}
	_, err := fs.UpsertFromLiteLLM(context.Background(), rows)
	require.NoError(t, err)
	svc := newPricingSvc(t, fs)
	for _, m := range []string{"pg-model-0", "pg-model-1000", "pg-model-2499"} {
		got, err := svc.GetPrice(m)
		require.NoError(t, err, "分页跨页行可达: %s", m)
		require.Equal(t, domain.PricingSourceLitellm, got.Source)
	}
}

// TestPricingSnapshotFailSafe ListPricing 失败 → 错误上报（ReloadPricingCtx
// 返回）+ 空快照（不阻断启动），GetPrice → ErrNotFound（而非 panic）。
func TestPricingSnapshotFailSafe(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertFromLiteLLM(context.Background(), []*domain.Pricing{litellmRow("m", 1, 2)})
	require.NoError(t, err)
	fs.pricingListErr = errors.New("db down")
	svc := New(fs, nil, NopInvalidator{}, nil, nil, nil, nil)
	require.Error(t, svc.ReloadPricingCtx(context.Background()), "加载失败错误上报（注册表 Status 可观测）")
	_, err = svc.GetPrice("m")
	require.ErrorIs(t, err, ErrNotFound, "加载失败 → 空快照，读路径 ErrNotFound 而非 panic")
}

// TestListPricing 管理端价格列表：source/model 筛选 + sort 白名单校验（非法
// source/sort → ErrInvalidInput 400 语义）。
func TestListPricing(t *testing.T) {
	fs := newFakeStore()
	_, err := fs.UpsertFromLiteLLM(context.Background(), []*domain.Pricing{
		litellmRow("gpt-4o", 250000, 1000000),
		litellmRow("claude-3-5-sonnet", 300000, 1500000),
	})
	require.NoError(t, err)
	_, err = fs.UpsertManual(context.Background(), manualReq("gpt-4o-mini", 100, 200))
	require.NoError(t, err)
	svc := newPricingSvc(t, fs)
	ctx := context.Background()

	t.Run("filter source=manual", func(t *testing.T) {
		src := domain.PricingSourceManual
		rows, total, err := svc.ListPricing(ctx, repository.ListQuery{}, &src, "", "")
		require.NoError(t, err)
		require.Equal(t, int64(1), total)
		require.Len(t, rows, 1)
		require.Equal(t, "gpt-4o-mini", rows[0].Model)
	})

	t.Run("filter model substring", func(t *testing.T) {
		rows, total, err := svc.ListPricing(ctx, repository.ListQuery{}, nil, "", "gpt-4o")
		require.NoError(t, err)
		require.Equal(t, int64(2), total, "gpt-4o + gpt-4o-mini")
		require.Len(t, rows, 2)
	})

	t.Run("sort model desc + pagination", func(t *testing.T) {
		rows, total, err := svc.ListPricing(ctx, repository.ListQuery{Limit: 2, Offset: 0, Sort: "model", Order: "desc"}, nil, "", "")
		require.NoError(t, err)
		require.Equal(t, int64(3), total)
		require.Len(t, rows, 2)
		require.Equal(t, "gpt-4o-mini", rows[0].Model, "desc 首行")
	})

	t.Run("invalid source", func(t *testing.T) {
		src := domain.PricingSource("bogus")
		_, _, err := svc.ListPricing(ctx, repository.ListQuery{}, &src, "", "")
		require.ErrorIs(t, err, ErrInvalidInput)
	})

	t.Run("invalid sort", func(t *testing.T) {
		_, _, err := svc.ListPricing(ctx, repository.ListQuery{Sort: "price"}, nil, "", "")
		require.ErrorIs(t, err, ErrInvalidInput)
	})

	t.Run("invalid order", func(t *testing.T) {
		_, _, err := svc.ListPricing(ctx, repository.ListQuery{Order: "sideways"}, nil, "", "")
		require.ErrorIs(t, err, ErrInvalidInput)
	})
}

// TestSyncPricingNow 手动触发同步（POST /api/admin/pricing/sync 语义）：fetch →
// upsert（manual 行级互斥）→ 快照重载；成功返回拉取统计；fetch 失败 →
// ErrPriceFetch（502 语义）、url 未配置 → ErrInvalidInput、未注入 fetcher →
// 错误。
func TestSyncPricingNow(t *testing.T) {
	ctx := context.Background()
	defURL := domain.DefaultSetting("price_source_url").Value
	require.NotEmpty(t, defURL, "settings 注册表含 price_source_url 默认值")

	litellmRows := []*domain.Pricing{
		litellmRow("gpt-4o", 250000, 1000000),
		litellmRow("claude-3-5-sonnet", 300000, 1500000),
	}

	t.Run("success: stats + upsert + snapshot reload", func(t *testing.T) {
		fs := newFakeStore()
		_, err := fs.UpsertManual(context.Background(), manualReq("gpt-4o", 999, 888))
		require.NoError(t, err) // manual 行：拉取不得覆盖
		svc := newPricingSvc(t, fs)
		f := &fakePriceFetcher{res: &pricing.FetchResult{Rows: litellmRows, Skipped: 3}}
		svc.SetPriceFetcher(f)

		stats, err := svc.SyncPricingNow(ctx)
		require.NoError(t, err)
		require.Equal(t, &PricingSyncStats{Rows: 2, Skipped: 3, Updated: 1}, stats,
			"manual 行不计入 updated")
		require.Equal(t, defURL, f.url, "fetch 使用 price_source_url 快照值")

		got, err := svc.GetPrice("claude-3-5-sonnet")
		require.NoError(t, err, "upsert 后快照重载生效")
		require.Equal(t, domain.PricingSourceLitellm, got.Source)

		got, err = svc.GetPrice("gpt-4o")
		require.NoError(t, err)
		require.Equal(t, int64(999), got.PromptPricePerMillion, "manual 行不被拉取覆盖")
		require.Equal(t, domain.PricingSourceManual, got.Source)
	})

	t.Run("fetch failure -> ErrPriceFetch", func(t *testing.T) {
		fs := newFakeStore()
		svc := newPricingSvc(t, fs)
		svc.SetPriceFetcher(&fakePriceFetcher{err: errors.New("upstream 500")})
		_, err := svc.SyncPricingNow(ctx)
		require.ErrorIs(t, err, ErrPriceFetch, "拉取失败 → 502 语义")
	})

	t.Run("fetch failure chain preserved (%w:%w)", func(t *testing.T) {
		fs := newFakeStore()
		svc := newPricingSvc(t, fs)
		// 模拟 fetch.go 错误拼装形态（"pricing: fetch %s: ..."，含 sourceURL）。
		underlying := errors.New("connection refused")
		fetchErr := fmt.Errorf("pricing: fetch %s: %w", "https://example.com/price.json", underlying)
		svc.SetPriceFetcher(&fakePriceFetcher{err: fetchErr})
		_, err := svc.SyncPricingNow(ctx)
		require.ErrorIs(t, err, ErrPriceFetch, "拉取失败 → 502 语义")
		require.ErrorIs(t, err, underlying,
			"%w:%w 多重包装保链——errors.Is 穿透命中 fetch 层错误（G3-1：%v 会断链恒 miss）")
	})

	t.Run("url not set -> ErrInvalidInput", func(t *testing.T) {
		fs := newFakeStore()
		svc := newPricingSvc(t, fs)
		svc.SetPriceFetcher(&fakePriceFetcher{res: &pricing.FetchResult{Rows: litellmRows}})
		_, err := svc.UpdateSetting(ctx, "price_source_url", "")
		require.NoError(t, err)
		_, err = svc.SyncPricingNow(ctx)
		require.ErrorIs(t, err, ErrInvalidInput, "url 未配置 → 400 语义")
	})

	t.Run("fetcher not injected", func(t *testing.T) {
		fs := newFakeStore()
		svc := newPricingSvc(t, fs)
		_, err := svc.SyncPricingNow(ctx)
		require.Error(t, err, "装配缺失必须显式报错而非静默跳过")
	})

	t.Run("upsert failure -> raw error", func(t *testing.T) {
		fs := newFakeStore()
		svc := newPricingSvc(t, fs)
		fs.pricingUpsertErr = errors.New("db down")
		svc.SetPriceFetcher(&fakePriceFetcher{res: &pricing.FetchResult{Rows: litellmRows}})
		_, err := svc.SyncPricingNow(ctx)
		require.ErrorIs(t, err, fs.pricingUpsertErr, "upsert 失败原样返回（500 语义）")
	})
}
