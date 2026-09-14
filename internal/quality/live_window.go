// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"math"
	"time"
)

// Live window seam (incident-wiring Phase 2): absolute per-minute rows not
// yet in PG. Two disjoint parts:
//
//  1. pendingQuality rows (converged-cell exports + external/hand rows):
//     unflushed by construction — the sync worker drains them out of the map
//     on every PG flush and only refills failures. Labels preserved.
//  2. current partial minute: per active cell, lifetime totals minus
//     already-emitted absolute rows (the sync worker reports each durably
//     persisted increment via MarkEmitted, mirroring its committed ledger),
//     minus same-generation pending rows (failed-flush refills of this cell),
//     labeled at the current minute.
//
// Gen discipline: rows and ledger entries carry the owning cell generation;
// a recreated cell (new gen) never inherits the dead generation's emission
// (no under-count pollution) and dead-generation pending rows never cancel
// the new residual (they are disjoint lifetimes, merged by summation).
// Generation 0 (hand-made/external rows) is a wildcard matching any cell.
//
// Fail-closed: per-field clamp at zero; a residual with successes > attempts
// (impossible state) is skipped wholesale; all-zero residuals are omitted.
// Callers receive clones — never aliases of live cells or pending rows.

// emittedTotals is the cumulative PG-flushed absolute for one key+generation.
type emittedTotals struct {
	attempts    int64
	successes   int64
	err429      int64
	err4xx      int64
	err5xx      int64
	errNetwork  int64
	ttftCount   int64
	sumQ32      int64
	sumSqQ32    int64
	hist        [10]int64
	input       int64
	output      int64
	cacheRead   int64
	cacheCreate int64
	calls       int64
	images      int64
}

type emittedEntry struct {
	gen    uint64
	totals emittedTotals
}

func totalsOf(qm *QualityMinute) emittedTotals {
	return emittedTotals{
		attempts: qm.attempts, successes: qm.successes,
		err429: qm.err429, err4xx: qm.err4xx, err5xx: qm.err5xx, errNetwork: qm.errNetwork,
		ttftCount: qm.ttftCount, sumQ32: qm.sumQ32, sumSqQ32: qm.sumSqQ32,
		hist:  qm.hist,
		input: qm.inputTokens, output: qm.outputTokens,
		cacheRead: qm.cacheRead, cacheCreate: qm.cacheCreate,
		calls: qm.calls, images: qm.images,
	}
}

// add saturates at MaxInt64 (fail-safe: over-counted emission can only shrink
// the live residual toward omission, never inflate it).
func (t *emittedTotals) add(o emittedTotals) {
	add := func(a, b int64) int64 {
		if a > math.MaxInt64-b {
			return math.MaxInt64
		}
		return a + b
	}
	t.attempts = add(t.attempts, o.attempts)
	t.successes = add(t.successes, o.successes)
	t.err429 = add(t.err429, o.err429)
	t.err4xx = add(t.err4xx, o.err4xx)
	t.err5xx = add(t.err5xx, o.err5xx)
	t.errNetwork = add(t.errNetwork, o.errNetwork)
	t.ttftCount = add(t.ttftCount, o.ttftCount)
	t.sumQ32 = add(t.sumQ32, o.sumQ32)
	t.sumSqQ32 = add(t.sumSqQ32, o.sumSqQ32)
	for i := range t.hist {
		t.hist[i] = add(t.hist[i], o.hist[i])
	}
	t.input = add(t.input, o.input)
	t.output = add(t.output, o.output)
	t.cacheRead = add(t.cacheRead, o.cacheRead)
	t.cacheCreate = add(t.cacheCreate, o.cacheCreate)
	t.calls = add(t.calls, o.calls)
	t.images = add(t.images, o.images)
}

// MarkEmitted records one durably PG-persisted absolute increment for key.
// Called by the sync worker at its markQualityCommitted choke point (every
// successful insert path funnels there); best-effort and infallible. Gen
// comes stamped on the row: a nonzero generation Rotating entry resets the
// totals (new cell lifetime starts at zero); generation 0 adds to whatever
// entry exists (external rows predate the gen discipline).
func (r *Recorder) MarkEmitted(key Key, delta *QualityMinute) {
	if r == nil || delta == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.emitted == nil {
		r.emitted = make(map[Key]emittedEntry)
	}
	gen := delta.gen
	inc := totalsOf(delta)
	e, ok := r.emitted[key]
	if !ok {
		r.emitted[key] = emittedEntry{gen: gen, totals: inc}
		return
	}
	if e.gen != gen && gen != 0 && e.gen != 0 {
		r.emitted[key] = emittedEntry{gen: gen, totals: inc}
		return
	}
	if e.gen == 0 && gen != 0 {
		e.gen = gen
	}
	e.totals.add(inc)
	r.emitted[key] = e
}

// subClamped subtracts prev field-wise, clamping negatives to zero.
func (q *QualityMinute) subClamped(prev *QualityMinute) {
	if prev == nil {
		return
	}
	sub := func(a, b int64) int64 {
		if r := a - b; r > 0 {
			return r
		}
		return 0
	}
	q.attempts = sub(q.attempts, prev.attempts)
	q.successes = sub(q.successes, prev.successes)
	q.err429 = sub(q.err429, prev.err429)
	q.err4xx = sub(q.err4xx, prev.err4xx)
	q.err5xx = sub(q.err5xx, prev.err5xx)
	q.errNetwork = sub(q.errNetwork, prev.errNetwork)
	q.ttftCount = sub(q.ttftCount, prev.ttftCount)
	q.sumQ32 = sub(q.sumQ32, prev.sumQ32)
	q.sumSqQ32 = sub(q.sumSqQ32, prev.sumSqQ32)
	for i := range q.hist {
		q.hist[i] = sub(q.hist[i], prev.hist[i])
	}
	q.inputTokens = sub(q.inputTokens, prev.inputTokens)
	q.outputTokens = sub(q.outputTokens, prev.outputTokens)
	q.cacheRead = sub(q.cacheRead, prev.cacheRead)
	q.cacheCreate = sub(q.cacheCreate, prev.cacheCreate)
	q.calls = sub(q.calls, prev.calls)
	q.images = sub(q.images, prev.images)
}

// subEmitted subtracts ledger totals field-wise, clamping at zero.
func (q *QualityMinute) subEmitted(tot emittedTotals) {
	prev := &QualityMinute{
		attempts: tot.attempts, successes: tot.successes,
		err429: tot.err429, err4xx: tot.err4xx, err5xx: tot.err5xx, errNetwork: tot.errNetwork,
		ttftCount: tot.ttftCount, sumQ32: tot.sumQ32, sumSqQ32: tot.sumSqQ32,
		hist:        tot.hist,
		inputTokens: tot.input, outputTokens: tot.output,
		cacheRead: tot.cacheRead, cacheCreate: tot.cacheCreate,
		calls: tot.calls, images: tot.images,
	}
	q.subClamped(prev)
}

// isEmpty reports a zero row (nothing unflushed).
func (q *QualityMinute) isEmpty() bool {
	return q.attempts == 0 && q.successes == 0 &&
		q.err429 == 0 && q.err4xx == 0 && q.err5xx == 0 && q.errNetwork == 0 &&
		q.ttftCount == 0 && q.sumQ32 == 0 && q.sumSqQ32 == 0 &&
		q.hist == [10]int64{} &&
		q.inputTokens == 0 && q.outputTokens == 0 && q.cacheRead == 0 && q.cacheCreate == 0 &&
		q.calls == 0 && q.images == 0
}

// UnflushedMinutes returns absolute per-minute rows not yet in PG: pending
// rows with their labels plus one current-minute residual per active cell
// with unflushed contributions. Compile-lane read-only accessor (same cold
// path discipline as LiveCells); the returned clones never alias recorder
// state. Nil when nothing is unflushed.
func (r *Recorder) UnflushedMinutes(now time.Time) map[int64]map[Key]*QualityMinute {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out map[int64]map[Key]*QualityMinute
	put := func(minute int64, k Key, qm *QualityMinute) {
		if out == nil {
			out = make(map[int64]map[Key]*QualityMinute)
		}
		rows, ok := out[minute]
		if !ok {
			rows = make(map[Key]*QualityMinute)
			out[minute] = rows
		}
		if existing, ok := rows[k]; ok {
			existing.merge(qm)
		} else {
			rows[k] = qm
		}
	}
	// Pending rows first: unflushed by construction, labels preserved.
	for minute, rows := range r.pendingQuality {
		for k, v := range rows {
			put(minute, k, v.Clone())
		}
	}
	curMin := now.UTC().Truncate(time.Minute).Unix()
	for k, c := range r.active {
		if c.attempts.Load() == 0 {
			continue
		}
		res := r.cellQualityMinute(c, curMin)
		// Subtract same-generation pending coverage (failed-flush refills of
		// this cell live in pendingQuality; generation 0 rows are wildcards).
		for _, rows := range r.pendingQuality {
			if pend, ok := rows[k]; ok && (pend.gen == 0 || pend.gen == c.gen) {
				res.subClamped(pend)
			}
		}
		// Subtract emitted absolutes (same generation or wildcard entry).
		if e, ok := r.emitted[k]; ok && (e.gen == 0 || e.gen == c.gen) {
			res.subEmitted(e.totals)
		}
		if res.isEmpty() {
			continue
		}
		if res.successes > res.attempts {
			continue // impossible state — fail-closed skip
		}
		put(curMin, k, res)
	}
	// Prune the ledger to live generations: entries for keys with no active
	// cell, or a rotated generation, can never feed a future residual
	// (recreated on demand by late MarkEmitted calls).
	for k, e := range r.emitted {
		c, ok := r.active[k]
		if !ok || (e.gen != 0 && e.gen != c.gen) {
			delete(r.emitted, k)
		}
	}
	return out
}
