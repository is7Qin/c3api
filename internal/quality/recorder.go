// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

const (
	DefaultRetiredCap        = 65536
	DefaultPendingCapBytes   = 256 * 1024 * 1024
	DefaultMinuteBucketsCap  = 4096
	EstimatedCellBytes       = 256
	EstimatedQualityRowBytes = 512
	EstimatedFlowMinuteBytes = 4096
	EstimatedFlowRowBytes    = 512
	DefaultFlowRowCap        = DefaultPendingCapBytes / EstimatedFlowRowBytes
	q32Scale                 = 1 << 32
	retiredBit               = uint64(1) << 63
	closedBit                = uint64(1) << 63
	inflightMask             = ^closedBit
)

var globalRecorderID atomic.Uint64

// Key is the canonical routing identity of a quality cell/row: the
// unique index (identity_version, route_class_id, quality_class_id,
// candidate_fingerprint) minus the minute bucket.
type Key struct {
	IdentityVersion int16
	RouteClassID    [32]byte
	QualityClassID  [32]byte
	Fingerprint     [32]byte
}

func CanonicalKey(routeClassID, qualityClassID, fingerprint [32]byte) Key {
	return Key{
		IdentityVersion: int16(domain.RoutingIdentityVersion),
		RouteClassID:    routeClassID,
		QualityClassID:  qualityClassID,
		Fingerprint:     fingerprint,
	}
}

type Observation struct {
	Success             bool
	TTFTMs              *int64
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	Tokens              int64
	Calls               int64
	Images              int64
	ErrClass            int
	IsCancel            bool
	IsLocal             bool
	IsReservation       bool
}

const (
	ErrClassNone    = -1
	ErrClass429     = 0
	ErrClass5xx     = 1
	ErrClassNetwork = 2
	ErrClass4xx     = 3
)

var TTFTEdges = [10]int64{10, 25, 50, 100, 250, 500, 1000, 2000, 5000, 10000}

func histIndex(ttft int64) int {
	for i, e := range TTFTEdges {
		if ttft <= e {
			return i
		}
	}
	return 9
}

func toQ32(ttft int64) int64 {
	if ttft < 1 {
		ttft = 1
	}
	return int64(math.Log(float64(ttft)) * float64(q32Scale))
}

func toSqQ32(ttft int64) int64 {
	if ttft < 1 {
		ttft = 1
	}
	lg := math.Log(float64(ttft))
	return int64(lg * lg * float64(q32Scale))
}

type Cell struct {
	key          Key
	owner        uint64
	gen          uint64
	state        atomic.Uint64
	converged    bool
	attempts     atomic.Int64
	successes    atomic.Int64
	ttftCount    atomic.Int64
	sumQ32       atomic.Int64
	sumSq        atomic.Int64
	hist         [10]atomic.Int64
	errClasses   [4]atomic.Int64
	inputTokens  atomic.Int64
	outputTokens atomic.Int64
	cacheRead    atomic.Int64
	cacheCreate  atomic.Int64
	tokens       atomic.Int64
	calls        atomic.Int64
	images       atomic.Int64
}

func (c *Cell) tryPin() bool {
	for {
		s := c.state.Load()
		if s&retiredBit != 0 {
			return false
		}
		if c.state.CompareAndSwap(s, s+1) {
			return true
		}
	}
}

func (c *Cell) retireCAS() bool {
	for {
		s := c.state.Load()
		if s&retiredBit != 0 {
			return false
		}
		if c.state.CompareAndSwap(s, s|retiredBit) {
			return true
		}
	}
}

func (c *Cell) inflight() uint64 {
	return c.state.Load() &^ retiredBit
}

func (c *Cell) isRetired() bool {
	return c.state.Load()&retiredBit != 0
}

func (c *Cell) isReclaimable() bool {
	return c.state.Load() == retiredBit
}

type QualityMinute struct {
	minute       int64
	key          Key
	attempts     int64
	successes    int64
	err429       int64
	err4xx       int64
	err5xx       int64
	errNetwork   int64
	ttftCount    int64
	sumQ32       int64
	sumSqQ32     int64
	hist         [10]int64
	inputTokens  int64
	outputTokens int64
	cacheRead    int64
	cacheCreate  int64
	calls        int64
	images       int64
}

func NewQualityMinute(minute int64, key Key) *QualityMinute {
	return &QualityMinute{minute: minute, key: key}
}
func (q *QualityMinute) Minute() int64                { return q.minute }
func (q *QualityMinute) Key() Key                     { return q.key }
func (q *QualityMinute) IdentityVersion() int16       { return q.key.IdentityVersion }
func (q *QualityMinute) RouteClassID() [32]byte       { return q.key.RouteClassID }
func (q *QualityMinute) QualityClassID() [32]byte     { return q.key.QualityClassID }
func (q *QualityMinute) Fingerprint() [32]byte        { return q.key.Fingerprint }
func (q *QualityMinute) Attempts() int64              { return q.attempts }
func (q *QualityMinute) SetAttempts(v int64)          { q.attempts = v }
func (q *QualityMinute) Successes() int64             { return q.successes }
func (q *QualityMinute) SetSuccesses(v int64)         { q.successes = v }
func (q *QualityMinute) Err429() int64                { return q.err429 }
func (q *QualityMinute) SetErr429(v int64)            { q.err429 = v }
func (q *QualityMinute) Err4xx() int64                { return q.err4xx }
func (q *QualityMinute) SetErr4xx(v int64)            { q.err4xx = v }
func (q *QualityMinute) Err5xx() int64                { return q.err5xx }
func (q *QualityMinute) SetErr5xx(v int64)            { q.err5xx = v }
func (q *QualityMinute) ErrNetwork() int64            { return q.errNetwork }
func (q *QualityMinute) SetErrNetwork(v int64)        { q.errNetwork = v }
func (q *QualityMinute) TTFTCount() int64             { return q.ttftCount }
func (q *QualityMinute) SetTTFTCount(v int64)         { q.ttftCount = v }
func (q *QualityMinute) SumQ32() int64                { return q.sumQ32 }
func (q *QualityMinute) SetSumQ32(v int64)            { q.sumQ32 = v }
func (q *QualityMinute) SumSqQ32() int64              { return q.sumSqQ32 }
func (q *QualityMinute) SetSumSqQ32(v int64)          { q.sumSqQ32 = v }
func (q *QualityMinute) Hist() [10]int64              { return q.hist }
func (q *QualityMinute) SetHist(h [10]int64)          { q.hist = h }
func (q *QualityMinute) InputTokens() int64           { return q.inputTokens }
func (q *QualityMinute) SetInputTokens(v int64)       { q.inputTokens = v }
func (q *QualityMinute) OutputTokens() int64          { return q.outputTokens }
func (q *QualityMinute) SetOutputTokens(v int64)      { q.outputTokens = v }
func (q *QualityMinute) CacheReadTokens() int64       { return q.cacheRead }
func (q *QualityMinute) SetCacheReadTokens(v int64)   { q.cacheRead = v }
func (q *QualityMinute) CacheCreateTokens() int64     { return q.cacheCreate }
func (q *QualityMinute) SetCacheCreateTokens(v int64) { q.cacheCreate = v }
func (q *QualityMinute) Calls() int64                 { return q.calls }
func (q *QualityMinute) SetCalls(v int64)             { q.calls = v }
func (q *QualityMinute) Images() int64                { return q.images }
func (q *QualityMinute) SetImages(v int64)            { q.images = v }

func (q *QualityMinute) Clone() *QualityMinute {
	cp := *q
	return &cp
}

// merge folds another same-identity same-minute absolute row into this one.
func (q *QualityMinute) merge(o *QualityMinute) {
	q.attempts += o.attempts
	q.successes += o.successes
	q.err429 += o.err429
	q.err4xx += o.err4xx
	q.err5xx += o.err5xx
	q.errNetwork += o.errNetwork
	q.ttftCount += o.ttftCount
	q.sumQ32 += o.sumQ32
	q.sumSqQ32 += o.sumSqQ32
	for i := range q.hist {
		q.hist[i] += o.hist[i]
	}
	q.inputTokens += o.inputTokens
	q.outputTokens += o.outputTokens
	q.cacheRead += o.cacheRead
	q.cacheCreate += o.cacheCreate
	q.calls += o.calls
	q.images += o.images
}

type FlowMinute struct {
	minute        int64
	edges         [8]int64
	counts        [8]int64
	flowRows      []repository.RoutingFlowRow
	emptySnapshot bool
}

func NewFlowMinute(minute int64, edges [8]int64) *FlowMinute {
	return &FlowMinute{minute: minute, edges: edges}
}

func NewFlowSnapshot(minute int64, rows []repository.RoutingFlowRow) *FlowMinute {
	cp := make([]repository.RoutingFlowRow, len(rows))
	for i := range rows {
		cp[i] = rows[i]
		if rows[i].PreviousAccountID != nil {
			v := *rows[i].PreviousAccountID
			cp[i].PreviousAccountID = &v
		}
	}
	return &FlowMinute{minute: minute, flowRows: cp}
}

func NewEmptyFlowSnapshot(minute int64) *FlowMinute {
	return &FlowMinute{minute: minute, emptySnapshot: true}
}

// flowEdgeIdentity is superseded by the packed attemptFact key (fold_fact.go):
// identity is codes plus fixed-size byte arrays, never strings.

func (f *FlowMinute) Minute() int64 { return f.minute }
func (f *FlowMinute) Edge(i int) int64 {
	if i < 0 || i >= 8 {
		return 0
	}
	return f.edges[i]
}
func (f *FlowMinute) SetEdge(i int, v int64) {
	if i >= 0 && i < 8 {
		f.edges[i] = v
	}
}
func (f *FlowMinute) Count(i int) int64 {
	if i < 0 || i >= 8 {
		return 0
	}
	return f.counts[i]
}
func (f *FlowMinute) SetCount(i int, v int64) {
	if i >= 0 && i < 8 {
		f.counts[i] = v
	}
}
func (f *FlowMinute) Edges() [8]int64     { return f.edges }
func (f *FlowMinute) SetEdges(e [8]int64) { f.edges = e }
func (f *FlowMinute) Counts() [8]int64    { return f.counts }
func (f *FlowMinute) FlowRows() []repository.RoutingFlowRow {
	if f == nil {
		return nil
	}
	cp := make([]repository.RoutingFlowRow, len(f.flowRows))
	for i := range f.flowRows {
		cp[i] = f.flowRows[i]
		if f.flowRows[i].PreviousAccountID != nil {
			v := *f.flowRows[i].PreviousAccountID
			cp[i].PreviousAccountID = &v
		}
	}
	return cp
}
func (f *FlowMinute) SetFlowRows(rows []repository.RoutingFlowRow) {
	if f == nil {
		return
	}
	cp := make([]repository.RoutingFlowRow, len(rows))
	for i := range rows {
		cp[i] = rows[i]
		if rows[i].PreviousAccountID != nil {
			v := *rows[i].PreviousAccountID
			cp[i].PreviousAccountID = &v
		}
	}
	f.flowRows = cp
	f.emptySnapshot = false
}
func (f *FlowMinute) IsEmptySnapshot() bool { return f != nil && f.emptySnapshot }
func (f *FlowMinute) HasFlowRows() bool     { return f != nil && (len(f.flowRows) > 0 || f.emptySnapshot) }
func (f *FlowMinute) Clone() *FlowMinute {
	cp := *f
	if f.flowRows != nil {
		cp.flowRows = make([]repository.RoutingFlowRow, len(f.flowRows))
		for i := range f.flowRows {
			cp.flowRows[i] = f.flowRows[i]
			if f.flowRows[i].PreviousAccountID != nil {
				v := *f.flowRows[i].PreviousAccountID
				cp.flowRows[i].PreviousAccountID = &v
			}
		}
	}
	return &cp
}

type Snapshot struct {
	Quality map[int64]map[Key]*QualityMinute
	Flow    map[int64]*FlowMinute
}

var ErrCapacity = errors.New("quality recorder capacity exceeded")
var errInvalidMaxInflight = errInvalid("invalid max inflight")

type errInvalid string

func (e errInvalid) Error() string { return string(e) }

type Recorder struct {
	id                   uint64
	genSeq               atomic.Uint64
	effectiveMaxInflight int64
	mu                   sync.Mutex
	now                  func() time.Time
	active               map[Key]*Cell
	retired              []*Cell
	retiredIndex         map[Key]int
	qualityOverflow      atomic.Int64
	flowOverflow         atomic.Int64
	minuteOverflow       atomic.Int64
	pendingQuality       map[int64]map[Key]*QualityMinute
	// pendingFlow 已迁入 FlowOwner（async-routing-quality-telemetry）：跨请求
	// flow 累计器、同分钟身份归并与提交流水线的唯一 state owner 是 owner，
	// Recorder 只保留 quality 面。flowOverflow/minuteOverflow 仍是 lane 级
	// 原子计数，owner 的淘汰路径在此增计。
	pendingBytes    atomic.Int64
	retiredCap      int
	pendingCapBytes int64
	minuteCap       int
	admission       atomic.Uint64
	zeroCh          chan struct{}
	finalSnapshot   atomic.Pointer[Snapshot]
	flow            *FlowOwner
}

func NewRecorder(effectiveMaxInflight int64) (*Recorder, error) {
	if effectiveMaxInflight <= 0 {
		return nil, errInvalidMaxInflight
	}
	r := &Recorder{
		id:                   globalRecorderID.Add(1),
		effectiveMaxInflight: effectiveMaxInflight,
		now:                  time.Now,
		active:               make(map[Key]*Cell),
		retired:              make([]*Cell, 0),
		retiredIndex:         make(map[Key]int),
		pendingQuality:       make(map[int64]map[Key]*QualityMinute),
		retiredCap:           DefaultRetiredCap,
		pendingCapBytes:      DefaultPendingCapBytes,
		minuteCap:            DefaultMinuteBucketsCap,
	}
	r.flow = newFlowOwner(r)
	return r, nil
}

// FlowOwner returns the managed worker exclusively owning this recorder's
// flow lane: register it (worker.Worker) and publish it (StatsProvider)
// beside quality-sync; reverse shutdown then closes quality-sync first and
// the owner second (the refill dependency).
func (r *Recorder) FlowOwner() *FlowOwner { return r.flow }

func (r *Recorder) EffectiveMaxInflight() int64 { return r.effectiveMaxInflight }

func (r *Recorder) isClosed() bool { return r.admission.Load()&closedBit != 0 }

// finalized is the lock-free fence: closed admission or a stored final
// snapshot rejects every further enqueue/submit.
func (r *Recorder) finalized() bool { return r.isClosed() || r.finalSnapshot.Load() != nil }

func (r *Recorder) tryIncAdmission() bool {
	for {
		a := r.admission.Load()
		if a&closedBit != 0 {
			return false
		}
		inflight := a & inflightMask
		if int64(inflight) >= r.effectiveMaxInflight {
			return false
		}
		if r.admission.CompareAndSwap(a, a+1) {
			return true
		}
	}
}

func (r *Recorder) decAdmissionAndMaybeSignal() {
	newA := r.admission.Add(^uint64(0))
	if newA&closedBit != 0 && newA&inflightMask == 0 {
		r.mu.Lock()
		if r.zeroCh != nil {
			close(r.zeroCh)
			r.zeroCh = nil
		}
		r.mu.Unlock()
		r.ensureFinalSnapshot()
	}
}

func (r *Recorder) GetOrCreateCell(key Key) *Cell {
	if r.isClosed() {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.isClosed() {
		return nil
	}
	if c, ok := r.active[key]; ok {
		return c
	}
	c := &Cell{key: key, owner: r.id, gen: r.genSeq.Add(1)}
	r.active[key] = c
	return c
}

func (r *Recorder) InitAttemptContext(cell *Cell, out *AttemptContext) bool {
	if cell == nil || out == nil {
		return false
	}
	if cell.gen == 0 {
		out.cell = nil
		out.recorder = nil
		out.gen = 0
		atomic.StoreUint32(&out.done, 0)
		return false
	}
	if r.isClosed() {
		out.cell = nil
		out.recorder = nil
		out.gen = 0
		atomic.StoreUint32(&out.done, 0)
		return false
	}
	if r.finalSnapshot.Load() != nil {
		out.cell = nil
		out.recorder = nil
		out.gen = 0
		atomic.StoreUint32(&out.done, 0)
		return false
	}
	if !r.tryIncAdmission() {
		out.cell = nil
		out.recorder = nil
		out.gen = 0
		atomic.StoreUint32(&out.done, 0)
		return false
	}
	if cell.owner != r.id {
		r.decAdmissionAndMaybeSignal()
		out.cell = nil
		out.recorder = nil
		out.gen = 0
		atomic.StoreUint32(&out.done, 0)
		return false
	}
	if cell.gen == 0 {
		r.decAdmissionAndMaybeSignal()
		out.cell = nil
		out.recorder = nil
		out.gen = 0
		atomic.StoreUint32(&out.done, 0)
		return false
	}
	if !cell.tryPin() {
		r.decAdmissionAndMaybeSignal()
		out.cell = nil
		out.recorder = nil
		out.gen = 0
		atomic.StoreUint32(&out.done, 0)
		return false
	}
	out.cell = cell
	out.recorder = r
	out.gen = cell.gen
	atomic.StoreUint32(&out.done, 0)
	return true
}

func (r *Recorder) NewAttemptContext(cell *Cell) *AttemptContext {
	if cell == nil || cell.gen == 0 || r.isClosed() || r.finalSnapshot.Load() != nil {
		return &AttemptContext{recorder: r}
	}
	if !r.tryIncAdmission() {
		return &AttemptContext{recorder: r}
	}
	if cell.owner != r.id || cell.gen == 0 {
		r.decAdmissionAndMaybeSignal()
		return &AttemptContext{recorder: r}
	}
	if !cell.tryPin() {
		r.decAdmissionAndMaybeSignal()
		return &AttemptContext{recorder: r}
	}
	return &AttemptContext{cell: cell, recorder: r, gen: cell.gen}
}

func (r *Recorder) Begin(key Key) *AttemptContext {
	if r.isClosed() || r.finalSnapshot.Load() != nil {
		return &AttemptContext{recorder: r}
	}
	if !r.tryIncAdmission() {
		return &AttemptContext{recorder: r}
	}
	cell := r.GetOrCreateCell(key)
	if cell == nil || cell.gen == 0 {
		r.decAdmissionAndMaybeSignal()
		return &AttemptContext{recorder: r}
	}
	if !cell.tryPin() {
		r.decAdmissionAndMaybeSignal()
		return &AttemptContext{recorder: r}
	}
	return &AttemptContext{cell: cell, recorder: r, gen: cell.gen}
}

// distinctMinuteCountLocked counts quality-lane buckets. The flow lane lives
// in the FlowOwner with an independent (but identically bounded) budget; the
// two lanes never evict each other and no request goroutine ever waits on
// flow reduction.
func (r *Recorder) distinctMinuteCountLocked() int {
	return len(r.pendingQuality)
}

func (r *Recorder) tryEvictQualityLocked() bool {
	var oldestMinute int64
	var oldestKey Key
	found := false
	for minute, rows := range r.pendingQuality {
		for k := range rows {
			if !found || minute < oldestMinute {
				oldestMinute = minute
				oldestKey = k
				found = true
			}
		}
	}
	if !found {
		return false
	}
	rows := r.pendingQuality[oldestMinute]
	delete(rows, oldestKey)
	if len(rows) == 0 {
		delete(r.pendingQuality, oldestMinute)
		r.minuteOverflow.Add(1)
	}
	r.pendingBytes.Add(-EstimatedQualityRowBytes)
	r.qualityOverflow.Add(1)
	return true
}

func (r *Recorder) AddQualityRow(minute int64, key Key) error {
	return r.EnqueueQualityMinute(NewQualityMinute(minute, key))
}

func (r *Recorder) EnqueueQualityMinute(qm *QualityMinute) error {
	if qm == nil {
		return errors.New("nil quality minute")
	}
	if r.isClosed() || r.finalSnapshot.Load() != nil {
		return ErrCapacity
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.isClosed() || r.finalSnapshot.Load() != nil {
		return ErrCapacity
	}
	return r.enqueueQualityMinuteLocked(qm)
}

// enqueueQualityMinuteLocked stores a canonical absolute quality row. Duplicate
// (minute, key) rows merge so no counted stats are lost. The conservative fixed
// charge is validated and eviction pressure applied BEFORE the insert, so an
// rejected row leaves the pending state untouched (no false drop). Quality
// eviction only frees quality rows: flow minutes are owned by the FlowOwner
// and evicted on that lane's own pressure.
func (r *Recorder) enqueueQualityMinuteLocked(qm *QualityMinute) error {
	if rows, ok := r.pendingQuality[qm.minute]; ok {
		if existing, ok2 := rows[qm.key]; ok2 {
			existing.merge(qm)
			return nil
		}
	}
	charge := int64(EstimatedQualityRowBytes)
	if charge > r.pendingCapBytes {
		return ErrCapacity
	}
	_, inQ := r.pendingQuality[qm.minute]
	newMinute := !inQ
	for r.pendingBytes.Load()+charge > r.pendingCapBytes || (newMinute && r.distinctMinuteCountLocked() >= r.minuteCap) {
		if !r.tryEvictQualityLocked() {
			return ErrCapacity
		}
	}
	if _, ok := r.pendingQuality[qm.minute]; !ok {
		r.pendingQuality[qm.minute] = make(map[Key]*QualityMinute)
	}
	r.pendingQuality[qm.minute][qm.key] = qm.Clone()
	r.pendingBytes.Add(charge)
	return nil
}

func (r *Recorder) AddFlowMinute(minute int64, edges [8]int64) error {
	return r.EnqueueFlowMinute(NewFlowMinute(minute, edges))
}

// EnqueueFlowMinute delegates the absolute flow-minute merge to the
// FlowOwner (the sole state owner): the legacy edges/empty-marker forms and
// the quality-sync failure refill enter the accumulator through this
// ownership-transfer API, never by aliasing owner maps.
func (r *Recorder) EnqueueFlowMinute(fm *FlowMinute) error {
	if fm == nil {
		return errors.New("nil flow minute")
	}
	if r.finalized() {
		return ErrCapacity
	}
	return r.flow.enqueue(fm)
}

func (r *Recorder) FlowMinute(minute int64) (*FlowMinute, bool) {
	return r.flow.lookup(minute)
}

func (r *Recorder) QualityMinute(minute int64, key Key) (*QualityMinute, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.pendingQuality[minute]
	if !ok {
		return nil, false
	}
	qm, ok := m[key]
	if !ok {
		return nil, false
	}
	return qm.Clone(), true
}

// convergeIfReclaimableLocked exports a fully completed cell's lifetime totals
// as one canonical absolute QualityMinute row, exactly once per cell lifetime.
// The row minute is the wall-clock minute of the convergence. If the pending
// bounds reject the row, the data is dropped and counted in qualityOverflow.
func (r *Recorder) convergeIfReclaimableLocked(c *Cell) {
	if r.finalSnapshot.Load() != nil {
		return
	}
	if !c.isRetired() || !c.isReclaimable() || c.converged {
		return
	}
	c.converged = true
	if c.attempts.Load() == 0 {
		return
	}
	minute := r.now().UTC().Truncate(time.Minute).Unix()
	qm := NewQualityMinute(minute, c.key)
	qm.attempts = c.attempts.Load()
	qm.successes = c.successes.Load()
	qm.err429 = c.errClasses[ErrClass429].Load()
	qm.err4xx = c.errClasses[ErrClass4xx].Load()
	qm.err5xx = c.errClasses[ErrClass5xx].Load()
	qm.errNetwork = c.errClasses[ErrClassNetwork].Load()
	qm.ttftCount = c.ttftCount.Load()
	qm.sumQ32 = c.sumQ32.Load()
	qm.sumSqQ32 = c.sumSq.Load()
	for i := range qm.hist {
		qm.hist[i] = c.hist[i].Load()
	}
	qm.inputTokens = c.inputTokens.Load()
	qm.outputTokens = c.outputTokens.Load()
	qm.cacheRead = c.cacheRead.Load()
	qm.cacheCreate = c.cacheCreate.Load()
	qm.calls = c.calls.Load()
	qm.images = c.images.Load()
	if err := r.enqueueQualityMinuteLocked(qm); err != nil {
		r.qualityOverflow.Add(1)
	}
}

func (r *Recorder) unpinnedRetiredCountLocked() int {
	n := 0
	for _, c := range r.retired {
		if c.isReclaimable() {
			n++
		}
	}
	return n
}

func (r *Recorder) SweepReclaim() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for r.unpinnedRetiredCountLocked() > r.retiredCap {
		idx := -1
		for i, c := range r.retired {
			if c.isReclaimable() {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		v := r.retired[idx]
		r.convergeIfReclaimableLocked(v)
		delete(r.retiredIndex, v.key)
		r.retired = append(r.retired[:idx], r.retired[idx+1:]...)
		for i := idx; i < len(r.retired); i++ {
			r.retiredIndex[r.retired[i].key] = i
		}
		r.qualityOverflow.Add(1)
		n++
	}
	return n
}

func (r *Recorder) Retire(key Key) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.isClosed() || r.finalSnapshot.Load() != nil {
		return
	}
	cell, ok := r.active[key]
	if !ok {
		return
	}
	cell.retireCAS()
	delete(r.active, key)
	r.retired = append(r.retired, cell)
	r.retiredIndex[key] = len(r.retired) - 1
	r.convergeIfReclaimableLocked(cell)
	r.SweepReclaimLocked()
}

func (r *Recorder) SweepReclaimLocked() {
	for r.unpinnedRetiredCountLocked() > r.retiredCap {
		idx := -1
		for i, c := range r.retired {
			if c.isReclaimable() {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		v := r.retired[idx]
		r.convergeIfReclaimableLocked(v)
		delete(r.retiredIndex, v.key)
		r.retired = append(r.retired[:idx], r.retired[idx+1:]...)
		for i := idx; i < len(r.retired); i++ {
			r.retiredIndex[r.retired[i].key] = i
		}
		r.qualityOverflow.Add(1)
	}
}

func (r *Recorder) ActiveCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.active)
}

func (r *Recorder) RetiredCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.retired)
}

func (r *Recorder) UnpinnedRetiredCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.unpinnedRetiredCountLocked()
}

func (r *Recorder) PinnedRetiredCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.retired {
		if !c.isReclaimable() && c.isRetired() {
			n++
		}
	}
	return n
}

func (r *Recorder) QualityOverflow() int64 { return r.qualityOverflow.Load() }
func (r *Recorder) FlowOverflow() int64    { return r.flowOverflow.Load() }
func (r *Recorder) MinuteOverflow() int64  { return r.minuteOverflow.Load() }
func (r *Recorder) PendingBytes() int64 {
	return r.pendingBytes.Load() + r.flow.pendingBytesSnapshot()
}

// MinuteBucketCount reports the total distinct pending minute buckets across
// both lanes (quality under rec.mu, flow under the owner lock — acquired
// sequentially, never nested).
func (r *Recorder) MinuteBucketCount() int {
	r.mu.Lock()
	q := len(r.pendingQuality)
	r.mu.Unlock()
	return q + r.flow.minuteCount()
}
func (r *Recorder) PinnedGauge() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.retired {
		if !c.isReclaimable() && c.isRetired() {
			n++
		}
	}
	return int64(n)
}
func (r *Recorder) GlobalInflight() int64 { return int64(r.admission.Load() & inflightMask) }

func (r *Recorder) CellStats(key Key) (attempts, successes, ttftCount, tokens, calls, images int64, sumQ32, sumSq int64, hist [10]int64, errClasses [4]int64, ok bool) {
	r.mu.Lock()
	var cell *Cell
	if c, ok2 := r.active[key]; ok2 {
		cell = c
		ok = true
	} else if idx, ok2 := r.retiredIndex[key]; ok2 {
		cell = r.retired[idx]
		ok = true
	}
	r.mu.Unlock()
	if !ok || cell == nil {
		return 0, 0, 0, 0, 0, 0, 0, 0, [10]int64{}, [4]int64{}, false
	}
	var h [10]int64
	for i := range h {
		h[i] = cell.hist[i].Load()
	}
	var ec [4]int64
	for i := range ec {
		ec[i] = cell.errClasses[i].Load()
	}
	tok := cell.tokens.Load()
	if tok == 0 {
		tok = cell.inputTokens.Load() + cell.outputTokens.Load() + cell.cacheRead.Load() + cell.cacheCreate.Load()
	}
	return cell.attempts.Load(), cell.successes.Load(), cell.ttftCount.Load(), tok, cell.calls.Load(), cell.images.Load(), cell.sumQ32.Load(), cell.sumSq.Load(), h, ec, true
}

func (r *Recorder) CellStatsDetailed(key Key) (attempts, successes, ttftCount int64, input, output, cacheRead, cacheCreate, calls, images int64, sumQ32, sumSq int64, hist [10]int64, errClasses [4]int64, ok bool) {
	r.mu.Lock()
	var cell *Cell
	if c, ok2 := r.active[key]; ok2 {
		cell = c
		ok = true
	} else if idx, ok2 := r.retiredIndex[key]; ok2 {
		cell = r.retired[idx]
		ok = true
	}
	r.mu.Unlock()
	if !ok || cell == nil {
		return 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, [10]int64{}, [4]int64{}, false
	}
	var h [10]int64
	for i := range h {
		h[i] = cell.hist[i].Load()
	}
	var ec [4]int64
	for i := range ec {
		ec[i] = cell.errClasses[i].Load()
	}
	return cell.attempts.Load(), cell.successes.Load(), cell.ttftCount.Load(), cell.inputTokens.Load(), cell.outputTokens.Load(), cell.cacheRead.Load(), cell.cacheCreate.Load(), cell.calls.Load(), cell.images.Load(), cell.sumQ32.Load(), cell.sumSq.Load(), h, ec, true
}

func (r *Recorder) cellQualityMinute(c *Cell, minute int64) *QualityMinute {
	qm := NewQualityMinute(minute, c.key)
	qm.attempts = c.attempts.Load()
	qm.successes = c.successes.Load()
	qm.err429 = c.errClasses[ErrClass429].Load()
	qm.err4xx = c.errClasses[ErrClass4xx].Load()
	qm.err5xx = c.errClasses[ErrClass5xx].Load()
	qm.errNetwork = c.errClasses[ErrClassNetwork].Load()
	qm.ttftCount = c.ttftCount.Load()
	qm.sumQ32 = c.sumQ32.Load()
	qm.sumSqQ32 = c.sumSq.Load()
	for i := range qm.hist {
		qm.hist[i] = c.hist[i].Load()
	}
	qm.inputTokens = c.inputTokens.Load()
	qm.outputTokens = c.outputTokens.Load()
	qm.cacheRead = c.cacheRead.Load()
	qm.cacheCreate = c.cacheCreate.Load()
	qm.calls = c.calls.Load()
	qm.images = c.images.Load()
	return qm
}

// snapshotQualityLocked projects the quality lane (pending rows plus the
// active/retired cell projections). The flow lane is composed separately via
// the FlowOwner snapshot API — rec.mu and owner.mu are never nested, so
// snapshotLocked cannot build the Flow map here.
func (r *Recorder) snapshotQualityLocked() map[int64]map[Key]*QualityMinute {
	qCopy := make(map[int64]map[Key]*QualityMinute)
	for m, rows := range r.pendingQuality {
		cp := make(map[Key]*QualityMinute)
		for k, v := range rows {
			cp[k] = v.Clone()
		}
		qCopy[m] = cp
	}
	minute := r.now().UTC().Truncate(time.Minute).Unix()
	for _, c := range r.active {
		if c.attempts.Load() == 0 {
			continue
		}
		qm := r.cellQualityMinute(c, minute)
		if rows, ok := qCopy[minute]; ok {
			if existing, ok2 := rows[c.key]; ok2 {
				existing.merge(qm)
				continue
			}
			rows[c.key] = qm
		} else {
			qCopy[minute] = map[Key]*QualityMinute{c.key: qm}
		}
	}
	for _, c := range r.retired {
		if c.converged {
			continue
		}
		if c.attempts.Load() == 0 {
			continue
		}
		qm := r.cellQualityMinute(c, minute)
		if rows, ok := qCopy[minute]; ok {
			if existing, ok2 := rows[c.key]; ok2 {
				existing.merge(qm)
				continue
			}
			rows[c.key] = qm
		} else {
			qCopy[minute] = map[Key]*QualityMinute{c.key: qm}
		}
	}
	return qCopy
}

// buildSnapshot composes both lanes with sequential (never nested) locking.
func (r *Recorder) buildSnapshot() *Snapshot {
	r.mu.Lock()
	q := r.snapshotQualityLocked()
	r.mu.Unlock()
	return &Snapshot{Quality: q, Flow: r.flow.snapshotAll()}
}

// ensureFinalSnapshot freezes the final snapshot exactly once (CAS: the
// first builder wins, late builders are no-ops).
func (r *Recorder) ensureFinalSnapshot() {
	if r.finalSnapshot.Load() != nil {
		return
	}
	r.finalSnapshot.CompareAndSwap(nil, r.buildSnapshot())
}

func (r *Recorder) ExportSnapshot() (map[int64]map[Key]*QualityMinute, map[int64]*FlowMinute) {
	snap := r.Snapshot()
	return snap.Quality, snap.Flow
}

// LiveCells returns cloned cumulative totals of all active cells (minute=0
// marker; zero-attempt cells carry no signal and are skipped). Compile-lane
// read-only accessor: called from the background routing-compile lane under
// the recorder mutex — never from the request path. Clones never alias live
// cells, so the caller may hold the map across cell mutation.
func (r *Recorder) LiveCells() map[Key]*QualityMinute {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[Key]*QualityMinute, len(r.active))
	for k, c := range r.active {
		if c.attempts.Load() == 0 {
			continue
		}
		out[k] = r.cellQualityMinute(c, 0)
	}
	return out
}

// Snapshot returns a fully cloned both-lane view: the finalized snapshot is
// deep-copied on every read, live reads compose quality (rec.mu) and the
// FlowOwner accumulator (drained first) with sequential locking.
func (r *Recorder) Snapshot() *Snapshot {
	if snap := r.finalSnapshot.Load(); snap != nil {
		qCopy := make(map[int64]map[Key]*QualityMinute, len(snap.Quality))
		for m, rows := range snap.Quality {
			cp := make(map[Key]*QualityMinute, len(rows))
			for k, v := range rows {
				cp[k] = v.Clone()
			}
			qCopy[m] = cp
		}
		fCopy := make(map[int64]*FlowMinute, len(snap.Flow))
		for m, v := range snap.Flow {
			fCopy[m] = v.Clone()
		}
		return &Snapshot{Quality: qCopy, Flow: fCopy}
	}
	return r.buildSnapshot()
}

func (r *Recorder) Close() error {
	return r.CloseWithContext(context.Background())
}

func (r *Recorder) CloseWithContext(ctx context.Context) error {
	for {
		a := r.admission.Load()
		if a&closedBit != 0 {
			break
		}
		if r.admission.CompareAndSwap(a, a|closedBit) {
			break
		}
	}
	if r.finalSnapshot.Load() != nil {
		return nil
	}
	r.mu.Lock()
	var ch chan struct{}
	waiting := false
	if r.admission.Load()&inflightMask == 0 {
		if r.zeroCh != nil {
			close(r.zeroCh)
			r.zeroCh = nil
		}
	} else {
		if r.zeroCh == nil {
			r.zeroCh = make(chan struct{})
		}
		ch = r.zeroCh
		waiting = true
	}
	r.mu.Unlock()
	if waiting {
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// Finalization runs after the last lock release: buildSnapshot takes the
	// owner lock, so it must not be nested under r.mu. A racing inflight
	// zero-crossing calls ensureFinalSnapshot too; the CAS keeps one winner.
	r.ensureFinalSnapshot()
	return nil
}

type AttemptContext struct {
	cell     *Cell
	recorder *Recorder
	gen      uint64
	done     uint32
}

func (a *AttemptContext) IsZero() bool { return a == nil || a.cell == nil }

func (a *AttemptContext) completeCommon(obs Observation) {
	if a == nil || a.cell == nil || a.recorder == nil || a.gen == 0 || a.gen != a.cell.gen || a.cell.gen == 0 {
		return
	}
	if !atomic.CompareAndSwapUint32(&a.done, 0, 1) {
		return
	}
	if a.gen == 0 || a.gen != a.cell.gen || a.cell.gen == 0 {
		return
	}
	c := a.cell
	r := a.recorder
	if obs.IsCancel || obs.IsLocal || obs.IsReservation {
		c.state.Add(^uint64(0))
		newA := r.admission.Add(^uint64(0))
		if newA&closedBit != 0 && newA&inflightMask == 0 {
			r.mu.Lock()
			if r.zeroCh != nil {
				close(r.zeroCh)
				r.zeroCh = nil
			}
			r.mu.Unlock()
			r.ensureFinalSnapshot()
		}
		if c.isRetired() && c.isReclaimable() {
			r.mu.Lock()
			r.convergeIfReclaimableLocked(c)
			r.SweepReclaimLocked()
			r.mu.Unlock()
		}
		return
	}
	c.attempts.Add(1)
	if obs.Success {
		c.successes.Add(1)
		if obs.TTFTMs != nil {
			v := *obs.TTFTMs
			if v < 1 {
				v = 1
			}
			c.ttftCount.Add(1)
			c.sumQ32.Add(toQ32(v))
			c.sumSq.Add(toSqQ32(v))
			c.hist[histIndex(v)].Add(1)
		}
	} else {
		if obs.ErrClass >= 0 && obs.ErrClass < 4 {
			c.errClasses[obs.ErrClass].Add(1)
		} else {
			c.errClasses[ErrClass4xx].Add(1)
		}
	}
	if obs.InputTokens != 0 {
		c.inputTokens.Add(obs.InputTokens)
		c.tokens.Add(obs.InputTokens)
	}
	if obs.OutputTokens != 0 {
		c.outputTokens.Add(obs.OutputTokens)
		c.tokens.Add(obs.OutputTokens)
	}
	if obs.CacheReadTokens != 0 {
		c.cacheRead.Add(obs.CacheReadTokens)
		c.tokens.Add(obs.CacheReadTokens)
	}
	if obs.CacheCreationTokens != 0 {
		c.cacheCreate.Add(obs.CacheCreationTokens)
		c.tokens.Add(obs.CacheCreationTokens)
	}
	if obs.Tokens != 0 && obs.InputTokens == 0 && obs.OutputTokens == 0 && obs.CacheReadTokens == 0 && obs.CacheCreationTokens == 0 {
		c.inputTokens.Add(obs.Tokens)
		c.tokens.Add(obs.Tokens)
	}
	if obs.Calls != 0 {
		c.calls.Add(obs.Calls)
	}
	if obs.Images != 0 {
		c.images.Add(obs.Images)
	}
	c.state.Add(^uint64(0))
	newA := r.admission.Add(^uint64(0))
	if newA&closedBit != 0 && newA&inflightMask == 0 {
		r.mu.Lock()
		if r.zeroCh != nil {
			close(r.zeroCh)
			r.zeroCh = nil
		}
		r.mu.Unlock()
		r.ensureFinalSnapshot()
	}
	if c.isRetired() && c.isReclaimable() {
		r.mu.Lock()
		r.convergeIfReclaimableLocked(c)
		r.SweepReclaimLocked()
		r.mu.Unlock()
	}
}

func (a *AttemptContext) Complete(success bool, ttftMs *int64, tokens, calls, images int64) {
	obs := Observation{Success: success, TTFTMs: ttftMs, Tokens: tokens, Calls: calls, Images: images}
	if !success {
		obs.ErrClass = ErrClass4xx
	} else {
		obs.ErrClass = ErrClassNone
	}
	a.completeCommon(obs)
}

func (a *AttemptContext) CompleteObservation(obs Observation) {
	a.completeCommon(obs)
}

func (a *AttemptContext) Cancel() {
	if a == nil || a.cell == nil || a.recorder == nil || a.gen == 0 || a.gen != a.cell.gen || a.cell.gen == 0 {
		return
	}
	if !atomic.CompareAndSwapUint32(&a.done, 0, 1) {
		return
	}
	if a.gen == 0 || a.gen != a.cell.gen || a.cell.gen == 0 {
		return
	}
	a.cell.state.Add(^uint64(0))
	newA := a.recorder.admission.Add(^uint64(0))
	if newA&closedBit != 0 && newA&inflightMask == 0 {
		a.recorder.mu.Lock()
		if a.recorder.zeroCh != nil {
			close(a.recorder.zeroCh)
			a.recorder.zeroCh = nil
		}
		a.recorder.mu.Unlock()
		a.recorder.ensureFinalSnapshot()
	}
	if a.cell.isRetired() && a.cell.isReclaimable() {
		a.recorder.mu.Lock()
		a.recorder.convergeIfReclaimableLocked(a.cell)
		a.recorder.SweepReclaimLocked()
		a.recorder.mu.Unlock()
	}
}

func (a *AttemptContext) IsCompleted() bool {
	if a == nil {
		return false
	}
	return atomic.LoadUint32(&a.done) == 1
}
