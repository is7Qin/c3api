// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

const (
	DefaultRetiredCap     = 65536
	DefaultPendingCapBytes = 256 * 1024 * 1024
	DefaultMinuteBucketsCap = 4096
	EstimatedCellBytes     = 256
	EstimatedMinuteBytes   = 256
)

type Key struct {
	FP [32]byte
	QC [32]byte
}

type qualityCell struct {
	key       Key
	attempts  atomic.Int64
	successes atomic.Int64
	ttftCount atomic.Int64
	sumLogBits atomic.Uint64
	sumSqBits  atomic.Uint64
	tokens    atomic.Int64
	calls     atomic.Int64
	images    atomic.Int64
	hist      [4]atomic.Int64
	inflight  atomic.Int32
}

func (c *qualityCell) addLog(logVal float64) {
	for {
		old := c.sumLogBits.Load()
		nf := math.Float64frombits(old) + logVal
		if c.sumLogBits.CompareAndSwap(old, math.Float64bits(nf)) {
			break
		}
	}
}

func (c *qualityCell) addSq(sq float64) {
	for {
		old := c.sumSqBits.Load()
		nf := math.Float64frombits(old) + sq
		if c.sumSqBits.CompareAndSwap(old, math.Float64bits(nf)) {
			break
		}
	}
}

type minuteBucket struct {
	minute int64
	count  atomic.Int64
}

type Recorder struct {
	effectiveMaxInflight int64
	mu                   sync.RWMutex
	active               map[Key]*qualityCell
	retired              []*qualityCell
	retiredIndex         map[Key]int
	qualityOverflow      atomic.Int64
	flowOverflow         atomic.Int64
	minuteOverflow       atomic.Int64
	minuteBuckets        map[int64]*minuteBucket
	pendingBytes         atomic.Int64
	retiredCap           int
	pendingCapBytes      int64
	minuteCap            int
	pinnedGauge          atomic.Int64
	closed               atomic.Bool
}

func NewRecorder(effectiveMaxInflight int64) (*Recorder, error) {
	if effectiveMaxInflight <= 0 {
		return nil, errInvalidMaxInflight
	}
	r := &Recorder{
		effectiveMaxInflight: effectiveMaxInflight,
		active:               make(map[Key]*qualityCell),
		retired:              make([]*qualityCell, 0),
		retiredIndex:         make(map[Key]int),
		minuteBuckets:        make(map[int64]*minuteBucket),
		retiredCap:           DefaultRetiredCap,
		pendingCapBytes:      DefaultPendingCapBytes,
		minuteCap:            DefaultMinuteBucketsCap,
	}
	return r, nil
}

var errInvalidMaxInflight = errInvalid("invalid max inflight")

type errInvalid string

func (e errInvalid) Error() string { return string(e) }

func (r *Recorder) EffectiveMaxInflight() int64 { return r.effectiveMaxInflight }

func (r *Recorder) Begin(fp [32]byte, qc [32]byte) AttemptContext {
	if r.closed.Load() {
		return AttemptContext{recorder: r}
	}
	key := Key{FP: fp, QC: qc}
	r.mu.RLock()
	cell, ok := r.active[key]
	r.mu.RUnlock()
	if !ok {
		r.mu.Lock()
		cell, ok = r.active[key]
		if !ok {
			if rc, ok2 := r.retiredIndex[key]; ok2 {
				cell = r.retired[rc]
				delete(r.retiredIndex, key)
				r.retired = append(r.retired[:rc], r.retired[rc+1:]...)
				for i := rc; i < len(r.retired); i++ {
					r.retiredIndex[r.retired[i].key] = i
				}
				r.pendingBytes.Add(-EstimatedCellBytes)
			} else {
				cell = &qualityCell{key: key}
				r.pendingBytes.Add(EstimatedCellBytes)
				r.enforcePendingCapLocked()
			}
			r.active[key] = cell
		}
		r.mu.Unlock()
	}
	cell.inflight.Add(1)
	return AttemptContext{cell: cell, recorder: r}
}

func (r *Recorder) enforcePendingCapLocked() {
	for r.pendingBytes.Load() > r.pendingCapBytes {
		victimIdx := -1
		for i, c := range r.retired {
			if c.inflight.Load() == 0 {
				victimIdx = i
				break
			}
		}
		if victimIdx >= 0 {
			v := r.retired[victimIdx]
			delete(r.retiredIndex, v.key)
			r.retired = append(r.retired[:victimIdx], r.retired[victimIdx+1:]...)
			for i := victimIdx; i < len(r.retired); i++ {
				r.retiredIndex[r.retired[i].key] = i
			}
			r.pendingBytes.Add(-EstimatedCellBytes)
			r.qualityOverflow.Add(1)
			continue
		}
		if len(r.minuteBuckets) > 0 {
			var oldest int64
			first := true
			for k := range r.minuteBuckets {
				if first || k < oldest {
					oldest = k
					first = false
				}
			}
			delete(r.minuteBuckets, oldest)
			r.pendingBytes.Add(-EstimatedMinuteBytes)
			r.flowOverflow.Add(1)
			r.minuteOverflow.Add(1)
			continue
		}
		r.qualityOverflow.Add(1)
		break
	}
}

func (r *Recorder) recordMinute(minute int64, success bool, ttftMs *int64, tokens int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed.Load() {
		return
	}
	b, ok := r.minuteBuckets[minute]
	if !ok {
		if len(r.minuteBuckets) >= r.minuteCap {
			var oldest int64
			first := true
			for k := range r.minuteBuckets {
				if first || k < oldest {
					oldest = k
					first = false
				}
			}
			delete(r.minuteBuckets, oldest)
			r.pendingBytes.Add(-EstimatedMinuteBytes)
			r.flowOverflow.Add(1)
			r.minuteOverflow.Add(1)
		}
		b = &minuteBucket{minute: minute}
		r.minuteBuckets[minute] = b
		r.pendingBytes.Add(EstimatedMinuteBytes)
		r.enforcePendingCapLocked()
	}
	b.count.Add(1)
}

func (r *Recorder) tryConverge() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.retired) > 0 && r.unpinnedRetiredCountLocked() > r.retiredCap {
		victimIdx := -1
		for i, c := range r.retired {
			if c.inflight.Load() == 0 {
				victimIdx = i
				break
			}
		}
		if victimIdx < 0 {
			break
		}
		v := r.retired[victimIdx]
		delete(r.retiredIndex, v.key)
		r.retired = append(r.retired[:victimIdx], r.retired[victimIdx+1:]...)
		for i := victimIdx; i < len(r.retired); i++ {
			r.retiredIndex[r.retired[i].key] = i
		}
		r.pendingBytes.Add(-EstimatedCellBytes)
		r.qualityOverflow.Add(1)
	}
	for len(r.minuteBuckets) > r.minuteCap {
		var oldest int64
		first := true
		for k := range r.minuteBuckets {
			if first || k < oldest {
				oldest = k
				first = false
			}
		}
		delete(r.minuteBuckets, oldest)
		r.pendingBytes.Add(-EstimatedMinuteBytes)
		r.flowOverflow.Add(1)
		r.minuteOverflow.Add(1)
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
	for r.unpinnedRetiredCountLocked() > r.retiredCap {
		victimIdx := -1
		for i, c := range r.retired {
			if c.inflight.Load() == 0 {
				victimIdx = i
				break
			}
		}
		if victimIdx < 0 {
			break
		}
		v := r.retired[victimIdx]
		delete(r.retiredIndex, v.key)
		r.retired = append(r.retired[:victimIdx], r.retired[victimIdx+1:]...)
		for i := victimIdx; i < len(r.retired); i++ {
			r.retiredIndex[r.retired[i].key] = i
		}
		r.pendingBytes.Add(-EstimatedCellBytes)
		r.qualityOverflow.Add(1)
	}
}

func (r *Recorder) ActiveCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.active)
}

func (r *Recorder) RetiredCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.retired)
}

func (r *Recorder) UnpinnedRetiredCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.unpinnedRetiredCountLocked()
}

func (r *Recorder) PinnedRetiredCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
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
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.minuteBuckets)
}

func (r *Recorder) CellStats(fp [32]byte, qc [32]byte) (attempts, successes, ttftCount, tokens, calls, images int64, sumLog, sumSq float64, hist [4]int64, ok bool) {
	key := Key{FP: fp, QC: qc}
	r.mu.RLock()
	cell, ok := r.active[key]
	if !ok {
		if idx, ok2 := r.retiredIndex[key]; ok2 {
			cell = r.retired[idx]
			ok = true
		}
	}
	r.mu.RUnlock()
	if !ok || cell == nil {
		return 0, 0, 0, 0, 0, 0, 0, 0, [4]int64{}, false
	}
	return cell.attempts.Load(), cell.successes.Load(), cell.ttftCount.Load(), cell.tokens.Load(), cell.calls.Load(), cell.images.Load(), math.Float64frombits(cell.sumLogBits.Load()), math.Float64frombits(cell.sumSqBits.Load()), [4]int64{cell.hist[0].Load(), cell.hist[1].Load(), cell.hist[2].Load(), cell.hist[3].Load()}, true
}

func (r *Recorder) Close() error {
	r.closed.Store(true)
	return nil
}

type AttemptContext struct {
	cell     *qualityCell
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
	}
	if success && ttftMs != nil && *ttftMs > 0 {
		v := *ttftMs
		if v < 1 {
			v = 1
		}
		lg := math.Log(float64(v))
		c.addLog(lg)
		c.addSq(lg * lg)
		c.ttftCount.Add(1)
		idx := 0
		if v > 100 {
			idx = 1
		}
		if v > 500 {
			idx = 2
		}
		if v > 1000 {
			idx = 3
		}
		c.hist[idx].Add(1)
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
	a.recorder.recordMinute(time.Now().UTC().Truncate(time.Minute).Unix(), success, ttftMs, tokens)
	c.inflight.Add(-1)
	a.recorder.tryConverge()
}

func (a *AttemptContext) Cancel() {
	if a == nil || a.cell == nil || a.recorder == nil {
		return
	}
	if !atomic.CompareAndSwapUint32(&a.done, 0, 1) {
		return
	}
	a.cell.inflight.Add(-1)
	a.recorder.tryConverge()
}

func (a *AttemptContext) IsCompleted() bool {
	if a == nil {
		return false
	}
	return atomic.LoadUint32(&a.done) == 1
}
