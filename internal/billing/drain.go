// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

// drain.go 排空消费机制面（F2-opt v2 三车道拓扑，spec-f2opt-settlement §〇-b；
// wave3 D-C 桶级并行）：每轮顺序执行 Balance 结算 → Temp FEFO 结算 → 零价批纯
// 标记。三车道 batch 谓词互斥（NOT-IN / IN temp-active）→ 同用户同周期不跨车道
// （跨道并行即成环）；车道内 K 桶并行（settleLaneParallel——桶谓词
// COALESCE(user_id,0)%K=i，桶间 uid 集合不相交 → 行锁集不相交，无死锁构造性
// 保证）。周期编排（锁/节流/Close 协议）见 flusher.go；结算语句本体见
// repository.BillingRepo.SettleBalanceBatch/SettleFefoBatch。

import (
	"context"
	"sync"
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

// settleParallelism 桶级并行度（wave3 D-C 架构裁决：K 由本编排层持有——仓库
// 方法保持 policy-free）：每车道 K 个 goroutine 各自独立 tx/独立连接并发执行
// 同一结算语句的不同桶。K=4 起步（W2 实测单语句串行是天花板——语句内 CTE 串行
// 执行，并行只能来自桶间）。
const settleParallelism = 4

// settleFn 单车道结算面签名（LedgerStore.SettleBalanceBatch/SettleFefoBatch
// 共形：ctx, limit, k, bucket）。
type settleFn func(context.Context, int, int, int) (domain.SettlementSummary, error)

// consumeBatch 单轮三车道消费（§〇-b）：① Balance 车道 K 桶并行结算语句（余额-
// only 用户）② Temp 车道集合化 FEFO 结算语句 K 桶并行（temp-active 用户）③ 零价
// 批 sweep（FetchUnbilledBatch 余量 cost<=0 行一次 MarkBilledBulk 纯标记——吸收
// 态/免费行零资金移动）。返回本轮退出游标的行数（0 = 全车道无进展）。
func (f *Flusher) consumeBatch(ctx context.Context) int64 {
	var drained int64
	drained += f.settleLaneParallel(ctx, f.store.SettleBalanceBatch, f.balanceCtl)
	drained += f.settleLaneParallel(ctx, f.store.SettleFefoBatch, f.fefoCtl)
	drained += f.sweepZeroCost(ctx)
	return drained
}

// settleLaneParallel 单车道 K 桶并行结算（wave3 D-C）：K goroutine 各自调用
// settle(ctx, ctl.limit(), K, i)（i=0..K-1，独立 tx/连接），批规模取本车道专用
// 控制器（ctl 参数——Balance/Fefo 各持其一，spec-adaptive-batch-v2 双车道分治）
// 当前值（batch_controller.go），调用后以实测时长/错误/是否满批反馈 observe
// （sub = BatchRows ≥ lim——满批门控倍增）——WaitGroup 全量收敛后合并 summary
// ——计数相加、Balances 对拼接（桶间 uid 集合按构造不相交，
// 对拼接无重复）。错误语义：首错胜出记录 + Warn，但**等全部 goroutine 完成后才
// 返回**（无 early-abort——他桶已提交的工作必须计入；回滚只发生在出错桶自己的
// 事务内，他桶不受扰）。成功桶的提交是真进展：合并 summary 照常 applySettlement
// + 返回 ΣMarked，失败桶行保持 unbilled 下周期重放（不丢不重）。
func (f *Flusher) settleLaneParallel(ctx context.Context, settle settleFn, ctl *batchController) int64 {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		total    domain.SettlementSummary
		firstErr error
	)
	for i := 0; i < settleParallelism; i++ {
		wg.Add(1)
		go func(bucket int) {
			defer wg.Done()
			lim := ctl.limit()
			t0 := time.Now()
			s, err := settle(ctx, lim, settleParallelism, bucket)
			ctl.observe(time.Since(t0), err, s.BatchRows >= int64(lim))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return // 失败桶零贡献；他桶照常合并
			}
			total.Marked += s.Marked
			total.BatchRows += s.BatchRows
			total.DebitedUsers += s.DebitedUsers
			total.ForcedUsers += s.ForcedUsers
			total.Quarantined += s.Quarantined
			total.Balances = append(total.Balances, s.Balances...)
		}(i)
	}
	wg.Wait()
	if firstErr != nil && f.log != nil && ctx.Err() == nil {
		f.log.Warn("billing settle lane failed", logx.Error(firstErr))
	}
	f.applySettlement(total) // 成功桶定向余额刷新 + quarantined 观测（空批 no-op）
	return total.Marked
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
