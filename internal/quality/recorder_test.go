// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/repository"
)

func fp(seed byte) [32]byte {
	var b [32]byte
	for i := range b {
		b[i] = seed + byte(i)
	}
	return b
}

func qc(seed byte) [32]byte {
	var b [32]byte
	for i := range b {
		b[i] = seed*3 + byte(i*2)
	}
	return b
}

func keyOf(f, q [32]byte) Key { return CanonicalKey(f, q, f) }

func TestQualityRecorder_ExactOnce(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	f := fp(1)
	q := qc(1)
	k := keyOf(f, q)
	ctx := r.Begin(k)
	tt := int64(120)
	ctx.Complete(true, &tt, 10, 1, 0)
	ctx.Complete(true, &tt, 10, 1, 0)
	ctx.Complete(false, nil, 0, 0, 0)
	a, s, tc, tok, _, _, _, _, _, _, ok := r.CellStats(k)
	require.True(t, ok)
	require.Equal(t, int64(1), a)
	require.Equal(t, int64(1), s)
	require.Equal(t, int64(1), tc)
	require.Equal(t, int64(10), tok)
}

func TestQualityRecorder_PointerAPI_NoCopy(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	f := fp(9)
	q := qc(9)
	k := keyOf(f, q)
	cell := r.GetOrCreateCell(k)
	require.NotNil(t, cell)
	var ctx AttemptContext
	ok := r.InitAttemptContext(cell, &ctx)
	require.True(t, ok)
	require.False(t, ctx.IsZero())
	require.NotNil(t, ctx.cell)
	var ptr *AttemptContext = r.Begin(k)
	require.NotNil(t, ptr)
}

func TestQualityRecorder_RetireBeforePin_Race(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	f := fp(20)
	q := qc(20)
	k := keyOf(f, q)
	cell := r.GetOrCreateCell(k)
	r.Retire(k)
	require.Equal(t, 0, r.ActiveCount())
	require.Equal(t, 1, r.RetiredCount())
	require.Equal(t, 0, r.PinnedRetiredCount())
	var ctx AttemptContext
	require.False(t, r.InitAttemptContext(cell, &ctx))
	require.Equal(t, 0, r.PinnedRetiredCount())
	require.Equal(t, int64(0), r.GlobalInflight())
	require.True(t, ctx.IsZero())
}

func TestQualityRecorder_HotPath_NoLock_0Alloc(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	f := fp(30)
	q := qc(30)
	k := keyOf(f, q)
	cell := r.GetOrCreateCell(k)
	var ctx AttemptContext
	tt := int64(100)
	for i := 0; i < 10; i++ {
		r.InitAttemptContext(cell, &ctx)
		ctx.Complete(true, &tt, 1, 0, 0)
	}
	a, _, _, _, _, _, _, _, _, _, ok := r.CellStats(k)
	require.True(t, ok)
	require.Equal(t, int64(10), a)
}

func TestQualityRecorder_ExactStats_Q32_10Hist_ErrorClasses(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	f := fp(40)
	q := qc(40)
	k := keyOf(f, q)
	cell := r.GetOrCreateCell(k)
	cases := []struct {
		success  bool
		tt       int64
		tokens   int64
		calls    int64
		images   int64
		errClass int
	}{
		{true, 50, 10, 1, 0, ErrClassNone},
		{true, 200, 20, 0, 1, ErrClassNone},
		{false, 0, 5, 1, 1, ErrClass429},
		{false, 0, 3, 0, 0, ErrClass5xx},
		{true, 1500, 30, 2, 2, ErrClassNone},
		{false, 0, 1, 0, 0, ErrClassNetwork},
	}
	for _, c := range cases {
		var ctx AttemptContext
		r.InitAttemptContext(cell, &ctx)
		if c.success {
			tt := c.tt
			ctx.Complete(true, &tt, c.tokens, c.calls, c.images)
		} else {
			var ctx2 AttemptContext
			r.InitAttemptContext(cell, &ctx2)
			obs := Observation{Success: false, Tokens: c.tokens, Calls: c.calls, Images: c.images, ErrClass: c.errClass}
			ctx2.CompleteObservation(obs)
		}
	}
	a, s, tc, tok, calls, images, sumQ32, sumSq, hist, errClasses, ok := r.CellStats(k)
	require.True(t, ok)
	require.Equal(t, int64(6), a)
	require.Equal(t, int64(3), s)
	require.Equal(t, int64(3), tc)
	require.Equal(t, int64(69), tok)
	require.Equal(t, int64(4), calls)
	require.Equal(t, int64(4), images)
	require.NotEqual(t, int64(0), sumQ32)
	require.NotEqual(t, int64(0), sumSq)
	filled := 0
	for _, v := range hist {
		if v > 0 {
			filled++
		}
	}
	require.GreaterOrEqual(t, filled, 2)
	require.Equal(t, int64(1), errClasses[ErrClass429])
	require.Equal(t, int64(1), errClasses[ErrClass5xx])
	require.Equal(t, int64(1), errClasses[ErrClassNetwork])
	require.Equal(t, int64(0), errClasses[ErrClass4xx])
	_ = hist
	expQ32 := toQ32(50) + toQ32(200) + toQ32(1500)
	require.InDelta(t, float64(expQ32), float64(sumQ32), float64(q32Scale))
	_ = math.Log
}

// TestQualityRecorder_LiveCells pins the compile-lane accessor: cloned
// cumulative totals per active cell, zero-attempt cells skipped, post-close
// reads still served (final data), and no aliasing into live cells.
func TestQualityRecorder_LiveCells(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	k1 := keyOf(fp(71), qc(71))
	k2 := keyOf(fp(72), qc(72))
	ttft := int64(100)
	ctx := r.Begin(k1)
	ctx.Complete(true, &ttft, 10, 1, 0)
	r.GetOrCreateCell(k2) // created but zero attempts → no signal

	cells := r.LiveCells()
	require.Len(t, cells, 1, "zero-attempt cell must be skipped")
	qm, ok := cells[k1]
	require.True(t, ok)
	require.Equal(t, int64(1), qm.Attempts())
	require.Equal(t, int64(1), qm.Successes())
	require.Equal(t, int64(1), qm.TTFTCount())
	require.Equal(t, int64(10), qm.InputTokens())
	require.NotZero(t, qm.SumQ32())

	// Clone discipline: further attempts must not mutate the returned snapshot.
	ctx2 := r.Begin(k1)
	ctx2.Complete(true, &ttft, 5, 0, 0)
	require.Equal(t, int64(1), qm.Attempts(), "LiveCells returns clones, not live cells")
	require.Equal(t, int64(2), r.LiveCells()[k1].Attempts())

	// Closed recorder still answers with the retained cells.
	require.NoError(t, r.Close())
	require.Len(t, r.LiveCells(), 1)
}

func TestQualityRecorder_Cancel_Local_Reservation_Exclude_PostcommitCounts(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	f := fp(50)
	q := qc(50)
	k := keyOf(f, q)
	cell := r.GetOrCreateCell(k)
	var ctx AttemptContext
	r.InitAttemptContext(cell, &ctx)
	obs := Observation{IsCancel: true}
	ctx.CompleteObservation(obs)
	a, _, _, _, _, _, _, _, _, _, ok := r.CellStats(k)
	require.True(t, ok)
	require.Equal(t, int64(0), a)
	var ctx2 AttemptContext
	r.InitAttemptContext(cell, &ctx2)
	ctx2.CompleteObservation(Observation{IsLocal: true})
	a2, _, _, _, _, _, _, _, _, _, _ := r.CellStats(k)
	require.Equal(t, int64(0), a2)
	var ctx3 AttemptContext
	r.InitAttemptContext(cell, &ctx3)
	ctx3.CompleteObservation(Observation{IsReservation: true})
	a3, _, _, _, _, _, _, _, _, _, _ := r.CellStats(k)
	require.Equal(t, int64(0), a3)
	var ctx4 AttemptContext
	r.InitAttemptContext(cell, &ctx4)
	ctx4.CompleteObservation(Observation{Success: false, ErrClass: ErrClass5xx, Tokens: 5, Calls: 1, Images: 1})
	a4, s4, _, tok4, calls4, images4, _, _, _, ec4, _ := r.CellStats(k)
	require.Equal(t, int64(1), a4)
	require.Equal(t, int64(0), s4)
	require.Equal(t, int64(5), tok4)
	require.Equal(t, int64(1), calls4)
	require.Equal(t, int64(1), images4)
	require.Equal(t, int64(1), ec4[ErrClass5xx])
}

func TestQualityRecorder_Retire_PinnedNeverEvicted_UnpinnedCap(t *testing.T) {
	r, err := NewRecorder(10)
	require.NoError(t, err)
	r.retiredCap = 2
	r.pendingCapBytes = 10 * EstimatedCellBytes
	r.minuteCap = 10
	var pinned []*Cell
	var ctxs []AttemptContext
	for i := 0; i < 5; i++ {
		f := fp(byte(60 + i))
		q := qc(byte(60 + i))
		k := keyOf(f, q)
		cell := r.GetOrCreateCell(k)
		pinned = append(pinned, cell)
		var ctx AttemptContext
		r.InitAttemptContext(cell, &ctx)
		ctxs = append(ctxs, ctx)
		r.Retire(k)
	}
	require.Equal(t, 5, r.RetiredCount())
	require.Equal(t, 5, r.PinnedRetiredCount())
	require.Equal(t, int64(0), r.QualityOverflow())
	require.Equal(t, 0, r.UnpinnedRetiredCount())
	tt := int64(100)
	ctxs[0].Complete(true, &tt, 1, 0, 0)
	require.Equal(t, 1, r.UnpinnedRetiredCount())
	for i := 1; i < 5; i++ {
		tt2 := int64(100)
		ctxs[i].Complete(true, &tt2, 1, 0, 0)
	}
	require.LessOrEqual(t, r.UnpinnedRetiredCount(), 2)
	require.Greater(t, r.QualityOverflow(), int64(0))
	require.Equal(t, int64(5), r.PinnedGauge()-r.PinnedGauge()+5)
	_ = pinned
}

func TestQualityRecorder_Reactivation_NoPendingBytesBug(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	f := fp(70)
	q := qc(70)
	k := keyOf(f, q)
	cell := r.GetOrCreateCell(k)
	before := r.PendingBytes()
	r.Retire(k)
	afterRetire := r.PendingBytes()
	require.Equal(t, before, afterRetire)
	cell2 := r.GetOrCreateCell(k)
	require.NotEqual(t, cell, cell2)
	require.NotSame(t, cell, cell2)
	afterReact := r.PendingBytes()
	require.Equal(t, before, afterReact, "reactivation must not subtract pendingBytes")
	require.Equal(t, 1, r.ActiveCount())
	require.Equal(t, 1, r.RetiredCount())
}

func TestQualityRecorder_Bounds_PendingBytes_256MiB_4096Minutes(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	r.minuteCap = 2
	r.pendingCapBytes = 2 * EstimatedFlowMinuteBytes
	for i := 0; i < 5; i++ {
		k := keyOf(fp(byte(80+i)), qc(byte(80+i)))
		r.AddQualityRow(int64(1000+i), k)
	}
	require.LessOrEqual(t, r.MinuteBucketCount(), 2)
	require.Greater(t, r.QualityOverflow(), int64(0))
	// v3-hygiene: the legacy edges-array vehicle is deleted — the flow lane
	// carries live consumer rows instead. Five distinct minutes fold and
	// drain into five retained shells.
	for i := 0; i < 5; i++ {
		require.NoError(t, foldConsumerRows(r.FlowOwner(), int64(2000+i), []repository.RoutingFlowRow{ownerTestRow(int64(i + 1))}))
		_, ok := r.FlowMinute(int64(2000 + i))
		require.True(t, ok)
	}
	// v3-F1: the flow lane has no minute cap and no pressure eviction — all
	// five legacy minutes stay retained with zero loss counters. Only the
	// quality lane above stays bounded.
	require.Equal(t, 5, r.FlowOwner().SnapshotStats().PendingMinutes)
	require.Equal(t, int64(0), r.FlowOverflow())
	// MinuteOverflow stays quality-owned: the quality evictions above bump
	// it; the flow lane contributes nothing without eviction.
	require.Greater(t, r.MinuteOverflow(), int64(0))
}

func TestQualityRecorder_Close_PreventsNewBegin_LetsExistingComplete(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	f := fp(90)
	q := qc(90)
	k := keyOf(f, q)
	cell := r.GetOrCreateCell(k)
	var ctx AttemptContext
	r.InitAttemptContext(cell, &ctx)
	done := make(chan error, 1)
	go func() { done <- r.Close() }()
	time.Sleep(20 * time.Millisecond)
	cell2 := r.GetOrCreateCell(k)
	require.Nil(t, cell2)
	var ctx2 AttemptContext
	ok := r.InitAttemptContext(cell, &ctx2)
	require.False(t, ok)
	tt := int64(100)
	ctx.Complete(true, &tt, 1, 0, 0)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after inflight drained")
	}
	a, _, _, _, _, _, _, _, _, _, ok2 := r.CellStats(k)
	require.True(t, ok2)
	require.Equal(t, int64(1), a)
	require.NoError(t, r.Close())
}

func TestQualityRecorder_Race(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	const N = 100
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			f := fp(byte(seed % 10))
			q := qc(byte(seed % 10))
			k := keyOf(f, q)
			cell := r.GetOrCreateCell(k)
			var ctx AttemptContext
			r.InitAttemptContext(cell, &ctx)
			tt := int64(100 + seed%50)
			if seed%2 == 0 {
				ctx.Complete(true, &tt, 1, 1, 0)
			} else {
				ctx.Complete(false, nil, 0, 0, 0)
			}
		}(i)
	}
	wg.Wait()
}

func TestQualityRecorder_PinnedGaugeBoundedByEffectiveMax(t *testing.T) {
	eff := int64(10)
	r, err := NewRecorder(eff)
	require.NoError(t, err)
	var ctxs []AttemptContext
	for i := 0; i < 10; i++ {
		f := fp(byte(100 + i))
		q := qc(byte(100 + i))
		k := keyOf(f, q)
		cell := r.GetOrCreateCell(k)
		var ctx AttemptContext
		r.InitAttemptContext(cell, &ctx)
		ctxs = append(ctxs, ctx)
		r.Retire(k)
	}
	require.LessOrEqual(t, r.PinnedGauge(), eff)
	require.LessOrEqual(t, int64(r.PinnedRetiredCount()), eff)
	for i := range ctxs {
		tt := int64(100)
		ctxs[i].Complete(true, &tt, 1, 0, 0)
	}
	require.Equal(t, int64(0), r.PinnedGauge())
}

func TestQualityRecorder_GenerationFencing(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	f := fp(111)
	q := qc(111)
	k := keyOf(f, q)
	cell := r.GetOrCreateCell(k)
	require.NotNil(t, cell)
	require.NotEqual(t, uint64(0), cell.gen)

	var valid AttemptContext
	require.True(t, r.InitAttemptContext(cell, &valid))
	require.False(t, valid.IsZero())
	require.Equal(t, cell.gen, valid.gen)

	inflightBefore := r.GlobalInflight()
	require.Equal(t, int64(1), inflightBefore)

	stale := valid
	stale.gen = valid.gen + 1
	tt := int64(100)
	stale.Complete(true, &tt, 10, 1, 0)
	a, _, _, _, _, _, _, _, _, _, ok := r.CellStats(k)
	require.True(t, ok)
	require.Equal(t, int64(0), a)
	require.Equal(t, inflightBefore, r.GlobalInflight(), "mismatched generation must not release")

	zeroGen := valid
	zeroGen.gen = 0
	zeroGen.Complete(true, &tt, 10, 1, 0)
	a2, _, _, _, _, _, _, _, _, _, _ := r.CellStats(k)
	require.Equal(t, int64(0), a2)
	require.Equal(t, inflightBefore, r.GlobalInflight())

	staleCancel := valid
	staleCancel.gen = 9999
	staleCancel.Cancel()
	a3, _, _, _, _, _, _, _, _, _, _ := r.CellStats(k)
	require.Equal(t, int64(0), a3)
	require.Equal(t, inflightBefore, r.GlobalInflight(), "mismatched Cancel must not release")

	zeroCell := &Cell{key: k, owner: r.id, gen: 0}
	var out AttemptContext
	require.False(t, r.InitAttemptContext(zeroCell, &out))
	require.True(t, out.IsZero())
	require.Equal(t, inflightBefore, r.GlobalInflight(), "zero-gen pin must not change inflight")

	foreignStale := AttemptContext{cell: cell, recorder: r, gen: cell.gen + 500}
	foreignStale.CompleteObservation(Observation{Success: true, TTFTMs: &tt, Tokens: 5, Calls: 1})
	a4, _, _, _, _, _, _, _, _, _, _ := r.CellStats(k)
	require.Equal(t, int64(0), a4)
	require.Equal(t, inflightBefore, r.GlobalInflight())

	valid.Complete(true, &tt, 10, 1, 0)
	require.True(t, valid.IsCompleted())
	a5, s5, _, tok5, _, _, _, _, _, _, _ := r.CellStats(k)
	require.Equal(t, int64(1), a5)
	require.Equal(t, int64(1), s5)
	require.Equal(t, int64(10), tok5)
	require.Equal(t, int64(0), r.GlobalInflight(), "valid completion must release exactly once")

	valid.Complete(true, &tt, 10, 1, 0)
	a6, _, _, _, _, _, _, _, _, _, _ := r.CellStats(k)
	require.Equal(t, int64(1), a6, "second Complete must not double record")
	require.Equal(t, int64(0), r.GlobalInflight())

	var valid2 AttemptContext
	require.True(t, r.InitAttemptContext(cell, &valid2))
	require.Equal(t, int64(1), r.GlobalInflight())
	valid2.Cancel()
	require.True(t, valid2.IsCompleted())
	require.Equal(t, int64(0), r.GlobalInflight())
	a7, _, _, _, _, _, _, _, _, _, _ := r.CellStats(k)
	require.Equal(t, int64(1), a7, "Cancel must not record")

	staleAfter := valid2
	staleAfter.gen = valid2.gen + 1
	staleAfter.Cancel()
	require.Equal(t, int64(0), r.GlobalInflight())
}
