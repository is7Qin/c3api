// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

// Package billing 计费核心：service_tier 归一化 + 价格矩阵纯函数 + 余额快照
// + 计费游标消费者（F2 ledger-cursor，spec 2026-08-23；F2-opt 吞吐极致化，
// spec-f2-cursor-throughput 2026-08-24）。扣费与请求路径分离。

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
// ABI-2 + 会话锁配套面 + F2-opt D3 chunk 事务面）。
type LedgerStore interface {
	// AcquireBillingLock 会话级 advisory lock：专用池连接取批前获取、持有整
	// 周期（含全部用户事务 COMMIT）后解锁释放——多实例取批互斥的唯一防线
	//（Momus M1：每事务 xact 锁形态下两实例可各自提交前取到同批未标记行 =
	// 双扣资金，明令禁止）。
	AcquireBillingLock(ctx context.Context) (release func(), ok bool, err error)
	// FetchUnbilledBatch 取未扣账本批（WHERE NOT billed AND error_type IN
	// ('none','abort') ORDER BY id LIMIT $n；F2-opt D1 单取批面——含 cost<=0
	// 行，消费侧内存路由）。
	FetchUnbilledBatch(ctx context.Context, limit int) ([]domain.LedgerRow, error)
	// DeductOnlyAndMark 单组事务：FEFO 扣减 → UPDATE billed=true, overdraft=$od
	// WHERE id=ANY($ids) AND NOT billed（用户缺失 → 跳过扣减仍标记、quarantined）。
	// F2-opt 后主路径走 DeductGroupsAndMark，本方法仅毒行行级二分机制直调。
	DeductOnlyAndMark(ctx context.Context, userID, cost int64, ids []int64) (balanceAfter int64, overdrafted, quarantined bool, err error)
	// DeductGroupsAndMark chunk 单事务：多用户组逐组 FEFO 扣减 + 合并标记
	//（F2-opt D3 纯增量；outcomes 与 groups 序一一对应）。
	DeductGroupsAndMark(ctx context.Context, groups []domain.LedgerGroup) ([]domain.LedgerGroupOutcome, error)
	// MarkBilledBulk 幂等纯标记（零价行快速路径 + 终极毒行隔离）。
	MarkBilledBulk(ctx context.Context, ids []int64) error
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
	// fetchBatchLimit 每次取批上限（F2-opt D6：500→2000）：排空式循环下单批
	// 规模即单次 DB 往返的行吞吐上限；批内分组后打包 ≤chunkUsersLimit 用户/
	// chunk，commit 数从 每用户一笔 → 每 chunk 一笔。
	fetchBatchLimit = 2000
	// chunkUsersLimit 单 chunk 用户组数上限（F2-opt D3）：一笔事务承载的用户
	// 组数——commit 数 = ⌈组数/64⌉/分片；deductTimeout=10s 包整事务（64 用户
	// ×~4 语句余量充足）。
	chunkUsersLimit = 64
	// lagWarnFraction lag 护栏阈值 = 保留期的 80%（spec §一：超保留期 80% 高声
	// warn——留 20% 缓冲给告警响应窗口）。
	lagWarnFraction = 0.8
)

// lagRefreshInterval lag/Stats 真值刷新节流（F2-opt D2）：距上次刷新 ≥1s 才
// 执行——排空循环内每批一刷会放大 UnbilledLag 探测压力，Stats().UnbilledRows
// 允许 ≤1s 陈旧度（不变量 #7 字段与告警语义不变，刷新频率让渡于吞吐）。
// var（非 const）：测试注入。
var lagRefreshInterval = time.Second

// inflightAbandonGrace 在途消费周期收尾宽限（A-P2-8-2，与 usage 包同值同语义
// ——两包各自声明）：Close 预算到期 Cancel baseCtx 后给在途周期收尾的兜底等待
// ——正常情形取消传播微秒级完成；DB 病态卡死时超时即放弃排空、Warn 截断退出
// （在途事务由已取消 baseCtx 收尾回滚，行保持 unbilled 不丢），不无界阻塞停机。
// var（非 const）：测试注入小阈值。
var inflightAbandonGrace = 500 * time.Millisecond

// Flusher 计费游标消费者（worker.Worker 契约，Name="billing"）。F2 重写裁决：
// 内存 pending 队列整体删除（双写元凶）——billable 行由 usage flusher 落库
// （billed=false 出生），本 worker 只消费账本游标：
//
//	每周期（FlushInterval 默认 250ms）：会话级 advisory lock 取批前获取、持有
//	整周期后释放（多实例取批互斥）→ 排空式循环（F2-opt D2）：FetchUnbilledBatch
//	(LIMIT 2000) 内存路由（D1）——cost<=0 行一次 MarkBilledBulk 纯标记（不走
//	FEFO 机器）；cost>0 行按 UserID 分组 → 分片 N worker 并发、片内连续组打包
//	≤64 用户/chunk（D3）逐块 DeductGroupsAndMark 单事务（逐组 FEFO 扣减 + 合并
//	标记原子——at-least-once 消费 + 原子 = exactly-once）→ 直至空批或 ctx 截止
//	→ 成功定向刷新余额快照（O(1)）。
//
// 毒行：chunk 结构错误 → 组为单位折半降级 → 单例组内二分重试归因（对齐 usage
// 包 poisonBisect）→ 单行仍失败 = 毒行 → MarkBilledBulk 终极隔离 +
// QuarantinedRows 计数 + Error——游标永不卡死；整库故障 → 行保持 unbilled 由
// DB 天然重放（无内存回灌面）。
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
	// unbilledN 当前 Unbilled 行数（UnbilledLag 探测刷新，≥1s 节流——Stats().
	// UnbilledRows 真值，允许 ≤1s 陈旧度）；quarantined 累计隔离行数（用户缺失
	// 组 + 毒行终极隔离）；lagMs 游标积压时滞（毫秒，= 探测时刻 now − 最老
	// unbilled 行 created_at；0 = 游标空/未探测，ABI-4 lag 族真值）；
	// lastLag 最近 lag 探测时刻（UnixMilli；节流基准，flushMu 内读写）；
	// lagWarned lag 护栏告警边沿（回落复位防刷屏）。
	lastFlush   atomic.Int64
	unbilledN   atomic.Int64
	quarantined atomic.Int64
	lagMs       atomic.Int64
	lastLag     atomic.Int64
	lagWarned   atomic.Bool
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
			f.consumeCycle(f.baseCtx, false)
		case <-refreshT.C:
			_ = f.bal.Reload(context.Background()) // fail-safe：内部 Warn + 保留旧快照
		}
	}
}

// consumeCycle 单消费周期（单入口：ticker/Close 共用，flushMu 串行）：会话锁 →
// 排空式消费（drain=true 时为 Close 排空语境）→ lag 护栏观测。返回本周期退出
// 游标的行数（扣费标记 + 隔离 + 零价标记；0 = 无进展）。
func (f *Flusher) consumeCycle(ctx context.Context, drain bool) int64 {
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
		marked = f.drainLoop(ctx)
		if marked > 0 {
			f.lastFlush.Store(time.Now().UnixMilli())
		}
	}
	f.refreshLag(ctx, drain) // 锁结果无关：他实例消费时本实例 Stats/lag 仍新鲜
	return marked
}

// drainLoop 排空式消费（F2-opt D2）：循环 取批→路由→消费 直至空批返回、零进展
// 或 ctx.Err()——一批一 tick 的节奏概念废除，FlushInterval 仅在游标空时作为
// 空转间隔。实现见 drain.go（排空消费机制面）。

// refreshLag lag 护栏 + Stats 真值刷新（≥1s 节流，D2；force=true = Close 排空
// 语境绕过节流强制刷新——防「陈旧 unbilledN==0 × n>0」提前退出排空）：最老
// unbilled 行距今超保留期 80% → 高声 Warn（边沿触发，回落复位防刷屏）——消费
// 停摆逼近分区 DROP 线提前可见。lag/unbilled 真值探测成功后原子写（Stats()
// 零锁直读）。仅在 flushMu 内调用（consumeCycle 收尾）——节流检查无竞态。
func (f *Flusher) refreshLag(ctx context.Context, force bool) {
	now := time.Now().UnixMilli()
	if !force {
		if last := f.lastLag.Load(); last != 0 && time.Duration(now-last)*time.Millisecond < lagRefreshInterval {
			return // 节流窗内跳过（首调 lastLag==0 必刷）
		}
	}
	f.lastLag.Store(now)
	oldest, count, err := f.store.UnbilledLag(ctx)
	if err != nil {
		if f.log != nil && ctx.Err() == nil {
			f.log.Warn("billing cursor lag probe failed", logx.Error(err))
		}
		return
	}
	f.unbilledN.Store(count)
	if count == 0 || oldest.IsZero() {
		f.lagMs.Store(0)
		f.lagWarned.Store(false)
		return
	}
	f.lagMs.Store(time.Since(oldest).Milliseconds())
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
// （受预算约束）→ 以 flushMu 获取等待在途周期（SIGTERM 时 ticker 周期可能在途
// 占住 flushMu；Close 必须先等其结束）→ 受 shutdown ctx 预算约束的排空循环
// （逐周期消费至游标清空；n==0 = 清空/锁被他实例持有/DB 故障——均退出，剩余
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
			// drain=true：排空语境绕过 lag 节流强制刷新——unbilledN 每周期新鲜，
			// 「陈旧 unbilledN==0 × n>0」提前退出不可能发生（Momus 维度5）。
			n := f.consumeCycle(ctx, true)
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
func groupLedgerRows(rows []domain.LedgerRow) []*domain.LedgerGroup {
	byUID := make(map[int64]*domain.LedgerGroup, 16)
	out := make([]*domain.LedgerGroup, 0, 16)
	for _, r := range rows {
		g, ok := byUID[r.UserID]
		if !ok {
			g = &domain.LedgerGroup{UserID: r.UserID}
			byUID[r.UserID] = g
			out = append(out, g)
		}
		g.Rows = append(g.Rows, r)
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
