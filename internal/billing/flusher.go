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

// LedgerStore 计费游标消费面（repository.Repository 门面实现；三车道拓扑，
// spec-f2opt-settlement §〇-b）。
type LedgerStore interface {
	// AcquireBillingLock 会话级 advisory lock：专用池连接取批前获取、持有整
	// 周期（含全部车道结算事务 COMMIT）后解锁释放——多实例取批互斥的唯一防线
	//（Momus M1：每事务 xact 锁形态下两实例可各自提交前取到同批未标记行 =
	// 双扣资金，明令禁止）。
	AcquireBillingLock(ctx context.Context) (release func(), ok bool, err error)
	// FetchUnbilledBatch 取未扣账本批（WHERE NOT billed AND error_type IN
	// ('none','abort') ORDER BY id LIMIT $n）：零价扫尾取数面（D1 单取批）。
	FetchUnbilledBatch(ctx context.Context, limit int) ([]domain.LedgerRow, error)
	// SettleBalanceBatch Balance 车道结算一个窗口（余额-only 用户；单语句单
	// 事务 取批→条件扣→透支补刀→标记；桶谓词 COALESCE(user_id,0)%k=bucket——
	// 桶级并行 wave3 D-C，K 由编排层给定）；毒行梯子内置——重试耗尽隔离该车道
	// 该桶队头越行，隔离行以 Quarantined 计数随 summary 返回。
	SettleBalanceBatch(ctx context.Context, limit, k, bucket int) (domain.SettlementSummary, error)
	// SettleFefoBatch Temp 车道结算一个窗口（temp-active 用户；集合化 FEFO +
	// 差额透支补刀 + 标记一体，D7；桶谓词同上）。事务纪律与毒行梯子同
	// SettleBalanceBatch。
	SettleFefoBatch(ctx context.Context, limit, k, bucket int) (domain.SettlementSummary, error)
	// MarkBilledBulk 幂等纯标记（零价行快速路径 + 终极毒行隔离）。
	MarkBilledBulk(ctx context.Context, ids []int64) error
	// UnbilledLag 游标积压度量（wave3 D-B 签名收缩：队头两步法取最老可结算行
	// created_at，ok=false = 游标空；精确 COUNT 已删）。
	UnbilledLag(ctx context.Context) (oldestCreated time.Time, ok bool, err error)
}

// FlushConfig 消费节奏（config.BillingConfig 映射）。
type FlushConfig struct {
	FlushInterval          time.Duration // 游标轮询周期（默认 250ms）
	BalanceRefreshInterval time.Duration // 余额快照全量刷新周期
	// Workers 历史并行度参数（chunk 分片时代遗留；三车道语句化后消费为单语句
	// 顺序执行，本字段仅保留 config ABI 兼容——不再影响消费行为）。
	Workers int
	// LogRetentionDays usage 日保留期（lag 护栏基准，cmd 接线
	// config.Usage.LogRetentionDays）：最老 unbilled 行距今超保留期 80% → 高声
	// Warn（停机护栏——消费停摆逼近分区 DROP 线提前可见）。<= 0 = 护栏禁用
	//（对齐 retention <= 0 不删除语义）。
	LogRetentionDays int
}

const (
	// fetchBatchLimit 零价扫/FEFO 车道取数上限（FetchUnbilledBatch 单次规模）。
	fetchBatchLimit = 2000
	// settleBatchLimit 结算语句单窗口行数的种子/初始值（自适应批控
	// batchController 的起点，非固定值——见 batch_controller.go）：F2-opt W2
	// 实测调参。语句固定成本（编排/行锁/WALK 摊派）按批摊薄——批越大每行成本
	// 越趋近纯 WAL 写入。实测边界：本盒单语句 DML ~3-6k 行/s（IO/WAL 共享竞争
	// 下更低），批规模必须满足 settleTimeout(10s) 预算——50000 行实测超时停摆
	// （生产 1170 万行脏可见性地图上单语句 >10s）。8000 行 ≈ 1.3-2.6s/语句，
	// 安全余量 3 倍+；控制器以实测时长反馈在 [500,64000] 内自适应调节。
	settleBatchLimit = 8000
	// lagWarnFraction lag 护栏阈值 = 保留期的 80%（spec §一：超保留期 80% 高声
	// warn——留 20% 缓冲给告警响应窗口）。
	lagWarnFraction = 0.8
)

// lagSlowEvery 精确 lag 探针低频系数（F2-opt W2 实测调参）：COUNT(*) 在大积压
// 下是 O(unbilled) 的 index-only 扫描——风暴后可见性地图未置位时退化为逐行堆
// 取（6.5M 行实测 474ms+），每周期必跑吃掉 ~20% 周期预算。精确值每 lagSlowEvery
// 个节流窗校准一次，周期间 UnbilledRows 保持上次快照、lag_ms 不更新（Close 排
// 空 force 绕过节流保证退出判据新鲜）。var（非 const）：测试注入。
var lagSlowEvery = 10

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
//	整周期后释放（多实例取批互斥）→ 排空式循环（F2-opt D2）三车道顺序消费
//	（spec-f2opt-settlement §〇-b；车道内 K 桶并行，wave3 D-C）：Balance 车道
//	SettleBalanceBatch（余额-only 用户，单语句单事务 取批→条件扣→透支补刀→标记）
//	→ Temp 车道 SettleFefoBatch（temp-active 用户，集合化 FEFO——at-least-once
//	消费 + 单语句原子 = exactly-once）→ 零价批扫尾 MarkBilledBulk 纯标记 → 直至
//	零进展或 ctx 截止 → 成功定向刷新余额快照（(uid,balance_after) 对 O(1) Set）。
//
// 毒行梯子每车道独立：连续失败 K 次 → 只读探针定位该车道队头行 → MarkBilledBulk
// 终极隔离越行 + QuarantinedRows 计数 + Error——游标永不卡死；一车道毒行不影响
// 他车道进展；整库故障 → 行保持 unbilled 由 DB 天然重放（无内存回灌面）。
// Close 排空惯用法保持（loopDone/baseCtx/flushMu/inflightAbandonGrace）：等在途
// 周期结束后循环消费至游标清空（预算内）或截断退出（剩余行下次启动收敛，
// RestartConvergence）。
type Flusher struct {
	cfg   FlushConfig
	store LedgerStore
	bal   *Balances
	log   *logx.Logger
	// ctl 结算批规模自适应控制器（batch_controller.go）：settleLaneParallel
	// 各桶 goroutine 观测语句时长/错误反馈调节批规模（并发安全，内含互斥量）。
	ctl *batchController
	// flushMu 单消费周期入口串行：ticker/Close 两处触发互斥；在途周期即其
	// 持有者（Close 排空惯用法，与 usage 包各自声明——有意重复）。
	flushMu    sync.Mutex
	started    atomic.Bool
	loopDone   chan struct{}
	closeOnce  sync.Once
	baseCtx    context.Context // ticker 路径周期的可取消父 ctx（Close 预算到期 Cancel）
	baseCancel context.CancelFunc
	// 观测原子：lastFlush 最近成功消费时刻（UnixMilli；0 = 尚未消费）；
	// unbilledN Unbilled 行数**占位恒 0**（wave3 D-B 精确 COUNT 已删——无硬消费
	// 者，Stats().UnbilledRows 可观测性降级显式化，spec §一 D-B「仪表盘允许估算
	// 降级」；字段保留 = ops JSON 契约 ABI 不变）；quarantined 累计隔离行数（幽灵
	// 用户行 + 毒行终极隔离）；lagMs 游标积压时滞（毫秒，= 探测时刻 now − 最老
	// unbilled 行 created_at；0 = 游标空/未探测，ABI-4 lag 族真值）；
	// lastLag 最近 lag 探测时刻（UnixMilli；节流基准，flushMu 内读写）；
	// lagWarned lag 护栏告警边沿（回落复位防刷屏）。
	lastFlush   atomic.Int64
	unbilledN   atomic.Int64 // 占位恒 0（D-B 降级显式化）——见上注释
	quarantined atomic.Int64
	lagMs       atomic.Int64
	lastLag     atomic.Int64
	lagSlowCnt  int // 精确探针低频计数（仅 flushMu 内读写——consumeCycle 收尾串行）
	lagWarned   atomic.Bool
}

// NewFlusher 构造游标消费者（store = repository 门面；bal 余额快照定向刷新面）。
func NewFlusher(cfg FlushConfig, store LedgerStore, bal *Balances, log *logx.Logger) *Flusher {
	f := &Flusher{
		cfg: cfg, store: store, bal: bal, log: log,
		ctl:      newBatchController(),
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

// refreshLag lag 护栏真值刷新（wave3 D-B 无计数世界重构）：force=true = Close
// 排空语境**必刷**（绕过全部节流——Momus 维度5：退出判据新鲜度不可让渡）；非
// force 双层节流——① lagRefreshInterval ≥1s 窗 ② 精确探针每 lagSlowEvery 个
// 节流窗校准一次（队头两步法虽已 O(log n)，低频化保留为探针压力上限），校准窗
// 之间 lagMs 保持上次值（陈旧度显式可接受，D-B 降级语义）。最老 unbilled 行距今
// 超保留期 80% → 高声 Warn（边沿触发，回落复位防刷屏）——消费停摆逼近分区 DROP
// 线提前可见。lag 真值探测成功后原子写（Stats() 零锁直读）。仅在 flushMu 内调用
// （consumeCycle 收尾）——节流检查无竞态。
func (f *Flusher) refreshLag(ctx context.Context, force bool) {
	now := time.Now().UnixMilli()
	if force {
		f.lastLag.Store(now)
	} else {
		if last := f.lastLag.Load(); last != 0 && time.Duration(now-last)*time.Millisecond < lagRefreshInterval {
			return // 节流窗内跳过（首调 lastLag==0 必刷）
		}
		f.lagSlowCnt++
		f.lastLag.Store(now)
		if (f.lagSlowCnt-1)%lagSlowEvery != 0 {
			return // 精确探针低频化：非校准窗跳过——lagMs 保持上次值（D-B 显式降级）
		}
	}
	oldest, ok, err := f.store.UnbilledLag(ctx)
	if err != nil {
		if f.log != nil && ctx.Err() == nil {
			f.log.Warn("billing cursor lag probe failed", logx.Error(err))
		}
		return
	}
	if !ok { // 游标空（队头两步法步①零行）
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
						logx.Int64("consumed_rows", flushed))
				}
				return
			}
			// drain=true：排空语境绕过 lag 节流强制刷新（Momus 维度5——判据新鲜度）。
			n := f.consumeCycle(ctx, true)
			flushed += n
			if n == 0 {
				// 本周期无进展（游标清空/锁他实例持有/DB 故障/预算内取消）——退出
				// 不空转，剩余行由下周期/下次启动收敛；预算已到期 → 归因截断 Warn。
				if ctx.Err() != nil && f.log != nil {
					f.log.Warn("billing flusher close: shutdown budget exceeded, truncated drain",
						logx.Int64("consumed_rows", flushed))
				}
				return
			}
		}
	})
	return nil
}
