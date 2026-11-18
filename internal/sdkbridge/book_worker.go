// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

// Codex 会话簿 worker：会话簿的轮次写经有界队列异步落 Redis，本 worker 按固定节拍
// 调 Book.Flush 排空；停机时在预算内最后 Flush 一次（尽量不丢已入队的轮次记录）。
// Stats() 暴露会话簿计数（conv_*）、本地拒绝计数（conv_local_rejects）与进程级整形
// 计数（shape_*）。
//
// 为什么在 sdkbridge 而不是 proxy：它 100% 是会话簿的生命周期（Flush 节拍、写入队列
// 的停机预算、计数器折叠），与 HTTP/WS 转发语义无关；而它的验收必须直接替换 SDK 的
// ConversationStore —— 留在 proxy 就等于让 proxy 的**测试**永久持有 SDK 符号。
//
// 纪律：Flush 的批与预算由 SDK 管（每批 256、2s）；worker 只负责节拍与停机。同一
// 时刻只有一个 Flush 在跑（节拍 goroutine 与 Close/FlushNow 由 flushMu 串行）。
// 热路径（Prepare 命中）与本 worker 无交互，不新增锁与分配。
//
// 取消纪律：SDK 的 Flush 先把条目出队成批再 SaveTurns，失败批不回队——所以节拍
// Flush 一律用 context.WithoutCancel（每批由 SDK 自带 2s 预算兜底），在途批次不会
// 被停机信号或 Close 打断；节拍循环只在两次 Flush 之间检查退出。循环的退出信号只
// 来自 Close（不随 StartAll 的信号 ctx 取消）：SIGTERM 后的 HTTP 排空窗口里仍在
// 入队的轮次照常按节拍落盘，不全压给 Close 的最后一次 Flush（对齐 ContBindWorker
// "批次 ctx 以 Background 为基"的先例）。
const conversationFlushIntervalDefault = 50 * time.Millisecond

// ConversationWorkerConfig 会话簿 worker 构造参数（零值字段取内部默认，不引入
// 运维配置键）。
type ConversationWorkerConfig struct {
	Interval time.Duration // Flush 节拍（默认 50ms；测试可注入）
	// LocalRejects 本地拒绝计数读取口（conv_local_rejects 的唯一来源仍在 proxy：
	// 拒绝发生在转发面，四处落点全在那里）。nil = 恒 0。
	LocalRejects func() int64
}

// ConversationWorker 会话簿 Flush worker（worker.Worker + handler.StatsProvider
// 契约，Name="codex-conversation"）。
type ConversationWorker struct {
	book     *Book
	log      *logx.Logger
	interval time.Duration
	rejects  func() int64

	// flushMu 串行化 Flush（SDK 要求同一时刻只有一个 Flush）：节拍、FlushNow 与
	// Close 的最后一次 Flush 互斥。
	flushMu sync.Mutex

	started   atomic.Bool
	loopCtx   context.Context // 节拍循环的退出信号（只由 Close 取消）
	cancel    context.CancelFunc
	loopDone  chan struct{}
	closeOnce sync.Once
	closeErr  error
}

// NewConversationWorker 构造会话簿 worker；book 须非 nil，log 可 nil（静默）。
func NewConversationWorker(book *Book, log *logx.Logger, cfg ConversationWorkerConfig) *ConversationWorker {
	if cfg.Interval <= 0 {
		cfg.Interval = conversationFlushIntervalDefault
	}
	return &ConversationWorker{
		book:     book,
		log:      log,
		interval: cfg.Interval,
		rejects:  cfg.LocalRejects,
		loopDone: make(chan struct{}),
	}
}

// Name worker.Worker 契约。
func (w *ConversationWorker) Name() string { return "codex-conversation" }

// Start 非阻塞启动节拍 goroutine（worker.Loop 托管，panic 不崩进程）。二次
// Start 返回错误。
func (w *ConversationWorker) Start(ctx context.Context) error {
	if !w.started.CompareAndSwap(false, true) {
		return fmt.Errorf("codex-conversation worker: already started")
	}
	// 剥离调用方取消（StartAll 的信号 ctx）、保留值：循环跑到 Close 为止。
	lctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	w.loopCtx, w.cancel = lctx, cancel
	go func() {
		defer close(w.loopDone)
		worker.Loop(lctx, "codex-conversation", w.log, w.loop)
	}()
	return nil
}

func (w *ConversationWorker) loop(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	// 在途 Flush 不受循环退出信号影响（见文件头"取消纪律"）。
	fctx := context.WithoutCancel(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.flush(fctx)
		}
	}
}

// flush 一次 Flush（串行）。错误只 Warn：失败批次 SDK 已丢弃并计入
// conv_save_turns_failures / conv_save_item_failures，不重试。
func (w *ConversationWorker) flush(ctx context.Context) {
	w.flushMu.Lock()
	err := w.book.Flush(ctx)
	w.flushMu.Unlock()
	if err != nil && ctx.Err() == nil {
		w.log.Warn("codex-conversation flush failed", logx.Error(err))
	}
}

// FlushNow 同步 Flush 一次（测试与跨副本用例的确定性屏障）。
func (w *ConversationWorker) FlushNow(ctx context.Context) {
	w.flush(ctx)
}

// Close 幂等：停节拍（在途 Flush 跑完，不被打断）→ 等 loop 退出（受 ctx 预算
// 约束）→ 在剩余预算内最后 Flush 一次。未 Start 也安全（直接最后一次 Flush）。
// 预算耗尽（loop 未按时退出，或轮到最后一次 Flush 时预算已到期）返回包装了
// ctx.Err() 的错误并 Warn——队列里可能还有未落盘的轮次，交给停机链报
// "shutdown incomplete"，不静默 nil。重复调用返回首次结果。
func (w *ConversationWorker) Close(ctx context.Context) error {
	w.closeOnce.Do(func() {
		if w.started.Load() {
			w.cancel()
			select {
			case <-w.loopDone:
			case <-ctx.Done():
				w.closeErr = fmt.Errorf("codex-conversation close: loop did not exit within budget: %w", ctx.Err())
				w.log.Warn("codex-conversation close: loop did not exit in time, final flush skipped", logx.Error(ctx.Err()))
				return
			}
		}
		if err := ctx.Err(); err != nil {
			w.closeErr = fmt.Errorf("codex-conversation close: budget exhausted before final flush: %w", err)
			w.log.Warn("codex-conversation close: budget exhausted, final flush skipped", logx.Error(err))
			return
		}
		w.flush(ctx)
	})
	return w.closeErr
}

// conversationWorkerStats /ops/workers 观测快照（handler.StatsProvider 形态）。
// json 键清单同步义务：openapi.yaml WorkerStatus.stats description + web locales
// ops.stats.*（前端缺 key 兜底显示原始字段名）。conv_* = 会话簿计数 + 本地拒绝；
// shape_* = 进程级整形计数。
type conversationWorkerStats struct {
	BookStats
	ShapeStats
	ConvLocalRejects int64 `json:"conv_local_rejects"`
}

// Stats handler.StatsProvider 契约：原子读组装（零锁）。
func (w *ConversationWorker) Stats() any {
	rejects := int64(0)
	if w.rejects != nil {
		rejects = w.rejects()
	}
	return conversationWorkerStats{
		BookStats:        w.book.Stats(),
		ShapeStats:       ShapeStatsSnapshot(),
		ConvLocalRejects: rejects,
	}
}
