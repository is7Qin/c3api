// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package usage

// attempt_logs 落盘 worker 单测（不碰 DB：fake inserter）：
// 批界 / 队列满丢弃计数 / Close 排空 / 停机预算截断 / 落库失败按采样丢弃 +
// 告警边沿一次 / nil inserter 构造期拒绝。
//
// 等待一律用 Close（显式排空）或直接调 flush（注入触发）——不用睡眠：这个
// worker 的正确性不该依赖「等一会儿」。
//
// 本包测试串行（AGENTS.md 全仓禁用并行测试）。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// captureAttemptLogInserter 记录 InsertBatch 批（批/字段断言）。
type captureAttemptLogInserter struct {
	mu      sync.Mutex
	batches [][]*domain.AttemptLog
	fail    bool
}

func (c *captureAttemptLogInserter) InsertBatch(ctx context.Context, rows []*domain.AttemptLog) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return errors.New("injected insert failure")
	}
	c.batches = append(c.batches, rows)
	return nil
}

func (c *captureAttemptLogInserter) setFail(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fail = v
}

func (c *captureAttemptLogInserter) rows() []*domain.AttemptLog {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*domain.AttemptLog
	for _, b := range c.batches {
		out = append(out, b...)
	}
	return out
}

func (c *captureAttemptLogInserter) batchSizes() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]int, 0, len(c.batches))
	for _, b := range c.batches {
		out = append(out, len(b))
	}
	return out
}

// attemptRow 造一条尝试明细（字段有区分度，便于断言未串行化错位）。
func attemptRow(i int) *domain.AttemptLog {
	return &domain.AttemptLog{
		RequestID:  "req-" + string(rune('a'+i%26)),
		Ordinal:    int16(i%8 + 1),
		AccountID:  int64(100 + i),
		Lane:       "lane-a",
		Format:     "openai-chat",
		StatusCode: 502,
		Outcome:    "upstream_error",
		ErrorType:  "5xx",
		LatencyMS:  int64(i),
		CreatedAt:  time.Now().UTC(),
	}
}

// newTestAttemptLogWorker 造 worker（FlushInterval 拉长到小时：测试靠 Close /
// 直接 flush 驱动，不靠 ticker 的时序）。
func newTestAttemptLogWorker(t *testing.T, writer AttemptLogInserter, queueSize int) *AttemptLogWorker {
	t.Helper()
	w, err := NewAttemptLogWorker(AttemptLogConfig{QueueSize: queueSize, FlushInterval: time.Hour}, writer, nil)
	require.NoError(t, err)
	return w
}

// TestAttemptLogWorkerRejectsNilInserter 构造期校验：没有落库面就不该有 worker
// （空指针留到热路径就是一次 panic）。
func TestAttemptLogWorkerRejectsNilInserter(t *testing.T) {
	w, err := NewAttemptLogWorker(AttemptLogConfig{}, nil, nil)
	require.Nil(t, w)
	require.ErrorIs(t, err, errAttemptLogNoInserter)
}

// TestAttemptLogWorkerDefaults 零值配置填默认：8192 / 100 / 500ms（批量 100 不是
// err_logs 的 500——每次失败尝试一行，多请求合并成批）。
func TestAttemptLogWorkerDefaults(t *testing.T) {
	w, err := NewAttemptLogWorker(AttemptLogConfig{}, &captureAttemptLogInserter{}, nil)
	require.NoError(t, err)
	require.Equal(t, defaultAttemptLogQueueSize, w.cfg.QueueSize)
	require.Equal(t, defaultAttemptLogBatchSize, w.cfg.BatchSize)
	require.Equal(t, defaultAttemptLogFlushInterval, w.cfg.FlushInterval)
	require.Equal(t, cap(w.q), w.cfg.QueueSize, "队列按容量建")
	require.Equal(t, "attemptlog", w.Name(), "worker.Worker 的 Name 是装配/排空顺序的键")
}

// TestAttemptLogWorkerPersists Close 完整排空：字段逐个保留（含 nil 语义——
// CommitState/ErrorMessage 为 nil 表示「未知」，不是空串）。
func TestAttemptLogWorkerPersists(t *testing.T) {
	store := &captureAttemptLogInserter{}
	w := newTestAttemptLogWorker(t, store, 64)
	msg := "upstream 502"
	withState := attemptRow(1)
	state := "upstream_responded"
	withState.CommitState = &state
	withState.ErrorMessage = &msg
	w.Enqueue(attemptRow(0))
	w.Enqueue(withState)
	w.Enqueue(attemptRow(2))

	require.NoError(t, w.Close(context.Background()))
	require.Equal(t, int64(3), w.Inserted())
	require.Zero(t, w.Dropped(), "无丢弃")
	require.Zero(t, w.Queued())

	rows := store.rows()
	require.Len(t, rows, 3)
	got := map[string]*domain.AttemptLog{}
	for _, r := range rows {
		got[r.RequestID] = r
	}
	require.Equal(t, int16(1), got["req-a"].Ordinal, "ordinal 原样落盘（允许空洞，不重排）")
	require.Equal(t, int64(100), got["req-a"].AccountID)
	require.Equal(t, "lane-a", got["req-a"].Lane)
	require.Nil(t, got["req-a"].CommitState, "nil = 未知（落库 NULL），不是空串")
	require.Nil(t, got["req-a"].ErrorMessage)
	require.NotNil(t, got["req-b"].CommitState)
	require.Equal(t, "upstream_responded", *got["req-b"].CommitState)
	require.Equal(t, msg, *got["req-b"].ErrorMessage)
}

// TestAttemptLogWorkerQueueFullDrops 有界队列 + 非阻塞投递（背压核心）：无消费方
// 灌满队列 → 多出的全部丢弃并计数，投递不阻塞。
func TestAttemptLogWorkerQueueFullDrops(t *testing.T) {
	w := newTestAttemptLogWorker(t, &captureAttemptLogInserter{}, 8)
	const sent = 100
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range sent {
			w.Enqueue(attemptRow(i))
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Enqueue 必须非阻塞完成（有界队列 + select-default）")
	}
	require.Equal(t, int64(sent-8), w.Dropped(), "丢弃计数 = 到达 - 队列容量")
	require.Equal(t, 8, w.Queued(), "队列有界（内存上界不随风暴增长）")
}

// TestAttemptLogWorkerBatches 批界：单批 ≤ BatchSize（120 行 / 50 = 3 批）。
func TestAttemptLogWorkerBatches(t *testing.T) {
	store := &captureAttemptLogInserter{}
	w, err := NewAttemptLogWorker(AttemptLogConfig{QueueSize: 4096, BatchSize: 50, FlushInterval: time.Hour}, store, nil)
	require.NoError(t, err)
	for i := range 120 {
		w.Enqueue(attemptRow(i))
	}
	require.NoError(t, w.Close(context.Background()))
	require.Equal(t, []int{50, 50, 20}, store.batchSizes(), "逐批取满再取尾批")
	require.Equal(t, int64(120), w.Inserted())
}

// TestAttemptLogWorkerFlushInsertFailureDrops 落库失败按采样语义丢弃：不回灌、
// 不重试，只计数（这条表不是审计义务，见文件头）。
func TestAttemptLogWorkerFlushInsertFailureDrops(t *testing.T) {
	store := &captureAttemptLogInserter{fail: true}
	w := newTestAttemptLogWorker(t, store, 64)
	w.Enqueue(attemptRow(0))
	w.Enqueue(attemptRow(1))
	w.flush()
	require.Zero(t, w.Inserted())
	require.Equal(t, int64(2), w.Dropped(), "整批丢弃计数")
	require.Zero(t, w.Queued(), "不回灌（有界队列不是重试缓冲）")

	// DB 恢复 ⇒ 新行正常落盘，历史丢弃不回补。
	store.setFail(false)
	w.Enqueue(attemptRow(2))
	w.flush()
	require.Equal(t, int64(1), w.Inserted())
	require.Equal(t, int64(2), w.Dropped(), "丢弃是终态的（对账指标不回落）")
}

// TestAttemptLogWorkerFailureWarnsOncePerEpisode 失败告警边沿：连续失败只告警
// 一次（DB 长时间故障不刷屏），成功一次后复位，下次故障再告警一次。
func TestAttemptLogWorkerFailureWarnsOncePerEpisode(t *testing.T) {
	store := &captureAttemptLogInserter{fail: true}
	w := newTestAttemptLogWorker(t, store, 64)
	logger, out := newTestAttemptLogLogger(t)
	w.log = logger

	w.Enqueue(attemptRow(0))
	w.flush() // 第 1 次失败：告警
	w.Enqueue(attemptRow(1))
	w.flush() // 第 2 次失败：不告警（边沿已置位）

	require.NoError(t, logger.Sync())
	body, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(body), "attemptlog batch insert failed"),
		"连续失败只产生一次告警")

	// 成功一次 ⇒ 边沿复位 ⇒ 再失败要重新告警。
	store.setFail(false)
	w.Enqueue(attemptRow(2))
	w.flush()
	store.setFail(true)
	w.Enqueue(attemptRow(3))
	w.flush()

	require.NoError(t, logger.Sync())
	body, err = os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, 2, strings.Count(string(body), "attemptlog batch insert failed"),
		"成功之后的新一轮故障重新告警")
}

// slowAttemptLogInserter 慢落库（预算截断路径模拟——不做真实 sleep：用 ctx 分支）。
type slowAttemptLogInserter struct {
	inserted int
}

func (s *slowAttemptLogInserter) InsertBatch(ctx context.Context, rows []*domain.AttemptLog) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.inserted += len(rows)
	return nil
}

// TestAttemptLogWorkerCloseTruncatesOnBudget 停机预算已到期：Close 不排空、
// 立即截断退出，剩余行数并入丢弃计数（对账不低估），不阻塞停机。
func TestAttemptLogWorkerCloseTruncatesOnBudget(t *testing.T) {
	store := &slowAttemptLogInserter{}
	w := newTestAttemptLogWorker(t, store, 4096)
	for i := range 30 {
		w.Enqueue(attemptRow(i))
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	require.NoError(t, w.Close(ctx))
	require.Zero(t, store.inserted, "预算已到期：一行都不落")
	require.Equal(t, int64(30), w.Dropped(), "截断面 = 本批 + 队列剩余（全部计入丢弃）")
	require.Zero(t, w.Queued())
}

// TestAttemptLogWorkerCloseIdempotent Close 幂等且排空后新增投递计入丢弃。
func TestAttemptLogWorkerCloseIdempotent(t *testing.T) {
	w := newTestAttemptLogWorker(t, &captureAttemptLogInserter{}, 8)
	w.Enqueue(attemptRow(0))
	require.NoError(t, w.Close(context.Background()))
	require.NoError(t, w.Close(context.Background()), "重复 Close 不得报错/不得重复排空")
	require.Equal(t, int64(1), w.Inserted())

	w.Enqueue(attemptRow(1)) // Close 之后：丢弃计数，不 panic 不入队
	require.Equal(t, int64(1), w.Dropped())
	require.Zero(t, w.Queued())
}

// TestAttemptLogWorkerStartDrainsOnClose 走真 Start（ticker 拉长到小时）：
// Close 先等 loop 退出再排空——队列里的行不丢。
func TestAttemptLogWorkerStartDrainsOnClose(t *testing.T) {
	store := &captureAttemptLogInserter{}
	w := newTestAttemptLogWorker(t, store, 64)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))
	require.Error(t, w.Start(ctx), "重复 Start 必须报错（装配写错要立刻可见）")

	for i := range 5 {
		w.Enqueue(attemptRow(i))
	}
	cancel()
	require.NoError(t, w.Close(context.Background()))
	require.Equal(t, int64(5), w.Inserted(), "loop 退出后由 Close 排空")
	require.Zero(t, w.Dropped())
}

// newTestAttemptLogLogger 落盘到临时文件的 logger（告警边沿断言用）。
func newTestAttemptLogLogger(t *testing.T) (*logx.Logger, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "attemptlog-test-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	out := filepath.Join(dir, "out.json")
	logger, err := logx.New("warn", out)
	require.NoError(t, err)
	return logger, out
}
