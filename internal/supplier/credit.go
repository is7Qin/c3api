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
	if !w.cfg.FreezeEnabled {
		if n, err := w.store.ReleaseFrozenNoWait(ctx, w.batchLimit()); err != nil {
			w.warn("supplier credit release frozen failed", logx.Error(err))
		} else if n > 0 {
			w.logInfo("supplier credit released frozen chunks", logx.Int("count", n))
		}
	}

	batch, err := w.store.FetchCreditBatch(ctx, w.batchLimit())
	if err != nil {
		w.warn("supplier credit fetch batch failed", logx.Error(err))
		return
	}
	if len(batch) > 0 {
		uids := uniqueUIDs(batch)
		freeze, ferr := w.store.FreezeHoursByUID(ctx, uids)
		if ferr != nil {
			w.warn("supplier credit freeze hours lookup failed", logx.Error(ferr))
			return
		}
		if aerr := w.applyWithRetry(ctx, batch, freeze); aerr != nil {
			w.warn("supplier credit apply failed", logx.Error(aerr))
			return
		}
	}
	w.refreshLag(ctx)
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
func (w *CreditWorker) refreshLag(ctx context.Context) {
	if head, err := w.store.CreditLagHead(ctx); err != nil {
		w.warn("supplier credit lag head failed", logx.Error(err))
	} else {
		w.lagHead.Store(head)
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
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		batch, err := w.store.FetchCreditBatch(ctx, w.batchLimit())
		if err != nil {
			w.warn("supplier credit drain fetch failed", logx.Error(err))
			return
		}
		if len(batch) == 0 {
			return
		}
		freeze, ferr := w.store.FreezeHoursByUID(ctx, uniqueUIDs(batch))
		if ferr != nil {
			w.warn("supplier credit drain freeze lookup failed", logx.Error(ferr))
			return
		}
		if aerr := w.applyWithRetry(ctx, batch, freeze); aerr != nil {
			w.warn("supplier credit drain apply failed", logx.Error(aerr))
			return
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
	LagHead int64
	LagFull int64
	LagEarn int64
}

// Stats 返回观测面快照。
func (w *CreditWorker) Stats() CreditStats {
	return CreditStats{LagHead: w.lagHead.Load(), LagFull: w.lagFull.Load(), LagEarn: w.lagEarn.Load()}
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
