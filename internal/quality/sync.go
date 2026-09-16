// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

const (
	redisQualityPrefix = "c3api:routing:quality:"
	redisFlowPrefix    = "c3api:routing:flow:"
	redisTTL           = 10 * time.Minute

	redisMaxCells    = 10000
	redisMaxBytes    = 8 * 1024 * 1024
	redisMaxDuration = 200 * time.Millisecond

	pgMaxRows     = 20000
	pgMaxBytes    = 16 * 1024 * 1024
	pgMaxDuration = 2 * time.Second
)

type PGQualityWriter interface {
	UpsertQualityAndMarkDirty(ctx context.Context, row repository.RoutingQualityRow) error
	UpsertFlowSnapshot(ctx context.Context, instanceSrc string, terminalMinute time.Time, identityVersion int16, absoluteSequence int64, rows []repository.RoutingFlowRow) error
}

type SyncConfig struct {
	BatchSize   int
	InstanceSrc string
}

type SyncStats struct {
	PendingQuality     int    `json:"pending_quality"`
	PendingFlow        int    `json:"pending_flow"`
	PendingBytes       int64  `json:"pending_bytes"`
	LagMs              int64  `json:"lag_ms"`
	DroppedQuality     int64  `json:"dropped_quality"`
	DroppedFlow        int64  `json:"dropped_flow"`
	PoisonDropped      int64  `json:"poison_dropped"`
	BatchQuality       int64  `json:"batch_quality"`
	BatchFlow          int64  `json:"batch_flow"`
	FreshnessMs        int64  `json:"freshness_ms"`
	LastRedisMs        int64  `json:"last_redis_ms"`
	LastPGMs           int64  `json:"last_pg_ms"`
	LastRedisAttemptMs int64  `json:"last_redis_attempt_ms"`
	LastRedisError     string `json:"last_redis_error"`
	RedisErrors        int64  `json:"redis_errors"`
	ResidualQuality    int64  `json:"residual_quality"`
	ResidualFlow       int64  `json:"residual_flow"`
}

type qRow struct {
	minute int64
	key    Key
	qm     *QualityMinute
	seq    int64
}

type cellSnap struct {
	attempts    int64
	successes   int64
	err429      int64
	err4xx      int64
	err5xx      int64
	errNetwork  int64
	ttftCount   int64
	sumQ32      int64
	sumSqQ32    int64
	hist        [10]int64
	input       int64
	output      int64
	cacheRead   int64
	cacheCreate int64
	calls       int64
	images      int64
}

type cursorEntry struct {
	snap cellSnap
	gen  uint64
}

type SyncWorker struct {
	rec         *Recorder
	rdb         *redis.Client
	pg          PGQualityWriter
	instanceSrc string
	clock       func() time.Time
	log         *logx.Logger
	cfg         SyncConfig

	mu               sync.Mutex
	flushMu          sync.Mutex
	flushStateMu     sync.Mutex
	flushDone        chan struct{}
	seq              map[int64]int64
	redisSeq         map[int64]int64
	pgSeq            map[int64]int64
	stats            SyncStats
	lastRedis        time.Time
	lastRedisAttempt time.Time
	lastRedisError   string
	lastPG           time.Time
	poison           atomic.Int64
	batchQ           atomic.Int64
	batchF           atomic.Int64

	lastCell    map[Key]cursorEntry
	pgLastCell  map[Key]cursorEntry
	minuteAbs   map[int64]map[Key]*QualityMinute
	pgMinuteAbs map[int64]map[Key]*QualityMinute
	committed   map[int64]map[Key]*QualityMinute
	// flowBuf 是 Redis flow 发布的复用 wrapper 缓冲（worker 单线程 + flushMu
	// 保护；rows 数组 JSON 由 FlowOwner 按 (minute, version) 缓存提供）。
	flowBuf []byte

	started              atomic.Bool
	closeOnce            sync.Once
	lifecycleMu          sync.Mutex
	closed               bool
	baseCtx              context.Context
	cancel               context.CancelFunc
	loopDone             <-chan struct{} // 恒为 loopDoneCh 的只读别名（构造器与 Start 共同维持）
	loopDoneCh           <-chan struct{}
	inflightAbandonGrace time.Duration
	closing              atomic.Bool
	refillMu             sync.Mutex
	refillClosed         bool
	// onQualityPersisted 是质量 influx 的事件驱动编译触发（缺陷 B 修复）：
	// PG 落库边界成功持久 ≥1 新质量行后同步调用（装配期接
	// scheduler.RequestCompile——非阻塞 select/default，下游 200ms 去抖收敛）。
	// 空刷/失败/纯 flow 刷不调用。调用在 worker loop goroutine 上，无新
	// goroutine、无轮询、无请求路径成本；nil = 未装配（既有测试/降级）。
	onQualityPersisted func()
}

func GenerateInstanceSrc() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(b))
}

func NewSyncWorker(rec *Recorder, rdb *redis.Client, pg PGQualityWriter, cfg SyncConfig, log *logx.Logger, onPersisted func()) *SyncWorker {
	src := cfg.InstanceSrc
	if src == "" {
		src = GenerateInstanceSrc()
	}
	bs := cfg.BatchSize
	if bs <= 0 {
		bs = 500
	}
	cfg.BatchSize = bs
	cfg.InstanceSrc = src
	loopDone := make(chan struct{})
	close(loopDone)
	w := &SyncWorker{
		rec:                  rec,
		rdb:                  rdb,
		pg:                   pg,
		instanceSrc:          src,
		clock:                time.Now,
		log:                  log,
		cfg:                  cfg,
		onQualityPersisted:   onPersisted,
		seq:                  make(map[int64]int64),
		redisSeq:             make(map[int64]int64),
		pgSeq:                make(map[int64]int64),
		lastCell:             make(map[Key]cursorEntry),
		pgLastCell:           make(map[Key]cursorEntry),
		minuteAbs:            make(map[int64]map[Key]*QualityMinute),
		pgMinuteAbs:          make(map[int64]map[Key]*QualityMinute),
		committed:            make(map[int64]map[Key]*QualityMinute),
		loopDoneCh:           loopDone,
		inflightAbandonGrace: 500 * time.Millisecond,
		flushDone:            func() chan struct{} { ch := make(chan struct{}); close(ch); return ch }(),
	}
	w.loopDone = w.loopDoneCh
	return w
}

func (w *SyncWorker) Name() string { return "quality-sync" }

func (w *SyncWorker) SetClock(fn func() time.Time) {
	if fn != nil {
		w.clock = fn
	}
}

func (w *SyncWorker) Start(ctx context.Context) error {
	w.lifecycleMu.Lock()
	defer w.lifecycleMu.Unlock()
	if w.closed {
		return fmt.Errorf("quality-sync: closed")
	}
	if !w.started.CompareAndSwap(false, true) {
		return fmt.Errorf("quality-sync: already started")
	}
	derived, cancel := context.WithCancel(ctx)
	w.baseCtx = derived
	w.cancel = cancel
	loopDone := worker.GoLoop(derived, "quality-sync", w.log, w.loop)
	w.loopDoneCh = loopDone
	w.loopDone = loopDone
	return nil
}

func (w *SyncWorker) loop(ctx context.Context) {
	redisTicker := time.NewTicker(500 * time.Millisecond)
	defer redisTicker.Stop()
	pgTicker := time.NewTicker(5 * time.Second)
	defer pgTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-redisTicker.C:
			w.doRedis(ctx)
		case <-pgTicker.C:
			w.doPG(ctx)
		}
	}
}

// snapshotActiveCells 抓取 recorder 当前活跃 cell 列表（快照后即放 rec 锁；
// 调用方随后持 w.mu 操作各自 sink 的 cursor/分钟桶，per-sink ack 语义不变）。
func snapshotActiveCells(rec *Recorder) []*Cell {
	if rec == nil {
		return nil
	}
	rec.mu.Lock()
	cells := make([]*Cell, 0, len(rec.active))
	for _, c := range rec.active {
		cells = append(cells, c)
	}
	rec.mu.Unlock()
	return cells
}

func snapCell(c *Cell) cellSnap {
	cur := cellSnap{
		attempts: c.attempts.Load(), successes: c.successes.Load(),
		err429: c.errClasses[ErrClass429].Load(), err4xx: c.errClasses[ErrClass4xx].Load(),
		err5xx: c.errClasses[ErrClass5xx].Load(), errNetwork: c.errClasses[ErrClassNetwork].Load(),
		ttftCount: c.ttftCount.Load(), sumQ32: c.sumQ32.Load(), sumSqQ32: c.sumSq.Load(),
		input: c.inputTokens.Load(), output: c.outputTokens.Load(), cacheRead: c.cacheRead.Load(), cacheCreate: c.cacheCreate.Load(),
		calls: c.calls.Load(), images: c.images.Load(),
	}
	for i := range cur.hist {
		cur.hist[i] = c.hist[i].Load()
	}
	return cur
}

// diffCellSnap 计算相对 prev 的增量；无 prev 或 gen 变化（retire/rebuild）时全量即增量。
func diffCellSnap(cur cellSnap, prev cursorEntry, ok bool, gen uint64) cellSnap {
	if !ok || prev.gen != gen {
		return cur
	}
	var delta cellSnap
	delta.attempts = cur.attempts - prev.snap.attempts
	delta.successes = cur.successes - prev.snap.successes
	delta.err429 = cur.err429 - prev.snap.err429
	delta.err4xx = cur.err4xx - prev.snap.err4xx
	delta.err5xx = cur.err5xx - prev.snap.err5xx
	delta.errNetwork = cur.errNetwork - prev.snap.errNetwork
	delta.ttftCount = cur.ttftCount - prev.snap.ttftCount
	delta.sumQ32 = cur.sumQ32 - prev.snap.sumQ32
	delta.sumSqQ32 = cur.sumSqQ32 - prev.snap.sumSqQ32
	for i := range delta.hist {
		delta.hist[i] = cur.hist[i] - prev.snap.hist[i]
	}
	delta.input = cur.input - prev.snap.input
	delta.output = cur.output - prev.snap.output
	delta.cacheRead = cur.cacheRead - prev.snap.cacheRead
	delta.cacheCreate = cur.cacheCreate - prev.snap.cacheCreate
	delta.calls = cur.calls - prev.snap.calls
	delta.images = cur.images - prev.snap.images
	return delta
}

func (d cellSnap) isZero() bool {
	return d.attempts == 0 && d.successes == 0 && d.ttftCount == 0 && d.input == 0 && d.output == 0 && d.calls == 0 && d.images == 0 && d.err429 == 0 && d.err4xx == 0 && d.err5xx == 0 && d.errNetwork == 0
}

// addTo 把增量累加进已存在的分钟行（merge 语义与原内联块一致）。
func (d cellSnap) addTo(qm *QualityMinute) {
	qm.attempts += d.attempts
	qm.successes += d.successes
	qm.err429 += d.err429
	qm.err4xx += d.err4xx
	qm.err5xx += d.err5xx
	qm.errNetwork += d.errNetwork
	qm.ttftCount += d.ttftCount
	qm.sumQ32 += d.sumQ32
	qm.sumSqQ32 += d.sumSqQ32
	for i := range qm.hist {
		qm.hist[i] += d.hist[i]
	}
	qm.inputTokens += d.input
	qm.outputTokens += d.output
	qm.cacheRead += d.cacheRead
	qm.cacheCreate += d.cacheCreate
	qm.calls += d.calls
	qm.images += d.images
}

func newQualityMinuteFromDelta(minute int64, key Key, gen uint64, d cellSnap) *QualityMinute {
	qm := NewQualityMinute(minute, key)
	qm.gen = gen
	qm.attempts = d.attempts
	qm.successes = d.successes
	qm.err429 = d.err429
	qm.err4xx = d.err4xx
	qm.err5xx = d.err5xx
	qm.errNetwork = d.errNetwork
	qm.ttftCount = d.ttftCount
	qm.sumQ32 = d.sumQ32
	qm.sumSqQ32 = d.sumSqQ32
	qm.hist = d.hist
	qm.inputTokens = d.input
	qm.outputTokens = d.output
	qm.cacheRead = d.cacheRead
	qm.cacheCreate = d.cacheCreate
	qm.calls = d.calls
	qm.images = d.images
	return qm
}

// accumulateDelta 把单 cell 增量并入分钟桶（新建桶/新建行/累加旧行三分支与原内联一致）。
func accumulateDelta(dst map[int64]map[Key]*QualityMinute, minute int64, key Key, gen uint64, d cellSnap) {
	m, ok := dst[minute]
	if !ok {
		m = make(map[Key]*QualityMinute)
		dst[minute] = m
	}
	if existing, ok := m[key]; ok {
		d.addTo(existing)
	} else {
		m[key] = newQualityMinuteFromDelta(minute, key, gen, d)
	}
}

func cloneKeyMap(src map[Key]*QualityMinute) map[Key]*QualityMinute {
	cp := make(map[Key]*QualityMinute, len(src))
	for k, v := range src {
		cp[k] = v.Clone()
	}
	return cp
}

func cloneMinuteMap(src map[int64]map[Key]*QualityMinute) map[int64]map[Key]*QualityMinute {
	out := make(map[int64]map[Key]*QualityMinute, len(src))
	for minute, rows := range src {
		out[minute] = cloneKeyMap(rows)
	}
	return out
}

func cloneMinuteMapUpto(src map[int64]map[Key]*QualityMinute, upto int64) map[int64]map[Key]*QualityMinute {
	out := make(map[int64]map[Key]*QualityMinute, len(src))
	for minute, rows := range src {
		if minute > upto {
			continue
		}
		out[minute] = cloneKeyMap(rows)
	}
	return out
}

// foldMinuteMap 把 src 各分钟行并入 dst（同 key merge，不同 key 深拷贝；
// 与原 doRedis/doPG 内联合并块逐字同义）。
func foldMinuteMap(dst, src map[int64]map[Key]*QualityMinute) {
	for minute, rows := range src {
		if _, ok := dst[minute]; !ok {
			dst[minute] = make(map[Key]*QualityMinute)
		}
		for k, v := range rows {
			if existing, ok := dst[minute][k]; ok {
				existing.merge(v)
			} else {
				dst[minute][k] = v.Clone()
			}
		}
	}
}

func (w *SyncWorker) collectRedisDeltaLocked(curMinuteUnix int64) map[int64]map[Key]*QualityMinute {
	if w.rec == nil {
		return nil
	}
	cells := snapshotActiveCells(w.rec)
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range cells {
		cur := snapCell(c)
		prev, ok := w.lastCell[c.key]
		delta := diffCellSnap(cur, prev, ok, c.gen)
		w.lastCell[c.key] = cursorEntry{snap: cur, gen: c.gen}
		if delta.isZero() {
			continue
		}
		accumulateDelta(w.minuteAbs, curMinuteUnix, c.key, c.gen, delta)
	}
	return cloneMinuteMapUpto(w.minuteAbs, curMinuteUnix)
}

func (w *SyncWorker) collectPGDeltaLocked() map[int64]map[Key]*QualityMinute {
	if w.rec == nil {
		return nil
	}
	cells := snapshotActiveCells(w.rec)
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range cells {
		cur := snapCell(c)
		prev, ok := w.pgLastCell[c.key]
		delta := diffCellSnap(cur, prev, ok, c.gen)
		w.pgLastCell[c.key] = cursorEntry{snap: cur, gen: c.gen}
		if delta.isZero() {
			continue
		}
		accumulateDelta(w.pgMinuteAbs, w.clock().UTC().Truncate(time.Minute).Unix(), c.key, c.gen, delta)
	}
	return cloneMinuteMap(w.pgMinuteAbs)
}

func (w *SyncWorker) doRedis(ctx context.Context) {
	if !w.flushMu.TryLock() {
		return
	}
	if !w.beginFlush() {
		return
	}
	defer w.endFlush()
	w.doRedisLocked(ctx)
}

// doRedisLocked 是 Redis 面单次 flush；调用方必须持有 flushMu（refill/回滚
// 全程在锁内，Close 的生命周期屏障依赖这一不变量）。
func (w *SyncWorker) doRedisLocked(ctx context.Context) {
	start := w.clock()
	w.mu.Lock()
	w.lastRedisAttempt = start
	w.stats.LastRedisAttemptMs = start.UnixMilli()
	w.mu.Unlock()

	if w.rec == nil {
		return
	}
	// collect redis delta (non-destructive ack after success)
	curMinute := start.UTC().Truncate(time.Minute).Unix()
	// We need to collect delta without yet acking; our collect methods already updated minuteAbs/lastCell.
	// To make it per-sink ack, we should not have updated lastCell before success. So we need to snapshot first, then ack after success.
	// For backward compat, our collect already mutated. To fix per-sink, we keep copies of lastCell before mutation and revert on failure.
	// Simplify: do snapshot collection into temp, then on success commit.
	// For now we implement by saving copies
	w.mu.Lock()
	savedLastCell := maps.Clone(w.lastCell)
	savedMinuteAbs := cloneMinuteMap(w.minuteAbs)
	w.mu.Unlock()

	activeAll := w.collectRedisDeltaLocked(curMinute)
	// also include pendingQuality for all due minutes
	w.rec.mu.Lock()
	pendingByMinute := make(map[int64]map[Key]*QualityMinute)
	for minute, rows := range w.rec.pendingQuality {
		if minute > curMinute {
			continue
		}
		pendingByMinute[minute] = cloneKeyMap(rows)
	}
	w.rec.mu.Unlock()
	// Flow comes through the FlowOwner snapshot handoff one minute at a time:
	// each payload is deep-owned, published synchronously, and released
	// before the next snapshot, so at most one O(R) flow payload is live
	// under flushMu. Read-only: no lease, no ack, owner retention untouched.
	flowIDs := w.rec.flow.redisCandidateMinutes(curMinute)

	// merge activeAll + pending for each minute
	mergedByMinute := make(map[int64]map[Key]*QualityMinute)
	for minute, rows := range pendingByMinute {
		mergedByMinute[minute] = rows
	}
	foldMinuteMap(mergedByMinute, activeAll)
	if len(mergedByMinute) == 0 && len(flowIDs) == 0 {
		// empty pass must not refresh freshness
		return
	}
	if w.rdb == nil {
		w.mu.Lock()
		// no client: do not refresh freshness, record error
		w.lastRedisError = "no redis client"
		w.stats.LastRedisError = w.lastRedisError
		w.stats.RedisErrors++
		// revert delta ack
		w.lastCell = savedLastCell
		w.minuteAbs = savedMinuteAbs
		w.mu.Unlock()
		return
	}
	// budget enforcement per minute? Apply global limits across all minutes
	type cell struct {
		minute int64
		key    Key
		qm     *QualityMinute
	}
	var cells []cell
	for minute, rows := range mergedByMinute {
		for k, qm := range rows {
			cells = append(cells, cell{minute: minute, key: k, qm: qm})
		}
	}
	if len(cells) > redisMaxCells {
		cells = cells[:redisMaxCells]
		// revert unsent deltas? Keep them for next tick: do not ack those minutes fully
		// For simplicity, do not ack at all on budget truncation; keep all for next
		w.mu.Lock()
		w.lastCell = savedLastCell
		w.minuteAbs = savedMinuteAbs
		w.lastRedisError = "redis budget truncated"
		w.stats.LastRedisError = w.lastRedisError
		w.stats.RedisErrors++
		w.mu.Unlock()
		return
	}
	bytesEst := len(cells) * EstimatedQualityRowBytes
	if bytesEst > redisMaxBytes || w.clock().Sub(start) > redisMaxDuration {
		w.mu.Lock()
		w.lastCell = savedLastCell
		w.minuteAbs = savedMinuteAbs
		w.lastRedisError = "redis budget exceeded"
		w.stats.LastRedisError = w.lastRedisError
		w.stats.RedisErrors++
		w.mu.Unlock()
		return
	}
	// 单缓冲编码：所有 cell 的 field/value 追加进同一 []byte，HSet 以子切片
	// 引用（只读使用；Exec 前不写缓冲 → 引用有效）。旧路径每 cell 一次
	// map+json.Marshal+4×hex.EncodeToString，是本进程第一大分配源。
	type cellOut struct {
		minute int64
		field  []byte
		val    []byte
	}
	outs := make([]cellOut, 0, len(cells))
	var buf []byte
	buf = make([]byte, 0, len(cells)*EstimatedQualityRowBytes)
	for _, c := range cells {
		fieldStart := len(buf)
		buf = appendQualityField(buf, c.key)
		field := buf[fieldStart:len(buf)]
		w.mu.Lock()
		seq := w.redisSeq[c.minute] + 1
		w.redisSeq[c.minute] = seq
		w.mu.Unlock()
		valStart := len(buf)
		buf = appendQualityCellJSON(buf, w.instanceSrc, c.minute, seq, c.key, c.qm)
		outs = append(outs, cellOut{minute: c.minute, field: field, val: buf[valStart:len(buf)]})
	}
	pipe := w.rdb.Pipeline()
	keyOfMinute := make(map[int64]string, 4)
	for _, o := range outs {
		qKey, ok := keyOfMinute[o.minute]
		if !ok {
			qKey = fmt.Sprintf("%s%d:%s", redisQualityPrefix, o.minute, w.instanceSrc)
			keyOfMinute[o.minute] = qKey
			// Expire 每 (minute, instance) 键每轮一次：旧实现每 cell 一发。
			pipe.Expire(ctx, qKey, redisTTL)
		}
		pipe.HSet(ctx, qKey, o.field, o.val)
	}
	if len(cells) > 0 {
		if _, err := pipe.Exec(ctx); err != nil {
			w.mu.Lock()
			w.lastRedisError = err.Error()
			w.stats.LastRedisError = w.lastRedisError
			w.stats.RedisErrors++
			// revert per-sink ack
			w.lastCell = savedLastCell
			w.minuteAbs = savedMinuteAbs
			if w.log != nil {
				w.log.Warn("quality redis publish failed", logx.Error(err))
			}
			w.mu.Unlock()
			return
		}
	}
	// Flow publishes one minute at a time. rows 数组 JSON 由 FlowOwner 按
	// (minute, shell version) 缓存（redisPayload）——同一版本的每 tick 序号心跳
	// 重发布只重拼小 wrapper；wrapper 手写进复用缓冲（旧路径此处
	// map[string]any+json.Marshal 是本方法最大分配源）。只读：无 lease、无 ack。
	for _, minute := range flowIDs {
		blob, state, ok := w.rec.flow.redisPayload(minute)
		if !ok || state == flowPayloadNone {
			// No rows and no empty marker (e.g. net-zero folds): nothing
			// to publish. The legacy zero edges/counts payload had no
			// consumer (no reader of the flow namespace exists) and is
			// skipped instead of published.
			continue
		}
		w.mu.Lock()
		seq := w.redisSeq[minute] + 1
		w.redisSeq[minute] = seq
		w.mu.Unlock()
		fKey := fmt.Sprintf("%s%d:%s", redisFlowPrefix, minute, w.instanceSrc)
		buf := w.flowBuf[:0]
		buf = append(buf, `{"instance_src":`...)
		buf = strconv.AppendQuote(buf, w.instanceSrc)
		buf = append(buf, `,"terminal_minute":`...)
		buf = strconv.AppendInt(buf, minute, 10)
		buf = append(buf, `,"absolute_sequence":`...)
		buf = strconv.AppendInt(buf, seq, 10)
		if state == flowPayloadEmpty {
			buf = append(buf, `,"empty":true}`...)
		} else {
			buf = append(buf, `,"rows":`...)
			buf = append(buf, blob...)
			buf = append(buf, '}')
		}
		w.flowBuf = buf
		if err := w.rdb.Set(ctx, fKey, buf, redisTTL).Err(); err != nil {
			w.mu.Lock()
			w.lastRedisError = err.Error()
			w.stats.LastRedisError = w.lastRedisError
			w.stats.RedisErrors++
			w.mu.Unlock()
			if w.log != nil {
				w.log.Warn("quality redis flow publish failed", logx.Error(err))
			}
			return
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// success: ack/remove exactly published minuteAbs, retain newer contributions; historical drain once
	for minute := range mergedByMinute {
		delete(w.minuteAbs, minute)
	}
	w.lastRedis = w.clock()
	w.stats.LastRedisMs = w.clock().Sub(start).Milliseconds()
	w.stats.FreshnessMs = 0
	w.stats.LastRedisError = ""
	w.lastRedisError = ""
	w.pruneLongLivedLocked(w.clock().Unix())
}

func (w *SyncWorker) doPG(ctx context.Context) {
	if !w.flushMu.TryLock() {
		return
	}
	if !w.beginFlush() {
		return
	}
	defer w.endFlush()
	w.doPGLocked(ctx)
}

func (w *SyncWorker) beginFlush() bool {
	w.flushStateMu.Lock()
	defer w.flushStateMu.Unlock()
	if w.closing.Load() {
		w.flushMu.Unlock()
		return false
	}
	w.flushDone = make(chan struct{})
	return true
}

func (w *SyncWorker) endFlush() {
	w.flushStateMu.Lock()
	close(w.flushDone)
	w.flushStateMu.Unlock()
	w.flushMu.Unlock()
}

// doPGLocked 是 PG 面单次 flush；调用方必须持有 flushMu。flow 面是版本化
// snapshot/ack：失败无 ack、无 refill，owner 状态保持 dirty 下周期重试。
func (w *SyncWorker) doPGLocked(ctx context.Context) {
	start := w.clock()
	if w.rec == nil || w.pg == nil {
		return
	}
	// collect PG active delta first, even if pending empty (active-only workload)
	pgActive := w.collectPGDeltaLocked()
	// Flow arrives via the owner's versioned snapshot/ack handoff: sync
	// snapshots one dirty minute at a time, writes the full cumulative
	// snapshot with replacement UpsertFlowSnapshot, and settles through the
	// lease token. Sync never owns flow rows and never mutates owner maps.
	flowIDs := w.rec.flow.pgCandidateMinutes()
	hasPendingQ := false
	w.rec.mu.Lock()
	hasPendingQ = len(w.rec.pendingQuality) > 0
	w.rec.mu.Unlock()
	if !hasPendingQ && len(flowIDs) == 0 && len(pgActive) == 0 {
		return
	}
	w.rec.mu.Lock()
	pendQ := w.rec.pendingQuality
	w.rec.pendingQuality = make(map[int64]map[Key]*QualityMinute)
	w.rec.pendingBytes.Store(0)
	w.rec.mu.Unlock()

	// merge pgActive into pendQ
	foldMinuteMap(pendQ, pgActive)
	// pgActive already contains all due pgMinuteAbs, clear after draining into pendQ
	w.mu.Lock()
	w.pgMinuteAbs = make(map[int64]map[Key]*QualityMinute)
	w.mu.Unlock()

	var allRows []qRow
	for minute, rows := range pendQ {
		w.mu.Lock()
		seq := w.pgSeq[minute] + 1
		w.pgSeq[minute] = seq
		// also sync global seq for backward compat
		w.seq[minute] = seq
		w.mu.Unlock()
		for k, qm := range rows {
			w.mu.Lock()
			committedForMinute := w.committed[minute]
			w.mu.Unlock()
			merged := qm.Clone()
			if committedForMinute != nil {
				if prev, ok := committedForMinute[k]; ok {
					tmp := prev.Clone()
					tmp.merge(merged)
					merged = tmp
				}
			}
			allRows = append(allRows, qRow{minute: minute, key: k, qm: merged, seq: seq})
		}
	}
	var toFlush []qRow
	var deferred []qRow
	bytes := 0
	for i, r := range allRows {
		if len(toFlush) >= pgMaxRows || bytes+EstimatedQualityRowBytes > pgMaxBytes || w.clock().Sub(start) > pgMaxDuration {
			deferred = append(deferred, allRows[i:]...)
			break
		}
		toFlush = append(toFlush, r)
		bytes += EstimatedQualityRowBytes
	}
	remainingQ := w.flushQualityBatch(ctx, toFlush, start)
	if len(deferred) > 0 {
		w.refillQualityDelta(deferred, pendQ)
	}
	if len(remainingQ) > 0 {
		w.refillQualityDelta(remainingQ, pendQ)
	}
	// Flow flushes one minute at a time through the versioned snapshot/ack
	// handoff. An empty-only dirty minute still calls UpsertFlowSnapshot with
	// a zero-row set so durable sequence advances; the call is never skipped.
	// On failure, deferral, expiry, or unwind the lease is released exactly
	// once with no ack and no refill: the owner state stays dirty and the
	// next cycle retries. Each candidate is attempted at most once per cycle.
	// The payload is consumed synchronously in its single repo call and all
	// references are dropped before the next snapshot.
	var batchFlow int64
	for _, minute := range flowIDs {
		if w.clock().Sub(start) > pgMaxDuration {
			break
		}
		snap, tok, ok := w.rec.flow.snapshotForPG(minute)
		if !ok {
			continue
		}
		settled := false
		func() {
			defer func() {
				if !settled {
					w.rec.flow.releasePG(tok)
				}
			}()
			w.mu.Lock()
			seq := w.pgSeq[minute] + 1
			w.pgSeq[minute] = seq
			// also sync global seq for backward compat
			w.seq[minute] = seq
			w.mu.Unlock()
			rows := flowRowsFromMinute(snap, w.instanceSrc, seq)
			if err := w.pg.UpsertFlowSnapshot(ctx, w.instanceSrc, time.Unix(minute, 0).UTC(), 1, seq, rows); err != nil {
				rows = nil
				w.rec.flow.releasePG(tok)
				settled = true
				return
			}
			rows = nil
			w.rec.flow.ackPG(tok)
			settled = true
			batchFlow++
		}()
		snap = nil
	}
	// on failure, pg delta should be retained for next attempt; we cleared pgMinuteAbs earlier, so need to restore unflushed?
	// Our pgActive deltas that were merged into pendQ and then deferred/failed are now in pendingQuality via refill, so they will be retried.
	// For successful minutes, committed already updated.
	w.mu.Lock()
	w.lastPG = w.clock()
	w.stats.LastPGMs = w.clock().Sub(start).Milliseconds()
	w.stats.BatchQuality = int64(len(toFlush) - len(remainingQ))
	w.stats.BatchFlow = batchFlow
	oldest := w.oldestPendingMinute(pendQ, flowIDs)
	if oldest != 0 {
		w.stats.LagMs = w.clock().Sub(time.Unix(oldest, 0)).Milliseconds()
		if w.stats.LagMs < 0 {
			w.stats.LagMs = 0
		}
	}
	w.stats.PendingQuality = w.rec.MinuteBucketCount()
	w.stats.PendingBytes = w.rec.PendingBytes()
	w.pruneLongLivedLocked(w.clock().Unix())
	// 缺陷 B 触发：本轮实际持久的新质量行数（预算截断的 deferred 已 refill，
	// 未计入；失败行同样未计入）。>0 = 真实质量 influx → 编译通知；空刷/
	// 纯 flow 刷静默。回调在锁外调用（RequestCompile 非阻塞，但不持 worker
	// 锁进下游是纪律）。
	persistedQuality := int64(len(toFlush) - len(remainingQ))
	notify := w.onQualityPersisted
	w.mu.Unlock()
	if persistedQuality > 0 && notify != nil {
		notify()
	}
	// ack PG minuteAbs on success is already cleared; on failure refill will preserve
}

func (w *SyncWorker) refillQualityDelta(rows []qRow, originalPend map[int64]map[Key]*QualityMinute) {
	w.refillMu.Lock()
	defer w.refillMu.Unlock()
	if w.refillClosed {
		w.mu.Lock()
		w.stats.ResidualQuality += int64(len(rows))
		w.mu.Unlock()
		return
	}
	for _, r := range rows {
		delta := r.qm
		if origRows, ok := originalPend[r.minute]; ok {
			if orig, ok2 := origRows[r.key]; ok2 {
				delta = orig
			} else {
				w.mu.Lock()
				committed := w.committed[r.minute]
				w.mu.Unlock()
				if committed != nil {
					if prev, ok := committed[r.key]; ok {
						tmp := r.qm.Clone()
						tmp.attempts -= prev.attempts
						tmp.successes -= prev.successes
						tmp.err429 -= prev.err429
						tmp.err4xx -= prev.err4xx
						tmp.err5xx -= prev.err5xx
						tmp.errNetwork -= prev.errNetwork
						tmp.ttftCount -= prev.ttftCount
						tmp.sumQ32 -= prev.sumQ32
						tmp.sumSqQ32 -= prev.sumSqQ32
						for i := range tmp.hist {
							tmp.hist[i] -= prev.hist[i]
						}
						tmp.inputTokens -= prev.inputTokens
						tmp.outputTokens -= prev.outputTokens
						tmp.cacheRead -= prev.cacheRead
						tmp.cacheCreate -= prev.cacheCreate
						tmp.calls -= prev.calls
						tmp.images -= prev.images
						delta = tmp
					}
				}
			}
		}
		if err := w.rec.EnqueueQualityMinute(delta); err != nil {
			// capacity drop already accounted via overflow counters, record explicitly
			w.mu.Lock()
			w.stats.DroppedQuality++
			w.mu.Unlock()
			if w.log != nil {
				w.log.Warn("quality refill dropped due to capacity", logx.Error(err))
			}
		}
	}
}

func (w *SyncWorker) flushQualityBatch(ctx context.Context, rows []qRow, start time.Time) []qRow {
	if len(rows) == 0 {
		return nil
	}
	shards := make(map[int64][]qRow)
	for _, r := range rows {
		shards[r.minute] = append(shards[r.minute], r)
	}
	var failed []qRow
	for _, shard := range shards {
		for i := 0; i < len(shard); i += w.cfg.BatchSize {
			if w.clock().Sub(start) > pgMaxDuration {
				failed = append(failed, shard[i:]...)
				break
			}
			end := i + w.cfg.BatchSize
			if end > len(shard) {
				end = len(shard)
			}
			chunk := shard[i:end]
			unpersisted, err := w.insertQualityChunk(ctx, chunk)
			if err != nil {
				w.markQualityCommitted(chunk[:len(chunk)-len(unpersisted)])
				if len(unpersisted) == 1 {
					// singleton first failure must refill; only typed row error is immediate poison
					if isRowDataError(err) {
						w.poison.Add(1)
						w.mu.Lock()
						w.stats.PoisonDropped++
						w.mu.Unlock()
						continue
					}
					failed = append(failed, unpersisted...)
					continue
				}
				poison, refill := w.bisectQuality(ctx, unpersisted)
				if poison != nil {
					w.poison.Add(1)
					w.mu.Lock()
					w.stats.PoisonDropped++
					w.mu.Unlock()
				}
				if len(refill) > 0 {
					failed = append(failed, refill...)
				}
				failed = append(failed, shard[end:]...)
				break
			} else {
				w.markQualityCommitted(chunk)
			}
		}
	}
	return failed
}

type RowDataError struct {
	Msg   string
	Cause error
}

func (e *RowDataError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return "row data error"
}

func (e *RowDataError) Unwrap() error { return e.Cause }

func isRowDataError(err error) bool {
	if err == nil {
		return false
	}
	var rde *RowDataError
	if errors.As(err, &rde) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if len(pgErr.Code) >= 2 {
			prefix := pgErr.Code[:2]
			if prefix == "22" || prefix == "23" {
				return true
			}
		}
	}
	return false
}

func (w *SyncWorker) insertQualityChunk(ctx context.Context, chunk []qRow) ([]qRow, error) {
	for i, r := range chunk {
		row := repository.RoutingQualityRow{
			IdentityVersion:      r.key.IdentityVersion,
			RouteClassID:         domain.RouteClassIDVal(r.key.RouteClassID),
			QualityClassID:       domain.QualityClassIDVal(r.key.QualityClassID),
			CandidateFingerprint: domain.CandidateFingerprintVal(r.key.Fingerprint),
			InstanceSrc:          w.instanceSrc,
			BucketMinute:         time.Unix(r.minute, 0).UTC(),
			AbsoluteSequence:     r.seq,
			Attempts:             r.qm.attempts,
			Successes:            r.qm.successes,
			Count429:             r.qm.err429,
			CountOrdinary4xx:     r.qm.err4xx,
			Count5xx:             r.qm.err5xx,
			CountNetwork:         r.qm.errNetwork,
			TTFTN:                r.qm.ttftCount,
			TTFTSumLogQ32:        r.qm.sumQ32,
			TTFTSumSqLogQ32:      r.qm.sumSqQ32,
			TTFTHist:             r.qm.hist[:],
			InputTokens:          r.qm.inputTokens,
			OutputTokens:         r.qm.outputTokens,
			CacheReadTokens:      r.qm.cacheRead,
			CacheCreateTokens:    r.qm.cacheCreate,
			Calls:                r.qm.calls,
			Images:               r.qm.images,
		}
		if err := w.pg.UpsertQualityAndMarkDirty(ctx, row); err != nil {
			if isRowDataError(err) {
				var rde *RowDataError
				if !errors.As(err, &rde) {
					return chunk[i:], &RowDataError{Msg: err.Error(), Cause: err}
				}
			}
			return chunk[i:], err
		}
	}
	return nil, nil
}

func (w *SyncWorker) markQualityCommitted(rows []qRow) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, r := range rows {
		if _, ok := w.committed[r.minute]; !ok {
			w.committed[r.minute] = make(map[Key]*QualityMinute)
		}
		// Live-seam feedback: the increment newly persisted for this
		// (minute, key) absolute (merged minus the previous absolute, clamped)
		// is exactly what UnflushedMinutes must henceforth exclude.
		inc := r.qm.Clone()
		if prev, ok := w.committed[r.minute][r.key]; ok && prev != nil {
			inc.subClamped(prev)
		}
		w.committed[r.minute][r.key] = r.qm.Clone()
		if w.rec != nil {
			w.rec.MarkEmitted(r.key, inc)
		}
	}
}

func (w *SyncWorker) bisectQuality(ctx context.Context, chunk []qRow) (poison *qRow, refill []qRow) {
	if len(chunk) == 1 {
		unpersisted, err := w.insertQualityChunk(ctx, chunk)
		w.markQualityCommitted(chunk[:len(chunk)-len(unpersisted)])
		if err != nil {
			if isRowDataError(err) {
				return &chunk[0], nil
			}
			return nil, chunk
		}
		return nil, nil
	}
	mid := len(chunk) / 2
	left, right := chunk[:mid], chunk[mid:]
	leftUnpersisted, leftErr := w.insertQualityChunk(ctx, left)
	w.markQualityCommitted(left[:len(left)-len(leftUnpersisted)])
	if leftErr == nil {
		p, rf := w.bisectQuality(ctx, right)
		return p, rf
	}
	rightUnpersisted, rightErr := w.insertQualityChunk(ctx, right)
	w.markQualityCommitted(right[:len(right)-len(rightUnpersisted)])
	if rightErr == nil {
		p, rf := w.bisectQuality(ctx, leftUnpersisted)
		return p, rf
	}
	return nil, append(leftUnpersisted, rightUnpersisted...)
}

func flowRowsFromMinute(fm *FlowMinute, instanceSrc string, seq int64) []repository.RoutingFlowRow {
	if fm == nil {
		return nil
	}
	if fm.IsEmptySnapshot() {
		return nil
	}
	if fm.HasFlowRows() {
		// 独占产物（snapshotForPG）→ 免去 FlowRows 的二次深拷；下面的 flush
		// 戳原地改写在该产物被同步消费后立即丢弃的前提下安全。
		rows := fm.FlowRows()
		for i := range rows {
			rows[i].InstanceSrc = instanceSrc
			rows[i].AbsoluteSequence = seq
			rows[i].TerminalMinute = time.Unix(fm.Minute(), 0).UTC()
		}
		return rows
	}
	// No rows and no empty marker: nothing to persist. (The legacy
	// edges-array fallback is deleted with the consumer seam — shell
	// edges/counts have no writer, so this point always carried zeros and
	// returned nil.)
	return nil
}

func (w *SyncWorker) pruneLongLivedLocked(nowUnix int64) {
	pending := make(map[int64]struct{})
	w.rec.mu.Lock()
	for m := range w.rec.pendingQuality {
		pending[m] = struct{}{}
	}
	w.rec.mu.Unlock()
	// Owner lane via handoff API: only dirty-or-leased (unsettled) minutes pin
	// sequences. Clean retained reconstruction state needs no future snapshot
	// and pins nothing.
	w.rec.flow.collectUnsettledMinutes(pending)
	for m := range w.minuteAbs {
		pending[m] = struct{}{}
	}
	for m := range w.pgMinuteAbs {
		pending[m] = struct{}{}
	}
	cutoff := nowUnix - 600
	for m := range w.seq {
		if m < cutoff {
			if _, ok := pending[m]; !ok {
				delete(w.seq, m)
			}
		}
	}
	for m := range w.redisSeq {
		if m < cutoff {
			if _, ok := pending[m]; !ok {
				delete(w.redisSeq, m)
			}
		}
	}
	// pgSeq is deliberately never pruned: the repository guards replacement
	// with greater-sequence fencing, so a restarted sequence would be ignored
	// while ack still clears dirty — silently losing the contribution until
	// some later merge. Retaining one small entry per minute ever snapshotted
	// preserves fencing across prune; growth is tens of bytes per active
	// minute, negligible beside the retained owner state itself.
	for m := range w.committed {
		if m < cutoff {
			if _, ok := pending[m]; !ok {
				delete(w.committed, m)
			}
		}
	}
}

func (w *SyncWorker) oldestPendingMinute(q map[int64]map[Key]*QualityMinute, flowIDs []int64) int64 {
	var oldest int64
	first := true
	for m := range q {
		if first || m < oldest {
			oldest = m
			first = false
		}
	}
	for _, m := range flowIDs {
		if first || m < oldest {
			oldest = m
			first = false
		}
	}
	return oldest
}

// Stats 满足 handler.StatsProvider 契约（any 直出 JSON，ops 端点零转换）。
func (w *SyncWorker) Stats() any { return w.statsSnapshot() }

func (w *SyncWorker) statsSnapshot() SyncStats {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.stats
	if w.rec != nil {
		w.rec.mu.Lock()
		pendingQ := 0
		for _, rows := range w.rec.pendingQuality {
			pendingQ += len(rows)
		}
		w.rec.mu.Unlock()
		// Flow visibility goes through the owner stats API; PendingFlow is the
		// bounded pgWorkTotal (queued pre-seal work plus dirty unleased
		// minutes plus active leases), so clean retained reconstruction
		// state neither blocks Close nor inflates pending work.
		s.PendingQuality = pendingQ
		s.PendingFlow = w.rec.flow.pgWorkTotal()
		s.PendingBytes = w.rec.PendingBytes()
		s.DroppedQuality = w.rec.QualityOverflow()
		s.DroppedFlow = w.rec.FlowOverflow()
	}
	s.PoisonDropped = w.poison.Load()
	if !w.lastRedis.IsZero() {
		s.FreshnessMs = w.clock().Sub(w.lastRedis).Milliseconds()
	}
	s.LastRedisAttemptMs = w.lastRedisAttempt.UnixMilli()
	s.LastRedisError = w.lastRedisError
	return s
}

func (w *SyncWorker) Close(ctx context.Context) error {
	w.lifecycleMu.Lock()
	if w.closed {
		w.lifecycleMu.Unlock()
		return nil
	}
	w.closed = true
	w.closing.Store(true)
	cancel := w.cancel
	started := w.started.Load()
	loopDone := w.loopDone
	w.lifecycleMu.Unlock()

	// Shutdown sealing with revocation runs on every terminal path below:
	// after normal drain attempts sealPG drains queued pre-seal submissions,
	// revokes every active lease, and moves all unconfirmed accepted credits
	// to residual exactly once. Late ack/release after seal is a no-op.
	if w.rec != nil && w.rec.flow != nil {
		defer w.rec.flow.sealPG()
	}

	var err error
	if cancel != nil {
		cancel()
	}
	if started {
		select {
		case <-loopDone:
		case <-ctx.Done():
			err = ctx.Err()
			w.refillMu.Lock()
			w.refillClosed = true
			w.refillMu.Unlock()
			return err
		}
	}
	drainCtxBase := context.WithoutCancel(ctx)
	var cancelDrain context.CancelFunc
	drainCtx := drainCtxBase
	if deadline, ok := ctx.Deadline(); ok {
		timeout := time.Until(deadline)
		if timeout <= 0 {
			timeout = time.Millisecond
		}
		drainCtx, cancelDrain = context.WithTimeout(drainCtxBase, timeout)
	} else {
		drainCtx, cancelDrain = context.WithTimeout(drainCtxBase, 5*time.Second)
	}
	defer cancelDrain()
	w.flushStateMu.Lock()
	flushDone := w.flushDone
	w.flushStateMu.Unlock()
	select {
	case <-flushDone:
	case <-ctx.Done():
		w.refillMu.Lock()
		w.refillClosed = true
		w.refillMu.Unlock()
		return fmt.Errorf("quality-sync: drain incomplete, remaining work is in flight: %w", ctx.Err())
	}
	drainedOnce := false
	for {
		if drainCtx.Err() != nil {
			w.rec.mu.Lock()
			qRem := 0
			for _, rows := range w.rec.pendingQuality {
				qRem += len(rows)
			}
			w.rec.mu.Unlock()
			fRem := w.rec.flow.pgWorkTotal()
			w.mu.Lock()
			pgRem := 0
			for _, m := range w.pgMinuteAbs {
				pgRem += len(m)
			}
			redisRem := 0
			for _, m := range w.minuteAbs {
				redisRem += len(m)
			}
			w.mu.Unlock()
			total := qRem + fRem + pgRem + redisRem
			if total > 0 {
				remErr := fmt.Errorf("quality-sync: drain incomplete, remaining quality=%d flow=%d pg=%d redis=%d: %w", qRem, fRem, pgRem, redisRem, drainCtx.Err())
				if err != nil {
					return fmt.Errorf("%w; %v", err, remErr)
				}
				return remErr
			}
			if err == nil {
				err = drainCtx.Err()
			}
			break
		}
		w.rec.mu.Lock()
		qPending := 0
		for _, rows := range w.rec.pendingQuality {
			qPending += len(rows)
		}
		w.rec.mu.Unlock()
		fPending := w.rec.flow.pgWorkTotal()
		w.mu.Lock()
		pgPending := len(w.pgMinuteAbs)
		redisPending := len(w.minuteAbs)
		w.mu.Unlock()
		if drainedOnce && qPending == 0 && fPending == 0 && pgPending == 0 && redisPending == 0 {
			break
		}
		w.doRedisLocked(drainCtx)
		w.doPGLocked(drainCtx)
		drainedOnce = true
	}
	w.rec.mu.Lock()
	qFinal := 0
	for _, rows := range w.rec.pendingQuality {
		qFinal += len(rows)
	}
	w.rec.mu.Unlock()
	fFinal := w.rec.flow.pgWorkTotal()
	w.mu.Lock()
	pgFinal := len(w.pgMinuteAbs)
	redisFinal := len(w.minuteAbs)
	w.mu.Unlock()
	if qFinal+fFinal+pgFinal+redisFinal > 0 {
		remErr := fmt.Errorf("quality-sync: drain incomplete, remaining quality=%d flow=%d pg=%d redis=%d", qFinal, fFinal, pgFinal, redisFinal)
		if err != nil {
			return fmt.Errorf("%w; %v", err, remErr)
		}
		return remErr
	}
	if started {
		select {
		case <-loopDone:
		case <-ctx.Done():
			if err == nil {
				err = ctx.Err()
			}
		}
	}
	return err
}
