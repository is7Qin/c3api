// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

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

// PGQualityWriter is a thin exact adapter over repository.PartitionRepo.
type PGQualityWriter interface {
	UpsertQualityAndMarkDirty(ctx context.Context, row repository.RoutingQualityRow) error
	UpsertFlowSnapshot(ctx context.Context, instanceSrc string, terminalMinute time.Time, identityVersion int16, absoluteSequence int64, rows []repository.RoutingFlowRow) error
}

type SyncConfig struct {
	BatchSize   int
	InstanceSrc string
}

type SyncStats struct {
	PendingQuality int   `json:"pending_quality"`
	PendingFlow    int   `json:"pending_flow"`
	PendingBytes   int64 `json:"pending_bytes"`
	LagMs          int64 `json:"lag_ms"`
	DroppedQuality int64 `json:"dropped_quality"`
	DroppedFlow    int64 `json:"dropped_flow"`
	PoisonDropped  int64 `json:"poison_dropped"`
	BatchQuality   int64 `json:"batch_quality"`
	BatchFlow      int64 `json:"batch_flow"`
	FreshnessMs    int64 `json:"freshness_ms"`
	LastRedisMs    int64 `json:"last_redis_ms"`
	LastPGMs       int64 `json:"last_pg_ms"`
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
	attempts   int64
	successes  int64
	err429     int64
	err4xx     int64
	err5xx     int64
	errNetwork int64
	ttftCount  int64
	sumQ32     int64
	sumSqQ32   int64
	hist       [10]int64
	input      int64
	output     int64
	cacheRead  int64
	cacheCreate int64
	calls      int64
	images     int64
}

type SyncWorker struct {
	rec         *Recorder
	rdb         *redis.Client
	pg          PGQualityWriter
	instanceSrc string
	clock       func() time.Time
	log         *logx.Logger
	cfg         SyncConfig

	mu        sync.Mutex
	flushMu   sync.Mutex
	seq       map[int64]int64
	stats     SyncStats
	lastRedis time.Time
	lastPG    time.Time
	poison    atomic.Int64
	batchQ    atomic.Int64
	batchF    atomic.Int64

	lastCell   map[Key]cellSnap
	minuteAbs  map[int64]map[Key]*QualityMinute
	committed  map[int64]map[Key]*QualityMinute

	started  atomic.Bool
	closeOnce sync.Once
	baseCtx  context.Context
	cancel   context.CancelFunc
	loopDone <-chan struct{}
	loopDoneCh chan struct{}
	inflightAbandonGrace time.Duration
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
	baseCtx, cancel := context.WithCancel(context.Background())
	w := &SyncWorker{
		rec:         rec,
		rdb:         rdb,
		pg:          pg,
		instanceSrc: src,
		clock:       time.Now,
		log:         log,
		cfg:         cfg,
		seq:         make(map[int64]int64),
		lastCell:    make(map[Key]cellSnap),
		minuteAbs:   make(map[int64]map[Key]*QualityMinute),
		committed:   make(map[int64]map[Key]*QualityMinute),
		baseCtx:     baseCtx,
		cancel:      cancel,
		loopDoneCh:  make(chan struct{}),
		inflightAbandonGrace: 500 * time.Millisecond,
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
	if !w.started.CompareAndSwap(false, true) {
		return fmt.Errorf("quality-sync: already started")
	}
	// single supervised serial loop
	w.loopDone = worker.GoLoop(ctx, "quality-sync", w.log, w.loop)
	// also close internal channel when loop exits to satisfy Close wait on unstarted
	go func() {
		<-w.loopDone
		close(w.loopDoneCh)
	}()
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

func (w *SyncWorker) collectActiveDeltaLocked(minuteUnix int64) map[Key]*QualityMinute {
	if w.rec == nil {
		return nil
	}
	w.rec.mu.Lock()
	defer w.rec.mu.Unlock()
	for _, c := range w.rec.active {
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
		if ok {
			delta.attempts = cur.attempts - prev.attempts
			delta.successes = cur.successes - prev.successes
			delta.err429 = cur.err429 - prev.err429
			delta.err4xx = cur.err4xx - prev.err4xx
			delta.err5xx = cur.err5xx - prev.err5xx
			delta.errNetwork = cur.errNetwork - prev.errNetwork
			delta.ttftCount = cur.ttftCount - prev.ttftCount
			delta.sumQ32 = cur.sumQ32 - prev.sumQ32
			delta.sumSqQ32 = cur.sumSqQ32 - prev.sumSqQ32
			for i := range delta.hist {
				delta.hist[i] = cur.hist[i] - prev.hist[i]
			}
			delta.input = cur.input - prev.input
			delta.output = cur.output - prev.output
			delta.cacheRead = cur.cacheRead - prev.cacheRead
			delta.cacheCreate = cur.cacheCreate - prev.cacheCreate
			delta.calls = cur.calls - prev.calls
			delta.images = cur.images - prev.images
		} else {
			delta = cur
		}
		// if delta is zero skip
		if delta.attempts == 0 && delta.successes == 0 && delta.ttftCount == 0 && delta.input == 0 && delta.output == 0 && delta.calls == 0 && delta.images == 0 && delta.err429 == 0 && delta.err4xx == 0 && delta.err5xx == 0 && delta.errNetwork == 0 {
			w.lastCell[c.key] = cur
			continue
		}
		w.lastCell[c.key] = cur
		// accumulate into minuteAbs
		if _, ok := w.minuteAbs[minuteUnix]; !ok {
			w.minuteAbs[minuteUnix] = make(map[Key]*QualityMinute)
		}
		m := w.minuteAbs[minuteUnix]
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
			qm := NewQualityMinute(minuteUnix, c.key)
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
	// return copy of minuteAbs for this minute
	if m, ok := w.minuteAbs[minuteUnix]; ok {
		cp := make(map[Key]*QualityMinute, len(m))
		for k, v := range m {
			cp[k] = v.Clone()
		}
		return cp
	}
	return nil
}

func (w *SyncWorker) doRedis(ctx context.Context) {
	if !w.flushMu.TryLock() {
		return
	}
	defer w.flushMu.Unlock()
	start := w.clock()
	minute := start.UTC().Truncate(time.Minute)
	unix := minute.Unix()
	w.mu.Lock()
	seq := w.seq[unix] + 1
	w.seq[unix] = seq
	w.mu.Unlock()

	// collect active delta and merge with pendingQuality snapshot for this minute
	activeMap := w.collectActiveDeltaLocked(unix)

	w.rec.mu.Lock()
	pendingForMinute := make(map[Key]*QualityMinute)
	if rows, ok := w.rec.pendingQuality[unix]; ok {
		for k, v := range rows {
			pendingForMinute[k] = v.Clone()
		}
	}
	pendingFlowForMinute, hasFlow := w.rec.pendingFlow[unix]
	if hasFlow {
		pendingFlowForMinute = pendingFlowForMinute.Clone()
	}
	w.rec.mu.Unlock()

	// merge pending + active delta into cells to publish
	merged := make(map[Key]*QualityMinute)
	for k, v := range pendingForMinute {
		merged[k] = v.Clone()
	}
	for k, v := range activeMap {
		if existing, ok := merged[k]; ok {
			existing.merge(v)
		} else {
			merged[k] = v.Clone()
		}
	}
	if len(merged) == 0 && pendingFlowForMinute == nil {
		w.mu.Lock()
		w.lastRedis = start
		w.mu.Unlock()
		return
	}
	type redisCell struct {
		key Key
		qm  *QualityMinute
	}
	var cells []redisCell
	for k, qm := range merged {
		cells = append(cells, redisCell{key: k, qm: qm})
	}
	// budget enforcement for redis (truncate without loss, since pending remains)
	if len(cells) > redisMaxCells {
		cells = cells[:redisMaxCells]
	}
	bytesEst := 0
	for i, c := range cells {
		bytesEst += EstimatedQualityRowBytes
		_ = c
		if bytesEst > redisMaxBytes {
			cells = cells[:i]
			break
		}
		if w.clock().Sub(start) > redisMaxDuration {
			cells = cells[:i]
			break
		}
	}
	if w.rdb == nil {
		w.mu.Lock()
		w.lastRedis = start
		w.stats.LastRedisMs = w.clock().Sub(start).Milliseconds()
		w.mu.Unlock()
		return
	}
	pipe := w.rdb.Pipeline()
	qKey := fmt.Sprintf("%s%d:%s", redisQualityPrefix, unix, w.instanceSrc)
	for _, c := range cells {
		field := hex.EncodeToString(c.key.RouteClassID[:]) + ":" + hex.EncodeToString(c.key.QualityClassID[:]) + ":" + hex.EncodeToString(c.key.Fingerprint[:])
		val, _ := json.Marshal(map[string]any{
			"identity_version":  c.key.IdentityVersion,
			"route_class_id":    hex.EncodeToString(c.key.RouteClassID[:]),
			"quality_class_id":  hex.EncodeToString(c.key.QualityClassID[:]),
			"fingerprint":       hex.EncodeToString(c.key.Fingerprint[:]),
			"instance_src":      w.instanceSrc,
			"bucket_minute":     unix,
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
		pipe.HSet(ctx, qKey, field, string(val))
	}
	pipe.Expire(ctx, qKey, redisTTL)
	// flow namespace publish
	if hasFlow && pendingFlowForMinute != nil {
		fKey := fmt.Sprintf("%s%d:%s", redisFlowPrefix, unix, w.instanceSrc)
		flowVal, _ := json.Marshal(map[string]any{
			"instance_src":      w.instanceSrc,
			"terminal_minute":   unix,
			"absolute_sequence": seq,
			"edges":             pendingFlowForMinute.Edges(),
			"counts":            pendingFlowForMinute.Counts(),
		})
		pipe.Set(ctx, fKey, string(flowVal), redisTTL)
	}
	_, err := pipe.Exec(ctx)
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		// degrade freshness, do not update lastRedis
		if w.log != nil {
			w.log.Warn("quality redis publish failed", logx.Error(err))
		}
		return
	}
	w.lastRedis = w.clock()
	w.stats.LastRedisMs = w.clock().Sub(start).Milliseconds()
	w.stats.FreshnessMs = 0
}

func (w *SyncWorker) doPG(ctx context.Context) {
	if !w.flushMu.TryLock() {
		return
	}
	defer w.flushMu.Unlock()
	start := w.clock()
	if w.rec == nil || w.pg == nil {
		return
	}
	w.rec.mu.Lock()
	if len(w.rec.pendingQuality) == 0 && len(w.rec.pendingFlow) == 0 {
		w.rec.mu.Unlock()
		return
	}
	pendQ := w.rec.pendingQuality
	pendF := w.rec.pendingFlow
	w.rec.pendingQuality = make(map[int64]map[Key]*QualityMinute)
	w.rec.pendingFlow = make(map[int64]*FlowMinute)
	w.rec.pendingBytes.Store(0)
	w.rec.mu.Unlock()

	// active delta for PG: also include active delta accumulation similar to redis but for each minute present in pending or current minute
	currentMinuteUnix := start.UTC().Truncate(time.Minute).Unix()
	activeForCurrent := w.collectActiveDeltaLocked(currentMinuteUnix)
	// merge activeForCurrent into pendQ for current minute
	if len(activeForCurrent) > 0 {
		if _, ok := pendQ[currentMinuteUnix]; !ok {
			pendQ[currentMinuteUnix] = make(map[Key]*QualityMinute)
		}
		for k, v := range activeForCurrent {
			if existing, ok := pendQ[currentMinuteUnix][k]; ok {
				existing.merge(v)
			} else {
				pendQ[currentMinuteUnix][k] = v.Clone()
			}
		}
	}

	// build qRows with budget handling
	var allRows []qRow
	for minute, rows := range pendQ {
		w.mu.Lock()
		seq := w.seq[minute] + 1
		w.seq[minute] = seq
		w.mu.Unlock()
		for k, qm := range rows {
			// merge with committed to avoid losing prior PG data
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
	// sort not needed, but budget enforcement must retain excess
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
	// if allRows within budget, toFlush = allRows
	if len(deferred) == 0 && len(toFlush) != len(allRows) {
		// already handled
	}
	remainingQ := w.flushQualityBatch(ctx, toFlush, start)
	// refill deferred (budget excess) and remaining (failed)
	if len(deferred) > 0 {
		// need to refill original delta, not merged, so we need to keep original rows for deferred
		// deferred currently holds merged rows, but we should refill the original delta rows to avoid double merging next time
		// For budget deferred, we haven't yet merged committed, so we should refill original pending rows.
		// Simpler: collect deferred original from pendQ not merged. But we merged already, so to avoid double, refill the delta part.
		w.refillQualityDelta(deferred, pendQ)
	}
	if len(remainingQ) > 0 {
		w.refillQualityDelta(remainingQ, pendQ)
	}
	// flow handling
	var flowMinutes []flowEntry
	for minute, fm := range pendF {
		w.mu.Lock()
		seq := w.seq[minute] + 1
		w.seq[minute] = seq
		w.mu.Unlock()
		flowMinutes = append(flowMinutes, flowEntry{minute: minute, fm: fm, seq: seq})
	}
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
	w.mu.Unlock()
}

func (w *SyncWorker) refillQualityDelta(rows []qRow, originalPend map[int64]map[Key]*QualityMinute) {
	// rows are merged absolutes, but we need to refill delta (original) to avoid double counting on next flush
	// Map from minute+key to original delta
	for _, r := range rows {
		delta := r.qm
		if origRows, ok := originalPend[r.minute]; ok {
			if orig, ok2 := origRows[r.key]; ok2 {
				delta = orig
			} else {
				// this row was merged from committed+delta, so delta is difference
				w.mu.Lock()
				committed := w.committed[r.minute]
				w.mu.Unlock()
				if committed != nil {
					if prev, ok := committed[r.key]; ok {
						// subtract committed to get delta
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
		_ = w.rec.EnqueueQualityMinute(delta)
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
			if err := w.insertQualityChunk(ctx, chunk); err != nil {
				if len(chunk) == 1 {
					// single poison detection
					if err2 := w.insertQualityChunk(ctx, chunk); err2 != nil {
						w.poison.Add(1)
						w.mu.Lock()
						w.stats.PoisonDropped++
						w.mu.Unlock()
						continue
					}
					failed = append(failed, chunk...)
					continue
				}
				poison, refill := w.bisectQuality(ctx, chunk)
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
				// success: update committed
				w.mu.Lock()
				for _, r := range chunk {
					if _, ok := w.committed[r.minute]; !ok {
						w.committed[r.minute] = make(map[Key]*QualityMinute)
					}
					w.committed[r.minute][r.key] = r.qm.Clone()
				}
				w.mu.Unlock()
			}
		}
	}
	return failed
}

func (w *SyncWorker) insertQualityChunk(ctx context.Context, chunk []qRow) error {
	for _, r := range chunk {
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
			return err
		}
	}
	return nil
}

func (w *SyncWorker) bisectQuality(ctx context.Context, chunk []qRow) (poison *qRow, refill []qRow) {
	if len(chunk) == 1 {
		if err := w.insertQualityChunk(ctx, chunk); err != nil {
			return &chunk[0], nil
		}
		return nil, nil
	}
	mid := len(chunk) / 2
	left, right := chunk[:mid], chunk[mid:]
	if err := w.insertQualityChunk(ctx, left); err == nil {
		// left succeeded, need to handle right
		// update committed for left
		w.mu.Lock()
		for _, r := range left {
			if _, ok := w.committed[r.minute]; !ok {
				w.committed[r.minute] = make(map[Key]*QualityMinute)
			}
			w.committed[r.minute][r.key] = r.qm.Clone()
		}
		w.mu.Unlock()
		p, rf := w.bisectQuality(ctx, right)
		return p, rf
	}
	if err := w.insertQualityChunk(ctx, right); err == nil {
		w.mu.Lock()
		for _, r := range right {
			if _, ok := w.committed[r.minute]; !ok {
				w.committed[r.minute] = make(map[Key]*QualityMinute)
			}
			w.committed[r.minute][r.key] = r.qm.Clone()
		}
		w.mu.Unlock()
		p, rf := w.bisectQuality(ctx, left)
		return p, rf
	}
	return nil, chunk
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
		rows := flowRowsFromMinute(e.fm, w.instanceSrc, e.seq)
		if err := w.pg.UpsertFlowSnapshot(ctx, w.instanceSrc, time.Unix(e.minute, 0).UTC(), 1, e.seq, rows); err != nil {
			failed = append(failed, e)
			continue
		}
	}
	return failed
}

func flowRowsFromMinute(fm *FlowMinute, instanceSrc string, seq int64) []repository.RoutingFlowRow {
	if fm == nil {
		return nil
	}
	var out []repository.RoutingFlowRow
	edges := fm.Edges()
	counts := fm.Counts()
	for i := 0; i < 8; i++ {
		if edges[i] == 0 && counts[i] == 0 {
			continue
		}
		out = append(out, repository.RoutingFlowRow{
			IdentityVersion: 1,
			TerminalMinute:  time.Unix(fm.Minute(), 0).UTC(),
			Ordinal:         int16(i + 1),
			Lane:            fmt.Sprintf("lane-%d", i),
			AccountID:       edges[i],
			TransitionReason: "flow",
			Outcome:         "success",
			IsTerminal:      true,
			Generation:      1,
			InstanceSrc:     instanceSrc,
			AbsoluteSequence: seq,
			ChainCount:      counts[i],
		})
	}
	if len(out) == 0 {
		// preserve empty snapshot semantics via empty rows but keep sequence
		return nil
	}
	return out
}

func (w *SyncWorker) refillFlow(entries []flowEntry) {
	for _, e := range entries {
		_ = w.rec.EnqueueFlowMinute(e.fm)
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

func (w *SyncWorker) Stats() SyncStats {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.stats
	if w.rec != nil {
		w.rec.mu.Lock()
		pendingQ := 0
		for _, rows := range w.rec.pendingQuality {
			pendingQ += len(rows)
		}
		pendingF := len(w.rec.pendingFlow)
		w.rec.mu.Unlock()
		s.PendingQuality = pendingQ
		s.PendingFlow = pendingF
		s.PendingBytes = w.rec.PendingBytes()
		s.DroppedQuality = w.rec.QualityOverflow()
		s.DroppedFlow = w.rec.FlowOverflow()
	}
	s.PoisonDropped = w.poison.Load()
	if !w.lastRedis.IsZero() {
		s.FreshnessMs = w.clock().Sub(w.lastRedis).Milliseconds()
	}
	return s
}

func (w *SyncWorker) Close(ctx context.Context) error {
	var err error
	w.closeOnce.Do(func() {
		w.cancel()
		// wait for loop to exit with bounded context
		if w.started.Load() {
			select {
			case <-w.loopDoneCh:
			case <-w.loopDone:
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		} else {
			// unstarted: ensure loopDoneCh closed safely
			select {
			case <-w.loopDoneCh:
			default:
				close(w.loopDoneCh)
			}
		}
		// bounded drain with non-canceled context
		drainCtx := context.WithoutCancel(ctx)
		drainCtx, cancel := context.WithTimeout(drainCtx, 5*time.Second)
		defer cancel()
		// wait for in-flight flush
		acquired := make(chan struct{})
		go func() {
			w.flushMu.Lock()
			close(acquired)
		}()
		select {
		case <-acquired:
			w.flushMu.Unlock()
		case <-drainCtx.Done():
			w.cancel()
			select {
			case <-acquired:
				w.flushMu.Unlock()
			case <-time.After(w.inflightAbandonGrace):
				if w.log != nil {
					w.log.Warn("quality sync close: in-flight flush not finished, abandoning")
				}
				return
			}
		}
		// final drain PG
		w.doPG(drainCtx)
	})
	return err
}
