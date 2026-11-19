// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// attempt_logs 的 PG 证据（S2a）——两条最容易被想当然的语义：
//
//  1. **迁移只建父表，分区是 runtime 的职责**：空库跑完版本化迁移后直接写会失败
//     （`no partition of relation … found`）；`-migrate up` 退出前会跑
//     EnsureAllTablePartitions，跑完之后**服务仍未启动**，插入就必须成功。
//     缺了这段，线上表现是「升级当天写入全 500」，而不是启动报错。
//  2. **err_logs.attempt_ordinal 可空**：循环不可见的行（本地拒绝 / 已交付失败）
//     没有序号，插不带该列的行必须得到 NULL。
//
// 基座用 CloneEmpty + 显式 migrate.Up：pgtest 的模板/克隆会顺手跑 ensureBase，
// 那会把「迁移建了分区」这条假象喂给用例。本包 PG 测试串行（无并行测试）。

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/db/migrate"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"
)

// attemptLogFor 造一条待落库的尝试明细（字段有区分度，便于断言列序没错位）。
func attemptLogFor(reqID string, ordinal int16, at time.Time) *domain.AttemptLog {
	return &domain.AttemptLog{
		RequestID:  reqID,
		Ordinal:    ordinal,
		AccountID:  42,
		Lane:       "lane-a",
		Format:     "openai-chat",
		StatusCode: 502,
		Outcome:    "upstream_error",
		ErrorType:  "5xx",
		LatencyMS:  1200,
		CreatedAt:  at,
	}
}

// openMigratedAttemptLogDB 空库 + 版本化迁移（**不**补分区、**不**启服务）。
func openMigratedAttemptLogDB(t *testing.T) (*repository.Repository, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	db := pgTestDB(t, pgtest.CloneEmpty(t))
	res, err := migrate.Up(ctx, db, migrate.UpOptions{})
	require.NoError(t, err)
	require.NotZero(t, res.To, "空库必须真的应用了迁移")
	repos, err := repository.NewWithoutMigrate(ctx, entsql.OpenDB(dialect.Postgres, db), nil)
	require.NoError(t, err)
	return repos, db
}

// pgRelationExists 判断 public 下的关系是否存在（按完整名，含 schema 前缀）。
func pgRelationExists(t *testing.T, db *sql.DB, qualified string) bool {
	t.Helper()
	var ok bool
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT to_regclass($1) IS NOT NULL`, qualified).Scan(&ok))
	return ok
}

// pgAttemptLogCount 父表当前行数（跨分区）。
func pgAttemptLogCount(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT count(*) FROM attempt_logs`).Scan(&n))
	return n
}

// TestAttemptLogInsertAfterMigrateOnlyPG 迁移只建父表；`-migrate up` 补完分区后，
// 服务不启动也必须能直接写入（分区缺失是 500 级事故，不是慢）。
func TestAttemptLogInsertAfterMigrateOnlyPG(t *testing.T) {
	ctx := context.Background()
	repos, db := openMigratedAttemptLogDB(t)
	now := time.Now().UTC()

	// 1. 迁移建的是分区父表（relkind='p'）…
	var parent int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relname = 'attempt_logs' AND c.relkind = 'p'`).Scan(&parent))
	require.Equal(t, 1, parent, "0003 建的是分区父表")

	// 2. …且**一个分区实例都不建**（分区归 runtime，不归迁移）。
	var parts int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_inherits i JOIN pg_class p ON p.oid = i.inhparent
		 WHERE p.relname = 'attempt_logs'`).Scan(&parts))
	require.Zero(t, parts, "迁移不建分区实例（否则「up 之后能不能写」就不再取决于 ensure 逻辑了）")

	// 3. 无分区时写入必须**响亮失败**（PG 不会静默丢行）。
	require.Error(t, repos.AttemptLogs.InsertBatch(ctx, []*domain.AttemptLog{attemptLogFor("att-nopart", 1, now)}),
		"父表无分区时插入必须报错，不许静默成功")

	// 4. `-migrate up` 退出前跑的就是这一个入口（服务仍未启动）。
	require.NoError(t, repos.EnsureAllTablePartitions(ctx, now, now.Add(24*time.Hour)))
	for _, name := range []string{
		"attempt_logs_" + now.Format("20060102"),
		"attempt_logs_" + now.AddDate(0, 0, 1).Format("20060102"),
	} {
		require.True(t, pgRelationExists(t, db, "public."+name), "up 必须预建当日/明日分区 %s", name)
	}

	// 5. 服务不启动，直接写：成功（这是 spec §3 验收 6 的字面要求）。
	state := "upstream_responded"
	msg := "upstream 502"
	withFacts := attemptLogFor("att-facts", 2, now)
	withFacts.CommitState = &state
	withFacts.ErrorMessage = &msg
	require.NoError(t, repos.AttemptLogs.InsertBatch(ctx, []*domain.AttemptLog{
		attemptLogFor("att-plain", 1, now),
		withFacts,
	}))

	// 6. 读回：列序/取值/nil 语义（commit_state 与 error_message 为 nil ⇒ NULL）。
	var (
		reqID    string
		ordinal  int16
		lane     string
		outcome  string
		commit   sql.NullString
		errMsg   sql.NullString
		latency  int64
		inserted int64
	)
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT request_id, attempt_ordinal, lane, outcome, commit_state, error_message, latency_ms, id
		   FROM attempt_logs WHERE request_id = 'att-plain'`).
		Scan(&reqID, &ordinal, &lane, &outcome, &commit, &errMsg, &latency, &inserted))
	require.Equal(t, "att-plain", reqID)
	require.Equal(t, int16(1), ordinal, "attempt_ordinal 原样落盘（允许空洞，不重排）")
	require.Equal(t, "lane-a", lane)
	require.Equal(t, "upstream_error", outcome)
	require.False(t, commit.Valid, "nil CommitState ⇒ NULL（未知，不是空串）")
	require.False(t, errMsg.Valid, "nil ErrorMessage ⇒ NULL")
	require.Equal(t, int64(1200), latency)
	require.NotZero(t, inserted, "id 走序列 DEFAULT nextval")

	var stated, message string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT commit_state, error_message FROM attempt_logs WHERE request_id = 'att-facts'`).Scan(&stated, &message))
	require.Equal(t, state, stated)
	require.Equal(t, msg, message)

	// 7. 大批量跨 chunk：501 行拆成两条语句（12 列 × 500 行 < PG 的 65535 参数上限），
	//    后半段的占位符编号必须接上前一条。
	big := make([]*domain.AttemptLog, 0, 501)
	for i := range 501 {
		big = append(big, attemptLogFor("att-chunk-"+time.Duration(i).String(), int16(i%8+1), now))
	}
	started := time.Now()
	require.NoError(t, repos.AttemptLogs.InsertBatch(ctx, big))
	t.Logf("attempt_logs: 501 rows in 2 statements took %s", time.Since(started).Round(time.Microsecond))
	require.Equal(t, int64(503), pgAttemptLogCount(t, db), "2 条已存在 + 501 条分批落盘")

	// 8. err_logs.attempt_ordinal：可空列，插不带它的行 ⇒ NULL。
	require.NoError(t, repos.InsertErrLogBatch(ctx, []*domain.UsageLog{errLogFor("err-no-ordinal", now)}))
	var (
		ordinalCol sql.NullInt16
		nullable   string
		dataType   string
	)
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT attempt_ordinal FROM err_logs WHERE request_id = 'err-no-ordinal'`).Scan(&ordinalCol))
	require.False(t, ordinalCol.Valid, "不带 attempt_ordinal 的 err_logs 行必须是 NULL（循环不可见 ⇒ 无序号的诚实表示）")
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT is_nullable, data_type FROM information_schema.columns
		  WHERE table_schema = 'public' AND table_name = 'err_logs' AND column_name = 'attempt_ordinal'`).
		Scan(&nullable, &dataType))
	require.Equal(t, "YES", nullable, "列必须可空（强制造值就是编数据）")
	require.Equal(t, "smallint", dataType, "与 attempt_logs.attempt_ordinal 同型：链上排序不跨类型比较")
}

// TestAttemptLogPartitionRetentionPG 独立保留期 DROP 边界：cutoff 之前的分区消失、
// 当日的仍在（分区名即日期，O(1) 退休，不扫行）。
func TestAttemptLogPartitionRetentionPG(t *testing.T) {
	ctx := context.Background()
	repos, db := openMigratedAttemptLogDB(t)
	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	yesterday := today.AddDate(0, 0, -1)

	// 预建 [昨日, 今日] 并写入两天的行（不启服务，直接走单入口）。
	require.NoError(t, repos.EnsureAllTablePartitions(ctx, yesterday, today))
	require.NoError(t, repos.AttemptLogs.InsertBatch(ctx, []*domain.AttemptLog{
		attemptLogFor("att-yesterday", 1, yesterday.Add(12*time.Hour)),
		attemptLogFor("att-today", 2, today.Add(time.Hour)),
	}))
	require.Equal(t, int64(2), pgAttemptLogCount(t, db))

	// cutoff = 今日零点 ⇒ 昨日分区过期、今日保留。
	n, err := repos.DropAttemptLogPartitionsBefore(ctx, today)
	require.NoError(t, err)
	require.Equal(t, 1, n, "仅昨日分区过期")
	require.False(t, pgRelationExists(t, db, "public.attempt_logs_"+yesterday.Format("20060102")),
		"昨日分区已 DROP")
	require.True(t, pgRelationExists(t, db, "public.attempt_logs_"+today.Format("20060102")),
		"今日分区必须保留")

	// 被删分区的数据从父表消失，今日数据完好。
	var kept string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT request_id FROM attempt_logs`).Scan(&kept))
	require.Equal(t, "att-today", kept)
	require.Equal(t, int64(1), pgAttemptLogCount(t, db))

	// DROP 幂等：再跑一次 0 个可删。
	n, err = repos.DropAttemptLogPartitionsBefore(ctx, today)
	require.NoError(t, err)
	require.Zero(t, n)
}
