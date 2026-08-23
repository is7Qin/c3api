// SPDX-License-Identifier: AGPL-3.0-or-later
package repository

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"entgo.io/ent/dialect"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/priceentry"
)

// PriceEntryRepo 统一价格条目持久化。
type PriceEntryRepo struct {
	client *ent.Client
	driver dialect.Driver
}

func (r *PriceEntryRepo) UpsertFromLiteLLM(ctx context.Context, rows []*domain.PriceEntry) (int, error) {
	now := time.Now()
	exec := func(ctx context.Context, start, end int) (int, error) {
		return r.upsertLitellmBatch(ctx, rows[start:end], now)
	}
	return litellmUpsertBatches(ctx, len(rows), "price_entry",
		litellmUpsertOpts{BatchSize: litellmBatchSize, Retries: litellmUpsertRetries, Backoff: litellmUpsertBackoff}, exec)
}

func (r *PriceEntryRepo) upsertLitellmBatch(ctx context.Context, batch []*domain.PriceEntry, now time.Time) (int, error) {
	sort.SliceStable(batch, func(i, j int) bool { return batch[i].Model < batch[j].Model })
	insertCols := []string{
		priceentry.FieldModel, priceentry.FieldMode,
		priceentry.FieldInputPerM, priceentry.FieldOutputPerM, priceentry.FieldCacheReadPerM, priceentry.FieldCacheWritePerM,
		priceentry.FieldPricePerCall,
		priceentry.FieldImgInTokPerM, priceentry.FieldImgOutTokPerM, priceentry.FieldPricePerImage,
		priceentry.FieldProvider, priceentry.FieldMaxInputTokens, priceentry.FieldMaxOutputTokens, priceentry.FieldSupportsPromptCaching, priceentry.FieldRaw, priceentry.FieldSource, priceentry.FieldCreatedAt, priceentry.FieldUpdatedAt,
	}
	updateCols := []string{
		priceentry.FieldMode,
		priceentry.FieldInputPerM, priceentry.FieldOutputPerM, priceentry.FieldCacheReadPerM, priceentry.FieldCacheWritePerM,
		priceentry.FieldPricePerCall,
		priceentry.FieldImgInTokPerM, priceentry.FieldImgOutTokPerM, priceentry.FieldPricePerImage,
		priceentry.FieldProvider, priceentry.FieldMaxInputTokens, priceentry.FieldMaxOutputTokens, priceentry.FieldSupportsPromptCaching, priceentry.FieldRaw, priceentry.FieldUpdatedAt,
	}
	const colsPerRow = 18
	var buf strings.Builder
	buf.WriteString("INSERT INTO " + priceentry.Table + " (" + strings.Join(insertCols, ", ") + ") VALUES ")
	args := make([]any, 0, len(batch)*colsPerRow)
	for i, p := range batch {
		if i > 0 {
			buf.WriteString(", ")
		}
		n := i * colsPerRow
		buf.WriteString("(")
		for c := 1; c <= colsPerRow; c++ {
			if c > 1 {
				buf.WriteString(", ")
			}
			if c == 15 {
				fmt.Fprintf(&buf, "$%d::jsonb", n+c)
			} else {
				fmt.Fprintf(&buf, "$%d", n+c)
			}
		}
		buf.WriteString(")")
		var in, out, cr, cw, ppc, imgIn, imgOut, pimg, provider, maxIn, maxOut, spc, raw any
		if p.InputPerM != nil {
			in = *p.InputPerM
		}
		if p.OutputPerM != nil {
			out = *p.OutputPerM
		}
		if p.CacheReadPerM != nil {
			cr = *p.CacheReadPerM
		}
		if p.CacheWritePerM != nil {
			cw = *p.CacheWritePerM
		}
		if p.PricePerCall != nil {
			ppc = *p.PricePerCall
		}
		if p.ImgInTokPerM != nil {
			imgIn = *p.ImgInTokPerM
		}
		if p.ImgOutTokPerM != nil {
			imgOut = *p.ImgOutTokPerM
		}
		if p.PricePerImage != nil {
			pimg = *p.PricePerImage
		}
		if p.Provider != nil {
			provider = *p.Provider
		}
		if p.MaxInputTokens != nil {
			maxIn = *p.MaxInputTokens
		}
		if p.MaxOutputTokens != nil {
			maxOut = *p.MaxOutputTokens
		}
		if p.SupportsPromptCaching != nil {
			spc = *p.SupportsPromptCaching
		}
		if len(p.Raw) > 0 {
			raw = string(p.Raw)
		}
		args = append(args, p.Model, string(p.Mode), in, out, cr, cw, ppc, imgIn, imgOut, pimg, provider, maxIn, maxOut, spc, raw, string(domain.PricingSourceLitellm), now, now)
	}
	buf.WriteString(" ON CONFLICT (" + priceentry.FieldModel + ") DO UPDATE SET ")
	for i, col := range updateCols {
		if i > 0 {
			buf.WriteString(", ")
		}
		buf.WriteString(col + " = EXCLUDED." + col)
	}
	buf.WriteString(" WHERE " + priceentry.Table + ".source != '" + string(domain.PricingSourceManual) + "'")
	tx, err := r.driver.Tx(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var res sql.Result
	if err := tx.Exec(ctx, buf.String(), args, &res); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// PriceEntryManual 手动设价入参。
type PriceEntryManual struct {
	Model          string
	Mode           domain.PriceMode
	InputPerM      *int64
	OutputPerM     *int64
	CacheReadPerM  *int64
	CacheWritePerM *int64
	PricePerCall   *int64
	ImgInTokPerM   *int64
	ImgOutTokPerM  *int64
	PricePerImage  *int64
}

func (r *PriceEntryRepo) UpsertManual(ctx context.Context, m *PriceEntryManual) (*domain.PriceEntry, error) {
	if m.Model == "" {
		return nil, fmt.Errorf("price_entry: model is required")
	}
	// check existence for insert vs update path? Use ent upsert.
	_, err := r.client.PriceEntry.Create().
		SetModel(m.Model).SetMode(priceentry.Mode(m.Mode)).
		SetNillableInputPerM(m.InputPerM).SetNillableOutputPerM(m.OutputPerM).
		SetNillableCacheReadPerM(m.CacheReadPerM).SetNillableCacheWritePerM(m.CacheWritePerM).
		SetNillablePricePerCall(m.PricePerCall).
		SetNillableImgInTokPerM(m.ImgInTokPerM).SetNillableImgOutTokPerM(m.ImgOutTokPerM).
		SetNillablePricePerImage(m.PricePerImage).
		SetSource(priceentry.SourceManual).
		OnConflictColumns(priceentry.FieldModel).
		Update(func(u *ent.PriceEntryUpsert) {
			u.SetMode(priceentry.Mode(m.Mode)).SetSource(priceentry.SourceManual).SetUpdatedAt(time.Now())
			if m.InputPerM != nil {
				u.SetInputPerM(*m.InputPerM)
			} else {
				u.ClearInputPerM()
			}
			if m.OutputPerM != nil {
				u.SetOutputPerM(*m.OutputPerM)
			} else {
				u.ClearOutputPerM()
			}
			if m.CacheReadPerM != nil {
				u.SetCacheReadPerM(*m.CacheReadPerM)
			} else {
				u.ClearCacheReadPerM()
			}
			if m.CacheWritePerM != nil {
				u.SetCacheWritePerM(*m.CacheWritePerM)
			} else {
				u.ClearCacheWritePerM()
			}
			if m.PricePerCall != nil {
				u.SetPricePerCall(*m.PricePerCall)
			} else {
				u.ClearPricePerCall()
			}
			if m.ImgInTokPerM != nil {
				u.SetImgInTokPerM(*m.ImgInTokPerM)
			} else {
				u.ClearImgInTokPerM()
			}
			if m.ImgOutTokPerM != nil {
				u.SetImgOutTokPerM(*m.ImgOutTokPerM)
			} else {
				u.ClearImgOutTokPerM()
			}
			if m.PricePerImage != nil {
				u.SetPricePerImage(*m.PricePerImage)
			} else {
				u.ClearPricePerImage()
			}
			u.ClearProvider().ClearRaw().ClearMaxInputTokens().ClearMaxOutputTokens().ClearSupportsPromptCaching()
		}).ID(ctx)
	if err != nil {
		return nil, err
	}
	return r.GetPriceEntry(ctx, m.Model)
}

func (r *PriceEntryRepo) DeleteManual(ctx context.Context, model string) error {
	n, err := r.client.PriceEntry.Delete().Where(priceentry.ModelEQ(model), priceentry.SourceEQ(priceentry.SourceManual)).Exec(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		exists, err := r.client.PriceEntry.Query().Where(priceentry.ModelEQ(model)).Exist(ctx)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("%w: model=%q source=litellm (manual price only)", ErrConflict, model)
		}
		return fmt.Errorf("%w: model=%q", ErrNotFound, model)
	}
	return nil
}

func (r *PriceEntryRepo) ListPriceEntries(ctx context.Context, q ListQuery, source *domain.PricingSource, mode *domain.PriceMode, model string) ([]*domain.PriceEntry, int64, error) {
	pred := r.client.PriceEntry.Query()
	if source != nil {
		pred = pred.Where(priceentry.SourceEQ(priceentry.Source(*source)))
	}
	if mode != nil {
		pred = pred.Where(priceentry.ModeEQ(priceentry.Mode(*mode)))
	}
	if model != "" {
		pred = pred.Where(priceentry.ModelContainsFold(model))
	}
	total, err := pred.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	order, err := q.sortOrder(priceEntrySortFields)
	if err != nil {
		return nil, 0, err
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	rows, err := pred.Order(order).Offset(q.Offset).Limit(q.Limit).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*domain.PriceEntry, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainPriceEntry(row))
	}
	return out, int64(total), nil
}

func (r *PriceEntryRepo) ManualEntryModels(ctx context.Context) ([]string, error) {
	rows, err := r.client.PriceEntry.Query().Where(priceentry.SourceEQ(priceentry.SourceManual)).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, p := range rows {
		out = append(out, p.Model)
	}
	return out, nil
}

func (r *PriceEntryRepo) GetPriceEntry(ctx context.Context, model string) (*domain.PriceEntry, error) {
	row, err := r.client.PriceEntry.Query().Where(priceentry.ModelEQ(model)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("%w: model=%q", ErrNotFound, model)
		}
		return nil, err
	}
	return toDomainPriceEntry(row), nil
}

func toDomainPriceEntry(p *ent.PriceEntry) *domain.PriceEntry {
	return &domain.PriceEntry{
		ID:                    p.ID,
		Model:                 p.Model,
		Mode:                  domain.PriceMode(p.Mode),
		InputPerM:             p.InputPerM,
		OutputPerM:            p.OutputPerM,
		CacheReadPerM:         p.CacheReadPerM,
		CacheWritePerM:        p.CacheWritePerM,
		PricePerCall:          p.PricePerCall,
		ImgInTokPerM:          p.ImgInTokPerM,
		ImgOutTokPerM:         p.ImgOutTokPerM,
		PricePerImage:         p.PricePerImage,
		Provider:              p.Provider,
		MaxInputTokens:        p.MaxInputTokens,
		MaxOutputTokens:       p.MaxOutputTokens,
		SupportsPromptCaching: p.SupportsPromptCaching,
		Raw:                   p.Raw,
		Source:                domain.PricingSource(p.Source),
		CreatedAt:             p.CreatedAt,
		UpdatedAt:             p.UpdatedAt,
	}
}
