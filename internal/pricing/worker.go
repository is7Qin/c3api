// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package pricing

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/adhocore/gronx"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

// defaultPriceSyncCron settings 快照缺失（启动加载失败等）时的兜底 cron。
const defaultPriceSyncCron = "0 3 * * *"

// retryDelay invalid cron 等调度失效时的重试间隔（1h 后重新解析——settings
// 变更下次循环生效，无需热加载通道）。
const retryDelay = time.Hour

// SettingReader 供 worker 读取价格同步设置（service 实现：settings 快照读，
// 零 DB；每次拉取/每轮调度循环现读 → 设置变更下次循环生效）。
type SettingReader interface {
	PriceSourceURL() string
	PriceSyncCron() string
}

// Upserter 拉取价落库（*repository.Repository 实现；500/批独立事务 + manual
// 行级互斥 WHERE source != 'manual'，部分成功可接受）。统一单表 + 变体批量。
type Upserter interface {
	UpsertPriceEntriesFromLiteLLM(ctx context.Context, rows []*domain.PriceEntry) (int, error)
	UpsertPriceVariantsFromLiteLLM(ctx context.Context, variants []*domain.PriceVariant) (int, error)
}

// SyncWorkerConfig SyncWorker 装配参数。
type SyncWorkerConfig struct {
	Fetcher  Fetcher
	Repo     Upserter
	Settings SettingReader
	// Reload 同步成功后刷新 service pricing 快照（svc.ReloadPricing）；nil 可
	// 用（纯落库不刷新快照，装配时必传真实实现）。
	Reload func()
	Log    *logx.Logger // nil 可用（静默）
}

// SyncWorker 模型价格同步 worker（worker.Worker 契约）：启动异步拉取一次 +
// gronx cron 定期循环。并发安全注记（评审 M-3）：手动 sync 与 cron 并发拉取
// 无锁——幂等安全（upsert 语义），最坏浪费一次 fetch，无需额外处理。
type SyncWorker struct {
	fetch    Fetcher
	repo     Upserter
	settings SettingReader
	reload   func()
	log      *logx.Logger
	// now/wait 可注入（测试）：now 固定时间基准（cron 数学确定性）；wait 替代
	// 真实 timer（测试免等真实时间）。默认实现见 waitReal。
	now       func() time.Time
	wait      func(ctx context.Context, d time.Duration) error
	startOnce atomic.Bool
	// running/lastSync 观测面（/ops/workers；低频路径原子写，零热路径成本）：
	// running 循环存活（Start 置位、cronLoop 退出复位——authSync/notify 同款，
	// 停机会观测 false）；startOnce 只是幂等守卫（"已 Start 过"，退出不复位），
	// 不作存活语义。lastSync 最近一次 fetch 尝试时刻（fetch 成败都记）。
	running  atomic.Bool
	lastSync atomic.Int64
}

// NewSyncWorker 构造同步 worker（now/wait 默认真实实现）。
func NewSyncWorker(cfg SyncWorkerConfig) *SyncWorker {
	w := &SyncWorker{
		fetch: cfg.Fetcher, repo: cfg.Repo, settings: cfg.Settings,
		reload: cfg.Reload, log: cfg.Log,
		now:  time.Now,
		wait: waitReal,
	}
	return w
}

// Name 满足 worker.Worker 契约。
func (w *SyncWorker) Name() string { return "pricing-sync" }

// Start 启动：异步拉取一次（独立 goroutine，不阻塞 Start/main——启动流程不被
// 外网延迟阻塞）+ cron 循环。重复 Start 幂等（返回错误）。启动拉取与 cron 首
// 触发可能并发（如 cron 为每分钟）——幂等安全，最坏浪费一次 fetch（M-3 同款）。
func (w *SyncWorker) Start(ctx context.Context) error {
	if !w.startOnce.CompareAndSwap(false, true) {
		return fmt.Errorf("pricing: sync worker already started")
	}
	// 观测面（running）生命周期收口在包装层：panic 重启后每次运行进出各置位
	// 一次，标志始终与"循环是否在跑"一致（cronLoop 本体不持有该状态）。
	worker.GoRecover("pricing-sync-once", w.log, func() { w.syncOnce(ctx) })
	worker.GoLoop(ctx, "pricing-sync-cron", w.log, func(ctx context.Context) {
		w.running.Store(true)
		defer w.running.Store(false)
		w.cronLoop(ctx)
	})
	return nil
}

// Close 满足 worker.Worker 契约：循环随 Start 的 ctx 取消而退出，无资源需排空；
// 幂等，未 Start 时也安全。
func (w *SyncWorker) Close(ctx context.Context) error { return nil }

// Sync 执行一次完整同步（fetch → 文本价 upsert → image 价 upsert → function
// 价 upsert → reload）：worker 内部路径与后续管理端手动触发（Task 3
// SyncPricingNow）共用；错误由调用方决定告警语义（worker 循环内 Warn 后等下
// 个周期）。
// 三线扩展：image 价与 function 价均与文本价独立判定、独立落库；拉取成功后
// 同样刷新对应快照（Reload 装配点由调用方聚合 pricing + image + function
// 三重载）。
func (w *SyncWorker) Sync(ctx context.Context) error {
	start := w.now()
	url := w.settings.PriceSourceURL()
	if url == "" {
		return fmt.Errorf("pricing: price_source_url not set, skip sync")
	}
	res, err := w.fetch.Fetch(ctx, url)
	w.lastSync.Store(time.Now().UnixMilli())
	if err != nil {
		return err
	}
	n, err := w.repo.UpsertPriceEntriesFromLiteLLM(ctx, res.PriceEntries)
	var nVar int
	var varErr error
	if len(res.Variants) > 0 {
		nVar, varErr = w.repo.UpsertPriceVariantsFromLiteLLM(ctx, res.Variants)
		if err == nil {
			err = varErr
		}
	}
	if w.reload != nil {
		w.reload()
	}
	if err == nil && w.log != nil {
		w.log.Info("pricing: sync done",
			logx.Int("rows", len(res.PriceEntries)),
			logx.Int("variants", len(res.Variants)),
			logx.Int("skipped", res.Skipped),
			logx.Int("updated", n), logx.Int("variants_updated", nVar),
			logx.Duration("elapsed", w.now().Sub(start)),
		)
	}
	return err
}

// syncOnce 单次同步 + 失败告警（不重试风暴——等下个周期）：fetch 失败 → Warn +
// 保留旧快照；upsert 部分失败 → Warn（已落库批仍生效）→ 仍刷新快照。
func (w *SyncWorker) syncOnce(ctx context.Context) {
	start := w.now()
	if err := w.Sync(ctx); err != nil {
		if w.log != nil {
			w.log.Warn("pricing: sync failed",
				logx.String("url", w.settings.PriceSourceURL()),
				logx.Duration("elapsed", w.now().Sub(start)), logx.Error(err))
		}
	}
}

// cronLoop 定期循环：每轮现读 cron 表达式（变更下次循环生效——settings 快照
// 读，无热加载通道）→ gronx 算下次触发 → timer 到点同步。ctx 取消退出；cron
// 非法 → Warn + 1h 后重新解析（settings 修正后自动恢复）。
func (w *SyncWorker) cronLoop(ctx context.Context) {
	for {
		d, err := w.nextDelay()
		if err != nil {
			if w.log != nil {
				w.log.Warn("pricing: invalid price_sync_cron",
					logx.String("cron", w.settings.PriceSyncCron()), logx.Error(err))
			}
			if werr := w.wait(ctx, retryDelay); werr != nil {
				return
			}
			continue
		}
		if err := w.wait(ctx, d); err != nil {
			return // ctx 取消（优雅退出）
		}
		w.syncOnce(ctx)
	}
}

// nextTrigger 下次 cron 触发时间（基于 w.now()；gronx 按参考时间的本地时区计算）。
// cron 空串 → 默认兜底（settings 快照加载失败时）。
func (w *SyncWorker) nextTrigger() (time.Time, error) {
	expr := w.settings.PriceSyncCron()
	if expr == "" {
		expr = defaultPriceSyncCron
	}
	g := gronx.New()
	if !g.IsValid(expr) {
		return time.Time{}, fmt.Errorf("pricing: invalid cron expression %q", expr)
	}
	next, err := gronx.NextTickAfter(expr, w.now(), false)
	if err != nil {
		return time.Time{}, fmt.Errorf("pricing: cron %q: %w", expr, err)
	}
	return next, nil
}

// nextDelay 距下次触发的时长（>= 0；测试注入 now 后仍确定性）。
func (w *SyncWorker) nextDelay() (time.Duration, error) {
	next, err := w.nextTrigger()
	if err != nil {
		return 0, err
	}
	d := next.Sub(w.now())
	if d < 0 {
		d = 0
	}
	return d, nil
}

// waitReal 默认等待：timer 到点返回 nil；ctx 取消返回 ctx.Err()。
func waitReal(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
