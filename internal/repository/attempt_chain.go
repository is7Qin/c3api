// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// attempt_chain.go — 请求链（spec 2026-11-18 retry-observability §4）的三源读面。
//
// 三条查询**全部**带同一个时间窗谓词 `created_at >= $2 AND created_at < $3`：
//   - attempt_logs：走 (request_id, created_at) 索引；
//   - err_logs：**没有** request_id 索引（加索引是另一次长锁决策，本阶段不做），
//     故只能靠 created_at 窗口做分区裁剪 + 索引范围扫描，再按 request_id 过滤；
//   - usage_logs：走唯一索引 usagelog_request_id_created_at。
//
// 「只给 request_id 全表扫」的形态被结构性排除：窗口是必填参数（handler/service
// 只给缺省 ±1d），这里不接受零值窗口。
//
// 每源取 attemptChainSourceLimit+1 行：多出来的那一行**只**用来判定 truncated。
// 静默截断会让「重试了 300 次」看起来像 128 次——那是撒谎，故必须显式标记。

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"
)

// attemptChainSourceLimit 单源返回行数上限（N）。三源各自独立判定：任一源
// 超限即 truncated=true（不是「总共超限」）。
const attemptChainSourceLimit = 128

// attemptChainAttemptSQL 尝试腿：循环可见的失败尝试（成功那跳不落本表）。
// 排序按 (ordinal, created_at)：service 层仍会重排（跨源合并），这里的顺序只
// 保证「同一源内确定性」。
const attemptChainAttemptSQL = `
SELECT attempt_ordinal, account_id, lane, format, status_code, outcome, error_type,
	commit_state, error_message, latency_ms, created_at
FROM attempt_logs
WHERE request_id = $1 AND created_at >= $2 AND created_at < $3
ORDER BY attempt_ordinal, created_at
LIMIT $4`

// attemptChainErrSQL 错误腿：终态错误 + 本地拒绝（后者带 NULL ordinal）。
// 访问路径 = errlog_created_at 的窗口范围扫描（request_id 无索引）。
const attemptChainErrSQL = `
SELECT attempt_ordinal, account_id, format, status_code, error_type, commit_state,
	error_message, latency_ms, created_at
FROM err_logs
WHERE request_id = $1 AND created_at >= $2 AND created_at < $3
ORDER BY created_at
LIMIT $4`

// attemptChainUsageSQL 终态腿：成功 / 已交付的失败行（usage_logs 每请求至多
// 一条——(request_id, created_at) 唯一索引）。没有 status_code/error_message/
// outcome/lane 列（瘦表，见 usageLogColumnDefs）。
const attemptChainUsageSQL = `
SELECT account_id, format, error_type, commit_state, latency_ms, created_at
FROM usage_logs
WHERE request_id = $1 AND created_at >= $2 AND created_at < $3
ORDER BY created_at
LIMIT $4`

// AttemptChainRow 链上的一跳（三源同形 DTO）。列集是三个来源的**并集**：某源
// 没有的列留零值（err/usage 无 lane 与 outcome；usage 无 status_code /
// error_message），service 层**不回填**——缺什么就显示缺什么。
//
// request_id 不在行里：整个查询按单一 request_id 取，去重键的另一半恒等。
type AttemptChainRow struct {
	// Ordinal attempt_logs 恒非 nil；err_logs 为 nil = 循环不可见的行（本地
	// 拒绝/已交付失败）；usage_logs 按设计没有序号（恒 nil）。
	Ordinal      *int16
	AccountID    int64
	Lane         string
	Format       string
	StatusCode   int
	Outcome      string
	ErrorType    string
	CommitState  *string
	ErrorMessage *string
	LatencyMS    int64
	CreatedAt    time.Time
}

// AttemptChainSource 单源读结果：至多 attemptChainSourceLimit 行；Truncated
// = 该源还有没返回的行（LIMIT N+1 探测到第 N+1 行）。
type AttemptChainSource struct {
	Rows      []AttemptChainRow
	Truncated bool
}

// AttemptChainRows 三源结果（legs 各自独立截断，合并顺序由 service 决定）。
type AttemptChainRows struct {
	Attempt AttemptChainSource
	Err     AttemptChainSource
	Usage   AttemptChainSource
}

// QueryAttemptChain 读一个 request_id 的三源链行（窗口 [from, to)）。三条查询
// 串行执行——链查询是低频排障面（一次点击），并发省下的那点延迟换来的是连接
// 池占用翻倍，不值得。
func (r *PartitionRepo) QueryAttemptChain(ctx context.Context, requestID string, from, to time.Time) (AttemptChainRows, error) {
	if requestID == "" {
		return AttemptChainRows{}, fmt.Errorf("query attempt_chain: empty request_id")
	}
	from, to = from.UTC(), to.UTC()
	if !from.Before(to) {
		return AttemptChainRows{}, fmt.Errorf("query attempt_chain: empty window [%s, %s)", from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	// 三腿共用同一组参数（driver 逐条串行发送，不会并发读到同一底层数组）。
	args := []any{requestID, from, to, attemptChainSourceLimit + 1}

	var out AttemptChainRows
	var err error
	if out.Attempt, err = r.readAttemptChainLeg(ctx, "attempt_logs", attemptChainAttemptSQL, args, scanAttemptChainAttempt); err != nil {
		return AttemptChainRows{}, err
	}
	if out.Err, err = r.readAttemptChainLeg(ctx, "err_logs", attemptChainErrSQL, args, scanAttemptChainErr); err != nil {
		return AttemptChainRows{}, err
	}
	if out.Usage, err = r.readAttemptChainLeg(ctx, "usage_logs", attemptChainUsageSQL, args, scanAttemptChainUsage); err != nil {
		return AttemptChainRows{}, err
	}
	return out, nil
}

// readAttemptChainLeg 跑一条腿并按 N+1 → N 收敛（第 N+1 行只用来置
// truncated）。leg 只进错误信息，不参与 SQL 拼装。
func (r *PartitionRepo) readAttemptChainLeg(ctx context.Context, leg, query string, args []any, scan func(*entsql.Rows) (AttemptChainRow, error)) (AttemptChainSource, error) {
	rows := &entsql.Rows{}
	if err := r.driver.Query(ctx, query, args, rows); err != nil {
		return AttemptChainSource{}, fmt.Errorf("attempt chain %s: %w", leg, err)
	}
	defer rows.Close()
	out := AttemptChainSource{Rows: make([]AttemptChainRow, 0, 8)}
	for rows.Next() {
		row, err := scan(rows)
		if err != nil {
			return AttemptChainSource{}, fmt.Errorf("attempt chain %s: scan: %w", leg, err)
		}
		out.Rows = append(out.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return AttemptChainSource{}, fmt.Errorf("attempt chain %s: %w", leg, err)
	}
	// LIMIT 取的是 N+1：多出一行即「还有更多」，裁到 N 并标记——绝不静默丢行。
	if len(out.Rows) > attemptChainSourceLimit {
		out.Rows = out.Rows[:attemptChainSourceLimit]
		out.Truncated = true
	}
	return out, nil
}

// scanAttemptChainAttempt attempt_logs 的行（全部列非空，无需 Null 包装）。
func scanAttemptChainAttempt(rows *entsql.Rows) (AttemptChainRow, error) {
	var (
		row     AttemptChainRow
		ordinal int16
	)
	if err := rows.Scan(&ordinal, &row.AccountID, &row.Lane, &row.Format, &row.StatusCode,
		&row.Outcome, &row.ErrorType, &row.CommitState, &row.ErrorMessage, &row.LatencyMS,
		&row.CreatedAt); err != nil {
		return AttemptChainRow{}, err
	}
	row.Ordinal = &ordinal
	return row, nil
}

// scanAttemptChainErr err_logs 的行（ordinal/account_id 可空：拒绝行没有账号
// 归属，循环不可见的行没有序号）。
func scanAttemptChainErr(rows *entsql.Rows) (AttemptChainRow, error) {
	var (
		row     AttemptChainRow
		ordinal sql.NullInt16
		account sql.NullInt64
	)
	if err := rows.Scan(&ordinal, &account, &row.Format, &row.StatusCode, &row.ErrorType,
		&row.CommitState, &row.ErrorMessage, &row.LatencyMS, &row.CreatedAt); err != nil {
		return AttemptChainRow{}, err
	}
	if ordinal.Valid {
		v := ordinal.Int16
		row.Ordinal = &v
	}
	row.AccountID = account.Int64
	return row, nil
}

// scanAttemptChainUsage usage_logs 的行（account_id 可空；没有序号列）。
func scanAttemptChainUsage(rows *entsql.Rows) (AttemptChainRow, error) {
	var (
		row     AttemptChainRow
		account sql.NullInt64
	)
	if err := rows.Scan(&account, &row.Format, &row.ErrorType, &row.CommitState,
		&row.LatencyMS, &row.CreatedAt); err != nil {
		return AttemptChainRow{}, err
	}
	row.AccountID = account.Int64
	return row, nil
}

// QueryAttemptChain *Repository 门面委托（service 经 store 能力探测调用它，
// 与 QueryFlowFactStats 同款：读面在 PartitionRepo，门面只做转发）。
func (r *Repository) QueryAttemptChain(ctx context.Context, requestID string, from, to time.Time) (AttemptChainRows, error) {
	return r.Partitions.QueryAttemptChain(ctx, requestID, from, to)
}
