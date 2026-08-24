// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// billing_chunk.go chunk 扣费事务面（F2-opt，spec-f2-cursor-throughput D3/D4）：
// 多用户组单事务合并消费——commit 数从 每用户一笔 → 每 chunk 一笔（吞吐核心）。
// 扣减核心 deductOnlyCore 原文复用（FEFO/条件扣/透支/缺失隔离全语义保持）；
// 标记步把全 chunk ids 按 overdraft 标志分两集合、至多两条 UPDATE，合并守卫
// 承接 markBilledExec 的锁丢失双扣防御（语义等价升级到 chunk 原子域）。游标
// 取批/纯标记/lag 面见 billing_cursor.go，单组事务面见 billing_repo.go。

import (
	"context"
	"fmt"

	"github.com/is7qin/c3api/internal/domain"
)

// billingSyncCommitOffSQL chunk 事务首语句（D4 会话级让渡）：SET LOCAL 事务
// 作用域——连接归还即失效，零泄漏面。安全性论证（钉入注释）：扣减与标记同一
// 事务——提交尾部 fsync 丢失的唯一后果是整个事务不存在 → 该批行保持 unbilled
// 下周期重放；**不存在「标了没扣」「扣了没标」中间态**，资金一致性由原子性
// 结构保证而非 fsync 保证。SET 失败 = 事务回滚重放（安全缺省）。usage INSERT
// 面（账本源头）不碰，保持库默认 on。
const billingSyncCommitOffSQL = `SET LOCAL synchronous_commit TO off`

// DeductGroupsAndMark chunk 单事务扣费 + 标记（F2-opt 新增，LedgerStore 纯增量；
// outcomes 与 groups 序一一对应）：按组序逐用户复用 deductOnlyCore 原文不动，
// 随后标记步将全部 ids 按 overdraft 分两集合至多两条 UPDATE（overdraft 回写 B2
// 语义保持），合并守卫 Σaffected == len(allIDs) 否则 errConcurrentMark 整块
// 回滚。任一步失败整体回滚（整 chunk 行保持 unbilled 下周期重放——原子域放大
// 到 chunk，崩溃回滚整块重放）。整个事务受 deductTimeout 约束（64 用户×~4 语句
// 余量充足）。
func (r *BillingRepo) DeductGroupsAndMark(ctx context.Context, groups []domain.LedgerGroup) ([]domain.LedgerGroupOutcome, error) {
	if len(groups) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, deductTimeout)
	defer cancel()
	if r.pool != nil {
		return r.deductGroupsAndMarkPGX(ctx, groups)
	}
	return r.deductGroupsAndMarkEnt(ctx, groups)
}

// deductGroupsAndMarkEnt ent 事务路径（pool == nil 回落；与 pgx 路径共用
// deductGroupsCore）。WithTx 事务内经 txDriver 的嵌套 Tx（返回自身）语义不变。
func (r *BillingRepo) deductGroupsAndMarkEnt(ctx context.Context, groups []domain.LedgerGroup) ([]domain.LedgerGroupOutcome, error) {
	tx, err := r.driver.Tx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() // nolint:errcheck // Commit 成功后 Rollback 返回 ErrTxDone，忽略
	exe := &entDeductTx{drv: &txDriver{tx: tx, drv: r.driver}}
	outcomes, err := deductGroupsCore(ctx, exe, groups)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return outcomes, nil
}

// deductGroupsAndMarkPGX pgx 直连路径：单连接 BEGIN → SET LOCAL → 逐组扣减 →
// 合并标记 → COMMIT。同事务原子；任一失败整体回滚——错误语义与 ent 路径一致
// （整 chunk 行保持 unbilled，flusher 下周期重放）。
func (r *BillingRepo) deductGroupsAndMarkPGX(ctx context.Context, groups []domain.LedgerGroup) ([]domain.LedgerGroupOutcome, error) {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) // nolint:errcheck // Commit 成功后返回 ErrTxClosed，忽略
	exe := &pgxDeductTx{tx: tx}
	outcomes, err := deductGroupsCore(ctx, exe, groups)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return outcomes, nil
}

// deductGroupsCore chunk 事务核心（两载体单一实现，防漂移）：首语句 sync_commit
// 让渡（D4）→ 按组序逐用户 deductOnlyCore 原文复用 → 标记步两集合 ≤2 条 UPDATE
// + 合并守卫。quarantined 组（用户缺失）与 cost<=0 组不产生资金移动，其 ids 归
// 入 overdraft=false 集合（overdraft 出生 false 保持）。
func deductGroupsCore(ctx context.Context, exe deductTx, groups []domain.LedgerGroup) ([]domain.LedgerGroupOutcome, error) {
	if _, err := exe.ExecAffected(ctx, billingSyncCommitOffSQL, nil); err != nil {
		return nil, fmt.Errorf("set synchronous_commit off: %w", err)
	}
	outcomes := make([]domain.LedgerGroupOutcome, len(groups))
	allIDs := make([]int64, 0, 4*len(groups))
	odIDs := make([]int64, 0, 4*len(groups))
	plainIDs := make([]int64, 0, 4*len(groups))
	for i, g := range groups {
		var cost int64
		for _, row := range g.Rows {
			cost += row.Cost // cost == Σ rows.Cost 逐行累加（不变量 #9，禁比例公式）
		}
		bal, od, q, err := deductOnlyCore(ctx, exe, g.UserID, cost)
		if err != nil {
			return nil, err
		}
		outcomes[i] = domain.LedgerGroupOutcome{BalanceAfter: bal, Overdrafted: od, Quarantined: q}
		for _, row := range g.Rows {
			allIDs = append(allIDs, row.ID)
			if od {
				odIDs = append(odIDs, row.ID)
			} else {
				plainIDs = append(plainIDs, row.ID)
			}
		}
	}
	// 标记步：markBilledOverdraftSQL 的 $1 是整语句统一 od 值 → 按 overdraft
	// 标志分两集合、至多两条 UPDATE（B2 回写语义保持）；合并守卫（markBilledExec
	// 模式升级到 chunk 域）：Σaffected < len(allIDs) = 他方消费者已抢先标记同批行
	// → errConcurrentMark 整块回滚（余额零变动），下周期游标重放恰扣一次。
	var marked int64
	for _, set := range []struct {
		ids []int64
		od  bool
	}{
		{ids: odIDs, od: true},
		{ids: plainIDs, od: false},
	} {
		if len(set.ids) == 0 {
			continue
		}
		n, err := exe.ExecAffected(ctx, markBilledOverdraftSQL, []any{set.od, set.ids})
		if err != nil {
			return nil, err
		}
		marked += n
	}
	if marked < int64(len(allIDs)) {
		return nil, fmt.Errorf("%w: marked %d/%d rows in chunk tx", errConcurrentMark, marked, len(allIDs))
	}
	return outcomes, nil
}
