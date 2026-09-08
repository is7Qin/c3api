// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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

type flowEntry struct {
	minute int64
	fm     *FlowMinute
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
	// committedFlow 是 flow 面的已提交累计快照（per minute）：UpsertFlowSnapshot
	// 按 (minute, instance_src, identity_version) 整组替换，跨周期 delta 必须先
	// 并入前次成功快照再写出，否则后周期会抹掉前周期链。
	committedFlow map[int64][]repository.RoutingFlowRow

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

func NewSyncWorker(rec *Recorder, rdb *redis.Client, pg PGQualityWriter, cfg SyncConfig, log *logx.Logger) *SyncWorker {
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
		seq:                  make(map[int64]int64),
		redisSeq:             make(map[int64]int64),
		pgSeq:                make(map[int64]int64),
		lastCell:             make(map[Key]cursorEntry),
		pgLastCell:           make(map[Key]cursorEntry),
		minuteAbs:            make(map[int64]map[Key]*QualityMinute),
		pgMinuteAbs:          make(map[int64]map[Key]*QualityMinute),
		committed:            make(map[int64]map[Key]*QualityMinute),
		committedFlow:        make(map[int64][]repository.RoutingFlowRow),
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

func (w *SyncWorker) collectRedisDeltaLocked(curMinuteUnix int64) map[int64]map[Key]*QualityMinute {
	if w.rec == nil {
		return nil
	}
	w.rec.mu.Lock()
	cells := make([]*Cell, 0, len(w.rec.active))
	for _, c := range w.rec.active {
		cells = append(cells, c)
	}
	w.rec.mu.Unlock()
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.clock().UTC().Truncate(time.Minute).Unix()
	// collect delta for all active cells, assign to minute bucket based on curMinuteUnix? Use current minute for new delta
	for _, c := range cells {
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
		prev, ok := w.lastCell[c.key]
		var delta cellSnap
		if ok && prev.gen == c.gen {
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
		} else {
			delta = cur
		}
		if delta.attempts == 0 && delta.successes == 0 && delta.ttftCount == 0 && delta.input == 0 && delta.output == 0 && delta.calls == 0 && delta.images == 0 && delta.err429 == 0 && delta.err4xx == 0 && delta.err5xx == 0 && delta.errNetwork == 0 {
			w.lastCell[c.key] = cursorEntry{snap: cur, gen: c.gen}
			continue
		}
		w.lastCell[c.key] = cursorEntry{snap: cur, gen: c.gen}
		if _, ok := w.minuteAbs[curMinuteUnix]; !ok {
			w.minuteAbs[curMinuteUnix] = make(map[Key]*QualityMinute)
		}
		m := w.minuteAbs[curMinuteUnix]
		if existing, ok := m[c.key]; ok {
			existing.attempts += delta.attempts
			existing.successes += delta.successes
			existing.err429 += delta.err429
			existing.err4xx += delta.err4xx
			existing.err5xx += delta.err5xx
			existing.errNetwork += delta.errNetwork
			existing.ttftCount += delta.ttftCount
			existing.sumQ32 += delta.sumQ32
			existing.sumSqQ32 += delta.sumSqQ32
			for i := range existing.hist {
				existing.hist[i] += delta.hist[i]
			}
			existing.inputTokens += delta.input
			existing.outputTokens += delta.output
			existing.cacheRead += delta.cacheRead
			existing.cacheCreate += delta.cacheCreate
			existing.calls += delta.calls
			existing.images += delta.images
		} else {
			qm := NewQualityMinute(curMinuteUnix, c.key)
			qm.attempts = delta.attempts
			qm.successes = delta.successes
			qm.err429 = delta.err429
			qm.err4xx = delta.err4xx
			qm.err5xx = delta.err5xx
			qm.errNetwork = delta.errNetwork
			qm.ttftCount = delta.ttftCount
			qm.sumQ32 = delta.sumQ32
			qm.sumSqQ32 = delta.sumSqQ32
			qm.hist = delta.hist
			qm.inputTokens = delta.input
			qm.outputTokens = delta.output
			qm.cacheRead = delta.cacheRead
			qm.cacheCreate = delta.cacheCreate
			qm.calls = delta.calls
			qm.images = delta.images
			m[c.key] = qm
		}
		_ = now
	}
	// return copy of all due minutes up to curMinuteUnix
	out := make(map[int64]map[Key]*QualityMinute)
	for minute, rows := range w.minuteAbs {
		if minute > curMinuteUnix {
			continue
		}
		cp := make(map[Key]*QualityMinute, len(rows))
		for k, v := range rows {
			cp[k] = v.Clone()
		}
		out[minute] = cp
	}
	return out
}

func (w *SyncWorker) collectPGDeltaLocked() map[int64]map[Key]*QualityMinute {
	if w.rec == nil {
		return nil
	}
	w.rec.mu.Lock()
	cells := make([]*Cell, 0, len(w.rec.active))
	for _, c := range w.rec.active {
		cells = append(cells, c)
	}
	w.rec.mu.Unlock()
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range cells {
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
		prev, ok := w.pgLastCell[c.key]
		var delta cellSnap
		if ok && prev.gen == c.gen {
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
		} else {
			delta = cur
		}
		if delta.attempts == 0 && delta.successes == 0 && delta.ttftCount == 0 && delta.input == 0 && delta.output == 0 && delta.calls == 0 && delta.images == 0 && delta.err429 == 0 && delta.err4xx == 0 && delta.err5xx == 0 && delta.errNetwork == 0 {
			w.pgLastCell[c.key] = cursorEntry{snap: cur, gen: c.gen}
			continue
		}
		w.pgLastCell[c.key] = cursorEntry{snap: cur, gen: c.gen}
		minute := w.clock().UTC().Truncate(time.Minute).Unix()
		if _, ok := w.pgMinuteAbs[minute]; !ok {
			w.pgMinuteAbs[minute] = make(map[Key]*QualityMinute)
		}
		m := w.pgMinuteAbs[minute]
		if existing, ok := m[c.key]; ok {
			existing.attempts += delta.attempts
			existing.successes += delta.successes
			existing.err429 += delta.err429
			existing.err4xx += delta.err4xx
			existing.err5xx += delta.err5xx
			existing.errNetwork += delta.errNetwork
			existing.ttftCount += delta.ttftCount
			existing.sumQ32 += delta.sumQ32
			existing.sumSqQ32 += delta.sumSqQ32
			for i := range existing.hist {
				existing.hist[i] += delta.hist[i]
			}
			existing.inputTokens += delta.input
			existing.outputTokens += delta.output
			existing.cacheRead += delta.cacheRead
			existing.cacheCreate += delta.cacheCreate
			existing.calls += delta.calls
			existing.images += delta.images
		} else {
			qm := NewQualityMinute(minute, c.key)
			qm.attempts = delta.attempts
			qm.successes = delta.successes
			qm.err429 = delta.err429
			qm.err4xx = delta.err4xx
			qm.err5xx = delta.err5xx
			qm.errNetwork = delta.errNetwork
			qm.ttftCount = delta.ttftCount
			qm.sumQ32 = delta.sumQ32
			qm.sumSqQ32 = delta.sumSqQ32
			qm.hist = delta.hist
			qm.inputTokens = delta.input
			qm.outputTokens = delta.output
			qm.cacheRead = delta.cacheRead
			qm.cacheCreate = delta.cacheCreate
			qm.calls = delta.calls
			qm.images = delta.images
			m[c.key] = qm
		}
	}
	out := make(map[int64]map[Key]*QualityMinute)
	for minute, rows := range w.pgMinuteAbs {
		cp := make(map[Key]*QualityMinute, len(rows))
		for k, v := range rows {
			cp[k] = v.Clone()
		}
		out[minute] = cp
	}
	return out
}

func (w *SyncWorker) ackRedisSuccess(minutes []int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, m := range minutes {
		delete(w.minuteAbs, m)
	}
}

func (w *SyncWorker) ackPGSuccess(minutes []int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, m := range minutes {
		delete(w.pgMinuteAbs, m)
	}
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
	savedLastCell := make(map[Key]cursorEntry, len(w.lastCell))
	for k, v := range w.lastCell {
		savedLastCell[k] = v
	}
	savedMinuteAbs := make(map[int64]map[Key]*QualityMinute)
	for minute, rows := range w.minuteAbs {
		cp := make(map[Key]*QualityMinute, len(rows))
		for k, v := range rows {
			cp[k] = v.Clone()
		}
		savedMinuteAbs[minute] = cp
	}
	w.mu.Unlock()

	activeAll := w.collectRedisDeltaLocked(curMinute)
	// also include pendingQuality for all due minutes
	w.rec.mu.Lock()
	pendingByMinute := make(map[int64]map[Key]*QualityMinute)
	for minute, rows := range w.rec.pendingQuality {
		if minute > curMinute {
			continue
		}
		cp := make(map[Key]*QualityMinute, len(rows))
		for k, v := range rows {
			cp[k] = v.Clone()
		}
		pendingByMinute[minute] = cp
	}
	w.rec.mu.Unlock()
	// Flow comes through the FlowOwner consumer handoff: owned clones of the
	// due buckets (queue drained on the owner side), never an alias of the
	// owner's accumulator.
	flowByMinute := w.rec.flow.dueSnapshot(curMinute)

	// merge activeAll + pending for each minute
	mergedByMinute := make(map[int64]map[Key]*QualityMinute)
	for minute, rows := range pendingByMinute {
		mergedByMinute[minute] = rows
	}
	for minute, rows := range activeAll {
		if _, ok := mergedByMinute[minute]; !ok {
			mergedByMinute[minute] = make(map[Key]*QualityMinute)
		}
		for k, v := range rows {
			if existing, ok := mergedByMinute[minute][k]; ok {
				existing.merge(v)
			} else {
				mergedByMinute[minute][k] = v.Clone()
			}
		}
	}
	if len(mergedByMinute) == 0 && len(flowByMinute) == 0 {
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
	pipe := w.rdb.Pipeline()
	for _, c := range cells {
		w.mu.Lock()
		seq := w.redisSeq[c.minute] + 1
		w.redisSeq[c.minute] = seq
		w.mu.Unlock()
		field := hex.EncodeToString(c.key.RouteClassID[:]) + ":" + hex.EncodeToString(c.key.QualityClassID[:]) + ":" + hex.EncodeToString(c.key.Fingerprint[:])
		val, _ := json.Marshal(map[string]any{
			"identity_version":  c.key.IdentityVersion,
			"route_class_id":    hex.EncodeToString(c.key.RouteClassID[:]),
			"quality_class_id":  hex.EncodeToString(c.key.QualityClassID[:]),
			"fingerprint":       hex.EncodeToString(c.key.Fingerprint[:]),
			"instance_src":      w.instanceSrc,
			"bucket_minute":     c.minute,
			"absolute_sequence": seq,
			"attempts":          c.qm.attempts,
			"successes":         c.qm.successes,
			"count_429":         c.qm.err429,
			"count_4xx":         c.qm.err4xx,
			"count_5xx":         c.qm.err5xx,
			"count_network":     c.qm.errNetwork,
			"ttft_n":            c.qm.ttftCount,
			"ttft_sum_q32":      c.qm.sumQ32,
			"ttft_sumsq_q32":    c.qm.sumSqQ32,
			"ttft_hist":         c.qm.hist[:],
			"input_tokens":      c.qm.inputTokens,
			"output_tokens":     c.qm.outputTokens,
			"cache_read":        c.qm.cacheRead,
			"cache_create":      c.qm.cacheCreate,
			"calls":             c.qm.calls,
			"images":            c.qm.images,
		})
		qKey := fmt.Sprintf("%s%d:%s", redisQualityPrefix, c.minute, w.instanceSrc)
		pipe.HSet(ctx, qKey, field, string(val))
		pipe.Expire(ctx, qKey, redisTTL)
	}
	for minute, fm := range flowByMinute {
		w.mu.Lock()
		seq := w.redisSeq[minute] + 1
		w.redisSeq[minute] = seq
		w.mu.Unlock()
		fKey := fmt.Sprintf("%s%d:%s", redisFlowPrefix, minute, w.instanceSrc)
		var flowVal []byte
		if fm.IsEmptySnapshot() {
			flowVal, _ = json.Marshal(map[string]any{
				"instance_src":      w.instanceSrc,
				"terminal_minute":   minute,
				"absolute_sequence": seq,
				"empty":             true,
			})
		} else if fm.HasFlowRows() {
			flowVal, _ = json.Marshal(map[string]any{
				"instance_src":      w.instanceSrc,
				"terminal_minute":   minute,
				"absolute_sequence": seq,
				"rows":              fm.FlowRows(),
			})
		} else {
			flowVal, _ = json.Marshal(map[string]any{
				"instance_src":      w.instanceSrc,
				"terminal_minute":   minute,
				"absolute_sequence": seq,
				"edges":             fm.Edges(),
				"counts":            fm.Counts(),
			})
		}
		pipe.Set(ctx, fKey, string(flowVal), redisTTL)
	}
	_, err := pipe.Exec(ctx)
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		w.lastRedisError = err.Error()
		w.stats.LastRedisError = w.lastRedisError
		w.stats.RedisErrors++
		// revert per-sink ack
		w.lastCell = savedLastCell
		w.minuteAbs = savedMinuteAbs
		if w.log != nil {
			w.log.Warn("quality redis publish failed", logx.Error(err))
		}
		return
	}
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

// doPGLocked 是 PG 面单次 flush（含失败 refill 回 recorder）；调用方必须持有
// flushMu。refill 只可能发生在持锁期间，这是 Close 生命周期屏障的前提。
func (w *SyncWorker) doPGLocked(ctx context.Context) {
	start := w.clock()
	if w.rec == nil || w.pg == nil {
		return
	}
	// collect PG active delta first, even if pending empty (active-only workload)
	pgActive := w.collectPGDeltaLocked()
	// Flow batch arrives via the FlowOwner's ownership transfer: the whole
	// drained accumulator map is caller-owned, pending restarts empty, and
	// sync never reads or mutates owner maps in place. Failed/deferred
	// entries return through EnqueueFlowMinute (the owner's merge API).
	pendF := w.rec.flow.takePending()
	hasPendingQ := false
	w.rec.mu.Lock()
	hasPendingQ = len(w.rec.pendingQuality) > 0
	w.rec.mu.Unlock()
	if !hasPendingQ && len(pendF) == 0 && len(pgActive) == 0 {
		return
	}
	w.rec.mu.Lock()
	pendQ := w.rec.pendingQuality
	w.rec.pendingQuality = make(map[int64]map[Key]*QualityMinute)
	w.rec.pendingBytes.Store(0)
	w.rec.mu.Unlock()

	// merge pgActive into pendQ
	for minute, rows := range pgActive {
		if _, ok := pendQ[minute]; !ok {
			pendQ[minute] = make(map[Key]*QualityMinute)
		}
		for k, v := range rows {
			if existing, ok := pendQ[minute][k]; ok {
				existing.merge(v)
			} else {
				pendQ[minute][k] = v.Clone()
			}
		}
	}
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
	var flowMinutes []flowEntry
	for minute, fm := range pendF {
		w.mu.Lock()
		seq := w.pgSeq[minute] + 1
		w.pgSeq[minute] = seq
		w.seq[minute] = seq
		w.mu.Unlock()
		flowMinutes = append(flowMinutes, flowEntry{minute: minute, fm: fm, seq: seq})
	}
	// also include any flow that was in pgMinuteAbs? No, flow handled via pendingFlow only
	var flowToFlush []flowEntry
	var flowDeferred []flowEntry
	flowBytes := 0
	for i, fe := range flowMinutes {
		if len(flowToFlush) >= pgMaxRows || flowBytes+EstimatedFlowMinuteBytes > pgMaxBytes || w.clock().Sub(start) > pgMaxDuration {
			flowDeferred = append(flowDeferred, flowMinutes[i:]...)
			break
		}
		flowToFlush = append(flowToFlush, fe)
		flowBytes += EstimatedFlowMinuteBytes
	}
	remainingF := w.flushFlowBatch(ctx, flowToFlush, start)
	if len(flowDeferred) > 0 {
		w.refillFlow(flowDeferred)
	}
	if len(remainingF) > 0 {
		w.refillFlow(remainingF)
	}
	// on failure, pg delta should be retained for next attempt; we cleared pgMinuteAbs earlier, so need to restore unflushed?
	// Our pgActive deltas that were merged into pendQ and then deferred/failed are now in pendingQuality via refill, so they will be retried.
	// For successful minutes, committed already updated.
	w.mu.Lock()
	w.lastPG = w.clock()
	w.stats.LastPGMs = w.clock().Sub(start).Milliseconds()
	w.stats.BatchQuality = int64(len(toFlush) - len(remainingQ))
	w.stats.BatchFlow = int64(len(flowToFlush) - len(remainingF))
	oldest := w.oldestPendingMinute(pendQ, pendF)
	if oldest != 0 {
		w.stats.LagMs = w.clock().Sub(time.Unix(oldest, 0)).Milliseconds()
		if w.stats.LagMs < 0 {
			w.stats.LagMs = 0
		}
	}
	w.stats.PendingQuality = w.rec.MinuteBucketCount()
	w.stats.PendingBytes = w.rec.PendingBytes()
	w.pruneLongLivedLocked(w.clock().Unix())
	w.mu.Unlock()
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
		w.committed[r.minute][r.key] = r.qm.Clone()
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

func (w *SyncWorker) flushFlowBatch(ctx context.Context, entries []flowEntry, start time.Time) []flowEntry {
	if len(entries) == 0 {
		return nil
	}
	var failed []flowEntry
	for _, e := range entries {
		if w.clock().Sub(start) > pgMaxDuration {
			failed = append(failed, e)
			continue
		}
		w.mu.Lock()
		prev := w.committedFlow[e.minute]
		w.mu.Unlock()
		rows := flowRowsFromMinute(e.fm, w.instanceSrc, e.seq)
		if prev != nil {
			// 累计快照语义：repo 整组替换，写出必须 = 已提交 + 本周期 delta。
			// 空标记 delta 贡献零行，永不抹掉已提交链。
			rows = mergeFlowRows(prev, rows)
			tm := time.Unix(e.minute, 0).UTC()
			for i := range rows {
				rows[i].InstanceSrc = w.instanceSrc
				rows[i].AbsoluteSequence = e.seq
				rows[i].TerminalMinute = tm
			}
		}
		if err := w.pg.UpsertFlowSnapshot(ctx, w.instanceSrc, time.Unix(e.minute, 0).UTC(), 1, e.seq, rows); err != nil {
			failed = append(failed, e)
			continue
		}
		// committed 只在 upsert 成功后推进；失败/延迟的 delta 经 refill 重放，
		// 下一周期再并入，不会重复计数。
		cp := make([]repository.RoutingFlowRow, len(rows))
		copy(cp, rows)
		w.mu.Lock()
		w.committedFlow[e.minute] = cp
		w.mu.Unlock()
	}
	return failed
}

func flowRowsFromMinute(fm *FlowMinute, instanceSrc string, seq int64) []repository.RoutingFlowRow {
	if fm == nil {
		return nil
	}
	if fm.IsEmptySnapshot() {
		return nil
	}
	if fm.HasFlowRows() {
		rows := fm.FlowRows()
		for i := range rows {
			rows[i].InstanceSrc = instanceSrc
			rows[i].AbsoluteSequence = seq
			rows[i].TerminalMinute = time.Unix(fm.Minute(), 0).UTC()
		}
		return rows
	}
	// legacy edges fallback (should not happen for new code, but preserve)
	var out []repository.RoutingFlowRow
	edges := fm.Edges()
	counts := fm.Counts()
	hasData := false
	for i := 0; i < 8; i++ {
		if edges[i] != 0 || counts[i] != 0 {
			hasData = true
			break
		}
	}
	if !hasData {
		return nil
	}
	for i := 0; i < 8; i++ {
		if edges[i] == 0 && counts[i] == 0 {
			continue
		}
		out = append(out, repository.RoutingFlowRow{
			IdentityVersion:  1,
			TerminalMinute:   time.Unix(fm.Minute(), 0).UTC(),
			Ordinal:          int16(i + 1),
			Lane:             fmt.Sprintf("lane-%d", i),
			AccountID:        edges[i],
			TransitionReason: "flow",
			Outcome:          "success",
			IsTerminal:       true,
			Generation:       1,
			InstanceSrc:      instanceSrc,
			AbsoluteSequence: seq,
			ChainCount:       counts[i],
		})
	}
	return out
}

func (w *SyncWorker) refillFlow(entries []flowEntry) {
	w.refillMu.Lock()
	defer w.refillMu.Unlock()
	if w.refillClosed {
		var rows int64
		for _, e := range entries {
			rows += int64(len(e.fm.FlowRows()))
		}
		w.mu.Lock()
		w.stats.ResidualFlow += rows
		w.mu.Unlock()
		return
	}
	for _, e := range entries {
		w.mu.Lock()
		curSeq := w.pgSeq[e.minute]
		w.mu.Unlock()
		if e.seq < curSeq {
			continue
		}
		// No "pending already exists" skip: a fresh same-minute delta may have
		// arrived while this flush was in flight; EnqueueFlowMinute merges the
		// requeued delta into it (distinct edges sum, empty marker never erases).
		// Dropping here would lose the failed delta permanently.
		if err := w.rec.EnqueueFlowMinute(e.fm); err != nil {
			w.mu.Lock()
			w.stats.DroppedFlow++
			w.mu.Unlock()
			if w.log != nil {
				w.log.Warn("flow refill dropped due to capacity", logx.Error(err))
			}
		}
	}
}

func (w *SyncWorker) pruneLongLivedLocked(nowUnix int64) {
	pending := make(map[int64]struct{})
	w.rec.mu.Lock()
	for m := range w.rec.pendingQuality {
		pending[m] = struct{}{}
	}
	w.rec.mu.Unlock()
	// Owner lane via handoff API (queued minutes included).
	w.rec.flow.collectLiveMinutes(pending)
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
	for m := range w.pgSeq {
		if m < cutoff {
			if _, ok := pending[m]; !ok {
				delete(w.pgSeq, m)
			}
		}
	}
	for m := range w.committed {
		if m < cutoff {
			if _, ok := pending[m]; !ok {
				delete(w.committed, m)
			}
		}
	}
	for m := range w.committedFlow {
		if m < cutoff {
			if _, ok := pending[m]; !ok {
				delete(w.committedFlow, m)
			}
		}
	}
}

func (w *SyncWorker) oldestPendingMinute(q map[int64]map[Key]*QualityMinute, f map[int64]*FlowMinute) int64 {
	var oldest int64
	first := true
	for m := range q {
		if first || m < oldest {
			oldest = m
			first = false
		}
	}
	for m := range f {
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
		// Flow visibility goes through the owner stats API (accumulator
		// minutes plus queued submissions); the owner owns that state.
		s.PendingQuality = pendingQ
		s.PendingFlow = w.rec.flow.pendingTotal()
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
			fRem := w.rec.flow.pendingTotal()
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
		fPending := w.rec.flow.pendingTotal()
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
	fFinal := w.rec.flow.pendingTotal()
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
