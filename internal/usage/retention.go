// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 归属说明：retention worker 放 usage 包——原 Recorder.janitorLoop（逐行
// DELETE）已删除，保留策略整体移交本 worker；保留天数与 Recorder 配置同源
// （config usage.log_retention_days），故与 Recorder 同包管理。
package usage

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

// PartitionManager 分区管理面（repository.Repository 实现）：保留策略只需
// DROP 过期分区 + 预建未来分区，不感知分区表内部 DDL。now/until 由调用方
// 传入（start 边界由 now 推导，测试可注入时钟）。四表各自独立调度（保留期
// 独立：LogRetentionDays / ErrLogRetentionDays / StatsRetentionDays——后者由
// usage_stats 与 usage_entity_stats 共用，同一循环 DROP+预建）。
// DeleteRedemptionUsesBefore 是普通表（redemption_uses 无分区可 DROP）的
// 有界批删路径——同为保留策略的周期清理手段，归口本接口（F3-2）。
type PartitionManager interface {
	EnsureUsageLogPartitions(ctx context.Context, now, until time.Time) error
	DropUsageLogPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error)
	EnsureErrLogPartitions(ctx context.Context, now, until time.Time) error
	DropErrLogPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error)
	EnsureUsageStatsPartitions(ctx context.Context, now, until time.Time) error
	DropUsageStatsPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error)
	EnsureUsageEntityStatsPartitions(ctx context.Context, now, until time.Time) error
	DropUsageEntityStatsPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error)
	DeleteRedemptionUsesBefore(ctx context.Context, cutoff time.Time) (int, error)
}

// redemptionUseRetentionDays redemption_uses 保留窗口（TTL 定死 90 天，F3-2）。
// 90 天窗口内的兑换记录即审计证据，超窗删除不破坏审计语义——兑换审计只需
// 近期窗口（新近兑换可追溯），留存超出窗口的行无审计价值。
const redemptionUseRetentionDays = 90

// RetentionConfig retention worker 配置。
type RetentionConfig struct {
	LogRetentionDays    int           // usage_logs 分区保留天数（config usage.log_retention_days；<= 0 = 不删除）
	ErrLogRetentionDays int           // err_logs 分区保留天数（config usage.errlog_retention_days，默认 7 天短保留——错误审计；<= 0 = 不删除）
	StatsRetentionDays  int           // usage_stats 分区保留天数（config usage.stats_retention_days，默认 180 天——聚合统计长保留；<= 0 = 不删除）
	TickerInterval      time.Duration // 巡检周期（生产 1h；测试注入短周期；<= 0 兜底 1h）
}

// RetentionWorker 按日分区保留 worker（worker.Worker 契约，Name="retention"）：
// 每小时巡检一次——四表各自独立调度（usage_logs 按 LogRetentionDays、err_logs
// 按 ErrLogRetentionDays、usage_stats/usage_entity_stats 共用 StatsRetentionDays
// （同一循环 DROP+预建，180d），同一循环无新增 goroutine）：
//   - DROP 分区下界 < now - 保留天数的分区（DROP TABLE O(1)，比逐行 DELETE
//     快 5~6 个量级；按分区名日期判定，无需查元数据——usage_stats 保留清理
//     用户裁决 2026-08-11：PG DELETE 不释放空间，必须分区 DROP）
//   - 预建 当日 + 未来 1 天 分区（PG 无自动建分区，防日界跨区插入失败）
//   - redemption_uses 有界批删（F3-2）：普通表无分区可 DROP，同一循环内每轮
//     DELETE 至多 5000 行超窗行（TTL 定死 90 天，见 redemptionUseRetentionDays）
//     ——低频表单轮即清，超大批多轮收敛（每轮上限防长事务持锁）
//
// DROP × 在途插入竞态（评审 I-3）：DROP TABLE 需 ACCESS EXCLUSIVE 锁，与
// 在途插入事务串行；能落进被 DROP 分区（保留期前）的行只有回放/陈旧
// created_at 的延迟日志——该分区数据本就在保留语义内（要清理）。万一插入
// 恰好失败 → 走落库失败路径（Warn + 丢弃，与普通批量落库失败同语义，不自愈
// 不重试），可接受。
//
// 与 Recorder/ErrLogWorker 解耦（不依赖明细管道）：DROP 幂等，无排空需求，
// Close 直接返回 nil。
type RetentionWorker struct {
	cfg     RetentionConfig
	parts   PartitionManager
	log     *logx.Logger
	started atomic.Bool
	// 观测面（/ops/workers；runOnce 收尾原子写，零新增 DB）：lastPatrol 最近
	// 一次巡检完成时刻（UnixMilli；0 = 尚未巡检）；lastDrop* 最近成功轮各表
	// DROP 分区数（失败轮保留上轮值——缓存 runOnce 现有返回值，DROP 的 n 即
	// 真实 DB 答案）；usage_entity_stats 与 usage_stats 共用 StatsRetentionDays，
	// 独立计数 lastDropEntityStats。
	lastPatrol          atomic.Int64
	lastDropLogs        atomic.Int64
	lastDropErrLogs     atomic.Int64
	lastDropStats       atomic.Int64
	lastDropEntityStats atomic.Int64
}

func NewRetention(cfg RetentionConfig, parts PartitionManager, log *logx.Logger) *RetentionWorker {
	return &RetentionWorker{cfg: cfg, parts: parts, log: log}
}

// Name worker.Worker 契约（注册顺序无依赖——DROP/预建均幂等）。
func (w *RetentionWorker) Name() string { return "retention" }

func (w *RetentionWorker) Start(ctx context.Context) error {
	if !w.started.CompareAndSwap(false, true) {
		return fmt.Errorf("retention: already started")
	}
	worker.GoLoop(ctx, "retention", w.log, w.loop)
	return nil
}

func (w *RetentionWorker) loop(ctx context.Context) {
	interval := w.cfg.TickerInterval
	if interval <= 0 {
		interval = time.Hour
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	w.runOnce() // 启动即巡检（兜底 bootstrap 预建 + 清理历史遗留分区）
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.runOnce()
		}
	}
}

// runOnce 单轮巡检：三表各自 DROP 过期分区（独立 cutoff）+ 预建未来分区 +
// redemption_uses 有界批删（F3-2，TTL 定死 90 天）；失败 Warn 不中断循环（下一
// 轮重试）。now 现取一次，cutoff/ensure 边界共用同一时钟（评审 I-2：边界由调用
// 方 now 推导，不各取各的）。逐表错误隔离（一表失败不影响他表——C32 纪律：
// usage_stats 180 天 DROP 失败不连带明细表清理；redemption_uses 批删失败不
// 影响分区三表，反之亦然）。
func (w *RetentionWorker) runOnce() {
	now := time.Now()
	ctx := context.Background()
	// 各表 DROP 计数收尾原子写（观测面缓存 runOnce 现有返回值；失败不覆盖——
	// 保留上一轮值，LastPatrol 仍推进标记"巡检发生过"）。
	if w.cfg.LogRetentionDays > 0 {
		cutoff := now.AddDate(0, 0, -w.cfg.LogRetentionDays)
		n, err := w.parts.DropUsageLogPartitionsBefore(ctx, cutoff)
		if err != nil {
			if w.log != nil {
				w.log.Warn("retention drop usage_logs partitions failed", logx.Error(err))
			}
		} else {
			w.lastDropLogs.Store(int64(n))
			if n > 0 && w.log != nil {
				w.log.Info("retention dropped usage_logs partitions", logx.Int("count", n))
			}
		}
	}
	if w.cfg.ErrLogRetentionDays > 0 {
		cutoff := now.AddDate(0, 0, -w.cfg.ErrLogRetentionDays)
		n, err := w.parts.DropErrLogPartitionsBefore(ctx, cutoff)
		if err != nil {
			if w.log != nil {
				w.log.Warn("retention drop err_logs partitions failed", logx.Error(err))
			}
		} else {
			w.lastDropErrLogs.Store(int64(n))
			if n > 0 && w.log != nil {
				w.log.Info("retention dropped err_logs partitions", logx.Int("count", n))
			}
		}
	}
	if w.cfg.StatsRetentionDays > 0 {
		cutoff := now.AddDate(0, 0, -w.cfg.StatsRetentionDays)
		n, err := w.parts.DropUsageStatsPartitionsBefore(ctx, cutoff)
		if err != nil {
			if w.log != nil {
				w.log.Warn("retention drop usage_stats partitions failed", logx.Error(err))
			}
		} else {
			w.lastDropStats.Store(int64(n))
			if n > 0 && w.log != nil {
				w.log.Info("retention dropped usage_stats partitions", logx.Int("count", n))
			}
		}
		m, err2 := w.parts.DropUsageEntityStatsPartitionsBefore(ctx, cutoff)
		if err2 != nil {
			if w.log != nil {
				w.log.Warn("retention drop usage_entity_stats partitions failed", logx.Error(err2))
			}
		} else {
			w.lastDropEntityStats.Store(int64(m))
			if m > 0 && w.log != nil {
				w.log.Info("retention dropped usage_entity_stats partitions", logx.Int("count", m))
			}
		}
	}
	// redemption_uses 有界批删（F3-2）：TTL 定死 90 天（非配置项）——90 天窗口
	// 内的兑换记录即审计证据，超窗删除不破坏审计语义。每轮至多删 5000 行
	// （分区三表 O(1) DROP 之外的普通表清理路径），失败 Warn 下轮重试。
	n, err := w.parts.DeleteRedemptionUsesBefore(ctx, now.AddDate(0, 0, -redemptionUseRetentionDays))
	if err != nil {
		if w.log != nil {
			w.log.Warn("retention delete redemption_uses failed", logx.Error(err))
		}
	} else if n > 0 && w.log != nil {
		w.log.Info("retention deleted redemption_uses", logx.Int("count", n))
	}
	if err := w.parts.EnsureUsageLogPartitions(ctx, now, now.AddDate(0, 0, 1)); err != nil {
		if w.log != nil {
			w.log.Warn("retention pre-create usage_logs partitions failed", logx.Error(err))
		}
	}
	if err := w.parts.EnsureErrLogPartitions(ctx, now, now.AddDate(0, 0, 1)); err != nil {
		if w.log != nil {
			w.log.Warn("retention pre-create err_logs partitions failed", logx.Error(err))
		}
	}
	if err := w.parts.EnsureUsageStatsPartitions(ctx, now, now.AddDate(0, 0, 1)); err != nil {
		if w.log != nil {
			w.log.Warn("retention pre-create usage_stats partitions failed", logx.Error(err))
		}
	}
	if err := w.parts.EnsureUsageEntityStatsPartitions(ctx, now, now.AddDate(0, 0, 1)); err != nil {
		if w.log != nil {
			w.log.Warn("retention pre-create usage_entity_stats partitions failed", logx.Error(err))
		}
	}
	w.lastPatrol.Store(now.UnixMilli())
}

// Close 幂等（worker.Worker 契约）：DROP/预建均幂等，无排空需求。
func (w *RetentionWorker) Close(ctx context.Context) error { return nil }
