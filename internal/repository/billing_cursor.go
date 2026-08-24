// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// billing_cursor.go 计费游标消费面（F2 ledger-cursor，spec 2026-08-23）：取批 /
// 纯标记 / lag 观测 / 会话级 advisory lock。游标 = 部分索引 usagelog_unbilled_id
// (id) WHERE NOT billed——行标记 billed=true 后自动退出索引，重启天然续传，无
// watermark 表。扣费事务本体见 billing_repo.go（DeductOnlyAndMark）。

import (
	"context"
	"errors"
	"fmt"
	"time"

	"entgo.io/ent/dialect/sql"

	"github.com/is7qin/c3api/internal/domain"
)

// billingRows 非事务查询行集句柄：rowScanner + 归一化 Close（entsql.Rows.Close()
// error 与 pgx.Rows.Close() 签名差异在构造点收敛——rowScanner 接口因此不含
// Close，见 billing_repo.go 注释）。
type billingRows struct {
	rowScanner
	closeFunc func()
}

// Close 释放行集（幂等语义由底层驱动保证；entsql 错误静默——非事务读路径无回滚面）。
func (b *billingRows) Close() { b.closeFunc() }

// billingCursorLockKey 计费游标消费者会话级 advisory lock 键（固定魔数，形态
// 对齐 statsAggLockKey；键值任意恒定即可）。**会话级持锁整周期是 Momus M1 的
// 双扣防线**：两实例若各自在提交前取到同批未标记行 = 双扣资金——故明令禁止
// 每事务 pg_advisory_xact_lock 形态（事务结束即放锁，取批与标记间无互斥）。
const billingCursorLockKey int64 = 0x62696c63 // "bilc"

// fetchUnbilledSQL 取未扣账本批（游标消费主查询）：部分索引谓词同构（NOT
// billed）+ error_type 收敛值域（usage_logs 仅 none/abort，IN 为防御性显式）。
// F2-opt D1 单取批面：cost > 0 谓词删除——零价行同批取出由消费侧内存路由
// （MarkBilledBulk 纯标记），消灭 FetchZeroCostIDs 第二遍全扫查询类。ORDER BY
// id 单调推进游标。
const fetchUnbilledSQL = `SELECT id, COALESCE(user_id, 0), cost, model,
	COALESCE(billing_tier, ''), call_count, format
	FROM usage_logs
	WHERE NOT billed AND error_type IN ('none', 'abort')
	ORDER BY id LIMIT $1`

// markBilledOverdraftSQL 扣费事务内标记（overdraft 回写，B2）：AND NOT billed
// 选用部分索引；幂等语义由调用方标记步受影响行数守卫承接（见 errConcurrentMark
// ——部分受影响不再静默成功，而是整事务回滚）。
const markBilledOverdraftSQL = `UPDATE usage_logs SET billed = TRUE, overdraft = $1
	WHERE id = ANY($2) AND NOT billed`

// markBilledBulkSQL 纯标记（零价行快速路径 + 终极毒行隔离）：不触碰 overdraft
// （出生 false 保持），幂等可重入。
const markBilledBulkSQL = `UPDATE usage_logs SET billed = TRUE
	WHERE id = ANY($1) AND NOT billed`

// unbilledLagSQL lag 度量（停机护栏数据源）：
// 部分索引最小 unbilled 行 created_at + 行数。COUNT 走部分索引；MIN(created_at)
// 需堆取列——unbilled 集合小（正常恒近空），O(unbilled) 可接受。
const unbilledLagSQL = `SELECT COUNT(*), MIN(created_at) FROM usage_logs WHERE NOT billed`

// FetchUnbilledBatch 取未扣账本批（F2 冻结 ABI-2，签名不得偏移）：LedgerRow
// 瘦身投影（ABI-1），按 id 升序返回至多 limit 行。limit <= 0 → 空批（防御，
// 不报错——调用方节奏参数由 config fail-fast 保证为正）。
func (r *BillingRepo) FetchUnbilledBatch(ctx context.Context, limit int) ([]domain.LedgerRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := r.queryRows(ctx, fetchUnbilledSQL, []any{limit})
	if err != nil {
		return nil, err
	}
	return scanLedgerRows(rows)
}

// MarkBilledBulk 纯标记（F2 冻结 ABI-2，签名不得偏移）：零价行快速路径 +
// 终极毒行隔离共用——幂等（AND NOT billed），单语句原子。行不存在/已标记 →
// 静默跳过（幂等语义，不报错）。
func (r *BillingRepo) MarkBilledBulk(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := r.execAffected(ctx, markBilledBulkSQL, []any{ids})
	return err
}

// UnbilledLag 游标积压度量（F2 冻结 ABI-2，签名不得偏移）：最老 unbilled 行
// created_at + 行数。空游标 → count=0、oldestCreated 零值。
func (r *BillingRepo) UnbilledLag(ctx context.Context) (oldestCreated time.Time, count int64, err error) {
	rows, err := r.queryRows(ctx, unbilledLagSQL, nil)
	if err != nil {
		return time.Time{}, 0, err
	}
	defer rows.Close()
	if !rows.Next() {
		return time.Time{}, 0, rows.Err()
	}
	var oldest *time.Time
	if err := rows.Scan(&count, &oldest); err != nil {
		return time.Time{}, 0, err
	}
	if oldest == nil { // 空游标：COUNT=0 且 MIN 为 NULL
		return time.Time{}, 0, nil
	}
	return *oldest, count, nil
}

// AcquireBillingLock 抢占计费游标会话级 advisory lock（pg_try_advisory_lock；
// **专用连接持有到 release**——池连接复用即丢锁，P3，形态对齐
// AcquireStatsAggLock）。抢锁失败 → ok=false（本周期跳过，其他实例在消费）。
// release 必须恰好调用一次（解锁 + 归还连接；解锁失败静默——连接归还后会话级
// 锁随连接生命周期消失，无泄漏）。pool 未注入 → 显式错误（单写者互斥不可缺）。
func (r *BillingRepo) AcquireBillingLock(ctx context.Context) (release func(), ok bool, err error) {
	if r.pool == nil {
		return nil, false, fmt.Errorf("billing repo: pgx pool not configured (repository.NewWithPG); cannot acquire billing cursor lock")
	}
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, billingCursorLockKey).Scan(&acquired); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}
	return func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, billingCursorLockKey)
		conn.Release()
	}, true, nil
}

// errConcurrentMark 并发标记哨兵（锁丢失双扣防御，oracle 终审 B 面 CONCERN）：
// DeductOnlyAndMark 标记步受影响行数少于批大小 = 他方消费者已抢先标记同批行。
// 触发场景：会话级 advisory lock 持有连接的后端异常死亡（pg_terminate_backend/
// OOM kill 单后端）而本实例其余池连接幸存——第二实例取锁消费重叠未标记行，本
// 实例在途组继续提交即双扣。返回本错误使整个扣费事务回滚（余额零变动），该组
// 行下周期由游标重放（重放时已标记行退出取批，恰扣一次）。
var errConcurrentMark = errors.New("billing: concurrent mark detected")

// markBilledExec 扣费事务内标记（DeductOnlyAndMark 第 ④ 步；overdraft 回写 B2）
// + 并发标记守卫：affected < len(ids) → errConcurrentMark（调用方整事务回滚）。
func markBilledExec(ctx context.Context, exe deductTx, ids []int64, overdrafted bool) error {
	affected, err := exe.ExecAffected(ctx, markBilledOverdraftSQL, []any{overdrafted, ids})
	if err != nil {
		return err
	}
	if affected < int64(len(ids)) {
		return fmt.Errorf("%w: marked %d/%d rows in deduct tx", errConcurrentMark, affected, len(ids))
	}
	return nil
}

// queryRows 非事务查询双载体分发：pool 直连优先（生产），nil 回落 ent driver
// （New 构造的仓库/测试装配）。返回归一化 Close 的行集句柄。
func (r *BillingRepo) queryRows(ctx context.Context, query string, args []any) (*billingRows, error) {
	if r.pool != nil {
		rows, err := r.pool.Query(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		return &billingRows{rowScanner: rows, closeFunc: func() { rows.Close() }}, nil
	}
	rows := &sql.Rows{}
	if err := r.driver.Query(ctx, query, args, rows); err != nil {
		return nil, err
	}
	return &billingRows{rowScanner: rows, closeFunc: func() { _ = rows.Close() }}, nil
}

// execAffected 非事务执行双载体分发（同 queryRows 纪律）。
func (r *BillingRepo) execAffected(ctx context.Context, query string, args []any) (int64, error) {
	if r.pool != nil {
		tag, err := r.pool.Exec(ctx, query, args...)
		if err != nil {
			return 0, err
		}
		return tag.RowsAffected(), nil
	}
	exe := &entDeductTx{drv: r.driver}
	return exe.ExecAffected(ctx, query, args)
}

// scanLedgerRows LedgerRow 扫描（fetchUnbilledSQL 列序 = ABI-1 字段序）。
func scanLedgerRows(rows *billingRows) ([]domain.LedgerRow, error) {
	defer rows.Close()
	out := make([]domain.LedgerRow, 0, 64)
	for rows.Next() {
		var row domain.LedgerRow
		if err := rows.Scan(&row.ID, &row.UserID, &row.Cost, &row.Model,
			&row.BillingTier, &row.CallCount, &row.Format); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
