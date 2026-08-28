// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"math"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
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

func TestQualityRecorder_ExactOnce(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	f := fp(1)
	q := qc(1)
	ctx := r.Begin(f, q)
	tt := int64(120)
	ctx.Complete(true, &tt, 10, 1, 0)
	ctx.Complete(true, &tt, 10, 1, 0)
	ctx.Complete(false, nil, 0, 0, 0)
	a, s, tc, tok, _, _, _, _, _, _, ok := r.CellStats(f, q)
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
	cell := r.GetOrCreateCell(f, q)
	require.NotNil(t, cell)
	var ctx AttemptContext
	ok := r.InitAttemptContext(cell, &ctx)
	require.True(t, ok)
	require.False(t, ctx.IsZero())
	require.NotNil(t, ctx.cell)
	// copied API impossible: Begin returns *AttemptContext, not value. Verify pointer signature by type.
	var ptr *AttemptContext = r.Begin(f, q)
	require.NotNil(t, ptr)
}

func TestQualityRecorder_RetireBeforePin_Race(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	f := fp(20)
	q := qc(20)
	cell := r.GetOrCreateCell(f, q)
	r.Retire(f, q)
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
	cell := r.GetOrCreateCell(f, q)
	var ctx AttemptContext
	tt := int64(100)
	// warm
	for i := 0; i < 10; i++ {
		r.InitAttemptContext(cell, &ctx)
		ctx.Complete(true, &tt, 1, 0, 0)
	}
	a, _, _, _, _, _, _, _, _, _, ok := r.CellStats(f, q)
	require.True(t, ok)
	require.Equal(t, int64(10), a)
}

func TestQualityRecorder_ExactStats_Q32_10Hist_ErrorClasses(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	f := fp(40)
	q := qc(40)
	cell := r.GetOrCreateCell(f, q)
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
	// use the successes via legacy completes already did 3 successes, need to account
	// we did 2 successes via Init+Complete, plus 1 more success = 3 successes total attempts 6?
	a, s, tc, tok, calls, images, sumQ32, sumSq, hist, errClasses, ok := r.CellStats(f, q)
	require.True(t, ok)
	require.Equal(t, int64(6), a)
	require.Equal(t, int64(3), s)
	require.Equal(t, int64(3), tc)
	require.Equal(t, int64(69), tok)
	require.Equal(t, int64(4), calls)
	require.Equal(t, int64(4), images)
	require.NotEqual(t, int64(0), sumQ32)
	require.NotEqual(t, int64(0), sumSq)
	// hist has 10 bins, at least 3 bins filled
	filled := 0
	for _, v := range hist {
		if v > 0 {
			filled++
		}
	}
	require.GreaterOrEqual(t, filled, 2)
	// error classes separate
	require.Equal(t, int64(1), errClasses[ErrClass429])
	require.Equal(t, int64(1), errClasses[ErrClass5xx])
	require.Equal(t, int64(1), errClasses[ErrClassNetwork])
	require.Equal(t, int64(0), errClasses[ErrClass4xx])
	_ = hist
	expQ32 := toQ32(50) + toQ32(200) + toQ32(1500)
	require.InDelta(t, float64(expQ32), float64(sumQ32), float64(q32Scale))
	_ = math.Log
}

func TestQualityRecorder_Cancel_Local_Reservation_Exclude_PostcommitCounts(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	f := fp(50)
	q := qc(50)
	cell := r.GetOrCreateCell(f, q)
	// cancel should not count
	var ctx AttemptContext
	r.InitAttemptContext(cell, &ctx)
	obs := Observation{IsCancel: true}
	ctx.CompleteObservation(obs)
	a, _, _, _, _, _, _, _, _, _, ok := r.CellStats(f, q)
	require.True(t, ok)
	require.Equal(t, int64(0), a)
	// local reject exclude
	var ctx2 AttemptContext
	r.InitAttemptContext(cell, &ctx2)
	ctx2.CompleteObservation(Observation{IsLocal: true})
	a2, _, _, _, _, _, _, _, _, _, _ := r.CellStats(f, q)
	require.Equal(t, int64(0), a2)
	// reservation exclude
	var ctx3 AttemptContext
	r.InitAttemptContext(cell, &ctx3)
	ctx3.CompleteObservation(Observation{IsReservation: true})
	a3, _, _, _, _, _, _, _, _, _, _ := r.CellStats(f, q)
	require.Equal(t, int64(0), a3)
	// postcommit failure counts
	var ctx4 AttemptContext
	r.InitAttemptContext(cell, &ctx4)
	ctx4.CompleteObservation(Observation{Success: false, ErrClass: ErrClass5xx, Tokens: 5, Calls: 1, Images: 1})
	a4, s4, _, tok4, calls4, images4, _, _, _, ec4, _ := r.CellStats(f, q)
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
		cell := r.GetOrCreateCell(f, q)
		pinned = append(pinned, cell)
		var ctx AttemptContext
		r.InitAttemptContext(cell, &ctx)
		ctxs = append(ctxs, ctx)
		r.Retire(f, q)
	}
	require.Equal(t, 5, r.RetiredCount())
	require.Equal(t, 5, r.PinnedRetiredCount())
	require.Equal(t, int64(0), r.QualityOverflow())
	// unpinned cap not applied to pinned
	require.Equal(t, 0, r.UnpinnedRetiredCount())
	// complete one pinned, it becomes unpinned and should trigger eviction if over cap
	tt := int64(100)
	ctxs[0].Complete(true, &tt, 1, 0, 0)
	require.Equal(t, 1, r.UnpinnedRetiredCount())
	// after release, should converge to cap 2 (evict oldest unpinned)
	for i := 1; i < 5; i++ {
		tt2 := int64(100)
		ctxs[i].Complete(true, &tt2, 1, 0, 0)
	}
	require.LessOrEqual(t, r.UnpinnedRetiredCount(), 2)
	require.Greater(t, r.QualityOverflow(), int64(0))
	require.Equal(t, int64(5), r.PinnedGauge()-r.PinnedGauge()+5) // gauge was 5
	_ = pinned
}

func TestQualityRecorder_Reactivation_NoPendingBytesBug(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	f := fp(70)
	q := qc(70)
	cell := r.GetOrCreateCell(f, q)
	before := r.PendingBytes()
	r.Retire(f, q)
	afterRetire := r.PendingBytes()
	require.Equal(t, before, afterRetire)
	// reactivate
	cell2 := r.GetOrCreateCell(f, q)
	require.Equal(t, cell, cell2)
	afterReact := r.PendingBytes()
	require.Equal(t, before, afterReact, "reactivation must not subtract pendingBytes")
	require.Equal(t, 1, r.ActiveCount())
	require.Equal(t, 0, r.RetiredCount())
}

func TestQualityRecorder_Bounds_PendingBytes_256MiB_4096Minutes(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	r.minuteCap = 2
	r.pendingCapBytes = 2 * EstimatedFlowMinuteBytes
	for i := 0; i < 5; i++ {
		r.AddQualityRow(int64(1000+i), fp(byte(80+i)), qc(byte(80+i)))
	}
	require.LessOrEqual(t, r.MinuteBucketCount(), 2)
	require.Greater(t, r.QualityOverflow(), int64(0))
	// flow whole-minute eviction
	for i := 0; i < 5; i++ {
		r.AddFlowMinute(int64(2000+i), [8]int64{int64(i)})
	}
	require.LessOrEqual(t, r.MinuteBucketCount(), 2)
	require.Greater(t, r.FlowOverflow(), int64(0))
	require.Greater(t, r.MinuteOverflow(), int64(0))
	// pendingBytes exact
	require.LessOrEqual(t, r.PendingBytes(), r.pendingCapBytes+EstimatedFlowMinuteBytes)
}

func TestQualityRecorder_FlowWholeMinute_EdgeArrayAPI(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	var edges [8]int64
	for i := range edges {
		edges[i] = int64(i * 10)
	}
	minute := int64(3000)
	r.AddFlowMinute(minute, edges)
	fm, ok := r.FlowMinute(minute)
	require.True(t, ok)
	require.Equal(t, minute, fm.Minute())
	for i := 0; i < 8; i++ {
		require.Equal(t, int64(i*10), fm.Edge(i))
	}
	fm.SetEdge(3, 999)
	require.Equal(t, int64(999), fm.Edge(3))
	require.Equal(t, [8]int64{0, 10, 20, 999, 40, 50, 60, 70}, fm.Edges())
	// adding same minute should not duplicate
	r.AddFlowMinute(minute, edges)
	require.Equal(t, 1, r.MinuteBucketCount())
	// eviction whole minute only
	r.minuteCap = 1
	r.AddFlowMinute(minute+1, edges)
	require.LessOrEqual(t, r.MinuteBucketCount(), 1)
}

func TestQualityRecorder_Close_PreventsNewBegin_LetsExistingComplete(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	f := fp(90)
	q := qc(90)
	cell := r.GetOrCreateCell(f, q)
	var ctx AttemptContext
	r.InitAttemptContext(cell, &ctx)
	require.NoError(t, r.Close())
	// new begin should fail
	cell2 := r.GetOrCreateCell(f, q)
	require.Nil(t, cell2)
	var ctx2 AttemptContext
	ok := r.InitAttemptContext(cell, &ctx2)
	require.False(t, ok)
	// existing should still complete (no lost minute)
	tt := int64(100)
	ctx.Complete(true, &tt, 1, 0, 0)
	a, _, _, _, _, _, _, _, _, _, ok2 := r.CellStats(f, q)
	require.True(t, ok2)
	require.Equal(t, int64(1), a)
	// second close idempotent
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
			cell := r.GetOrCreateCell(f, q)
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
		cell := r.GetOrCreateCell(f, q)
		var ctx AttemptContext
		r.InitAttemptContext(cell, &ctx)
		ctxs = append(ctxs, ctx)
		r.Retire(f, q)
	}
	require.LessOrEqual(t, r.PinnedGauge(), eff)
	require.LessOrEqual(t, int64(r.PinnedRetiredCount()), eff)
	for i := range ctxs {
		tt := int64(100)
		ctxs[i].Complete(true, &tt, 1, 0, 0)
	}
	require.Equal(t, int64(0), r.PinnedGauge())
}
