// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

// Fold-at-source tick-owned reads (v3 F1–F2): production data enters ONLY
// through the request walk (FoldChain/addFact → cells). Every read below
// folds pending cells first, so reads observe every folded fact; growth stays
// tick-expansion-only. (The synchronous edges/empty-marker consumer seam is
// deleted: zero production callers — tests drive the live cell path directly.)

import (
	"time"
)

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

// horizonNow prefers the injected owner clock and falls back to rec.now.
func (o *FlowOwner) horizonNow() time.Time {
	if o.clock != nil {
		return o.clock()
	}
	return o.rec.now()
}
