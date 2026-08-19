// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// 列事实源锚（align 补列机制删除后保留的防漂移职责，评审 I-1）：建表 DDL 与
// 列定义事实源（usageLogColumnDefs/errLogColumnDefs/usageStatsColumnDefs）的
// 列集合必须一致——任一同步点被绕过/手改立即被本文件锚测试捕获（防"向静态
// DDL 加列忘加事实源"漂移，含类型漂移——列定义字符串整体生成）。列集合的
// 在场/取反断言是列集合演化的自动化防线。

import (
	"errors"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/ent/usagelog"
)

// ddlColumnNames 提取建表 DDL 的列名（行首标识符，跳过 CREATE/PRIMARY 非列
// 行）与补列 ALTER 的列名（IF NOT EXISTS 后标识符），排序后返回。
func ddlColumnNames(ddls ...string) []string {
	lineRe := regexp.MustCompile(`^\s*([a-z_][a-z0-9_]*)\b`)
	alterRe := regexp.MustCompile(`ADD COLUMN IF NOT EXISTS ([a-z_][a-z0-9_]*)`)
	var names []string
	for _, ddl := range ddls {
		if m := alterRe.FindStringSubmatch(ddl); m != nil {
			names = append(names, m[1])
			continue
		}
		for _, line := range strings.Split(ddl, "\n") {
			m := lineRe.FindStringSubmatch(line)
			if m == nil || m[1] == "create" || m[1] == "primary" {
				continue
			}
			names = append(names, m[1])
		}
	}
	sort.Strings(names)
	return names
}

func TestUsageLogColumnDefsMatchCreateDDL(t *testing.T) {
	source := ddlColumnNames(strings.Join(usageLogColumnDefs, "\n"))
	create := ddlColumnNames(usageLogCreateDDL)
	require.NotEmpty(t, source, "事实源列集合非空")
	require.Equal(t, source, create, "建表 DDL 与事实源列集合一致")
	require.Contains(t, source, "price_input_millis", "锚必须覆盖 P1 缺列（快照列）")
	// 统一计费模型（spec 2026-08-13）：删 6 加 2——新列锚 + 旧列取反断言
	//（任何同步点残留图片 6 列即红）。
	require.Contains(t, source, "call_count", "锚必须覆盖 call_count 新列")
	require.Contains(t, source, "price_per_call_millis", "锚必须覆盖 price_per_call_millis 新列")
	// raw_cost（spec 2026-08-18）：倍率前原始成本列锚（建表 DDL 事实源）。
	require.Contains(t, source, "raw_cost", "锚必须覆盖 raw_cost 新列")
	for _, old := range []string{"image_input_tokens", "image_output_tokens", "image_count",
		"price_image_input_millis", "price_image_output_millis", "price_per_image_millis"} {
		require.NotContains(t, source, old, "删 6 列：%s 不得残留", old)
	}
}

// TestUsageLogCopyColumnsMatchColumnDefs COPY 列集合锚（spec 2026-08-18 gate
// 修订）：COPY 列清单（usageLogCopyColumns——COPY 事实源）与建表 DDL 数据列
// 集合一致（除 id 自增列——COPY 不写 id，序列默认生成）；列数 29→30 锚定。
// raw_cost 紧随 cost（两事实源列序同向防漂移）。
func TestUsageLogCopyColumnsMatchColumnDefs(t *testing.T) {
	source := ddlColumnNames(strings.Join(usageLogColumnDefs, "\n"))
	require.Len(t, usageLogCopyColumns, 30, "COPY 列数 29→30")
	want := make([]string, 0, len(source)-1)
	for _, s := range source {
		if s != "id" { // id 为自增列（COPY 不写，序列默认生成）
			want = append(want, s)
		}
	}
	got := append([]string(nil), usageLogCopyColumns...)
	sort.Strings(want)
	sort.Strings(got)
	require.Equal(t, want, got, "COPY 列集合 = 建表 DDL 数据列集合（除 id）")
	for i, c := range usageLogCopyColumns {
		if c == usagelog.FieldCost {
			require.Equal(t, usagelog.FieldRawCost, usageLogCopyColumns[i+1], "raw_cost 紧随 cost")
			return
		}
	}
	t.Fatal("COPY 列清单缺少 cost 列")
}

// TestErrLogColumnDefsMatchCreateDDL err_logs 列事实源锚（架构审查 S2——
// errLogColumnDefs 是第二列事实源，建表 DDL 与事实源列集合一致；防"向静态
// DDL 加列忘加事实源"）。
func TestErrLogColumnDefsMatchCreateDDL(t *testing.T) {
	source := ddlColumnNames(strings.Join(errLogColumnDefs, "\n"))
	create := ddlColumnNames(errLogCreateDDL)
	require.NotEmpty(t, source, "事实源列集合非空")
	require.Equal(t, source, create, "建表 DDL 与事实源列集合一致")
	require.Contains(t, source, "error_message", "锚必须覆盖 err_logs 错误审计列")
	require.Contains(t, source, "billing_tier", "锚必须覆盖 I-3 tier 审计列")
	require.NotContains(t, source, "cost", "err_logs 瘦表无计费列")
	require.NotContains(t, source, "input_tokens", "err_logs 瘦表无 token 列")
}

// TestUsageStatsColumnDefsMatchCreateDDL usage_stats 列事实源锚（用户裁决
// 2026-08-11 三表统一分区机制——usageStatsColumnDefs 第三列事实源，建表 DDL
// 与事实源列集合一致；防 P1 同型复发）→ spec 2026-08-14 表重建：删
// total_latency_ms、加 call_count/ttft_* 六列 + bigint[] 数组列。
func TestUsageStatsColumnDefsMatchCreateDDL(t *testing.T) {
	source := ddlColumnNames(strings.Join(usageStatsColumnDefs, "\n"))
	create := ddlColumnNames(usageStatsCreateDDL)
	require.NotEmpty(t, source, "事实源列集合非空")
	require.Equal(t, source, create, "建表 DDL 与事实源列集合一致")
	require.Contains(t, source, "bucket_time", "锚必须覆盖分区键 bucket_time")
	require.Contains(t, source, "cost", "锚必须覆盖计费预聚合列")
	require.Contains(t, source, "call_count", "表重建必须含按次调用列")
	require.Contains(t, source, "ttft_hist", "表重建必须含 TTFT 直方图数组列（ent carve-out）")
	require.NotContains(t, source, "total_latency_ms", "表重建删除总延迟列（TTFT 直方图替代）")
}

// TestStatsAggColumnAlignment usage_stats 聚合/扫描列序锚（spec 2026-08-19：
// 列序四段对齐——SELECT → rows.Scan → 赋值 → INSERT，raw 恒在 cost 后）：
//   - statsAggInsertCols = 建表 DDL 数据列集合（除 id 自增列）+ raw_cost 紧随
//     cost（INSERT 列序与 DDL 同向）；
//   - 三聚合 SQL/扫描 SQL 字符串级锚 raw 紧随 cost（防 SELECT/Scan 错位）。
func TestStatsAggColumnAlignment(t *testing.T) {
	source := ddlColumnNames(strings.Join(usageStatsColumnDefs, "\n"))
	want := make([]string, 0, len(source)-1)
	for _, s := range source {
		if s != "id" { // id 自增列 INSERT 不带（DEFAULT nextval）
			want = append(want, s)
		}
	}
	got := append([]string(nil), statsAggInsertCols...)
	sort.Strings(got)
	require.Equal(t, want, got, "statsAggInsertCols 集合 = 建表 DDL 数据列集合（除 id）")
	for i, c := range statsAggInsertCols {
		if c == "cost" {
			require.Equal(t, "raw_cost", statsAggInsertCols[i+1], "INSERT 列序 raw_cost 紧随 cost")
			break
		}
	}
	// 事实源内序锚（与 INSERT 同向）：usageStatsColumnDefs 的 raw_cost 恒在 cost 后
	for i, c := range usageStatsColumnDefs {
		if strings.HasPrefix(c, "cost ") {
			require.True(t, strings.HasPrefix(usageStatsColumnDefs[i+1], "raw_cost "),
				"usageStatsColumnDefs 内序 raw_cost 紧随 cost")
			break
		}
	}
	// SELECT/Scan 列序锚（与 INSERT 同向——错位即四段对齐链断）
	require.Contains(t, statScanSQL, "cost, raw_cost, call_count", "statScanSQL 列序 raw 紧随 cost")
	require.Contains(t, aggUsageSuccessSQL, "sum(cost), COALESCE(sum(raw_cost), 0)", "成功桶 SELECT raw 紧随 cost（COALESCE 空区间归零）")
	require.Contains(t, aggUsageAbortSQL, "sum(cost), COALESCE(sum(raw_cost), 0)", "abort 桶 SELECT raw 紧随 cost")
	// aggErrLogSQL 恒 0 补位锚：测量恒 0 行 7 位（in/out/tot/cr/cc/cost/raw——
	// raw 补位在 cost 恒 0 位后）+ call/TTFT 恒 0 行 4 位不变
	require.Contains(t, aggErrLogSQL,
		"0::bigint, 0::bigint, 0::bigint, 0::bigint, 0::bigint, 0::bigint, 0::bigint,\n\t0::bigint, 0::bigint, 0::bigint, 0::bigint,",
		"aggErrLogSQL 恒 0 序列 7+4 位（raw 补位后）")
	require.Contains(t, statSummarySQL, "COALESCE(sum(cost), 0)::bigint,\n\tCOALESCE(sum(raw_cost), 0)::bigint", "summary SELECT raw 紧随 cost")
	require.Contains(t, statTrendSQL, "COALESCE(sum(cost), 0)::bigint,\n\tCOALESCE(sum(raw_cost), 0)::bigint", "trend SELECT raw 紧随 cost")
}

// TestIsBootstrapRaceErrorCodeSet 并发 bootstrap 容忍码集锚：撞名
// （isDuplicateObject——42P07 关系插入撞锁 / 42710 类型名预检撞隐式复合类型 /
// 23505 CREATE SEQUENCE 并发）+ 缺失（isMissingObject——42P01，stale-DROP 窗
// 口）全部容忍；其余错误码与非 pgconn 错误不误判。漏 42710/42P01 均导致
// TestUsageLogPartitionConcurrentBootstrapPG 偶发失败（2026-08-13 实测两种码
// 均现）。
func TestIsBootstrapRaceErrorCodeSet(t *testing.T) {
	pgErr := func(code string) error {
		return &pgconn.PgError{Code: code}
	}
	// 撞名集
	require.True(t, isDuplicateObject(pgErr("42P07")), "CREATE TABLE 撞关系 42P07")
	require.True(t, isDuplicateObject(pgErr("42710")), "CREATE TABLE 撞隐式复合类型 42710")
	require.True(t, isDuplicateObject(pgErr("23505")), "CREATE SEQUENCE 并发 23505")
	// 缺失集（stale-DROP 窗口）
	require.True(t, isMissingObject(pgErr("42P01")), "OWNED BY/索引/分区引用缺失的表或序列 42P01")
	require.False(t, isMissingObject(pgErr("42P07")), "撞名非缺失不得归入 42P01 集")
	// 组合判定
	require.True(t, isBootstrapRaceError(pgErr("42P07")), "撞名 → bootstrap 竞态")
	require.True(t, isBootstrapRaceError(pgErr("42710")), "类型撞名 → bootstrap 竞态")
	require.True(t, isBootstrapRaceError(pgErr("23505")), "序列并发 → bootstrap 竞态")
	require.True(t, isBootstrapRaceError(pgErr("42P01")), "窗口缺失 → bootstrap 竞态")
	require.False(t, isBootstrapRaceError(pgErr("42P02")), "其它错误码不误判")
	require.False(t, isBootstrapRaceError(errors.New("network error")), "非 pgconn 错误不误判")
}
