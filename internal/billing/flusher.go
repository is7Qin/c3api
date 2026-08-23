// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

// Package billing 计费核心：service_tier 归一化 + 价格矩阵纯函数 + 余额快照
// + 计费游标消费者（F2 ledger-cursor，spec 2026-08-23）。扣费与请求路径分离。

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// LedgerStore 计费游标消费面（repository.Repository 门面实现；签名 = F2 冻结
// ABI-2 + 会话锁/零价取数两个配套面）。
type LedgerStore interface {
	// AcquireBillingLock 会话级 advisory lock：专用池连接取批前获取、持有整
	// 周期（含全部用户事务 COMMIT）后解锁释放——多实例取批互斥的唯一防线
	// （Momus M1：每事务 xact 锁形态下两实例可各自提交前取到同批未标记行 =
	// 双扣资金，明令禁止）。
	AcquireBillingLock(ctx context.Context) (release func(), ok bool, err error)
	// FetchUnbilledBatch 取未扣账本批（WHERE NOT billed AND error_type IN
	// ('none','abort') AND cost > 0 ORDER BY id LIMIT $n）。
	FetchUnbilledBatch(ctx context.Context, limit int) ([]domain.LedgerRow, error)
	// DeductOnlyAndMark 单事务：FEFO 扣减 → UPDATE billed=true, overdraft=$od
	// WHERE id=ANY($ids) AND NOT billed（用户缺失 → 跳过扣减仍标记、quarantined）。
	DeductOnlyAndMark(ctx context.Context, userID, cost int64, ids []int64) (balanceAfter int64, overdrafted, quarantined bool, err error)
	// MarkBilledBulk 幂等纯标记（cost=0 快速路径 + 终极毒行隔离）。
	MarkBilledBulk(ctx context.Context, ids []int64) error
	// FetchZeroCostIDs 取 cost=0 未标记行 id 批（快速标记取数半）。
	FetchZeroCostIDs(ctx context.Context, limit int) ([]int64, error)
	// UnbilledLag 游标积压度量（最老 unbilled 行 created_at + 行数）。
	UnbilledLag(ctx context.Context) (oldestCreated time.Time, count int64, err error)
}

// FlushConfig 消费节奏（config.BillingConfig 映射）。
type FlushConfig struct {
	FlushInterval          time.Duration // 游标轮询周期（默认 250ms）
	BalanceRefreshInterval time.Duration // 余额快照全量刷新周期
	Workers                int           // 用户组并行消费 worker 数（0 = 单 worker）
	// LogRetentionDays usage 日保留期（lag 护栏基准，cmd 接线
	// config.Usage.LogRetentionDays）：最老 unbilled 行距今超保留期 80% → 高声
	// Warn（停机护栏——消费停摆逼近分区 DROP 线提前可见）。<= 0 = 护栏禁用
	//（对齐 retention <= 0 不删除语义）。
	LogRetentionDays int
}

const (
	// fetchBatchLimit 每周期取批上限（spec §一 LIMIT 500）：单批按 UserID 分组
	// 后每组一笔事务，批规模即单周期事务数上界。
	fetchBatchLimit = 500
	// zeroCostBatchLimit cost=0 快速标记单批行数：bulk UPDATE 单语句承载
	// （ANY($1) 数组参数，无 ent 参数上限问题），429/免费消耗风暴下以少量
	// 周期收敛大积压。
	zeroCostBatchLimit = 2000
	// lagWarnFraction lag 护栏阈值 = 保留期的 80%（spec §一：超保留期 80% 高声
	// warn——留 20% 缓冲给告警响应窗口）。
	lagWarnFraction = 0.8
)

// inflightAbandonGrace 在途消费周期收尾宽限（A-P2-8-2，与 usage 包同值同语义
// ——两包各自声明）：Close 预算到期 Cancel baseCtx 后给在途周期收尾的兜底等待
// ——正常情形取消传播微秒级完成；DB 病态卡死时超时即放弃排空、Warn 截断退出
//（在途事务由已取消 baseCtx 收尾回滚，行保持 unbilled 不丢），不无界阻塞停机。
// var（非 const）：测试注入小阈值。
var inflightAbandonGrace = 500 * time.Millisecond

// userGroup 单用户消费组：同批同用户行（保序）——一组一笔 DeductOnlyAndMark
// 事务。cost 恒 = Σ rows[i].Cost（构造即和，无拆块比例公式）。
type userGroup struct {
	userID int64
	rows   []domain.LedgerRow
}

// Flusher 计费游标消费者（worker.Worker 契约，Name="billing"）。F2 重写裁决：
// 内存 pending 队列整体删除（双写元凶）——billable 行由 usage flusher 落库
// （billed=false 出生），本 worker 只消费账本游标：
//
//	每周期（FlushInterval 默认 250ms）：会话级 advisory lock 取批前获取、持有
//	整周期后释放（多实例取批互斥）→ cost=0 行首/尾批量纯标记（不走 FEFO 机器，
//	m4）→ FetchUnbilledBatch(LIMIT 500) 按 UserID 分组 → N worker 并发逐组
//	DeductOnlyAndMark 单事务（FEFO 扣减 + billed 标记原子——at-least-once 消费
//	+ 原子 = exactly-once，丢账窗口构造性为零）→ 成功定向刷新余额快照（O(1)）。
//
// 毒行：结构错误 → 组内二分重试归因（对齐 usage 包 poisonBisect）→ 单行仍失败
// = 毒行 → MarkBilledBulk 终极隔离 + QuarantinedRows 计数 + Error——游标永不
// 卡死；整库故障 → 行保持 unbilled 由 DB 天然重放（无内存回灌面）。
// Close 排空惯用法保持（loopDone/baseCtx/flushMu/inflightAbandonGrace）：等在途
// 周期结束后循环消费至游标清空（预算内）或截断退出（剩余行下次启动收敛，
// RestartConvergence）。
type Flusher struct {
	cfg     FlushConfig
	store   LedgerStore
	bal     *Balances
	log     *logx.Logger
	workers int
	// flushMu 单消费周期入口串行：ticker/Close 两处触发互斥；在途周期即其
	// 持有者（Close 排空惯用法，与 usage 包各自声明——有意重复）。
	flushMu    sync.Mutex
	started    atomic.Bool
	loopDone   chan struct{}
	closeOnce  sync.Once
	baseCtx    context.Context // ticker 路径周期的可取消父 ctx（Close 预算到期 Cancel）
	baseCancel context.CancelFunc
	// 观测原子：lastFlush 最近成功消费时刻（UnixMilli；0 = 尚未消费）；
	// unbilledN 当前 Unbilled 行数（每周期 UnbilledLag 刷新——Stats().PendingLogs
	// 语义重映射真值）；quarantined 累计隔离行数（用户缺失组 + 毒行终极隔离）；
	// lagOldestUnixMs 最老 unbilled 行 created_at（UnixMilli；0 = 游标空/未探测，
	// T5 lag 族真值）；lagWarned lag 护栏告警边沿（回落复位防刷屏）。
	lastFlush       atomic.Int64
	unbilledN       atomic.Int64
	quarantined     atomic.Int64
	lagOldestUnixMs atomic.Int64
	lagWarned       atomic.Bool
}

// NewFlusher 构造游标消费者（store = repository 门面；bal 余额快照定向刷新面）。
func NewFlusher(cfg FlushConfig, store LedgerStore, bal *Balances, log *logx.Logger) *Flusher {
	workers := cfg.Workers
	if workers <= 0 {
		workers = 1
	}
	f := &Flusher{
		cfg: cfg, store: store, bal: bal, log: log,
		workers:  workers,
		loopDone: make(chan struct{}),
	}
	f.baseCtx, f.baseCancel = context.WithCancel(context.Background())
	return f
}

// Name worker.Worker 契约（wm 按注册反向排空：flusher 最后注册最先排空）。
func (f *Flusher) Name() string { return "billing" }

func (f *Flusher) Start(ctx context.Context) error {
	if !f.started.CompareAndSwap(false, true) {
		return fmt.Errorf("billing flusher: already started")
	}
	go func() {
		defer close(f.loopDone)
		f.loop(ctx)
	}()
	return nil
}

func (f *Flusher) loop(ctx context.Context) {
	flushT := time.NewTicker(f.cfg.FlushInterval)
	defer flushT.Stop()
	refreshT := time.NewTicker(f.cfg.BalanceRefreshInterval)
	defer refreshT.Stop()
	for {
		select {
		case <-ctx.Done():
			// 最终排空由 Close 以 shutdown 预算 ctx 执行（方向 A 批次 1d，
			// 对齐 usage.go）——本 loop ctx 在 SIGTERM 即已取消，此处调
			// consumeCycle 传它会恒截断；Close 持预算 ctx 才能"正常完整刷 /
			// 到期截断"两全。
			return
		case <-flushT.C:
			f.consumeCycle(f.baseCtx)
		case <-refreshT.C:
			_ = f.bal.Reload(context.Background()) // fail-safe：内部 Warn + 保留旧快照
		}
	}
}

// consumeCycle 单消费周期（单入口：ticker/Close 共用，flushMu 串行）：
// 会话锁 → cost=0 首/尾批量标记 → 主批 FEFO 消费 → lag 护栏观测。返回本周期
// 退出游标的行数（扣费标记 + 隔离 + 零价标记；0 = 无进展）。
func (f *Flusher) consumeCycle(ctx context.Context) int64 {
	f.flushMu.Lock()
	defer f.flushMu.Unlock()

	var marked int64
	release, ok, err := f.store.AcquireBillingLock(ctx)
	switch {
	case err != nil:
		if f.log != nil && ctx.Err() == nil {
			f.log.Warn("billing cursor lock acquire failed", logx.Error(err))
		}
	case !ok:
		// 他实例在消费：本周期跳过（会话锁互斥；观测面照常刷新）。
	default:
		defer release()
		marked += f.markZeroCost(ctx)  // 首：清上一周期残留 cost=0
		marked += f.consumeBatch(ctx)  // 主批：cost>0 FEFO 消费
		marked += f.markZeroCost(ctx)  // 尾：本周期新到 cost=0
		if marked > 0 {
			f.lastFlush.Store(time.Now().UnixMilli())
		}
	}
	f.refreshLag(ctx) // 锁结果无关：他实例消费时本实例 Stats/lag 仍新鲜
	return marked
}

// consumeBatch 主批消费：FetchUnbilledBatch → 按 UserID 分组（同 user 恒同组，
// 实例内串行——FEFO 行锁跨实例安全不变）→ 分片 N worker 并发逐组单事务。
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
	groups := groupLedgerRows(rows)
	shards := make([][]*userGroup, f.workers)
	for _, g := range groups {
		i := uint64(g.userID) % uint64(f.workers)
		shards[i] = append(shards[i], g)
	}
	var wg sync.WaitGroup
	var drained atomic.Int64
	for _, shard := range shards {
		if len(shard) == 0 {
			continue
		}
		wg.Add(1)
		go func(s []*userGroup) {
			defer wg.Done()
			for _, g := range s {
				if ctx.Err() != nil {
					break // 预算到期：剩余组保持 unbilled，下周期重放（不丢不重）
				}
				drained.Add(f.consumeGroup(ctx, g))
			}
		}(shard)
	}
	wg.Wait()
	return drained.Load()
}

// consumeGroup 单用户组消费：一笔 DeductOnlyAndMark 事务（FEFO 扣减 + billed
// 标记原子）。结构错误 → 组内二分重试归因（bisectGroup）。返回本组退出游标
// 的行数（含用户缺失被标记的行；0 = 全组未推进）。
func (f *Flusher) consumeGroup(ctx context.Context, g *userGroup) int64 {
	bal, _, quarantined, err := f.store.DeductOnlyAndMark(ctx, g.userID, groupCost(g.rows), ledgerIDs(g.rows))
	if err == nil {
		f.settleGroup(g.userID, bal, quarantined, len(g.rows))
		return int64(len(g.rows))
	}
	if ctx.Err() != nil {
		return 0 // 预算到期：整组保持 unbilled，下周期重放
	}
	return f.bisectGroup(ctx, g)
}

// settleGroup 成功事务收尾：余额快照定向刷新（O(1) 原地 Store）；用户缺失
//（不变量 #1 尾语义：跳过扣减仍标记全部 ids）→ QuarantinedRows 计数 + Warn
//（毒用户不卡游标）。
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
func (f *Flusher) bisectGroup(ctx context.Context, g *userGroup) int64 {
	if len(g.rows) == 1 {
		row := g.rows[0]
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
	mid := len(g.rows) / 2
	drained := f.consumeGroup(ctx, &userGroup{userID: g.userID, rows: g.rows[:mid]})
	drained += f.consumeGroup(ctx, &userGroup{userID: g.userID, rows: g.rows[mid:]})
	return drained
}

// markZeroCost cost=0 快速标记（m4 具名机制 CostZeroFastMark）：免费消耗/出生
// 吸收态行不进 FEFO 机器——每周期首/尾各一批幂等纯标记（与取批同节拍）。
func (f *Flusher) markZeroCost(ctx context.Context) int64 {
	ids, err := f.store.FetchZeroCostIDs(ctx, zeroCostBatchLimit)
	if err != nil {
		if f.log != nil && ctx.Err() == nil {
			f.log.Warn("billing cursor zero-cost fetch failed", logx.Error(err))
		}
		return 0
	}
	if len(ids) == 0 {
		return 0
	}
	if err := f.store.MarkBilledBulk(ctx, ids); err != nil {
		if f.log != nil && ctx.Err() == nil {
			f.log.Warn("billing cursor zero-cost mark failed", logx.Error(err))
		}
		return 0
	}
	return int64(len(ids))
}

// refreshLag lag 护栏 + Stats 真值刷新：最老 unbilled 行距今超保留期 80% →
// 高声 Warn（边沿触发，回落复位防刷屏）——消费停摆逼近分区 DROP 线提前可见
//（替代旧 pendingWaterline 口径，spec §一 lag 度量源点名）。
func (f *Flusher) refreshLag(ctx context.Context) {
	oldest, count, err := f.store.UnbilledLag(ctx)
	if err != nil {
		if f.log != nil && ctx.Err() == nil {
			f.log.Warn("billing cursor lag probe failed", logx.Error(err))
		}
		return
	}
	f.unbilledN.Store(count)
	if count == 0 || oldest.IsZero() {
		f.lagOldestUnixMs.Store(0)
		f.lagWarned.Store(false)
		return
	}
	f.lagOldestUnixMs.Store(oldest.UnixMilli())
	if f.cfg.LogRetentionDays <= 0 { // 护栏禁用（对齐 retention <= 0 不删除语义）——真值照常刷新
		f.lagWarned.Store(false)
		return
	}
	retention := time.Duration(int64(f.cfg.LogRetentionDays) * int64(24*time.Hour))
	if time.Since(oldest) > time.Duration(float64(retention)*lagWarnFraction) {
		if f.lagWarned.CompareAndSwap(false, true) && f.log != nil {
			f.log.Warn("billing cursor lag exceeds retention guardrail, consumption stalled?",
				logx.Any("oldest_unbilled", oldest),
				logx.Int64("unbilled_rows", count),
				logx.Int("log_retention_days", f.cfg.LogRetentionDays))
		}
		return
	}
	f.lagWarned.Store(false)
}

// Close 幂等排空（优雅停机核心，惯用法与 usage 包同形态）：等消费 loop 退出
//（受预算约束）→ 以 flushMu 获取等待在途周期（SIGTERM 时 ticker 周期可能在途
// 占住 flushMu；Close 必须先等其结束）→ 受 shutdown ctx 预算约束的排空循环
//（逐周期消费至游标清空；n==0 = 清空/锁被他实例持有/DB 故障——均退出，剩余
// 行下次启动收敛）。ctx 到期 → Cancel baseCtx（在途事务快速失败回滚，行保持
// unbilled 不丢）+ Warn 截断退出；在途收尾超时（A-P2-8-2）→ 放弃排空 Warn
// 截断退出。未 Start 也安全（跳过 loop 等待；在途周期同样等待/排空）。
func (f *Flusher) Close(ctx context.Context) error {
	f.closeOnce.Do(func() {
		defer f.baseCancel() // flusher 关闭后 baseCtx 不得再有存活周期
		if f.started.Load() {
			select {
			case <-f.loopDone:
			case <-ctx.Done():
				if f.log != nil {
					f.log.Warn("billing flusher close: consumer loop did not exit in time")
				}
			}
		}
		acquired := make(chan struct{})
		go func() { f.flushMu.Lock(); close(acquired) }()
		select {
		case <-acquired:
			f.flushMu.Unlock()
		case <-ctx.Done():
			f.baseCancel()
			// 第二 select 兜底（A-P2-8-2，对齐 usage.go）：DB 病态卡死时取消
			// 路径本身可能被拖住——超时 → 放弃排空、Warn 截断退出（在途事务
			// 由已取消 baseCtx 回滚，行保持 unbilled 不丢）。
			select {
			case <-acquired:
				f.flushMu.Unlock()
			case <-time.After(inflightAbandonGrace):
				if f.log != nil {
					f.log.Warn("billing flusher close: in-flight cycle not finished in time, abandoning drain")
				}
				return
			}
		}
		var flushed int64
		for {
			if ctx.Err() != nil { // 预算到期：截断退出（剩余行保持 unbilled，重启收敛）
				if f.log != nil {
					f.log.Warn("billing flusher close: shutdown budget exceeded, truncated drain",
						logx.Int64("consumed_rows", flushed), logx.Int64("remaining_rows", f.unbilledN.Load()))
				}
				return
			}
			n := f.consumeCycle(ctx)
			flushed += n
			if f.unbilledN.Load() == 0 || n == 0 {
				// 游标清空，或本周期无进展（锁他实例持有/DB 故障/预算内取消）
				// ——退出不空转，剩余行由下周期/下次启动收敛；预算已到期 →
				// 归因截断 Warn（consumed/remaining 行数单位一致）。
				if ctx.Err() != nil && f.log != nil {
					f.log.Warn("billing flusher close: shutdown budget exceeded, truncated drain",
						logx.Int64("consumed_rows", flushed), logx.Int64("remaining_rows", f.unbilledN.Load()))
				}
				return
			}
		}
	})
	return nil
}

// groupLedgerRows 按 UserID 保序分组（确定性——测试断言与分片均不依赖 map 迭代序）。
func groupLedgerRows(rows []domain.LedgerRow) []*userGroup {
	byUID := make(map[int64]*userGroup, 16)
	out := make([]*userGroup, 0, 16)
	for _, r := range rows {
		g, ok := byUID[r.UserID]
		if !ok {
			g = &userGroup{userID: r.UserID}
			byUID[r.UserID] = g
			out = append(out, g)
		}
		g.rows = append(g.rows, r)
	}
	return out
}

// groupCost 组内成本和（cost == Σ rows.Cost 不变量，逐行累加——禁止按比例公式）。
func groupCost(rows []domain.LedgerRow) int64 {
	var cost int64
	for _, r := range rows {
		cost += r.Cost
	}
	return cost
}

// ledgerIDs 组内行 id 序列（DeductOnlyAndMark 标记面实参）。
func ledgerIDs(rows []domain.LedgerRow) []int64 {
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}
