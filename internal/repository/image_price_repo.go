// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

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
	"github.com/is7qin/c3api/internal/ent/imageprice"
)

// ImagePriceRepo 图片生成价格持久化（Task A 数据面；images 端点计费价格来源）。
// 机制与 PricingRepo 同款（共享 helper，见 litellm_upsert.go）：source 行级
// 互斥优先级 manual > litellm（拉取 upsert 带 WHERE 条件永不覆盖手动价）、
// 500/批独立事务、批内 model 排序 + 40P01 死锁重试。行有效性（至少一个价格
// 分量非 nil）在应用层判定——本仓库只落结构，不做业务校验（对齐 PricingRepo）。
type ImagePriceRepo struct {
	client *ent.Client
	// driver 为 raw SQL（UpsertFromLiteLLM 批量 upsert）用：普通 client 与
	// tx client（WithTx 内）均可用（对齐 PricingRepo 先例）。
	driver dialect.Driver
}

// UpsertFromLiteLLM 批量 upsert 拉取 image 价（评审 M-2 同款机制，评审修复后
// 分批/重试/部分成功语义由共享 helper 承载——litellm_upsert.go
// litellmUpsertBatches；本表只传 SQL 组装 upsertImageLitellmBatch）：
//   - 核心语义：ON CONFLICT (model) DO UPDATE ... WHERE image_price.source
//     != 'manual'——永不覆盖手动价；litellm 源行的换算价格、raw 与 source 均
//     更新（source 恒为 litellm）
//   - 分批 500/批、每批独立事务：部分成功可接受——返回成功行数，失败的批记
//     Warn 日志（返回首个失败错误，worker 侧决定重试/告警）
//   - 死锁收敛（#37 P3' 同款）：批内按 model 排序 + 40P01 瞬时重试
func (r *ImagePriceRepo) UpsertFromLiteLLM(ctx context.Context, rows []*domain.ImagePrice) (int, error) {
	now := time.Now()
	exec := func(ctx context.Context, start, end int) (int, error) {
		return r.upsertImageLitellmBatch(ctx, rows[start:end], now)
	}
	return litellmUpsertBatches(ctx, len(rows), "image_price",
		litellmUpsertOpts{BatchSize: litellmBatchSize, Retries: litellmUpsertRetries, Backoff: litellmUpsertBackoff}, exec)
}

// upsertImageLitellmBatch 单批 upsert（独立事务，单语句原子）：raw SQL 手工拼
// 多行 VALUES + ON CONFLICT (model) DO UPDATE SET ... WHERE image_price.source
// != 'manual'（ent v0.14 的 upsert 构建器不支持 DO UPDATE 的 WHERE 条件）。
// 9 列 = model + 三价格 + provider + raw + source + created_at/updated_at；
// raw JSONB 转 string 传参并 SQL 侧显式 ::jsonb 强转（nil → NULL）；批内按
// model 排序（锁顺序一致化，防多实例并发 deadlock——对齐 upsertLitellmBatch
// 先例）。
func (r *ImagePriceRepo) upsertImageLitellmBatch(ctx context.Context, batch []*domain.ImagePrice, now time.Time) (int, error) {
	sort.SliceStable(batch, func(i, j int) bool { return batch[i].Model < batch[j].Model })
	insertCols := []string{
		imageprice.FieldModel,
		imageprice.FieldInputImageTokenPricePerMillion,
		imageprice.FieldOutputImageTokenPricePerMillion,
		imageprice.FieldOutputCostPerImageMilli,
		imageprice.FieldProvider,
		imageprice.FieldRaw,
		imageprice.FieldSource,
		imageprice.FieldCreatedAt,
		imageprice.FieldUpdatedAt,
	}
	// 除 model/source/created_at 外全更新（三价格 + provider + raw + updated_at）；
	// source 由 WHERE 条件保护，created_at 首次插入后不变。
	updateCols := append(append([]string{}, insertCols[1:len(insertCols)-3]...), imageprice.FieldUpdatedAt)
	const colsPerRow = 9
	var buf strings.Builder
	buf.WriteString("INSERT INTO " + imageprice.Table + " (" + strings.Join(insertCols, ", ") + ") VALUES ")
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
			if c == 6 { // raw 列 → ::jsonb 强转
				fmt.Fprintf(&buf, "$%d::jsonb", n+c)
			} else {
				fmt.Fprintf(&buf, "$%d", n+c)
			}
		}
		buf.WriteString(")")
		var inP, outP, perP, provider, raw any
		if p.InputImageTokenPricePerMillion != nil {
			inP = *p.InputImageTokenPricePerMillion
		}
		if p.OutputImageTokenPricePerMillion != nil {
			outP = *p.OutputImageTokenPricePerMillion
		}
		if p.OutputCostPerImageMilli != nil {
			perP = *p.OutputCostPerImageMilli
		}
		if p.Provider != nil {
			provider = *p.Provider
		}
		if len(p.Raw) > 0 {
			raw = string(p.Raw)
		}
		// source 恒为 litellm（本方法即 litellm 拉取路径，防止误传 manual 行破坏优先级）。
		args = append(args, p.Model, inP, outP, perP, provider, raw,
			string(domain.PricingSourceLitellm), now, now)
	}
	buf.WriteString(" ON CONFLICT (" + imageprice.FieldModel + ") DO UPDATE SET ")
	for i, col := range updateCols {
		if i > 0 {
			buf.WriteString(", ")
		}
		buf.WriteString(col + " = EXCLUDED." + col)
	}
	buf.WriteString(" WHERE " + imageprice.Table + ".source != '" + string(domain.PricingSourceManual) + "'")

	tx, err := r.driver.Tx(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() // nolint:errcheck // Commit 成功后 Rollback 返回 ErrTxDone，忽略
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

// ImagePriceManual 手动设价入参（管理端 PUT /api/admin/image-price?model=X，
// 全量替换语义）：三价格分量全可选——nil = 清空该分量（新行 NULL；接管的
// litellm 行清空该分量价），非 nil = 显式设价（≥0，可为 0 = 该价明确为 0）。
// 单位：token 价毫分/1M image tokens、per-image 毫分/张（handler 边界已由
// API 入参 USD 换算）。校验（至少一个分量非 nil、非 nil ≥ 0）在 service 层。
// manual 行恒不写 raw（仓库强制清空）。
type ImagePriceManual struct {
	Model                           string
	InputImageTokenPricePerMillion  *int64
	OutputImageTokenPricePerMillion *int64
	OutputCostPerImageMilli         *int64
}

// HasAnyPrice 行有效性 = 至少一个价格分量非 nil（全 nil → 应用层 400 拒绝，
// 与 domain.ImagePrice.HasAnyPrice 同语义，写路径判定用）。
func (m *ImagePriceManual) HasAnyPrice() bool {
	return m.InputImageTokenPricePerMillion != nil ||
		m.OutputImageTokenPricePerMillion != nil ||
		m.OutputCostPerImageMilli != nil
}

// UpsertManual 手动设价（upsert 强制 source=manual，可接管已存在的 litellm
// 行——唯一冲突 → DO UPDATE 全字段 + source 改 manual）；model 非空。可选
// 字段语义见 ImagePriceManual：nil = 清空（接管行该分量价清除）；非 nil =
// 覆盖（可为 0）。manual 行恒不写 raw（接管 litellm 行时清空）。
// 返回落库后的完整行（GetImagePrice 重查）。
func (r *ImagePriceRepo) UpsertManual(ctx context.Context, m *ImagePriceManual) (*domain.ImagePrice, error) {
	if m.Model == "" {
		return nil, fmt.Errorf("image_price: model is required")
	}
	_, err := r.client.ImagePrice.Create().
		SetModel(m.Model).
		SetNillableInputImageTokenPricePerMillion(m.InputImageTokenPricePerMillion).
		SetNillableOutputImageTokenPricePerMillion(m.OutputImageTokenPricePerMillion).
		SetNillableOutputCostPerImageMilli(m.OutputCostPerImageMilli).
		SetSource(imageprice.SourceManual).
		OnConflictColumns(imageprice.FieldModel).
		Update(func(u *ent.ImagePriceUpsert) {
			u.SetSource(imageprice.SourceManual).
				SetUpdatedAt(time.Now())
			// 冲突更新路径无 SetNillable（ent 生成）：nil → 显式 SetNull（清掉
			// 接管行原有的分量价）；非 nil → Set 覆盖。全量替换语义（PUT）。
			if m.InputImageTokenPricePerMillion != nil {
				u.SetInputImageTokenPricePerMillion(*m.InputImageTokenPricePerMillion)
			} else {
				u.ClearInputImageTokenPricePerMillion()
			}
			if m.OutputImageTokenPricePerMillion != nil {
				u.SetOutputImageTokenPricePerMillion(*m.OutputImageTokenPricePerMillion)
			} else {
				u.ClearOutputImageTokenPricePerMillion()
			}
			if m.OutputCostPerImageMilli != nil {
				u.SetOutputCostPerImageMilli(*m.OutputCostPerImageMilli)
			} else {
				u.ClearOutputCostPerImageMilli()
			}
			// manual 行恒无 litellm 元数据/raw：接管 litellm 行时清掉（新行本来 NULL）。
			u.ClearProvider().
				ClearRaw()
		}).
		ID(ctx)
	if err != nil {
		return nil, err
	}
	return r.GetImagePrice(ctx, m.Model)
}

// DeleteManual 删除手动价行：仅 source=manual 可删（litellm 行 → ErrConflict——
// 语义：只允许删手动价，防误删拉取行；删除后下轮拉取会补回）；
// 行不存在 → ErrNotFound。错误消息恒英文（G3-2 分层：对外响应体恒英文，
// 中文仅限内部日志）。
func (r *ImagePriceRepo) DeleteManual(ctx context.Context, model string) error {
	n, err := r.client.ImagePrice.Delete().
		Where(imageprice.ModelEQ(model), imageprice.SourceEQ(imageprice.SourceManual)).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		exists, err := r.client.ImagePrice.Query().Where(imageprice.ModelEQ(model)).Exist(ctx)
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

// ListImagePrice 图片价格列表：分页/筛选（source、provider 等值、model 模糊
// ilike）/排序（sort 白名单：model/updated_at；非法值 → ErrInvalidSort，
// 对齐 ListQuery）。
func (r *ImagePriceRepo) ListImagePrice(ctx context.Context, q ListQuery, source *domain.PricingSource, provider, model string) ([]*domain.ImagePrice, int64, error) {
	pred := r.client.ImagePrice.Query()
	if source != nil {
		pred = pred.Where(imageprice.SourceEQ(imageprice.Source(*source)))
	}
	if provider != "" {
		pred = pred.Where(imageprice.ProviderEQ(provider))
	}
	if model != "" {
		pred = pred.Where(imageprice.ModelContainsFold(model))
	}
	total, err := pred.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	order, err := q.sortOrder(imagePriceSortFields)
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
	out := make([]*domain.ImagePrice, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainImagePrice(row))
	}
	return out, int64(total), nil
}

// GetImagePrice 按 model 取图片价格行（热路径快照读的 DB 侧数据源）；缺失 →
// ErrNotFound。
func (r *ImagePriceRepo) GetImagePrice(ctx context.Context, model string) (*domain.ImagePrice, error) {
	row, err := r.client.ImagePrice.Query().Where(imageprice.ModelEQ(model)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("%w: model=%q", ErrNotFound, model)
		}
		return nil, err
	}
	return toDomainImagePrice(row), nil
}

func toDomainImagePrice(p *ent.ImagePrice) *domain.ImagePrice {
	return &domain.ImagePrice{
		ID:                              p.ID,
		Model:                           p.Model,
		InputImageTokenPricePerMillion:  p.InputImageTokenPricePerMillion,
		OutputImageTokenPricePerMillion: p.OutputImageTokenPricePerMillion,
		OutputCostPerImageMilli:         p.OutputCostPerImageMilli,
		Provider:                        p.Provider,
		Raw:                             p.Raw,
		Source:                          domain.PricingSource(p.Source),
		CreatedAt:                       p.CreatedAt,
		UpdatedAt:                       p.UpdatedAt,
	}
}
