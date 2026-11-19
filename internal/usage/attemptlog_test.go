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
	require.Equal(t, int64(sent-8), w.DroppedFull(), "队列满丢弃 = 到达 - 队列容量")
	require.Zero(t, w.DroppedFailed(), "队列满不是落库失败：两个原因不能混")
	require.Zero(t, w.DroppedClose(), "队列满不是停机截断")
	require.Equal(t, int64(sent-8), w.Dropped(), "Dropped = 各原因之和")
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
	require.Equal(t, int64(2), w.DroppedFailed(), "整批丢弃 = 批行数（落库失败）")
	require.Zero(t, w.DroppedFull(), "落库失败不是背压：两个原因不能混")
	require.Equal(t, int64(2), w.Dropped(), "Dropped = 各原因之和")
	require.Zero(t, w.Queued(), "不回灌（有界队列不是重试缓冲）")

	// DB 恢复 ⇒ 新行正常落盘，历史丢弃不回补。
	store.setFail(false)
	w.Enqueue(attemptRow(2))
	w.flush()
	require.Equal(t, int64(1), w.Inserted())
	require.Equal(t, int64(2), w.DroppedFailed(), "丢弃是终态的（对账指标不回落）")
	require.Equal(t, int64(2), w.Dropped())
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

// TestAttemptLogWorkerQueueFullWarnsOnEdge 队列满告警边沿（评审要求补的证据）：
// 桶满 ⇒ 丢弃累计越过阈值只告警**一次**（热路径不刷日志），队列被 flush 排空
// 后边沿复位，下一次堆满再告警一次。
//
// 为什么这条不能只靠「Dropped() 数字涨了」：计数涨了但告警从不触发，运维永远
// 不会知道明细在丢（这正是 review 指出的证据缺口）。阈值是包内 var（10_000），
// 这里临时压低到 3 并 t.Cleanup 还原——它只是采样口径，不是行为。
func TestAttemptLogWorkerQueueFullWarnsOnEdge(t *testing.T) {
	old := attemptLogDropWarnThreshold
	attemptLogDropWarnThreshold = 3
	t.Cleanup(func() { attemptLogDropWarnThreshold = old })

	w := newTestAttemptLogWorker(t, &captureAttemptLogInserter{}, 1)
	logger, out := newTestAttemptLogLogger(t)
	t.Cleanup(func() { _ = logger.Close() })
	w.log = logger

	fill := func() {
		for i := range 4 { // 1 行入队 + 3 行丢弃 ≥ 阈值
			w.Enqueue(attemptRow(i))
		}
	}
	countWarns := func() int {
		t.Helper()
		require.NoError(t, logger.Sync())
		body, err := os.ReadFile(out)
		require.NoError(t, err)
		return strings.Count(string(body), "attemptlog queue full, dropping")
	}

	fill()
	require.Equal(t, 1, countWarns(), "队列满的告警必须出现（否则明细在丢而无人知）")
	require.NoError(t, logger.Sync())
	body, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(body), `"dropped_full"`, "告警必须能看出是背压（不是 DB 故障）")
	require.Contains(t, string(body), `"dropped_total"`, "告警带总量便于对账")
	for range 2 { // 排空 1 行（batch 100）；再 flush 一次即触发边沿复位
		w.flush()
	}
	require.Zero(t, w.Queued())
	require.Equal(t, 1, countWarns(), "同一轮堆积只告警一次（不刷屏）")

	fill()
	require.Equal(t, 2, countWarns(), "排空后再次堆积必须重新告警（边沿复位）")
}

// TestAttemptLogWorkerDroppedSumsAllReasons 一条 worker 上同时造出三种丢弃，
// 断言 Dropped() = 各原因之和（对账口径：到达 - 落盘，既不低估也不互相吞掉）。
//
// 为什么要这条：拆开计数的唯一风险是「总和不再等于实际丢弃」（漏加一个桶、
// 某处改回混计），而按原因断言各自为政时看不出来。
func TestAttemptLogWorkerDroppedSumsAllReasons(t *testing.T) {
	store := &captureAttemptLogInserter{fail: true}
	w := newTestAttemptLogWorker(t, store, 1) // 队列容量 1：第 2 行起就是背压

	// 1 行入队 + 2 行丢弃（队列满）
	for i := range 3 {
		w.Enqueue(attemptRow(i))
	}
	w.flush()                // 入队那 1 行落库失败 ⇒ 整批丢弃
	w.Enqueue(attemptRow(3)) // 再塞 1 行进队列，供停机截断

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	require.NoError(t, w.Close(ctx))

	require.Equal(t, int64(2), w.DroppedFull(), "两次队列满")
	require.Equal(t, int64(1), w.DroppedFailed(), "一次落库失败（1 行）")
	require.Equal(t, int64(1), w.DroppedClose(), "停机截断 1 行")
	require.Zero(t, w.DroppedClosed())
	require.Equal(t, w.DroppedFull()+w.DroppedFailed()+w.DroppedClose()+w.DroppedClosed(), w.Dropped(),
		"Dropped() 必须等于各原因之和")
	require.Equal(t, int64(4), w.Dropped())
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
	require.Equal(t, int64(30), w.DroppedClose(), "截断面 = 本批 + 队列剩余（全部计入停机截断）")
	require.Zero(t, w.DroppedFull(), "截断不是背压（队列没满）")
	require.Zero(t, w.DroppedFailed(), "截断不是落库失败（一行都没写过）")
	require.Zero(t, w.Queued())
}

// TestAttemptLogWorkerCloseIdempotent Close 幂等且排空后新增投递计入丢弃。
func TestAttemptLogWorkerCloseIdempotent(t *testing.T) {
	w := newTestAttemptLogWorker(t, &captureAttemptLogInserter{}, 8)
	w.Enqueue(attemptRow(0))
	require.NoError(t, w.Close(context.Background()))
	require.NoError(t, w.Close(context.Background()), "重复 Close 不得报错/不得重复排空")
	require.Equal(t, int64(1), w.Inserted())
	require.Zero(t, w.DroppedClosed(), "正常停机不该出现「关后投递」")

	w.Enqueue(attemptRow(1)) // Close 之后：丢弃计数，不 panic 不入队
	require.Equal(t, int64(1), w.DroppedClosed(), "关闭后的投递是生命周期错误，单列")
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
	require.Zero(t, w.DroppedClosed(), "正常停机路径不得产生关后投递（生命周期错误的专属计数）")
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
