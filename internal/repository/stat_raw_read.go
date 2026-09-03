// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7qin.

package repository

// —— 原始行读取面：浏览器时区精确分组（request-browser-timezone-stats 2026-09-03）——
//
// usage_stats / usage_entity_stats 卷积表小时桶界恒规范 UTC。请求浏览器时区无法
// 由卷积行精确重组的场景（domain.ZoneCubeExact == false：窗口内 DST 偏移漂移、
// 或 :30/:45 半小时偏移劈开小时行）下，分组读取改从 usage_logs + err_logs 原始
// 行直接聚合，逐行语义与 cube 写侧两查询（aggUsageSQL/aggErrLogSQL）完全一致：
//   - usage_logs：error_type IN ('none','abort') 放行行全测量（rc=count(*)、
//     ec=FILTER(<>'none')、tokens×5/cost/raw/call/TTFT sum/count/max/hist）；
//   - err_logs：error_type <> 'abort'（abort 已由 usage 源全字段计，防双计），
//     rc=count(*)（含豁免非错误行）、ec=FILTER(<>'none')，测量列恒 0（瘦表）；
//   - 跨源同桶 Go 侧累加合并（max 取大），与 LoadAggRange 同一合并纪律。
//
// 桶 = 本地桶起点的绝对时刻（hour 减法不塌缩 fall-back 重复小时；day
// date_trunc 精确归日——表达式论证见 stat_raw_expr.go rawBucketExpr）。
// 白名单/绑定纪律与 [from,to) 绝对谓词同见 expr 半区。
//
// 窗口上限：本路径受原始行保留期约束（usage.log_retention_days 默认 30d、
// usage.errlog_retention_days 默认 7d）。service 层对非 cube 精确窗口（时区
// 不精确或界劈开卷积行）按部署配置换算的 horizon（缺省 MaxStatsRawSpan 8d）
// 强制上限，宁可 400 也不静默返回残缺桶。
//
// 本文件只放执行面（pool 查询 + Go 合并/排序/回落）；SQL 形状构造在
// stat_raw_expr.go。

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/is7qin/c3api/internal/domain"
)

// scanRawBuckets 执行双源桶 SQL 逐行回调（跨源合并由回调自持——与
// LoadAggRange 同一合并纪律）。
func (r *StatRepo) scanRawBuckets(ctx context.Context, args []any, usageSQL, errSQL string, row func(rawBucketRow) error) error {
	for _, sql := range []string{usageSQL, errSQL} {
		rows, err := r.pool.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var x rawBucketRow
			if err := rows.Scan(&x.bt, &x.req, &x.errN, &x.in, &x.out, &x.tot, &x.cr, &x.cc,
				&x.cost, &x.raw, &x.call, &x.ttftS, &x.ttftC, &x.ttftM); err != nil {
				rows.Close()
				return err
			}
			if err := row(x); err != nil {
				rows.Close()
				return err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// rawTrend 原始行时间趋势（StatsTrend 非精确时区路径；unit ∈ hour|day）。返回
// 桶只含时间维度 + 测量列（TTFTHist nil——与 cube 趋势面同形），按桶升序，
// 时刻 .In(zone)（墙钟分量 = 请求时区）。
func (r *StatRepo) rawTrend(ctx context.Context, from, to time.Time, unit string, f rawZoneFilter, zone *time.Location) ([]*domain.StatBucket, error) {
	if _, ok := statsTrendBucket[unit]; !ok {
		return nil, fmt.Errorf("stat repo: rawTrend: unknown unit %q", unit)
	}
	args, where := rawTrendArgs(from, to, zone, f)
	usageSQL, errSQL := rawTrendSQLs(rawBucketExpr("created_at", "$3", unit), where)
	merged := make(map[time.Time]*domain.StatBucket)
	err := r.scanRawBuckets(ctx, args, usageSQL, errSQL, func(x rawBucketRow) error {
		b := &domain.StatBucket{
			BucketTime: x.bt, RequestCount: x.req, ErrorCount: x.errN,
			InputTokens: x.in, OutputTokens: x.out, TotalTokens: x.tot,
			CacheReadTokens: x.cr, CacheCreationTokens: x.cc, Cost: x.cost, RawCost: x.raw, CallCount: x.call,
			TTFTTotalMS: x.ttftS, TTFTCount: x.ttftC, TTFTMaxMS: x.ttftM,
		}
		if m, ok := merged[x.bt.UTC()]; ok {
			mergeAggRow(m, b)
		} else {
			merged[x.bt.UTC()] = b
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]*domain.StatBucket, 0, len(merged))
	for _, b := range merged {
		b.TTFTHist = nil // 趋势面不拖数组列（cube 路径同款形态）
		b.BucketTime = b.BucketTime.In(zone)
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BucketTime.Before(out[j].BucketTime) })
	return out, nil
}

// rawEntityTrend 原始行单实体时间趋势（StatsEntityTrend 非精确时区路径）。
// entityCol 白名单已在调用方查表（statEntityCols）；EntityType/EntityID 回填
// 自入参（与 cube 路径同形）。
func (r *StatRepo) rawEntityTrend(ctx context.Context, from, to time.Time, unit, entityType string, entityID int64, f rawZoneFilter, zone *time.Location) ([]*domain.EntityStatBucket, error) {
	if _, ok := statsTrendBucket[unit]; !ok {
		return nil, fmt.Errorf("stat repo: rawEntityTrend: unknown unit %q", unit)
	}
	args, where := rawTrendArgs(from, to, zone, f)
	usageSQL, errSQL := rawTrendSQLs(rawBucketExpr("created_at", "$3", unit), where)
	merged := make(map[time.Time]*domain.EntityStatBucket)
	err := r.scanRawBuckets(ctx, args, usageSQL, errSQL, func(x rawBucketRow) error {
		b := &domain.EntityStatBucket{
			BucketTime: x.bt, EntityType: entityType, EntityID: entityID,
			RequestCount: x.req, ErrorCount: x.errN,
			InputTokens: x.in, OutputTokens: x.out, TotalTokens: x.tot,
			CacheReadTokens: x.cr, CacheCreationTokens: x.cc, Cost: x.cost, RawCost: x.raw, CallCount: x.call,
			TTFTTotalMS: x.ttftS, TTFTCount: x.ttftC, TTFTMaxMS: x.ttftM,
		}
		if m, ok := merged[x.bt.UTC()]; ok {
			mergeEntityRow(m, b)
		} else {
			merged[x.bt.UTC()] = b
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]*domain.EntityStatBucket, 0, len(merged))
	for _, b := range merged {
		b.BucketTime = b.BucketTime.In(zone)
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BucketTime.Before(out[j].BucketTime) })
	return out, nil
}

// rawSummary 原始行区间单行聚合（SummarizeStats 非精确时区路径；无分组——
// 本地边界劈开 cube 小时行时的精确替代）。两源单行 → Go 合并（hist 逐元素）。
func (r *StatRepo) rawSummary(ctx context.Context, from, to time.Time, groupID int64) (*StatSummary, error) {
	s := &StatSummary{}
	{
		// 空区间 sum(bigint) → NULL、扫描进 int64 即报错——每个 SUM 恒 COALESCE
		// 归零（与 cube 读面 statSummarySQL 同纪律），裸行路径"无数据 = 零值
		// 结构"是调用方（overview summary）依赖的契约。
		sql := `SELECT count(*), count(*) FILTER (WHERE error_type <> 'none'),
	COALESCE(sum(input_tokens), 0), COALESCE(sum(output_tokens), 0), COALESCE(sum(total_tokens), 0),
	COALESCE(sum(cache_read_tokens), 0), COALESCE(sum(cost), 0), COALESCE(sum(raw_cost), 0),
	COALESCE(sum(call_count), 0),
	COALESCE(sum(ttft_ms), 0), count(ttft_ms), COALESCE(max(ttft_ms), 0), ` + aggHistExpr + `
FROM usage_logs WHERE created_at >= $1 AND created_at < $2 AND error_type IN ('none', 'abort')`
		args := []any{from, to}
		if groupID > 0 {
			sql += ` AND "group_id" = $3`
			args = append(args, groupID)
		}
		var hist []int64
		if err := r.pool.QueryRow(ctx, sql, args...).Scan(&s.Requests, &s.Errors,
			&s.InputTokens, &s.OutputTokens, &s.TotalTokens, &s.CacheReadTokens,
			&s.Cost, &s.RawCost, &s.CallCount,
			&s.TTFTTotalMS, &s.TTFTCount, &s.TTFTMaxMS, &hist); err != nil {
			return nil, err
		}
		s.TTFTHist = make([]int64, len(ttftHistBounds))
		mergeHist(s.TTFTHist, hist)
	}
	{
		sql := `SELECT count(*), count(*) FILTER (WHERE error_type <> 'none')
FROM err_logs WHERE created_at >= $1 AND created_at < $2 AND error_type <> 'abort'`
		args := []any{from, to}
		if groupID > 0 {
			sql += ` AND "group_id" = $3`
			args = append(args, groupID)
		}
		var req, errN int64
		if err := r.pool.QueryRow(ctx, sql, args...).Scan(&req, &errN); err != nil {
			return nil, err
		}
		s.Requests += req
		s.Errors += errN
	}
	return s, nil
}

// rawScanStatsDays 原始行日桶聚合（ScanStatsDays 非精确时区路径；overview
// trend 精确形态：直方图逐桶 aggHistExpr，跨源累加，桶时刻 .In(zone)）。
func (r *StatRepo) rawScanStatsDays(ctx context.Context, from, to time.Time, groupID int64, zone *time.Location) ([]*StatDayAgg, error) {
	bucket := rawBucketExpr("created_at", "$3", "day")
	args := []any{from, to, zoneName(zone)}
	where := "created_at >= $1 AND created_at < $2"
	if groupID > 0 {
		where += ` AND "group_id" = $4`
		args = append(args, groupID)
	}
	merged := make(map[time.Time]*StatDayAgg)
	usageSQL := `SELECT ` + bucket + `,
	count(*), count(*) FILTER (WHERE error_type <> 'none'), sum(total_tokens),
	sum(cost), COALESCE(sum(raw_cost), 0), sum(call_count),
	COALESCE(sum(ttft_ms), 0), count(ttft_ms), COALESCE(max(ttft_ms), 0), ` + aggHistExpr + `
FROM usage_logs WHERE ` + where + ` AND error_type IN ('none', 'abort') GROUP BY 1 ORDER BY 1`
	errSQL := `SELECT ` + bucket + `,
	count(*), count(*) FILTER (WHERE error_type <> 'none'), 0::bigint,
	0::bigint, 0::bigint, 0::bigint,
	0::bigint, 0::bigint, 0::bigint, ` + rawHistZero + `
FROM err_logs WHERE ` + where + ` AND error_type <> 'abort' GROUP BY 1 ORDER BY 1`
	for _, sql := range []string{usageSQL, errSQL} {
		rows, err := r.pool.Query(ctx, sql, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			d := &StatDayAgg{}
			var hist []int64
			if err := rows.Scan(&d.Date, &d.Requests, &d.Errors, &d.Tokens,
				&d.Cost, &d.RawCost, &d.CallCount,
				&d.TTFTTotalMS, &d.TTFTCount, &d.TTFTMaxMS, &hist); err != nil {
				rows.Close()
				return nil, err
			}
			if m, ok := merged[d.Date.UTC()]; ok {
				m.Requests += d.Requests
				m.Errors += d.Errors
				m.Tokens += d.Tokens
				m.Cost += d.Cost
				m.RawCost += d.RawCost
				m.CallCount += d.CallCount
				m.TTFTTotalMS += d.TTFTTotalMS
				m.TTFTCount += d.TTFTCount
				if d.TTFTMaxMS > m.TTFTMaxMS {
					m.TTFTMaxMS = d.TTFTMaxMS
				}
				mergeHist(m.TTFTHist, hist)
			} else {
				d.TTFTHist = make([]int64, len(ttftHistBounds))
				mergeHist(d.TTFTHist, hist)
				merged[d.Date.UTC()] = d
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	out := make([]*StatDayAgg, 0, len(merged))
	for _, d := range merged {
		d.Date = d.Date.In(zone)
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	return out, nil
}
