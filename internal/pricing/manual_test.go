// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package pricing

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	serviceerr "github.com/is7qin/c3api/internal/service/errors"
)

// fakeSnapshot 可编程 SnapshotReader：nil map = 快照未加载。
type fakeSnapshot struct {
	models map[string]struct{}
}

func (s *fakeSnapshot) PriceModels() map[string]struct{} { return s.models }

func intPtr(v int) *int { return &v }

func int64Ptr(v int64) *int64 { return &v }

// TestSyncNowGuardsManualVariants 手工定价优先（自 service 侧迁移的核心回归）：
// fetcher 变体中同名手工模型行被过滤，liteLLM 模型行落库；统计口径 Rows=拉取
// 行数、Variants=拉取变体数（含被过滤）；成功后 reload。
func TestSyncNowGuardsManualVariants(t *testing.T) {
	f := &fakeFetcher{result: &FetchResult{
		PriceEntries: []*domain.PriceEntry{{Model: "guard-model", Mode: domain.PriceModeToken, Source: domain.PricingSourceLitellm}},
		Variants: []*domain.PriceVariant{
			{Model: "guard-model", Seq: 1, MultBP: intPtr(20000)},
			{Model: "litellm-model", Seq: 1, MultBP: intPtr(30000)},
		},
		Skipped: 2,
	}}
	u := &fakeUpserter{n: 1, nVar: 1, manual: []string{"guard-model"}}
	reloads := 0
	w := NewSyncWorker(SyncWorkerConfig{
		Fetcher: f, Repo: u, Settings: &fakeSettings{url: "https://u"},
		Reload: func() { reloads++ }, Log: nil,
	})

	stats, err := w.SyncNow(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, stats.Rows)
	require.Equal(t, 2, stats.Skipped)
	require.Equal(t, 1, stats.Updated)
	require.Equal(t, 2, stats.Variants, "Variants 按拉取数计（含被守卫过滤）")
	require.Equal(t, 1, reloads, "成功必须 reload（快照/publish/notify 链）")

	require.Equal(t, 1, u.variantCount())
	var models []string
	for _, v := range u.varRows {
		models = append(models, v.Model)
	}
	require.Equal(t, []string{"litellm-model"}, models, "手工模型变体必须被过滤")
}

// TestSyncNowErrorContracts 错误契约矩阵：nil fetcher / 空 URL（InvalidInput →
// 端点 400）/ fetch 失败（ErrPriceFetch → 端点 502）/ 条目落库失败仍 reload。
func TestSyncNowErrorContracts(t *testing.T) {
	t.Run("nil fetcher", func(t *testing.T) {
		w := NewSyncWorker(SyncWorkerConfig{Repo: &fakeUpserter{}, Settings: &fakeSettings{url: "https://u"}})
		_, err := w.SyncNow(context.Background())
		require.Error(t, err)
		require.Contains(t, err.Error(), "fetcher not injected")
	})

	t.Run("empty url", func(t *testing.T) {
		w := NewSyncWorker(SyncWorkerConfig{Fetcher: &fakeFetcher{}, Repo: &fakeUpserter{}, Settings: &fakeSettings{url: ""}})
		_, err := w.SyncNow(context.Background())
		require.ErrorIs(t, err, serviceerr.ErrInvalidInput)
		require.Contains(t, err.Error(), "price_source_url not set")
	})

	t.Run("fetch failure", func(t *testing.T) {
		u := &fakeUpserter{}
		reloads := 0
		w := NewSyncWorker(SyncWorkerConfig{
			Fetcher: &fakeFetcher{err: errors.New("network down")}, Repo: u,
			Settings: &fakeSettings{url: "https://u"}, Reload: func() { reloads++ },
		})
		_, err := w.SyncNow(context.Background())
		require.ErrorIs(t, err, ErrPriceFetch)
		require.Zero(t, u.count(), "fetch 失败不落库")
		require.Zero(t, reloads, "fetch 失败不刷新快照")
	})

	t.Run("entries failure still reloads", func(t *testing.T) {
		u := &fakeUpserter{err: errors.New("db down")}
		reloads := 0
		w := NewSyncWorker(SyncWorkerConfig{
			Fetcher: &fakeFetcher{result: &FetchResult{PriceEntries: []*domain.PriceEntry{{Model: "m"}}}}, Repo: u,
			Settings: &fakeSettings{url: "https://u"}, Reload: func() { reloads++ },
		})
		_, err := w.SyncNow(context.Background())
		require.Error(t, err)
		require.Equal(t, 1, reloads, "已落库批立即生效 → 仍刷新快照")
	})
}

// TestPreview 预览矩阵：未加载快照全量 ToAdd；已加载按 membership 分 add/update；
// 全程零写库；错误契约与 SyncNow 同形。
func TestPreview(t *testing.T) {
	newEntry := func(model string) *domain.PriceEntry {
		return &domain.PriceEntry{Model: model, Mode: domain.PriceModeToken, Source: domain.PricingSourceLitellm}
	}

	t.Run("unloaded snapshot all add no writes", func(t *testing.T) {
		f := &fakeFetcher{result: &FetchResult{
			PriceEntries: []*domain.PriceEntry{newEntry("a"), newEntry("b")},
			Variants:     []*domain.PriceVariant{{Model: "a", Seq: 1}},
			Skipped:      1,
		}}
		u := &fakeUpserter{}
		w := NewSyncWorker(SyncWorkerConfig{Fetcher: f, Repo: u, Settings: &fakeSettings{url: "https://u"}})
		p, err := w.Preview(context.Background())
		require.NoError(t, err)
		require.Equal(t, 2, p.ToAdd)
		require.Equal(t, 0, p.ToUpdate)
		require.Equal(t, 1, p.Skipped)
		require.Equal(t, 1, p.VariantsChanged)
		require.Len(t, p.Entries, 2)
		require.Zero(t, u.count(), "预览零写库")
		require.Zero(t, u.variantCount())
	})

	t.Run("loaded snapshot splits add update", func(t *testing.T) {
		f := &fakeFetcher{result: &FetchResult{
			PriceEntries: []*domain.PriceEntry{newEntry("old"), newEntry("new")},
		}}
		u := &fakeUpserter{}
		w := NewSyncWorker(SyncWorkerConfig{
			Fetcher: f, Repo: u, Settings: &fakeSettings{url: "https://u"},
			Snapshot: &fakeSnapshot{models: map[string]struct{}{"old": {}}},
		})
		p, err := w.Preview(context.Background())
		require.NoError(t, err)
		require.Equal(t, 1, p.ToAdd)
		require.Equal(t, 1, p.ToUpdate)
		require.Equal(t, "update", p.Entries[0].Action)
		require.Equal(t, "add", p.Entries[1].Action)
		require.Zero(t, u.count(), "预览零写库")
	})

	t.Run("error contracts", func(t *testing.T) {
		w := NewSyncWorker(SyncWorkerConfig{Repo: &fakeUpserter{}, Settings: &fakeSettings{url: "https://u"}})
		_, err := w.Preview(context.Background())
		require.Error(t, err)
		require.Contains(t, err.Error(), "fetcher not injected")

		w = NewSyncWorker(SyncWorkerConfig{Fetcher: &fakeFetcher{}, Repo: &fakeUpserter{}, Settings: &fakeSettings{url: ""}})
		_, err = w.Preview(context.Background())
		require.ErrorIs(t, err, serviceerr.ErrInvalidInput)

		w = NewSyncWorker(SyncWorkerConfig{Fetcher: &fakeFetcher{err: errors.New("boom")}, Repo: &fakeUpserter{}, Settings: &fakeSettings{url: "https://u"}})
		_, err = w.Preview(context.Background())
		require.ErrorIs(t, err, ErrPriceFetch)
	})
}
