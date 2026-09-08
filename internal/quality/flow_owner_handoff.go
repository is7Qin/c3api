// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import "math"

func (o *FlowOwner) enqueue(fm *FlowMinute) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.submitState.Load()&closedBit != 0 {
		return ErrCapacity
	}
	return o.mergeLocked(fm)
}

func (o *FlowOwner) lookup(minute int64) (*FlowMinute, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.drainQueueLocked()
	fm, ok := o.pending[minute]
	if !ok {
		return nil, false
	}
	return fm.Clone(), true
}

func (o *FlowOwner) takePending() map[int64]*FlowMinute {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.drainQueueLocked()
	if len(o.pending) == 0 {
		return nil
	}
	taken := o.pending
	o.pending = make(map[int64]*FlowMinute)
	o.pendingBytes = 0
	return taken
}

func (o *FlowOwner) dueSnapshot(curMinute int64) map[int64]*FlowMinute {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.drainQueueLocked()
	out := make(map[int64]*FlowMinute, len(o.pending))
	for minute, fm := range o.pending {
		if minute <= curMinute {
			out[minute] = fm.Clone()
		}
	}
	return out
}

func (o *FlowOwner) snapshotAll() map[int64]*FlowMinute { return o.dueSnapshot(math.MaxInt64) }

func (o *FlowOwner) collectLiveMinutes(dst map[int64]struct{}) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.drainQueueLocked()
	for minute := range o.pending {
		dst[minute] = struct{}{}
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
