// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

// drain.go 排空消费机制面（F2-opt，spec-f2-cursor-throughput D1/D2/D3）：取批
// 内存路由 → 分片打包 → chunk 单事务消费 → 失败两层二分降级。周期编排（锁/
// 节流/Close 协议）见 flusher.go；chunk 事务本体见 repository.BillingRepo.

import (
	"context"
	"sync"
	"sync/atomic"
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

// drainLoop 排空式消费（D2）：循环 取批→路由→消费 直至空批返回、零进展、
// 周期预算到期或 ctx.Err()——一批一 tick 的节奏概念废除，FlushInterval 仅在
// 游标空时作为空转间隔。
func (f *Flusher) drainLoop(ctx context.Context) int64 {
	deadline := time.Now().Add(drainCycleBudget)
	var drained int64
	for ctx.Err() == nil {
		n := f.consumeBatch(ctx)
		if n == 0 {
			return drained // 空批 / 取批失败 / 整库故障重放：本周期收尾（不空转）
		}
		drained += n
		if drainCycleBudget > 0 && time.Now().After(deadline) {
			return drained // 预算到期：让位 ticker/refreshT，剩余积压下一 tick 续排
		}
	}
	return drained
}

// consumeBatch 单批取数 + 内存路由消费（D1 单取批面）：cost<=0 行一次
// MarkBilledBulk 纯标记（吸收态/免费行零资金移动语义不变）；cost>0 行进扣费面。
// 返回本批退出游标的行数（0 = 空批/取批失败/零进展）。
func (f *Flusher) consumeBatch(ctx context.Context) int64 {
	rows, err := f.store.FetchUnbilledBatch(ctx, fetchBatchLimit)
	if err != nil {
		if f.log != nil && ctx.Err() == nil {
			f.log.Warn("billing cursor fetch failed", logx.Error(err))
		}
		return 0
	}
	if len(rows) == 0 {
		return 0
	}
	var paid []domain.LedgerRow
	var zeroIDs []int64
	for _, r := range rows {
		if r.Cost <= 0 {
			zeroIDs = append(zeroIDs, r.ID)
		} else {
			paid = append(paid, r)
		}
	}
	var marked int64
	if len(zeroIDs) > 0 {
		if err := f.store.MarkBilledBulk(ctx, zeroIDs); err != nil {
			if f.log != nil && ctx.Err() == nil {
				f.log.Warn("billing cursor zero-cost mark failed", logx.Error(err))
			}
		} else {
			marked += int64(len(zeroIDs))
		}
	}
	if len(paid) > 0 {
		marked += f.consumeGroups(ctx, groupLedgerRows(paid))
	}
	return marked
}

// consumeGroups 扣费面分片并发消费：shard key userID%workers 不变（同用户恒同
// 分片且片内串行——FEFO 行锁跨实例安全不变）；片内连续组打包 ≤chunkUsersLimit
// 用户/chunk（D3）逐块单事务。
func (f *Flusher) consumeGroups(ctx context.Context, groups []*domain.LedgerGroup) int64 {
	shards := make([][]*domain.LedgerGroup, f.workers)
	for _, g := range groups {
		i := uint64(g.UserID) % uint64(f.workers)
		shards[i] = append(shards[i], g)
	}
	var wg sync.WaitGroup
	var drained atomic.Int64
	for _, shard := range shards {
		if len(shard) == 0 {
			continue
		}
		wg.Add(1)
		go func(s []*domain.LedgerGroup) {
			defer wg.Done()
			for _, chunk := range packChunks(s) {
				if ctx.Err() != nil {
					break // 预算到期：剩余组保持 unbilled，下周期重放（不丢不重）
				}
				drained.Add(f.consumeChunk(ctx, chunk))
			}
		}(shard)
	}
	wg.Wait()
	return drained.Load()
}

// packChunks 片内连续组打包：≤chunkUsersLimit 用户/chunk（D3 chunk 打包策略；
// 保序切分——确定性，测试断言与降级路径均不依赖 map 迭代序）。
func packChunks(groups []*domain.LedgerGroup) [][]*domain.LedgerGroup {
	chunks := make([][]*domain.LedgerGroup, 0, (len(groups)+chunkUsersLimit-1)/chunkUsersLimit)
	for start := 0; start < len(groups); start += chunkUsersLimit {
		end := min(start+chunkUsersLimit, len(groups))
		chunks = append(chunks, groups[start:end])
	}
	return chunks
}

// consumeChunk 单 chunk 消费：多组 chunk 走一笔 DeductGroupsAndMark 事务（逐组
// FEFO 扣减 + 合并标记原子——原子域放大到 chunk，崩溃回滚整块重放）；单例组
// 直走单组事务面（consumeGroup——等价语义，免一次已知失败形态的重复尝试）。
// 结构错误 → 组为单位折半降级（两层二分的外层）；单例组失败 → 既有
// bisectGroup 行级机制（内层，正交复用）。返回本 chunk 退出游标的行数。
func (f *Flusher) consumeChunk(ctx context.Context, chunk []*domain.LedgerGroup) int64 {
	if len(chunk) == 1 {
		return f.consumeGroup(ctx, chunk[0])
	}
	outcomes, err := f.store.DeductGroupsAndMark(ctx, groupRows(chunk))
	if err == nil {
		var n int64
		for i, g := range chunk {
			f.settleGroup(g.UserID, outcomes[i].BalanceAfter, outcomes[i].Quarantined, len(g.Rows))
			n += int64(len(g.Rows))
		}
		return n
	}
	if ctx.Err() != nil {
		return 0 // 预算到期：整块保持 unbilled，下周期重放
	}
	mid := len(chunk) / 2
	return f.consumeChunk(ctx, chunk[:mid]) + f.consumeChunk(ctx, chunk[mid:])
}

// groupRows chunk 组指针切片 → 值切片（DeductGroupsAndMark store 面实参）。
func groupRows(chunk []*domain.LedgerGroup) []domain.LedgerGroup {
	out := make([]domain.LedgerGroup, len(chunk))
	for i, g := range chunk {
		out[i] = *g
	}
	return out
}

// consumeGroup 单用户组消费（chunk 二分的单例终点）：一笔 DeductOnlyAndMark
// 事务（FEFO 扣减 + billed 标记原子）。结构错误 → 组内二分重试归因
// （bisectGroup）。返回本组退出游标的行数（含用户缺失被标记的行；0 = 全组未推进）。
func (f *Flusher) consumeGroup(ctx context.Context, g *domain.LedgerGroup) int64 {
	bal, _, quarantined, err := f.store.DeductOnlyAndMark(ctx, g.UserID, groupCost(g.Rows), ledgerIDs(g.Rows))
	if err == nil {
		f.settleGroup(g.UserID, bal, quarantined, len(g.Rows))
		return int64(len(g.Rows))
	}
	if ctx.Err() != nil {
		return 0 // 预算到期：整组保持 unbilled，下周期重放
	}
	return f.bisectGroup(ctx, g)
}

// settleGroup 成功事务收尾：余额快照定向刷新（O(1) 原地 Store）；用户缺失
// （不变量 #1 尾语义：跳过扣减仍标记全部 ids）→ QuarantinedRows 计数 + Warn
// （毒用户不卡游标）。
func (f *Flusher) settleGroup(userID, bal int64, quarantined bool, n int) {
	if quarantined {
		f.quarantined.Add(int64(n))
		if f.log != nil {
			f.log.Warn("billing cursor: user missing, rows marked without deduction",
				logx.Int64("user_id", userID), logx.Int("rows", n))
		}
		return
	}
	f.bal.Set(userID, bal)
}

// bisectGroup 毒行二分隔离（对齐 usage 包 poisonBisect 形态；游标无内存回灌
// ——失败半保持 unbilled 由 DB 天然重放）：整组失败后折半重试（每半独立事务，
// 成功半原子推进）；两半都失败 = 整库故障 → 放弃本组（下周期重放，不锤击不
// 误隔离）；二分至单行重试仍失败 = 毒行 → MarkBilledBulk 终极隔离（写销该行
// 计费 + QuarantinedRows 计数 + Error）——游标永不卡死。
func (f *Flusher) bisectGroup(ctx context.Context, g *domain.LedgerGroup) int64 {
	if len(g.Rows) == 1 {
		row := g.Rows[0]
		// 重试一次消歧瞬态失败（父级对照保证含毒——同 usage poisonBisect
		// len==1 分支）：成功 = 瞬态，正常收尾；仍失败 = 毒行 → 隔离。
		bal, _, quarantined, err := f.store.DeductOnlyAndMark(ctx, row.UserID, row.Cost, []int64{row.ID})
		if err == nil {
			f.settleGroup(row.UserID, bal, quarantined, 1)
			return 1
		}
		if ctx.Err() != nil {
			return 0
		}
		if merr := f.store.MarkBilledBulk(ctx, []int64{row.ID}); merr != nil {
			if f.log != nil {
				f.log.Warn("billing cursor: poison row isolation failed, retried next cycle",
					logx.Error(merr), logx.Int64("usage_log_id", row.ID))
			}
			return 0 // 连标记都失败 = 整库故障：行保持 unbilled 下周期重放
		}
		f.quarantined.Add(1)
		if f.log != nil {
			f.log.Error("billing cursor: poison row isolated without deduction",
				logx.Int64("usage_log_id", row.ID), logx.Int64("user_id", row.UserID), logx.Int64("cost", row.Cost))
		}
		return 1 // 该行未扣费但已退出游标（隔离写销，QuarantinedRows 另计）
	}
	mid := len(g.Rows) / 2
	drained := f.consumeGroup(ctx, &domain.LedgerGroup{UserID: g.UserID, Rows: g.Rows[:mid]})
	drained += f.consumeGroup(ctx, &domain.LedgerGroup{UserID: g.UserID, Rows: g.Rows[mid:]})
	return drained
}
