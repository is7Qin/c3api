// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// billing_repo.go 扣费事务面（F2 ledger-cursor，spec 2026-08-23）：FEFO 临时额度
// 优先 + 条件扣费（允许透支）+ 同事务标记 billed——扣减与标记原子（at-least-once
// 消费 + 原子 = exactly-once，丢账窗口构造性为零）。usage_logs 明细的唯一写者是
// usage flusher（InsertBatch）；本文件不再插日志（旧 DeductAndLog 三件套 +
// InsertLogs 双实现随双写一并删除）。游标取批/纯标记/lag 观测面见 billing_cursor.go。

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/tempbalance"
	"github.com/is7qin/c3api/internal/ent/user"
)

// deductTimeout 计费扣费事务 per-query 超时（F-P2-4 降级形态）：会话级
// statement_timeout=10s 与 admin 面 ScanStats 大窗口聚合实测冲突（720 万行/30
// 天 → 57014，见 f1-impl-report.md 副作用核实）→ 按 spec 授权降级为计费路径
// per-query 超时：扣费事务整体 10s 上限（执行时长 + 锁等待双有界；锁等待另有
// 池级 lock_timeout=5s 会话 GUC 先行兜底——SELECT 不取行锁不受其影响）。
// 超时 → 事务取消回滚 → 行保持 unbilled，游标下周期重放（不丢不重）。
const deductTimeout = 10 * time.Second

// BillingRepo 计费扣减仓库：DeductOnlyAndMark 单事务完成 FEFO 扣减 + billed 标记。
// 全毫分直接扣减（1 USD = 100,000 毫分，零换算零取整误差）。
//
// 双事务载体（形态沿袭热点修复 A，载体差异收敛到 deductTx 双适配）：
//   - pool != nil（生产 NewWithPG 装配）→ pgx 直连事务
//   - pool == nil（mock/无池装配、WithTx 事务内）→ ent txDriver 事务回落
//
// 两载体共用 deductOnlyCore（FEFO/余额逻辑单一实现，防漂移）。
type BillingRepo struct {
	client *ent.Client
	// driver 为 raw SQL（FEFO/余额条件更新）用：与 txDriver 组合保证 raw SQL
	// 与 ent 构建器同事务连接（WithTx 同构，评审 I-1）。
	driver dialect.Driver
	// pool 为 pgx 连接池（NewWithPG 注入；New 构造的仓库为 nil）：非 nil →
	// DeductOnlyAndMark 走 pgx 直连事务；nil → ent 事务路径回落。WithTx 的
	// tx 版仓库恒传 nil（事务内不挂池，见 repository.go WithTx）。
	pool *pgxpool.Pool
}

// DeductOnlyAndMark 单事务扣费 + 标记（F2 冻结 ABI-2，签名不得偏移）：
//
// ① FEFO 临时额度（不变量 #1）：未过期 temp_balances 按 expires_at 升序逐行扣
//
//	（最早到期先扣、永久 NULL 最后——PG ASC 默认 NULLS LAST），行级条件更新
//	amount >= take 防并发透支（条件不满足则跳过该行，剩余转扣余额，temp 恒
//	不为负）
//
// ② 剩余扣 users.balance：条件更新 balance >= remain（防并发负余额）；0 行 →
//
//	无条件扣（允许透支）；再 0 行 = 用户不存在 → **跳过扣减但仍标记全部 ids、
//	quarantined=true 返回**（不变量 #1 尾语义——毒用户不卡游标）
//
// ③ 事务内 SELECT balance 回读（行锁串行无竞态）返回 balanceAfter
// ④ UPDATE usage_logs SET billed=true, overdraft=$od WHERE id=ANY($ids) AND NOT
//
//	billed——与扣减同事务原子；AND NOT billed 幂等 + 选用部分索引
//	usagelog_unbilled_id（spec §五 非 HOT 注记）
//
// 任一步失败整体回滚（行保持 unbilled，游标重放）。cost <= 0 → 纯标记不扣减
// （防御路径：游标消费面 cost>0 行才进本方法，cost=0 走 MarkBilledBulk 快速
// 标记）。整个事务受 deductTimeout per-query 超时约束（ctx 截止 → 回滚）。
func (r *BillingRepo) DeductOnlyAndMark(ctx context.Context, userID, cost int64, ids []int64) (balanceAfter int64, overdrafted, quarantined bool, err error) {
	if len(ids) == 0 {
		return 0, false, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, deductTimeout)
	defer cancel()
	if r.pool != nil {
		return r.deductOnlyAndMarkPGX(ctx, userID, cost, ids)
	}
	return r.deductOnlyAndMarkEnt(ctx, userID, cost, ids)
}

// deductOnlyAndMarkEnt ent 事务路径（pool == nil 回落；与 pgx 路径共用
// deductOnlyCore）。WithTx 事务内经 txDriver 的嵌套 Tx（返回自身）语义不变。
func (r *BillingRepo) deductOnlyAndMarkEnt(ctx context.Context, userID, cost int64, ids []int64) (int64, bool, bool, error) {
	tx, err := r.driver.Tx(ctx)
	if err != nil {
		return 0, false, false, err
	}
	defer tx.Rollback() // nolint:errcheck // Commit 成功后 Rollback 返回 ErrTxDone，忽略
	drv := &txDriver{tx: tx, drv: r.driver}
	exe := &entDeductTx{drv: drv}
	bal, od, q, err := deductOnlyCore(ctx, exe, userID, cost)
	if err != nil {
		return 0, false, false, err
	}
	if _, err := markBilledExec(ctx, exe, ids, od); err != nil {
		return 0, false, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, false, err
	}
	return bal, od, q, nil
}

// deductOnlyAndMarkPGX pgx 直连路径：单连接 BEGIN → deductOnlyCore → 标记 →
// COMMIT。同事务原子；任一失败整体回滚——错误语义与 ent 路径一致（行保持
// unbilled，flusher 下周期重放）。
func (r *BillingRepo) deductOnlyAndMarkPGX(ctx context.Context, userID, cost int64, ids []int64) (int64, bool, bool, error) {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return 0, false, false, err
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, false, false, err
	}
	defer tx.Rollback(ctx) // nolint:errcheck // Commit 成功后返回 ErrTxClosed，忽略
	exe := &pgxDeductTx{tx: tx}
	bal, od, q, err := deductOnlyCore(ctx, exe, userID, cost)
	if err != nil {
		return 0, false, false, err
	}
	if _, err := markBilledExec(ctx, exe, ids, od); err != nil {
		return 0, false, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, false, err
	}
	return bal, od, q, nil
}

// —— deductOnlyCore：扣费事务核心（两载体单一实现，防漂移） ——

// deductTx 扣费事务执行面（ent txDriver / pgx.Tx 双适配）：FEFO/余额/标记逻辑在
// deductOnlyCore 单一实现，事务载体差异收敛到本接口。
type deductTx interface {
	// ExecAffected 执行一条 SQL 返回受影响行数（条件更新判定）。
	ExecAffected(ctx context.Context, query string, args []any) (int64, error)
	// QueryRows 执行查询返回行扫描器（FEFO SELECT）。
	QueryRows(ctx context.Context, query string, args []any) (rowScanner, error)
	// QueryRowScan 单行查询扫描（余额回读）；0 行 → 错误（整体回滚）。
	QueryRowScan(ctx context.Context, query string, args []any, dest ...any) error
}

// rowScanner 行扫描面（entsql.Rows 与 pgx.Rows 的公共子集；两者 Close 签名
// 不同——entsql.Close() error vs pgx.Close()，故不含 Close）。deductOnlyCore
// 读取至 EOF（rows.Err() 确认）即释放连接：FEFO 行集在事务连接上，tx 生命周期
// 内无池归还压力，EOF 后连接立即可复用（database/sql 与 pgx 同语义）。
type rowScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

// fefoSelectSQL FEFO 临时额度查询（与 ent 生成的语义等价：未过期 + 正余额，
// expires_at 升序——PG ASC 默认 NULLS LAST，永久额度最后）。两载体共用同一
// SQL 事实源。
const fefoSelectSQL = `SELECT id, amount FROM temp_balances
WHERE user_id = $1 AND amount > 0 AND (expires_at IS NULL OR expires_at > $2)
ORDER BY expires_at`

// balanceSelectSQL 余额回读（事务内行锁串行无竞态）。
const balanceSelectSQL = `SELECT balance FROM users WHERE id = $1`

// deductOnlyCore 事务内扣费核心：FEFO 逐行条件扣 → 余额条件扣（允许透支）→
// 余额回读。**不插日志**（usage flusher 是 usage_logs 唯一写者）。用户缺失 →
// quarantined=true 返回（跳过扣减；标记仍由调用方执行——不变量 #1 尾语义）。
// cost <= 0 → 不扣不减不回读（纯标记防御路径）。任一步失败整体回滚（由调用方
// 事务语义保证）。
func deductOnlyCore(ctx context.Context, exe deductTx, userID, cost int64) (balanceAfter int64, overdrafted, quarantined bool, err error) {
	remain := cost
	if cost > 0 {
		rows, err := exe.QueryRows(ctx, fefoSelectSQL, []any{userID, time.Now()})
		if err != nil {
			return 0, false, false, err
		}
		type tempRow struct{ id, amount int64 }
		var tempRows []tempRow
		for rows.Next() {
			var t tempRow
			if err := rows.Scan(&t.id, &t.amount); err != nil {
				return 0, false, false, err
			}
			tempRows = append(tempRows, t)
		}
		if err := rows.Err(); err != nil {
			return 0, false, false, err
		}
		for _, tb := range tempRows {
			if remain <= 0 {
				break
			}
			take := tb.amount
			if take > remain {
				take = remain
			}
			q, args := tempBalanceDeductQuery(tb.id, take)
			n, err := exe.ExecAffected(ctx, q, args)
			if err != nil {
				return 0, false, false, err
			}
			if n > 0 {
				remain -= take
			}
		}
	}
	if remain > 0 {
		q, args := balanceDeductQuery(userID, remain)
		n, err := exe.ExecAffected(ctx, q, args)
		if err != nil {
			return 0, false, false, err
		}
		if n == 0 {
			// 余额不足 → 无条件扣（允许透支）；再 0 行 = 用户不存在
			q, args = balanceDeductForceQuery(userID, remain)
			n, err = exe.ExecAffected(ctx, q, args)
			if err != nil {
				return 0, false, false, err
			}
			if n == 0 {
				return 0, false, true, nil // 用户缺失：跳过扣减，quarantined 出口
			}
			overdrafted = true
		}
	}
	if cost > 0 { // 有扣减面（FEFO/余额/透支任一）即回读（事务内行锁串行无竞态）
		if err := exe.QueryRowScan(ctx, balanceSelectSQL, []any{userID}, &balanceAfter); err != nil {
			return 0, false, false, err
		}
	}
	return balanceAfter, overdrafted, false, nil
}

// tempBalanceDeductQuery 临时额度行级条件扣（amount >= take 防并发透支）。
func tempBalanceDeductQuery(id, take int64) (string, []any) {
	u := entsql.Update(tempbalance.Table).
		Set(tempbalance.FieldAmount, entsql.ExprFunc(func(b *entsql.Builder) {
			b.Ident(tempbalance.FieldAmount).WriteString(" - ").Arg(take)
		})).
		Where(entsql.And(
			entsql.EQ(tempbalance.FieldID, id),
			entsql.GTE(tempbalance.FieldAmount, take),
		))
	u.SetDialect(dialect.Postgres)
	return u.Query()
}

// balanceDeductQuery 余额条件扣（balance >= remain 防并发负余额）。
func balanceDeductQuery(userID, remain int64) (string, []any) {
	u := entsql.Update(user.Table).
		Set(user.FieldBalance, entsql.ExprFunc(func(b *entsql.Builder) {
			b.Ident(user.FieldBalance).WriteString(" - ").Arg(remain)
		})).
		Where(entsql.And(
			entsql.EQ(user.FieldID, userID),
			entsql.GTE(user.FieldBalance, remain),
		))
	u.SetDialect(dialect.Postgres)
	return u.Query()
}

// balanceDeductForceQuery 无条件扣（允许透支；用户缺失时 0 行）。
func balanceDeductForceQuery(userID, remain int64) (string, []any) {
	u := entsql.Update(user.Table).
		Set(user.FieldBalance, entsql.ExprFunc(func(b *entsql.Builder) {
			b.Ident(user.FieldBalance).WriteString(" - ").Arg(remain)
		})).
		Where(entsql.EQ(user.FieldID, userID))
	u.SetDialect(dialect.Postgres)
	return u.Query()
}

// —— ent txDriver 适配（回落路径） ——

// entDeductTx ent 事务路径执行面（txDriver）。
type entDeductTx struct {
	drv dialect.Driver
}

func (d *entDeductTx) ExecAffected(ctx context.Context, query string, args []any) (int64, error) {
	var res sql.Result
	if err := d.drv.Exec(ctx, query, args, &res); err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (d *entDeductTx) QueryRows(ctx context.Context, query string, args []any) (rowScanner, error) {
	rows := &entsql.Rows{}
	if err := d.drv.Query(ctx, query, args, rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func (d *entDeductTx) QueryRowScan(ctx context.Context, query string, args []any, dest ...any) error {
	rows := &entsql.Rows{}
	if err := d.drv.Query(ctx, query, args, rows); err != nil {
		return err
	}
	// 具体类型直闭（接口无 Close——见 rowScanner 注释）；余额回读单行后必须
	// 释放行集，否则事务连接保持 busy。
	defer rows.Close() // nolint:errcheck
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		// 0 行 ≈ ent Only 的 NotFoundError：余额回读目标行不存在 → 整体回滚。
		return errors.New("billing: user not found")
	}
	return rows.Scan(dest...)
}

// —— pgx 直连适配 ——

// pgxDeductTx pgx 事务执行面。
type pgxDeductTx struct {
	tx pgx.Tx
}

func (x *pgxDeductTx) ExecAffected(ctx context.Context, query string, args []any) (int64, error) {
	tag, err := x.tx.Exec(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (x *pgxDeductTx) QueryRows(ctx context.Context, query string, args []any) (rowScanner, error) {
	return x.tx.Query(ctx, query, args...)
}

func (x *pgxDeductTx) QueryRowScan(ctx context.Context, query string, args []any, dest ...any) error {
	return x.tx.QueryRow(ctx, query, args...).Scan(dest...)
}
