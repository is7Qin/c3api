// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package usage

// err_logs worker 单测：有界队列满非阻塞丢弃 + 计数、双队列
// 豁免（拒绝风暴不丢双轨行）、flush 批界、Close 完整排空/预算截断、InsertBatch
// 失败豁免回灌重试 + 拒绝按采样语义丢弃（双计数口径）、尾
// 窗口竞态、告警边沿。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// captureErrLogInserter 记录 InsertBatch 批（批/字段断言）。
type captureErrLogInserter struct {
	mu      sync.Mutex
	batches [][]*domain.UsageLog
	fail    bool // true = 注入失败
}

func (c *captureErrLogInserter) InsertBatch(ctx context.Context, logs []*domain.UsageLog) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return errors.New("injected insert failure")
	}
	c.batches = append(c.batches, logs)
	return nil
}

// setFail 注入/解除落库失败（持锁写，避免与 InsertBatch 读竞态）。
func (c *captureErrLogInserter) setFail(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fail = v
}

func (c *captureErrLogInserter) rows() []*domain.UsageLog {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*domain.UsageLog
	for _, b := range c.batches {
		out = append(out, b...)
	}
	return out
}

func (c *captureErrLogInserter) batchCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.batches)
}

func rejectLog(i int) *domain.UsageLog {
	return &domain.UsageLog{
		RequestID: "rej-" + string(rune('a'+i%26)),
		Model:     "m", Format: domain.FormatOpenAIChat,
		StatusCode: 429, ErrorType: domain.Err429, CreatedAt: time.Now(),
	}
}

func errorLog(i int) *domain.UsageLog {
	return &domain.UsageLog{
		RequestID: "err-" + string(rune('a'+i%26)),
		Model:     "m", Format: domain.FormatOpenAIChat,
		StatusCode: 502, ErrorType: domain.Err5xx, CreatedAt: time.Now(),
	}
}

func newTestErrLogWorker(writer ErrLogInserter, queueSize int) *ErrLogWorker {
	return NewErrLogWorker(ErrLogConfig{
		QueueSize: queueSize, FlushInterval: time.Hour,
	}, writer, nil)
}

// TestErrLogWorkerPersists 正常落盘：拒绝 + 双轨行混合入队 → Close 完整排空，
// 字段保留（request_id/error_type/status/错误文本）。
func TestErrLogWorkerPersists(t *testing.T) {
	store := &captureErrLogInserter{}
	w := newTestErrLogWorker(store, 64)
	msg := "key quota exhausted"
	l := rejectLog(0)
	l.ErrorMessage = &msg
	w.EnqueueRejected(l)
	w.EnqueueRejected(rejectLog(1))
	w.EnqueueError(errorLog(2))
	w.EnqueueError(errorLog(3))

	require.NoError(t, w.Close(context.Background()))
	require.Equal(t, int64(4), w.Inserted())
	require.Zero(t, w.DroppedReject()+w.DroppedExempt(), "无丢弃")
	rows := store.rows()
	require.Len(t, rows, 4)
	got := map[string]*domain.UsageLog{}
	for _, r := range rows {
		got[r.RequestID] = r
	}
	require.NotNil(t, got["rej-a"].ErrorMessage)
	require.Equal(t, msg, *got["rej-a"].ErrorMessage, "错误文本保留")
	require.Equal(t, domain.Err429, got["rej-a"].ErrorType)
	require.Equal(t, 429, got["rej-a"].StatusCode)
	require.Equal(t, domain.Err5xx, got["err-c"].ErrorType)
	require.Equal(t, 502, got["err-c"].StatusCode)
}

// TestErrLogWorkerRejectStormSamplesAndExemptSurvives 核心：拒绝风暴灌满
// 普通队列 → 拒绝行采样丢弃（非阻塞、计数正确、队列有界）——双轨行走豁免
// 队列**不丢**（无消费方下 EnqueueError 全部入队）。
func TestErrLogWorkerRejectStormSamplesAndExemptSurvives(t *testing.T) {
	store := &captureErrLogInserter{}
	w := newTestErrLogWorker(store, 64) // 小队列放大风暴效应；无消费方（不 Start）

	const rejectN = 100_000
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < rejectN; i++ {
			w.EnqueueRejected(rejectLog(i))
		}
	}()
	select { // 热路径不阻塞（背压核心）
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("EnqueueRejected 风暴必须非阻塞完成（有界队列 select-default）")
	}

	// 双轨行（豁免通道）恒入队：无消费方 + 风暴后仍全部进豁免队列
	for i := 0; i < 100; i++ {
		w.EnqueueError(errorLog(i))
	}
	require.Equal(t, int64(rejectN-64), w.DroppedReject(), "拒绝行丢弃计数 = 到达 - 队列容量")
	require.Zero(t, w.DroppedExempt(), "双轨行零丢弃（豁免）")
	require.Equal(t, 64, len(w.rejectQ), "拒绝队列有界（内存稳定）")
	require.Equal(t, 100, len(w.exemptQ), "豁免队列全量入队（不参与采样）")
	require.Equal(t, 164, w.Queued())

	require.NoError(t, w.Close(context.Background()))
	require.Equal(t, int64(64+100), w.Inserted(), "队列内拒绝行 + 全部双轨行完整排空")
	require.Zero(t, w.Queued())
}

// TestErrLogWorkerBatches 批界：单批 ≤ BatchSize（Close 排空逐批）。
func TestErrLogWorkerBatches(t *testing.T) {
	store := &captureErrLogInserter{}
	w := NewErrLogWorker(ErrLogConfig{QueueSize: 4096, BatchSize: 50, FlushInterval: time.Hour}, store, nil)
	for i := 0; i < 120; i++ {
		w.EnqueueRejected(rejectLog(i))
	}
	require.NoError(t, w.Close(context.Background()))
	require.Equal(t, 3, store.batchCount(), "120 行 / 50 批界 = 3 批")
	for _, b := range store.batches {
		require.LessOrEqual(t, len(b), 50, "单批 ≤ BatchSize")
	}
}

// slowErrLogInserter 慢落库（每批 sleep——预算截断路径模拟）。
type slowErrLogInserter struct {
	latency time.Duration
}

func (s *slowErrLogInserter) InsertBatch(ctx context.Context, logs []*domain.UsageLog) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.latency):
	}
	return nil
}

// TestErrLogWorkerCloseTruncatesOnBudget 停机排空受预算约束：deadline 到期 →
// 截断退出 + Warn（flushed/remaining），不无阻塞停机。
func TestErrLogWorkerCloseTruncatesOnBudget(t *testing.T) {
	w := NewErrLogWorker(ErrLogConfig{QueueSize: 4096, BatchSize: 100, FlushInterval: time.Hour},
		&slowErrLogInserter{latency: 30 * time.Millisecond}, nil)
	for i := 0; i < 1000; i++ {
		w.EnqueueRejected(rejectLog(i))
	}
	logger, out := newTestErrLogLogger(t)
	w.log = logger

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Millisecond)
	defer cancel()
	require.NoError(t, w.Close(ctx))
	require.Greater(t, w.Inserted(), int64(0), "预算内尽量排空（至少一批）")
	require.Greater(t, w.DroppedReject()+w.DroppedExempt(), int64(0),
		"截断剩余计入丢弃计数（落库失败/截断批并入类计数——对账指标）")
	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(b), "shutdown budget exceeded, truncated drain")
}

// TestErrLogWorkerCloseTruncationCountsQueueBacklog 精确回归：预算已到期
// 时 Close 立即截断——截断面 = 本批（BatchSize 100，双轨优先：50 双轨 + 50 拒绝）
// + 两队列剩余积压（拒绝 200），全部并入 dropped 对账指标，不低估；本批按来源
// 拆类计数（droppedExempt 只计豁免行、droppedReject 只计拒绝行）。
func TestErrLogWorkerCloseTruncationCountsQueueBacklog(t *testing.T) {
	w := NewErrLogWorker(ErrLogConfig{QueueSize: 4096, BatchSize: 100, FlushInterval: time.Hour},
		&captureErrLogInserter{}, nil)
	for i := 0; i < 250; i++ {
		w.EnqueueRejected(rejectLog(i))
	}
	for i := 0; i < 50; i++ {
		w.EnqueueError(errorLog(i))
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	require.NoError(t, w.Close(ctx))
	require.Zero(t, w.Inserted(), "预算已到期：无任何落库")
	require.Equal(t, int64(50), w.DroppedExempt(), "本批豁免行 50 按类计入双轨丢弃（对齐 Close 截断按类拆对）")
	require.Equal(t, int64(250), w.DroppedReject(), "本批拒绝行 50 + 拒绝队列剩余 200 按类计入（对账不低估）")
}

// TestErrLogWorkerInsertFailureDrops Close 排空失败止损（改写——旧行为"失败即
// 丢"已废除）：豁免行回灌重试一次仍失败 → 按类丢弃、拒绝行直接按采样语义丢弃
// ——双计数各自归位（droppedExempt 只计豁免行、droppedReject 只计拒绝行），
// 不再整批混计双轨丢弃。
func TestErrLogWorkerInsertFailureDrops(t *testing.T) {
	store := &captureErrLogInserter{fail: true}
	w := newTestErrLogWorker(store, 64)
	w.EnqueueError(errorLog(1))
	w.EnqueueRejected(rejectLog(1))
	require.NoError(t, w.Close(context.Background()))
	require.Zero(t, w.Inserted())
	require.Equal(t, int64(1), w.DroppedExempt(), "豁免行重试一次仍失败 → 按类丢弃（不无限重试）")
	require.Equal(t, int64(1), w.DroppedReject(), "拒绝行按采样语义丢弃（计数入类）")
}

// TestErrLogWorkerInsertFailureRequuesForRetry 核心：flush 落库失败 →
// 豁免行回灌 exemptQ 下轮重试（保 provenance——不随拒绝行丢弃）、拒绝行按采样
// 语义丢弃；DB 恢复后下轮 flush 回灌行落盘成功（旧行为"首次失败即丢"已被
// 回灌重试取代，违反不变式 A2 的整批丢弃路径不再存在）。
func TestErrLogWorkerInsertFailureRequuesForRetry(t *testing.T) {
	store := &captureErrLogInserter{fail: true}
	w := newTestErrLogWorker(store, 64)
	w.EnqueueError(errorLog(1))
	w.EnqueueRejected(rejectLog(1))
	w.flush() // 失败 → 豁免行回灌、拒绝行丢弃
	require.Zero(t, w.Inserted())
	require.Equal(t, int64(1), w.DroppedReject(), "拒绝行按采样语义丢弃（计数入类）")
	require.Zero(t, w.DroppedExempt(), "豁免行回灌不丢（双轨行恒落盘语义由重试兑现）")
	require.Equal(t, 1, len(w.exemptQ), "豁免行回灌 exemptQ 等下轮重试")
	require.Zero(t, len(w.rejectQ), "拒绝队列已清空（本批拒绝行丢弃）")
	// DB 恢复 → 下轮 flush 回灌行落盘成功
	store.setFail(false)
	w.flush()
	require.Equal(t, int64(1), w.Inserted(), "回灌豁免行下轮落盘成功")
	require.Zero(t, w.DroppedExempt())
	require.Zero(t, w.Queued(), "队列排空")
}

// TestErrLogWorkerMixedBatchFailure 混合批次失败（豁免行稀疏 + 拒绝行补位——
// 实证为风暴期常态）：exempt 全部回灌重试、reject 全部按采样语义丢弃、
// 双计数各自归位（droppedExempt 只计豁免行、droppedReject 只计拒绝
// 行——对账口径，拒绝风暴不再错类混计）。
func TestErrLogWorkerMixedBatchFailure(t *testing.T) {
	store := &captureErrLogInserter{fail: true}
	w := newTestErrLogWorker(store, 64)
	for i := 0; i < 10; i++ {
		w.EnqueueError(errorLog(i))
	}
	for i := 0; i < 20; i++ {
		w.EnqueueRejected(rejectLog(i))
	}
	w.flush()
	require.Zero(t, w.Inserted())
	require.Equal(t, int64(20), w.DroppedReject(), "拒绝行全部按采样语义丢弃（拒绝风暴采样面）")
	require.Zero(t, w.DroppedExempt(), "豁免行全部回灌（0 丢弃）")
	require.Equal(t, 10, len(w.exemptQ), "豁免行全部回灌 exemptQ")
	require.Zero(t, len(w.rejectQ), "拒绝队列已清空")
	// DB 恢复 → 回灌豁免行全部落盘
	store.setFail(false)
	w.flush()
	require.Equal(t, int64(10), w.Inserted())
	require.Equal(t, int64(20), w.DroppedReject())
	require.Zero(t, w.DroppedExempt())
	require.Zero(t, w.Queued())
}

// TestErrLogWorkerPersistentFailureBoundedBackpressure DB 持续故障硬约束：回灌
// 不产生无限增长——豁免队列恒 ≤ 容量（有界队列即反压面），新到达豁免行按既有
// 溢出采样语义丢弃计数（内存上界不破坏）；DB 恢复后回灌行全部落盘。
func TestErrLogWorkerPersistentFailureBoundedBackpressure(t *testing.T) {
	store := &captureErrLogInserter{fail: true}
	w := NewErrLogWorker(ErrLogConfig{QueueSize: 64, ExemptQueueSize: 8, BatchSize: 50, FlushInterval: time.Hour}, store, nil)
	for i := 0; i < 100; i++ {
		w.EnqueueError(errorLog(i))
	}
	require.Equal(t, int64(92), w.DroppedExempt(), "豁免队列溢出按既有采样语义丢弃")
	require.Equal(t, 8, len(w.exemptQ), "豁免队列有界")
	for i := 0; i < 10; i++ { // 持续故障多轮 flush：回灌恒 ≤ 容量，不增长
		w.flush()
		require.LessOrEqual(t, len(w.exemptQ), 8, "回灌不产生无限增长（内存上界）")
		require.Zero(t, w.DroppedReject())
	}
	require.Equal(t, int64(92), w.DroppedExempt(), "回灌本身不新增丢弃（恒 ≤ 容量回填）")
	// DB 恢复 → 回灌行全部落盘
	store.setFail(false)
	w.flush()
	require.Equal(t, int64(8), w.Inserted(), "DB 恢复后回灌行全部落盘")
	require.Zero(t, w.Queued())
}

// TestErrLogWorkerCloseTailWindowNoSilentLoss：Close 与 Enqueue 并发——置位
// closed 后无尾窗口静默丢（inserted + dropped == 全部投递）。
func TestErrLogWorkerCloseTailWindowNoSilentLoss(t *testing.T) {
	store := &captureErrLogInserter{}
	w := newTestErrLogWorker(store, 4096)

	const total = 20_000
	enqueued := atomic.Int64{}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(4)
	for g := 0; g < 4; g++ {
		go func(g int) {
			defer wg.Done()
			i := 0
			for {
				select {
				case <-stop:
					return
				default:
					w.EnqueueRejected(rejectLog(g*1000 + i))
					enqueued.Add(1)
					i++
				}
			}
		}(g)
	}
	// 风暴投递同时 Close（预算充足——排空应覆盖全部已入队行）
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, w.Close(ctx))
	close(stop)
	wg.Wait()

	accounted := w.Inserted() + w.DroppedReject() + w.DroppedExempt()
	require.Equal(t, enqueued.Load(), accounted,
		"无尾窗口静默丢：inserted+dropped == 全部投递")
	require.Zero(t, w.Queued(), "排空后队列空")
}

// TestErrLogWorkerDropWarnEdge：丢弃累计 ≥ 阈值 → Warn 恰好一次；队列排空
// （flush 空批）边沿回落 → 再次风暴再次 Warn（每风暴一次，不刷屏）。
func TestErrLogWorkerDropWarnEdge(t *testing.T) {
	old := errlogDropWarnThreshold
	errlogDropWarnThreshold = 50
	t.Cleanup(func() { errlogDropWarnThreshold = old })

	store := &captureErrLogInserter{}
	w := newTestErrLogWorker(store, 8)
	logger, out := newTestErrLogLogger(t)
	w.log = logger

	// 第一轮风暴（无消费方）→ 一次 Warn
	for i := 0; i < 100; i++ {
		w.EnqueueRejected(rejectLog(i))
	}
	w.flush() // 排空队列 → 告警边沿回落
	// 第二轮风暴 → 再次 Warn
	for i := 0; i < 100; i++ {
		w.EnqueueRejected(rejectLog(i))
	}
	w.flush()
	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, 2, strings.Count(string(b), "errlog queue full, dropping"),
		"每风暴一次 Warn（边沿回落）")
	require.NoError(t, w.Close(context.Background()))
}

// newTestErrLogLogger warn 级文件 logger（Warn 断言用；Windows 句柄 best-effort）。
func newTestErrLogLogger(t *testing.T) (*logx.Logger, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "errlog-test-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	out := filepath.Join(dir, "out.json")
	logger, err := logx.New("warn", out)
	require.NoError(t, err)
	return logger, out
}
