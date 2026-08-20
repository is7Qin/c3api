// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// usage_logs 明细查询/插入（消费面改名裁决：log_repo → usage 语义命名——/logs
// API 改名 /usages 后内部类型随改名，UsageRepo/UsageQuery/QueryUsages；错误审计
// 面由 errlog_repo.go（err_logs）承载）。

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/usagelog"
)

type UsageQuery struct {
	GroupID   int64 // 0 = 不过滤
	AccountID int64
	UserID    int64 // 0 = 不过滤（/api/user/usages 强制 = 自己）
	KeyID     int64
	Model     string
	Format    string // 空 = 不过滤（无效值自然查空——与 model 同语义，契约不校验值域）
	ErrorType string // usage_logs = 纯计费明细（仅 cost>0）→ 值域收敛 none/abort（err_logs 分表后）
	From      *time.Time
	To        *time.Time
	Cursor    int64 // keyset 游标（上页最后一条 id；<=0 = 首页无 id 谓词）
	Limit     int
}

type UsageRepo struct {
	client *ent.Client
	// pool 为聚合 SQL 直查入口（ScanUsageAgg——usage_logs 含 raw_cost 等
	// SUM 聚合，ent 构建器无 SUM 能力，pgx 直查同 StatRepo carve-out 形态）；
	// NewWithPG 注入（生产与 ent driver 同 DSN），New 未注入 → 显式错误。
	pool *pgxpool.Pool
}

// usageAggMaxAccountIDs ScanUsageAgg 批量 ids 上限（= handler account_ids
// ≤100 契约——repo 层防御 handler 之外调用方，N5）。
const usageAggMaxAccountIDs = 100

func (r *UsageRepo) InsertBatch(ctx context.Context, logs []*domain.UsageLog) error {
	if len(logs) == 0 {
		return nil
	}
	builders := make([]*ent.UsageLogCreate, 0, len(logs))
	for _, l := range logs {
		builders = append(builders, buildUsageLogCreate(r.client, l))
	}
	_, err := r.client.UsageLog.CreateBulk(builders...).Save(ctx)
	return err
}

// buildUsageLogCreate 构建单条 usagelog 插入构建器（InsertBatch 与计费事务
// DeductAndLog 共用——tx client 经同一 client 传入即同一事务连接）。
// 计费列（Phase 5）：Cost 毫分（0 = 未计费/错误路径）；BillingTier 空 = 未计费
// 路径（落库 NULL）；AboveHit/Overdraft 布尔直接落。RawCost（spec 2026-08-18）
// 乘倍率前原始成本——恒落（对齐 SetCost，ent 缺省 0 无妨），COPY 路径
// usageLogRowValues 同序（两路径列集合锚定）。
// 时间/价格快照列（nil = NULL 落库，SQL 不写该列）：TTFTMS 首 token 时间毫秒
// （非流式/失败路径 nil）；Price*Millis 每 M token 毫分单价快照（未计费路径
// /无该分量 nil）。
// 统一计费模型功能调用分量（spec 2026-08-13）：CallCount 直接落（0 默认——
// 图片生成 = 张数、search = 1）；PricePerCallMillis **毫分/单元**（search 每次
// /图片每张，例外单位——per-call 计费不走 /1e6 除法，同原 price_per_image
// _millis 语义）——nil = NULL 落库。原图片 6 列已删：image token 并入
// InputTokens/OutputTokens（TotalTokens 口径不变）。
// 用户裁决（err_logs 分表）：StatusCode/ErrorMessage 为域内瞬态审计字段
// （err_logs 承载），不再写 usage_logs（该两列已从表移除——瘦身）。
func buildUsageLogCreate(client *ent.Client, l *domain.UsageLog) *ent.UsageLogCreate {
	c := client.UsageLog.Create().
		SetRequestID(l.RequestID).
		SetModel(l.Model).
		SetFormat(usagelog.Format(l.Format)).
		SetErrorType(string(l.ErrorType)).
		SetLatencyMs(l.LatencyMS).
		SetInputTokens(l.InputTokens).
		SetOutputTokens(l.OutputTokens).
		SetTotalTokens(l.TotalTokens).
		SetCacheReadTokens(l.CacheReadTokens).
		SetCacheCreationTokens(l.CacheCreationTokens).
		SetCallCount(l.CallCount).
		SetCost(l.Cost).
		SetRawCost(l.RawCost).
		SetAboveHit(l.AboveHit).
		SetOverdraft(l.Overdraft).
		SetCreatedAt(l.CreatedAt)
	// client_ip（S-E 2026-08-17）：非空才 Set（ent 只落被 Set 的列——空 = NULL
	// 不写该列，与 COPY 路径 usageLogRowValues 条件赋值一一对应）。
	if l.ClientIP != "" {
		c = c.SetClientIP(l.ClientIP)
	}
	if l.GroupID > 0 {
		c = c.SetGroupID(l.GroupID)
	}
	if l.AccountID > 0 {
		c = c.SetAccountID(l.AccountID)
	}
	if l.TemplateID > 0 {
		c = c.SetTemplateID(l.TemplateID)
	}
	if l.UserID > 0 {
		c = c.SetUserID(l.UserID)
	}
	if l.KeyID > 0 {
		c = c.SetKeyID(l.KeyID)
	}
	if l.MappedModel != "" {
		c = c.SetMappedModel(l.MappedModel)
	}
	if l.BillingTier != "" {
		c = c.SetBillingTier(l.BillingTier)
	}
	if l.TTFTMS != nil {
		c = c.SetTtftMs(*l.TTFTMS)
	}
	if l.PriceInputMillis != nil {
		c = c.SetPriceInputMillis(*l.PriceInputMillis)
	}
	if l.PriceOutputMillis != nil {
		c = c.SetPriceOutputMillis(*l.PriceOutputMillis)
	}
	if l.PriceCacheReadMillis != nil {
		c = c.SetPriceCacheReadMillis(*l.PriceCacheReadMillis)
	}
	if l.PriceCacheCreationMillis != nil {
		c = c.SetPriceCacheCreationMillis(*l.PriceCacheCreationMillis)
	}
	if l.PricePerCallMillis != nil {
		c = c.SetPricePerCallMillis(*l.PricePerCallMillis)
	}
	return c
}

// QueryUsages usage_logs keyset 游标分页查询（用户裁决：无 from/to 的全分区
// OFFSET 扫描是压测中危——游标分页 + from/to 必填 + 零新索引，id 主键天然有序）。
// 游标语义：WHERE id < cursor AND created_at BETWEEN from/to [AND 既有过滤]，
// ORDER BY id DESC LIMIT limit+1——多取 1 行探测是否有下一页（调用方按
// len(rows) > limit 组装 next_cursor）；去 Count（Total 已从契约移除）。
func (r *UsageRepo) QueryUsages(ctx context.Context, q UsageQuery) ([]*domain.UsageLog, error) {
	pred := r.client.UsageLog.Query()
	if q.GroupID > 0 {
		pred = pred.Where(usagelog.GroupIDEQ(q.GroupID))
	}
	if q.AccountID > 0 {
		pred = pred.Where(usagelog.AccountIDEQ(q.AccountID))
	}
	if q.UserID > 0 {
		pred = pred.Where(usagelog.UserIDEQ(q.UserID))
	}
	if q.KeyID > 0 {
		pred = pred.Where(usagelog.KeyIDEQ(q.KeyID))
	}
	if q.Model != "" {
		pred = pred.Where(usagelog.ModelEQ(q.Model))
	}
	if q.Format != "" {
		pred = pred.Where(usagelog.FormatEQ(usagelog.Format(q.Format)))
	}
	if q.ErrorType != "" {
		pred = pred.Where(usagelog.ErrorTypeEQ(q.ErrorType))
	}
	if q.From != nil {
		pred = pred.Where(usagelog.CreatedAtGTE(*q.From))
	}
	if q.To != nil {
		pred = pred.Where(usagelog.CreatedAtLTE(*q.To))
	}
	if q.Cursor > 0 {
		pred = pred.Where(usagelog.IDLT(q.Cursor))
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	rows, err := pred.Order(ent.Desc(usagelog.FieldID)).Limit(q.Limit + 1).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.UsageLog, 0, len(rows))
	for _, row := range rows {
		l := &domain.UsageLog{
			ID: row.ID, RequestID: row.RequestID,
			Model: row.Model, Format: domain.RequestFormat(row.Format),
			// 用户裁决（err_logs 分表）：StatusCode/ErrorMessage 不再落 usage_logs
			// ——查询结果恒零值/nil（错误审计字段由 err_logs 承载）。
			ErrorType:                domain.ErrorType(row.ErrorType),
			LatencyMS:                row.LatencyMs,
			TTFTMS:                   row.TtftMs,
			InputTokens:              row.InputTokens,
			PriceInputMillis:         row.PriceInputMillis,
			OutputTokens:             row.OutputTokens,
			PriceOutputMillis:        row.PriceOutputMillis,
			TotalTokens:              row.TotalTokens,
			CacheReadTokens:          row.CacheReadTokens,
			PriceCacheReadMillis:     row.PriceCacheReadMillis,
			CacheCreationTokens:      row.CacheCreationTokens,
			PriceCacheCreationMillis: row.PriceCacheCreationMillis,
			CallCount:                row.CallCount,
			PricePerCallMillis:       row.PricePerCallMillis,
			Cost:                     row.Cost,
			RawCost:                  row.RawCost,
			AboveHit:                 row.AboveHit,
			Overdraft:                row.Overdraft,
			CreatedAt:                row.CreatedAt,
		}
		if row.GroupID != nil {
			l.GroupID = *row.GroupID
		}
		if row.AccountID != nil {
			l.AccountID = *row.AccountID
		}
		if row.TemplateID != nil {
			l.TemplateID = *row.TemplateID
		}
		if row.UserID != nil {
			l.UserID = *row.UserID
		}
		if row.KeyID != nil {
			l.KeyID = *row.KeyID
		}
		if row.MappedModel != nil {
			l.MappedModel = *row.MappedModel
		}
		if row.BillingTier != nil {
			l.BillingTier = *row.BillingTier
		}
		if row.ClientIP != nil {
			l.ClientIP = *row.ClientIP
		}
		out = append(out, l)
	}
	return out, nil
}

// ScanUsageAgg 批量账号 usage_logs 区间聚合（/api/admin/accounts/usage 查询面——
// 统一 usage API spec 2026-08-18）：单连接单查询，`ANY($1)` 100 ids 参数数组
// 规模内 + created_at 半开区间 [from, to)（分区键——RANGE 分区剪枝 + 既有
// account_id/created_at 索引）。SQL 侧 GROUP BY 聚合（F-P2-2 形态：服务端
// 聚合，不拉全行客户端算）；SUM 毫分 int64 原样（USD 换算在 handler 展示
// 边界）。返回 map[account_id]agg——无记录账号无键（补零由 service 层按 ids
// 全量组装）。pool 未注入（New 构造）→ 显式错误（与 StatRepo 同纪律）。
// 数量防御（N5）：>usageAggMaxAccountIDs → 显式错误（防御 handler 之外调用
// 方——ANY 参数数组规模上限）。
func (r *UsageRepo) ScanUsageAgg(ctx context.Context, accountIDs []int64, from, to time.Time) (map[int64]*domain.UsageAgg, error) {
	if len(accountIDs) > usageAggMaxAccountIDs {
		return nil, fmt.Errorf("usage repo: ScanUsageAgg: %d account ids exceed limit %d", len(accountIDs), usageAggMaxAccountIDs)
	}
	if r.pool == nil {
		return nil, fmt.Errorf("usage repo: pgx pool not configured (repository.NewWithPG); cannot scan usage agg")
	}
	// sum(bigint) → numeric，显式 ::bigint 回落（pgx 扫描 int64 不受 numeric
	// 精度语义干扰——statSummarySQL 同款）；GROUP BY 行必有行 → sum 非 NULL，
	// COALESCE 归零仅为形态防御。
	rows, err := r.pool.Query(ctx, `SELECT account_id, count(*)::bigint,
		COALESCE(sum(cost), 0)::bigint, COALESCE(sum(raw_cost), 0)::bigint,
		COALESCE(sum(total_tokens), 0)::bigint
		FROM usage_logs WHERE account_id = ANY($1) AND created_at >= $2 AND created_at < $3
		GROUP BY account_id`, accountIDs, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]*domain.UsageAgg, len(accountIDs))
	for rows.Next() {
		a := &domain.UsageAgg{}
		if err := rows.Scan(&a.AccountID, &a.Requests, &a.Cost, &a.RawCost, &a.TotalTokens); err != nil {
			return nil, err
		}
		out[a.AccountID] = a
	}
	return out, rows.Err()
}
