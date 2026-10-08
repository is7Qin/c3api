// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package supplier

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

// ViewStore 供应商视图装载持久面（repository 实现：accounts 归属 + supplier_balances
// 分成率，同一一致性视图；§4.3）。
type ViewStore interface {
	LoadSupplierView(ctx context.Context) (owner map[int64]int64, share map[int64]int, err error)
}

// ViewSink 视图发布面（proxy.SupplierSnapshot 实现：单原子指针换入一致视图；
// 失败保留旧视图 fail-safe）。
type ViewSink interface {
	Store(owner map[int64]int64, share map[int64]int, now time.Time)
}

// ViewConfig 视图装载配置。
type ViewConfig struct {
	// Interval 独立 ticker 周期（不借用 billing 的；§6.4）。<=0 ⇒ 兜底 10s。
	Interval time.Duration
	// WarnInterval NotReady 限频 Warn 的最小间隔（ops 可见；<=0 ⇒ 兜底 1m）。
	WarnInterval time.Duration
}

// ViewLoader 供应商视图装载器（§4.3/§4.4/§6.4）：独立 ticker 周期装载 +
// 一次成功 Store（失败保留旧视图）+ NotReady 限频 Warn。LoadOnce 串行化，
// 防慢的旧 Reload 覆盖新结果。
type ViewLoader struct {
	cfg   ViewConfig
	store ViewStore
	sink  ViewSink
	log   *logx.Logger

	mu          sync.Mutex // LoadOnce 串行化（ticker 与外部 Reload 不并发装载）
	started     atomic.Bool
	lastWarnMs  atomic.Int64
	loadAttempt atomic.Int64
	loadSuccess atomic.Int64
	// obs 视图快照三态观测面（装配期注入一次；Start 之前写、之后只读——见
	// SetObsProvider）。
	obs func(now time.Time) any
}

// NewViewLoader 构造视图装载器。
func NewViewLoader(cfg ViewConfig, store ViewStore, sink ViewSink, log *logx.Logger) *ViewLoader {
	return &ViewLoader{cfg: cfg, store: store, sink: sink, log: log}
}

// Name worker.Worker 契约。
func (l *ViewLoader) Name() string { return "supplier-view" }

// Start 启动 ticker（worker.GoLoop 监督契约）。首刷由装配期 LoadOnce 同步完成。
func (l *ViewLoader) Start(ctx context.Context) error {
	if !l.started.CompareAndSwap(false, true) {
		return nil
	}
	worker.GoLoop(ctx, "supplier-view", l.log, l.loop)
	return nil
}

func (l *ViewLoader) loop(ctx context.Context) {
	interval := l.cfg.Interval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.LoadOnce(ctx)
		}
	}
}

// Close 停止 ticker（无内部资源；loop 随 ctx 退出）。
func (l *ViewLoader) Close(ctx context.Context) error { return nil }

// Reload 外部触发的本地有界 Reload（accounts 写面失效路径 / 远端分派接入点）。
func (l *ViewLoader) Reload(ctx context.Context) bool { return l.LoadOnce(ctx) }

// LoadOnce 单次装载：成功 ⇒ Store 一次并清零 NotReady 计数；失败 ⇒ 保留旧视图 +
// 限频 Warn（否则「功能开了但从未装载成功」表现为收益永远为零而无人察觉，§4.4）。
func (l *ViewLoader) LoadOnce(ctx context.Context) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loadAttempt.Add(1)
	owner, share, err := l.store.LoadSupplierView(ctx)
	if err != nil {
		l.warnRateLimited("supplier view load failed", logx.Error(err))
		return false
	}
	l.sink.Store(owner, share, time.Now())
	l.loadSuccess.Add(1)
	return true
}

// Stats 装载可观测（ops 面）。
type ViewLoadStats struct {
	Attempts int64 `json:"attempts"`
	Success  int64 `json:"success"`
	// Snapshot 视图快照三态（loaded/revision/last_success_unix_ms/stale_age_ms；
	// §4.4/A13④）。装配侧经 SetObsProvider 注入（supplier 包不 import proxy——
	// 分层约束）；未注入 = nil（JSON 省略）。三态是「功能开了但从未装载成功」
	// 的唯一可见痕迹（收益恒零而无人察觉）。
	Snapshot any `json:"snapshot,omitempty"`
}

// Stats 返回装载计数 + 视图快照三态（实现 handler.StatsProvider——Name() +
// Stats() any，装配进 /api/admin/ops/workers）。
func (l *ViewLoader) Stats() any {
	st := ViewLoadStats{Attempts: l.loadAttempt.Load(), Success: l.loadSuccess.Load()}
	if l.obs != nil {
		st.Snapshot = l.obs(time.Now())
	}
	return st
}

// SetObsProvider 注入视图快照三态观测面（装配期一次，Start 之前；now 注入便于
// 测试）。supplier 包不直接依赖 proxy——由组合根经闭包桥接（Obs(now)）。
func (l *ViewLoader) SetObsProvider(obs func(now time.Time) any) { l.obs = obs }

func (l *ViewLoader) warnRateLimited(msg string, fields ...logx.Field) {
	if l.log == nil {
		return
	}
	interval := l.cfg.WarnInterval
	if interval <= 0 {
		interval = time.Minute
	}
	now := time.Now().UnixMilli()
	last := l.lastWarnMs.Load()
	if last != 0 && now-last < interval.Milliseconds() {
		return
	}
	l.lastWarnMs.Store(now)
	l.log.Warn(msg, fields...)
}
