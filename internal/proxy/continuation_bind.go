// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 异步续接绑定 worker（REST 流式）：删除 REST 闸门后，业务帧不再等待 Redis
// 绑定确认——首个有效响应 id 帧在写出接缝处**同步快照**为 continuation.BindRequest
// 投入有界队列，本 worker 常驻批量落库。绑定语义由"生产侧 fail-closed（绑完才
// 可见）"改为"消费侧 fail-closed（lookup/pin 校验）"；worker 的丢弃/失败/冲突
// **只计数、绝不回写当前响应**。
//
// 纪律对齐 internal/usage/errlog.go：有界队列 + 非阻塞投递（队列满 → 丢弃 +
// 原子计数）；worker.GoLoop 托管；停机**先禁入队再排空**。批节奏：从首条入批起
// 最多等 BatchWait，或攒够 BatchSize；每批 Redis 操作受 BatchTimeout 约束。
package proxy

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/continuation"
	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

// 内部默认（不引入运维配置键）：进程内常量，构造期零值即取默认。
const (
	contBindQueueDefault     = 8192
	contBindWorkersDefault   = 1
	contBindBatchSizeDefault = 256
	contBindBatchWaitDefault = 2 * time.Millisecond
	contBindTimeoutDefault   = 2 * time.Second
)

// ContBindConfig 异步绑定 worker 构造参数（零值字段取内部默认）。
type ContBindConfig struct {
	QueueSize    int           // 有界队列容量（背压面：满 → 丢弃计数）
	Workers      int           // 并发消费 goroutine 数
	BatchSize    int           // 每批最多条数
	BatchWait    time.Duration // 从首条入批起的最大等待（凑批）
	BatchTimeout time.Duration // 每批 Redis 操作超时
}

// ContBindWorker 异步绑定 worker（worker.Worker 契约，Name="cont-bind"）。
type ContBindWorker struct {
	store *continuation.Store
	log   *logx.Logger
	cfg   ContBindConfig

	ch chan continuation.BindRequest

	// mu 保护 closed 与投递（Enqueue 短临界区：closed 检查 + 非阻塞 send）。
	// Close 置位 closed 后再排空——与投递互斥串行，无"排空尾窗口静默丢"。
	mu     sync.Mutex
	closed bool

	started   atomic.Bool
	loopDone  chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once

	dropped   atomic.Int64 // 队列满/已关闭丢弃（观测）
	conflicts atomic.Int64 // CAS conflict（保留旧绑定；不得声称为必然 410）
	failed    atomic.Int64 // 批量失败/逐条 I/O 错误（观测）
	bound     atomic.Int64 // created/refreshed 成功落库计数
}

// NewContBindWorker 构造异步绑定 worker；store 为绑定存储（须非 nil），
// log 可 nil（静默）。
func NewContBindWorker(store *continuation.Store, log *logx.Logger, cfg ContBindConfig) *ContBindWorker {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = contBindQueueDefault
	}
	if cfg.Workers <= 0 {
		cfg.Workers = contBindWorkersDefault
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = contBindBatchSizeDefault
	}
	if cfg.BatchWait <= 0 {
		cfg.BatchWait = contBindBatchWaitDefault
	}
	if cfg.BatchTimeout <= 0 {
		cfg.BatchTimeout = contBindTimeoutDefault
	}
	return &ContBindWorker{
		store:    store,
		log:      log,
		cfg:      cfg,
		ch:       make(chan continuation.BindRequest, cfg.QueueSize),
		loopDone: make(chan struct{}),
	}
}

// Name worker.Worker 契约。
func (w *ContBindWorker) Name() string { return "cont-bind" }

// Start 非阻塞启动消费 goroutine（worker.Loop 托管，panic 不崩进程）。幂等：
// 二次 Start 返回错误。
func (w *ContBindWorker) Start(ctx context.Context) error {
	if !w.started.CompareAndSwap(false, true) {
		return fmt.Errorf("cont-bind worker: already started")
	}
	w.wg.Add(w.cfg.Workers)
	for i := 0; i < w.cfg.Workers; i++ {
		go func() {
			defer w.wg.Done()
			worker.Loop(ctx, "cont-bind", w.log, w.loop)
		}()
	}
	go func() {
		defer close(w.loopDone)
		w.wg.Wait()
	}()
	return nil
}

// Enqueue 非阻塞有界投递：队列满或已关闭 → 丢弃 + 计数（绝不阻塞请求热路径）。
func (w *ContBindWorker) Enqueue(req continuation.BindRequest) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		w.dropped.Add(1)
		return
	}
	select {
	case w.ch <- req:
	default:
		w.dropped.Add(1)
	}
}

// loop 消费循环：阻塞取首条 → 凑批（最多 BatchWait / BatchSize）→ 批量落库。
// ctx 取消即退出（最终排空由 Close 以 shutdown 预算执行）。
func (w *ContBindWorker) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case first := <-w.ch:
			batch := make([]continuation.BindRequest, 0, w.cfg.BatchSize)
			batch = append(batch, first)
			w.collect(ctx, &batch)
			// 批次 ctx 以 Background 为基，避免停机取消在途批次（同步调用，
			// loop 退出前必已收尾；对齐 errlog.flush）。
			w.flush(context.Background(), batch)
		}
	}
}

// collect 从首条入批起继续取至多 BatchSize（或 BatchWait 到点、ctx 取消）。
func (w *ContBindWorker) collect(ctx context.Context, batch *[]continuation.BindRequest) {
	if len(*batch) >= w.cfg.BatchSize {
		return
	}
	timer := time.NewTimer(w.cfg.BatchWait)
	defer timer.Stop()
	for len(*batch) < w.cfg.BatchSize {
		select {
		case req := <-w.ch:
			*batch = append(*batch, req)
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

// takeBatch 非阻塞取至多 BatchSize 条（Close 独占排空时调用）。
func (w *ContBindWorker) takeBatch() []continuation.BindRequest {
	batch := make([]continuation.BindRequest, 0, w.cfg.BatchSize)
	for len(batch) < w.cfg.BatchSize {
		select {
		case req := <-w.ch:
			batch = append(batch, req)
		default:
			return batch
		}
	}
	return batch
}

// flush 单批落库（受 BatchTimeout 约束）：批量失败按批内条数计 failed；成功时
// 逐条分类计数（created/refreshed → bound，conflict → conflicts，其余 → failed）。
func (w *ContBindWorker) flush(base context.Context, batch []continuation.BindRequest) {
	if len(batch) == 0 {
		return
	}
	if w.store == nil {
		w.failed.Add(int64(len(batch)))
		return
	}
	ctx, cancel := context.WithTimeout(base, w.cfg.BatchTimeout)
	defer cancel()
	results, err := w.store.CreateOrRefreshBatch(ctx, batch)
	if err != nil {
		w.failed.Add(int64(len(batch)))
		if w.log != nil {
			w.log.Warn("cont-bind batch failed", logx.Error(err), logx.Int("batch", len(batch)))
		}
		return
	}
	for i := range results {
		switch {
		case results[i].Err != nil:
			w.failed.Add(1)
		case results[i].Status == "conflict":
			w.conflicts.Add(1)
		case results[i].Status == "created" || results[i].Status == "refreshed":
			w.bound.Add(1)
		default:
			w.failed.Add(1)
		}
	}
}

// Close 幂等排空：置位 closed（此后 Enqueue 丢弃计数）→ 等 loop 退出（受 ctx
// 预算约束）→ 排空剩余（每批 ≤ BatchSize，受 ctx 预算约束；预算耗尽 → 剩余
// 丢弃计数）。未 Start 也安全（跳过 loop 等待直接排空）。
func (w *ContBindWorker) Close(ctx context.Context) error {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		w.closed = true
		w.mu.Unlock()
		if w.started.Load() {
			select {
			case <-w.loopDone:
			case <-ctx.Done():
				if w.log != nil {
					w.log.Warn("cont-bind close: loop did not exit in time")
				}
			}
		}
		for {
			if ctx.Err() != nil {
				w.dropped.Add(int64(len(w.ch)))
				break
			}
			batch := w.takeBatch()
			if len(batch) == 0 {
				break
			}
			w.flush(ctx, batch)
		}
	})
	return nil
}

// Dropped 队列满/已关闭丢弃计数（背压观测）。
func (w *ContBindWorker) Dropped() int64 { return w.dropped.Load() }

// Conflicts CAS conflict 计数（保留旧绑定；不等于下次必然 410——Lookup 可能
// 命中旧记录）。
func (w *ContBindWorker) Conflicts() int64 { return w.conflicts.Load() }

// Failed 批量失败/逐条 I/O 错误计数（观测；不影响当前响应）。
func (w *ContBindWorker) Failed() int64 { return w.failed.Load() }

// Bound 成功落库（created/refreshed）计数（观测/测试）。
func (w *ContBindWorker) Bound() int64 { return w.bound.Load() }

// Queued 当前队列积压条数（背压观测）。
func (w *ContBindWorker) Queued() int { return len(w.ch) }
