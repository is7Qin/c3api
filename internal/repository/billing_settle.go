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
// → COMMIT，外层毒行梯子重试 ≤settleMaxAttempts 次、耗尽隔离队头越行。usage_logs
// 明细唯一写者仍是 usage flusher（InsertBatch）；本文件只做消费。游标取批/纯标记/
// lag/会话锁面见 billing_cursor.go，SQL 事实源见 billing_settle_sql.go。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
// k=1,bucket=0 = 全量单桶回归路径）。limit<=0 → 零结果 no-op。毒行梯子随迁
// （oracle 必改 #2）：失败重试 ≤settleMaxAttempts 次，耗尽后隔离该车道该桶队头
// 行（MarkBilledBulk 写销 + Quarantined 计数返回）——确定性毒行纯 LIMIT 重试
// 永不收敛，游标越过继承「游标永不卡死」不变量。整个调用受 settleTimeout
// per-query 超时约束（ctx 截止 → 回滚重放，不隔离）。
func (r *BillingRepo) SettleBalanceBatch(ctx context.Context, limit, k, bucket int) (domain.SettlementSummary, error) {
	return r.settleBatch(ctx, limit, k, bucket, settleBalancePlan)
}

// SettleFefoBatch Temp 车道结算一个窗口（≤limit 行，temp-active 用户）：集合化
// FEFO 消耗 + 差额透支补刀 + 标记一体（D7）。事务纪律与毒行梯子同
// SettleBalanceBatch。
func (r *BillingRepo) SettleFefoBatch(ctx context.Context, limit, k, bucket int) (domain.SettlementSummary, error) {
	return r.settleBatch(ctx, limit, k, bucket, settleFefoPlan)
}

// settlePlan 单车道结算执行计划：语句本体 + 毒行梯子队头探针（batch 谓词同构）
// + 归因名（错误包装可读性；两车道共用同一梯子编排防漂移）。
type settlePlan struct {
	sqlText  string
	probeSQL string
	name     string
}

var settleBalancePlan = settlePlan{sqlText: settleBalanceSQL, probeSQL: probeBalanceHeadSQL, name: "balance"}
var settleFefoPlan = settlePlan{sqlText: settleFefoSQL, probeSQL: probeFefoHeadSQL, name: "fefo"}

// settleMaxAttempts 毒行梯子重试上限（oracle 必改 #2）：确定性语句错误（数据/
// 约束类）在预算内重试仍恒败 = 车道毒行 → 隔离队头越行；并发标记竞态走回滚
// 重放；瞬态类错误（锁等待/序列化/取消）不进梯子——立即返回下周期重放。
const settleMaxAttempts = 3

// settleBatch 车道入口（两车道共用单一实现防漂移）：错误分诊三路——① 并发标记
// 守卫（errConcurrentMark）→ 整事务已回滚零移动，预算内重放 ≤K 次（被抢标行已
// 退出游标，重放批自然收缩恰扣一次）；② 确定性语句错误（数据异常 22xxx/完整性
// 约束 23xxx）→ 重试 ≤K 次耗尽后隔离该车道该桶队头行——ProbeLaneHead 只读定位
// → MarkBilledBulk 写销 → 返回隔离标记（Marked=1/Quarantined=1）；③ 其余瞬态类
// （锁等待 55P03/死锁/序列化/取消/非 PG 错误）→ 立即上抛不隔离（wave-1「不锤击
// 不误隔离」不变量——管理员长事务持锁绝不触发写销），行保持 unbilled 下周期
// 重放。停机/预算到期（ctx.Err()）同样不隔离。
func (r *BillingRepo) settleBatch(ctx context.Context, limit, k, bucket int, plan settlePlan) (domain.SettlementSummary, error) {
	if limit <= 0 {
		return domain.SettlementSummary{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, settleTimeout)
	defer cancel()
	var lastErr error
	for attempt := 0; attempt < settleMaxAttempts; attempt++ {
		res, err := r.settleOnce(ctx, limit, k, bucket, plan.sqlText)
		if err == nil {
			return res, nil
		}
		if ctx.Err() != nil {
			return domain.SettlementSummary{}, err // 停机/超时：不隔离，下周期重放
		}
		if !errors.Is(err, errConcurrentMark) && !isDeterministicStmtErr(err) {
			// 瞬态类：不锤击不误隔离——本周期放弃，行保持 unbilled 下周期重放。
			return domain.SettlementSummary{}, fmt.Errorf("billing settle %s lane: %w", plan.name, err)
		}
		lastErr = err
	}
	head, ok, err := r.ProbeLaneHead(ctx, plan.probeSQL, k, bucket)
	if err != nil {
		return domain.SettlementSummary{}, fmt.Errorf("billing settle %s lane: head probe: %w", plan.name, err)
	}
	if !ok {
		return domain.SettlementSummary{}, fmt.Errorf("billing settle %s lane: poison ladder exhausted after %d attempts, lane empty: %w",
			plan.name, settleMaxAttempts, lastErr)
	}
	if err := r.MarkBilledBulk(ctx, []int64{head}); err != nil {
		return domain.SettlementSummary{}, fmt.Errorf("billing settle %s lane: poison isolation failed: %w", plan.name, err)
	}
	// 隔离写销：该行未扣费但退出游标（Quarantined 另计——对齐 wave-1 终极隔离语义）。
	return domain.SettlementSummary{Marked: 1, Quarantined: 1}, nil
}

// isDeterministicStmtErr 确定性语句错误判别（毒行梯子隔离门槛）：仅数据异常
// （22xxx）/完整性约束（23xxx）类视为车道毒行——同语句重试恒败才值得写销越行；
// 锁等待（55P03）/死锁（40P01）/序列化失败（40001）/取消（57014）等瞬态类以及
// 非 PG 服务端错误一律按瞬态处理（不隔离）。
func isDeterministicStmtErr(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return strings.HasPrefix(pgErr.Code, "22") || strings.HasPrefix(pgErr.Code, "23")
}

// ProbeLaneHead 毒行梯子只读探针（probeSQL = 对应车道 batch 谓词同构查询，含
// 桶谓词——args = [k, bucket]，wave3 D-C）：返回该车道该桶批队头行 id；空批 →
// ok=false。非事务读（无锁无副作用），仅供隔离决策。
func (r *BillingRepo) ProbeLaneHead(ctx context.Context, probeSQL string, k, bucket int) (id int64, ok bool, err error) {
	rows, err := r.queryRows(ctx, probeSQL, []any{k, bucket})
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
// 五计数；其余行为 debited/forced 的定向余额对。哨兵缺失 = 语句契约破坏（防御，
// 上抛回滚）。
func scanSettleResult(rows *billingRows) (domain.SettlementSummary, error) {
	defer rows.Close()
	var res domain.SettlementSummary
	seen := false
	for rows.Next() {
		var uid, bal, batchN, debN, forcN, markN, ghostN int64
		if err := rows.Scan(&uid, &bal, &batchN, &debN, &forcN, &markN, &ghostN); err != nil {
			return domain.SettlementSummary{}, err
		}
		if !seen && uid == -1 {
			res = domain.SettlementSummary{BatchRows: batchN, DebitedUsers: debN,
				ForcedUsers: forcN, Marked: markN, Quarantined: ghostN}
			seen = true
			continue
		}
		res.Balances = append(res.Balances, domain.UserBalance{UserID: uid, Balance: bal})
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
