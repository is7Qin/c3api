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
	"github.com/is7qin/c3api/internal/ent/functionprice"
)

// FunctionPriceRepo 按单元计费功能类价格持久化（search 起，audio/video 等未来
// per-unit 端点复用；对齐 image_price 形态）。机制与 PricingRepo/ImagePriceRepo
// 同款（共享 helper，见 litellm_upsert.go）：source 行级互斥优先级 manual >
// litellm（拉取 upsert 带 WHERE 条件永不覆盖手动价）、500/批独立事务、批内
// model 排序 + 40P01 死锁重试。行有效性（price_per_call 非 nil）在应用层判定
// ——本仓库只落结构，不做业务校验（对齐两表先例）。
type FunctionPriceRepo struct {
	client *ent.Client
	// driver 为 raw SQL（UpsertFromLiteLLM 批量 upsert）用：普通 client 与
	// tx client（WithTx 内）均可用（对齐 PricingRepo 先例）。
	driver dialect.Driver
}

// UpsertFromLiteLLM 批量 upsert 拉取按单元价（机制与 ImagePriceRepo 同款，
// 语义/分批/重试/部分成功由共享 helper litellmUpsertBatches 承载）：
//   - 核心语义：ON CONFLICT (model) DO UPDATE ... WHERE function_price.source
//     != 'manual'——永不覆盖手动价；litellm 源行的换算价格、raw 与 source 均
//     更新（source 恒为 litellm）
//   - 分批 500/批、每批独立事务：部分成功可接受——返回成功行数，失败的批记
//     Warn 日志（返回首个失败错误，worker 侧决定重试/告警）
//   - 死锁收敛（#37 P3' 同款）：批内按 model 排序 + 40P01 瞬时重试
func (r *FunctionPriceRepo) UpsertFromLiteLLM(ctx context.Context, rows []*domain.FunctionPrice) (int, error) {
	now := time.Now()
	exec := func(ctx context.Context, start, end int) (int, error) {
		return r.upsertFunctionLitellmBatch(ctx, rows[start:end], now)
	}
	return litellmUpsertBatches(ctx, len(rows), "function_price",
		litellmUpsertOpts{BatchSize: litellmBatchSize, Retries: litellmUpsertRetries, Backoff: litellmUpsertBackoff}, exec)
}

// upsertFunctionLitellmBatch 单批 upsert（独立事务，单语句原子）：raw SQL 手工拼
// 多行 VALUES + ON CONFLICT (model) DO UPDATE SET ... WHERE function_price.source
// != 'manual'（ent v0.14 的 upsert 构建器不支持 DO UPDATE 的 WHERE 条件）。
// 7 列 = model + price_per_call + provider + raw + source + created_at/
// updated_at；raw JSONB 转 string 传参并 SQL 侧显式 ::jsonb 强转（nil →
// NULL）；批内按 model 排序（锁顺序一致化，防多实例并发 deadlock——对齐
// upsertLitellmBatch 先例）。
func (r *FunctionPriceRepo) upsertFunctionLitellmBatch(ctx context.Context, batch []*domain.FunctionPrice, now time.Time) (int, error) {
	sort.SliceStable(batch, func(i, j int) bool { return batch[i].Model < batch[j].Model })
	insertCols := []string{
		functionprice.FieldModel,
		functionprice.FieldPricePerCall,
		functionprice.FieldProvider,
		functionprice.FieldRaw,
		functionprice.FieldSource,
		functionprice.FieldCreatedAt,
		functionprice.FieldUpdatedAt,
	}
	// 除 model/source/created_at 外全更新（price_per_call + provider + raw +
	// updated_at）；source 由 WHERE 条件保护，created_at 首次插入后不变。
	updateCols := append(append([]string{}, insertCols[1:len(insertCols)-3]...), functionprice.FieldUpdatedAt)
	const colsPerRow = 7
	var buf strings.Builder
	buf.WriteString("INSERT INTO " + functionprice.Table + " (" + strings.Join(insertCols, ", ") + ") VALUES ")
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
			if c == 4 { // raw 列 → ::jsonb 强转
				fmt.Fprintf(&buf, "$%d::jsonb", n+c)
			} else {
				fmt.Fprintf(&buf, "$%d", n+c)
			}
		}
		buf.WriteString(")")
		var perCall, provider, raw any
		if p.PricePerCall != nil {
			perCall = *p.PricePerCall
		}
		if p.Provider != nil {
			provider = *p.Provider
		}
		if len(p.Raw) > 0 {
			raw = string(p.Raw)
		}
		// source 恒为 litellm（本方法即 litellm 拉取路径，防止误传 manual 行破坏优先级）。
		args = append(args, p.Model, perCall, provider, raw,
			string(domain.PricingSourceLitellm), now, now)
	}
	buf.WriteString(" ON CONFLICT (" + functionprice.FieldModel + ") DO UPDATE SET ")
	for i, col := range updateCols {
		if i > 0 {
			buf.WriteString(", ")
		}
		buf.WriteString(col + " = EXCLUDED." + col)
	}
	buf.WriteString(" WHERE " + functionprice.Table + ".source != '" + string(domain.PricingSourceManual) + "'")

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

// FunctionPriceManual 手动设价入参（管理端 PUT /api/admin/function-prices?model=X，
// 全量替换语义）：price_per_call 必填（≥0，可为 0 = 该价明确为 0——按次免费）。
// 单位：毫分/次（handler 边界已由 API 入参 USD 换算）。校验（非 nil 且 ≥0）
// 在 service 层。manual 行恒不写 raw（仓库强制清空）。
type FunctionPriceManual struct {
	Model        string
	PricePerCall *int64
}

// HasAnyPrice 行有效性 = 按单元价非 nil（全 nil → 应用层 400 拒绝，与
// domain.FunctionPrice.HasAnyPrice 同语义，写路径判定用）。
func (m *FunctionPriceManual) HasAnyPrice() bool {
	return m.PricePerCall != nil
}

// UpsertManual 手动设价（upsert 强制 source=manual，可接管已存在的 litellm
// 行——唯一冲突 → DO UPDATE 全字段 + source 改 manual）；model 非空。
// PricePerCall 语义见 FunctionPriceManual：非 nil = 覆盖（可为 0）；nil =
// 清空（接管行该分量价清除）。manual 行恒不写 raw（接管 litellm 行时清空）。
// 返回落库后的完整行（GetFunctionPrice 重查）。
// 调用方需先校验行有效性（HasAnyPrice）：全 nil 属非法状态（service 层
// 400 拦截，当前唯一入口 PUT 必经）。冲突路径的 nil → ClearPricePerCall
// 分支因此不可达，保留为防御完整性——防未来绕过 service 直接调用时
// 产生全 nil 落行；如该约束被违反，请先在此方法补显式拒绝。
func (r *FunctionPriceRepo) UpsertManual(ctx context.Context, m *FunctionPriceManual) (*domain.FunctionPrice, error) {
	if m.Model == "" {
		return nil, fmt.Errorf("function_price: model is required")
	}
	_, err := r.client.FunctionPrice.Create().
		SetModel(m.Model).
		SetNillablePricePerCall(m.PricePerCall).
		SetSource(functionprice.SourceManual).
		OnConflictColumns(functionprice.FieldModel).
		Update(func(u *ent.FunctionPriceUpsert) {
			u.SetSource(functionprice.SourceManual).
				SetUpdatedAt(time.Now())
			// 冲突更新路径无 SetNillable（ent 生成）：nil → 显式 SetNull（清掉
			// 接管行原有的分量价）；非 nil → Set 覆盖。全量替换语义（PUT）。
			if m.PricePerCall != nil {
				u.SetPricePerCall(*m.PricePerCall)
			} else {
				u.ClearPricePerCall()
			}
			// manual 行恒无 litellm 元数据/raw：接管 litellm 行时清掉（新行本来 NULL）。
			u.ClearProvider().
				ClearRaw()
		}).
		ID(ctx)
	if err != nil {
		return nil, err
	}
	return r.GetFunctionPrice(ctx, m.Model)
}

// DeleteManual 删除手动价行：仅 source=manual 可删（litellm 行 → ErrConflict——
// 语义：只允许删手动价，防误删拉取行；删除后下轮拉取会补回；codex-search
// 种子行亦为 manual 源，可删——重启 bootstrap 幂等补回）；
// 行不存在 → ErrNotFound。
func (r *FunctionPriceRepo) DeleteManual(ctx context.Context, model string) error {
	n, err := r.client.FunctionPrice.Delete().
		Where(functionprice.ModelEQ(model), functionprice.SourceEQ(functionprice.SourceManual)).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		exists, err := r.client.FunctionPrice.Query().Where(functionprice.ModelEQ(model)).Exist(ctx)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("%w: model=%q source=litellm（只允许删手动价）", ErrConflict, model)
		}
		return fmt.Errorf("%w: model=%q", ErrNotFound, model)
	}
	return nil
}

// ListFunctionPrice 按单元价列表：分页/筛选（source、provider 等值、model 模糊
// ilike）/排序（sort 白名单：model/updated_at；非法值 → ErrInvalidSort，
// 对齐 ListQuery）。
func (r *FunctionPriceRepo) ListFunctionPrice(ctx context.Context, q ListQuery, source *domain.PricingSource, provider, model string) ([]*domain.FunctionPrice, int64, error) {
	pred := r.client.FunctionPrice.Query()
	if source != nil {
		pred = pred.Where(functionprice.SourceEQ(functionprice.Source(*source)))
	}
	if provider != "" {
		pred = pred.Where(functionprice.ProviderEQ(provider))
	}
	if model != "" {
		pred = pred.Where(functionprice.ModelContainsFold(model))
	}
	total, err := pred.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	order, err := q.sortOrder(functionPriceSortFields)
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
	out := make([]*domain.FunctionPrice, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainFunctionPrice(row))
	}
	return out, int64(total), nil
}

// GetFunctionPrice 按 model 取按单元价行（热路径快照读的 DB 侧数据源）；缺失 →
// ErrNotFound。
func (r *FunctionPriceRepo) GetFunctionPrice(ctx context.Context, model string) (*domain.FunctionPrice, error) {
	row, err := r.client.FunctionPrice.Query().Where(functionprice.ModelEQ(model)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("%w: model=%q", ErrNotFound, model)
		}
		return nil, err
	}
	return toDomainFunctionPrice(row), nil
}

// EnsureCodexSearchSeed 幂等插入 codex-search 初始化行（bootstrap 钩子；启动期
// 在 ent migrate 之后调用）：model="codex-search" + price_per_call=1000
// （$0.01/次，domain.DefaultCodexSearchPricePerCall）+ source=manual。
// ON CONFLICT (model) DO NOTHING——已存在（含管理端改价后的行）恒不覆盖：
// 种子只在缺失时补一次，管理端改价/删除后重启按当前值补回（删除后下轮
// 启动重新播种，语义 = "默认价永远有兜底"）。失败即返回错误（启动 bootstrap
// 面，调用方 fatal）。
func (r *FunctionPriceRepo) EnsureCodexSearchSeed(ctx context.Context) error {
	// raw SQL 而非 ent upsert 构建器：ent 的 OnConflictColumns().DoNothing().
	// ID(ctx) 带 RETURNING id——冲突（行已存在）时无返回行 → sql.ErrNoRows
	// 报错，幂等语义被破坏；无 RETURNING 的 INSERT ... ON CONFLICT DO NOTHING
	// 冲突时静默跳过（受影响行数 0），首插/重复调用/管理端改价后均不报错。
	query := "INSERT INTO " + functionprice.Table +
		" (" + functionprice.FieldModel + ", " + functionprice.FieldPricePerCall +
		", " + functionprice.FieldSource + ", " + functionprice.FieldCreatedAt +
		", " + functionprice.FieldUpdatedAt + ") VALUES ($1, $2, '" +
		string(domain.PricingSourceManual) + "', $3, $3)" +
		" ON CONFLICT (" + functionprice.FieldModel + ") DO NOTHING"
	var res sql.Result
	return r.driver.Exec(ctx, query, []any{
		domain.CodexSearchModel, domain.DefaultCodexSearchPricePerCall, time.Now(),
	}, &res)
}

func toDomainFunctionPrice(p *ent.FunctionPrice) *domain.FunctionPrice {
	return &domain.FunctionPrice{
		ID:           p.ID,
		Model:        p.Model,
		PricePerCall: p.PricePerCall,
		Provider:     p.Provider,
		Raw:          p.Raw,
		Source:       domain.PricingSource(p.Source),
		CreatedAt:    p.CreatedAt,
		UpdatedAt:    p.UpdatedAt,
	}
}
