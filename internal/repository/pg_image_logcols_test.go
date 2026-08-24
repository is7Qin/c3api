// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// usage_logs 统一计费模型落账面（spec 2026-08-13）真实 PG 测试：删图片 6 列
// （image input/output tokens + image_count + 3 价格快照）加 2 列（call_count +
// price_per_call_millis）5 处同步点 + format=openai-search 枚举扩展。覆盖：
//   - bootstrap 建表列存在断言（DROP 重建语义——usageLogColumnDefs 即终态：
//     新 2 列随建表存在、旧 6 列不存在；普通表路径 bootstrap 重建后同态）
//   - ent CreateBulk 路径（InsertBatch）roundtrip：2 列有值 + image token 并入
//     in/out + format=openai-images 落库、QueryUsages 读回、SQL 层直查
//   - 价格列 NULL 语义（未设置 → NULL 落库、nil 读回；call_count DEFAULT 0）
//   - F2 单写点 + 游标消费：openai-images/openai-search 行经 InsertBatch 落库
//     （ent FormatValidator 校验通过）→ DeductOnlyAndMark 扣费标记 → SQL 层
//     直查 billed 翻转（旧 COPY 路径断言随双写删除——usage flusher 是唯一写者）
//
// 基座约定同 pg_logcols_test.go：newPGRepos 每测试 DROP SCHEMA 重建 + migrate
//（钩子跳过 usagelog）+ 分区 bootstrap。

import (
	"context"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/usagelog"
	"github.com/is7qin/c3api/internal/repository"
)

// pgCallLogCols 统一计费模型 2 列元数据（data_type/is_nullable 断言用）：
// call_count NOT NULL DEFAULT 0（与既有 token 列同形态）；price_per_call_millis
// NULL（nil = 无按单元分量，对齐 pricings nil 语义）。
var pgCallLogCols = []struct {
	name       string
	dataType   string
	isNullable string
	columnDflt string
}{
	{"call_count", "bigint", "NO", "0"},
	{"price_per_call_millis", "bigint", "YES", ""},
}

// pgRemovedImageLogCols 已删除的图片 6 列（存在性取反断言——DROP 重建即终态，
// 任何同步点残留旧列即红）。
var pgRemovedImageLogCols = []string{
	"image_input_tokens", "image_output_tokens", "image_count",
	"price_image_input_millis", "price_image_output_millis", "price_per_image_millis",
}

func pgCallColMeta(t *testing.T, pool *pgxpool.Pool, name string) (dataType, isNullable, columnDflt string) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `
		SELECT data_type, is_nullable, COALESCE(column_default, '')
		FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'usage_logs' AND column_name = $1`, name).
		Scan(&dataType, &isNullable, &columnDflt)
	require.NoError(t, err, "bootstrap 建表必须含 %s 列", name)
	return
}

func pgCallColAbsent(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'usage_logs' AND column_name = $1`, name).
		Scan(&n)
	require.NoError(t, err)
	require.Zero(t, n, "删 6 列后建表不得残留 %s（DROP 重建即终态）", name)
}

// TestUsageLogCallColumnsExistPG bootstrap 建表列存在断言：分区表 + 普通表
// DROP 重建两路径建表后新 2 列齐全（类型/可空/默认值逐项断言）+ 旧 6 列
// 不存在（删 6 加 2 的建表终态语义）。
func TestUsageLogCallColumnsExistPG(t *testing.T) {
	newPGRepos(t) // bootstrap 副作用（分区表路径建表）
	pool := pgTestPool(t)

	// 分区表路径（newPGRepos 已 bootstrap）
	for _, c := range pgCallLogCols {
		dt, nul, dflt := pgCallColMeta(t, pool, c.name)
		require.Equal(t, c.dataType, dt, "%s 类型", c.name)
		require.Equal(t, c.isNullable, nul, "%s 可空", c.name)
		if c.columnDflt != "" {
			require.Contains(t, dflt, c.columnDflt, "%s 默认值", c.name)
		}
	}
	for _, name := range pgRemovedImageLogCols {
		pgCallColAbsent(t, pool, name)
	}

	// 普通表 → bootstrap DROP 重建路径（用户裁决：直接重建即终态，无补列逻辑）
	ctx := context.Background()
	db := pgTestDB(t)
	_, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;`)
	require.NoError(t, err)
	drv := entsql.OpenDB(dialect.Postgres, db)
	oldClient := ent.NewClient(ent.Driver(drv))
	require.NoError(t, oldClient.Schema.Create(ctx)) // 无钩子 migrate → 普通表
	repos2, err := repository.New(drv, true)
	require.NoError(t, err)
	require.NoError(t, repos2.EnsureUsageLogPartitioned(ctx, time.Now()))
	for _, c := range pgCallLogCols {
		dt, nul, _ := pgCallColMeta(t, pool, c.name)
		require.Equal(t, c.dataType, dt, "DROP 重建后 %s 类型", c.name)
		require.Equal(t, c.isNullable, nul, "DROP 重建后 %s 可空", c.name)
	}
	for _, name := range pgRemovedImageLogCols {
		pgCallColAbsent(t, pool, name)
	}
	// 重建后插入含 2 列正常路由（无迁移终态语义）
	require.NoError(t, repos2.Usages.InsertBatch(ctx, []*domain.UsageLog{imageLogFor(0, "img-rebuilt")}))
	rows, err := repos2.QueryUsages(ctx, repository.UsageQuery{Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, int64(2), rows[0].CallCount, "重建后 call_count 落库读回")
}

// imageLogFor 图片计费日志（统一计费模型 spec 2026-08-13 实参形态）：image
// token 并入 Input/OutputTokens（TotalTokens 含之）、张数入 CallCount、每张价
// 入 PricePerCallMillis（毫分/张，例外单位）。gpt-image-2 官方价形态
// 800,000/3,000,000 毫分 1M + aiml 5,400 毫分/张——按 ImageCost 口径计
// 5000×800000/1e6 + 20000×3000000/1e6 + 2×5400 = 74800（仅描述价格形态；
// Cost 字段不在此设，沿用 logFor 的 130，消费断言只钉列值与价快照）。
func imageLogFor(userID int64, requestID string) *domain.UsageLog {
	l := logFor(userID, requestID)
	l.Format = domain.FormatOpenAIImages
	l.Model = "gpt-image-2"
	l.InputTokens = 5000
	l.OutputTokens = 20000
	l.TotalTokens = 25000
	l.CallCount = 2
	l.PricePerCallMillis = int64Ptr(5_400) // 毫分/张（例外单位——per-call 不走 /1e6）
	return l
}

// TestUsageLogCallColumnsRoundtripPG ent CreateBulk 路径（InsertBatch）：
// format=openai-images + 2 列有值（image token 并入 in/out）落库 →
// QueryUsages 读回 + SQL 层直查；未设置路径 → call_count 0（DEFAULT）、
// price_per_call_millis NULL/nil。
func TestUsageLogCallColumnsRoundtripPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)

	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{
		imageLogFor(0, "img-1"),
	}))
	rows, err := repos.QueryUsages(ctx, repository.UsageQuery{Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	got := rows[0]
	require.Equal(t, domain.FormatOpenAIImages, got.Format, "format=openai-images 落库读回")
	require.Equal(t, int64(5000), got.InputTokens, "image token 并入 input_tokens")
	require.Equal(t, int64(20000), got.OutputTokens, "image token 并入 output_tokens")
	require.Equal(t, int64(25000), got.TotalTokens, "TotalTokens 含 image tokens（口径不变）")
	require.Equal(t, int64(2), got.CallCount, "张数入 call_count")
	require.Equal(t, int64(5_400), *got.PricePerCallMillis, "每张价入 price_per_call_millis")

	// SQL 层直查（不经 domain 映射）
	var it, ot, tt, cnt int64
	var perCall *int64
	err = pool.QueryRow(ctx, `SELECT input_tokens, output_tokens, total_tokens, call_count,
		price_per_call_millis
		FROM usage_logs WHERE request_id = 'img-1'`).
		Scan(&it, &ot, &tt, &cnt, &perCall)
	require.NoError(t, err)
	require.Equal(t, int64(5000), it)
	require.Equal(t, int64(20000), ot)
	require.Equal(t, int64(25000), tt)
	require.Equal(t, int64(2), cnt)
	require.Equal(t, int64(5_400), *perCall)

	// 未设置路径：call_count 落 DEFAULT 0、price_per_call_millis NULL、读回 nil
	l2 := usageLogFor("img-2", time.Now().UTC()) // chat 普通日志，不设功能调用列
	l2.Format = domain.FormatOpenAIImages
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{l2}))
	rows, err = repos.QueryUsages(ctx, repository.UsageQuery{Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, r := range rows {
		if r.RequestID != "img-2" {
			continue
		}
		require.Zero(t, r.CallCount, "未设置 → 0（DEFAULT）")
		require.Nil(t, r.PricePerCallMillis, "未设置按单元价 → nil")
	}
	var raw *int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT price_per_call_millis FROM usage_logs WHERE request_id = 'img-2'`).Scan(&raw))
	require.Nil(t, raw, "DB 层 price_per_call_millis 为 NULL")
}

// TestUsageLogCallColumnsBillingCursorPG F2 单写点 + 游标消费：format=
// openai-images 行经 InsertBatch 落库（ent CreateBulk FormatValidator 校验通过）
// → SettleBalanceBatch 扣费标记——SQL 层直查 2 列 + billed 翻转（旧 COPY 路径
// 断言随双写删除）。
func TestUsageLogCallColumnsBillingCursorPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)
	u := seedPGUser(t, repos, "imgcopy@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 1_000_000))

	l := imageLogFor(u.ID, "img-copy-1")
	l.Cost = 130
	seedUnbilled(t, repos, l)
	rows := fetchAllUnbilled(t, repos)
	require.Len(t, rows, 1)

	res, err := repos.SettleBalanceBatch(ctx, 10)
	require.NoError(t, err, "openai-images 行经单写点落库 + 游标消费必须成功")
	require.Len(t, res.Balances, 1)
	require.Equal(t, int64(999_870), res.Balances[0].Balance)

	var it, ot, cnt int64
	var perCall *int64
	var format string
	var billed bool
	err = pool.QueryRow(ctx, `SELECT format, input_tokens, output_tokens, call_count,
		price_per_call_millis, billed
		FROM usage_logs WHERE request_id = 'img-copy-1'`).
		Scan(&format, &it, &ot, &cnt, &perCall, &billed)
	require.NoError(t, err)
	require.Equal(t, "openai-images", format)
	require.Equal(t, int64(5000), it)
	require.Equal(t, int64(20000), ot)
	require.Equal(t, int64(2), cnt)
	require.Equal(t, int64(5_400), *perCall)
	require.True(t, billed, "扣费事务内 billed 翻转")
}

// TestUsageLogSearchBillingCursorPG format=openai-search 单写点落库 + 游标消费
// （spec 2026-08-13：search 端点 task 消费本枚举 + call_count 落账）：call_count=1
// + price_per_call_millis（按次价快照）经 InsertBatch 落库 → SettleBalanceBatch
// 标记翻转。
func TestUsageLogSearchBillingCursorPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)
	u := seedPGUser(t, repos, "search@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 1_000_000))

	l := logFor(u.ID, "search-1")
	l.Format = domain.FormatOpenAISearch
	l.CallCount = 1
	l.PricePerCallMillis = int64Ptr(2_000_000) // 毫分/次（search 按次价快照形态）
	seedUnbilled(t, repos, l)
	rows := fetchAllUnbilled(t, repos)
	require.Len(t, rows, 1)

	res, err := repos.SettleBalanceBatch(ctx, 10)
	require.NoError(t, err, "openai-search 行经单写点落库 + 游标消费必须成功")
	require.Len(t, res.Balances, 1)
	require.Equal(t, int64(999_870), res.Balances[0].Balance)

	var format string
	var cnt int64
	var perCall *int64
	var billed bool
	err = pool.QueryRow(ctx, `SELECT format, call_count, price_per_call_millis, billed
		FROM usage_logs WHERE request_id = 'search-1'`).
		Scan(&format, &cnt, &perCall, &billed)
	require.NoError(t, err)
	require.Equal(t, "openai-search", format)
	require.Equal(t, int64(1), cnt)
	require.Equal(t, int64(2_000_000), *perCall)
	require.True(t, billed, "扣费事务内 billed 翻转")
}

// TestUsageLogCallFormatValidator 客户端面校验（spec §4.3/2026-08-13）：ent 生
// 成的 FormatValidator 接受 openai-images/openai-search、拒绝未知值——两条
// 插入路径都走该校验（CreateBulk Save 前 / COPY 逐行前置）。
func TestUsageLogCallFormatValidator(t *testing.T) {
	require.NoError(t, usagelog.FormatValidator(usagelog.FormatOpenaiImages),
		"ent FormatValidator 必须接受 openai-images（否则 CreateBulk/COPY 双双拒绝）")
	require.NoError(t, usagelog.FormatValidator(usagelog.FormatOpenaiSearch),
		"ent FormatValidator 必须接受 openai-search（否则 search 行 COPY 恒失败回灌）")
	require.Error(t, usagelog.FormatValidator(usagelog.Format("bogus-image-format")),
		"未知 format 拒绝（非法枚举语义保持）")
	require.True(t, domain.FormatOpenAIImages.Valid())
	require.True(t, domain.FormatOpenAISearch.Valid())
}
