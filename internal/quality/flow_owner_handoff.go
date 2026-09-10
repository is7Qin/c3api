// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import "sort"

// Flow sync-facing handoff: versioned snapshot/ack with lease, incarnation,
// and independent lease identity, serialized by the sync flushMu. Sync never
// owns flow rows; it snapshots one dirty minute at a time under the owner
// lock, writes the full cumulative snapshot with replacement
// UpsertFlowSnapshot, and settles through the token. Failure means no ack and
// no refill: owner state stays dirty and retries next cycle.

// enqueue is the private synchronous consumer seam (legacy edges/empty-marker
// forms and tests): identical signature and ownership semantics as before —
// the fold deep-copies, never retaining the caller's slice or pointers.
// After PG seal the ingestion is residual-classified like the Submit path
// (foldResidualLocked): rows fold residual without ever setting dirty, and
// markers/legacy edges are preserved without dirtying, so a post-seal
// synchronous enqueue can never create PG-open dirty accepted state.
// Caller-facing; takes o.mu.
func (o *FlowOwner) enqueue(fm *FlowMinute) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.submitState.Load()&closedBit != 0 {
		return ErrCapacity
	}
	if o.pgSealed.Load() {
		return o.enqueueSealedLocked(fm)
	}
	_, _, err := o.mergeLocked(fm, false)
	return err
}

// enqueueSealedLocked residual-classifies post-seal consumer ingestion under
// the seal contract: terminal drains never drop by age and never dirty.
// Caller must hold o.mu.
func (o *FlowOwner) enqueueSealedLocked(fm *FlowMinute) error {
	if fm == nil {
		return ErrCapacity
	}
	cutoff := o.ownerCutoffLocked()
	if fm.emptySnapshot {
		_, _, err := o.foldEmptyLocked(fm.minute, cutoff)
		return err
	}
	if len(fm.flowRows) > 0 {
		o.foldResidualLocked(fm.minute, fm.flowRows, cutoff)
		return nil
	}
	if hasLegacyEdges(fm) {
		if acc, ok := o.pending[fm.minute]; ok {
			acc.edges = fm.edges
			acc.counts = fm.counts
			acc.version++
			return nil
		}
		acc, aerr := o.admitMinuteLocked(fm.minute, cutoff)
		if aerr != nil {
			return nil
		}
		acc.edges = fm.edges
		acc.counts = fm.counts
		acc.version++
		return nil
	}
	return nil
}

func (o *FlowOwner) lookup(minute int64) (*FlowMinute, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.drainQueueLocked()
	acc, ok := o.pending[minute]
	if !ok {
		return nil, false
	}
	return acc.materialize(), true
}

func (o *FlowOwner) snapshotAll() map[int64]*FlowMinute {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.drainQueueLocked()
	out := make(map[int64]*FlowMinute, len(o.pending))
	for minute, acc := range o.pending {
		out[minute] = acc.materialize()
	}
	return out
}

// collectUnsettledMinutes adds dirty-or-leased minute IDs for sync sequence
// pruning. Clean retained reconstruction state pins no sequence.
func (o *FlowOwner) collectUnsettledMinutes(dst map[int64]struct{}) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.drainQueueLocked()
	for minute, acc := range o.pending {
		if acc.dirty || acc.leased {
			dst[minute] = struct{}{}
		}
	}
}

func (o *FlowOwner) pendingTotal() int {
	o.mu.Lock()
	pending := len(o.pending)
	o.mu.Unlock()
	return pending + len(o.queue)
}

func (o *FlowOwner) pendingBytesSnapshot() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.pendingBytes
}

// pgCandidateMinutes returns dirty, unleased, PG-open minute IDs in
// deterministic oldest-first order after draining the queue. Each candidate
// is attempted at most once per flush cycle, so there is no spin.
// Caller-facing; takes o.mu.
func (o *FlowOwner) pgCandidateMinutes() []int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.drainQueueLocked()
	if o.pgSealed.Load() {
		return nil
	}
	var out []int64
	for minute, acc := range o.pending {
		if acc.dirty && !acc.leased {
			out = append(out, minute)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// redisCandidateMinutes returns PG-open pre-seal retained minute IDs in
// deterministic oldest-first order with no row work. Read-only: no lease, no
// ack. Caller-facing; takes o.mu.
func (o *FlowOwner) redisCandidateMinutes(curMinute int64) []int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.drainQueueLocked()
	if o.pgSealed.Load() {
		return nil
	}
	var out []int64
	for minute := range o.pending {
		if minute <= curMinute {
			out = append(out, minute)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// snapshotForPG atomically acquires a per-minute lease with
// leaseID = ++owner.nextLeaseID, captures the token, and returns one
// deep-owned payload. Same-minute merges arriving while the lease is active
// fold into the live accumulator (bump version, stay dirty, exceed the
// captured watermark) and never mutate the materialized payload. At most one
// O(R) payload is live per call; callers consume it synchronously before the
// next snapshot. Caller-facing; takes o.mu.
func (o *FlowOwner) snapshotForPG(minute int64) (*FlowMinute, flowSnapshotToken, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.drainQueueLocked()
	if o.pgSealed.Load() {
		return nil, flowSnapshotToken{}, false
	}
	acc, ok := o.pending[minute]
	if !ok || !acc.dirty || acc.leased {
		return nil, flowSnapshotToken{}, false
	}
	o.nextLeaseID++
	acc.leased = true
	acc.activeLeaseID = o.nextLeaseID
	acc.capturedVersion = acc.version
	acc.leaseAcceptedWatermark = acc.totalAcceptedContrib
	tok := flowSnapshotToken{
		minute:            minute,
		incarnation:       acc.incarnation,
		leaseID:           acc.activeLeaseID,
		capturedVersion:   acc.capturedVersion,
		acceptedWatermark: acc.leaseAcceptedWatermark,
	}
	return acc.materialize(), tok, true
}

// snapshotForRedis deep-materializes exactly one retained minute with
// read-only semantics: no lease, no ack, owner retention untouched.
// Caller-facing; takes o.mu.
func (o *FlowOwner) snapshotForRedis(minute int64) *FlowMinute {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.pgSealed.Load() {
		return nil
	}
	acc, ok := o.pending[minute]
	if !ok {
		return nil
	}
	return acc.materialize()
}

// matchLeaseLocked is the three-field identity match (minute plus
// incarnation plus activeLeaseID with an active lease). The captured version
// is only for the clean-vs-dirty decision, never for identity, so a stale
// token from any prior lease is a no-op even when the version is unchanged.
func (o *FlowOwner) matchLeaseLocked(tok flowSnapshotToken) (*flowMinuteAccumulator, bool) {
	acc, ok := o.pending[tok.minute]
	if !ok || !acc.leased || acc.incarnation != tok.incarnation || acc.activeLeaseID != tok.leaseID {
		return nil, false
	}
	return acc, true
}

// ackPG settles a lease on success: advances the persisted watermark to the
// captured value, marks everPersisted (including empty zero-row snapshots),
// clears the lease, and marks clean only when the version is unchanged and
// PG is not sealed. Ack never overwrites new state; any mismatch is a no-op.
// Caller-facing; takes o.mu.
func (o *FlowOwner) ackPG(tok flowSnapshotToken) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	acc, ok := o.matchLeaseLocked(tok)
	if !ok {
		return false
	}
	if tok.acceptedWatermark > acc.persistedAcceptedWatermark {
		acc.persistedAcceptedWatermark = tok.acceptedWatermark
	}
	acc.everPersisted = true
	acc.leased = false
	acc.activeLeaseID = 0
	if acc.version == tok.capturedVersion && !o.pgSealed.Load() {
		acc.dirty = false
	}
	return true
}

// releasePG settles a lease on failure, deferral, expiry, or unwind: clears
// the lease and leaves the minute dirty with watermarks unchanged for
// next-cycle retry. A stale token from any prior lease is a no-op and clears
// nothing. Caller-facing; takes o.mu.
func (o *FlowOwner) releasePG(tok flowSnapshotToken) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	acc, ok := o.matchLeaseLocked(tok)
	if !ok {
		return false
	}
	acc.leased = false
	acc.activeLeaseID = 0
	return true
}

// sealPG revokes PG candidacy at shutdown: it drains queued pre-seal
// submissions first (merged accepted while unsealed), sets pgSealed, revokes
// every active lease so every late ack/release is a stale-token no-op, and
// moves ALL unconfirmed accepted credits
// (totalAcceptedContrib - persistedAcceptedWatermark) from accepted to
// residual exactly once for every minute including leased ones. Sealed minutes
// retain cumulative rows for diagnostics only and are never published after
// seal. Idempotent; the sweep runs exactly once per owner lifetime.
// Called by SyncWorker.Close on every terminal path.
func (o *FlowOwner) sealPG() {
	o.submitFence.Lock()
	defer o.submitFence.Unlock()
	o.mu.Lock()
	defer o.mu.Unlock()
	o.drainQueueLocked()
	if o.pgSealed.Load() {
		return
	}
	o.pgSealed.Store(true)
	for _, acc := range o.pending {
		acc.leased = false
		acc.activeLeaseID = 0
		if unconfirmed := acc.totalAcceptedContrib - acc.persistedAcceptedWatermark; unconfirmed > 0 {
			o.edgeRowsAccepted.Add(-unconfirmed)
			o.residualRows.Add(unconfirmed)
			acc.residualContrib += unconfirmed
			acc.persistedAcceptedWatermark = acc.totalAcceptedContrib
		}
	}
}

// pgWorkTotal is the bounded Close-drain and PendingFlow signal without
// scanning any channel: queued pre-seal submissions plus dirty unleased
// PG-open minutes plus active leases. Clean retained reconstruction state
// contributes nothing, so it cannot block Close or inflate pending work.
func (o *FlowOwner) pgWorkTotal() int {
	total := int(o.queuedPG.Load())
	if o.pgSealed.Load() {
		return total
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.pgSealed.Load() {
		return int(o.queuedPG.Load())
	}
	for _, acc := range o.pending {
		if acc.leased || acc.dirty {
			total++
		}
	}
	return total
}
