// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7qin.

package repository

// —— 原始行读取面 SQL 形状库（stat_raw_read.go 执行面的纯构造半区）——
//
// 无用户输入拼接：时区名恒 $n 绑定（handler 边界 time.LoadLocation 校验过的
// 规范 *time.Location）、粒度恒 statsTrendBucket 白名单映射、追加谓词占位符
// 编号与 rawZoneFilter.where/sql 登记序严格一致（group→model→entity，$4 起）。
// [from,to) 恒绝对时刻谓词（分区裁剪生效；显式 from/to 直透语义）。

import (
	"fmt"
	"time"
)

// statsTrendBucket unit 白名单：hour 用减法、day 用 date_trunc（理由见
// rawBucketExpr）。键集与 statsTrendUnits 一致。
var statsTrendBucket = map[string]bool{"hour": true, "day": true}

// zoneName 绑进 AT TIME ZONE $n 的规范时区名（nil = UTC）。
func zoneName(zone *time.Location) string {
	if zone == nil {
		return "UTC"
	}
	return zone.String()
}

// locOrUTC 读面入口归一（time.Time.In(nil) panic——直接构造/旧调用零值兜底）。
func locOrUTC(zone *time.Location) *time.Location {
	if zone == nil {
		return time.UTC
	}
	return zone
}

// rawBucketExpr 原始行本地桶起点表达式（按粒度二选一）：
//
//	hour = created_at − 该行本地墙钟"时内已过秒数"（wall-epoch mod 3600 减法）。
//	       减法对每个绝对时刻产出唯一本地整点起点，DST fall-back 的重复墙钟
//	       小时（EDT 01:xx 与 EST 01:xx）落到两个不同绝对桶（05:00Z / 06:00Z），
//	       **不塌缩**——避开 date_trunc('hour') AT TIME ZONE 对歧义墙钟的
//	       "归第一次出现"合并（会静默吞掉后一小时的行）。契约以 RFC3339 offset
//	       区分两桶（选定的精确行为，OpenAPI 与本注释文档化）。spring-forward
//	       缺失小时无行、自然无桶。半小时偏移（Kolkata :30）下界 = UTC :30，
//	       卷积表无法表达的形态在此逐行精确。
//	day  = date_trunc('day', created_at AT TIME ZONE zone) AT TIME ZONE zone。
//	       本地午夜在一个日历日内唯一（目标时区午夜跳表形态罕见且 PG 消解
//	       一致），date_trunc 把本地日全部绝对时刻（含 fall-back 重复小时的
//	       两段）精确归到同一本地午夜起点。此处绝不能用减法：fall-back 日
//	       减法对第二次出现的墙钟小时偏一小时，产出非午夜的假日界起点。
func rawBucketExpr(col, zoneParam, unit string) string {
	if unit == "day" {
		return "date_trunc('day', " + col + " AT TIME ZONE " + zoneParam + ") AT TIME ZONE " + zoneParam
	}
	return fmt.Sprintf("%s - ((extract(epoch from %s AT TIME ZONE %s) - floor(extract(epoch from %s AT TIME ZONE %s) / 3600) * 3600) * interval '1 second')",
		col, col, zoneParam, col, zoneParam)
}

// rawUsageMeasures usage_logs 原始行测量投影——列序与 statMeasureSums（cube 读
// 面 13 列）逐位对齐：rc/ec/in/out/tot/cr/cc/cost/raw/call/ttftS/ttftC/ttftM。
// 表达式照抄写侧 aggUsageSQL（COALESCE 布点一致——NOT NULL 列 sum 恒非 NULL，
// 仅防御性对齐）。
const rawUsageMeasures = `count(*),
	count(*) FILTER (WHERE error_type <> 'none'),
	sum(input_tokens), sum(output_tokens), sum(total_tokens),
	sum(cache_read_tokens), sum(cache_creation_tokens), sum(cost), COALESCE(sum(raw_cost), 0),
	sum(call_count),
	COALESCE(sum(ttft_ms), 0), count(ttft_ms), COALESCE(max(ttft_ms), 0)`

// rawErrMeasures err_logs 瘦表测量补位（11 个恒零列，列序同 rawUsageMeasures；
// rc/ec 真计数）。
const rawErrMeasures = `count(*),
	count(*) FILTER (WHERE error_type <> 'none'),
	0::bigint, 0::bigint, 0::bigint, 0::bigint, 0::bigint, 0::bigint, 0::bigint,
	0::bigint, 0::bigint, 0::bigint, 0::bigint`

// rawHistZero 直方图零数组（err 源补位；档位形状与 aggHistExpr 钉死一致）。
const rawHistZero = `ARRAY[0::bigint, 0, 0, 0, 0, 0, 0, 0, 0, 0]`

// rawZoneFilter 原始读共享过滤（$1=from $2=to $3=zone 之后的追加谓词；全部
// 参数化绑定）。entityCol 恒来自 statEntityCols 白名单值域（调用方查表后传入，
// "" = 无实体过滤）。
type rawZoneFilter struct {
	groupID   int64
	model     string
	entityCol string
	entityID  int64
}

// where 登记追加过滤参数（占位符次序与 sql() 一致：group → model → entity）。
func (f rawZoneFilter) where(args *[]any) {
	if f.groupID > 0 {
		*args = append(*args, f.groupID)
	}
	if f.model != "" {
		*args = append(*args, f.model)
	}
	if f.entityCol != "" {
		*args = append(*args, f.entityID)
	}
}

// sql 拼接追加谓词片段（$4 起编号恒按 where 的登记序；列名来自白名单映射
// 值域，值全绑定参数）。
func (f rawZoneFilter) sql() string {
	out := ""
	n := 4
	if f.groupID > 0 {
		out += fmt.Sprintf(` AND "group_id" = $%d`, n)
		n++
	}
	if f.model != "" {
		out += fmt.Sprintf(` AND "model" = $%d`, n)
		n++
	}
	if f.entityCol != "" {
		out += fmt.Sprintf(` AND "%s" = $%d`, f.entityCol, n)
	}
	return out
}

// rawBucketRow 原始行分组扫描行（bucket + rawUsageMeasures 13 列——列序与
// statMeasureSums 对齐）。
type rawBucketRow struct {
	bt                                                                    time.Time
	req, errN, in, out, tot, cr, cc, cost, raw, call, ttftS, ttftC, ttftM int64
}

// rawTrendSQLs 趋势/实体趋势共享双源 SQL（usage 全测量 + errlog 计数补充；
// 行级语义与 cube 写侧 aggUsageSQL/aggErrLogSQL 逐位一致）。
func rawTrendSQLs(bucket, where string) (string, string) {
	return `SELECT ` + bucket + `, ` + rawUsageMeasures + ` FROM usage_logs WHERE ` + where + ` AND error_type IN ('none', 'abort') GROUP BY 1 ORDER BY 1`,
		`SELECT ` + bucket + `, ` + rawErrMeasures + ` FROM err_logs WHERE ` + where + ` AND error_type <> 'abort' GROUP BY 1 ORDER BY 1`
}

// rawTrendArgs 组装 [from, to, zone, filters...] 绑定序列（$1..$3 固定，
// 过滤位续 $4+，与 rawZoneFilter.sql 编号一致）。
func rawTrendArgs(from, to time.Time, zone *time.Location, f rawZoneFilter) ([]any, string) {
	args := []any{from, to, zoneName(zone)}
	f.where(&args)
	return args, "created_at >= $1 AND created_at < $2" + f.sql()
}
