// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"math"
	"sync"
	"sync/atomic"
)

const (
	DefaultRetiredCap       = 65536
	DefaultPendingCapBytes  = 256 * 1024 * 1024
	DefaultMinuteBucketsCap = 4096
	EstimatedCellBytes      = 256
	EstimatedQualityRowBytes = 256
	EstimatedFlowMinuteBytes = 4096
	q32Scale                 = 1 << 32
)

type Key struct {
	FP [32]byte
	QC [32]byte
}

type Observation struct {
	Success      bool
	TTFTMs       *int64
	Tokens       int64
	Calls        int64
	Images       int64
	ErrClass     int
	IsCancel     bool
	IsLocal      bool
	IsReservation bool
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
	return int64(lg*lg*float64(q32Scale))
}

type Cell struct {
	key        Key
	attempts   atomic.Int64
	successes  atomic.Int64
	ttftCount  atomic.Int64
	sumQ32     atomic.Int64
	sumSq      atomic.Int64
	hist       [10]atomic.Int64
	errClasses [4]atomic.Int64
	tokens     atomic.Int64
	calls      atomic.Int64
	images     atomic.Int64
	inflight   atomic.Int32
}

type FlowMinute struct {
	minute int64
	edges  [8]int64
	counts [8]int64
}

func (f *FlowMinute) Minute() int64              { return f.minute }
func (f *FlowMinute) Edge(i int) int64           { if i < 0 || i >= 8 { return 0 }; return f.edges[i] }
func (f *FlowMinute) SetEdge(i int, v int64)     { if i >= 0 && i < 8 { f.edges[i] = v } }
func (f *FlowMinute) Count(i int) int64          { if i < 0 || i >= 8 { return 0 }; return f.counts[i] }
func (f *FlowMinute) SetCount(i int, v int64)    { if i >= 0 && i < 8 { f.counts[i] = v } }
func (f *FlowMinute) Edges() [8]int64            { return f.edges }

type pendingQualityRow struct {
	minute int64
	key    Key
}

type Recorder struct {
	effectiveMaxInflight int64
	mu                   sync.Mutex
	active               map[Key]*Cell
	retired              []*Cell
	retiredIndex         map[Key]int
	qualityOverflow      atomic.Int64
	flowOverflow         atomic.Int64
	minuteOverflow       atomic.Int64
	pendingQuality       map[int64]map[Key]*pendingQualityRow
	pendingFlow          map[int64]*FlowMinute
	pendingBytes         atomic.Int64
	retiredCap           int
	pendingCapBytes      int64
	minuteCap            int
	pinnedGauge          atomic.Int64
	closed               atomic.Bool
	finalSnapshot        atomic.Bool
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
		pendingQuality:       make(map[int64]map[Key]*pendingQualityRow),
		pendingFlow:          make(map[int64]*FlowMinute),
		retiredCap:           DefaultRetiredCap,
		pendingCapBytes:      DefaultPendingCapBytes,
		minuteCap:            DefaultMinuteBucketsCap,
	}, nil
}

var errInvalidMaxInflight = errInvalid("invalid max inflight")

type errInvalid string

func (e errInvalid) Error() string { return string(e) }

func (r *Recorder) EffectiveMaxInflight() int64 { return r.effectiveMaxInflight }

// GetOrCreateCell is off-path (may lock). Request path must use precompiled Cell.
func (r *Recorder) GetOrCreateCell(fp [32]byte, qc [32]byte) *Cell {
	if r.closed.Load() {
		return nil
	}
	key := Key{FP: fp, QC: qc}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.active[key]; ok {
		return c
	}
	if idx, ok := r.retiredIndex[key]; ok {
		c := r.retired[idx]
		delete(r.retiredIndex, key)
		r.retired = append(r.retired[:idx], r.retired[idx+1:]...)
		for i := idx; i < len(r.retired); i++ {
			r.retiredIndex[r.retired[i].key] = i
		}
		if c.inflight.Load() > 0 {
			r.pinnedGauge.Add(-1)
		}
		r.active[key] = c
		return c
	}
	c := &Cell{key: key}
	r.active[key] = c
	r.pendingBytes.Add(EstimatedCellBytes)
	r.enforcePendingCapLocked()
	return c
}

// InitAttemptContext is hot path: pointer-only, caller-owned, no lock/allocation.
// Pin is acquired before retirement can evict.
func (r *Recorder) InitAttemptContext(cell *Cell, out *AttemptContext) bool {
	if cell == nil || out == nil {
		return false
	}
	if r.closed.Load() {
		out.cell = nil
		out.recorder = nil
		atomic.StoreUint32(&out.done, 0)
		return false
	}
	cell.inflight.Add(1)
	out.cell = cell
	out.recorder = r
	atomic.StoreUint32(&out.done, 0)
	return true
}

// NewAttemptContext allocates (for convenience). Prefer InitAttemptContext for 0 alloc.
func (r *Recorder) NewAttemptContext(cell *Cell) *AttemptContext {
	if cell == nil || r.closed.Load() {
		return &AttemptContext{recorder: r}
	}
	cell.inflight.Add(1)
	return &AttemptContext{cell: cell, recorder: r}
}

// Begin is deprecated off-path helper (uses GetOrCreateCell + pin). Kept for compatibility but not hot path.
func (r *Recorder) Begin(fp [32]byte, qc [32]byte) *AttemptContext {
	cell := r.GetOrCreateCell(fp, qc)
	if cell == nil {
		return &AttemptContext{recorder: r}
	}
	return r.NewAttemptContext(cell)
}

func (r *Recorder) enforcePendingCapLocked() {
	for r.pendingBytes.Load() > r.pendingCapBytes || r.distinctMinuteCountLocked() > r.minuteCap {
		removed := false
		if r.tryEvictQualityLocked() {
			removed = true
		} else if r.tryEvictFlowLocked() {
			removed = true
		}
		if !removed {
			if r.pendingBytes.Load() > r.pendingCapBytes {
				r.qualityOverflow.Add(1)
			}
			break
		}
	}
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
		for k, row := range rows {
			cell, ok := r.active[k]
			if !ok {
				if idx, ok2 := r.retiredIndex[k]; ok2 {
					cell = r.retired[idx]
				}
			}
			if cell != nil && cell.inflight.Load() > 0 {
				continue
			}
			_ = row
			if first || minute < oldestMinute {
				oldestMinute = minute
				oldestKey = k
				found = true
				first = false
			}
		}
	}
	if !found {
		for i, c := range r.retired {
			if c.inflight.Load() == 0 {
				delete(r.retiredIndex, c.key)
				r.retired = append(r.retired[:i], r.retired[i+1:]...)
				for j := i; j < len(r.retired); j++ {
					r.retiredIndex[r.retired[j].key] = j
				}
				r.pendingBytes.Add(-EstimatedCellBytes)
				r.qualityOverflow.Add(1)
				return true
			}
		}
		return false
	}
	rows := r.pendingQuality[oldestMinute]
	delete(rows, oldestKey)
	if len(rows) == 0 {
		delete(r.pendingQuality, oldestMinute)
	}
	r.pendingBytes.Add(-EstimatedQualityRowBytes)
	r.qualityOverflow.Add(1)
	if len(r.pendingQuality) == 0 && len(r.pendingFlow) == 0 {
		// minute evicted fully handled by distinct count
	}
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

func (r *Recorder) AddQualityRow(minute int64, fp [32]byte, qc [32]byte) {
	if r.closed.Load() {
		return
	}
	key := Key{FP: fp, QC: qc}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.pendingQuality[minute]; !ok {
		r.pendingQuality[minute] = make(map[Key]*pendingQualityRow)
	}
	if _, ok := r.pendingQuality[minute][key]; ok {
		return
	}
	r.pendingQuality[minute][key] = &pendingQualityRow{minute: minute, key: key}
	r.pendingBytes.Add(EstimatedQualityRowBytes)
	r.enforcePendingCapLocked()
}

func (r *Recorder) AddFlowMinute(minute int64, edges [8]int64) {
	if r.closed.Load() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.pendingFlow[minute]; ok {
		return
	}
	fm := &FlowMinute{minute: minute, edges: edges}
	r.pendingFlow[minute] = fm
	r.pendingBytes.Add(EstimatedFlowMinuteBytes)
	r.enforcePendingCapLocked()
}

func (r *Recorder) FlowMinute(minute int64) (*FlowMinute, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fm, ok := r.pendingFlow[minute]
	return fm, ok
}

func (r *Recorder) tryConvergeLocked() {
	for r.unpinnedRetiredCountLocked() > r.retiredCap {
		idx := -1
		for i, c := range r.retired {
			if c.inflight.Load() == 0 {
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
		r.pendingBytes.Add(-EstimatedCellBytes)
		r.qualityOverflow.Add(1)
	}
	for len(r.pendingQuality)+len(r.pendingFlow) > 0 && r.distinctMinuteCountLocked() > r.minuteCap {
		if !r.tryEvictQualityLocked() && !r.tryEvictFlowLocked() {
			break
		}
	}
}

func (r *Recorder) unpinnedRetiredCountLocked() int {
	n := 0
	for _, c := range r.retired {
		if c.inflight.Load() == 0 {
			n++
		}
	}
	return n
}

func (r *Recorder) Retire(fp [32]byte, qc [32]byte) {
	key := Key{FP: fp, QC: qc}
	r.mu.Lock()
	defer r.mu.Unlock()
	cell, ok := r.active[key]
	if !ok {
		return
	}
	delete(r.active, key)
	r.retired = append(r.retired, cell)
	r.retiredIndex[key] = len(r.retired) - 1
	if cell.inflight.Load() > 0 {
		r.pinnedGauge.Add(1)
	}
	for r.unpinnedRetiredCountLocked() > r.retiredCap {
		idx := -1
		for i, c := range r.retired {
			if c.inflight.Load() == 0 {
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
		r.pendingBytes.Add(-EstimatedCellBytes)
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
		if c.inflight.Load() > 0 {
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
func (r *Recorder) PinnedGauge() int64 { return r.pinnedGauge.Load() }

func (r *Recorder) CellStats(fp [32]byte, qc [32]byte) (attempts, successes, ttftCount, tokens, calls, images int64, sumQ32, sumSq int64, hist [10]int64, errClasses [4]int64, ok bool) {
	key := Key{FP: fp, QC: qc}
	r.mu.Lock()
	cell, ok := r.active[key]
	if !ok {
		if idx, ok2 := r.retiredIndex[key]; ok2 {
			cell = r.retired[idx]
			ok = true
		}
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
	return cell.attempts.Load(), cell.successes.Load(), cell.ttftCount.Load(), cell.tokens.Load(), cell.calls.Load(), cell.images.Load(), cell.sumQ32.Load(), cell.sumSq.Load(), h, ec, true
}

func (r *Recorder) ExportSnapshot() (map[int64]map[Key]Observation, map[int64]*FlowMinute) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// for Close determinism, mark final
	r.finalSnapshot.Store(true)
	return nil, nil
}

func (r *Recorder) Close() error {
	if r.closed.Swap(true) {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finalSnapshot.Store(true)
	return nil
}

type AttemptContext struct {
	cell     *Cell
	recorder *Recorder
	done     uint32
}

func (a *AttemptContext) IsZero() bool { return a == nil || a.cell == nil }

func (a *AttemptContext) Complete(success bool, ttftMs *int64, tokens, calls, images int64) {
	if a == nil || a.cell == nil || a.recorder == nil {
		return
	}
	if !atomic.CompareAndSwapUint32(&a.done, 0, 1) {
		return
	}
	c := a.cell
	c.attempts.Add(1)
	if success {
		c.successes.Add(1)
		if ttftMs != nil && *ttftMs > 0 {
			v := *ttftMs
			c.ttftCount.Add(1)
			c.sumQ32.Add(toQ32(v))
			c.sumSq.Add(toSqQ32(v))
			c.hist[histIndex(v)].Add(1)
		}
	} else {
		c.errClasses[ErrClass4xx].Add(1)
	}
	if tokens != 0 {
		c.tokens.Add(tokens)
	}
	if calls != 0 {
		c.calls.Add(calls)
	}
	if images != 0 {
		c.images.Add(images)
	}
	c.inflight.Add(-1)
	if c.inflight.Load() == 0 && a.recorder.needsConverge() {
		a.recorder.mu.Lock()
		if _, ok := a.recorder.retiredIndex[c.key]; ok && a.recorder.pinnedGauge.Load() > 0 {
			a.recorder.pinnedGauge.Add(-1)
		}
		a.recorder.tryConvergeLocked()
		a.recorder.mu.Unlock()
	}
}

func (a *AttemptContext) CompleteObservation(obs Observation) {
	if a == nil || a.cell == nil || a.recorder == nil {
		return
	}
	if !atomic.CompareAndSwapUint32(&a.done, 0, 1) {
		return
	}
	if obs.IsCancel || obs.IsLocal || obs.IsReservation {
		a.cell.inflight.Add(-1)
		if a.cell.inflight.Load() == 0 && a.recorder.needsConverge() {
			a.recorder.mu.Lock()
			if _, ok := a.recorder.retiredIndex[a.cell.key]; ok && a.recorder.pinnedGauge.Load() > 0 {
				a.recorder.pinnedGauge.Add(-1)
			}
			a.recorder.tryConvergeLocked()
			a.recorder.mu.Unlock()
		}
		return
	}
	c := a.cell
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
	if obs.Tokens != 0 {
		c.tokens.Add(obs.Tokens)
	}
	if obs.Calls != 0 {
		c.calls.Add(obs.Calls)
	}
	if obs.Images != 0 {
		c.images.Add(obs.Images)
	}
	c.inflight.Add(-1)
	if c.inflight.Load() == 0 && a.recorder.needsConverge() {
		a.recorder.mu.Lock()
		if _, ok := a.recorder.retiredIndex[c.key]; ok && a.recorder.pinnedGauge.Load() > 0 {
			a.recorder.pinnedGauge.Add(-1)
		}
		a.recorder.tryConvergeLocked()
		a.recorder.mu.Unlock()
	}
}

func (r *Recorder) needsConverge() bool {
	if r.pinnedGauge.Load() > 0 {
		return true
	}
	r.mu.Lock()
	need := r.unpinnedRetiredCountLocked() > r.retiredCap || r.distinctMinuteCountLocked() > r.minuteCap || r.pendingBytes.Load() > r.pendingCapBytes
	r.mu.Unlock()
	return need
}

func (a *AttemptContext) Cancel() {
	if a == nil || a.cell == nil || a.recorder == nil {
		return
	}
	if !atomic.CompareAndSwapUint32(&a.done, 0, 1) {
		return
	}
	a.cell.inflight.Add(-1)
	if a.cell.inflight.Load() == 0 && a.recorder.needsConverge() {
		a.recorder.mu.Lock()
		if _, ok := a.recorder.retiredIndex[a.cell.key]; ok && a.recorder.pinnedGauge.Load() > 0 {
			a.recorder.pinnedGauge.Add(-1)
		}
		a.recorder.tryConvergeLocked()
		a.recorder.mu.Unlock()
	}
}

func (a *AttemptContext) IsCompleted() bool {
	if a == nil {
		return false
	}
	return atomic.LoadUint32(&a.done) == 1
}
