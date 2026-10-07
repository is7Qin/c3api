// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package supplier

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

// ThawStore 解冻链持久面（repository 实现：四步独立顺序 SQL + 单连接契约 + 提交前
// 覆盖守卫；§5.3）。ThawDueChunks 返回本批删除的桶行数。
type ThawStore interface {
	ThawDueChunks(ctx context.Context, limit int) (int, error)
}

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

// runOnce 单周期：解冻到期桶（有界重试）。失败 Warn 不中断（下轮重试；Loop 正常
// 返回即退出 ⇒ 不 panic/不 return）。
func (w *ThawWorker) runOnce(ctx context.Context) {
	w.cycles.Add(1)
	n, err := w.thawWithRetry(ctx)
	if err != nil {
		w.warn("supplier thaw failed", logx.Error(err))
		return
	}
	if n > 0 {
		w.deleted.Add(int64(n))
	}
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
	DeletedTotal int64
	Cycles       int64
}

// Stats 返回观测面快照。
func (w *ThawWorker) Stats() ThawStats {
	return ThawStats{DeletedTotal: w.deleted.Load(), Cycles: w.cycles.Load()}
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
			return nil
		}
		if n == 0 {
			return nil
		}
	}
	return nil
}
