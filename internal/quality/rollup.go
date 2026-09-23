// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7qin.

// routing rollup worker：消费 quality-sync 落在 instance 分钟表的脏分钟，调用
// repository 既有 RollupQuality 缝（单桶事务 + advisory lock +
// dirty 清除 + watermark 推进原子完成——状态只在成功后推进，失败分钟保持
// dirty 下轮重试）。请求路径零参与；S3 起仅剩 quality 单道（flow 写入直达
// 合并层，无下游重算）。watermark 之下的迟到脏分钟不在本车道
// （watermark-ordering follow-up）。
package quality

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

// rollup 面的 kind 字面量与 routing_dirty_minute 写入侧（repository/routing.go
// UpsertQualityAndMarkDirty）同源。flow 无下游重算，dirty 仅剩 quality 单道。
const rollupKindQuality = "quality"

// defaultRollupInterval rollup tick（var 供测试注入小值；对齐 quality-sync
// PG 面 5s 节奏——脏分钟由该 lane 产生，消费节奏无需更快）。
var defaultRollupInterval = 5 * time.Second

// rollupBatchLimit 单 tick 单 kind 最大桶数（ponytail: 固定 60=1h 分钟数，
// 停摆恢复分批追赶；需要运维调参时再上 config）。
const rollupBatchLimit = 60

// RollupStore rollup worker 存储面（实现 = *repository.PartitionRepo，经
// repos.Partitions 直连——同 quality-sync PG 写面装配惯例）。
type RollupStore interface {
	ListDirtyMinutes(ctx context.Context, kind string, version int16, from time.Time, limit int) ([]time.Time, error)
	GetWatermark(ctx context.Context, kind string, version int16) (time.Time, error)
	RollupQuality(ctx context.Context, bucket time.Time, version int16) error
}

// RollupConfig rollup worker 配置。
type RollupConfig struct {
	Interval time.Duration // 0 = 默认 5s
}

// RollupStats rollup worker 观测（/ops/workers；Stats 直出 JSON）。
// S3 起仅剩 quality 单道：flow 写入直达合并层，无下游重算。
type RollupStats struct {
	QualityRolled          int64  `json:"quality_rolled"`
	Failed                 int64  `json:"failed"`
	LastDurationMs         int64  `json:"last_duration_ms"`
	LastError              string `json:"last_error"`
	WatermarkQualityUnixMs int64  `json:"watermark_quality_unix_ms"`
}

// RollupWorker 常驻 rollup worker（worker.Worker 契约，Name="routing-rollup"）：
// 每 tick 对 quality 单道：读 watermark → 选 ≥watermark 的最老脏分钟
// （升序、有界）→ 逐桶调 RollupQuality；任一桶失败即中断（保序：继续跑更新
// 桶会让 watermark 跳过失败桶，永久丢该分钟）——失败桶保持 dirty，下 tick
// 从它重试。Close 取消循环并等在途 tick 退出；无内存队列可排空（DB dirty
// 表就是队列，停机期间脏分钟自然累积，重启启动 tick 追赶）。
type RollupWorker struct {
	store RollupStore
	log   *logx.Logger
	cfg   RollupConfig

	now func() time.Time

	started atomic.Bool
	lifeMu  sync.Mutex
	cancel  context.CancelFunc
	done    <-chan struct{}

	mu    sync.Mutex
	stats RollupStats
}

func NewRollupWorker(store RollupStore, cfg RollupConfig, log *logx.Logger) *RollupWorker {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultRollupInterval
	}
	return &RollupWorker{store: store, log: log, cfg: cfg, now: time.Now}
}

// Name worker.Worker 契约。
func (w *RollupWorker) Name() string { return "routing-rollup" }

// Start worker.Worker 契约：非阻塞启动监督循环（worker.Loop：panic→Error→
// 5s 重启，尊重 ctx）；启动 tick 立即追赶一次。
func (w *RollupWorker) Start(ctx context.Context) error {
	w.lifeMu.Lock()
	defer w.lifeMu.Unlock()
	if !w.started.CompareAndSwap(false, true) {
		return fmt.Errorf("routing rollup: already started")
	}
	loopCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	w.done = worker.GoLoop(loopCtx, "routing-rollup", w.log, w.loop)
	return nil
}

func (w *RollupWorker) loop(ctx context.Context) {
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	w.runOnce(ctx) // 启动 tick（冷启动/停摆恢复立即追赶）
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.runOnce(ctx)
		}
	}
}

// runOnce 单轮：quality 单道。空脏集 = 纯 no-op（不写
// 观测的 rolled 计数，仅记录本轮耗时与 watermark 位置）。
func (w *RollupWorker) runOnce(ctx context.Context) {
	start := w.now()
	qwm := w.runKind(ctx, rollupKindQuality)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stats.LastDurationMs = w.now().Sub(start).Milliseconds()
	// watermark 观测随本轮读值刷新（失败轮也可见当前位置；nil/zero = 未初始化保留旧值）。
	if qwm != nil && !qwm.IsZero() {
		w.stats.WatermarkQualityUnixMs = qwm.UnixMilli()
	}
}

// runKind 处理一个 kind；返回本轮读到的 watermark（nil = 读失败，观测不
// 刷新）。逐桶失败中断该道（保序重试），成功桶的状态推进由 repo 事务完成。
func (w *RollupWorker) runKind(ctx context.Context, kind string) *time.Time {
	version := int16(domain.RoutingIdentityVersion)
	wm, err := w.store.GetWatermark(ctx, kind, version)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		w.fail("get watermark", kind, err)
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		wm = time.Time{}
	}
	cur := wm
	buckets, err := w.store.ListDirtyMinutes(ctx, kind, version, wm, rollupBatchLimit)
	if err != nil {
		w.fail("list dirty minutes", kind, err)
		return &cur
	}
	for _, b := range buckets {
		if ctx.Err() != nil {
			return &cur // 停机取消：剩余脏分钟保持 dirty，下轮/重启追赶
		}
		var rerr error
		rerr = w.store.RollupQuality(ctx, b, version)
		if rerr != nil {
			w.fail("rollup "+kind, b.UTC().Format(time.RFC3339), rerr)
			return &cur
		}
		w.mu.Lock()
		w.stats.QualityRolled++
		w.mu.Unlock()
		cur = b // repo 成功事务把 watermark 推到该桶
	}
	return &cur
}

// fail 失败观测 + Warn（worker 自愈语义：dirty 未清，下 tick 重试）。
func (w *RollupWorker) fail(step, detail string, err error) {
	w.mu.Lock()
	w.stats.Failed++
	w.stats.LastError = step + ": " + err.Error()
	w.mu.Unlock()
	if w.log != nil {
		w.log.Warn("routing rollup cycle failed", logx.String("step", step), logx.String("detail", detail), logx.Error(err))
	}
}

// Stats 满足 handler.StatsProvider 契约（any 直出 JSON，ops 端点零转换）。
func (w *RollupWorker) Stats() any {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stats
}

// Close 幂等（worker.Worker 契约）：取消循环并等待在途 tick 退出（tick 内
// 所有 store 调用尊重 ctx，卡住的调用快速失败释放）。
func (w *RollupWorker) Close(ctx context.Context) error {
	w.lifeMu.Lock()
	cancel, done := w.cancel, w.done
	w.lifeMu.Unlock()
	if cancel == nil || done == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
