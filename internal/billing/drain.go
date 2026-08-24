// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

// drain.go 排空消费机制面（F2-opt v2 三车道拓扑，spec-f2opt-settlement §〇-b）：
// 每轮顺序执行 Balance 结算语句 → Temp FEFO 结算语句 → 零价批纯标记。三车道
// batch 谓词互斥（NOT-IN / IN temp-active）→ 同用户同周期不跨车道；会话锁内
// 顺序执行（并行即成环）。周期编排（锁/节流/Close 协议）见 flusher.go；结算
// 语句本体见 repository.BillingRepo.SettleBalanceBatch/SettleFefoBatch。

import (
	"context"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// drainCycleBudget 单消费周期内排空循环的时间预算（F2-opt G1 审计 D 面）：
// 持续到达下零价行持续供进度会使单周期无界持有会话级 advisory lock 与
// flushMu——refreshT 停摆 → Balances.Reload 停摆 → 新用户预检快照缺失
// 402（guardPipeline 预检 fail-closed）。预算到期收尾本周期，剩余积压保持
// unbilled 由下一 tick 续排（RestartConvergence 收敛语义不变）；最坏超出 =
// 单批时长。var（非 const）：测试注入；<=0 = 禁用预算。
var drainCycleBudget = 500 * time.Millisecond

// drainLoop 排空式消费（D2）：循环 三车道消费 直至零进展、周期预算到期或
// ctx.Err()——一批一 tick 的节奏概念废除，FlushInterval 仅在游标空时作为
// 空转间隔。
func (f *Flusher) drainLoop(ctx context.Context) int64 {
	deadline := time.Now().Add(drainCycleBudget)
	var drained int64
	for ctx.Err() == nil {
		n := f.consumeBatch(ctx)
		if n == 0 {
			return drained // 三车道全零进展：本周期收尾（不空转）
		}
		drained += n
		if drainCycleBudget > 0 && time.Now().After(deadline) {
			return drained // 预算到期：让位 ticker/refreshT，剩余积压下一 tick 续排
		}
	}
	return drained
}

// consumeBatch 单轮三车道顺序消费（§〇-b）：① Balance 车道结算语句（余额-only
// 用户）② Temp 车道集合化 FEFO 结算语句（temp-active 用户）③ 零价批 sweep
// （FetchUnbilledBatch 余量 cost<=0 行一次 MarkBilledBulk 纯标记——吸收态/免费
// 行零资金移动）。返回本轮退出游标的行数（0 = 全车道无进展）。
func (f *Flusher) consumeBatch(ctx context.Context) int64 {
	var drained int64
	drained += f.settleLane(ctx, f.store.SettleBalanceBatch)
	drained += f.settleLane(ctx, f.store.SettleFefoBatch)
	drained += f.sweepZeroCost(ctx)
	return drained
}

// settleLane 单车道结算收尾：语句失败 → Warn 本车道归零（毒行梯子已在仓库侧
// 收敛——隔离行以 Quarantined 计数随 summary 返回）；成功 → 定向余额刷新 +
// quarantined 观测，返回退出游标行数。
func (f *Flusher) settleLane(ctx context.Context, settle func(context.Context, int) (domain.SettlementSummary, error)) int64 {
	s, err := settle(ctx, fetchBatchLimit)
	if err != nil {
		if f.log != nil && ctx.Err() == nil {
			f.log.Warn("billing settle lane failed", logx.Error(err))
		}
		return 0 // 行保持 unbilled，下周期重放（不丢不重）
	}
	f.applySettlement(s)
	return s.Marked
}

// applySettlement 结算成功收尾：(uid,balance_after) 对定向刷新余额快照（O(1)
// 原地 Store——oracle 必改 #3，10s Reload 间隙预检新鲜度）；幽灵/隔离行计数 +
// Warn（毒用户不卡游标）。
func (f *Flusher) applySettlement(s domain.SettlementSummary) {
	for _, p := range s.Balances {
		f.bal.Set(p.UserID, p.Balance)
	}
	if s.Quarantined > 0 {
		f.quarantined.Add(s.Quarantined)
		if f.log != nil {
			f.log.Warn("billing settle: rows marked without deduction",
				logx.Int64("rows", s.Quarantined))
		}
	}
}

// sweepZeroCost 零价批车道（§〇-b 车道 3）：取批余量中 cost<=0 行一次
// MarkBilledBulk 纯标记。cost>0 行忽略（两结算车道的后续窗口取批——本车道只
// 吃免费行，绝不越权标记未扣费行）。
func (f *Flusher) sweepZeroCost(ctx context.Context) int64 {
	rows, err := f.store.FetchUnbilledBatch(ctx, fetchBatchLimit)
	if err != nil {
		if f.log != nil && ctx.Err() == nil {
			f.log.Warn("billing cursor fetch failed", logx.Error(err))
		}
		return 0
	}
	var zeroIDs []int64
	for _, r := range rows {
		if r.Cost <= 0 {
			zeroIDs = append(zeroIDs, r.ID)
		}
	}
	if len(zeroIDs) == 0 {
		return 0
	}
	if err := f.store.MarkBilledBulk(ctx, zeroIDs); err != nil {
		if f.log != nil && ctx.Err() == nil {
			f.log.Warn("billing cursor zero-cost mark failed", logx.Error(err))
		}
		return 0
	}
	return int64(len(zeroIDs))
}
