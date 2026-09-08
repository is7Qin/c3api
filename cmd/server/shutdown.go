// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/is7qin/c3api/internal/quality"
	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
	"github.com/is7qin/c3api/pkg/redisx"
)

const qualityRecorderCloseBudget = time.Second

// shutdownTail 优雅停机尾部（顺序契约：worker 反向排空 → quality recorder
// 终态快照 → Redis 客户端释放 → 终态日志）。独立成函数：让"排空失败"成为
// 可行为测试观测的接缝（shutdown_tail_test.go）。
//
// 排空失败显式上报（review blocker 2026-08-30）：wm.Shutdown 的返回错误曾被
// `_ =` 丢弃——quality-sync 等 worker 排空不完整时（PG/Redis 写失败，未落库
// pending refill 回 recorder，Close 返回 "drain incomplete ..."），进程照常
// 关闭 recorder 并以 Info "shutdown complete" 宣称干净退出。现契约：
//   - 排空失败以 Error 级日志收敛上报（Manager 内部仅 per-worker Warn，聚合
//     错误只有此处可见）；不改退出码、不阻塞退出——优雅停机语义保留，失败
//     由日志面观测。
//   - 终态不宣称 clean shutdown：失败时以 "shutdown incomplete" Error 收尾，
//     文案明示 pending 只保留在 recorder 终态快照（纯内存，随进程退出）——
//     数据面真实状态不粉饰。
func shutdownTail(shutdownCtx context.Context, wm *worker.Manager, qualityRecorder *quality.Recorder, rdb *redis.Client, log *logx.Logger) {
	// 反向排空 worker：quality-sync 的 Close 在此链内排空 Redis/PG，PG 失败
	// 时把未落库 quality/flow refill 回 recorder pending（重试语义）——
	// recorder 必须仍开着，关闭的 recorder 把 refill 当容量拒绝 → 静默丢弃。
	drainErr := wm.Shutdown(shutdownCtx)
	// recorder 收尾必须在 wm.Shutdown 之后：排空失败时 final snapshot 如实
	// 保留未落库 pending——这不是成功排空的终态。
	recorderCtx, cancelRecorder := context.WithTimeout(context.WithoutCancel(shutdownCtx), qualityRecorderCloseBudget)
	defer cancelRecorder()
	if err := qualityRecorder.CloseWithContext(recorderCtx); err != nil {
		log.Warn("quality recorder close failed", logx.Error(err))
	}
	// Redis 客户端最后释放（foundation spec §2.3：worker 排空完成后再关连接
	// 池——discovery 的停机 ZREM 等收尾命令都走在池上）。
	if err := redisx.Close(rdb); err != nil {
		log.Warn("redis client close failed", logx.Error(err))
	}
	if drainErr != nil {
		log.Error("shutdown incomplete: worker drain failed, pending quality/flow data retained only in recorder final snapshot", logx.Error(drainErr))
		return
	}
	log.Info("shutdown complete")
}
