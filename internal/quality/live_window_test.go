// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Phase 2 RED: Recorder.UnflushedMinutes returns absolute per-minute rows not
// yet in PG — pending (converged/external) rows with their labels plus the
// current partial minute (active cell totals minus already-emitted absolute
// rows, gen-gated, clamped ≥0, fail-closed skip on inconsistency).

var liveFixed = time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)

func liveSum(rows map[int64]map[Key]*QualityMinute, k Key) (attempts, successes int64, n int) {
	for _, byKey := range rows {
		if qm, ok := byKey[k]; ok {
			attempts += qm.Attempts()
			successes += qm.Successes()
			n++
		}
	}
	return attempts, successes, n
}

func liveComplete(t *testing.T, r *Recorder, k Key, success bool, ttft int64, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		obs := Observation{Success: success, ErrClass: ErrClass5xx}
		if success {
			v := ttft
			obs.TTFTMs = &v
			obs.ErrClass = ErrClassNone
			obs.InputTokens = 10
			obs.OutputTokens = 20
		}
		r.Begin(k).CompleteObservation(obs)
	}
}

func TestRecorderUnflushedMinutesPendingAndPartial(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	// Distinct keys: the pending hand row is an independent lifetime (gen 0
	// rows never cancel an active cell's residual; same-generation pending
	// coverage is exercised by the failed-flush test).
	kPend := keyOf(fp(21), qc(21))
	kLive := keyOf(fp(31), qc(31))

	// Pending hand row keeps its label and exact fields.
	old := liveFixed.Add(-10 * time.Minute).Truncate(time.Minute).Unix()
	hand := NewQualityMinute(old, kPend)
	hand.SetAttempts(6)
	hand.SetSuccesses(4)
	hand.SetTTFTCount(2)
	hand.SetSumQ32(12345)
	hand.SetSumSqQ32(67890)
	hand.SetErr429(1)
	hand.SetErr5xx(1)
	hand.SetInputTokens(60)
	hand.SetOutputTokens(40)
	hand.SetCacheReadTokens(7)
	hand.SetCacheCreateTokens(8)
	hand.SetCalls(2)
	hand.SetImages(1)
	require.NoError(t, r.EnqueueQualityMinute(hand))

	// Active partial minute: 7 success (TTFT 100ms) + 3 failures.
	liveComplete(t, r, kLive, true, 100, 7)
	liveComplete(t, r, kLive, false, 0, 3)

	got := r.UnflushedMinutes(liveFixed)
	require.Len(t, got, 2, "pending minute + current partial minute")
	pend, ok := got[old][kPend]
	require.True(t, ok, "pending row keeps its minute label")
	require.Equal(t, int64(6), pend.Attempts())
	require.Equal(t, int64(4), pend.Successes())
	require.Equal(t, int64(2), pend.TTFTCount())
	require.Equal(t, int64(12345), pend.SumQ32())
	require.Equal(t, int64(67890), pend.SumSqQ32())
	require.Equal(t, int64(1), pend.Err429())
	require.Equal(t, int64(1), pend.Err5xx())
	require.Equal(t, int64(60), pend.InputTokens())
	require.Equal(t, int64(40), pend.OutputTokens())
	require.Equal(t, int64(7), pend.CacheReadTokens())
	require.Equal(t, int64(8), pend.CacheCreateTokens())
	require.Equal(t, int64(2), pend.Calls())
	require.Equal(t, int64(1), pend.Images())

	curMin := liveFixed.UTC().Truncate(time.Minute).Unix()
	part, ok := got[curMin][kLive]
	require.True(t, ok, "active residual labeled at the current minute")
	require.Equal(t, int64(10), part.Attempts())
	require.Equal(t, int64(7), part.Successes())
	require.Equal(t, int64(7), part.TTFTCount())
	require.Equal(t, 7*toQ32(100), part.SumQ32())
	require.Equal(t, 7*toSqQ32(100), part.SumSqQ32())
	require.Equal(t, int64(70), part.InputTokens())
	require.Equal(t, int64(140), part.OutputTokens())

	// Advancing now moves the residual label; the pending label is stable.
	rolled := r.UnflushedMinutes(liveFixed.Add(3 * time.Minute))
	require.Contains(t, rolled, old)
	newMin := liveFixed.Add(3 * time.Minute).UTC().Truncate(time.Minute).Unix()
	require.Contains(t, rolled, newMin)
	require.Equal(t, int64(10), rolled[newMin][kLive].Attempts())
}

func TestRecorderUnflushedMinutesPostFlushExclusion(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	k := keyOf(fp(22), qc(22))
	liveComplete(t, r, k, true, 100, 7)
	liveComplete(t, r, k, false, 0, 3)

	pg := newFakePG()
	w := NewSyncWorker(r, nil, pg, SyncConfig{InstanceSrc: "src-live", BatchSize: 500}, nil)
	w.SetClock(func() time.Time { return liveFixed })
	w.doPG(context.Background())
	require.Len(t, pg.quality, 1, "one absolute row flushed")
	require.Equal(t, int64(10), pg.quality[0].Attempts)
	require.Equal(t, int64(7), pg.quality[0].Successes)

	// Flushed ∩ live = ∅: residual is zero and omitted.
	got := r.UnflushedMinutes(liveFixed)
	att, suc, n := liveSum(got, k)
	require.Equal(t, int64(0), att)
	require.Equal(t, int64(0), suc)
	require.Equal(t, 0, n)

	// Post-flush attempts surface as the exact delta; telescoping holds:
	// flushed (10) + live (5) == cell totals (15).
	liveComplete(t, r, k, true, 100, 5)
	got = r.UnflushedMinutes(liveFixed)
	att, suc, n = liveSum(got, k)
	require.Equal(t, int64(5), att)
	require.Equal(t, int64(5), suc)
	require.Equal(t, 1, n)
	a, _, _, _, _, _, _, _, _, _, ok := r.CellStats(k)
	require.True(t, ok)
	require.Equal(t, int64(15), a)
	require.Equal(t, pg.quality[0].Attempts+att, a, "flushed + live == cell totals")
}

func TestRecorderUnflushedMinutesFailedFlushStaysLive(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	k := keyOf(fp(23), qc(23))
	liveComplete(t, r, k, true, 100, 10)

	pg := newFakePG()
	pg.failAll = true
	w := NewSyncWorker(r, nil, pg, SyncConfig{InstanceSrc: "src-live-fail", BatchSize: 500}, nil)
	w.SetClock(func() time.Time { return liveFixed })
	w.doPG(context.Background())
	require.Empty(t, pg.quality)

	// Failed rows refill to pending; the residual must not double-count them:
	// exactly one row carrying the 10 attempts.
	got := r.UnflushedMinutes(liveFixed)
	att, _, n := liveSum(got, k)
	require.Equal(t, int64(10), att)
	require.Equal(t, 1, n, "refill + residual stay disjoint")

	// 3 newer attempts: pending refill (old label) + fresh residual (new label).
	liveComplete(t, r, k, true, 100, 3)
	rolled := r.UnflushedMinutes(liveFixed.Add(3 * time.Minute))
	att, _, _ = liveSum(rolled, k)
	require.Equal(t, int64(13), att)
	oldMin := liveFixed.UTC().Truncate(time.Minute).Unix()
	newMin := liveFixed.Add(3 * time.Minute).UTC().Truncate(time.Minute).Unix()
	require.Equal(t, int64(10), rolled[oldMin][k].Attempts(), "refill keeps flush-minute label")
	require.Equal(t, int64(3), rolled[newMin][k].Attempts(), "residual labeled at the new minute")
}

func TestRecorderUnflushedMinutesClampedSkip(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	k := keyOf(fp(24), qc(24))
	liveComplete(t, r, k, true, 100, 7)
	liveComplete(t, r, k, false, 0, 3)

	// Inconsistent emission (more attempts emitted than observed) clamps the
	// attempts to zero; successes would then exceed attempts → row skipped.
	huge := NewQualityMinute(0, k)
	huge.SetAttempts(1000)
	r.MarkEmitted(k, huge)
	got := r.UnflushedMinutes(liveFixed)
	_, _, n := liveSum(got, k)
	require.Equal(t, 0, n, "inconsistent residual is skipped fail-closed")

	// Partial emission stays exact.
	r2, err := NewRecorder(50000)
	require.NoError(t, err)
	liveComplete(t, r2, k, true, 100, 7)
	liveComplete(t, r2, k, false, 0, 3)
	part := NewQualityMinute(0, k)
	part.SetAttempts(3)
	r2.MarkEmitted(k, part)
	got = r2.UnflushedMinutes(liveFixed)
	att, suc, n := liveSum(got, k)
	require.Equal(t, int64(7), att)
	require.Equal(t, int64(7), suc)
	require.Equal(t, 1, n)
}

func TestRecorderUnflushedMinutesGenRotation(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	k := keyOf(fp(25), qc(25))
	liveComplete(t, r, k, true, 100, 10)

	pg := newFakePG()
	w := NewSyncWorker(r, nil, pg, SyncConfig{InstanceSrc: "src-live-gen", BatchSize: 500}, nil)
	w.SetClock(func() time.Time { return liveFixed })
	w.doPG(context.Background())
	require.Len(t, pg.quality, 1)

	// Retire exports the old lifetime; a recreated cell must not be cancelled
	// by the previous generation's emission ledger.
	r.Retire(k)
	liveComplete(t, r, k, true, 100, 3)
	got := r.UnflushedMinutes(liveFixed)
	att, _, _ := liveSum(got, k)
	require.Equal(t, int64(13), att, "old lifetime row + new residual, no gen pollution")
	var sizes []int64
	for _, byKey := range got {
		if qm, ok := byKey[k]; ok {
			sizes = append(sizes, qm.Attempts())
		}
	}
	require.ElementsMatch(t, []int64{10, 3}, sizes)
}

func TestRecorderUnflushedMinutesEmpty(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	require.Empty(t, r.UnflushedMinutes(liveFixed))
}
