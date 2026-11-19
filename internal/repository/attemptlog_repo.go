// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// attempt_logs 尝试明细（spec 2026-11-18 retry-observability §3）：只写不读，
// 故**没有 ent 实体**——S3 的链式查询走 SQL，写侧只需要一条参数化多值 INSERT。
// 分区表 DDL 由 0003_attempt_logs 迁移独占管理（bootstrap 不建这张表：它不在
// MigrationBaselineDDLs 里，见该函数注释）。

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"entgo.io/ent/dialect"

	"github.com/is7qin/c3api/internal/domain"
)

// attemptLogInsertColumns 插入列序（与 attemptLogColumnDefs 同向，除 id：id 走
// 序列 DEFAULT nextval，与建表语义一致）。S2b 的写点只填这些语义列。
const attemptLogInsertColumns = `request_id, attempt_ordinal, account_id, lane, format, ` +
	`status_code, outcome, error_type, commit_state, error_message, latency_ms, created_at`

// attemptLogInsertColumnCount 上面那串列的数量（占位符编号用）。
const attemptLogInsertColumnCount = 12

// attemptLogInsertChunk 单条多值 INSERT 的最大行数：12 列 × 500 行 = 6000 个绑定
// 参数，远低于 PG 的 65535 上限；调用方给多大的批都按此切片（参数个数恒定有界）。
const attemptLogInsertChunk = 500

// AttemptLogRepo attempt_logs 写入面（raw SQL：无 ent 实体，见文件头）。
type AttemptLogRepo struct {
	// driver 为 raw SQL 入口（dialect.Driver.Exec；WithTx 内为事务驱动，与 ent
	// 构建器同连接——与 partition.go 的 DDL 通道同款）。
	driver dialect.Driver
}

// InsertBatch 批量插入尝试明细（单条参数化多值 INSERT，每 attemptLogInsertChunk
// 行一条语句）。字段 NULL 语义：CommitState / ErrorMessage 为 nil ⇒ NULL
// （database/sql 的默认转换器把 nil 指针转成 NULL，不需要手写 IS NULL 分支）。
//
// 幂等性：本表**没有**唯一键——重复插入就是重复行。写点（S2b）只在一次尝试真实
// 结束后投递一次，不去重（去重会掩盖「同一次尝试落了两次」这种真故障）。
func (r *AttemptLogRepo) InsertBatch(ctx context.Context, rows []*domain.AttemptLog) error {
	if len(rows) == 0 {
		return nil
	}
	for start := 0; start < len(rows); start += attemptLogInsertChunk {
		end := min(start+attemptLogInsertChunk, len(rows))
		if err := r.insertChunk(ctx, rows[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// insertChunk 插入一个 ≤ attemptLogInsertChunk 行的切片。
//
// I1（写入面唯一性）：行只在**一次尝试真实结束**后由 failover 循环投递一次，
// 本层不做任何"补写/重放/去重"——这里看到几行，就是上游被打了几次（成功那跳不
// 落行）。幂等性靠"写点唯一"而不是靠本层：想加去重要先证明存在重复投递路径。
func (r *AttemptLogRepo) insertChunk(ctx context.Context, rows []*domain.AttemptLog) error {
	var sb strings.Builder
	sb.Grow(len("INSERT INTO attempt_logs ("+attemptLogInsertColumns+") VALUES ") + len(rows)*40)
	sb.WriteString("INSERT INTO attempt_logs (")
	sb.WriteString(attemptLogInsertColumns)
	sb.WriteString(") VALUES ")
	args := make([]any, 0, len(rows)*attemptLogInsertColumnCount)
	for i, row := range rows {
		if i > 0 {
			sb.WriteByte(',')
		}
		base := i * attemptLogInsertColumnCount
		sb.WriteByte('(')
		for col := range attemptLogInsertColumnCount {
			if col > 0 {
				sb.WriteByte(',')
			}
			sb.WriteByte('$')
			sb.WriteString(strconv.Itoa(base + col + 1))
		}
		sb.WriteByte(')')
		args = append(args,
			row.RequestID, row.Ordinal, row.AccountID, row.Lane, row.Format,
			row.StatusCode, row.Outcome, row.ErrorType, row.CommitState, row.ErrorMessage,
			row.LatencyMS, row.CreatedAt)
	}
	var res sql.Result
	if err := r.driver.Exec(ctx, sb.String(), args, &res); err != nil {
		return fmt.Errorf("insert attempt_logs (%d rows): %w", len(rows), err)
	}
	return nil
}
