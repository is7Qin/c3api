// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package usage 承载请求明细的异步落库（规格 §7.2/§10.5）、key 额度增量回写
// （quota 在线保留——独立于统计，不随离线聚合搬移）、usagelog 保留策略
// （retention worker，Phase 5 T4.5：按日分区 DROP 清理）与离线聚合 worker
// （stats_agg.go：spec 2026-08-14 使用量统计离线聚合化）。
// 请求路径**零统计计算**（用户裁决 2026-08-14）：统计内存桶机制整体删除，
// Record 锁内仅明细 append + quota 原子累加；usage_stats 由离线聚合 worker
// 每周期从 DB 重建（DELETE+INSERT 覆盖语义，见 stats_agg.go）。明细经无界
// pending 批量落库（O1 管道化：Record 永不阻塞——此前有界 channel cap 16384
// 饱和阻塞发送是压测 off 路径 16.4k goroutine 卡 chan send、healthz inflight
// 31-33k @10k 幽灵根因，O3 复测定位 2026-08-09；崩溃丢 ≤1 flush 窗口的崩溃
// 等价语义不变，pending 内存即唯一积压面，由水线 Warn 观测）。
package usage

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

type UsageConfig struct {
	BatchSize          int
	FlushInterval      time.Duration
	QuotaFlushInterval time.Duration // quota 增量批量回写 cadence
	Workers            int           // flush 并行 worker 数（0 = 单 worker；O1 模式分片并行）
	// StatsAggInterval 离线聚合周期（spec 2026-08-14；config usage.stats_agg_
	// interval，默认 5m；0 = 禁用聚合——不装配聚合 worker 的等价语义）。
	StatsAggInterval time.Duration
}

type LogInserter interface {
	InsertBatch(ctx context.Context, logs []*domain.UsageLog) error
}

// QuotaWriter 批量回写 key 额度消耗（增量；内存权威，DB 滞后 ≤ flush 间隔）。
// 由 proxy 的 gate 计数 + 本 Recorder 的 flush 节奏落库（Phase 3a：额度后扣）。
type QuotaWriter interface {
	AddQuotaUsed(ctx context.Context, deltas map[int64]int64) error
}

// quotaBatchSize 单条批量 SQL 的 key 数（PG 参数上限 65535：500 ×（CASE 两参数
// + IN 一参数）≈ 1.5k，安全）。usage 层负责分块——失败回灌以块为原子单位，
// 防部分成功重复计数。
const quotaBatchSize = 500

// pendingWaterline 明细 pending 条数水线：超过 → Warn（可观测，非反压——
// Record 永不阻塞，pending 内存即唯一积压面；与 billing Flusher 同模式）。
// var（非 const）：测试注入小阈值，默认 1M 不变，后续可配置化。
var pendingWaterline int64 = 1_000_000

// inflightAbandonGrace 在途批次收尾宽限（A-P2-8-2）：Close 预算到期 Cancel
// baseCtx 后给在途批次收尾的兜底等待——正常情形取消传播微秒级完成（完整排空
// 语义不变）；DB 病态卡死（database/sql 取消路径本身被拖住）时超时即放弃排空、
// Warn 截断退出（在途批次由已取消 baseCtx 收尾回灌不丢），不无界阻塞停机。
// var（非 const）：测试注入小阈值。
var inflightAbandonGrace = 500 * time.Millisecond

type Recorder struct {
	cfg       UsageConfig
	logs      LogInserter
	quota     QuotaWriter // 可选（nil = 不回写额度）
	log       *logx.Logger
	workers   int
	mu        sync.Mutex // 保护 pending/quotaUsed（Record 与 flush 换批/回灌并发）
	pending   []*domain.UsageLog
	quotaUsed map[int64]int64 // key_id → 待回写 token 增量
	pendingN  atomic.Int64    // pending 明细条数（水线观测 + Close Warn 单位；换批/回灌同步增减）
	warned    atomic.Bool     // 水线越过告警边沿（回落复位，避免重复刷屏）
	// flushMu 单 flush 入口串行：日志 flush（flushLogs）与额度回写（flushQuota）
	// 共用同一互斥锁——Close 的在途屏障需要（"是否有批次在途"即"flushMu 是否被
	// 占"），这是单一互斥锁的代价（评审 I-1 耦合）：DB 故障恢复后日志积压巨大时，
	// 单次 flushLogs 占锁可致额度回写排队、延迟同幅放大（额度持久化滞后，
	// **非丢数据**）。ticker/Close 两处触发互斥；在途批次即其持有者。
	flushMu    sync.Mutex
	failCounts []int // 分片级 flush 失败计数（A-P2-8-4 二分隔离后为复位面保留）：毒丸
	// 行定位丢弃后复位、成功推进复位；整库故障（两半都失败）**不累计**——DB
	// 恢复即重试成功，消除故障期进行式丢明细（旧实现每 5 周期丢 1 chunk/分片）。
	// 仅失败归因/成功路径写（Record 热路径零触碰）。安全：flushLogs 由 flushMu
	// 串行，单次调用内每分片恰一个 goroutine 写自己的槽位，wg.Wait 后才进入
	// 下一轮 flush。
	startOnce   atomic.Bool
	loopDone    chan struct{} // Start 的两个 loop 全部退出后关闭
	closeOnce   sync.Once
	closed      atomic.Bool // Close 完成后置位（I-4）：后续 Record 走 Warn 一次路径
	closeWarned atomic.Bool // closed 后首次 Record 的 Warn 边沿（只告警一次，防刷屏）
	// O2 停机：ticker 路径批次的可取消父 ctx（常时 = Background 语义；Close
	// 预算到期 Cancel → 在途落库快速失败回灌，不丢）。baseCtx 仅经 baseCancel
	// 修改（Close 内单写者），loop/Close 并发读安全。
	baseCtx    context.Context
	baseCancel context.CancelFunc
}

func New(cfg UsageConfig, logs LogInserter, log *logx.Logger) *Recorder {
	workers := cfg.Workers
	if workers <= 0 {
		workers = 1
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 1
	}
	r := &Recorder{
		cfg:        cfg,
		logs:       logs,
		log:        log,
		workers:    workers,
		failCounts: make([]int, workers),
		quotaUsed:  make(map[int64]int64),
		loopDone:   make(chan struct{}),
	}
	r.baseCtx, r.baseCancel = context.WithCancel(context.Background())
	return r
}

// SetQuotaWriter 注入额度回写器（装配期调用；nil = 关闭回写）。
func (r *Recorder) SetQuotaWriter(q QuotaWriter) {
	r.mu.Lock()
	r.quota = q
	r.mu.Unlock()
}

// Name 满足 worker.Worker 契约（Global Constraints #5）；重复 Start 幂等。
func (r *Recorder) Name() string { return "usage" }

func (r *Recorder) Start(ctx context.Context) error {
	if !r.startOnce.CompareAndSwap(false, true) {
		return fmt.Errorf("recorder: already started")
	}
	go func() {
		defer close(r.loopDone)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); r.logWriterLoop(ctx) }()
		go func() { defer wg.Done(); r.quotaFlushLoop(ctx) }()
		wg.Wait()
	}()
	return nil
}

// Record 记录一次放行路径明细（非 billed 行）：短锁归并 pending（无界 slice
// append，O(1) 摊还）+ quota 原子累加——**永不阻塞**（无 channel：此前有界
// channel cap 16384 饱和阻塞发送是 off 路径 16.4k goroutine 卡 chan send 幽灵
// 根因；HTTP 层过载保护由 max_inflight 兜底，pending 内存由水线 Warn 观测，
// 崩溃丢 ≤1 flush 窗口语义不变）。热路径零额外开销：closed 检查为 1 次
// atomic.Load（I-4）。**零统计计算**（spec 2026-08-14）：统计桶机制整体删除，
// 锁内仅剩明细 append + quotaUsed 累加两个 O(1) 操作。
func (r *Recorder) Record(l *domain.UsageLog) {
	if r.closed.Load() { // Close 完成后无消费者——防御性缺口（评审 I-4）：
		// Warn 恰好一次（不刷屏）；明细仍聚合入 pending **不丢**（驻留内存由
		// 本 Warn 观测，worker 管理器顺序保证正常停机不触发）。
		if r.closeWarned.CompareAndSwap(false, true) && r.log != nil {
			r.log.Warn("usage record after close: detail retained in memory (no consumer)",
				logx.String("request_id", l.RequestID))
		}
	}
	r.mu.Lock()
	r.pending = append(r.pending, l)
	if l.KeyID > 0 { // quota 在线保留（独立于统计；本处累加是 quota 唯一生产增量入口——计费 worker 只动余额不动配额）
		r.quotaUsed[l.KeyID] += l.TotalTokens
	}
	n := r.pendingN.Add(1)
	r.mu.Unlock()
	if n > pendingWaterline && r.warned.CompareAndSwap(false, true) {
		if r.log != nil {
			r.log.Warn("usage pending exceeds waterline", logx.Int64("pending_logs", n), logx.Int64("waterline", pendingWaterline))
		}
	}
}

// AddQuota 累加 key 额度增量并入同一 quotaUsed map、同一回写路径（不回桶、
// 不落统计）。旧世界 billed 行经 billing Flusher.Record 走此入口；F2 后计费
// worker 只动余额不动配额——Record 对 KeyID>0 行的累加是唯一生产增量入口，
// 本方法当前仅测试/手工注入面。
func (r *Recorder) AddQuota(keyID int64, delta int64) {
	if keyID <= 0 || delta == 0 {
		return
	}
	r.mu.Lock()
	r.quotaUsed[keyID] += delta
	r.mu.Unlock()
}

// Pending 返回尚未落库的明细条数（测试与积压观测用）。
func (r *Recorder) Pending() int { return int(r.pendingN.Load()) }

func (r *Recorder) logWriterLoop(ctx context.Context) {
	t := time.NewTicker(r.cfg.FlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// 最终排空由 Close 以 shutdown 预算 ctx 执行（O2 停机纪律）——本
			// loop ctx 在 SIGTERM 即已取消，此处 flush 传它会恒截断丢全部明细；
			// Close 持预算 ctx 才能"正常完整刷 / 到期截断"两全。
			return
		case <-t.C:
			r.flushLogs(r.baseCtx)
		}
	}
}

// flushLogs 换批 + 并行落库（O1 管道化消费侧）：锁内 swap 整个 pending（换新
// slice，flush 期间新日志进新 pending 零阻塞）→ 按 userID 分片（同 user 恒同
// worker）→ N worker 并发逐 chunk InsertBatch（chunk = cfg.BatchSize；ent
// CreateBulk 参数上限 PG 65535，500 × ~20 列安全）→ 失败 chunk 二分隔离归因
// （A-P2-8-4，见 poisonBisect）后连同其后剩余一并回灌 pending（不丢，下次
// flush 重试；DB 故障不锤击——本 shard 停止）。返回本批成功落库条数（Close
// 汇总作 Warn 诊断）。
// flushMu 串行单入口（ticker/Close 两处触发共用；在途批次即其持有者——Close
// 以获取 flushMu 等待在途批次）。**互斥耦合（评审 I-1）**：flushLogs 与
// flushQuota 共用 flushMu（Close 在途屏障需要）——DB 故障积压时单次 flushLogs
// 占锁可致额度回写延迟同幅放大（非丢数据）。毒丸行止损
// （A-P2-8-4 二分隔离替代评审 I-3 的整 chunk 止损）：单行毒丸 → 二分定位后仅
// 丢弃该行（Error + request_id），其余行照常落库；整库故障 → 回灌不丢 + 不
// 累计失败计数（DB 恢复即重试成功，消除故障期进行式丢明细）。
func (r *Recorder) flushLogs(ctx context.Context) int64 {
	r.flushMu.Lock()
	defer r.flushMu.Unlock()

	r.mu.Lock()
	if len(r.pending) == 0 {
		r.mu.Unlock()
		return 0
	}
	pend := r.pending
	r.pending = nil
	r.pendingN.Add(-int64(len(pend)))
	if r.pendingN.Load() < pendingWaterline {
		r.warned.Store(false)
	}
	r.mu.Unlock()

	if ctx.Err() != nil { // 预算到期：不落库，原样回灌（Close 截断路径决定放弃）
		r.refillLogs(pend)
		return 0
	}

	shards := make([][]*domain.UsageLog, r.workers)
	for i := range shards {
		shards[i] = make([]*domain.UsageLog, 0, len(pend)/r.workers+1)
	}
	for _, l := range pend {
		shards[uint64(l.UserID)%uint64(r.workers)] = append(shards[uint64(l.UserID)%uint64(r.workers)], l)
	}

	var wg sync.WaitGroup
	var drained atomic.Int64
	for si, shard := range shards {
		if len(shard) == 0 {
			continue
		}
		wg.Add(1)
		go func(si int, s []*domain.UsageLog) {
			defer wg.Done()
			for start := 0; start < len(s); start += r.cfg.BatchSize {
				if ctx.Err() != nil { // 预算到期：剩余回灌
					r.refillLogs(s[start:])
					return
				}
				end := min(start+r.cfg.BatchSize, len(s))
				if err := r.logs.InsertBatch(ctx, s[start:end]); err != nil {
					// 毒丸止损二分隔离（A-P2-8-4）：整 chunk 失败不再直接计数/
					// 丢弃（旧实现整 chunk 500 行丢弃，单行毒丸连带 499 行；且
					// 丢弃后立即复位不区分失败原因 → DB 持续故障时每 5 周期丢
					// 1 chunk/分片，故障期进行式丢明细）——二分重试归因（失败
					// 路径非热路径，性能不敏感）：
					//   - 单半成功另半失败 → 继续二分定位毒丸行 → 丢弃该行
					//     （Error + request_id）+ 其余行已由二分过程成功落库 +
					//     shard 剩余回灌 + 计数复位；
					//   - 两半都失败（整库故障）→ 未落库行全部回灌（不丢）+
					//     不累计失败计数——DB 恢复即重试成功。
					if end-start == 1 {
						// 单行 chunk 无法二分归因（无同级半可对照）——按整库
						// 故障语义回灌不丢（BatchSize=1 仅测试形态；防故障期
						// 单行误丢）。
						if r.log != nil {
							r.log.Warn("usage batch insert failed (DB-wide), refilled", logx.Error(err))
						}
						r.refillLogs(s[start:])
						return
					}
					poison, refill, n := r.poisonBisect(ctx, s[start:end])
					if poison != nil {
						if r.failCounts[si] > 0 {
							r.failCounts[si] = 0 // 毒丸定位隔离 → 计数复位
						}
						if r.log != nil {
							r.log.Error("usage batch insert failed, dropping poison row",
								logx.Error(err), logx.String("request_id", poison.RequestID),
								logx.Int("dropped_logs", 1))
						}
					} else if r.log != nil {
						r.log.Warn("usage batch insert failed (DB-wide), refilled", logx.Error(err))
					}
					if len(refill) > 0 {
						r.refillLogs(refill) // 整库故障：未落库行回灌（不丢）
					}
					r.refillLogs(s[end:]) // 其后剩余回灌（不丢）
					drained.Add(n)        // 二分过程成功落库的行数（毒丸定位：其余全部）
					return
				}
				if r.failCounts[si] > 0 {
					r.failCounts[si] = 0 // 成功推进复位（仅对曾失败的 shard 写）
				}
				drained.Add(int64(end - start))
			}
		}(si, shard)
	}
	wg.Wait()
	return drained.Load()
}

// poisonBisect 毒丸止损二分隔离（A-P2-8-4）：chunk 整体 InsertBatch 已失败
// （调用方保证 len ≥ 2）后的二分重试归因——失败路径非热路径，性能不敏感但
// 逻辑可测。返回：
//   - poison != nil：已定位毒丸行（该行未落库，由调用方丢弃）；其余行均已由
//     二分过程的成功半落库（"其余入库"，不再整 chunk 连带丢弃）；
//   - refill：未落库需回灌的行（两半都失败 = 整库故障时 = chunk 全部；调用方
//     回灌不丢、不累计失败计数——DB 恢复即重试成功）；
//   - drained：二分过程成功落库的行数。
//
// 递归不变量：chunk 已知含毒（父级某半成功对照保证）。纯瞬态失败（无毒丸行）
// 时逐层对照使最后一行被判为毒丸——len==1 分支重试该行消歧：重试成功 = 瞬态
// 失败，该行照常落库；重试仍失败 = 毒丸行。单行 chunk（BatchSize=1）无同级半
// 可对照，由调用方按整库故障语义回灌（防故障期误丢）。
func (r *Recorder) poisonBisect(ctx context.Context, chunk []*domain.UsageLog) (poison *domain.UsageLog, refill []*domain.UsageLog, drained int64) {
	if len(chunk) == 1 {
		// 同级半已成功（父级对照）：重试区分瞬态失败与毒丸行
		if err := r.logs.InsertBatch(ctx, chunk); err == nil {
			return nil, nil, 1
		}
		return chunk[0], nil, 0
	}
	mid := len(chunk) / 2
	left, right := chunk[:mid], chunk[mid:]
	if err := r.logs.InsertBatch(ctx, left); err == nil {
		// 左半成功 → 毒丸在右半；左半已落库，继续二分右半
		p, rf, d := r.poisonBisect(ctx, right)
		return p, rf, d + int64(len(left))
	}
	if err := r.logs.InsertBatch(ctx, right); err == nil {
		// 右半成功 → 毒丸在左半；右半已落库，继续二分左半
		p, rf, d := r.poisonBisect(ctx, left)
		return p, rf, d + int64(len(right))
	}
	// 两半都失败 → 整库故障：全部未落库行回灌（不丢），调用方不累计失败计数。
	// （同节点双毒丸行亦归入此类：下轮 flush 整体重试仍两半都失败——继续回灌
	// 不丢，可观测性由 pending 增长 + DB-wide Warn 覆盖。）
	return nil, chunk, 0
}

// refillLogs 失败/截断回灌：合并回当前 pending（锁内 append——flush 期间
// Record 进新 slice，回灌与 Record 并发安全）。
func (r *Recorder) refillLogs(logs []*domain.UsageLog) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending = append(r.pending, logs...)
	r.pendingN.Add(int64(len(logs)))
}

// quotaFlushLoop quota 专用回写循环（spec 2026-08-14 评审 P1-C：统计 flush 整体
// 删除后 AddQuotaUsed 唯一调用方消失——Recorder 保留 quota 专用 flush，驱动
// cadence 复用既有 QuotaFlushInterval ticker）。
func (r *Recorder) quotaFlushLoop(ctx context.Context) {
	t := time.NewTicker(r.cfg.QuotaFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// 最终回写由 Close 以 shutdown 预算 ctx 执行（O2 停机修复）——本
			// loop ctx 在 SIGTERM 即已取消，传它会恒截断丢全部额度增量；Close
			// 持预算 ctx 才能"正常完整刷 / 到期截断"两全。跳过也消除与 Close
			// 并发抢换批的竞态（谁先 swap 谁独占数据，后者见空）。
			return
		case <-t.C:
			r.flushQuota(r.baseCtx)
		}
	}
}

// flushQuota quota 增量批量回写（换批 + 落库，受 ctx 预算约束——O2 停机修复：
// O1 复测 Close 用 Background 逐 key AddQuotaUsed 独占 3.8 分钟吃掉停机预算
// 尾部，main 卡死）：
//   - 锁内只换 map 引用（O(1)，A-P2-8-3）：换批 = 交换引用 + 建新 map，锁外
//     遍历 old 分组（换出后无写者——写者只碰新 map，无数据竞争）；失败整组
//     回灌合并到下一批（下次 flush 重试，不丢）；
//   - 按 quotaBatchSize 分组批量回写（单组一条 raw SQL CASE 更新——10k 逐 key
//     轮询是 #15 验收统计面慢 flush 3-5min 周期根因之一）；逐组前查 ctx.Err()，
//     到期 → 截断（丢弃，崩溃等价语义）+ Warn（已刷/剩余组键数）。
//
// 截断丢的是额度刷新（内存权威、DB 滞后 ≤ flush 间隔的崩溃等价语义），**非
// 计费扣费**——cost 经 billing Flusher 落库，与本额度面同窗口、互不影响。
// 统计桶落库已整体删除（离线聚合 worker 承担，见 stats_agg.go）。
func (r *Recorder) flushQuota(ctx context.Context) {
	r.flushMu.Lock()
	defer r.flushMu.Unlock()

	r.mu.Lock()
	quota := r.quotaUsed
	r.quotaUsed = make(map[int64]int64)
	qw := r.quota
	r.mu.Unlock()

	if qw == nil || len(quota) == 0 {
		return
	}
	var flushedKeys, remainingKeys int
	keys := make([]int64, 0, len(quota))
	for k, v := range quota {
		if v != 0 { // 与 repo 语义一致：零增量无回写价值
			keys = append(keys, k)
		}
	}
	for start := 0; start < len(keys); start += quotaBatchSize {
		end := min(start+quotaBatchSize, len(keys))
		group := make(map[int64]int64, end-start)
		for _, k := range keys[start:end] {
			group[k] = quota[k]
		}
		if ctx.Err() != nil { // 预算到期：截断（丢弃，不落库不回灌）
			remainingKeys += len(group)
			continue
		}
		if err := qw.AddQuotaUsed(ctx, group); err != nil {
			if r.log != nil {
				r.log.Warn("usage quota writeback failed", logx.Error(err))
			}
			r.mu.Lock()
			for k, v := range group {
				r.quotaUsed[k] += v
			}
			r.mu.Unlock()
			continue
		}
		flushedKeys += len(group)
	}
	if remainingKeys > 0 && r.log != nil {
		r.log.Warn("usage quota flush truncated on shutdown budget",
			logx.Int("quota_flushed_keys", flushedKeys), logx.Int("quota_remaining_keys", remainingKeys))
	}
}

// Close 幂等排空（优雅停机核心）：等聚合 goroutine 退出（受预算约束）→ 以
// flushMu 获取等待在途批次（SIGTERM 时 ticker 批次可能已在途占住 flushMu 且
// pending 已 swap；Close 必须先等其结束，否则 drain 循环见 pendingN==0 会
// 静默提前返回，在途批次无界运行——O1 复测根因 1）→ 受 shutdown ctx 预算
// 约束的排空循环（此时无在途批次、flushMu 无竞争）。正常情形完整排空语义
// 不变（无 deadline ctx = 全部落库）；ctx 到期 → Cancel baseCtx（在途落库
// 快速失败回灌，不丢）+ Warn（flushed/remaining 条数单位一致）+ 截断退出，
// 不阻塞停机；在途批次收尾超时（A-P2-8-2）→ 放弃排空、Warn 截断退出（在途
// 由已取消 baseCtx 收尾回灌不丢）。额度面由 flushQuota 以本 ctx 预算收尾
// （到期截断 + Warn）。未 Start 也安全（跳过聚合等待；在途 flush 与 pending
// 残留同样等待/排空）。
func (r *Recorder) Close(ctx context.Context) error {
	r.closeOnce.Do(func() {
		defer r.baseCancel() // recorder 关闭后 baseCtx 不得再有存活批次
		if r.startOnce.Load() {
			// 等聚合 goroutine 退出（受预算约束）。SIGTERM 时 loop 可能阻塞在
			// ticker flush（baseCtx 批次在途）——loopDone 待其批次结束 + 末次
			// flush 后才关闭；预算到期 → Warn + 继续（在途批次由下面 flushMu
			// 等待强制取消）。
			select {
			case <-r.loopDone:
			case <-ctx.Done():
				if r.log != nil {
					r.log.Warn("usage close: loops did not exit in time")
				}
			}
		}
		// 等在途批次（有界）：flushLogs/flushQuota 由 flushMu 串行——"是否有
		// 批次在途"即"flushMu 是否被占"；尝试获取 flushMu：拿到即无在途批次
		// （其退出前必释放），预算内等其自然完成（完整排空语义不变）；到期 →
		// Cancel baseCtx 强制在途落库快速失败（回灌不丢），等批次收尾后走截
		// 断路径。未 Start 时无竞争立即拿到（此前测试直接调 flush 的在途批次
		// 同样被等待）。
		acquired := make(chan struct{})
		go func() { r.flushMu.Lock(); close(acquired) }()
		select {
		case <-acquired:
			r.flushMu.Unlock()
		case <-ctx.Done():
			r.baseCancel()
			// 第二 select 兜底（A-P2-8-2，对齐 loopDone 等待模式）：预算到期后
			// 等在途批次收尾——正常情形取消传播微秒级完成，预算内等其自然
			// 完成（完整排空语义不变，随后排空循环照常 Warn 截断）；但 DB
			// 病态卡死时 database/sql 取消路径本身可能被拖住，`<-acquired`
			// 无界等待违反"到期截断退出、不阻塞停机"承诺——超时 → 放弃排空、
			// Warn 截断退出（在途批次由已取消 baseCtx 收尾回灌不丢；后续排空
			// 循环/额度收尾都会被 flushMu 挡住，不可再触碰）。
			select {
			case <-acquired:
				r.flushMu.Unlock()
			case <-time.After(inflightAbandonGrace):
				if r.log != nil {
					r.log.Warn("usage close: in-flight flush not finished in time, abandoning drain")
				}
				r.closed.Store(true)
				return
			}
		}
		// 排空明细（预算内循环；到期 → Warn 截断退出——flushed/remaining 均
		// 为明细条数，单位一致）。
		var flushed int64
		for r.pendingN.Load() > 0 {
			if ctx.Err() != nil { // 预算到期：截断退出（剩余明细由 flushLogs 截断回灌，丢 ≤1 flush 窗口）
				if r.log != nil {
					r.log.Warn("usage close: shutdown budget exceeded, truncated drain",
						logx.Int64("flushed_logs", flushed), logx.Int64("remaining_logs", r.pendingN.Load()))
				}
				break
			}
			flushed += r.flushLogs(ctx)
		}
		// 额度面收尾：flushQuota 内部受 ctx 预算约束（到期 → 截断 Warn，崩溃
		// 等价语义；正常完整刷）。预算已到期时此处即"额度截断"告警面。
		r.flushQuota(ctx)
		// Close 完成后置位 closed（评审 I-4）：后续 Record 走 Warn 一次路径
		//（明细仍聚合入 pending 不丢，驻留内存由 Warn 观测）。
		r.closed.Store(true)
	})
	return nil
}
