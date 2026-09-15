// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/pricing"
)

// hPriceFetcher 可编程 fetcher。
type hPriceFetcher struct {
	res *pricing.FetchResult
	err error
}

func (f *hPriceFetcher) Fetch(ctx context.Context, url string) (*pricing.FetchResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.res, nil
}

// hPriceUpserter 记录写库的 Upserter（断言预览零写库）。
type hPriceUpserter struct {
	entryCalls   int
	variantCalls int
	manual       []string
}

func (u *hPriceUpserter) UpsertPriceEntriesFromLiteLLM(ctx context.Context, rows []*domain.PriceEntry) (int, error) {
	u.entryCalls++
	return len(rows), nil
}

func (u *hPriceUpserter) UpsertPriceVariantsFromLiteLLM(ctx context.Context, rows []*domain.PriceVariant) (int, error) {
	u.variantCalls++
	return len(rows), nil
}

func (u *hPriceUpserter) ManualEntryModels(ctx context.Context) ([]string, error) {
	return u.manual, nil
}

// hPriceSettings 固定 URL 的 SettingReader。
type hPriceSettings struct{ url string }

func (s *hPriceSettings) PriceSourceURL() string { return s.url }
func (s *hPriceSettings) PriceSyncCron() string  { return "" }

// hPriceSnapshot 可编程 SnapshotReader。
type hPriceSnapshot struct{ models map[string]struct{} }

func (s *hPriceSnapshot) PriceModels() map[string]struct{} { return s.models }

func newPricingTestHandler(f *hPriceFetcher, u *hPriceUpserter, url string) (*AdminAPI, *pricing.SyncWorker) {
	w := pricing.NewSyncWorker(pricing.SyncWorkerConfig{
		Fetcher: f, Repo: u, Settings: &hPriceSettings{url: url},
		Reload: func() {}, Log: nil,
	})
	return New(nil, OpsOptions{PricingSync: w}), w
}

func postPricing(t *testing.T, h *AdminAPI, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/admin"+path, nil)
	rec := httptest.NewRecorder()
	h.Router().ServeHTTP(rec, req)
	return rec
}

// TestPostPricingSync 同步端点矩阵：成功 200 口径；fetch 失败 502；空 URL 400；
// 未装配 500（旧 nil-fetcher 降级语义）。
func TestPostPricingSync(t *testing.T) {
	newEntry := func(model string) *domain.PriceEntry {
		return &domain.PriceEntry{Model: model, Mode: domain.PriceModeToken, Source: domain.PricingSourceLitellm}
	}

	t.Run("success", func(t *testing.T) {
		h, _ := newPricingTestHandler(
			&hPriceFetcher{res: &pricing.FetchResult{PriceEntries: []*domain.PriceEntry{newEntry("m")}, Skipped: 1}},
			&hPriceUpserter{}, "https://u")
		rec := postPricing(t, h, "/pricing/sync")
		require.Equal(t, http.StatusOK, rec.Code)
		var out PricingSyncResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		require.Equal(t, 1, out.Rows)
		require.Equal(t, 1, out.Skipped)
		require.Equal(t, 1, out.Updated)
	})

	t.Run("fetch failure 502", func(t *testing.T) {
		h, _ := newPricingTestHandler(&hPriceFetcher{err: errors.New("down")}, &hPriceUpserter{}, "https://u")
		rec := postPricing(t, h, "/pricing/sync")
		require.Equal(t, http.StatusBadGateway, rec.Code)
	})

	t.Run("empty url 400", func(t *testing.T) {
		h, _ := newPricingTestHandler(&hPriceFetcher{}, &hPriceUpserter{}, "")
		rec := postPricing(t, h, "/pricing/sync")
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("unassembled 500", func(t *testing.T) {
		h := New(nil, OpsOptions{})
		rec := postPricing(t, h, "/pricing/sync")
		require.Equal(t, http.StatusInternalServerError, rec.Code)
	})
}

// TestPostPricingSyncPreview 预览端点矩阵：成功 200 + add/update 拆分；零写库；
// 未装配 500。
func TestPostPricingSyncPreview(t *testing.T) {
	newEntry := func(model string) *domain.PriceEntry {
		return &domain.PriceEntry{Model: model, Mode: domain.PriceModeToken, Source: domain.PricingSourceLitellm}
	}

	t.Run("success splits add update no writes", func(t *testing.T) {
		u := &hPriceUpserter{}
		w := pricing.NewSyncWorker(pricing.SyncWorkerConfig{
			Fetcher: &hPriceFetcher{res: &pricing.FetchResult{
				PriceEntries: []*domain.PriceEntry{newEntry("old"), newEntry("new")},
			}},
			Repo: u, Settings: &hPriceSettings{url: "https://u"},
			Snapshot: &hPriceSnapshot{models: map[string]struct{}{"old": {}}},
			Reload:   func() {},
		})
		h := New(nil, OpsOptions{PricingSync: w})
		rec := postPricing(t, h, "/pricing/sync/preview")
		require.Equal(t, http.StatusOK, rec.Code)
		var out PricingSyncPreviewResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		require.Equal(t, 1, out.ToAdd)
		require.Equal(t, 1, out.ToUpdate)
		require.Zero(t, u.entryCalls, "预览零写库")
		require.Zero(t, u.variantCalls)
	})

	t.Run("unassembled 500", func(t *testing.T) {
		h := New(nil, OpsOptions{})
		rec := postPricing(t, h, "/pricing/sync/preview")
		require.Equal(t, http.StatusInternalServerError, rec.Code)
	})
}
