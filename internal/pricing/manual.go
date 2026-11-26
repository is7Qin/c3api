// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package pricing

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/pricingresult"
	"github.com/is7qin/c3api/internal/service/errors"
	"github.com/is7qin/c3api/pkg/logx"
)

// ErrPriceFetch 拉取失败（fetch err 包装）：管理端手动 sync 端点映射 502。
//
// ⚠ 仅供本包测试与既有调用方；跨包判定请用 pricingresult.FetchError + errors.As
// （不要认变量名）。
var ErrPriceFetch = errors.New("pricing: price fetch failed")

// SyncStats / Preview / PreviewEntry 的 canonical 定义在 internal/pricingresult
// （结果值的中立叶子包）。别名而非新类型：SyncWorker 的既有签名与全部调用点不变。
type (
	SyncStats    = pricingresult.SyncStats
	Preview      = pricingresult.Preview
	PreviewEntry = pricingresult.PreviewEntry
)

// SnapshotReader 预览 membership 源（service 定价快照实现）：nil map =
// 快照未加载（全量 ToAdd，与旧 service.priceSnapshot==nil 分支同语义）。
type SnapshotReader interface {
	PriceModels() map[string]struct{}
}

// SyncNow 执行一次手动同步（fetch → 文本价 upsert → image/变体价 upsert →
// reload）：管理端 POST /pricing/sync 与 cron Sync 同源不同形——本路径多
// manual 变体守卫（手工定价优先：过滤同名 liteLLM 变体）+ 返回统计。
// 错误契约：fetcher 未装配 → "pricing: fetcher not injected"；source_url 空 →
// serviceerr.ErrInvalidInput 包装（端点 400）；fetch 失败 → ErrPriceFetch 包装
// （端点 502）。条目落库失败仍 reload（已落库批立即生效），变体失败同样。
func (w *SyncWorker) SyncNow(ctx context.Context) (*SyncStats, error) {
	if w.fetch == nil {
		return nil, errors.New("pricing: fetcher not injected")
	}
	url := w.settings.PriceSourceURL()
	if url == "" {
		return nil, fmt.Errorf("%w: price_source_url not set, skip sync", serviceerr.ErrInvalidInput)
	}
	res, err := w.fetch.Fetch(ctx, url)
	if err != nil {
		w.log.Warn("pricing sync failed", logx.Error(err))
		// 一处 fetch 失败 = 三条语义同时成立：
		//   - &pricingresult.FetchError  → handler 用 errors.As 判定（**专属、精确**，不认变量名）
		//   - ErrPriceFetch              → 本包既有测试的 ErrorIs 契约
		//   - serviceerr.ErrUpstream     → 任何经 httpface.WriteServiceErr 的路径拿到通用 502
		// 三者用嵌套 %w 一次表达（Go 1.27 支持多个 %w）。⚠ handler 的 502 分支只认
		// 第一条——外层那条 ErrUpstream 不是它的承重件。
		return nil, fmt.Errorf("%w: %w", &pricingresult.FetchError{
			Err: fmt.Errorf("%w: %w", ErrPriceFetch, err),
		}, serviceerr.ErrUpstream)
	}
	entries := res.PriceEntries
	n, err := w.repo.UpsertPriceEntriesFromLiteLLM(ctx, entries)
	if err != nil {
		if w.reload != nil {
			w.reload()
		}
		return nil, err
	}
	if len(res.Variants) > 0 {
		filtered := res.Variants
		if manualModels, merr := w.repo.ManualEntryModels(ctx); merr == nil && len(manualModels) > 0 {
			manualSet := make(map[string]struct{}, len(manualModels))
			for _, m := range manualModels {
				manualSet[m] = struct{}{}
			}
			// 手工定价优先：过滤掉 liteLLM 的同名变体（in-place，与原 service
			// retain 循环同语义——DeleteFunc 额外把尾部清零）。
			filtered = slices.DeleteFunc(filtered, func(v *domain.PriceVariant) bool {
				_, isManual := manualSet[v.Model]
				return isManual
			})
		}
		if len(filtered) > 0 {
			if verr := func() error {
				_, e := w.repo.UpsertPriceVariantsFromLiteLLM(ctx, filtered)
				return e
			}(); verr != nil {
				err = verr
			}
		}
	}
	if w.reload != nil {
		w.reload()
	}
	if err != nil {
		return nil, err
	}
	return &SyncStats{Rows: len(entries), Skipped: res.Skipped, Updated: n, Variants: len(res.Variants)}, nil
}

// Preview 预览一次同步（fetch + 快照 membership 比较）：零写库。
// 错误契约与 SyncNow 同形（端点统一走 WriteServiceErr）。
func (w *SyncWorker) Preview(ctx context.Context) (*Preview, error) {
	if w.fetch == nil {
		return nil, errors.New("pricing: fetcher not injected")
	}
	url := w.settings.PriceSourceURL()
	if url == "" {
		return nil, fmt.Errorf("%w: price_source_url not set, skip sync", serviceerr.ErrInvalidInput)
	}
	res, err := w.fetch.Fetch(ctx, url)
	if err != nil {
		w.log.Warn("pricing sync preview failed", logx.Error(err))
		// 一处 fetch 失败 = 三条语义同时成立：
		//   - &pricingresult.FetchError  → handler 用 errors.As 判定（**专属、精确**，不认变量名）
		//   - ErrPriceFetch              → 本包既有测试的 ErrorIs 契约
		//   - serviceerr.ErrUpstream     → 任何经 httpface.WriteServiceErr 的路径拿到通用 502
		// 三者用嵌套 %w 一次表达（Go 1.27 支持多个 %w）。⚠ handler 的 502 分支只认
		// 第一条——外层那条 ErrUpstream 不是它的承重件。
		return nil, fmt.Errorf("%w: %w", &pricingresult.FetchError{
			Err: fmt.Errorf("%w: %w", ErrPriceFetch, err),
		}, serviceerr.ErrUpstream)
	}
	entries := res.PriceEntries
	preview := &Preview{Skipped: res.Skipped}
	var models map[string]struct{}
	if w.snapshot != nil {
		models = w.snapshot.PriceModels()
	}
	if models == nil {
		preview.ToAdd = len(entries)
		for _, e := range entries {
			preview.Entries = append(preview.Entries, PreviewEntry{Model: e.Model, Mode: string(e.Mode), Action: "add"})
		}
		preview.VariantsChanged = len(res.Variants)
		return preview, nil
	}
	for _, e := range entries {
		if _, ok := models[e.Model]; ok {
			preview.ToUpdate++
			preview.Entries = append(preview.Entries, PreviewEntry{Model: e.Model, Mode: string(e.Mode), Action: "update"})
		} else {
			preview.ToAdd++
			preview.Entries = append(preview.Entries, PreviewEntry{Model: e.Model, Mode: string(e.Mode), Action: "add"})
		}
	}
	preview.VariantsChanged = len(res.Variants)
	return preview, nil
}
