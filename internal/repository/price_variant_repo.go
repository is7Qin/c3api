// SPDX-License-Identifier: AGPL-3.0-or-later
package repository

import (
	"context"
	"fmt"
	"sort"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/pricevariant"
)

type PriceVariantRepo struct {
	client *ent.Client
}

func (r *PriceVariantRepo) UpsertFromLiteLLM(ctx context.Context, variants []*domain.PriceVariant) (int, error) {
	if len(variants) == 0 {
		return 0, nil
	}
	byModel := make(map[string][]*domain.PriceVariant)
	for _, v := range variants {
		byModel[v.Model] = append(byModel[v.Model], v)
	}
	total := 0
	for model, lst := range byModel {
		if _, err := r.ReplaceBatch(ctx, model, lst); err != nil {
			return total, err
		}
		total += len(lst)
	}
	return total, nil
}

func (r *PriceVariantRepo) ListByModel(ctx context.Context, model string) ([]*domain.PriceVariant, error) {
	rows, err := r.client.PriceVariant.Query().Where(pricevariant.ModelEQ(model)).Order(ent.Asc(pricevariant.FieldSeq)).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.PriceVariant, 0, len(rows))
	for _, v := range rows {
		out = append(out, toDomainVariant(v))
	}
	return out, nil
}

func (r *PriceVariantRepo) ListAll(ctx context.Context) ([]*domain.PriceVariant, error) {
	rows, err := r.client.PriceVariant.Query().Order(ent.Asc(pricevariant.FieldModel), ent.Asc(pricevariant.FieldSeq)).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.PriceVariant, 0, len(rows))
	for _, v := range rows {
		out = append(out, toDomainVariant(v))
	}
	return out, nil
}

// ReplaceBatch 批量整体替换该模型变体集：删除旧集 + 插入新集（事务）。
func (r *PriceVariantRepo) ReplaceBatch(ctx context.Context, model string, variants []*domain.PriceVariant) ([]*domain.PriceVariant, error) {
	// validation: seq unique already enforced by DB, but pre-check
	seqs := make(map[int]bool)
	for _, v := range variants {
		if v.MultBP == nil && v.SetInputPerM == nil && v.SetOutputPerM == nil {
			return nil, fmt.Errorf("variant seq %d: at least one effect required", v.Seq)
		}
		if seqs[v.Seq] {
			return nil, fmt.Errorf("duplicate seq %d", v.Seq)
		}
		seqs[v.Seq] = true
	}
	sort.Slice(variants, func(i, j int) bool { return variants[i].Seq < variants[j].Seq })
	// crude transaction via client: delete then create loop
	// use ent Tx if available via driver? Use client.Tx
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.PriceVariant.Delete().Where(pricevariant.ModelEQ(model)).Exec(ctx); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	for _, v := range variants {
		cre := tx.PriceVariant.Create().SetModel(model).SetSeq(v.Seq)
		if v.ServiceTier != nil {
			cre = cre.SetServiceTier(*v.ServiceTier)
		}
		if v.CtxMin != nil {
			cre = cre.SetCtxMin(*v.CtxMin)
		}
		if v.CtxMax != nil {
			cre = cre.SetCtxMax(*v.CtxMax)
		}
		if v.TimeStart != nil {
			cre = cre.SetTimeStart(*v.TimeStart)
		}
		if v.TimeEnd != nil {
			cre = cre.SetTimeEnd(*v.TimeEnd)
		}
		if v.DowMask != nil {
			cre = cre.SetDowMask(*v.DowMask)
		}
		if v.MultBP != nil {
			cre = cre.SetMultBp(*v.MultBP)
		}
		if v.SetInputPerM != nil {
			cre = cre.SetSetInputPerM(*v.SetInputPerM)
		}
		if v.SetOutputPerM != nil {
			cre = cre.SetSetOutputPerM(*v.SetOutputPerM)
		}
		if _, err := cre.Save(ctx); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return r.ListByModel(ctx, model)
}

func toDomainVariant(v *ent.PriceVariant) *domain.PriceVariant {
	return &domain.PriceVariant{
		ID:            v.ID,
		Model:         v.Model,
		Seq:           v.Seq,
		ServiceTier:   v.ServiceTier,
		CtxMin:        v.CtxMin,
		CtxMax:        v.CtxMax,
		TimeStart:     v.TimeStart,
		TimeEnd:       v.TimeEnd,
		DowMask:       v.DowMask,
		MultBP:        v.MultBp,
		SetInputPerM:  v.SetInputPerM,
		SetOutputPerM: v.SetOutputPerM,
		CreatedAt:     v.CreatedAt,
		UpdatedAt:     v.UpdatedAt,
	}
}
