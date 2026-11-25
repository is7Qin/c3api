// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"regexp"
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
// pgRowFake（碰不到 SQL），而真正的 *_pg_test.go 在未设 TEST_DATABASE_URL 时全部 SKIP
// ⇒ 在无 PG 的环境里是恒绿的、不构成防线。
//
// 这里用**纯字符串**核对：INSERT 列清单（除 updated_at，由 now() 生成）必须与
// RoutingQualityRow 的导出字段逐一对应。字段改名会让 UpsertQualityRow 的 args
// 编译失败（显式 row.FieldName），但 **SQL 文案**与字段名之间的错位编译器看不见。
func TestQualityUpsertSQLColumnsMatchRowFields(t *testing.T) {
	const table = "routing_quality_fact"
	sqlText := repository.QualityUpsertSQLForTest(table, "route_class_id, candidate_fingerprint, bucket_minute, instance_src, quality_class_id")

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

	require.ElementsMatch(t, rowCols, sqlCols,
		"qualityUpsertSQL 的列清单必须与 RoutingQualityRow 的字段一一对应（字段名是契约，顺序不是）")

	// VALUES 的占位符个数 = 列数 − updated_at（now() 不占位）。
	require.Len(t, regexp.MustCompile(`\$\d+`).FindAllString(sqlText, -1), len(sqlCols)-1,
		"占位符个数必须等于需要绑定的列数（updated_at 用 now()）")

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

// TestRoutingFlowRowStillReachableFromQualitymodel 是一次轻量回归：R1-b 之后
// 行契约对两侧都可见（quality 生产、repository 落库），且字段名未变。
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
