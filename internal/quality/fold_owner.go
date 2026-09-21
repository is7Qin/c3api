// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

// Fold-at-source owner (v3–): the FlowOwner is the SOLE owner of the
// folded flow state — the counter-cell table (request-Add / tick-drain,
// fold_cells.go) plus the per-minute cumulative shells (tick-only,
// fold_expand.go). The old Submit queue, per-request FlowChain box, and
// push-merge entry points are gone outright: exactly-once lives in the
// completion walk below plus the §5.1 counting table.
//
// The proxy reaches the cells through exactly two methods on the pre-existing
// *FlowOwner type (no new exported types — the fold_* structures stay
// unexported per §9): FoldChain walks one completed chain's stack facts, and
// NoteIncompleteChain records a close-without-terminal. Cap-overflow is
// counted per dropped attempt at the append site via NoteCapOverflow. All
// parameters are pre-existing domain primitives — strings cross only
// transiently and never enter a fact.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/worker"
)

// FlowOwnerStats is the typed Stats() payload served on
// /api/admin/ops/workers under name "quality-flow-owner". Field set is
// pinned by openapi.yaml WorkerStatus.stats — rename = contract break.
// v3 re-sourcing (values, not shape): queued = accepted-not-yet-folded facts
// (no queue remains); queue_cap = total counter-cell capacity; accepted =
// landed facts (event units — one FoldChain call contributes its emitted fact
// count, so accepted == processed + residual_submissions at drained points);
// processed = accepted-origin facts folded by the tick.
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
// exclusively owning the folded flow state: the counter-cell table plus the
// per-minute cumulative shells. Constructed by NewRecorder (one per
// recorder); retrieved with Recorder.FlowOwner() for registration.
//
// Lock discipline: o.mu guards the shells only (tick-owned reads and folds).
// The request path (FoldChain/Adds) never takes o.mu — only lock-free cell
// atomics plus the leaf shard locks inside the table.
type FlowOwner struct {
	rec   *Recorder
	cells *foldCellTable

	mu     sync.Mutex
	shells map[int64]*foldShell
	// nextLeaseID is the owner-wide monotonic lease identity (never reused,
	// never reset) so a stale token from any prior lease is a no-op.
	nextLeaseID uint64
	// drainSeq is the tick-owned expansion sequence stamped on cells at
	// clear-after-expansion (per-cell seq, tick-owned; requests never touch
	// seq or dirty — dirty is derived at expansion from count!=0).
	drainSeq uint64
	sealed   atomic.Bool
	// sealedGen is the seal-split generation (0 = unsealed, 1 = sealed, set
	// once by sealPG and never cleared). Requests Add with the sealed-observed
	// classification stamped at Add time; any pre-seal/post-seal race is
	// reconciled exactly once at fold time by drainFoldLocked and the seal
	// sweep — there is deliberately NO request-side recheck or move-one fixup
	// (it would double-reconcile against the drain path).
	sealedGen atomic.Uint64
	// clock is the explicit owner clock for the late-arrival horizon. Nil in
	// production (admission stays total); tests inject a fixed clock.
	clock func() time.Time

	running             atomic.Bool
	accepted            atomic.Int64
	processed           atomic.Int64
	overflowed          atomic.Int64
	edgeRowsAccepted    atomic.Int64
	edgeRowsDropped     atomic.Int64
	lastMergeMs         atomic.Int64
	residualSubmissions atomic.Int64
	residualRows        atomic.Int64
	closeUnixMs         atomic.Int64

	started atomic.Bool
	closed  atomic.Bool
}

func newFlowOwner(rec *Recorder) *FlowOwner {
	return &FlowOwner{
		rec:    rec,
		cells:  newFoldCellTable(),
		shells: make(map[int64]*foldShell),
	}
}

// Name implements worker.Worker and handler.StatsProvider.
func (o *FlowOwner) Name() string { return "quality-flow-owner" }

// FoldChain walks one completed chain's stack facts (option (a): terminality
// is final — the caller force-marks the last edge before walking) and folds
// each fact with one atomic Add. minuteBucket is minted ONCE by the caller
// and stamped on every fact, so all cells of the chain expand under one
// TerminalMinute. Cap-8: only the first 8 facts emit; the rest count
// cap-overflow with zero cell adds. Calls after owner close (or recorder
// finalization) land nowhere and count dropped.
//
// Seal split (normative §4): per fact, the atomic Add carries the
// sealed-observed classification stamped at Add time (Go atomics are
// sequentially consistent, so the sealed-generation read carries at least
// acquire ordering). Pre-seal/post-seal races reconcile exactly once at fold
// time (drainFoldLocked pre-seal-origin-under-seal branch + seal sweep) —
// no request-side fixup exists by design (see the sealedGen field comment).
func (o *FlowOwner) FoldChain(bucket int64, n int, next func(i int) (
	route domain.RouteClassIDVal,
	fp domain.CandidateFingerprintVal,
	accountID, prevAccount, generation int64,
	ordinal uint8,
	lane, outcome, prevOutcome string,
	terminal, hasPrev bool,
)) {
	if o.closed.Load() || o.rec.finalized() {
		for i := 0; i < n; i++ {
			o.edgeRowsDropped.Add(1)
			o.overflowed.Add(1)
		}
		return
	}
	emit := n
	if emit > foldMaxOrdinal {
		for i := foldMaxOrdinal; i < n; i++ {
			flowChainCapacityOverflow.Add(1)
			o.edgeRowsDropped.Add(1)
			o.overflowed.Add(1)
		}
		emit = foldMaxOrdinal
	}
	for i := 0; i < emit; i++ {
		route, fp, accountID, prevAccount, generation, ordinal, lane, outcome, prevOutcome, terminal, hasPrev := next(i)
		f, err := makeFact(foldSeed{
			route: route, fp: fp, accountID: accountID, prevAccount: prevAccount,
			generation: generation, ordinal: ordinal, lane: lane, outcome: outcome,
			prevOutcome: prevOutcome, terminal: terminal, hasPrev: hasPrev,
		}, bucket)
		if err != nil {
			o.edgeRowsDropped.Add(1)
			o.overflowed.Add(1)
			continue
		}
		// Go atomics are sequentially consistent: the sealed-generation
		// recheck below carries (at least) acquire ordering.
		sealed := o.sealedGen.Load()
		f.residual = sealed != 0
		if o.addFact(f, 1) {
			o.accepted.Add(1)
			if f.residual {
				o.residualSubmissions.Add(1)
			}
		} else {
			o.edgeRowsDropped.Add(1)
			o.overflowed.Add(1)
			continue
		}
		// No request-side seal fixup here by design (review): the
		// pre-seal/post-seal race is reconciled exactly once at fold time by
		// drainFoldLocked (pre-seal origin folded under seal) and the seal
		// sweep. A request-side move-one fixup would double-reconcile the
		// class counters against the drain path (edgeRowsAccepted −2 net,
		// residualRows +2) on the Add→seal→drain→recheck interleave.
	}
}

// addFact lands one delta and tallies its class. Deltas are events: a
// consumer-seam row with ChainCount N folds N events, keeping
// accepted+dropped+residual == offered exact.
func (o *FlowOwner) addFact(f attemptFact, delta int64) bool {
	if !o.cells.add(f, delta) {
		return false
	}
	if f.residual {
		o.residualRows.Add(delta)
	} else {
		o.edgeRowsAccepted.Add(delta)
	}
	return true
}

// NoteIncompleteChain records a close-without-terminal (the §5.1 Close row:
// same unexported counter +1, facts emitted none).
func (o *FlowOwner) NoteIncompleteChain() { flowChainIncomplete.Add(1) }

// NoteCapOverflow records one dropped attempt beyond the cap-8 bound at the
// append site: same unexported cap-overflow counter +1, zero cell adds.
func (o *FlowOwner) NoteCapOverflow() {
	flowChainCapacityOverflow.Add(1)
	o.edgeRowsDropped.Add(1)
	o.overflowed.Add(1)
}

func (o *FlowOwner) Stats() any { return o.SnapshotStats() }

func (o *FlowOwner) SnapshotStats() FlowOwnerStats {
	o.mu.Lock()
	pendingMinutes := len(o.shells)
	pendingBytes := o.pendingBytesLocked()
	o.mu.Unlock()
	var queueCap int64
	for i := range o.cells.shards {
		queueCap += int64(len(o.cells.shards[i].slots))
	}
	return FlowOwnerStats{
		Running: o.running.Load(), Queued: int(o.cells.pending.Load()), QueueCap: int(queueCap),
		Accepted: o.accepted.Load(), Processed: o.processed.Load(), Overflowed: o.overflowed.Load(),
		EdgeRowsAccepted: o.edgeRowsAccepted.Load(), EdgeRowsDropped: o.edgeRowsDropped.Load(),
		PendingMinutes: pendingMinutes, PendingBytes: pendingBytes, LastMergeDurationMs: o.lastMergeMs.Load(),
		ResidualSubmissions: o.residualSubmissions.Load(), ResidualRows: o.residualRows.Load(), CloseUnixMs: o.closeUnixMs.Load(),
	}
}

// Start marks the owner running. There is no owner goroutine anymore —
// nothing is queued, so no consumer loop exists; the tick expansions run
// inside the sync flushes. Registration order (owner before quality-sync) is
// unchanged.
func (o *FlowOwner) Start(ctx context.Context) error {
	if o.closed.Load() {
		return fmt.Errorf("quality-flow-owner: closed")
	}
	if !o.started.CompareAndSwap(false, true) {
		return fmt.Errorf("quality-flow-owner: already started")
	}
	o.running.Store(true)
	return nil
}

// Close stops the owner. Seal runs via SyncWorker.Close (unchanged); here
// only the lifecycle flips. Safe before Start and idempotent.
func (o *FlowOwner) Close(ctx context.Context) error {
	o.closed.Store(true)
	o.running.Store(false)
	o.closeUnixMs.Store(o.rec.now().UnixMilli())
	return nil
}

var _ worker.Worker = (*FlowOwner)(nil)
var _ interface{ Stats() any } = (*FlowOwner)(nil)
