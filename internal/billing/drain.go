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
	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

// drainCycleBudget 单消费周期内排空循环的时间预算（F2-opt G1 审计 D 面）：
// 持续到达下零价行持续供进度会使单周期无界持有会话级 advisory lock 与
// flushMu——refreshT 停摆 → Balances.Reload 停摆 → 新用户预检快照缺失
// 402（guardPipeline 预检 fail-closed）。预算到期收尾本周期，剩余积压保持
// unbilled 由下一 tick 续排（RestartConvergence 收敛语义不变）；最坏超出 =
// 单批时长。var（非 const）：测试注入；<=0 = 禁用预算。
var drainCycleBudget = 500 * time.Millisecond

// drainLoop 排空式消费（D2 + F7 失败闭合）：循环 三车道消费 直至零进展、
// 周期预算到期或 ctx.Err()——F7 失败闭合要求失败 lane/bucket 在本周期内不再重试
// （下周期 ticker 重试），健康桶继续独立提交。
func (f *Flusher) drainLoop(ctx context.Context) int64 {
	deadline := time.Now().Add(drainCycleBudget)
	var (
		drained       int64
		failedBalance [settleParallelism]bool
		failedFefo    [settleParallelism]bool
	)
	for ctx.Err() == nil {
		n := f.consumeBatchFiltered(ctx, &failedBalance, &failedFefo)
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

// consumeBatchFiltered F7 失败闭合：同 consumeBatch 但传入周期内已失败桶集合——
// 失败桶在本周期内不再重试（下周期重试），健康桶继续。
func (f *Flusher) consumeBatchFiltered(ctx context.Context, failedBalance, failedFefo *[settleParallelism]bool) int64 {
	var drained int64
	drained += f.settleLaneParallel(ctx, "balance", f.store.SettleBalanceBatch, f.balanceCtl, failedBalance)
	drained += f.settleLaneParallel(ctx, "fefo", f.store.SettleFefoBatch, f.fefoCtl, failedFefo)
	drained += f.sweepZeroCost(ctx)
	return drained
}

// settleLaneParallel 单车道 K 桶并行结算（wave3 D-C + F7 失败闭合）：K
// goroutine 各自调用 settle(ctx, ctl.limit(), K, i)（i=0..K-1，独立 tx/连接），
// 批规模取本车道专用控制器当前值，调用后 observe 反馈。failed 非 nil 时跳过本
// 周期已失败桶（不重试，下周期重试）；失败桶按 lane/bucket 粒度 Warn（F7 可观测
// 性：lane, bucket, retry_scope=next_cycle, error）。WaitGroup 全量收敛后合并
// summary；成功桶照常 applySettlement，失败桶零贡献且记录到 failed 集合。
func (f *Flusher) settleLaneParallel(ctx context.Context, lane string, settle settleFn, ctl *batchController, failed *[settleParallelism]bool) int64 {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		total domain.SettlementSummary
	)
	for bucket := 0; bucket < settleParallelism; bucket++ {
		if failed != nil && (*failed)[bucket] {
			continue
		}
		wg.Add(1)
		go func(bucket int) {
			defer wg.Done()
			defer worker.CatchPanic("billing-bucket", f.log)
			lim := ctl.limit()
			t0 := time.Now()
			s, err := settle(ctx, lim, settleParallelism, bucket)
			ctl.observe(time.Since(t0), err, s.BatchRows >= int64(lim))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if failed != nil {
					failed[bucket] = true
				}
				if f.log != nil && ctx.Err() == nil {
					f.log.Warn("billing settle lane failed",
						logx.String("lane", lane),
						logx.Int("bucket", bucket),
						logx.String("retry_scope", "next_cycle"),
						logx.Error(err))
				}
				return
			}
			total.Marked += s.Marked
			total.BatchRows += s.BatchRows
			total.DebitedUsers += s.DebitedUsers
			total.ForcedUsers += s.ForcedUsers
			total.Quarantined += s.Quarantined
			total.Balances = append(total.Balances, s.Balances...)
			total.BalanceWarnings = append(total.BalanceWarnings, s.BalanceWarnings...)
		}(bucket)
	}
	wg.Wait()
	f.applySettlement(total)
	return total.Marked
}

// applySettlement 结算成功收尾：(uid,balance_after) 对定向刷新余额快照（O(1)
// 原地 Store——oracle 必改 #3，10s Reload 间隙预检新鲜度）；幽灵/隔离行计数 +
// Warn（毒用户不卡游标）。
func (f *Flusher) applySettlement(s domain.SettlementSummary) {
	for _, p := range s.Balances {
		f.bal.Set(p.UserID, p.Balance)
	}
	if f.warningSink != nil {
		for _, event := range s.BalanceWarnings {
			f.warningSink.TrySubmit(event)
		}
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
