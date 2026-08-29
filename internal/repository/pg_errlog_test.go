// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// 错误明细落盘（err_logs 分表设计，用户裁决）真实 PG 测试：bootstrap 建表含
// error_message 列；有值/空值（NULL）roundtrip（ErrLogRepo.InsertBatch +
// QueryErrLogs 读回）；usage_logs 侧断言行**不含** error_message/status_code
// 列（瘦身——C26）。
//
// 基座约定同 pg_partition_test.go：newPGRepos 每测试 DROP SCHEMA 重建 +
// migrate（钩子跳过分区表）+ 分区 bootstrap（两表：usage_logs + err_logs）。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func errLogFor(reqID string, at time.Time) *domain.UsageLog {
	return &domain.UsageLog{
		RequestID: reqID, Model: "m", Format: domain.FormatOpenAIChat,
		StatusCode: 429, ErrorType: domain.Err429, CreatedAt: at,
	}
}

// TestErrLogMessageRoundtripPG err_logs error_message 有值/NULL roundtrip
// （QueryErrLogs 读回）+ 审计字段投影。
func TestErrLogMessageRoundtripPG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	pool := pgSharedPool(t)

	// bootstrap 建表必须含 error_message 列（varchar，无默认值 = NULL 可空）
	var dataType string
	var isNullable string
	err := pool.QueryRow(ctx, `
		SELECT data_type, is_nullable FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'err_logs' AND column_name = 'error_message'`).
		Scan(&dataType, &isNullable)
	require.NoError(t, err, "bootstrap 建表必须含 error_message 列")
	require.Equal(t, "character varying", dataType)
	require.Equal(t, "YES", isNullable)

	// 有值 roundtrip：600 字符 → 域内截断 500 落库读回
	truncated := domain.TruncateErrMsg(strings.Repeat("x", 600))
	require.Len(t, truncated, domain.ErrMsgMaxLen)
	l1 := errLogFor("err-msg-1", time.Now().UTC())
	l1.ErrorMessage = &truncated
	l1.GroupID = 10
	l1.AccountID = 20
	l1.UserID = 30
	l1.KeyID = 40
	l1.BillingTier = "priority"
	l1.LatencyMS = 55
	// 空值 roundtrip：ErrorMessage nil → 列 NULL（成功路径语义，SQL 不写该列）
	l2 := errLogFor("err-msg-2", time.Now().UTC())
	require.NoError(t, repos.InsertErrLogBatch(ctx, []*domain.UsageLog{l1, l2}))

	rows, err := repos.QueryErrLogs(ctx, repository.ErrLogQuery{Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	got := map[string]*domain.UsageLog{}
	for _, r := range rows {
		got[r.RequestID] = r
	}
	require.NotNil(t, got["err-msg-1"].ErrorMessage, "有值必须读回")
	require.Equal(t, truncated, *got["err-msg-1"].ErrorMessage)
	require.Equal(t, domain.Err429, got["err-msg-1"].ErrorType, "error_type 投影")
	require.Equal(t, 429, got["err-msg-1"].StatusCode, "status_code 投影")
	require.Equal(t, int64(10), got["err-msg-1"].GroupID)
	require.Equal(t, int64(20), got["err-msg-1"].AccountID)
	require.Equal(t, int64(30), got["err-msg-1"].UserID)
	require.Equal(t, int64(40), got["err-msg-1"].KeyID)
	require.Equal(t, "priority", got["err-msg-1"].BillingTier)
	require.Equal(t, int64(55), got["err-msg-1"].LatencyMS)
	require.Nil(t, got["err-msg-2"].ErrorMessage, "未设置 ErrorMessage → NULL")

	// SQL 层直查确认 NULL 语义（不经 domain 映射）
	var raw *string
	err = pool.QueryRow(ctx, `SELECT error_message FROM err_logs WHERE request_id = 'err-msg-2'`).Scan(&raw)
	require.NoError(t, err)
	require.Nil(t, raw, "DB 层 error_message 为 NULL")
	var rawVal string
	err = pool.QueryRow(ctx, `SELECT error_message FROM err_logs WHERE request_id = 'err-msg-1'`).Scan(&rawVal)
	require.NoError(t, err)
	require.Equal(t, truncated, rawVal)
}

// TestUsageLogSlimColumnsPG usage_logs 瘦身（C26）：新库建表**不含**
// status_code/error_message 列；插入忽略瞬态字段（不报 42703——ent 构建器
// 不再写该列），查询结果恒零值/nil。
func TestUsageLogSlimColumnsPG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	pool := pgSharedPool(t)

	for _, col := range []string{"status_code", "error_message"} {
		var n int
		err := pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = 'usage_logs' AND column_name = $1`, col).Scan(&n)
		require.NoError(t, err)
		require.Zero(t, n, "usage_logs 不得含 %s 列（瘦身）", col)
	}

	// 域对象带瞬态审计字段（err_logs 承载）插入 → 只落瘦列，无 42703
	l := usageLogFor("slim-1", time.Now().UTC())
	msg := "boom"
	l.ErrorMessage = &msg
	l.StatusCode = 502
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{l}))

	rows, err := repos.QueryUsages(ctx, repository.UsageQuery{Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Zero(t, rows[0].StatusCode, "usage_logs 查询无 status_code（恒 0）")
	require.Nil(t, rows[0].ErrorMessage, "usage_logs 查询无 error_message（恒 nil）")
	require.Equal(t, domain.ErrNone, rows[0].ErrorType, "error_type 保留")
}
