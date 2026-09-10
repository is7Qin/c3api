// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

// Identity-indexed flow accumulator (normative spec
// docs/superpowers/specs/identity-indexed-flow-accumulator.md section 4):
// FlowOwner is the sole long-lived owner of cumulative flow state. One
// identity-indexed accumulator per minute stores the full cumulative
// ChainCount; sync never owns flow rows. The merge folds one incoming chain
// (at most flowChainCap rows) in place at O(chain length): one hash lookup
// per row, zero work proportional to the retained distinct identity count,
// zero steady-state allocation on duplicate folds beyond the already-owned
// submission.
//
// Representation map to the spec text: spec `entries` is acc.entries,
// spec `order` (first-insertion sequence) is acc.order, spec
// `flowIdentityEntry{row, count}` is entry{row, count} with
// count == row.ChainCount at all times. Legacy whole-minute edge arrays
// (AddFlowMinute form) ride along in the accumulator payload and are
// preserved verbatim through materialization; they carry no identity charge.
//
// Locking: every function here requires o.mu. Submit never takes o.mu (queue
// only); pgSealed is atomic so pgWorkTotal stays race-free off the lock.

import (
	"errors"
	"time"

	"github.com/is7qin/c3api/internal/repository"
)

const (
	minuteWidth           = time.Minute
	flowLateCutoffSeconds = 600
)

// flowIdentityEntry is one retained edge identity: the owned deep row plus
// its cumulative count, kept equal to row.ChainCount at all times.
type flowIdentityEntry struct {
	row   repository.RoutingFlowRow
	count int64
}

// flowMinuteAccumulator is the single-owner cumulative state for one minute.
type flowMinuteAccumulator struct {
	minute  int64
	entries map[flowEdgeIdentity]*flowIdentityEntry
	order   []flowEdgeIdentity // first-insertion sequence, append only, never reordered
	edges   [8]int64           // legacy whole-minute edge arrays, preserved verbatim
	counts  [8]int64
	hasRows bool // any identity ever retained (distinguishes empty from erased)

	emptySnapshot bool
	version       uint64 // bumped on every retained-state mutation
	dirty         bool   // set only by PG-open accepted-class mutations, cleared only by clean ack

	incarnation uint64 // assigned once as ++owner.nextIncarnation at creation, immutable
	leased      bool
	// activeLeaseID is the lease identity of the active lease, assigned as
	// ++owner.nextLeaseID at acquisition; never repeats even when the
	// version is unchanged, so a stale token from any prior lease is a
	// no-op even at the same version.
	activeLeaseID          uint64
	capturedVersion        uint64 // accumulator version at lease acquisition; clean-vs-dirty only
	leaseAcceptedWatermark int64  // totalAcceptedContrib at lease acquisition

	everPersisted              bool // set by every successful ack incl. empty zero-row; never cleared
	totalAcceptedContrib       int64
	persistedAcceptedWatermark int64
	residualContrib            int64 // close-drain terminal contributions currently retained
}

// flowSnapshotToken binds minute, incarnation, lease identity, captured
// version, and watermark. Private value passed from snapshotForPG to
// ackPG/releasePG through sync; never persisted or logged with row contents.
type flowSnapshotToken struct {
	minute            int64
	incarnation       uint64
	leaseID           uint64
	capturedVersion   uint64
	acceptedWatermark int64
}

// ownerCutoffLocked is the late-arrival horizon: horizon clock truncated to
// the minute minus 600 seconds. The horizon clock is the injected owner
// clock when set, else rec.now. Eviction eligibility always consults it;
// admission rejection additionally requires the explicit owner clock (see
// admissionCutoff). Caller must hold o.mu.
func (o *FlowOwner) ownerCutoffLocked() int64 {
	return o.horizonNow().UTC().Truncate(minuteWidth).Unix() - flowLateCutoffSeconds
}

// horizonNow prefers the injected owner clock and falls back to rec.now.
func (o *FlowOwner) horizonNow() time.Time {
	if o.clock != nil {
		return o.clock()
	}
	return o.rec.now()
}

// admissionCutoff reports the old-absent admission cutoff and whether it is
// enforced. Enforcement requires the explicit owner clock: the default
// construction admits totally (cold-start and replay tolerant), while an
// injected fixed clock makes the 600-second rejection deterministic.
func (o *FlowOwner) admissionCutoff() (int64, bool) {
	if o.clock == nil {
		return 0, false
	}
	return o.ownerCutoffLocked(), true
}

// flowRowBudget returns R, the per-minute distinct identity cap.
func (o *FlowOwner) flowRowBudget() int {
	rowCap := o.rec.flowRowCap
	if byBytes := int(o.rec.flowRowBytesCap / EstimatedFlowRowBytes); byBytes < rowCap {
		rowCap = byBytes
	}
	return rowCap
}

// materialize projects the accumulator into a deep-owned *FlowMinute in
// first-insertion (old-first) order. Live maps, snapshot payloads, and repo
// payloads never alias each other: every row and PreviousAccountID pointee
// is fresh.
func (a *flowMinuteAccumulator) materialize() *FlowMinute {
	fm := &FlowMinute{
		minute:        a.minute,
		edges:         a.edges,
		counts:        a.counts,
		emptySnapshot: a.emptySnapshot,
	}
	if len(a.order) > 0 {
		fm.flowRows = make([]repository.RoutingFlowRow, len(a.order))
		for i, id := range a.order {
			fm.flowRows[i] = cloneFlowRow(a.entries[id].row)
		}
	}
	return fm
}

func cloneFlowRow(row repository.RoutingFlowRow) repository.RoutingFlowRow {
	if row.PreviousAccountID != nil {
		v := *row.PreviousAccountID
		row.PreviousAccountID = &v
	}
	return row
}

// evictOldestEligibleLocked evicts the oldest minute eligible under the
// capacity policy: a leased minute is never eligible; any everPersisted
// minute (clean or dirty) is ineligible before the cutoff because it carries
// the durable reconstruction baseline. Eviction reclassifies exactly the
// unpersisted accepted credits (accepted->dropped) and retained residual
// credits (residual->dropped) with the source bucket required to hold them —
// no saturation, no clamping. Caller must hold o.mu.
func (o *FlowOwner) evictOldestEligibleLocked(cutoff int64) bool {
	var oldest int64
	target, found := int64(0), false
	for minute, acc := range o.pending {
		if acc.leased {
			continue
		}
		if acc.everPersisted && minute >= cutoff {
			continue
		}
		if !found || minute < oldest {
			oldest, target, found = minute, minute, true
		}
	}
	if !found {
		return false
	}
	acc := o.pending[target]
	if unpersisted := acc.totalAcceptedContrib - acc.persistedAcceptedWatermark; unpersisted > 0 {
		o.edgeRowsAccepted.Add(-unpersisted)
		o.edgeRowsDropped.Add(unpersisted)
	}
	if acc.residualContrib > 0 {
		o.residualRows.Add(-acc.residualContrib)
		o.edgeRowsDropped.Add(acc.residualContrib)
	}
	o.pendingBytes -= EstimatedFlowMinuteBytes + int64(len(acc.entries))*EstimatedFlowRowBytes
	delete(o.pending, target)
	o.rec.flowOverflow.Add(1)
	o.rec.minuteOverflow.Add(1)
	return true
}

// admitMinuteLocked makes room for a genuinely new minute (slot plus the
// minute charge) by evicting the oldest eligible minutes. Caller holds o.mu.
func (o *FlowOwner) admitMinuteLocked(minute int64, cutoff int64) (*flowMinuteAccumulator, error) {
	for len(o.pending) >= o.rec.minuteCap || o.pendingBytes+EstimatedFlowMinuteBytes > o.rec.pendingCapBytes {
		if !o.evictOldestEligibleLocked(cutoff) {
			return nil, ErrCapacity
		}
	}
	o.nextIncarnation++
	acc := &flowMinuteAccumulator{
		minute:      minute,
		entries:     make(map[flowEdgeIdentity]*flowIdentityEntry),
		incarnation: o.nextIncarnation,
	}
	o.pending[minute] = acc
	o.pendingBytes += EstimatedFlowMinuteBytes
	return acc, nil
}

// foldRowsLocked folds row-form rows into acc in place at O(chain length).
// Duplicates add into the stored count in place (never move in order, never
// check capacity, never allocate beyond the owned submission); distinct
// identities insert an owned deep copy (fresh PreviousAccountID pointee) with
// per-identity R plus liveCharge budget checks — a rejected identity is
// classified dropped immediately and leaves existing state untouched.
// Caller must hold o.mu; acc must be PG-open (pre-seal, non-residual).
func (o *FlowOwner) foldRowsLocked(acc *flowMinuteAccumulator, rows []repository.RoutingFlowRow, creditAccepted bool) (kept, dropped int64) {
	rowCap := o.flowRowBudget()
	for _, row := range rows {
		id := flowEdgeIdentityOf(row)
		if e, ok := acc.entries[id]; ok {
			e.row.ChainCount += row.ChainCount
			e.count += row.ChainCount
			kept++
			continue
		}
		if len(acc.entries) >= rowCap || o.pendingBytes+EstimatedFlowRowBytes > o.rec.pendingCapBytes {
			dropped++
			continue
		}
		cp := cloneFlowRow(row)
		acc.entries[id] = &flowIdentityEntry{row: cp, count: cp.ChainCount}
		acc.order = append(acc.order, id)
		o.pendingBytes += EstimatedFlowRowBytes
		kept++
	}
	if kept > 0 {
		acc.hasRows = true
		acc.emptySnapshot = false
		acc.version++
		acc.dirty = true
		if creditAccepted {
			acc.totalAcceptedContrib += kept
			o.edgeRowsAccepted.Add(kept)
		}
	}
	if dropped > 0 {
		o.edgeRowsDropped.Add(dropped)
	}
	return kept, dropped
}

// foldResidualLocked merges rows as residual-classified terminal state:
// tracked in residualContrib, counted residual, never dirty, never a PG
// candidate. Residual close-drain merges never drop by age. Caller must hold
// o.mu.
func (o *FlowOwner) foldResidualLocked(minute int64, rows []repository.RoutingFlowRow, cutoff int64) (kept, dropped int64) {
	acc, ok := o.pending[minute]
	if !ok {
		var err error
		if acc, err = o.admitMinuteLocked(minute, cutoff); err != nil {
			if len(rows) > 0 {
				o.edgeRowsDropped.Add(int64(len(rows)))
			}
			return 0, int64(len(rows))
		}
	}
	rowCap := o.flowRowBudget()
	for _, row := range rows {
		id := flowEdgeIdentityOf(row)
		if e, ok := acc.entries[id]; ok {
			e.row.ChainCount += row.ChainCount
			e.count += row.ChainCount
			kept++
			continue
		}
		if len(acc.entries) >= rowCap || o.pendingBytes+EstimatedFlowRowBytes > o.rec.pendingCapBytes {
			dropped++
			continue
		}
		cp := cloneFlowRow(row)
		acc.entries[id] = &flowIdentityEntry{row: cp, count: cp.ChainCount}
		acc.order = append(acc.order, id)
		o.pendingBytes += EstimatedFlowRowBytes
		kept++
	}
	if kept > 0 {
		acc.hasRows = true
		acc.emptySnapshot = false
		acc.version++
		acc.residualContrib += kept
		o.residualRows.Add(kept)
	}
	if dropped > 0 {
		o.edgeRowsDropped.Add(dropped)
	}
	return kept, dropped
}

// mergeLocked folds one absolute flow-minute delta into the accumulator.
// Row-form merges are O(chain length) in place; legacy edge arrays are
// preserved verbatim without disturbing retained rows; empty markers advance
// sequence without erasing. Late arrivals against absent minutes older than
// the enforced horizon are rejected as dropped with no accumulator created
// (admissionCutoff: enforced only with an explicit owner clock, so default
// construction stays total). Existing dirty old minutes keep merging through
// the normal path. Returns kept plus dropped with kept+dropped ==
// len(fm.flowRows) for row-form, and ErrCapacity on terminal admission
// failure. Caller must hold o.mu; the minute must be PG-open (callers route
// sealed/residual merges to foldResidualLocked).
func (o *FlowOwner) mergeLocked(fm *FlowMinute, creditAccepted bool) (kept, dropped int64, err error) {
	if fm == nil {
		return 0, 0, errors.New("nil flow minute")
	}
	cutoff := o.ownerCutoffLocked()
	if fm.emptySnapshot {
		return o.foldEmptyLocked(fm.minute, cutoff)
	}
	if len(fm.flowRows) == 0 && !hasLegacyEdges(fm) {
		return 0, 0, nil
	}
	if acc, ok := o.pending[fm.minute]; ok {
		if len(fm.flowRows) > 0 {
			kept, dropped = o.foldRowsLocked(acc, fm.flowRows, creditAccepted)
			return kept, dropped, nil
		}
		acc.edges = fm.edges
		acc.counts = fm.counts
		acc.version++
		acc.dirty = true
		return 0, 0, nil
	}
	if cutoff, enforced := o.admissionCutoff(); enforced && fm.minute < cutoff {
		if n := int64(len(fm.flowRows)); n > 0 {
			o.edgeRowsDropped.Add(n)
			return 0, n, ErrCapacity
		}
		return 0, 0, ErrCapacity
	}
	acc, aerr := o.admitMinuteLocked(fm.minute, cutoff)
	if aerr != nil {
		if n := int64(len(fm.flowRows)); n > 0 {
			o.edgeRowsDropped.Add(n)
			return 0, n, ErrCapacity
		}
		return 0, 0, ErrCapacity
	}
	if len(fm.flowRows) == 0 {
		acc.edges = fm.edges
		acc.counts = fm.counts
		acc.version++
		acc.dirty = true
		return 0, 0, nil
	}
	kept, dropped = o.foldRowsLocked(acc, fm.flowRows, creditAccepted)
	return kept, dropped, nil
}

// foldEmptyLocked applies an authoritative empty marker: it takes effect only
// when the minute has no accumulator or the existing accumulator holds no
// rows; a non-empty accumulator is never erased. A same-minute marker costs
// nothing and triggers no eviction; a new-minute marker needs only the minute
// slot and is dropped silently with zero counter change when the slot cannot
// be made. An old-absent marker is ignored with zero counter change when the
// horizon is enforced (explicit owner clock); default construction admits it.
func (o *FlowOwner) foldEmptyLocked(minute, cutoff int64) (int64, int64, error) {
	if acc, ok := o.pending[minute]; ok {
		if len(acc.order) > 0 {
			return 0, 0, nil
		}
		acc.emptySnapshot = true
		acc.version++
		if !o.pgSealed.Load() {
			acc.dirty = true
		}
		return 0, 0, nil
	}
	if enforcedCutoff, enforced := o.admissionCutoff(); enforced && minute < enforcedCutoff {
		return 0, 0, nil
	}
	for len(o.pending) >= o.rec.minuteCap || o.pendingBytes+EstimatedFlowMinuteBytes > o.rec.pendingCapBytes {
		if !o.evictOldestEligibleLocked(cutoff) {
			return 0, 0, nil
		}
	}
	o.nextIncarnation++
	o.pending[minute] = &flowMinuteAccumulator{
		minute:        minute,
		entries:       make(map[flowEdgeIdentity]*flowIdentityEntry),
		emptySnapshot: true,
		version:       1,
		dirty:         !o.pgSealed.Load(),
		incarnation:   o.nextIncarnation,
	}
	o.pendingBytes += EstimatedFlowMinuteBytes
	return 0, 0, nil
}

func hasLegacyEdges(fm *FlowMinute) bool {
	for i := 0; i < 8; i++ {
		if fm.edges[i] != 0 || fm.counts[i] != 0 {
			return true
		}
	}
	return false
}
