// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

// Fold-at-source consumer seam and tick-owned reads (v3 F1–F2): the
// synchronous ingestion path (legacy edges/empty-marker forms and tests)
// folds rows into cells through the same packed facts as the request walk —
// one mechanism. Every read below folds pending cells first, so reads observe
// every folded fact; growth stays tick-expansion-only.

import (
	"time"
)

// enqueue is the private synchronous consumer seam: rows fold into cells
// through the same packed facts as the request walk (no eager drain —
// tick-owned reads fold). After PG seal the ingestion is
// residual-classified: rows fold residual without ever setting dirty, and
// markers/legacy edges are preserved without dirtying, so a post-seal
// synchronous enqueue can never create PG-open dirty accepted state.
// Caller-facing; takes o.mu via the method itself.
func (o *FlowOwner) enqueue(fm *FlowMinute) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed.Load() || o.rec.finalized() {
		return ErrCapacity
	}
	if fm == nil {
		return ErrCapacity
	}
	if o.sealed.Load() {
		return o.enqueueSealedLocked(fm)
	}
	cutoff := o.ownerCutoffLocked()
	if fm.emptySnapshot {
		o.markEmptyLocked(fm.minute, cutoff)
		return nil
	}
	if len(fm.flowRows) > 0 {
		if _, ok := o.shells[fm.minute]; !ok {
			if c, enforced := o.admissionCutoff(); enforced && fm.minute < c {
				n := int64(len(fm.flowRows))
				o.edgeRowsDropped.Add(n)
				return ErrCapacity
			}
		}
		seeds := make([]foldSeed, 0, len(fm.flowRows))
		deltas := make([]int64, 0, len(fm.flowRows))
		for _, row := range fm.flowRows {
			seed, delta, err := rowToSeed(row)
			if err != nil {
				return err
			}
			seeds = append(seeds, seed)
			deltas = append(deltas, delta)
		}
		for i, seed := range seeds {
			f, err := makeFact(seed, fm.minute)
			if err != nil {
				return err
			}
			if !o.addFact(f, deltas[i]) {
				o.edgeRowsDropped.Add(deltas[i])
				o.overflowed.Add(deltas[i])
			}
		}
		return nil
	}
	if hasLegacyEdges(fm) {
		if _, ok := o.shells[fm.minute]; !ok {
			if c, enforced := o.admissionCutoff(); enforced && fm.minute < c {
				return ErrCapacity
			}
		}
		shell := o.getOrCreateShell(fm.minute)
		shell.edges = fm.edges
		shell.counts8 = fm.counts
		shell.version++
		shell.dirty = true
	}
	return nil
}

// enqueueSealedLocked residual-classifies post-seal consumer ingestion:
// terminal drains never drop by age and never dirty. Caller must hold o.mu.
func (o *FlowOwner) enqueueSealedLocked(fm *FlowMinute) error {
	if fm.emptySnapshot {
		o.markEmptyLocked(fm.minute, o.ownerCutoffLocked())
		return nil
	}
	if len(fm.flowRows) > 0 {
		for _, row := range fm.flowRows {
			seed, delta, err := rowToSeed(row)
			if err != nil {
				return err
			}
			f, err := makeFact(seed, fm.minute)
			if err != nil {
				return err
			}
			f.residual = true
			if !o.addFact(f, delta) {
				o.edgeRowsDropped.Add(delta)
				o.overflowed.Add(delta)
			}
		}
		return nil
	}
	if hasLegacyEdges(fm) {
		shell := o.getOrCreateShell(fm.minute)
		shell.edges = fm.edges
		shell.counts8 = fm.counts
		shell.version++
	}
	return nil
}

// markEmptyLocked applies an authoritative empty marker: it takes effect only
// when the minute has no shell or the existing shell holds no rows; a
// non-empty shell is never erased. Old-absent markers are ignored with zero
// counter change when the horizon is enforced. Caller must hold o.mu.
func (o *FlowOwner) markEmptyLocked(minute, cutoff int64) {
	if shell, ok := o.shells[minute]; ok {
		if len(shell.counts) > 0 || hasLegacyCounts(shell) {
			return
		}
		shell.emptyMarked = true
		shell.version++
		if !o.sealed.Load() {
			shell.dirty = true
		}
		return
	}
	if _, enforced := o.admissionCutoff(); enforced && minute < cutoff {
		return
	}
	o.shells[minute] = &foldShell{
		minute:      minute,
		counts:      make(map[attemptFact]int64),
		emptyMarked: true,
		version:     1,
		dirty:       !o.sealed.Load(),
	}
}

func hasLegacyEdges(fm *FlowMinute) bool {
	return anyLegacyCounts(fm.edges, fm.counts)
}

// lookup materializes one minute for diagnostics and tests (draining pending
// cells first, so reads observe every folded fact). Caller-facing.
func (o *FlowOwner) lookup(minute int64) (*FlowMinute, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.drainFoldLocked()
	shell, ok := o.shells[minute]
	if !ok {
		return nil, false
	}
	return materializeShell(shell), true
}

func (o *FlowOwner) snapshotAll() map[int64]*FlowMinute {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.drainFoldLocked()
	out := make(map[int64]*FlowMinute, len(o.shells))
	for minute, shell := range o.shells {
		out[minute] = materializeShell(shell)
	}
	return out
}

// collectUnsettledMinutes adds dirty-or-leased minute IDs for sync sequence
// pruning (draining first so undrained cells pin their minutes). Clean
// retained reconstruction state pins no sequence.
func (o *FlowOwner) collectUnsettledMinutes(dst map[int64]struct{}) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.drainFoldLocked()
	for minute, shell := range o.shells {
		if shell.dirty || shell.leased {
			dst[minute] = struct{}{}
		}
	}
}

func (o *FlowOwner) pendingTotal() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := len(o.shells)
	if o.cells.pending.Load() > 0 {
		n++
	}
	return n
}

func (o *FlowOwner) minuteCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.shells)
}

func (o *FlowOwner) pendingBytesLocked() int64 {
	var n int64
	for _, shell := range o.shells {
		n += EstimatedFlowMinuteBytes + int64(len(shell.counts))*EstimatedFlowRowBytes
	}
	return n
}

func (o *FlowOwner) pendingBytesSnapshot() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.pendingBytesLocked()
}

// ownerCutoffLocked is the late-arrival horizon: horizon clock truncated to
// the minute minus 600 seconds. Caller must hold o.mu.
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
