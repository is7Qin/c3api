// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// qualityRowNonColumnFields 是 RoutingQualityRow 里**不落列**的字段及原因。
// 显式列出来的价值：将来有人给行加字段，若既不在 SQL 列里也不在此表里，
// TestQualityUpsertSQLColumnsMatchRowFields 会红 —— 强制做一次"这字段要不要落库"
// 的自觉决定，而不是让行与表悄悄漂移。
var qualityRowNonColumnFields = map[string]string{
	// 只参与 advisory lock key（advisoryLockTx 的 args），DB 列不携带额外身份信息
	// （schema 注释：版本化由身份哈希首字节承担）。
	"IdentityVersion": "advisory-lock key only, not a column",
}

// qualityRowSQLAliases 是**字段名 ≠ snake_case(字段名)** 的少数例外。
// 其余字段走自动推导，所以最常见的"加一列"场景不需要动两条清单。
var qualityRowSQLAliases = map[string]string{
	"Count429":         "count_429",
	"CountOrdinary4xx": "count_ordinary_4xx",
	"Count5xx":         "count_5xx",
	"TTFTN":            "ttft_n",
	"TTFTSumSqLogQ32":  "ttft_sumsq_log_q32",
	"TTFTHist":         "ttft_hist",
	"TTFTSumLogQ32":    "ttft_sum_log_q32",
}

// TestQualityUpsertSQLColumnsMatchRowFields 是 spec §5.4 要求的**不依赖 PG** 的
// 列对齐断言。
//
// 为什么需要它：R1-b 把行契约搬到 internal/qualitymodel 后，"落库行字段名仍与 SQL
// 列清单对齐"是唯一的静默风险面。原 spec 打算交给 sync_rowdata_pg_test.go，但它用的是
// pgRowFake（碰不到 SQL），而真正的 *_pg_test.go 在未配置测试库时全部 SKIP
// ⇒ 在无 PG 的环境里是恒绿的、不构成防线。
// （注：本文件**不**读任何测试库配置，纯字符串比对；这里也不得写出那个环境变量名，
// 否则会触发 internal/testsupport/pgtest 的 DSN 来源门禁。）
//
// 这里用**纯字符串**核对：INSERT 列清单（除 updated_at，由 now() 生成）必须与
// RoutingQualityRow 的导出字段逐一对应。字段改名会让 UpsertQualityRow 的 args
// 编译失败（显式 row.FieldName），但 **SQL 文案**与字段名之间的错位编译器看不见。
func TestQualityUpsertSQLColumnsMatchRowFields(t *testing.T) {
	const table = "routing_quality_fact"
	sqlText := repository.QualityUpsertSQL(table, "route_class_id, candidate_fingerprint, bucket_minute, instance_src, quality_class_id")

	// 只取 INSERT 段的列清单：`INSERT INTO <table> (a, b, ...)`。
	re := regexp.MustCompile(`(?s)INSERT INTO ` + table + ` \(([^)]*)\)`)
	m := re.FindStringSubmatch(sqlText)
	require.Len(t, m, 2, "qualityUpsertSQL 的 INSERT 列表形状变了，本断言需要同步更新")
	sqlCols := splitAndTrim(m[1])

	rowCols := make([]string, 0, len(sqlCols))
	for _, f := range routingQualityRowFieldNames(repository.RoutingQualityRow{}) {
		if _, skip := qualityRowNonColumnFields[f]; skip {
			continue
		}
		if alias, ok := qualityRowSQLAliases[f]; ok {
			rowCols = append(rowCols, alias)
			continue
		}
		rowCols = append(rowCols, snakeCase(f))
	}
	// updated_at 不在行里：由 now() 生成（写面单调，不许调用方回填）。
	rowCols = append(rowCols, "updated_at")

	// ⚠ 必须比**序列**，不能用 ElementsMatch（集合比）：
	// UpsertQualityRow 的 args 是按**位置**绑定的（routing.go:341）。
	// 若 SQL 列清单里对调两列（如 attempts ↔ successes）而 args 不动，
	// 集合比对依然全绿，但落库数据已经错位——而唯一能发现它的
	// routing_pg_test 在无测试库时是 SKIP 的（实测）。
	require.Equal(t, rowCols, sqlCols,
		"qualityUpsertSQL 的列清单必须与 RoutingQualityRow 的字段**同序且一一对应**："+
			"args 按位置绑定，序列错位会静默写错列")

	// VALUES 的占位符必须是 $1..$N 连续且个数 = 列数 − updated_at（now() 不占位）。
	// 只数个数不够：$1,$2,$3 写成 $1,$3,$2 也会让绑定错位。
	placeholders := regexp.MustCompile(`\$(\d+)`).FindAllStringSubmatch(sqlText, -1)
	require.Len(t, placeholders, len(sqlCols)-1,
		"占位符个数必须等于需要绑定的列数（updated_at 用 now()）")
	for i, ph := range placeholders {
		require.Equal(t, strconv.Itoa(i+1), ph[1],
			"占位符必须按 $1..$N 顺序出现：第 %d 个占位符应为 $%d，实为 %s", i+1, i+1, ph[1])
	}

	// 别名表只许写真正的例外：拼错了（映射到不存在的列）这里会红。
	for field := range qualityRowSQLAliases {
		require.Contains(t, routingQualityRowFieldNames(repository.RoutingQualityRow{}), field,
			"qualityRowSQLAliases 指向了不存在的行字段")
	}
	for field := range qualityRowNonColumnFields {
		require.Contains(t, routingQualityRowFieldNames(repository.RoutingQualityRow{}), field,
			"qualityRowNonColumnFields 指向了不存在的行字段")
	}
}

func TestRoutingFlowRowStillReachableFromQualitymodel(t *testing.T) {
	row := repository.RoutingFlowRow{
		RouteClassID:      domain.RouteClassIDVal{},
		Generation:        9,
		InstanceSrc:       "s1",
		AbsoluteSequence:  1,
		ChainCount:        2,
		PreviousAccountID: nil,
	}
	require.Equal(t, int64(9), row.Generation)
	require.Equal(t, "s1", row.InstanceSrc)
}
