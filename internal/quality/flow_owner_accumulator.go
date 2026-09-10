// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

// Owner merge entry points: mergeLocked folds one absolute flow-minute delta
// into the single-owner cumulative accumulator in place at O(chain length)
// (see flow_accumulator.go for the fold rules); mergeAcceptedLocked adds the
// accepted-event attribution for queued submissions. Caller must hold o.mu
// for every function here.

// mergeAcceptedLocked merges one queued submission with accepted-event
// attribution (owner loop, consumer drain, seal drain). A terminal capacity
// rejection still counts the event processed; its rows land in
// edge_rows_dropped so edge_rows_accepted + edge_rows_dropped +
// residual_rows == offered Submit rows. Caller must hold o.mu.
func (o *FlowOwner) mergeAcceptedLocked(sub flowSubmission) {
	charge := int64(len(sub.rows)) * EstimatedFlowRowBytes
	o.queuedBytes.Add(-charge)
	if !sub.residual {
		o.queuedPG.Add(-1)
	}
	if o.rec.finalSnapshot.Load() != nil || sub.residual || o.pgSealed.Load() {
		o.foldResidualLocked(sub.minute, sub.rows, o.ownerCutoffLocked())
		o.residualSubmissions.Add(1)
		return
	}
	if len(sub.rows) == 0 {
		o.processed.Add(1)
		return
	}
	start := o.rec.now()
	_, dropped, err := o.mergeLocked(adoptFlowSnapshot(sub.minute, sub.rows), true)
	o.lastMergeMs.Store(o.rec.now().Sub(start).Milliseconds())
	o.processed.Add(1)
	if err != nil || dropped > 0 {
		o.rec.flowOverflow.Add(1)
	}
}

// drainQueueLocked merges every queued submission; every pre-seal accepted
// drain returns queuedPG toward zero exactly once per submission. Caller
// must hold o.mu.
func (o *FlowOwner) drainQueueLocked() {
	for {
		select {
		case sub := <-o.queue:
			o.mergeAcceptedLocked(sub)
		default:
			return
		}
	}
}

// closeDrainLocked drains the accepted queue as residual-classified terminal
// state (owner Close path). Caller must hold o.mu.
func (o *FlowOwner) closeDrainLocked() {
	for {
		select {
		case sub := <-o.queue:
			charge := int64(len(sub.rows)) * EstimatedFlowRowBytes
			o.queuedBytes.Add(-charge)
			if !sub.residual {
				o.queuedPG.Add(-1)
			}
			o.foldResidualLocked(sub.minute, sub.rows, o.ownerCutoffLocked())
			o.residualSubmissions.Add(1)
		default:
			return
		}
	}
}
