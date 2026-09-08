// SPDX-License-Identifier: AGPL-3.0-or-later
// Package quality — async routing quality telemetry (spec
// docs/superpowers/specs/async-routing-quality-telemetry.md): the flow owner
// is the SOLE state owner of the cross-request flow accumulator. Request
// settlement performs one immutable, nonblocking Submit; all same-minute
// identity reduction (mergeFlowRows) happens off the request goroutine —
// either in the owner loop or in consumer-side handoff calls (quality-sync,
// recorder snapshots). Owner overflow is telemetry loss only: it never
// touches HTTP, failover, health, quota, usage, or billing.
//
// Lock discipline (deadlock fence): o.mu and rec.mu are NEVER held
// simultaneously in either direction. Composite reads (recorder snapshots,
// minute-bucket counts) acquire rec.mu, release it, then acquire o.mu —
// sequentially, never nested. The request path never takes o.mu at all; it
// only holds the uncontended Submit close-fence RLock across the closed
// check + one nonblocking channel send.
package quality

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/worker"
)

// FlowOwnerQueueCap is the fixed submission queue bound (bounded-memory
// decision, not a retention promise under arbitrary overload).
const FlowOwnerQueueCap = 8192

// SubmitResult is the typed, nonblocking outcome of one flow submission.
type SubmitResult uint8

const (
	// SubmitAccepted: the submission is queued and will be merged exactly
	// once (owner loop, consumer drain, or close drain — never discarded).
	SubmitAccepted SubmitResult = iota
	// SubmitOverflowed: the bounded queue is full; counted once in
	// overflowed + edge_rows_dropped. Telemetry loss only.
	SubmitOverflowed
	// SubmitClosed: close began or the recorder is finalized; counted once
	// in overflowed + edge_rows_dropped (rejected after close begins).
	SubmitClosed
)

// flowSubmission owns deep-copied request rows: no request-backed slice or
// pointer may reach the owner state.
type flowSubmission struct {
	minute int64
	rows   []repository.RoutingFlowRow
}

// FlowOwnerStats is the typed Stats() payload served on
// /api/admin/ops/workers under name "quality-flow-owner". Field set is
// pinned by openapi.yaml WorkerStatus.stats — rename = contract break.
type FlowOwnerStats struct {
	Running             bool  `json:"running"`
	Queued              int   `json:"queued"`
	QueueCap            int   `json:"queue_cap"`
	Accepted            int64 `json:"accepted"`
	Processed           int64 `json:"processed"`
	Overflowed          int64 `json:"overflowed"`
	EdgeRowsAccepted    int64 `json:"edge_rows_accepted"`
	EdgeRowsDropped     int64 `json:"edge_rows_dropped"`
	PendingMinutes      int   `json:"pending_minutes"`
	PendingBytes        int64 `json:"pending_bytes"`
	LastMergeDurationMs int64 `json:"last_merge_duration_ms"`
	ResidualSubmissions int64 `json:"residual_submissions"`
	ResidualRows        int64 `json:"residual_rows"`
	CloseUnixMs         int64 `json:"close_unix_ms"`
}

// FlowOwner is the managed worker (worker.Worker + handler.StatsProvider)
// exclusively owning the pending flow accumulator, same-minute identity
// merge, and flow submission accounting. Constructed by NewRecorder (one per
// recorder); retrieved with Recorder.FlowOwner() for registration.
type FlowOwner struct {
	rec   *Recorder
	queue chan flowSubmission

	// submitFence couples the final closed check and channel send to close
	// finalization. mu guards the accumulator only (consumer-side handoff sync point).
	// Request paths never take it. Capacity settings (minuteCap,
	// pendingCapBytes) stay on the Recorder as shared lane config:
	// immutable after construction in production, test-written only from
	// the same goroutine before concurrent use.
	submitFence  sync.RWMutex
	mu           sync.Mutex
	pending      map[int64]*FlowMinute
	pendingBytes int64
	mergeDropped int64

	running             atomic.Bool
	accepted            atomic.Int64
	queuedBytes         atomic.Int64
	processed           atomic.Int64
	overflowed          atomic.Int64
	edgeRowsAccepted    atomic.Int64
	edgeRowsDropped     atomic.Int64
	lastMergeMs         atomic.Int64
	residualSubmissions atomic.Int64
	residualRows        atomic.Int64
	closeUnixMs         atomic.Int64
	submitState         atomic.Uint64
	submitDone          chan struct{}

	started     atomic.Bool
	lifecycleMu sync.Mutex
	closed      bool
	cancel      context.CancelFunc
	// loopDone is the join signal; aliased to loopDoneCh from construction
	// (pre-closed = "no loop ran") through Start, same contract as
	// SyncWorker.loopDone.
	loopDone <-chan struct{}
}

func newFlowOwner(rec *Recorder) *FlowOwner {
	o := &FlowOwner{
		rec:        rec,
		queue:      make(chan flowSubmission, FlowOwnerQueueCap),
		pending:    make(map[int64]*FlowMinute),
		submitDone: make(chan struct{}),
	}
	done := make(chan struct{})
	close(done)
	o.loopDone = done
	return o
}

// Name implements worker.Worker and handler.StatsProvider.
func (o *FlowOwner) Name() string { return "quality-flow-owner" }

// Submit is the request-path seam: one immutable, nonblocking handoff. It
// takes the read-side close fence only across the final closed check and one
// nonblocking queue select; it never takes the owner merge lock. The row copy
// is bounded by flowChainCap.
func (o *FlowOwner) Submit(minute int64, rows []repository.RoutingFlowRow) SubmitResult {
	for {
		state := o.submitState.Load()
		if state&(uint64(1)<<63) != 0 {
			o.overflowed.Add(1)
			o.edgeRowsDropped.Add(int64(len(rows)))
			return SubmitClosed
		}
		if o.submitState.CompareAndSwap(state, state+1) {
			break
		}
	}
	defer o.finishSubmit()
	if o.rec.finalized() {
		o.overflowed.Add(1)
		o.edgeRowsDropped.Add(int64(len(rows)))
		return SubmitClosed
	}
	if len(rows) > o.rec.flowRowCap || (len(rows) > 0 && int64(len(rows)) > o.rec.flowRowBytesCap/EstimatedFlowRowBytes) {
		o.overflowed.Add(1)
		o.edgeRowsDropped.Add(int64(len(rows)))
		return SubmitOverflowed
	}
	own := make([]repository.RoutingFlowRow, len(rows))
	copyFlowRows(own, rows)
	charge := int64(len(own)) * EstimatedFlowRowBytes
	o.queuedBytes.Add(charge)
	o.submitFence.RLock()
	defer o.submitFence.RUnlock()
	if o.submitState.Load()&(uint64(1)<<63) != 0 || o.rec.finalized() {
		o.queuedBytes.Add(-charge)
		o.overflowed.Add(1)
		o.edgeRowsDropped.Add(int64(len(rows)))
		return SubmitClosed
	}
	select {
	case o.queue <- flowSubmission{minute: minute, rows: own}:
		o.accepted.Add(1)
		return SubmitAccepted
	default:
		o.queuedBytes.Add(-charge)
		o.overflowed.Add(1)
		o.edgeRowsDropped.Add(int64(len(rows)))
		return SubmitOverflowed
	}
}

// Start launches the single owner goroutine (worker.Worker contract).
func (o *FlowOwner) Start(ctx context.Context) error {
	o.lifecycleMu.Lock()
	defer o.lifecycleMu.Unlock()
	if o.closed {
		return fmt.Errorf("quality-flow-owner: closed")
	}
	if !o.started.CompareAndSwap(false, true) {
		return fmt.Errorf("quality-flow-owner: already started")
	}
	derived, cancel := context.WithCancel(ctx)
	o.cancel = cancel
	o.loopDone = worker.GoLoop(derived, o.Name(), nil, o.loop)
	return nil
}

func (o *FlowOwner) loop(ctx context.Context) {
	o.running.Store(true)
	defer o.running.Store(false)
	for {
		select {
		case <-ctx.Done():
			// Final drain: bounded pure-memory work (queue is finite), so
			// cancellation never strands accepted submissions.
			o.mu.Lock()
			o.drainQueueLocked()
			o.mu.Unlock()
			return
		case sub := <-o.queue:
			o.mu.Lock()
			o.mergeAcceptedLocked(sub)
			o.drainQueueLocked()
			o.mu.Unlock()
		}
	}
}

// Close stops new submissions, joins the owner goroutine when its context
// permits, and drains the accepted queue under the write fence. The fence
// itself is always acquired: Submit holds it only around a bounded copy and a
// nonblocking send, so returning without the fence would permit a late send.
func (o *FlowOwner) Close(ctx context.Context) error {
	o.lifecycleMu.Lock()
	if !o.closed {
		o.closed = true
	}
	started := o.started.Load()
	loopDone := o.loopDone
	cancel := o.cancel
	o.lifecycleMu.Unlock()

	err := o.closeSubmitFence(ctx)
	defer o.submitFence.Unlock()
	if cancel != nil {
		cancel()
	}

	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if started {
		select {
		case <-loopDone:
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	o.mu.Lock()
	o.closeDrainLocked()
	o.mu.Unlock()
	o.closeUnixMs.Store(o.rec.now().UnixMilli())
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	return err
}

// closeSubmitFence is shared by owner shutdown and recorder finalization. The
// high bit rejects new submissions; the in-flight count keeps every accepted
// submission in the final read-side fence. The write lock then prevents any
// later send before draining or snapshot construction.
func (o *FlowOwner) closeSubmitFence(ctx context.Context) error {
	for {
		state := o.submitState.Load()
		if state&(uint64(1)<<63) != 0 {
			break
		}
		if o.submitState.CompareAndSwap(state, state|(uint64(1)<<63)) {
			if state == 0 {
				close(o.submitDone)
			}
			break
		}
	}
	var err error
	if o.submitState.Load()&^(uint64(1)<<63) != 0 {
		select {
		case <-o.submitDone:
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	// The read-side section contains only a bounded row copy and one
	// nonblocking send. Acquire the write fence even after ctx expires so an
	// in-flight Submit cannot send after the close drain or final snapshot.
	o.submitFence.Lock()
	return err
}

func (o *FlowOwner) finishSubmit() {
	state := o.submitState.Add(^uint64(0))
	if state&(uint64(1)<<63) != 0 && state&^(uint64(1)<<63) == 0 {
		close(o.submitDone)
	}
}
