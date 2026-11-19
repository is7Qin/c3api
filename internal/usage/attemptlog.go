// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package usage

// attempt_logs 尝试明细落盘 worker（spec 2026-11-18 retry-observability §3.3）。
//
// 与 ErrLogWorker 的**刻意差别**——这不是抄漏，是语义不同：
//   - 单队列：这里没有「双轨行 vs 拒绝行」的来源之分，attempt 行全部同等价值；
//   - 无回灌重试：一条尝试明细丢了就丢了（它不参与计费，也不构成审计义务），
//     回灌只会把 DB 故障期变成内存与重试放大；
//   - 无 provenance：链式查询靠 request_id + ordinal 关联，不靠投递顺序。
//
// 背压：有界队列 + 投递非阻塞（队列满 → 丢弃 + 原子计数）——热路径绝不因 DB
// 慢而阻塞请求；DB 写速率上界 = BatchSize/FlushInterval，不随请求放大。失败按
// 采样语义丢弃（计数 + Warn 边沿一次），绝不重试。
//
// 丢弃**按原因分账**（DroppedFull / DroppedFailed / DroppedClose /
// DroppedClosed，Dropped 是四者之和）：对账时分不清「背压」与「DB 故障」等于
// 没有对账——一个要调批量/节奏，一个要修库，处置完全不同。
//
// 落盘行数：单次请求最多 8~N 行（failover 尝试数），但队列按「行」计——多请求
// 合并成批，BatchSize=100 是有界内存与写放大的折中（err_logs 的 500 不适用：
// 它的行只有拒绝/失败时才产生，而这里每一次上游失败都产生一行）。

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

// AttemptLogConfig attempt_logs 落盘 worker 节奏。
type AttemptLogConfig struct {
	QueueSize     int           // 队列容量（满 → 丢弃；默认 8192）
	BatchSize     int           // 每批落盘行数（默认 100）
	FlushInterval time.Duration // 批间隔（默认 500ms；DB 写速率上界 = BatchSize/FlushInterval）
}

// AttemptLogInserter attempt_logs 批量插入面（repository.AttemptLogRepo 实现）。
type AttemptLogInserter interface {
	InsertBatch(ctx context.Context, rows []*domain.AttemptLog) error
}

const (
	defaultAttemptLogQueueSize     = 8192
	defaultAttemptLogBatchSize     = 100
	defaultAttemptLogFlushInterval = 500 * time.Millisecond
)

// attemptLogFlushTimeout 单批落盘超时（防慢 DB/挂死连接无界占用 worker 循环；
// 超时即失败——按采样语义丢弃，不重试、不阻塞请求）。
const attemptLogFlushTimeout = 5 * time.Second

// attemptLogDropWarnThreshold 丢弃累计告警阈值：达到后 Warn 恰好一次（队列排空
// 后边沿回落——每次持续堆积只告警一次，不刷屏）。var（非 const）：测试注入小
// 阈值。
var attemptLogDropWarnThreshold int64 = 10_000

// errAttemptLogNoInserter 构造期校验：没有落库面就没有 worker（宁可在装配期
// 报错，也不要在热路径上空指针 panic）。
var errAttemptLogNoInserter = errors.New("usage: attempt_logs worker needs a non-nil inserter")

// AttemptLogWorker attempt_logs 落盘 worker（worker.Worker 契约，Name="attemptlog"）。
type AttemptLogWorker struct {
	cfg    AttemptLogConfig
	writer AttemptLogInserter
	log    *logx.Logger
	// mu 保护 closed 与投递（Enqueue 短临界区：closed 检查 + 非阻塞 send）。
	// Close 置位 closed 后再排空——与投递互斥串行，无"排空尾窗口静默丢"。
	mu     sync.Mutex
	closed bool
	// q 有界队列（容量 cfg.QueueSize；投递 select-default 非阻塞）。
	q chan *domain.AttemptLog
	// 丢弃按原因分账（见文件头）：混成一个数只能看出"丢了"，看不出该干什么。
	droppedFull   atomic.Int64 // 队列满丢弃（Enqueue 的 default 分支）= 背压
	droppedFailed atomic.Int64 // 落库失败整批丢弃（insert 失败；不回灌不重试）= DB 故障
	droppedClose  atomic.Int64 // 停机预算截断（Close 截断分支）= 停机预算不够
	// droppedClosed 是 **worker 已关之后**的投递（排空完成后仍有写入）：生命周期
	// 错误，与上面三种"运行期丢弃"不是一回事——单列，免得污染三者的语义。
	droppedClosed atomic.Int64
	inserted      atomic.Int64 // 成功落盘累计（观测/测试）
	warnDropped   atomic.Bool  // 丢弃告警边沿（≥ 阈值告警一次；队列排空回落）
	warnFailed    atomic.Bool  // 落库失败告警边沿（连续失败只告警一次；成功一次后复位）
	started       atomic.Bool
	loopDone      chan struct{}
	closeOnce     sync.Once
}

// NewAttemptLogWorker 构造 worker：nil inserter 直接报错（构造即校验，不把空
// 指针留到热路径）；零值节奏字段按默认值填充。
func NewAttemptLogWorker(cfg AttemptLogConfig, writer AttemptLogInserter, log *logx.Logger) (*AttemptLogWorker, error) {
	if writer == nil {
		return nil, errAttemptLogNoInserter
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultAttemptLogQueueSize
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultAttemptLogBatchSize
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = defaultAttemptLogFlushInterval
	}
	return &AttemptLogWorker{
		cfg: cfg, writer: writer, log: log,
		q:        make(chan *domain.AttemptLog, cfg.QueueSize),
		loopDone: make(chan struct{}),
	}, nil
}

// Name worker.Worker 契约。
func (w *AttemptLogWorker) Name() string { return "attemptlog" }

// Start 起落盘循环（重复 Start 报错：第二次调用意味着装配写错了）。
func (w *AttemptLogWorker) Start(ctx context.Context) error {
	if !w.started.CompareAndSwap(false, true) {
		return errors.New("attemptlog worker: already started")
	}
	go func() {
		defer close(w.loopDone)
		worker.Loop(ctx, "attemptlog", w.log, w.loop)
	}()
	return nil
}

func (w *AttemptLogWorker) loop(ctx context.Context) {
	t := time.NewTicker(w.cfg.FlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// 最终排空由 Close 以 shutdown 预算 ctx 执行（停机纪律）：本 loop 的
			// ctx 在 SIGTERM 时已取消，拿它 flush 会恒截断丢整批。
			return
		case <-t.C:
			w.flush()
		}
	}
}

// Enqueue 投递一条尝试明细。**热路径**：非阻塞（队列满即丢弃计数），不分配、
// 不做日志、不取第二把锁。
func (w *AttemptLogWorker) Enqueue(row *domain.AttemptLog) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		w.droppedClosed.Add(1)
		return
	}
	select {
	case w.q <- row:
	default:
		n := w.droppedFull.Add(1)
		if n >= attemptLogDropWarnThreshold && w.warnDropped.CompareAndSwap(false, true) {
			w.log.Warn("attemptlog queue full, dropping (bounded backpressure)",
				logx.Int64("dropped_full", n), logx.Int64("dropped_total", w.Dropped()),
				logx.Int64("threshold", attemptLogDropWarnThreshold),
				logx.Int("queue_cap", cap(w.q)))
		}
	}
}

// flush 取至多 BatchSize 行批量落库（单写者：仅 loop 调用；Close 在 loopDone
// 之后独占队列）。失败 ⇒ 整批按采样语义丢弃 + 计数（不回灌：见文件头）。
func (w *AttemptLogWorker) flush() {
	batch := w.take()
	if len(batch) == 0 {
		// 队列排空 ⇒ 丢弃告警边沿回落（下次再堆起来还要告警一次）。
		w.warnDropped.Store(false)
		return
	}
	if w.insert(batch) {
		w.inserted.Add(int64(len(batch)))
	}
}

// insert 落库一批；返回是否成功。失败计数 + 告警边沿（连续失败只告警一次，
// 成功一次后复位——DB 长时间故障不会把日志刷爆）。
func (w *AttemptLogWorker) insert(batch []*domain.AttemptLog) bool {
	ctx, cancel := context.WithTimeout(context.Background(), attemptLogFlushTimeout)
	defer cancel()
	if err := w.writer.InsertBatch(ctx, batch); err != nil {
		failed := w.droppedFailed.Add(int64(len(batch)))
		if w.warnFailed.CompareAndSwap(false, true) {
			w.log.Warn("attemptlog batch insert failed, dropping batch (retry disabled by design)",
				logx.Error(err), logx.Int("rows", len(batch)),
				logx.Int64("dropped_failed", failed), logx.Int64("dropped_total", w.Dropped()),
				logx.String("request_id", batch[0].RequestID))
		}
		return false
	}
	w.warnFailed.Store(false)
	return true
}

// take 从队列取至多 BatchSize 行（非阻塞轮询——队列空即返回）。单写者/Close
// 独占调用，无锁。
func (w *AttemptLogWorker) take() []*domain.AttemptLog {
	batch := make([]*domain.AttemptLog, 0, w.cfg.BatchSize)
	for len(batch) < w.cfg.BatchSize {
		select {
		case row := <-w.q:
			batch = append(batch, row)
		default:
			return batch
		}
	}
	return batch
}

// Close 幂等排空（优雅停机）：置位 closed（此后 Enqueue 全部丢弃计数——排空
// 无尾窗口竞态）→ 等 loop 退出（受预算约束）→ 逐批落库，ctx 到期即截断退出并
// 把剩余下发计入丢弃（对账指标不低估），不阻塞停机。
func (w *AttemptLogWorker) Close(ctx context.Context) error {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		w.closed = true
		w.mu.Unlock()
		if w.started.Load() {
			select {
			case <-w.loopDone:
			case <-ctx.Done():
				w.log.Warn("attemptlog close: loop did not exit in time")
			}
		}
		var flushed int64
		for {
			batch := w.take()
			if len(batch) == 0 {
				break
			}
			if ctx.Err() != nil {
				remaining := len(batch) + len(w.q)
				w.droppedClose.Add(int64(remaining))
				w.log.Warn("attemptlog close: shutdown budget exceeded, truncated drain",
					logx.Int64("flushed_rows", flushed), logx.Int64("remaining_rows", int64(remaining)),
					logx.Int64("dropped_close", int64(remaining)), logx.Int64("dropped_total", w.Dropped()))
				return
			}
			if w.insert(batch) {
				w.inserted.Add(int64(len(batch)))
				flushed += int64(len(batch))
			}
		}
		w.log.Info("attemptlog worker closed",
			logx.Int64("inserted_rows", w.inserted.Load()),
			logx.Int64("dropped_full", w.droppedFull.Load()),
			logx.Int64("dropped_failed", w.droppedFailed.Load()),
			logx.Int64("dropped_close", w.droppedClose.Load()),
			logx.Int64("dropped_closed", w.droppedClosed.Load()),
			logx.Int64("dropped_total", w.Dropped()))
	})
	return nil
}

// Dropped 丢弃总数 = 四个原因之和（对账口径：到达 - 落盘，不低估）。
//
// 只想知道"一共丢了多少"读它；想知道"为什么丢"读下面四个——背压、DB 故障、
// 停机预算、生命周期错误是四种完全不同的处置。
func (w *AttemptLogWorker) Dropped() int64 {
	return w.droppedFull.Load() + w.droppedFailed.Load() + w.droppedClose.Load() + w.droppedClosed.Load()
}

// DroppedFull 队列满丢弃（背压）：投递速率 > 落盘速率。处置 = 调大 BatchSize/
// 缩短 FlushInterval，或承认上游失败率本身就该被削。
func (w *AttemptLogWorker) DroppedFull() int64 { return w.droppedFull.Load() }

// DroppedFailed 落库失败整批丢弃（DB 故障）：处置 = 修库；此时所有明细都在丢。
func (w *AttemptLogWorker) DroppedFailed() int64 { return w.droppedFailed.Load() }

// DroppedClose 停机预算截断（最后一批没排完）：处置 = 调大停机预算。
func (w *AttemptLogWorker) DroppedClose() int64 { return w.droppedClose.Load() }

// DroppedClosed worker 已关之后的投递（生命周期错误：Close 之后仍有人写）：
// 处置 = 查装配/停机顺序，不是调参。
func (w *AttemptLogWorker) DroppedClosed() int64 { return w.droppedClosed.Load() }

// Inserted 成功落盘累计（观测/测试）。
func (w *AttemptLogWorker) Inserted() int64 { return w.inserted.Load() }

// Queued 当前队列积压行数（≤ QueueSize）。
func (w *AttemptLogWorker) Queued() int { return len(w.q) }
