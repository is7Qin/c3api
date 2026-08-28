// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
)

const (
	DefaultRetiredCap        = 65536
	DefaultPendingCapBytes   = 256 * 1024 * 1024
	DefaultMinuteBucketsCap  = 4096
	EstimatedCellBytes       = 256
	EstimatedQualityRowBytes = 256
	EstimatedFlowMinuteBytes = 4096
	q32Scale                 = 1 << 32
	retiredBit               = uint64(1) << 63
	closedBit                = uint64(1) << 63
	inflightMask             = ^closedBit
)

type Key struct {
	FP [32]byte
	QC [32]byte
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
	IsMalformed         bool
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
	state        atomic.Uint64
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

type FlowMinute struct {
	minute int64
	edges  [8]int64
	counts [8]int64
}

func NewFlowMinute(minute int64, edges [8]int64) *FlowMinute {
	return &FlowMinute{minute: minute, edges: edges}
}
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
func (f *FlowMinute) Clone() *FlowMinute {
	cp := *f
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
	effectiveMaxInflight int64
	mu                   sync.Mutex
	active               map[Key]*Cell
	retired              []*Cell
	retiredIndex         map[Key]int
	qualityOverflow      atomic.Int64
	flowOverflow         atomic.Int64
	minuteOverflow       atomic.Int64
	pendingQuality       map[int64]map[Key]*QualityMinute
	pendingFlow          map[int64]*FlowMinute
	pendingBytes         atomic.Int64
	retiredCap           int
	pendingCapBytes      int64
	minuteCap            int
	admission            atomic.Uint64
	zeroCh               chan struct{}
	finalSnapshot        atomic.Pointer[Snapshot]
}

func NewRecorder(effectiveMaxInflight int64) (*Recorder, error) {
	if effectiveMaxInflight <= 0 {
		return nil, errInvalidMaxInflight
	}
	return &Recorder{
		effectiveMaxInflight: effectiveMaxInflight,
		active:               make(map[Key]*Cell),
		retired:              make([]*Cell, 0),
		retiredIndex:         make(map[Key]int),
		pendingQuality:       make(map[int64]map[Key]*QualityMinute),
		pendingFlow:          make(map[int64]*FlowMinute),
		retiredCap:           DefaultRetiredCap,
		pendingCapBytes:      DefaultPendingCapBytes,
		minuteCap:            DefaultMinuteBucketsCap,
	}, nil
}

func (r *Recorder) EffectiveMaxInflight() int64 { return r.effectiveMaxInflight }

func (r *Recorder) isClosed() bool { return r.admission.Load()&closedBit != 0 }

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
		if r.finalSnapshot.Load() == nil {
			snap := r.snapshotLocked()
			r.finalSnapshot.Store(snap)
		}
		r.mu.Unlock()
	}
}

func (r *Recorder) GetOrCreateCell(fp [32]byte, qc [32]byte) *Cell {
	if r.isClosed() {
		return nil
	}
	key := Key{FP: fp, QC: qc}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.isClosed() {
		return nil
	}
	if c, ok := r.active[key]; ok {
		return c
	}
	if idx, ok := r.retiredIndex[key]; ok {
		c := r.retired[idx]
		if c.isRetired() && !c.isReclaimable() {
			return nil
		}
		if c.isReclaimable() {
			delete(r.retiredIndex, key)
			r.retired = append(r.retired[:idx], r.retired[idx+1:]...)
			for i := idx; i < len(r.retired); i++ {
				r.retiredIndex[r.retired[i].key] = i
			}
			r.active[key] = c
			c.state.Store(0)
			return c
		}
		delete(r.retiredIndex, key)
		r.retired = append(r.retired[:idx], r.retired[idx+1:]...)
		for i := idx; i < len(r.retired); i++ {
			r.retiredIndex[r.retired[i].key] = i
		}
		r.active[key] = c
		c.state.Store(0)
		return c
	}
	c := &Cell{key: key}
	r.active[key] = c
	return c
}

func (r *Recorder) InitAttemptContext(cell *Cell, out *AttemptContext) bool {
	if cell == nil || out == nil {
		return false
	}
	if r.isClosed() {
		out.cell = nil
		out.recorder = nil
		atomic.StoreUint32(&out.done, 0)
		return false
	}
	if r.finalSnapshot.Load() != nil {
		out.cell = nil
		out.recorder = nil
		atomic.StoreUint32(&out.done, 0)
		return false
	}
	if !r.tryIncAdmission() {
		out.cell = nil
		out.recorder = nil
		atomic.StoreUint32(&out.done, 0)
		return false
	}
	if !cell.tryPin() {
		r.decAdmissionAndMaybeSignal()
		out.cell = nil
		out.recorder = nil
		atomic.StoreUint32(&out.done, 0)
		return false
	}
	out.cell = cell
	out.recorder = r
	atomic.StoreUint32(&out.done, 0)
	return true
}

func (r *Recorder) NewAttemptContext(cell *Cell) *AttemptContext {
	if cell == nil || r.isClosed() || r.finalSnapshot.Load() != nil {
		return &AttemptContext{recorder: r}
	}
	if !r.tryIncAdmission() {
		return &AttemptContext{recorder: r}
	}
	if !cell.tryPin() {
		r.decAdmissionAndMaybeSignal()
		return &AttemptContext{recorder: r}
	}
	return &AttemptContext{cell: cell, recorder: r}
}

func (r *Recorder) Begin(fp [32]byte, qc [32]byte) *AttemptContext {
	if r.isClosed() || r.finalSnapshot.Load() != nil {
		return &AttemptContext{recorder: r}
	}
	if !r.tryIncAdmission() {
		return &AttemptContext{recorder: r}
	}
	cell := r.GetOrCreateCell(fp, qc)
	if cell == nil {
		r.decAdmissionAndMaybeSignal()
		return &AttemptContext{recorder: r}
	}
	if !cell.tryPin() {
		r.decAdmissionAndMaybeSignal()
		return &AttemptContext{recorder: r}
	}
	return &AttemptContext{cell: cell, recorder: r}
}

func (r *Recorder) distinctMinuteCountLocked() int {
	set := make(map[int64]struct{})
	for m := range r.pendingQuality {
		set[m] = struct{}{}
	}
	for m := range r.pendingFlow {
		set[m] = struct{}{}
	}
	return len(set)
}

func (r *Recorder) tryEvictQualityLocked() bool {
	var oldestMinute int64
	var oldestKey Key
	found := false
	first := true
	for minute, rows := range r.pendingQuality {
		for k := range rows {
			if first || minute < oldestMinute {
				oldestMinute = minute
				oldestKey = k
				found = true
				first = false
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
	}
	r.pendingBytes.Add(-EstimatedQualityRowBytes)
	r.qualityOverflow.Add(1)
	return true
}

func (r *Recorder) tryEvictFlowLocked() bool {
	if len(r.pendingFlow) == 0 {
		return false
	}
	var oldest int64
	first := true
	for m := range r.pendingFlow {
		if first || m < oldest {
			oldest = m
			first = false
		}
	}
	delete(r.pendingFlow, oldest)
	r.pendingBytes.Add(-EstimatedFlowMinuteBytes)
	r.flowOverflow.Add(1)
	r.minuteOverflow.Add(1)
	return true
}

func (r *Recorder) AddQualityRow(minute int64, fp [32]byte, qc [32]byte) error {
	return r.EnqueueQualityMinute(&QualityMinute{minute: minute, key: Key{FP: fp, QC: qc}})
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
	minute := qm.minute
	key := qm.key
	if rows, ok := r.pendingQuality[minute]; ok {
		if _, ok2 := rows[key]; ok2 {
			return nil
		}
	}
	cp := qm.Clone()
	if _, ok := r.pendingQuality[minute]; !ok {
		r.pendingQuality[minute] = make(map[Key]*QualityMinute)
	}
	r.pendingQuality[minute][key] = cp
	r.pendingBytes.Add(EstimatedQualityRowBytes)
	for r.pendingBytes.Load() > r.pendingCapBytes || r.distinctMinuteCountLocked() > r.minuteCap {
		removed := false
		if r.tryEvictQualityLocked() {
			removed = true
		} else if r.tryEvictFlowLocked() {
			removed = true
		}
		if !removed {
			r.pendingBytes.Add(-EstimatedQualityRowBytes)
			delete(r.pendingQuality[minute], key)
			if len(r.pendingQuality[minute]) == 0 {
				delete(r.pendingQuality, minute)
			}
			return ErrCapacity
		}
	}
	return nil
}

func (r *Recorder) AddFlowMinute(minute int64, edges [8]int64) error {
	return r.EnqueueFlowMinute(NewFlowMinute(minute, edges))
}

func (r *Recorder) EnqueueFlowMinute(fm *FlowMinute) error {
	if fm == nil {
		return errors.New("nil flow minute")
	}
	if r.isClosed() || r.finalSnapshot.Load() != nil {
		return ErrCapacity
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.isClosed() || r.finalSnapshot.Load() != nil {
		return ErrCapacity
	}
	minute := fm.minute
	if _, ok := r.pendingFlow[minute]; ok {
		return nil
	}
	cp := fm.Clone()
	r.pendingFlow[minute] = cp
	r.pendingBytes.Add(EstimatedFlowMinuteBytes)
	for r.pendingBytes.Load() > r.pendingCapBytes || r.distinctMinuteCountLocked() > r.minuteCap {
		removed := false
		if r.tryEvictQualityLocked() {
			removed = true
		} else if r.tryEvictFlowLocked() {
			removed = true
		}
		if !removed {
			r.pendingBytes.Add(-EstimatedFlowMinuteBytes)
			delete(r.pendingFlow, minute)
			return ErrCapacity
		}
	}
	return nil
}

func (r *Recorder) FlowMinute(minute int64) (*FlowMinute, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fm, ok := r.pendingFlow[minute]
	if !ok {
		return nil, false
	}
	return fm.Clone(), true
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

func (r *Recorder) tryConvergeLocked() {}

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

func (r *Recorder) Retire(fp [32]byte, qc [32]byte) {
	key := Key{FP: fp, QC: qc}
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
func (r *Recorder) PendingBytes() int64    { return r.pendingBytes.Load() }
func (r *Recorder) MinuteBucketCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.distinctMinuteCountLocked()
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

func (r *Recorder) CellStats(fp [32]byte, qc [32]byte) (attempts, successes, ttftCount, tokens, calls, images int64, sumQ32, sumSq int64, hist [10]int64, errClasses [4]int64, ok bool) {
	key := Key{FP: fp, QC: qc}
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

func (r *Recorder) CellStatsDetailed(fp [32]byte, qc [32]byte) (attempts, successes, ttftCount int64, input, output, cacheRead, cacheCreate, calls, images int64, sumQ32, sumSq int64, hist [10]int64, errClasses [4]int64, ok bool) {
	key := Key{FP: fp, QC: qc}
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

func (r *Recorder) snapshotLocked() *Snapshot {
	qCopy := make(map[int64]map[Key]*QualityMinute)
	for m, rows := range r.pendingQuality {
		cp := make(map[Key]*QualityMinute)
		for k, v := range rows {
			cp[k] = v.Clone()
		}
		qCopy[m] = cp
	}
	fCopy := make(map[int64]*FlowMinute)
	for m, v := range r.pendingFlow {
		fCopy[m] = v.Clone()
	}
	return &Snapshot{Quality: qCopy, Flow: fCopy}
}

func (r *Recorder) ExportSnapshot() (map[int64]map[Key]*QualityMinute, map[int64]*FlowMinute) {
	if snap := r.finalSnapshot.Load(); snap != nil {
		qCopy := make(map[int64]map[Key]*QualityMinute)
		for m, rows := range snap.Quality {
			cp := make(map[Key]*QualityMinute)
			for k, v := range rows {
				cp[k] = v.Clone()
			}
			qCopy[m] = cp
		}
		fCopy := make(map[int64]*FlowMinute)
		for m, v := range snap.Flow {
			fCopy[m] = v.Clone()
		}
		return qCopy, fCopy
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	qCopy := make(map[int64]map[Key]*QualityMinute)
	for m, rows := range r.pendingQuality {
		cp := make(map[Key]*QualityMinute)
		for k, v := range rows {
			cp[k] = v.Clone()
		}
		qCopy[m] = cp
	}
	fCopy := make(map[int64]*FlowMinute)
	for m, v := range r.pendingFlow {
		fCopy[m] = v.Clone()
	}
	return qCopy, fCopy
}

func (r *Recorder) Snapshot() *Snapshot {
	if snap := r.finalSnapshot.Load(); snap != nil {
		qCopy := make(map[int64]map[Key]*QualityMinute)
		for m, rows := range snap.Quality {
			cp := make(map[Key]*QualityMinute)
			for k, v := range rows {
				cp[k] = v.Clone()
			}
			qCopy[m] = cp
		}
		fCopy := make(map[int64]*FlowMinute)
		for m, v := range snap.Flow {
			fCopy[m] = v.Clone()
		}
		return &Snapshot{Quality: qCopy, Flow: fCopy}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked()
}

func (r *Recorder) Close() error {
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
	if r.admission.Load()&inflightMask == 0 {
		if r.finalSnapshot.Load() == nil {
			snap := r.snapshotLocked()
			r.finalSnapshot.Store(snap)
		}
		if r.zeroCh != nil {
			close(r.zeroCh)
			r.zeroCh = nil
		}
		r.mu.Unlock()
		return nil
	}
	if r.zeroCh == nil {
		r.zeroCh = make(chan struct{})
	}
	if r.admission.Load()&inflightMask == 0 {
		if r.finalSnapshot.Load() == nil {
			snap := r.snapshotLocked()
			r.finalSnapshot.Store(snap)
		}
		ch := r.zeroCh
		close(ch)
		r.zeroCh = nil
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()
	return nil
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
	if r.admission.Load()&inflightMask == 0 {
		if r.finalSnapshot.Load() == nil {
			snap := r.snapshotLocked()
			r.finalSnapshot.Store(snap)
		}
		if r.zeroCh != nil {
			close(r.zeroCh)
			r.zeroCh = nil
		}
		r.mu.Unlock()
		return nil
	}
	if r.zeroCh == nil {
		r.zeroCh = make(chan struct{})
	}
	ch := r.zeroCh
	if r.admission.Load()&inflightMask == 0 {
		if r.finalSnapshot.Load() == nil {
			snap := r.snapshotLocked()
			r.finalSnapshot.Store(snap)
		}
		close(ch)
		r.zeroCh = nil
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()
	select {
	case <-ch:
	case <-ctx.Done():
		return ctx.Err()
	}
	r.mu.Lock()
	if r.finalSnapshot.Load() == nil {
		snap := r.snapshotLocked()
		r.finalSnapshot.Store(snap)
	}
	r.mu.Unlock()
	return nil
}

type AttemptContext struct {
	cell     *Cell
	recorder *Recorder
	done     uint32
}

func (a *AttemptContext) IsZero() bool { return a == nil || a.cell == nil }

func (a *AttemptContext) completeCommon(obs Observation) {
	if a == nil || a.cell == nil || a.recorder == nil {
		return
	}
	if !atomic.CompareAndSwapUint32(&a.done, 0, 1) {
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
			if r.finalSnapshot.Load() == nil {
				snap := r.snapshotLocked()
				r.finalSnapshot.Store(snap)
			}
			r.mu.Unlock()
		}
		if c.isRetired() && c.isReclaimable() {
			r.mu.Lock()
			r.SweepReclaimLocked()
			r.mu.Unlock()
		}
		return
	}
	c.attempts.Add(1)
	if obs.Success {
		c.successes.Add(1)
		if obs.TTFTMs != nil && *obs.TTFTMs > 0 {
			v := *obs.TTFTMs
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
		if r.finalSnapshot.Load() == nil {
			snap := r.snapshotLocked()
			r.finalSnapshot.Store(snap)
		}
		r.mu.Unlock()
	}
	if c.isRetired() && c.isReclaimable() {
		r.mu.Lock()
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
	if a == nil || a.cell == nil || a.recorder == nil {
		return
	}
	if !atomic.CompareAndSwapUint32(&a.done, 0, 1) {
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
		if a.recorder.finalSnapshot.Load() == nil {
			snap := a.recorder.snapshotLocked()
			a.recorder.finalSnapshot.Store(snap)
		}
		a.recorder.mu.Unlock()
	}
	if a.cell.isRetired() && a.cell.isReclaimable() {
		a.recorder.mu.Lock()
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
