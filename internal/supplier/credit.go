// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package supplier

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

// CreditStore credit 链持久面（repository 实现绑定真实 PG；测试注入 fake）。
// 每个方法自持事务边界（ApplyCreditTx 单事务含阶段 A→D 与两条守卫 + 守恒）。
type CreditStore interface {
	// FetchCreditBatch 取批（消费索引①；`ORDER BY id LIMIT`，双谓词
	// `NOT supplier_credited AND supplier_earn_millis > 0`）。返回行携带
	// uid/cost/earn/created_at（§5.1 ①）。
	FetchCreditBatch(ctx context.Context, limit int) ([]BatchRow, error)
	// FreezeHoursByUID 各 uid 生效冻结小时（全局开关优先 + NULL 注入默认，§2.4）。
	FreezeHoursByUID(ctx context.Context, uids []int64) (map[int64]int, error)
	// ApplyCreditTx 单事务（阶段 A chunks → B 按 uid 升序预锁 balances → C 建行/
	// 累加 → D 标记 usage_logs credited）：余额覆盖守卫 + 标记计数守卫 + 金额守恒；
	// 任一失败整事务回滚。封账按源日 upsert（§3.10）。
	ApplyCreditTx(ctx context.Context, batch []BatchRow, freezeByUID map[int64]int) error
	// ReleaseFrozenNoWait freeze_enabled=false 的存量释放（忽略 available_at，
	// 保留 LIMIT 每周期一批）——最终收敛语义（§5.4）。
	ReleaseFrozenNoWait(ctx context.Context, limit int) (int, error)
	// CreditLagHead 头行快速 lag（游标头 id 定位，低成本；高频）。
	CreditLagHead(ctx context.Context) (int64, error)
	// CreditLagFull 完整 backlog COUNT + SUM（O(backlog) 成本；降频）。
	CreditLagFull(ctx context.Context) (rows int64, sumEarn int64, err error)
}

// CreditConfig credit worker 配置。
type CreditConfig struct {
	Interval      time.Duration // 消费周期（<=0 兜底 200ms）
	BatchLimit    int           // 每批行数（<=0 兜底 500）
	DrainBudget   time.Duration // Close 排空总预算（独立于 drainCycleBudget）
	FreezeEnabled bool          // 全局冻结开关（false ⇒ 额外每周期释放存量）
	LagFullEvery  int           // 全量 lag 降频（每 N 周期一次；<=0 兜底 20）
}

// CreditWorker 记账链消费 worker（worker.Worker 契约，Name="supplier-credit"）。
// 从 usage_logs 未记账正收益子集派生（消费面；只 UPDATE supplier_credited——
// 不改 billed、不绕 errConcurrentMark）。渐进式排空，失败 Warn + 下轮重试。
type CreditWorker struct {
	cfg     CreditConfig
	store   CreditStore
	log     *logx.Logger
	started atomic.Bool

	// 观测面。
	lagHead    atomic.Int64 // 头行快速 lag（行数）
	lagFull    atomic.Int64 // 完整 backlog 行数
	lagEarn    atomic.Int64 // 完整 backlog Σ earn
	cycleCount atomic.Int64
	// lagObservedUnixMs 最近一次成功刷新 lag 的时刻（0 = 从未）。
	lagObservedUnixMs atomic.Int64
	// lagStale true = 最近一轮周期未能成功刷新观测（取批/冻结查询/apply 失败），
	// 当前呈现的 lag 可能是上轮旧值——不得把故障期的旧健康值当新鲜值（spec I2）。
	lagStale atomic.Bool
}

// NewCredit 构造记账 worker。
func NewCredit(cfg CreditConfig, store CreditStore, log *logx.Logger) *CreditWorker {
	return &CreditWorker{cfg: cfg, store: store, log: log}
}

// Name worker.Worker 契约。
func (w *CreditWorker) Name() string { return "supplier-credit" }

// Start 启动循环（worker.GoLoop 监督契约）。
func (w *CreditWorker) Start(ctx context.Context) error {
	if !w.started.CompareAndSwap(false, true) {
		return fmt.Errorf("supplier-credit: already started")
	}
	worker.GoLoop(ctx, "supplier-credit", w.log, w.loop)
	return nil
}

func (w *CreditWorker) loop(ctx context.Context) {
	interval := w.cfg.Interval
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	w.runOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.runOnce(ctx)
		}
	}
}

// runOnce 单周期（leader 锁门控）：取批 → 记账事务 → lag 刷新（头行高频 +
// 全量降频）→ freeze_enabled=false 存量释放。任一失败 Warn 不中断（下轮重试；
// Loop 正常返回即退出，故不 panic/不 return）。
func (w *CreditWorker) runOnce(ctx context.Context) {
	w.withLeaderLock(ctx, func() { w.runOnceLocked(ctx) })
}

func (w *CreditWorker) runOnceLocked(ctx context.Context) {
	// 存量释放：freeze_enabled=false ⇒ 不注册解冻 worker，须由本链释放（§5.4）。
	// 必须在没有 usage 批时也执行——故置于批次之前且不因 backlog==0 跳过。
	if n, err := w.releaseFrozenOnce(ctx); err != nil {
		w.warn("supplier credit release frozen failed", logx.Error(err))
	} else if n > 0 {
		w.logInfo("supplier credit released frozen chunks", logx.Int("count", n))
	}

	if _, err := w.consumeBatch(ctx); err != nil {
		w.warn("supplier credit consume failed", logx.Error(err))
		// 故障轮仍更新观测（陈旧状态）——置 stale，不刷新则 lag 保持旧健康值，
		// ops 看不到记账链已停摆（spec I2）。
		w.lagStale.Store(true)
		return
	}
	w.refreshLag(ctx)
}

// consumeBatch 单批消费（fetch → freeze → apply），正常周期与排空共用。返回本次
// 实际记账的行数（批为空 ⇒ 0）；任一阶段失败返回带阶段前缀的错误（调用方决定
// 告警措辞与 stale 标记）。不改重试次数、释放顺序、总预算。
func (w *CreditWorker) consumeBatch(ctx context.Context) (int, error) {
	batch, err := w.store.FetchCreditBatch(ctx, w.batchLimit())
	if err != nil {
		return 0, fmt.Errorf("fetch batch: %w", err)
	}
	if len(batch) == 0 {
		return 0, nil
	}
	freeze, err := w.store.FreezeHoursByUID(ctx, uniqueUIDs(batch))
	if err != nil {
		return 0, fmt.Errorf("freeze hours lookup: %w", err)
	}
	if err := w.applyWithRetry(ctx, batch, freeze); err != nil {
		return 0, fmt.Errorf("apply: %w", err)
	}
	return len(batch), nil
}

// releaseFrozenOnce freeze_enabled=false 的存量释放一批（无 usage 批时亦执行；§5.4）。
// freeze_enabled=true ⇒ no-op 返回 0。返回本批释放行数。
func (w *CreditWorker) releaseFrozenOnce(ctx context.Context) (int, error) {
	if w.cfg.FreezeEnabled {
		return 0, nil
	}
	return w.store.ReleaseFrozenNoWait(ctx, w.batchLimit())
}

// applyWithRetry 有界重试 40P01/55P03（§5.7）。同一批重放幂等（标记计数守卫）。
func (w *CreditWorker) applyWithRetry(ctx context.Context, batch []BatchRow, freeze map[int64]int) error {
	const maxAttempts = 5
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err = w.store.ApplyCreditTx(ctx, batch, freeze); err == nil {
			return nil
		}
		if !IsRetryableTxErr(err) || ctx.Err() != nil {
			return err
		}
		// 有界退避（可预期冲突；非 sleep 掩盖竞态——重试是显式的收敛策略）。
		backoff := time.Duration(attempt+1) * 20 * time.Millisecond
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
	return err
}

// refreshLag 头行高频 + 全量降频（I3：COUNT/SUM 即使命中索引仍是 O(backlog)）。
// 成功取到头行 ⇒ 记观测时刻并清除 stale；头行查询失败 ⇒ 置 stale（保留旧值但
// 标陈旧，不冒充新鲜）。
func (w *CreditWorker) refreshLag(ctx context.Context) {
	if head, err := w.store.CreditLagHead(ctx); err != nil {
		w.warn("supplier credit lag head failed", logx.Error(err))
		w.lagStale.Store(true)
	} else {
		w.lagHead.Store(head)
		w.lagObservedUnixMs.Store(time.Now().UnixMilli())
		w.lagStale.Store(false)
	}
	n := w.cycleCount.Add(1)
	every := w.cfg.LagFullEvery
	if every <= 0 {
		every = 20
	}
	if int(n)%every != 0 {
		return
	}
	if rows, sum, err := w.store.CreditLagFull(ctx); err != nil {
		w.warn("supplier credit lag full failed", logx.Error(err))
		w.lagStale.Store(true)
	} else {
		w.lagFull.Store(rows)
		w.lagEarn.Store(sum)
	}
}

// Close 排空：独立总预算（不得复用 drainCycleBudget），消费到 backlog==0 或预算
// 到期；尽力语义（失败仅 Warn 留证，不阻断停机）。leader 锁门控——非 leader
// 不做排空（其他实例在消费）。
func (w *CreditWorker) Close(ctx context.Context) error {
	w.withLeaderLock(ctx, func() {
		w.closeDrain(ctx)
	})
	return nil
}

func (w *CreditWorker) closeDrain(ctx context.Context) {
	budget := w.cfg.DrainBudget
	if budget <= 0 {
		budget = 5 * time.Second
	}
	// 以 shutdown ctx 派生**总预算** timeout：单条 DB 操作也受总预算约束
	// （不得只在循环入口比较时间而放任单条语句超预算，§5.5）。
	dctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	for dctx.Err() == nil {
		// freeze_enabled=false ⇒ 不注册解冻 worker，停机时须由本链释放存量
		// （§5.4）：每轮先限量释放一批（忽略 available_at），**不因 usage
		// backlog==0 提前退出**；只有 usage 批为空且本轮无存量释放才收敛。
		released, rerr := w.releaseFrozenOnce(dctx)
		if rerr != nil {
			w.warn("supplier credit drain release frozen failed", logx.Error(rerr))
			return
		}
		rows, err := w.consumeBatch(dctx)
		if err != nil {
			w.warn("supplier credit drain consume failed", logx.Error(err))
			return
		}
		if rows == 0 {
			// 退出条件：usage 批为空 且（冻结启用 或 本轮无存量释放）。
			if w.cfg.FreezeEnabled || released == 0 {
				return
			}
			continue
		}
	}
}

func (w *CreditWorker) batchLimit() int {
	if w.cfg.BatchLimit <= 0 {
		return 500
	}
	return w.cfg.BatchLimit
}

func (w *CreditWorker) warn(msg string, fields ...logx.Field) {
	if w.log != nil {
		w.log.Warn(msg, fields...)
	}
}

func (w *CreditWorker) logInfo(msg string, fields ...logx.Field) {
	if w.log != nil {
		w.log.Info(msg, fields...)
	}
}

// CreditStats 观测面快照。
type CreditStats struct {
	LagHead int64 `json:"lag_head"`
	LagFull int64 `json:"lag_full"`
	LagEarn int64 `json:"lag_earn"`
	// LagObservedUnixMs 最近一次成功刷新 lag 的时刻（0 = 从未）。
	LagObservedUnixMs int64 `json:"lag_observed_unix_ms"`
	// LagStale true = 最近一轮刷新失败，呈现的是上轮旧值（不得当新鲜值）。
	LagStale bool `json:"lag_stale"`
}

// Stats 返回观测面快照（实现 handler.StatsProvider——Name() + Stats() any；
// 装配链路见 internal/handler/ops.go 文件头）。返回 any 与全仓其余 worker 一致，
// 使 cmd/server 的类型断言能收进 /api/admin/ops/workers。
func (w *CreditWorker) Stats() any {
	return CreditStats{
		LagHead:           w.lagHead.Load(),
		LagFull:           w.lagFull.Load(),
		LagEarn:           w.lagEarn.Load(),
		LagObservedUnixMs: w.lagObservedUnixMs.Load(),
		LagStale:          w.lagStale.Load(),
	}
}

// uniqueUIDs 批内去重 uid（升序由 AggregateBatch 保证）。
func uniqueUIDs(batch []BatchRow) []int64 {
	seen := make(map[int64]struct{}, len(batch))
	out := make([]int64, 0, len(batch))
	for _, r := range batch {
		if _, ok := seen[r.UID]; ok {
			continue
		}
		seen[r.UID] = struct{}{}
		out = append(out, r.UID)
	}
	return out
}

// ErrNotLeader credit 取批由 leader/会话锁把持（多实例减少撞同批）；非 leader
// 返回本哨兵 → 空转下轮。
var ErrNotLeader = errors.New("supplier: not leader")

// CreditLocker 记账链取批的 leader/会话锁面（repository *SupplierRepo 实现；
// fake 不实现 ⇒ 视为单 leader，锁 no-op）。I4：多实例用会话锁取批，标记计数守卫
// 继续作锁丢失后的安全后盾（§5.2）。
type CreditLocker interface {
	AcquireSupplierLock(ctx context.Context) (release func(), ok bool, err error)
}

// withLeaderLock 取 leader 锁后执行 fn：store 非 locker（或单实例）⇒ 直接执行；
// 抢锁失败（其他实例在消费）⇒ ok=false，跳过本周期。
func (w *CreditWorker) withLeaderLock(ctx context.Context, fn func()) (ok bool) {
	locker, isLocker := w.store.(CreditLocker)
	if !isLocker {
		fn()
		return true
	}
	release, acquired, err := locker.AcquireSupplierLock(ctx)
	if err != nil {
		w.warn("supplier credit leader lock failed", logx.Error(err))
		return false
	}
	if !acquired {
		return false
	}
	defer release()
	fn()
	return true
}
