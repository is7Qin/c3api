// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import "github.com/is7qin/c3api/internal/repository"

// mergeLocked folds one absolute flow-minute delta into the accumulator with
// the established identity rules: identical edges sum ChainCount, distinct
// edges remain distinct, the empty marker never erases conserved rows, and
// incoming rows lead the merged order. Capacity is enforced with the shared
// lane settings and flow-lane eviction (recorder flowOverflow/minuteOverflow
// counters); quality and flow budgets are independent but each lane is
// individually bounded. No retention path aliases fm.flowRows — every stored
// row comes from Clone or mergeFlowRowsBounded — so owned rows (queued
// submissions via adoptFlowSnapshot) need no pre-copy. Caller must hold o.mu.
func (o *FlowOwner) mergeLocked(fm *FlowMinute) error {
	o.mergeDropped = 0
	if existing, ok := o.pending[fm.minute]; ok {
		if fm.emptySnapshot {
			if len(existing.flowRows) == 0 {
				*existing = *fm.Clone()
			}
			return nil
		}
		if len(fm.flowRows) > 0 {
			existing.emptySnapshot = false
			byteCap := o.rec.flowRowBytesCap
			room := o.rec.pendingCapBytes - (o.pendingBytes - flowMinuteCharge(existing))
			if room < byteCap {
				byteCap = room
			}
			if byteCap < 0 {
				byteCap = 0
			}
			merged, dropped := mergeFlowRowsBounded(existing.flowRows, fm.flowRows, o.rec.flowRowCap, byteCap)
			existing.flowRows = merged
			o.mergeDropped = dropped
			return nil
		}
		*existing = *fm.Clone()
		return nil
	}
	charge := flowMinuteCharge(fm)
	if charge > o.rec.pendingCapBytes {
		o.mergeDropped = int64(len(fm.flowRows))
		return ErrCapacity
	}
	for o.pendingBytes+charge > o.rec.pendingCapBytes || len(o.pending) >= o.rec.minuteCap {
		if !o.evictOldestLocked() {
			o.mergeDropped = int64(len(fm.flowRows))
			return ErrCapacity
		}
	}
	bucket := *fm // value copy: minute/edges/counts/emptySnapshot are all inline
	bucket.flowRows, o.mergeDropped = mergeFlowRowsBounded(nil, fm.flowRows, o.rec.flowRowCap, o.rec.flowRowBytesCap)
	o.pending[fm.minute] = &bucket
	o.pendingBytes += charge
	return nil
}

func flowMinuteCharge(fm *FlowMinute) int64 {
	_ = fm
	return int64(EstimatedFlowMinuteBytes)
}

func (o *FlowOwner) evictOldestLocked() bool {
	if len(o.pending) == 0 {
		return false
	}
	var oldest int64
	first := true
	for minute := range o.pending {
		if first || minute < oldest {
			oldest, first = minute, false
		}
	}
	removed := o.pending[oldest]
	delete(o.pending, oldest)
	o.pendingBytes -= flowMinuteCharge(removed)
	o.rec.flowOverflow.Add(1)
	o.rec.minuteOverflow.Add(1)
	o.edgeRowsDropped.Add(int64(len(removed.flowRows)))
	return true
}

// mergeAcceptedLocked merges one queued submission with accepted-event
// attribution (owner loop / consumer drain). A terminal capacity rejection
// still counts the event processed; its rows land in edge_rows_dropped so
// edge_rows_accepted + edge_rows_dropped + residual_rows == offered rows.
// The submission's rows are already owner-owned (deep-copied at Submit) and
// are adopted without a second copy. Caller must hold o.mu.
func (o *FlowOwner) mergeAcceptedLocked(sub flowSubmission) {
	if o.rec.finalSnapshot.Load() != nil {
		o.residualSubmissions.Add(1)
		o.residualRows.Add(int64(len(sub.rows)))
		return
	}
	start := o.rec.now()
	err := o.mergeLocked(adoptFlowSnapshot(sub.minute, sub.rows))
	o.lastMergeMs.Store(o.rec.now().Sub(start).Milliseconds())
	o.processed.Add(1)
	o.edgeRowsAccepted.Add(int64(len(sub.rows)) - o.mergeDropped)
	o.edgeRowsDropped.Add(o.mergeDropped)
	if err != nil {
		o.rec.flowOverflow.Add(1)
	} else if o.mergeDropped > 0 {
		o.rec.flowOverflow.Add(1)
	}
}

func (o *FlowOwner) drainQueueLocked() {
	for {
		select {
		case sub := <-o.queue:
			o.queuedBytes.Add(-int64(len(sub.rows)) * EstimatedFlowRowBytes)
			o.mergeAcceptedLocked(sub)
		default:
			return
		}
	}
}

func (o *FlowOwner) closeDrainLocked() {
	for {
		select {
		case sub := <-o.queue:
			o.queuedBytes.Add(-int64(len(sub.rows)) * EstimatedFlowRowBytes)
			if o.rec.finalSnapshot.Load() != nil {
				o.residualSubmissions.Add(1)
				o.residualRows.Add(int64(len(sub.rows)))
				continue
			}
			err := o.mergeLocked(adoptFlowSnapshot(sub.minute, sub.rows))
			o.residualSubmissions.Add(1)
			o.residualRows.Add(int64(len(sub.rows)) - o.mergeDropped)
			o.edgeRowsDropped.Add(o.mergeDropped)
			if err != nil {
				o.rec.flowOverflow.Add(1)
			} else if o.mergeDropped > 0 {
				o.rec.flowOverflow.Add(1)
			}
		default:
			return
		}
	}
}

// mergeFlowRowsBounded is the accumulator's single retention funnel: every
// stored row is a fresh clone (slice slot and PreviousAccountID pointer), so
// neither input may alias retained state. Incoming rows lead the merged
// order; only incoming rows past the caps count as drops (displaced existing
// rows were already accounted when accepted).
func mergeFlowRowsBounded(existing, incoming []repository.RoutingFlowRow, rowCap int, byteCap int64) ([]repository.RoutingFlowRow, int64) {
	index := make(map[flowEdgeIdentity]int, len(existing)+len(incoming))
	merged := make([]repository.RoutingFlowRow, 0, minInt(rowCap, len(existing)+len(incoming)))
	var dropped int64
	for _, row := range incoming {
		key := flowEdgeIdentityOf(row)
		if i, ok := index[key]; ok {
			merged[i].ChainCount += row.ChainCount
			continue
		}
		if len(merged) >= rowCap || int64(len(merged)+1)*EstimatedFlowRowBytes > byteCap {
			dropped++
			continue
		}
		index[key] = len(merged)
		merged = append(merged, cloneFlowRow(row))
	}
	for _, row := range existing {
		key := flowEdgeIdentityOf(row)
		if i, ok := index[key]; ok {
			merged[i].ChainCount += row.ChainCount
			continue
		}
		if len(merged) >= rowCap || int64(len(merged)+1)*EstimatedFlowRowBytes > byteCap {
			continue
		}
		index[key] = len(merged)
		merged = append(merged, cloneFlowRow(row))
	}
	return merged, dropped
}

func cloneFlowRow(row repository.RoutingFlowRow) repository.RoutingFlowRow {
	if row.PreviousAccountID != nil {
		v := *row.PreviousAccountID
		row.PreviousAccountID = &v
	}
	return row
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
