// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// billing_cursor.go 计费游标消费面（F2 ledger-cursor，spec 2026-08-23）：取批 /
// 纯标记 / lag 观测 / 会话级 advisory lock。游标 = 部分索引 usagelog_unbilled_id
// (id) WHERE NOT billed——行标记 billed=true 后自动退出索引，重启天然续传，无
// watermark 表。结算事务本体见 billing_settle.go（三车道 SettleBalanceBatch /
// SettleFefoBatch）。

import (
	"context"
	"fmt"
	"time"

	"entgo.io/ent/dialect/sql"

	"github.com/is7qin/c3api/internal/domain"
)

// rowScanner 行扫描面（entsql.Rows 与 pgx.Rows 的公共子集；两者 Close 签名
// 不同——entsql.Close() error vs pgx.Close()，故不含 Close）。读取至 EOF
// （rows.Err() 确认）即释放连接：行集在事务/池连接上，EOF 后连接立即可复用。
type rowScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

// billingRows 非事务查询行集句柄：rowScanner + 归一化 Close（entsql.Rows.Close()
// error 与 pgx.Rows.Close() 签名差异在构造点收敛——rowScanner 接口因此不含
// Close，见 billing_settle.go 注释）。
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

// markBilledBulkSQL 纯标记（零价行快速路径 + 终极毒行隔离）：不触碰 overdraft
// （出生 false 保持），幂等可重入。
const markBilledBulkSQL = `UPDATE usage_logs SET billed = TRUE
	WHERE id = ANY($1) AND NOT billed`

// unbilledHeadIDSQL / unbilledHeadCreatedSQL 队头两步法探针（wave3 D-A/D-B，
// spec-f2opt-wave3 §一）：步① 走部分索引 usagelog_unbilled_id 瞬时定位最老可
// 结算行 id（谓词同结算批：cost>0 可结算子集）；步② pkey 回表取 created_at。
// 两次 O(log n)，替代已删除的 usagelog_unbilled_created 索引（marked 步索引
// 维护 -33%）。
// 语义注记（D-B）：队头行 created_at 是 MIN(created_unbilled) 的**有界近似**——
// 游标按 id 升序消费、id 与 created_at 同序（序列分配），偏差上界 = flush 缓冲
// 延迟 + 时钟偏移（秒级），远小于保留期护栏阈值（天级）；随游标推进收敛。
const unbilledHeadIDSQL = `SELECT id FROM usage_logs
	WHERE NOT billed AND error_type IN ('none', 'abort') AND cost > 0
	ORDER BY id LIMIT 1`

const unbilledHeadCreatedSQL = `SELECT created_at FROM usage_logs WHERE id = $1`

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

// UnbilledLag 游标积压度量（wave3 D-B 签名收缩）：最老可结算行 created_at，
// ok=false = 游标空。精确 COUNT 已删（无硬消费者——护栏本质是时间判据；
// Stats().UnbilledRows 降级为占位 0，spec §一 D-B 显式化）。队头两步法见
// unbilledHeadIDSQL 注释。
func (r *BillingRepo) UnbilledLag(ctx context.Context) (oldestCreated time.Time, ok bool, err error) {
	id, ok, err := r.probeCursorHead(ctx)
	if err != nil || !ok {
		return time.Time{}, false, err
	}
	rows, err := r.queryRows(ctx, unbilledHeadCreatedSQL, []any{id})
	if err != nil {
		return time.Time{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return time.Time{}, false, rows.Err()
	}
	if err := rows.Scan(&oldestCreated); err != nil {
		return time.Time{}, false, err
	}
	return oldestCreated, true, nil
}

// probeCursorHead 队头两步法步①（部分索引 usagelog_unbilled_id 瞬时下降）：
// 返回最老可结算行 id；空游标 → ok=false。形态对齐 ProbeLaneHead。
func (r *BillingRepo) probeCursorHead(ctx context.Context) (id int64, ok bool, err error) {
	rows, err := r.queryRows(ctx, unbilledHeadIDSQL, nil)
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, false, rows.Err()
	}
	if err := rows.Scan(&id); err != nil {
		return 0, false, err
	}
	return id, true, nil
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
	exe := &entSettleTx{drv: r.driver}
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
