// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 离线聚合 worker（spec 2026-08-14 使用量统计离线聚合化 + spec 2026-08-23 v2
// 双表扩展）：请求路径零统计计算/投递——usage_stats（cube）与 usage_entity_
// stats（实体卷积）只由本 worker 每周期从 DB（usage_logs + err_logs）重建
// （SQL 侧聚合，DELETE+INSERT 覆盖语义）。放行行（含 abort）从 usage_logs
// 重建（全字段）；纯错误行（4xx/5xx/network）从 err_logs 重建（count 语义）；
// 拒绝行随 err_logs 采样队列丢样（口径注释见 proxy/forward.go recordRejected）。
// 统计分钟级陈旧（接受，不加 hot counter）；quota 回写在线保留（Recorder
// flushQuota，独立于本 worker）。
package usage

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

// StatsAggStore 离线聚合存储面（repository.StatRepo 实现）：两范围分离 +
// 单事务覆盖落盘 + watermark + 会话级 advisory lock 全部在 repo 侧
// （stat_repo.go/stat_entity_agg.go——pgx 直查直写，ent 无数组列类型）。
type StatsAggStore interface {
	// AcquireStatsAggLock 抢占会话级 advisory lock（专用连接持有到 release；
	// 抢锁失败 ok=false——其他实例在聚合，本轮跳过）。
	AcquireStatsAggLock(ctx context.Context) (release func(), ok bool, err error)
	// LoadAggRange 重算范围 [from,to) 双结果集全量重建：cube 两查询 +
	// entity 六查询（含消费明细行数——cube 两查询 count(*) 合计）。
	LoadAggRange(ctx context.Context, from, to time.Time) ([]*domain.StatBucket, []*domain.EntityStatBucket, int64, error)
	// AggregateRange 单事务 DELETE cube [delFrom,delTo) + INSERT cube +
	// DELETE entity [同范围] + INSERT entity + watermark 推进 wmTo（= 读窗口 T，
	// ≠ 重算范围上界——P1-A 两范围分离；双表同一事务原子回滚）。
	AggregateRange(ctx context.Context, delFrom, delTo, wmTo time.Time, cube []*domain.StatBucket, entity []*domain.EntityStatBucket) error
	LoadStatsAggWatermark(ctx context.Context) (time.Time, error)
	InitStatsAggWatermark(ctx context.Context, t time.Time) error
}

// StatsAggConfig 离线聚合 worker 配置。
type StatsAggConfig struct {
	Interval time.Duration // 聚合周期（config usage.stats_agg_interval，默认 5m；0 = 禁用——Start 直接返回）
	Lag      time.Duration // 读窗口滞后（读窗口 [W, T)，T = now − Lag；watermark 只推进到 T）
}

// statsAggCatchUpLimit 停摆恢复后单周期最大读窗口（spec 评审 P2-1 追赶上限：
// 单周期窗口 ≤ 1h 分批收敛，防单次超大窗口扫全史 + 大 DELETE 长事务）。
const statsAggCatchUpLimit = time.Hour

// defaultStatsAggLag 读窗口滞后（spec 评审 P2-4）：滞后 ≥ max(两表落库节奏)
// ——usage_logs flush 默认间隔 500ms 与 errlog worker 默认 500ms flush
// （errlog.go:101-102）+ 队内滞留；取固定安全值 5s = 2×max 节奏 + 滞留余量
// （4s 余量），注释写明依据。var（非 const）：测试注入小值。
var defaultStatsAggLag = 5 * time.Second

// StatsAggWorkerStats 离线聚合 worker 观测（/ops/workers；runOnce 收尾原子写，
// 零新增 DB）。
type StatsAggWorkerStats struct {
	// WatermarkUnixMs watermark 位置（毫秒；0 = 尚未推进——未初始化/首轮未完成）
	WatermarkUnixMs int64 `json:"watermark_unix_ms"`
	// LastBuckets 上轮写入桶数（cube + entity 合计；失败轮保留上轮值）
	LastBuckets int64 `json:"last_buckets"`
	// LastRows 上轮消费明细行数（cube 两查询 count(*) 合计——实体六查询扫的
	// 是同批源行不重复计数；失败轮保留上轮值）
	LastRows int64 `json:"last_rows"`
	// LastDurationMs 上轮耗时（毫秒；失败轮保留上轮值）
	LastDurationMs int64 `json:"last_duration_ms"`
}

// StatsAggWorker 离线聚合 worker（worker.Worker 契约，Name="stats-agg"）：
// 每周期一个两范围 + 双结果集（cube 两查询 + entity 六查询）+ 单事务流程
// （spec §3）：
//
//	读窗口 [W, T)：W = watermark，T = now − 滞后——只推进 watermark，不直接
//	              用于 DELETE（部分小时桶边界问题，见下）
//	重算范围 [R0, R1) = [trunc_hour(W), trunc_hour(T) + 1h)——DELETE + SELECT
//	              共同边界（cube 与 entity 两表同范围）
//	LoadAggRange(R0, R1) → AggregateRange(R0, R1, T, cube, entity) 单事务落盘
//
// **两范围分离（评审 P1-A，核心正确性）**：小时桶是部分完成的桶（跨多周期
// 累积），直接按读窗口 DELETE 会截断当前小时桶（[小时起点, W) 的行丢失）→
// 每周期欠计。小时对齐扩展后：SELECT 覆盖已消费行无害（DELETE 先清、INSERT
// 全量覆盖，幂等仍成立——重放同范围结果一致）。watermark 只推进到 T（原始读
// 位置），**不推进到 R1**——推进到 R1 会永久跳过 [T, R1) 的行（正是 P1-A 要防
// 的错误形态；签名显式分离见 AggregateRange 的 wmTo 参数）。
//
// 幂等/重放（issue #8 教训）：DELETE+INSERT+watermark 推进同一事务——崩溃
// 回滚 → 游标不动 → 重算恢复不双计；重复执行同范围（手动重算修正）结果一致
// （覆盖语义）。
//
// 并发防护：pg_try_advisory_lock（会话级，专用连接持有整个周期——池连接复用
// 即丢锁，P3）；抢锁失败 → 本轮跳过（其他实例在聚合）。单写者语义由此钉死，
// 事务内串行无 40P01 重试需求（P2-5 取舍：advisory lock 串行下单写者）。
//
// watermark 存储/初始化/追赶（评审 P2-1）：单行 watermark 表（stats_agg_
// watermark，bootstrap 建表见 partition.go）；**全新库初始化 = now − 滞后**
// （防首跑扫全史 + DELETE 撞 retention 已 DROP 分区）；ON CONFLICT DO NOTHING
// 容忍多实例并发初始化（败者重读既有值）；**追赶上限**：停摆恢复后单周期
// 窗口 ≤ 1h 分批收敛（防单次超大窗口）。
//
// **手动重建运维口径（Momus B1 勘误）**：worker 对缺失 watermark 行的初始化
// 硬编码 now−lag——"清空 watermark 行"不会触发历史回算。手动重建统计必须：
// (1) 清空 usage_stats / usage_entity_stats 数据；(2) **手工种子单行 watermark**
// 至最早保留小时边界（INSERT INTO stats_agg_watermark (id, watermark)
// VALUES (1, '<最早保留小时>')），否则历史窗口永远聚合不出数据（worker 只从
// watermark 起追赶，且受 1h/周期上限分批收敛）。
type StatsAggWorker struct {
	cfg     StatsAggConfig
	store   StatsAggStore
	log     *logx.Logger
	started atomic.Bool
	// now 时钟注入（默认 time.Now；测试注入固定时钟——评审 I-2 惯例：边界由
	// 调用方 now 推导，不内部各取各的）。
	now func() time.Time
	// 观测面（runOnce 收尾原子写，零新增 DB）：watermark 位置 / 上轮桶数 /
	// 上轮行数 / 上轮耗时（失败轮保留上轮值——缓存 runOnce 现有返回值）。
	lastWatermark atomic.Int64
	lastBuckets   atomic.Int64
	lastRows      atomic.Int64
	lastDuration  atomic.Int64
}

func NewStatsAgg(cfg StatsAggConfig, store StatsAggStore, log *logx.Logger) *StatsAggWorker {
	w := &StatsAggWorker{cfg: cfg, store: store, log: log, now: time.Now}
	if w.cfg.Lag <= 0 {
		w.cfg.Lag = defaultStatsAggLag
	}
	return w
}

// Name worker.Worker 契约。
func (w *StatsAggWorker) Name() string { return "stats-agg" }

// Start worker.Worker 契约：Interval <= 0 = 禁用聚合（config 0 语义——不启动
// 循环，Close 直接返回；等价于不装配本 worker）。
func (w *StatsAggWorker) Start(ctx context.Context) error {
	if !w.started.CompareAndSwap(false, true) {
		return fmt.Errorf("stats agg: already started")
	}
	if w.cfg.Interval <= 0 {
		return nil // 禁用聚合（usage.stats_agg_interval = 0）
	}
	worker.GoLoop(ctx, "stats-agg", w.log, w.loop)
	return nil
}

func (w *StatsAggWorker) loop(ctx context.Context) {
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	w.runOnce() // 启动即聚合（停摆恢复/冷启动快速收敛——watermark 初始化后立即追赶）
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.runOnce()
		}
	}
}

// runOnce 单轮聚合（两范围 + 双结果集 + 单事务，见类型注释）；任一步失败 →
// Warn + 跳过（watermark 未推进 → 下轮重试不双计）。抢锁失败 → 静默跳过
// （其他实例在聚合，非异常）。
func (w *StatsAggWorker) runOnce() {
	ctx := context.Background()
	start := w.now()
	release, ok, err := w.store.AcquireStatsAggLock(ctx)
	if err != nil {
		if w.log != nil {
			w.log.Warn("stats agg: advisory lock failed", logx.Error(err))
		}
		return
	}
	if !ok {
		return // 其他实例持有锁：本轮跳过（静默——多实例正常互斥形态）
	}
	defer release() // 专用连接持有整个周期（P3：池连接复用即丢锁）

	// watermark 读取/初始化（全新库 = now − 滞后；ON CONFLICT DO NOTHING 容忍
	// 多实例并发初始化）。
	wm, err := w.store.LoadStatsAggWatermark(ctx)
	if err != nil {
		w.warnErr("load watermark", err)
		return
	}
	if wm.IsZero() {
		if err := w.store.InitStatsAggWatermark(ctx, start.Add(-w.cfg.Lag)); err != nil {
			w.warnErr("init watermark", err)
			return
		}
		wm, err = w.store.LoadStatsAggWatermark(ctx) // 败者重读既有值（并发初始化）
		if err != nil {
			w.warnErr("reload watermark", err)
			return
		}
		if wm.IsZero() { // 防御：初始化后仍无行（表缺失/权限）——显式失败下轮重试
			w.warnErr("watermark missing after init", fmt.Errorf("stats_agg_watermark empty"))
			return
		}
	}

	// 读窗口 [W, T)：T = now − 滞后（只推进 watermark，不直接用于 DELETE）。
	// 追赶上限：停摆恢复后单周期窗口 ≤ 1h 分批收敛（评审 P2-1）。
	t := start.Add(-w.cfg.Lag)
	if t.Sub(wm) > statsAggCatchUpLimit {
		t = wm.Add(statsAggCatchUpLimit)
	}
	if !t.After(wm) {
		w.lastWatermark.Store(wm.UnixMilli()) // 观测面推进（无新数据也记录当前位置）
		return
	}

	// 重算范围 [R0, R1) = [trunc_hour(W), trunc_hour(T) + 1h)：小时对齐扩展——
	// DELETE + SELECT 共同边界（P1-A 部分小时桶不截断，见类型注释）。
	r0 := wm.UTC().Truncate(time.Hour)
	r1 := t.UTC().Truncate(time.Hour).Add(time.Hour)

	cube, entity, detailRows, err := w.store.LoadAggRange(ctx, r0, r1)
	if err != nil {
		w.warnErr("load agg range", err)
		return
	}
	if err := w.store.AggregateRange(ctx, r0, r1, t, cube, entity); err != nil {
		w.warnErr("aggregate range", err)
		return
	}
	// 观测面收尾原子写（成功轮才推进；失败轮保留上轮值）。
	w.lastWatermark.Store(t.UnixMilli())
	w.lastBuckets.Store(int64(len(cube) + len(entity)))
	w.lastRows.Store(detailRows)
	w.lastDuration.Store(time.Since(start).Milliseconds())
	if w.log != nil {
		w.log.Info("stats agg cycle complete",
			logx.Int("cube_buckets", len(cube)), logx.Int("entity_buckets", len(entity)),
			logx.Int64("rows", detailRows),
			logx.Int64("watermark_ms", t.UnixMilli()),
			logx.String("range", r0.UTC().Format("2006-01-02T15:04Z")+".."+r1.UTC().Format("2006-01-02T15:04Z")))
	}
}

// warnErr 失败 Warn（统一文案——worker 自愈：watermark 未推进，下轮重试不双计）。
func (w *StatsAggWorker) warnErr(step string, err error) {
	if w.log != nil {
		w.log.Warn("stats agg cycle failed", logx.String("step", step), logx.Error(err))
	}
}

// Close 幂等（worker.Worker 契约）：无排空需求（usage_stats 由 DB 侧覆盖语义
// 收敛，worker 停摆期间的落后窗口由重启后追赶上限分批收敛）。
func (w *StatsAggWorker) Close(ctx context.Context) error { return nil }

// Stats 满足 handler.StatsProvider（观测面：watermark/上轮桶数/上轮行数/上轮
// 耗时；失败轮保留上轮值——对齐 RetentionWorker 观测纪律）。
func (w *StatsAggWorker) Stats() any {
	return StatsAggWorkerStats{
		WatermarkUnixMs: w.lastWatermark.Load(),
		LastBuckets:     w.lastBuckets.Load(),
		LastRows:        w.lastRows.Load(),
		LastDurationMs:  w.lastDuration.Load(),
	}
}
