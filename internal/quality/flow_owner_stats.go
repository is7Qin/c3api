// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import "github.com/is7qin/c3api/internal/worker"

func (o *FlowOwner) Stats() any { return o.SnapshotStats() }

func (o *FlowOwner) SnapshotStats() FlowOwnerStats {
	o.mu.Lock()
	pendingMinutes, pendingBytes := len(o.pending), o.pendingBytes
	o.mu.Unlock()
	return FlowOwnerStats{
		Running: o.running.Load(), Queued: len(o.queue), QueueCap: FlowOwnerQueueCap,
		Accepted: o.accepted.Load(), Processed: o.processed.Load(), Overflowed: o.overflowed.Load(),
		EdgeRowsAccepted: o.edgeRowsAccepted.Load(), EdgeRowsDropped: o.edgeRowsDropped.Load(),
		PendingMinutes: pendingMinutes, PendingBytes: pendingBytes + o.queuedBytes.Load(), LastMergeDurationMs: o.lastMergeMs.Load(),
		ResidualSubmissions: o.residualSubmissions.Load(), ResidualRows: o.residualRows.Load(), CloseUnixMs: o.closeUnixMs.Load(),
	}
}

var _ worker.Worker = (*FlowOwner)(nil)
var _ interface{ Stats() any } = (*FlowOwner)(nil)
