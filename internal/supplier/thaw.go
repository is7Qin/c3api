// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package supplier

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

// ThawStore 解冻链持久面（repository 实现：四步独立顺序 SQL + 单连接契约 + 提交前
// 覆盖守卫；§5.3）。ThawDueChunks 返回本批删除的桶行数。
type ThawStore interface {
	ThawDueChunks(ctx context.Context, limit int) (int, error)
	// ThawBucketStats 冻结桶物理观测（§7.2/G7）：bucket_rows（总行数）、
	// bucket_lag（最老未解冻桶 available_at 距今秒数；全部未来/空 = 0）、overdue
	// 行数/金额。物理行数在停摆/多代可超数学上界，故容量以观测保证。
	ThawBucketStats(ctx context.Context) (ThawBucketSnapshot, error)
}

// ThawBucketSnapshot 冻结桶物理观测（spec §7.2/G7）：DAL 直读，零缓存。
type ThawBucketSnapshot struct {
	BucketRows       int64 // supplier_frozen_chunks 总行数
	BucketLagSeconds int64 // 最老未解冻桶 available_at 距 now 秒数（全部未来/空 = 0）
	OverdueRows      int64 // 已到期（available_at <= now）桶行数
	OverdueAmount    int64 // 已到期桶金额合计（millis）
}

// thawRateWindow 解冻速率滚动窗口（spec §7.2：滚动 5 min 成功删除行/s）。
const thawRateWindow = 5 * time.Minute

// ThawConfig 解冻 worker 配置。
type ThawConfig struct {
	Interval    time.Duration // 解冻周期（<=0 兜底 1s）
	BatchLimit  int           // 每批桶行数（<=0 兜底 1000）
	DrainBudget time.Duration // Close 排空总预算（独立）
}

// ThawWorker 解冻链消费 worker（worker.Worker 契约，Name="supplier-thaw"）：
// 处理到期桶（available_at <= now()），整体到期、整体解冻（§5.3）。失败 Warn +
// 收敛重试（40P01/55P03 有界重试）。
type ThawWorker struct {
	cfg     ThawConfig
	store   ThawStore
	log     *logx.Logger
	started atomic.Bool

	deleted atomic.Int64 // 累计删除桶行数（观测）
	cycles  atomic.Int64

	// 桶物理观测（§7.2/G7）：每轮刷新；失败抛 stale（不冒充新鲜值）。
	bucketRows       atomic.Int64
	bucketLagSeconds atomic.Int64
	overdueRows      atomic.Int64
	overdueAmount    atomic.Int64
	thawRateMilli    atomic.Int64 // 滚动 5min 成功删除速率（行/s ×1000；原子存整数）
	bucketStale      atomic.Bool

	// rateSamples 滚动窗口样本（时间 + 累计删除）；rateMu 保护。
	rateMu      sync.Mutex
	rateSamples []thawRateSample
}

// thawRateSample 速率窗口样本：时刻 + 当时累计删除行数。
type thawRateSample struct {
	at      time.Time
	deleted int64
}

// NewThaw 构造解冻 worker。
func NewThaw(cfg ThawConfig, store ThawStore, log *logx.Logger) *ThawWorker {
	return &ThawWorker{cfg: cfg, store: store, log: log}
}

// Name worker.Worker 契约。
func (w *ThawWorker) Name() string { return "supplier-thaw" }

// Start 启动循环（worker.GoLoop 监督契约）。
func (w *ThawWorker) Start(ctx context.Context) error {
	if !w.started.CompareAndSwap(false, true) {
		return fmt.Errorf("supplier-thaw: already started")
	}
	worker.GoLoop(ctx, "supplier-thaw", w.log, w.loop)
	return nil
}

func (w *ThawWorker) loop(ctx context.Context) {
	interval := w.cfg.Interval
	if interval <= 0 {
		interval = time.Second
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

// runOnce 单周期：解冻到期桶（有界重试）+ 刷新桶物理观测与滚动速率。失败 Warn
// 不中断（下轮重试；Loop 正常返回即退出 ⇒ 不 panic/不 return）。**即使解冻失败
// 也刷新观测**——停摆要可见（bucket_stale / thaw_rate→0），不得静默。
func (w *ThawWorker) runOnce(ctx context.Context) {
	w.cycles.Add(1)
	n, err := w.thawWithRetry(ctx)
	if err != nil {
		w.warn("supplier thaw failed", logx.Error(err))
	} else if n > 0 {
		w.deleted.Add(int64(n))
	}
	w.recordRate(time.Now(), w.deleted.Load())
	w.refreshBucketStats(ctx)
}

// recordRate 记录本轮累计删除并重算滚动 5min 成功删除速率（行/s）。
func (w *ThawWorker) recordRate(now time.Time, total int64) {
	w.rateMu.Lock()
	defer w.rateMu.Unlock()
	w.rateSamples = append(w.rateSamples, thawRateSample{at: now, deleted: total})
	cutoff := now.Add(-thawRateWindow)
	// 丢弃窗口外的前导样本，但保留边界样本（第一个 ≥cutoff 的前一个），
	// 让速率覆盖 ≤5min 的真实跨度（而不是丢弃后跨度骤减）。
	for len(w.rateSamples) > 2 && !w.rateSamples[1].at.After(cutoff) {
		w.rateSamples = w.rateSamples[1:]
	}
	first := w.rateSamples[0]
	last := w.rateSamples[len(w.rateSamples)-1]
	elapsed := last.at.Sub(first.at).Seconds()
	if elapsed <= 0 {
		w.thawRateMilli.Store(0)
		return
	}
	rate := float64(last.deleted-first.deleted) / elapsed
	if rate < 0 {
		rate = 0
	}
	w.thawRateMilli.Store(int64(rate * 1000))
}

// refreshBucketStats 刷新桶物理观测（失败 ⇒ stale，保留上轮值）。
func (w *ThawWorker) refreshBucketStats(ctx context.Context) {
	st, err := w.store.ThawBucketStats(ctx)
	if err != nil {
		w.warn("supplier thaw bucket stats failed", logx.Error(err))
		w.bucketStale.Store(true)
		return
	}
	w.bucketRows.Store(st.BucketRows)
	w.bucketLagSeconds.Store(st.BucketLagSeconds)
	w.overdueRows.Store(st.OverdueRows)
	w.overdueAmount.Store(st.OverdueAmount)
	w.bucketStale.Store(false)
}

// recoveryEtaSeconds 解冻恢复 ETA（spec §7.2）：到期桶数 / 速率；速率 0 ⇒ -1
// （unknown）。
func (w *ThawWorker) recoveryEtaSeconds() int64 {
	rate := w.thawRateMilli.Load()
	if rate <= 0 {
		return -1
	}
	return w.overdueRows.Load() * 1000 / rate
}

// thawWithRetry 有界重试 40P01/55P03（§5.7）。同一桶重放幂等（谓词 + 删除同事务）。
func (w *ThawWorker) thawWithRetry(ctx context.Context) (int, error) {
	const maxAttempts = 5
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		var n int
		if n, err = w.store.ThawDueChunks(ctx, w.batchLimit()); err == nil {
			return n, nil
		}
		if !IsRetryableTxErr(err) || ctx.Err() != nil {
			return 0, err
		}
		backoff := time.Duration(attempt+1) * 20 * time.Millisecond
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(backoff):
		}
	}
	return 0, err
}

func (w *ThawWorker) batchLimit() int {
	if w.cfg.BatchLimit <= 0 {
		return 1000
	}
	return w.cfg.BatchLimit
}

func (w *ThawWorker) warn(msg string, fields ...logx.Field) {
	if w.log != nil {
		w.log.Warn(msg, fields...)
	}
}

// ThawStats 解冻观测面快照。
type ThawStats struct {
	DeletedTotal     int64   `json:"deleted_total"`
	Cycles           int64   `json:"cycles"`
	BucketRows       int64   `json:"bucket_rows"`
	BucketLagSeconds int64   `json:"bucket_lag_seconds"`
	OverdueRows      int64   `json:"overdue_rows"`
	OverdueAmount    int64   `json:"overdue_amount"`
	ThawRatePerSec   float64 `json:"thaw_rate_per_sec"`
	// RecoveryEtaSeconds 解冻恢复 ETA（到期桶数 / 速率，秒）；-1 = unknown（速率 0）。
	RecoveryEtaSeconds int64 `json:"recovery_eta_seconds"`
	// BucketStale true = 最近一轮桶观测查询失败，呈现的是上轮旧值。
	BucketStale bool `json:"bucket_stale"`
}

// Stats 返回观测面快照（实现 handler.StatsProvider——Name() + Stats() any）。
func (w *ThawWorker) Stats() any {
	return ThawStats{
		DeletedTotal:       w.deleted.Load(),
		Cycles:             w.cycles.Load(),
		BucketRows:         w.bucketRows.Load(),
		BucketLagSeconds:   w.bucketLagSeconds.Load(),
		OverdueRows:        w.overdueRows.Load(),
		OverdueAmount:      w.overdueAmount.Load(),
		ThawRatePerSec:     float64(w.thawRateMilli.Load()) / 1000.0,
		RecoveryEtaSeconds: w.recoveryEtaSeconds(),
		BucketStale:        w.bucketStale.Load(),
	}
}

// Close 尽力排空（独立总预算）：消费到无到期桶或预算到期；失败仅 Warn 留证，
// 不阻断停机。
func (w *ThawWorker) Close(ctx context.Context) error {
	budget := w.cfg.DrainBudget
	if budget <= 0 {
		budget = 3 * time.Second
	}
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		n, err := w.thawWithRetry(ctx)
		if err != nil {
			w.warn("supplier thaw drain failed", logx.Error(err))
			w.refreshBucketStats(ctx)
			return nil
		}
		if n == 0 {
			break
		}
	}
	// 停机前刷新一次桶观测（排空后桶行数/ETA 的最新值）。
	w.refreshBucketStats(ctx)
	return nil
}
