// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// billing_settle.go 结算语句面（F2-opt v2 三车道拓扑，spec-f2opt-settlement §〇-b/
// §一/D7）：单条自包含 CTE 结算一个窗口——取批/扣减/标记一体，每窗口一次往返。
//
//   - Balance 车道 SettleBalanceBatch：batch 排除 temp-active 用户（NOT-IN），
//     totals→条件扣（balance>=delta RETURNING）→透支补刀（未命中者无条件扣）→
//     标记（AND NOT l.billed 守卫随迁）。
//   - Temp 车道 SettleFefoBatch：batch 限定 temp-active 用户（IN），窗口函数
//     集合化 FEFO（expires ASC NULLS LAST；rn/cum ROWS 帧防同刻并列错账）→
//     行级条件扣（amount>=take）→ spill 差额进余额条件扣→透支补刀→标记。
//
// 两车道 batch 谓词互斥（NOT-IN / IN temp-active）→ 同用户同周期不跨车道；
// 车道间会话锁内顺序执行（跨道并行即成环），车道内 K 桶并行（wave3 D-C——桶间
// uid 不相交，行锁集不相交，无死锁构造性保证）。事务纪律：BEGIN → SET LOCAL
// sync_commit=off → 执行 → marked==batch 计数比对（不齐 = 并发标记，整事务回滚）
// → COMMIT。结算失败保持 unbilled，由下周期重放；usage_logs
// 明细唯一写者仍是 usage flusher（InsertBatch）；本文件只做消费。游标取批/纯标记/
// lag/会话锁面见 billing_cursor.go，SQL 事实源见 billing_settle_sql.go。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/is7qin/c3api/internal/domain"
)

// billingSyncCommitOffSQL 结算事务首语句（F2-opt D4 会话级让渡）：SET LOCAL 事务
// 作用域——连接归还即失效，零泄漏面。安全性论证（钉入注释）：扣减与标记同一
// 语句同一事务——提交尾部 fsync 丢失的唯一后果是整个事务不存在 → 该批行保持
// unbilled 下周期重放；**不存在「标了没扣」「扣了没标」中间态**，资金一致性由
// 原子性结构保证而非 fsync 保证。SET 失败 = 事务回滚重放（安全缺省）。
const billingSyncCommitOffSQL = `SET LOCAL synchronous_commit TO off`

// errConcurrentMark 并发标记哨兵（锁丢失双扣防御；语义自 billing_cursor.go 迁入
// 结算语句守卫）：结算语句标记步受影响行数少于批大小 = 他方消费者已抢先标记同
// 批行（EPQ 重评时 AND NOT l.billed 谓词失败跳过该行）。触发场景：会话级
// advisory lock 持有连接的后端异常死亡而本实例其余池连接幸存——第二实例取锁消费
// 重叠未标记行。返回本错误使整个结算事务回滚（余额零变动），该批下周期由游标
// 重放恰扣一次。
var errConcurrentMark = errors.New("billing: concurrent mark detected")

// SettleBalanceBatch Balance 车道结算一个窗口（≤limit 行，余额-only 用户）：
// 单语句单事务原子完成 取批→条件扣→透支补刀→标记。桶谓词 COALESCE(user_id,0)
// % k = bucket（wave3 D-C 桶级并行——K 由调用方编排层给定，本包保持 policy-free；
// k=1,bucket=0 = 全量单桶回归路径）。limit<=0 → 零结果 no-op。F7 失败闭合：
// 确定性 22xxx/23xxx 单次尝试后直接返回错误不写销；errConcurrentMark 至多重放
// 一次，二次仍败则返回错误；瞬态/取消立即返回错误。行保持 unbilled 下周期重放。
func (r *BillingRepo) SettleBalanceBatch(ctx context.Context, limit, k, bucket int) (domain.SettlementSummary, error) {
	return r.settleBatch(ctx, limit, k, bucket, settleBalancePlan)
}

// SettleFefoBatch Temp 车道结算一个窗口（≤limit 行，temp-active 用户）：集合化
// FEFO 消耗 + 差额透支补刀 + 标记一体（D7）。事务纪律与 SettleBalanceBatch 同
// F7 失败闭合语义。
func (r *BillingRepo) SettleFefoBatch(ctx context.Context, limit, k, bucket int) (domain.SettlementSummary, error) {
	return r.settleBatch(ctx, limit, k, bucket, settleFefoPlan)
}

// settlePlan 单车道结算执行计划：语句本体 + 归因名（错误包装可读性）。
type settlePlan struct {
	sqlText string
	name    string
}

var settleBalancePlan = settlePlan{sqlText: settleBalanceSQL, name: "balance"}
var settleFefoPlan = settlePlan{sqlText: settleFefoSQL, name: "fefo"}

// settleBatch 车道入口（F7 失败闭合）：① 成功 → 原子扣减+标记不变；②
// errConcurrentMark → 至多一次重放，重放成功则收敛，二次仍败返回错误不写销；
// ③ 确定性语句错误（22xxx/23xxx）→ 单次尝试后直接返回错误不写销；④ 瞬态类
// （锁等待 55P03/死锁/序列化/取消/非 PG 错误）→ 立即返回错误不写销。所有失败
// 行保持 unbilled 下周期重放（可重放），MarkBilledBulk 仅保留零价行路径。
func (r *BillingRepo) settleBatch(ctx context.Context, limit, k, bucket int, plan settlePlan) (domain.SettlementSummary, error) {
	if limit <= 0 {
		return domain.SettlementSummary{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, settleTimeout)
	defer cancel()
	res, err := r.settleOnce(ctx, limit, k, bucket, plan.sqlText)
	if err == nil {
		return res, nil
	}
	if ctx.Err() != nil {
		return domain.SettlementSummary{}, err
	}
	if errors.Is(err, errConcurrentMark) {
		res2, err2 := r.settleOnce(ctx, limit, k, bucket, plan.sqlText)
		if err2 == nil {
			return res2, nil
		}
		if ctx.Err() != nil {
			return domain.SettlementSummary{}, err2
		}
		return domain.SettlementSummary{}, fmt.Errorf("billing settle %s lane: %w", plan.name, err2)
	}
	return domain.SettlementSummary{}, fmt.Errorf("billing settle %s lane: %w", plan.name, err)
}

// settleOnce 单次尝试：按载体选事务路径（pool → pgx 直连；nil → ent txDriver
// 回落——WithTx 嵌套 Tx 语义不变）。
func (r *BillingRepo) settleOnce(ctx context.Context, limit, k, bucket int, sqlText string) (domain.SettlementSummary, error) {
	if r.pool != nil {
		return settlePGX(ctx, r.pool, limit, k, bucket, sqlText)
	}
	return settleEnt(ctx, r.driver, limit, k, bucket, sqlText)
}

// settleTx 结算事务执行面（pgx.Tx / ent txDriver 双适配；载体差异收敛到本接口，
// 语句编排单一实现防漂移）。QueryRows 返回归一化 Close 的 *billingRows（entsql
// 与 pgx 行集 Close 签名差异在适配点收敛）。
type settleTx interface {
	// ExecAffected 执行一条 SQL 返回受影响行数（SET LOCAL 面）。
	ExecAffected(ctx context.Context, query string, args []any) (int64, error)
	// QueryRows 执行查询返回行集句柄（结算终 SELECT）。
	QueryRows(ctx context.Context, query string, args []any) (*billingRows, error)
}

// settlePGX pgx 直连路径：单连接 BEGIN → 结算语句 → 计数比对 → COMMIT；任一
// 失败整体回滚（defer Rollback；行保持 unbilled 游标重放）。
func settlePGX(ctx context.Context, pool *pgxpool.Pool, limit, k, bucket int, sqlText string) (domain.SettlementSummary, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return domain.SettlementSummary{}, err
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return domain.SettlementSummary{}, err
	}
	defer tx.Rollback(ctx) // nolint:errcheck // Commit 成功后返回 ErrTxClosed，忽略
	res, err := runSettleStmt(ctx, &pgxSettleTx{tx: tx}, sqlText, limit, k, bucket)
	if err != nil {
		return domain.SettlementSummary{}, err // 回滚零移动（并发标记/语句错误）
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.SettlementSummary{}, err
	}
	return res, nil
}

// settleEnt ent 事务路径（pool == nil 回落）：txDriver 包装保证 raw SQL 与 ent
// 构建器同事务连接（WithTx 同构）。
func settleEnt(ctx context.Context, drv dialect.Driver, limit, k, bucket int, sqlText string) (domain.SettlementSummary, error) {
	tx, err := drv.Tx(ctx)
	if err != nil {
		return domain.SettlementSummary{}, err
	}
	defer tx.Rollback() // nolint:errcheck // Commit 成功后 Rollback 返回 ErrTxDone，忽略
	res, err := runSettleStmt(ctx, &entSettleTx{drv: &txDriver{tx: tx, drv: drv}}, sqlText, limit, k, bucket)
	if err != nil {
		return domain.SettlementSummary{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.SettlementSummary{}, err
	}
	return res, nil
}

// runSettleStmt 结算语句编排（两载体单一实现）：sync_commit 让渡 → 执行（args =
// [limit, k, bucket]——桶谓词占位 $2/$3，wave3 D-C）→ 扫描 → marked==batch 计数
// 比对守卫（不齐 = 他方消费者已抢标同批行 → errConcurrentMark 使整事务回滚——
// markBilledExec Σ守卫的语句化迁移）。
func runSettleStmt(ctx context.Context, exe settleTx, sqlText string, limit, k, bucket int) (domain.SettlementSummary, error) {
	if _, err := exe.ExecAffected(ctx, billingSyncCommitOffSQL, nil); err != nil {
		return domain.SettlementSummary{}, fmt.Errorf("set synchronous_commit off: %w", err)
	}
	rows, err := exe.QueryRows(ctx, sqlText, []any{limit, k, bucket})
	if err != nil {
		return domain.SettlementSummary{}, err
	}
	res, err := scanSettleResult(rows)
	if err != nil {
		return domain.SettlementSummary{}, err
	}
	if res.Marked != res.BatchRows {
		return res, fmt.Errorf("%w: marked %d/%d rows in settle stmt", errConcurrentMark, res.Marked, res.BatchRows)
	}
	return res, nil
}

// scanSettleResult 终 SELECT 扫描：首行聚合哨兵（uid=-1，ORDER BY 恒置首）承载
// 五计数；其余行为 debited/forced 的定向余额对，末两列仅 crossing 时携带阈值
// 与邮箱快照。哨兵缺失 = 语句契约破坏（防御，上抛回滚）。
func scanSettleResult(rows *billingRows) (domain.SettlementSummary, error) {
	defer rows.Close()
	var res domain.SettlementSummary
	seen := false
	for rows.Next() {
		var uid, bal, batchN, debN, forcN, markN, ghostN int64
		var warningThreshold sql.NullInt64
		var warningEmail sql.NullString
		if err := rows.Scan(&uid, &bal, &batchN, &debN, &forcN, &markN, &ghostN,
			&warningThreshold, &warningEmail); err != nil {
			return domain.SettlementSummary{}, err
		}
		if !seen && uid == -1 {
			res = domain.SettlementSummary{BatchRows: batchN, DebitedUsers: debN,
				ForcedUsers: forcN, Marked: markN, Quarantined: ghostN}
			seen = true
			continue
		}
		res.Balances = append(res.Balances, domain.UserBalance{UserID: uid, Balance: bal})
		if warningThreshold.Valid != warningEmail.Valid {
			return domain.SettlementSummary{}, errors.New("billing settle: incomplete balance warning row")
		}
		if warningThreshold.Valid {
			res.BalanceWarnings = append(res.BalanceWarnings, domain.BalanceWarningEvent{
				EventType:       domain.NotificationBalanceWarningCrossed,
				EntityType:      domain.NotificationUser,
				EntityID:        uid,
				BalanceMillis:   bal,
				ThresholdMillis: warningThreshold.Int64,
				Email:           warningEmail.String,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return domain.SettlementSummary{}, err
	}
	if !seen {
		return domain.SettlementSummary{}, errors.New("billing settle: aggregate sentinel row missing")
	}
	return res, nil
}

// —— 载体适配（pgx 直连 / ent txDriver） ——

// pgxSettleTx pgx 事务执行面。
type pgxSettleTx struct {
	tx pgx.Tx
}

func (x *pgxSettleTx) ExecAffected(ctx context.Context, query string, args []any) (int64, error) {
	tag, err := x.tx.Exec(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (x *pgxSettleTx) QueryRows(ctx context.Context, query string, args []any) (*billingRows, error) {
	rows, err := x.tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return &billingRows{rowScanner: rows, closeFunc: func() { rows.Close() }}, nil
}

// entSettleTx ent 事务路径执行面（txDriver）。
type entSettleTx struct {
	drv dialect.Driver
}

func (d *entSettleTx) ExecAffected(ctx context.Context, query string, args []any) (int64, error) {
	var res sql.Result
	if err := d.drv.Exec(ctx, query, args, &res); err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (d *entSettleTx) QueryRows(ctx context.Context, query string, args []any) (*billingRows, error) {
	rows := &entsql.Rows{}
	if err := d.drv.Query(ctx, query, args, rows); err != nil {
		return nil, err
	}
	return &billingRows{rowScanner: rows, closeFunc: func() { _ = rows.Close() }}, nil
}
