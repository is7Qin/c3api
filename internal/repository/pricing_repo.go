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
	"github.com/is7qin/c3api/internal/ent/pricing"
)

// PricingRepo 模型价格持久化（Phase 5 计费价格来源）。
// source 行级互斥优先级 manual > litellm：拉取 upsert 带 WHERE 条件永不覆盖
// 手动价；手动设价可接管 litellm 行；删除手动行后下轮拉取补回。
type PricingRepo struct {
	client *ent.Client
	// driver 为 raw SQL（UpsertFromLiteLLM 批量 upsert）用：普通 client 与
	// tx client（WithTx 内）均可用——ent v0.14 生成代码无 ExecContext，
	// raw SQL 经 dialect.Driver 统一执行（对齐 UpdateUserBalance 先例）。
	driver dialect.Driver
}

// UpsertFromLiteLLM 批量 upsert 拉取价（评审 M-2）：
//   - 核心语义：ON CONFLICT (model) DO UPDATE ... WHERE pricing.source != 'manual'
//     —— 永不覆盖手动价（表内一行 = 最终生效价）；litellm 源行的换算价格、
//     max_tokens、cache 价、provider/mode/supports_prompt_caching、raw 与
//     source 均更新（source 恒为 litellm）
//   - 分批/重试/部分成功语义由共享 helper 承载（litellm_upsert.go
//     litellmUpsertBatches：500/批独立事务、失败批 Warn + 首个错误上抛、
//     批内排序 + 40P01 死锁重试）；本表只传 SQL 组装（upsertLitellmBatch）
//   - 返回 n = 实际插入/更新的行数（DO UPDATE 被 WHERE 过滤掉的手动行不计入；
//     PG 对未修改行不产生命令标签计数）
func (r *PricingRepo) UpsertFromLiteLLM(ctx context.Context, rows []*domain.Pricing) (int, error) {
	now := time.Now()
	exec := func(ctx context.Context, start, end int) (int, error) {
		return r.upsertLitellmBatch(ctx, rows[start:end], now)
	}
	return litellmUpsertBatches(ctx, len(rows), "pricing",
		litellmUpsertOpts{BatchSize: litellmBatchSize, Retries: litellmUpsertRetries, Backoff: litellmUpsertBackoff}, exec)
}

// upsertLitellmBatch 单批 upsert（独立事务，单语句原子）：raw SQL 手工拼
// 多行 VALUES + ON CONFLICT (model) DO UPDATE SET ... WHERE pricings.source
// != 'manual'——ent v0.14 的 upsert 构建器不支持 DO UPDATE 的 WHERE 条件，
// 故 raw SQL 经 driver 执行。created_at/updated_at 显式传值（无默认值依赖）。
// raw JSONB：pgx 对 []byte 参数按 bytea 编码，无法隐式转 jsonb → 转 string 传参
// 并 SQL 侧显式 ::jsonb 强转（nil → NULL）；其余列 nil → NULL。
// #37 P3'：批内按冲突键排序（锁顺序一致化，见本函数首行 sort）——多实例
// pricing sync 并发批量 upsert 同批 model（INSERT..ON CONFLICT DO UPDATE
// 逐行取锁序 = VALUES 行序）锁顺序交错 → deadlock detected。
func (r *PricingRepo) upsertLitellmBatch(ctx context.Context, batch []*domain.Pricing, now time.Time) (int, error) {
	// #37 P3'：同款治本，pricing 批内按 model 排序——使各实例按同一顺序取
	// 行锁，消除死锁主因（残余交错由共享 helper litellmExecBatchWithRetry 的
	// 40P01 重试兜底，见 litellm_upsert.go）。批内 model 唯一（litellm 行无
	// 重复），排序纯为锁顺序一致化。
	sort.SliceStable(batch, func(i, j int) bool { return batch[i].Model < batch[j].Model })
	// 36 列 = 4 基础价 + max 窗口 2 + cache 2 + 矩阵 22（Phase 5）+ 元数据 6。
	insertCols := []string{
		pricing.FieldModel, pricing.FieldPromptPricePerMillion, pricing.FieldCompletionPricePerMillion,
		pricing.FieldMaxInputTokens, pricing.FieldMaxOutputTokens,
		pricing.FieldCacheReadPricePerMillion, pricing.FieldCacheCreationPricePerMillion,
		pricing.FieldPriorityPromptPricePerMillion, pricing.FieldPriorityCompletionPricePerMillion,
		pricing.FieldPriorityCacheReadPricePerMillion, pricing.FieldPriorityCacheCreationPricePerMillion,
		pricing.FieldFlexPromptPricePerMillion, pricing.FieldFlexCompletionPricePerMillion,
		pricing.FieldFlexCacheReadPricePerMillion, pricing.FieldFlexCacheCreationPricePerMillion,
		pricing.FieldAboveThreshold,
		pricing.FieldAbovePromptPricePerMillion, pricing.FieldAboveCompletionPricePerMillion,
		pricing.FieldAboveCacheReadPricePerMillion, pricing.FieldAboveCacheCreationPricePerMillion,
		pricing.FieldAbovePriorityPromptPricePerMillion, pricing.FieldAbovePriorityCompletionPricePerMillion,
		pricing.FieldAbovePriorityCacheReadPricePerMillion, pricing.FieldAbovePriorityCacheCreationPricePerMillion,
		pricing.FieldAboveFlexPromptPricePerMillion, pricing.FieldAboveFlexCompletionPricePerMillion,
		pricing.FieldAboveFlexCacheReadPricePerMillion, pricing.FieldAboveFlexCacheCreationPricePerMillion,
		pricing.FieldFastMultiplier,
		pricing.FieldProvider, pricing.FieldMode, pricing.FieldSupportsPromptCaching,
		pricing.FieldRaw, pricing.FieldSource, pricing.FieldCreatedAt, pricing.FieldUpdatedAt,
	}
	// 除 model/source/created_at 外全更新（含矩阵 22 列 + raw + updated_at）；
	// source 由 WHERE 条件保护，created_at 首次插入后不变。
	updateCols := append(append([]string{}, insertCols[1:len(insertCols)-3]...), pricing.FieldUpdatedAt)
	const colsPerRow = 36
	var buf strings.Builder
	buf.WriteString("INSERT INTO " + pricing.Table + " (" + strings.Join(insertCols, ", ") + ") VALUES ")
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
			if c == 33 { // raw 列 → ::jsonb 强转
				fmt.Fprintf(&buf, "$%d::jsonb", n+c)
			} else {
				fmt.Fprintf(&buf, "$%d", n+c)
			}
		}
		buf.WriteString(")")
		var maxIn, maxOut, cacheRead, cacheCreate, provider, mode, spc, raw any
		if p.MaxInputTokens != nil {
			maxIn = *p.MaxInputTokens
		}
		if p.MaxOutputTokens != nil {
			maxOut = *p.MaxOutputTokens
		}
		if p.CacheReadPricePerMillion != nil {
			cacheRead = *p.CacheReadPricePerMillion
		}
		if p.CacheCreationPricePerMillion != nil {
			cacheCreate = *p.CacheCreationPricePerMillion
		}
		if p.Provider != nil {
			provider = *p.Provider
		}
		if p.Mode != nil {
			mode = *p.Mode
		}
		if p.SupportsPromptCaching != nil {
			spc = *p.SupportsPromptCaching
		}
		if len(p.Raw) > 0 {
			raw = string(p.Raw)
		}
		// 矩阵 22 列：nil → NULL（计费回退语义）。先经 *int64 判空再解引用装箱，
		// 避免 typed-nil 指针装箱进 any 后 interface 判空失效（nil *int64 → any != nil）。
		matrixVals := []*int64{
			p.PriorityPromptPricePerMillion, p.PriorityCompletionPricePerMillion,
			p.PriorityCacheReadPricePerMillion, p.PriorityCacheCreationPricePerMillion,
			p.FlexPromptPricePerMillion, p.FlexCompletionPricePerMillion,
			p.FlexCacheReadPricePerMillion, p.FlexCacheCreationPricePerMillion,
			p.AboveThreshold,
			p.AbovePromptPricePerMillion, p.AboveCompletionPricePerMillion,
			p.AboveCacheReadPricePerMillion, p.AboveCacheCreationPricePerMillion,
			p.AbovePriorityPromptPricePerMillion, p.AbovePriorityCompletionPricePerMillion,
			p.AbovePriorityCacheReadPricePerMillion, p.AbovePriorityCacheCreationPricePerMillion,
			p.AboveFlexPromptPricePerMillion, p.AboveFlexCompletionPricePerMillion,
			p.AboveFlexCacheReadPricePerMillion, p.AboveFlexCacheCreationPricePerMillion,
			p.FastMultiplier,
		}
		matrix := make([]any, len(matrixVals))
		for i, v := range matrixVals {
			if v != nil {
				matrix[i] = *v
			}
		}
		// source 恒为 litellm（本方法即 litellm 拉取路径，防止误传 manual 行破坏优先级）。
		args = append(args, p.Model, p.PromptPricePerMillion, p.CompletionPricePerMillion,
			maxIn, maxOut, cacheRead, cacheCreate)
		args = append(args, matrix...)
		args = append(args, provider, mode, spc, raw,
			string(domain.PricingSourceLitellm), now, now)
	}
	buf.WriteString(" ON CONFLICT (" + pricing.FieldModel + ") DO UPDATE SET ")
	for i, col := range updateCols {
		if i > 0 {
			buf.WriteString(", ")
		}
		buf.WriteString(col + " = EXCLUDED." + col)
	}
	buf.WriteString(" WHERE " + pricing.Table + ".source != '" + string(domain.PricingSourceManual) + "'")

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

// PricingManual 手动设价入参（管理端 PUT /api/admin/pricing?model=X，全量替换语义）：
// Prompt/CompletionPricePerMillion 必填（≥0）；其余全部可选——nil = 不设价（新行
// NULL；接管的 litellm 行清空该矩阵价，计费回退基础价），非 nil = 显式设价（≥0，
// 可为 0 = 该价明确为 0）。单位毫分/1M tokens（1 USD = 100,000 毫分）；
// FastMultiplier 为万分数（20000 = ×2.0）。manual 行恒不写 litellm 元数据
// （provider/mode/spc/raw，仓库强制清空）。
type PricingManual struct {
	Model                                     string
	PromptPricePerMillion                     int64
	CompletionPricePerMillion                 int64
	CacheReadPricePerMillion                  *int64
	CacheCreationPricePerMillion              *int64
	PriorityPromptPricePerMillion             *int64 // service_tier=priority 单价替换档
	PriorityCompletionPricePerMillion         *int64
	PriorityCacheReadPricePerMillion          *int64
	PriorityCacheCreationPricePerMillion      *int64
	FlexPromptPricePerMillion                 *int64 // service_tier=flex 单价替换档
	FlexCompletionPricePerMillion             *int64
	FlexCacheReadPricePerMillion              *int64
	FlexCacheCreationPricePerMillion          *int64
	AboveThreshold                            *int64 // 分段阈值（tokens）；nil = 无分段
	AbovePromptPricePerMillion                *int64 // 超阈值分段价（基础组）
	AboveCompletionPricePerMillion            *int64
	AboveCacheReadPricePerMillion             *int64
	AboveCacheCreationPricePerMillion         *int64
	AbovePriorityPromptPricePerMillion        *int64 // 超阈值分段价（priority 组）
	AbovePriorityCompletionPricePerMillion    *int64
	AbovePriorityCacheReadPricePerMillion     *int64
	AbovePriorityCacheCreationPricePerMillion *int64
	AboveFlexPromptPricePerMillion            *int64 // 超阈值分段价（flex 组）
	AboveFlexCompletionPricePerMillion        *int64
	AboveFlexCacheReadPricePerMillion         *int64
	AboveFlexCacheCreationPricePerMillion     *int64
	FastMultiplier                            *int64 // Anthropic Fast Mode 整单倍率（万分数）
}

// UpsertManual 手动设价（upsert 强制 source=manual，可接管已存在的 litellm
// 行——唯一冲突 → DO UPDATE 全字段 + source 改 manual）；model 非空。
// 可选字段语义见 PricingManual：nil = 清空（接管行该矩阵价清除，回退基础价）；
// 非 nil = 覆盖（可为 0——与主价一致，表示该价明确为 0）。manual 行恒不写
// litellm 元数据（provider/mode/spc/raw，接管 litellm 行时清空）。
// 返回落库后的完整行（GetPricing 重查：upsert 路径 RETURNING 仅 id，
// created_at 等列需读库才准确）。
func (r *PricingRepo) UpsertManual(ctx context.Context, m *PricingManual) (*domain.Pricing, error) {
	if m.Model == "" {
		return nil, fmt.Errorf("pricing: model is required")
	}
	_, err := r.client.Pricing.Create().
		SetModel(m.Model).SetPromptPricePerMillion(m.PromptPricePerMillion).
		SetCompletionPricePerMillion(m.CompletionPricePerMillion).
		SetNillableCacheReadPricePerMillion(m.CacheReadPricePerMillion).
		SetNillableCacheCreationPricePerMillion(m.CacheCreationPricePerMillion).
		SetNillablePriorityPromptPricePerMillion(m.PriorityPromptPricePerMillion).
		SetNillablePriorityCompletionPricePerMillion(m.PriorityCompletionPricePerMillion).
		SetNillablePriorityCacheReadPricePerMillion(m.PriorityCacheReadPricePerMillion).
		SetNillablePriorityCacheCreationPricePerMillion(m.PriorityCacheCreationPricePerMillion).
		SetNillableFlexPromptPricePerMillion(m.FlexPromptPricePerMillion).
		SetNillableFlexCompletionPricePerMillion(m.FlexCompletionPricePerMillion).
		SetNillableFlexCacheReadPricePerMillion(m.FlexCacheReadPricePerMillion).
		SetNillableFlexCacheCreationPricePerMillion(m.FlexCacheCreationPricePerMillion).
		SetNillableAboveThreshold(m.AboveThreshold).
		SetNillableAbovePromptPricePerMillion(m.AbovePromptPricePerMillion).
		SetNillableAboveCompletionPricePerMillion(m.AboveCompletionPricePerMillion).
		SetNillableAboveCacheReadPricePerMillion(m.AboveCacheReadPricePerMillion).
		SetNillableAboveCacheCreationPricePerMillion(m.AboveCacheCreationPricePerMillion).
		SetNillableAbovePriorityPromptPricePerMillion(m.AbovePriorityPromptPricePerMillion).
		SetNillableAbovePriorityCompletionPricePerMillion(m.AbovePriorityCompletionPricePerMillion).
		SetNillableAbovePriorityCacheReadPricePerMillion(m.AbovePriorityCacheReadPricePerMillion).
		SetNillableAbovePriorityCacheCreationPricePerMillion(m.AbovePriorityCacheCreationPricePerMillion).
		SetNillableAboveFlexPromptPricePerMillion(m.AboveFlexPromptPricePerMillion).
		SetNillableAboveFlexCompletionPricePerMillion(m.AboveFlexCompletionPricePerMillion).
		SetNillableAboveFlexCacheReadPricePerMillion(m.AboveFlexCacheReadPricePerMillion).
		SetNillableAboveFlexCacheCreationPricePerMillion(m.AboveFlexCacheCreationPricePerMillion).
		SetNillableFastMultiplier(m.FastMultiplier).
		SetSource(pricing.SourceManual).
		OnConflictColumns(pricing.FieldModel).
		Update(func(u *ent.PricingUpsert) {
			u.SetPromptPricePerMillion(m.PromptPricePerMillion).
				SetCompletionPricePerMillion(m.CompletionPricePerMillion).
				SetSource(pricing.SourceManual).
				SetUpdatedAt(time.Now())
			// 冲突更新路径无 SetNillable（ent 生成）：nil → 显式 SetNull（清掉
			// 接管行原有的矩阵价）；非 nil → Set 覆盖。矩阵价全量替换语义（PUT）。
			if m.CacheReadPricePerMillion != nil {
				u.SetCacheReadPricePerMillion(*m.CacheReadPricePerMillion)
			} else {
				u.ClearCacheReadPricePerMillion()
			}
			if m.CacheCreationPricePerMillion != nil {
				u.SetCacheCreationPricePerMillion(*m.CacheCreationPricePerMillion)
			} else {
				u.ClearCacheCreationPricePerMillion()
			}
			setMatrix := func(v *int64, set func(int64) *ent.PricingUpsert, clr func() *ent.PricingUpsert) {
				if v != nil {
					set(*v)
				} else {
					clr()
				}
			}
			setMatrix(m.PriorityPromptPricePerMillion, u.SetPriorityPromptPricePerMillion, u.ClearPriorityPromptPricePerMillion)
			setMatrix(m.PriorityCompletionPricePerMillion, u.SetPriorityCompletionPricePerMillion, u.ClearPriorityCompletionPricePerMillion)
			setMatrix(m.PriorityCacheReadPricePerMillion, u.SetPriorityCacheReadPricePerMillion, u.ClearPriorityCacheReadPricePerMillion)
			setMatrix(m.PriorityCacheCreationPricePerMillion, u.SetPriorityCacheCreationPricePerMillion, u.ClearPriorityCacheCreationPricePerMillion)
			setMatrix(m.FlexPromptPricePerMillion, u.SetFlexPromptPricePerMillion, u.ClearFlexPromptPricePerMillion)
			setMatrix(m.FlexCompletionPricePerMillion, u.SetFlexCompletionPricePerMillion, u.ClearFlexCompletionPricePerMillion)
			setMatrix(m.FlexCacheReadPricePerMillion, u.SetFlexCacheReadPricePerMillion, u.ClearFlexCacheReadPricePerMillion)
			setMatrix(m.FlexCacheCreationPricePerMillion, u.SetFlexCacheCreationPricePerMillion, u.ClearFlexCacheCreationPricePerMillion)
			setMatrix(m.AboveThreshold, u.SetAboveThreshold, u.ClearAboveThreshold)
			setMatrix(m.AbovePromptPricePerMillion, u.SetAbovePromptPricePerMillion, u.ClearAbovePromptPricePerMillion)
			setMatrix(m.AboveCompletionPricePerMillion, u.SetAboveCompletionPricePerMillion, u.ClearAboveCompletionPricePerMillion)
			setMatrix(m.AboveCacheReadPricePerMillion, u.SetAboveCacheReadPricePerMillion, u.ClearAboveCacheReadPricePerMillion)
			setMatrix(m.AboveCacheCreationPricePerMillion, u.SetAboveCacheCreationPricePerMillion, u.ClearAboveCacheCreationPricePerMillion)
			setMatrix(m.AbovePriorityPromptPricePerMillion, u.SetAbovePriorityPromptPricePerMillion, u.ClearAbovePriorityPromptPricePerMillion)
			setMatrix(m.AbovePriorityCompletionPricePerMillion, u.SetAbovePriorityCompletionPricePerMillion, u.ClearAbovePriorityCompletionPricePerMillion)
			setMatrix(m.AbovePriorityCacheReadPricePerMillion, u.SetAbovePriorityCacheReadPricePerMillion, u.ClearAbovePriorityCacheReadPricePerMillion)
			setMatrix(m.AbovePriorityCacheCreationPricePerMillion, u.SetAbovePriorityCacheCreationPricePerMillion, u.ClearAbovePriorityCacheCreationPricePerMillion)
			setMatrix(m.AboveFlexPromptPricePerMillion, u.SetAboveFlexPromptPricePerMillion, u.ClearAboveFlexPromptPricePerMillion)
			setMatrix(m.AboveFlexCompletionPricePerMillion, u.SetAboveFlexCompletionPricePerMillion, u.ClearAboveFlexCompletionPricePerMillion)
			setMatrix(m.AboveFlexCacheReadPricePerMillion, u.SetAboveFlexCacheReadPricePerMillion, u.ClearAboveFlexCacheReadPricePerMillion)
			setMatrix(m.AboveFlexCacheCreationPricePerMillion, u.SetAboveFlexCacheCreationPricePerMillion, u.ClearAboveFlexCacheCreationPricePerMillion)
			setMatrix(m.FastMultiplier, u.SetFastMultiplier, u.ClearFastMultiplier)
			// manual 行恒无 litellm 元数据/raw：接管 litellm 行时清掉（新行本来 NULL）。
			u.ClearProvider().
				ClearMode().
				ClearSupportsPromptCaching().
				ClearRaw()
		}).
		ID(ctx)
	if err != nil {
		return nil, err
	}
	return r.GetPricing(ctx, m.Model)
}

// DeleteManual 删除手动价行：仅 source=manual 可删（litellm 行 → ErrConflict——
// 语义：只允许删手动价，防误删拉取行；删除后下轮拉取会补回）；
// 行不存在 → ErrNotFound。错误消息恒英文（G3-2 分层：对外响应体恒英文——
// 与 ImagePriceRepo.DeleteManual 同款对齐；中文仅限内部日志）。
func (r *PricingRepo) DeleteManual(ctx context.Context, model string) error {
	n, err := r.client.Pricing.Delete().
		Where(pricing.ModelEQ(model), pricing.SourceEQ(pricing.SourceManual)).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		exists, err := r.client.Pricing.Query().Where(pricing.ModelEQ(model)).Exist(ctx)
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

// ListPricing 价格列表：分页/筛选（source、provider 等值、model 模糊 ilike）/
// 排序（sort 白名单：model/updated_at；非法值 → ErrInvalidSort，对齐
// ListQuery）。provider 为 DB 侧自由字符串等值（不限于 openapi Provider
// enum——litellm_provider 动态，新厂商不受枚举约束）。
func (r *PricingRepo) ListPricing(ctx context.Context, q ListQuery, source *domain.PricingSource, provider, model string) ([]*domain.Pricing, int64, error) {
	pred := r.client.Pricing.Query()
	if source != nil {
		pred = pred.Where(pricing.SourceEQ(pricing.Source(*source)))
	}
	if provider != "" {
		pred = pred.Where(pricing.ProviderEQ(provider))
	}
	if model != "" {
		pred = pred.Where(pricing.ModelContainsFold(model))
	}
	total, err := pred.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	order, err := q.sortOrder(pricingSortFields)
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
	out := make([]*domain.Pricing, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainPricing(row))
	}
	return out, int64(total), nil
}

// GetPricing 按 model 取价格行（内部/后续 Phase 5 计费用）；缺失 → ErrNotFound。
func (r *PricingRepo) GetPricing(ctx context.Context, model string) (*domain.Pricing, error) {
	row, err := r.client.Pricing.Query().Where(pricing.ModelEQ(model)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("%w: model=%q", ErrNotFound, model)
		}
		return nil, err
	}
	return toDomainPricing(row), nil
}

func toDomainPricing(p *ent.Pricing) *domain.Pricing {
	return &domain.Pricing{
		ID:                                        p.ID,
		Model:                                     p.Model,
		PromptPricePerMillion:                     p.PromptPricePerMillion,
		CompletionPricePerMillion:                 p.CompletionPricePerMillion,
		MaxInputTokens:                            p.MaxInputTokens,
		MaxOutputTokens:                           p.MaxOutputTokens,
		CacheReadPricePerMillion:                  p.CacheReadPricePerMillion,
		CacheCreationPricePerMillion:              p.CacheCreationPricePerMillion,
		PriorityPromptPricePerMillion:             p.PriorityPromptPricePerMillion,
		PriorityCompletionPricePerMillion:         p.PriorityCompletionPricePerMillion,
		PriorityCacheReadPricePerMillion:          p.PriorityCacheReadPricePerMillion,
		PriorityCacheCreationPricePerMillion:      p.PriorityCacheCreationPricePerMillion,
		FlexPromptPricePerMillion:                 p.FlexPromptPricePerMillion,
		FlexCompletionPricePerMillion:             p.FlexCompletionPricePerMillion,
		FlexCacheReadPricePerMillion:              p.FlexCacheReadPricePerMillion,
		FlexCacheCreationPricePerMillion:          p.FlexCacheCreationPricePerMillion,
		AboveThreshold:                            p.AboveThreshold,
		AbovePromptPricePerMillion:                p.AbovePromptPricePerMillion,
		AboveCompletionPricePerMillion:            p.AboveCompletionPricePerMillion,
		AboveCacheReadPricePerMillion:             p.AboveCacheReadPricePerMillion,
		AboveCacheCreationPricePerMillion:         p.AboveCacheCreationPricePerMillion,
		AbovePriorityPromptPricePerMillion:        p.AbovePriorityPromptPricePerMillion,
		AbovePriorityCompletionPricePerMillion:    p.AbovePriorityCompletionPricePerMillion,
		AbovePriorityCacheReadPricePerMillion:     p.AbovePriorityCacheReadPricePerMillion,
		AbovePriorityCacheCreationPricePerMillion: p.AbovePriorityCacheCreationPricePerMillion,
		AboveFlexPromptPricePerMillion:            p.AboveFlexPromptPricePerMillion,
		AboveFlexCompletionPricePerMillion:        p.AboveFlexCompletionPricePerMillion,
		AboveFlexCacheReadPricePerMillion:         p.AboveFlexCacheReadPricePerMillion,
		AboveFlexCacheCreationPricePerMillion:     p.AboveFlexCacheCreationPricePerMillion,
		FastMultiplier:                            p.FastMultiplier,
		Provider:                                  p.Provider,
		Mode:                                      p.Mode,
		SupportsPromptCaching:                     p.SupportsPromptCaching,
		Raw:                                       p.Raw,
		Source:                                    domain.PricingSource(p.Source),
		CreatedAt:                                 p.CreatedAt,
		UpdatedAt:                                 p.UpdatedAt,
	}
}
