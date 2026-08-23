// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// 统计读取面（spec 2026-08-23 §4）：/stats/trend、/stats/top、
// /stats/entity-trend、/stats/ttft 四端点的存储查询族——全部 SQL 下推聚合
// （服务端 GROUP BY/date_trunc/percentile_cont，不拉全行客户端聚合）。
// 动态片段仅两处且均过白名单映射（unit ∈ hour|day、by ∈ cost|requests|tokens、
// entityType ∈ account|user|key）——禁字符串直插。

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/is7qin/c3api/internal/domain"
)

// statsTrendUnits 粒度白名单（unit → date_trunc 字面量；缺省归一化在 service
// 层，repo 层查表失败显式报错——双保险）。
var statsTrendUnits = map[string]string{"hour": "hour", "day": "day"}

// statTopSortKeys by 白名单（排序键 → sum 表达式；禁字符串直插）。次级排序
// entity_id ASC 固定拼接——同值 tie-break 确定性（分页/测试重放稳定）。
var statTopSortKeys = map[string]string{
	"cost":     `COALESCE(sum(cost), 0)`,
	"requests": `COALESCE(sum(request_count), 0)`,
	"tokens":   `COALESCE(sum(total_tokens), 0)`,
}

// statMeasureSums 测量列 sum 投影（trend/top/entity-trend 三查询共享列序 =
// domain.StatBucket / domain.EntityStatBucket 测量字段序）。
const statMeasureSums = `COALESCE(sum(request_count), 0)::bigint,
	COALESCE(sum(error_count), 0)::bigint,
	COALESCE(sum(input_tokens), 0)::bigint,
	COALESCE(sum(output_tokens), 0)::bigint,
	COALESCE(sum(total_tokens), 0)::bigint,
	COALESCE(sum(cache_read_tokens), 0)::bigint,
	COALESCE(sum(cache_creation_tokens), 0)::bigint,
	COALESCE(sum(cost), 0)::bigint,
	COALESCE(sum(raw_cost), 0)::bigint,
	COALESCE(sum(call_count), 0)::bigint,
	COALESCE(sum(ttft_total_ms), 0)::bigint,
	COALESCE(sum(ttft_count), 0)::bigint,
	COALESCE(max(ttft_max_ms), 0)::bigint`

// StatsTrend cube 时间趋势下推（date_trunc(unit) 服务端分组；unit ∈ hour|day；
// groupID > 0 / model 非空 = 过滤，零值 = 不过滤）。返回桶只含时间维度 + 测量
// 列（TTFTHist 恒 nil——直方图草图走 StatsTTFTSketch，趋势面不拖数组列）。
func (r *StatRepo) StatsTrend(ctx context.Context, from, to time.Time, unit string, groupID int64, model string) ([]*domain.StatBucket, error) {
	if r.pool == nil {
		return nil, fmt.Errorf("stat repo: pgx pool not configured (repository.NewWithPG); cannot query stats trend")
	}
	trunc, ok := statsTrendUnits[unit]
	if !ok {
		return nil, fmt.Errorf("stat repo: StatsTrend: unknown unit %q", unit)
	}
	sql := `SELECT date_trunc('` + trunc + `', bucket_time AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
	` + statMeasureSums + `
FROM "usage_stats" WHERE "bucket_time" >= $1 AND "bucket_time" < $2`
	args := []any{from, to}
	n := 3
	if groupID > 0 {
		sql += fmt.Sprintf(` AND "group_id" = $%d`, n)
		args = append(args, groupID)
		n++
	}
	if model != "" {
		sql += fmt.Sprintf(` AND "model" = $%d`, n)
		args = append(args, model)
	}
	sql += ` GROUP BY 1 ORDER BY 1`
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*domain.StatBucket{}
	for rows.Next() {
		b := &domain.StatBucket{}
		if err := rows.Scan(&b.BucketTime, &b.RequestCount, &b.ErrorCount, &b.InputTokens,
			&b.OutputTokens, &b.TotalTokens, &b.CacheReadTokens, &b.CacheCreationTokens,
			&b.Cost, &b.RawCost, &b.CallCount, &b.TTFTTotalMS, &b.TTFTCount, &b.TTFTMaxMS); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// StatsTop 实体排行下推（usage_entity_stats 按 entity_type 分组求和后按 by 排
// 序取前 limit；by/entityType 白名单映射，禁字符串直插）。返回桶含实体维度 +
// 测量列（BucketTime 零值——排行无时间维度）。
func (r *StatRepo) StatsTop(ctx context.Context, from, to time.Time, entityType string, by string, limit int) ([]*domain.EntityStatBucket, error) {
	if r.pool == nil {
		return nil, fmt.Errorf("stat repo: pgx pool not configured (repository.NewWithPG); cannot query stats top")
	}
	if _, ok := statEntityCols[entityType]; !ok {
		return nil, fmt.Errorf("stat repo: StatsTop: unknown entity type %q", entityType)
	}
	sortExpr, ok := statTopSortKeys[by]
	if !ok {
		return nil, fmt.Errorf("stat repo: StatsTop: unknown sort key %q", by)
	}
	sql := `SELECT entity_type, entity_id,
	` + statMeasureSums + `
FROM "usage_entity_stats" WHERE "bucket_time" >= $1 AND "bucket_time" < $2 AND "entity_type" = $3
GROUP BY entity_type, entity_id
ORDER BY ` + sortExpr + ` DESC, entity_id ASC
LIMIT $4`
	rows, err := r.pool.Query(ctx, sql, from, to, entityType, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*domain.EntityStatBucket{}
	for rows.Next() {
		b := &domain.EntityStatBucket{}
		if err := rows.Scan(&b.EntityType, &b.EntityID, &b.RequestCount, &b.ErrorCount,
			&b.InputTokens, &b.OutputTokens, &b.TotalTokens, &b.CacheReadTokens,
			&b.CacheCreationTokens, &b.Cost, &b.RawCost, &b.CallCount,
			&b.TTFTTotalMS, &b.TTFTCount, &b.TTFTMaxMS); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// StatsEntityTrend 单实体时间趋势下推（强制实体过滤 + 可选 model 过滤；unit ∈
// hour|day）。返回桶含时间维度 + 测量列（EntityType/EntityID 回填自入参——
// GROUP BY 仅时间，SQL 不回实体列）。
func (r *StatRepo) StatsEntityTrend(ctx context.Context, from, to time.Time, unit string, entityType string, entityID int64, model string) ([]*domain.EntityStatBucket, error) {
	if r.pool == nil {
		return nil, fmt.Errorf("stat repo: pgx pool not configured (repository.NewWithPG); cannot query entity trend")
	}
	trunc, ok := statsTrendUnits[unit]
	if !ok {
		return nil, fmt.Errorf("stat repo: StatsEntityTrend: unknown unit %q", unit)
	}
	if _, ok := statEntityCols[entityType]; !ok {
		return nil, fmt.Errorf("stat repo: StatsEntityTrend: unknown entity type %q", entityType)
	}
	sql := `SELECT date_trunc('` + trunc + `', bucket_time AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
	` + statMeasureSums + `
FROM "usage_entity_stats" WHERE "bucket_time" >= $1 AND "bucket_time" < $2
	AND "entity_type" = $3 AND "entity_id" = $4`
	args := []any{from, to, entityType, entityID}
	if model != "" {
		sql += ` AND "model" = $5`
		args = append(args, model)
	}
	sql += ` GROUP BY 1 ORDER BY 1`
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*domain.EntityStatBucket{}
	for rows.Next() {
		b := &domain.EntityStatBucket{EntityType: entityType, EntityID: entityID}
		if err := rows.Scan(&b.BucketTime, &b.RequestCount, &b.ErrorCount, &b.InputTokens,
			&b.OutputTokens, &b.TotalTokens, &b.CacheReadTokens, &b.CacheCreationTokens,
			&b.Cost, &b.RawCost, &b.CallCount, &b.TTFTTotalMS, &b.TTFTCount, &b.TTFTMaxMS); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// StatsTTFTSketch 平台级 TTFT 分位数草图（cube hist 服务端合并：窗口内桶数由
// service 层钳制 ≤ MaxStatsSketchBuckets；array_agg 带回逐行直方图，Go 侧
// mergeHist 逐元素合并后 TTFTPercentileMS 插值——与 overview 同一实现）。
func (r *StatRepo) StatsTTFTSketch(ctx context.Context, from, to time.Time, model string) (*domain.TTFTSummary, error) {
	if r.pool == nil {
		return nil, fmt.Errorf("stat repo: pgx pool not configured (repository.NewWithPG); cannot query ttft sketch")
	}
	sql := `SELECT COALESCE(sum(ttft_total_ms), 0)::bigint,
	COALESCE(sum(ttft_count), 0)::bigint,
	COALESCE(max(ttft_max_ms), 0)::bigint,
	COALESCE(ARRAY_AGG(ttft_hist) FILTER (WHERE ttft_hist IS NOT NULL), ARRAY[]::bigint[][])
FROM "usage_stats" WHERE "bucket_time" >= $1 AND "bucket_time" < $2`
	args := []any{from, to}
	if model != "" {
		sql += ` AND "model" = $3`
		args = append(args, model)
	}
	var total, count, maxMS int64
	var rawHist [][]int64
	if err := r.pool.QueryRow(ctx, sql, args...).Scan(&total, &count, &maxMS, &rawHist); err != nil {
		return nil, err
	}
	hist := make([]int64, len(ttftHistBounds))
	for _, h := range rawHist { // array_agg 逐行直方图合并（加法交换序无关）
		mergeHist(hist, h)
	}
	return &domain.TTFTSummary{
		Count:  count,
		AvgMS:  roundDivMS(total, count),
		P50MS:  TTFTPercentileMS(hist, count, 0.50),
		P95MS:  TTFTPercentileMS(hist, count, 0.95),
		P99MS:  TTFTPercentileMS(hist, count, 0.99),
		MaxMS:  maxMS,
		Source: "sketch",
	}, nil
}

// StatsTTFTExact 实体级 TTFT 分位数精确值（usage_logs percentile_cont 下推；
// /stats/ttft 带实体过滤分支 + 用户面 self 过滤共用）。spec §4 SQL 形状补
// sum(ttft_ms) 一列——TTFTSummary.AvgMS 需要总量（percentile_cont 不给 avg），
// 其余列序照抄。空集：percentile_cont/max 返回 NULL、count=0 → Go 侧归零值
// 结构（SQL 不加 COALESCE，保语义清晰）；Source 恒标 "exact"（空窗判空以
// Count 为准）。
func (r *StatRepo) StatsTTFTExact(ctx context.Context, from, to time.Time, entityType string, entityID int64, model string) (*domain.TTFTSummary, error) {
	if r.pool == nil {
		return nil, fmt.Errorf("stat repo: pgx pool not configured (repository.NewWithPG); cannot query ttft exact")
	}
	col, ok := statEntityCols[entityType]
	if !ok {
		return nil, fmt.Errorf("stat repo: StatsTTFTExact: unknown entity type %q", entityType)
	}
	sql := `SELECT percentile_cont(ARRAY[0.5, 0.95, 0.99]) WITHIN GROUP (ORDER BY ttft_ms),
	count(ttft_ms), COALESCE(sum(ttft_ms), 0)::bigint, max(ttft_ms)
FROM "usage_logs"
WHERE "created_at" >= $1 AND "created_at" < $2 AND "ttft_ms" IS NOT NULL AND "` + col + `" = $3`
	args := []any{from, to, entityID}
	if model != "" {
		sql += ` AND "model" = $4`
		args = append(args, model)
	}
	var (
		pp    []float64
		count int64
		total int64
		maxMS *int64
	)
	if err := r.pool.QueryRow(ctx, sql, args...).Scan(&pp, &count, &total, &maxMS); err != nil {
		return nil, err
	}
	s := &domain.TTFTSummary{Source: "exact"}
	if count == 0 || len(pp) < 3 { // 空集 NULL → pp nil（零值结构）
		return s, nil
	}
	s.Count = count
	s.AvgMS = roundDivMS(total, count)
	s.P50MS = int64(math.Round(pp[0]))
	s.P95MS = int64(math.Round(pp[1]))
	s.P99MS = int64(math.Round(pp[2]))
	if maxMS != nil {
		s.MaxMS = *maxMS
	}
	return s, nil
}

// roundDivMS TTFT 均值（查询侧 Go 除——SQL 只 sum/count；无样本 → 0；除法后
// math.Round 收敛整数毫秒，对齐 StatSummary.TTFTAvgMS 口径）。
func roundDivMS(total, count int64) int64 {
	if count <= 0 {
		return 0
	}
	return int64(math.Round(float64(total) / float64(count)))
}
