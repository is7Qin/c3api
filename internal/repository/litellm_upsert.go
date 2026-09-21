// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// 本文件承载 litellm 拉取批量 upsert 的共享机制（评审修复：pricing 与
// image_price 两仓库原先逐字复制，现抽 table-agnostic helper）：
//   - litellmUpsertBatches：分块 + 部分成功语义（失败批 Warn、首个错误上抛）
//   - litellmExecBatchWithRetry：单批执行 + 40P01 死锁瞬时重试
//
// 各仓库只保留本表的 SQL 组装（列清单/值装箱/事务执行，见 upsertLitellmBatch
// 与 upsertImageLitellmBatch），经 litellmBatchExec 契约接入。

// litellmBatchSize litellm 官方表 ~2k 模型，单条 INSERT 过大（评审 M-2）：
// 按 500/批分块执行（pricing 与 image_price 两表共用同一默认）。
const litellmBatchSize = 500

// litellmUpsertRetries 死锁（40P01）重试次数（同款兜底）：双实例
// litellm sync worker 并发批量 upsert 同批 model（锁顺序交错 → PG 判定
// deadlock detected 并终止一方，压测启动期偶发）。死锁为瞬时错误（PG 惯例
// 重试 1-2 次），重试成功即无影响；重试耗尽才返回错误 → 现有失败路径语义
// 不变（batch Warn + 首个错误上抛，下轮 price_sync_cron 补拉）。
const litellmUpsertRetries = 2

// litellmUpsertBackoff 死锁重试短退避（规避两实例同节奏再碰撞；死锁窗口内
// 最多追加 ~150ms 延迟）。
var litellmUpsertBackoff = []time.Duration{50 * time.Millisecond, 100 * time.Millisecond}

// litellmUpsertOpts 批量 upsert 参数（批大小/重试策略）：pricing 与
// image_price 共用默认值（litellmBatchSize/litellmUpsertRetries/
// litellmUpsertBackoff）；后续其他表如需不同策略可单独传参。
type litellmUpsertOpts struct {
	BatchSize int
	Retries   int
	Backoff   []time.Duration
}

// litellmBatchExec 单批 upsert 执行契约（table-agnostic）：start/end 为本批
// 在待写 rows 中的下标区间，调用方据此切片并组装本表的多行 VALUES SQL
// （批内排序 + 独立事务 + WHERE source != 'manual' 行级互斥），返回受影响
// 行数。
type litellmBatchExec func(ctx context.Context, start, end int) (int, error)

// litellmUpsertBatches 分块批量 upsert（部分成功语义，pricing/image_price
// 共用；评审修复——原先两仓库逐字复制）：
//   - 核心语义：ON CONFLICT (model) DO UPDATE ... WHERE <table>.source !=
//     'manual'——永不覆盖手动价（表内一行 = 最终生效价），由调用方的 SQL
//     组装保证（litellmBatchExec 契约）
//   - 分批 500/批、每批独立事务：部分成功可接受——返回成功行数，失败的批记
//     Warn 日志（返回首个失败错误，worker 侧决定重试/告警）；不影响已成功批
//   - 死锁收敛：批内排序（exec 内）+ 40P01 瞬时重试
//     （litellmExecBatchWithRetry）——多实例并发同批 model 不再
//     deadlock detected（排序消除主因，重试兜底残余交错）
//   - 返回 n = 实际插入/更新的行数（DO UPDATE 被 WHERE 过滤掉的手动行不计入；
//     PG 对未修改行不产生命令标签计数）
func litellmUpsertBatches(ctx context.Context, total int, logPrefix string, opts litellmUpsertOpts, exec litellmBatchExec) (int, error) {
	if total == 0 {
		return 0, nil
	}
	totalRows := 0
	var firstErr error
	for start := 0; start < total; start += opts.BatchSize {
		end := start + opts.BatchSize
		if end > total {
			end = total
		}
		n, err := litellmExecBatchWithRetry(ctx, opts, func(ctx context.Context) (int, error) {
			return exec(ctx, start, end)
		})
		if err != nil {
			log.Printf("%s: litellm upsert batch [%d:%d) failed: %v", logPrefix, start, end, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		totalRows += n
	}
	return totalRows, firstErr
}

// isDeadlock 判断死锁错误（SQLSTATE 40P01）：并发批量 upsert 同批目标行锁
// 顺序交错。pgx 原样透传 pgconn.PgError，errors.As 解包。
func isDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

// litellmExecBatchWithRetry 单批 upsert + 40P01 死锁重试（同款
// 兜底）：批内排序消除主因后，残余交错由此收敛。重试以整批独立事务为单位
// 重做（死锁回滚后无残留状态）；ctx 取消优先（不吞停机预算）。重试耗尽 →
// 原样返回错误（现有失败路径语义不变）。
func litellmExecBatchWithRetry(ctx context.Context, opts litellmUpsertOpts, exec func(ctx context.Context) (int, error)) (int, error) {
	var n int
	var err error
	for attempt := 0; attempt <= opts.Retries; attempt++ {
		if attempt > 0 {
			var delay time.Duration
			if attempt-1 < len(opts.Backoff) { // 退避表短于重试次数 → 该次不退避
				delay = opts.Backoff[attempt-1]
			}
			select { // 短退避；ctx 取消优先（不吞停机预算）
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(delay):
			}
		}
		n, err = exec(ctx)
		if !isDeadlock(err) {
			return n, err
		}
		// 40P01：死锁瞬时错误，重试；重试耗尽 → 原样返回（现有失败路径语义）
	}
	return n, err
}
