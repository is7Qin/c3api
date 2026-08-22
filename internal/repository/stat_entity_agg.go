// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// 实体小时卷积聚合（spec 2026-08-23 §3.2）：usage_entity_stats 只由离线聚合
// worker 写入——三实体类型（account/user/key）× 两源（usage_logs 全字段 +
// err_logs 仅 error_count）共六查询，GROUP BY hour × entity × model。实体视角
// 无 group 维（路由概念仅 cube）；无 hist 列（实体分位数走 StatsTTFTExact 的
// percentile_cont 精确路径）。

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/is7qin/c3api/internal/domain"
)

// 实体类型字面量与 entity_col 映射（聚合 WHERE/SELECT 与读取面过滤共用单一
// 事实源；entity_type 字面量恒由 Go 侧白名单绑定，禁字符串直插 SQL）。
const (
	statEntityAccount = "account"
	statEntityUser    = "user"
	statEntityKey     = "key"
)

// statEntityCols entityType → usage_logs/err_logs 归属列。未知类型查表失败即
// 显式报错（不 fallback、不拼接）。
var statEntityCols = map[string]string{
	statEntityAccount: "account_id",
	statEntityUser:    "user_id",
	statEntityKey:     "key_id",
}

// aggEntityDimCols 实体六查询共享维度列前缀（%[1]s = entity_col）。**WHERE 加
// <entity_col> IS NOT NULL**——0 = 无主行（鉴权失败/无 key 等）不入卷积表，
// 否则会伪造"ID=0 的实体"；这与 cube 的 COALESCE(<col>,0) 有意不对称：cube 是
// 平台总卷（无主行归 0 组保留口径完整），实体表按 ID 钻取、ID=0 不是合法实体。
var aggEntityDimCols = `date_trunc('hour', created_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
       COALESCE(%[1]s, 0), COALESCE(model, '')`

// aggEntityUsageSQLTpl usage_logs → 实体桶（全字段同 cube 单扫描口径：
// rc=count(*)、ec=FILTER(<>'none')、测量列全量 sum；WHERE error_type IN
// ('none','abort') 放行行含 abort）。
var aggEntityUsageSQLTpl = `SELECT ` + aggEntityDimCols + `,
	count(*), count(*) FILTER (WHERE error_type <> 'none'), sum(call_count),
	sum(input_tokens), sum(output_tokens), sum(total_tokens),
	sum(cache_read_tokens), sum(cache_creation_tokens),
	sum(cost), COALESCE(sum(raw_cost), 0),
	COALESCE(sum(ttft_ms), 0), count(ttft_ms), COALESCE(max(ttft_ms), 0)
FROM usage_logs
WHERE created_at >= $1 AND created_at < $2 AND error_type IN ('none', 'abort')
	AND %[1]s IS NOT NULL
GROUP BY 1, 2, 3`

// aggEntityErrLogSQLTpl err_logs → 实体桶补充（仅两计数列；口径照抄 cube
// aggErrLogSQL：rc=count(*) 含豁免非错误行、ec=FILTER(<>'none')、WHERE
// error_type <> 'abort' 防双计——abort 行已由 usage 源全字段计）。
var aggEntityErrLogSQLTpl = `SELECT ` + aggEntityDimCols + `,
	count(*), count(*) FILTER (WHERE error_type <> 'none')
FROM err_logs
WHERE created_at >= $1 AND created_at < $2 AND error_type <> 'abort'
	AND %[1]s IS NOT NULL
GROUP BY 1, 2, 3`

// aggEntityUsageSQLs / aggEntityErrLogSQLs 六查询 SQL 常量族（包初始化期按
// 白名单展开一次；键集 = statEntityCols 键集）。
var (
	aggEntityUsageSQLs  = map[string]string{}
	aggEntityErrLogSQLs = map[string]string{}
)

func init() {
	for et, col := range statEntityCols {
		aggEntityUsageSQLs[et] = fmt.Sprintf(aggEntityUsageSQLTpl, col)
		aggEntityErrLogSQLs[et] = fmt.Sprintf(aggEntityErrLogSQLTpl, col)
	}
}

// entityAggKey 实体桶合并键（跨源撞 key 判定：usage_logs 行与 err_logs 行同
// 维度可撞——合并累加；排序键 = 唯一索引列序）。
type entityAggKey struct {
	bucketTime time.Time
	entityType string
	entityID   int64
	model      string
}

func entityKeyOf(b *domain.EntityStatBucket) entityAggKey {
	return entityAggKey{bucketTime: b.BucketTime, entityType: b.EntityType,
		entityID: b.EntityID, model: b.Model}
}

// mergeEntityRow 跨源同 key 桶测量列累加（TTFT max 取大）。
func mergeEntityRow(dst, src *domain.EntityStatBucket) {
	dst.RequestCount += src.RequestCount
	dst.ErrorCount += src.ErrorCount
	dst.CallCount += src.CallCount
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	dst.TotalTokens += src.TotalTokens
	dst.CacheReadTokens += src.CacheReadTokens
	dst.CacheCreationTokens += src.CacheCreationTokens
	dst.Cost += src.Cost
	dst.RawCost += src.RawCost
	dst.TTFTTotalMS += src.TTFTTotalMS
	dst.TTFTCount += src.TTFTCount
	if src.TTFTMaxMS > dst.TTFTMaxMS {
		dst.TTFTMaxMS = src.TTFTMaxMS
	}
}

// lessEntityStatBucket 桶确定性排序（排序键 = 唯一索引列序 bucket_time,
// entity_type, entity_id, model）。
func lessEntityStatBucket(a, b *domain.EntityStatBucket) bool {
	if !a.BucketTime.Equal(b.BucketTime) {
		return a.BucketTime.Before(b.BucketTime)
	}
	if a.EntityType != b.EntityType {
		return a.EntityType < b.EntityType
	}
	if a.EntityID != b.EntityID {
		return a.EntityID < b.EntityID
	}
	return a.Model < b.Model
}

// loadEntityAggRange 实体六查询 + 双源合并（LoadAggRange 的 entity 半区；
// detailRows 不计实体查询——扫的是 cube 同批源行，重复计数会虚增观测口径）。
func (r *StatRepo) loadEntityAggRange(ctx context.Context, from, to time.Time) ([]*domain.EntityStatBucket, error) {
	merged := make(map[entityAggKey]*domain.EntityStatBucket)
	for _, et := range []string{statEntityAccount, statEntityUser, statEntityKey} {
		if err := r.scanEntityUsageRows(ctx, aggEntityUsageSQLs[et], et, from, to, merged); err != nil {
			return nil, err
		}
		if err := r.scanEntityErrRows(ctx, aggEntityErrLogSQLs[et], et, from, to, merged); err != nil {
			return nil, err
		}
	}
	out := make([]*domain.EntityStatBucket, 0, len(merged))
	for _, b := range merged {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return lessEntityStatBucket(out[i], out[j]) })
	return out, nil
}

// upsertEntity 同 key 桶合并累加（跨源撞 key：usage 行 vs errlog 行）。
func upsertEntity(merged map[entityAggKey]*domain.EntityStatBucket, b *domain.EntityStatBucket) {
	key := entityKeyOf(b)
	if m2, ok := merged[key]; ok {
		mergeEntityRow(m2, b)
	} else {
		merged[key] = b
	}
}

// scanEntityUsageRows usage 源扫描（列序 = 维度 3 + 计数 2 + call + tokens×5 +
// cost/raw + TTFT×3；pgx Scan 需逐行独立地址）。
func (r *StatRepo) scanEntityUsageRows(ctx context.Context, sql, entityType string, from, to time.Time, merged map[entityAggKey]*domain.EntityStatBucket) error {
	rows, err := r.pool.Query(ctx, sql, from, to)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			bt                                                                    time.Time
			eid                                                                   int64
			model                                                                 string
			req, errN, call, in, out, tot, cr, cc, cost, raw, ttftS, ttftC, ttftM int64
		)
		if err := rows.Scan(&bt, &eid, &model, &req, &errN, &call, &in, &out, &tot,
			&cr, &cc, &cost, &raw, &ttftS, &ttftC, &ttftM); err != nil {
			return err
		}
		upsertEntity(merged, &domain.EntityStatBucket{
			BucketTime: bt, EntityType: entityType, EntityID: eid, Model: model,
			RequestCount: req, ErrorCount: errN, CallCount: call,
			InputTokens: in, OutputTokens: out, TotalTokens: tot,
			CacheReadTokens: cr, CacheCreationTokens: cc, Cost: cost, RawCost: raw,
			TTFTTotalMS: ttftS, TTFTCount: ttftC, TTFTMaxMS: ttftM,
		})
	}
	return rows.Err()
}

// scanEntityErrRows errlog 源扫描（仅两计数列——瘦表无测量列，恒零值入桶）。
func (r *StatRepo) scanEntityErrRows(ctx context.Context, sql, entityType string, from, to time.Time, merged map[entityAggKey]*domain.EntityStatBucket) error {
	rows, err := r.pool.Query(ctx, sql, from, to)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var bt time.Time
		var eid int64
		var model string
		var req, errN int64
		if err := rows.Scan(&bt, &eid, &model, &req, &errN); err != nil {
			return err
		}
		upsertEntity(merged, &domain.EntityStatBucket{
			BucketTime: bt, EntityType: entityType, EntityID: eid, Model: model,
			RequestCount: req, ErrorCount: errN,
		})
	}
	return rows.Err()
}

// usageEntityStatsInsertCols 实体桶 INSERT 列（与 usage_entity_stats DDL v2.1
// 列序一致；不含 id——DEFAULT nextval；无 hist 列）。
var usageEntityStatsInsertCols = []string{
	"bucket_time", "entity_type", "entity_id", "model",
	"request_count", "error_count", "call_count",
	"input_tokens", "output_tokens", "total_tokens",
	"cache_read_tokens", "cache_creation_tokens", "cost", "raw_cost",
	"ttft_total_ms", "ttft_count", "ttft_max_ms", "updated_at",
}

// insertEntityStatBuckets 参数化批量 INSERT（entity；列序 =
// usageEntityStatsInsertCols；18 列 × 500 ≈ 9k 参数，PG 上限安全）。
func insertEntityStatBuckets(ctx context.Context, tx pgx.Tx, rows []*domain.EntityStatBucket, now time.Time) error {
	argRows := make([][]any, len(rows))
	for i, b := range rows {
		argRows[i] = []any{
			b.BucketTime, b.EntityType, b.EntityID, b.Model,
			b.RequestCount, b.ErrorCount, b.CallCount,
			b.InputTokens, b.OutputTokens, b.TotalTokens,
			b.CacheReadTokens, b.CacheCreationTokens, b.Cost, b.RawCost,
			b.TTFTTotalMS, b.TTFTCount, b.TTFTMaxMS, now,
		}
	}
	return batchInsertExec(ctx, tx, "usage_entity_stats", usageEntityStatsInsertCols, argRows)
}
