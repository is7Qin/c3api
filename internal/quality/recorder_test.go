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
	a, s, tc, tok, _, _, _, _, _, ok := r.CellStats(f, q)
	require.True(t, ok)
	require.Equal(t, int64(1), a)
	require.Equal(t, int64(1), s)
	require.Equal(t, int64(1), tc)
	require.Equal(t, int64(10), tok)
}

func TestQualityRecorder_Cancel(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	f := fp(2)
	q := qc(2)
	ctx := r.Begin(f, q)
	ctx.Cancel()
	a, _, _, _, _, _, _, _, _, ok := r.CellStats(f, q)
	require.True(t, ok)
	require.Equal(t, int64(0), a)
	require.Equal(t, 1, r.ActiveCount())
	ctx.Cancel()
	a2, _, _, _, _, _, _, _, _, _ := r.CellStats(f, q)
	require.Equal(t, int64(0), a2)
}

func TestQualityRecorder_LocalReject(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	f := fp(3)
	q := qc(3)
	a0, _, _, _, _, _, _, _, _, _ := r.CellStats(f, q)
	require.Equal(t, int64(0), a0)
	_ = r.ActiveCount()
}

func TestQualityRecorder_PostCommit(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	f := fp(4)
	q := qc(4)
	ctx := r.Begin(f, q)
	tt := int64(200)
	ctx.Complete(false, &tt, 5, 1, 1)
	a, s, tc, tok, calls, images, _, _, hist, ok := r.CellStats(f, q)
	require.True(t, ok)
	require.Equal(t, int64(1), a)
	require.Equal(t, int64(0), s)
	require.Equal(t, int64(0), tc)
	require.Equal(t, int64(5), tok)
	require.Equal(t, int64(1), calls)
	require.Equal(t, int64(1), images)
	_ = hist
}

func TestQualityRecorder_MinuteBucket(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	r.minuteCap = 4096
	for i := 0; i < 10; i++ {
		f := fp(byte(10 + i))
		q := qc(byte(10 + i))
		ctx := r.Begin(f, q)
		tt := int64(50 + int64(i))
		ctx.Complete(true, &tt, 1, 0, 0)
	}
	require.GreaterOrEqual(t, r.MinuteBucketCount(), 1)
	require.LessOrEqual(t, r.MinuteBucketCount(), 4096)
}

func TestQualityRecorder_Retire(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	f := fp(20)
	q := qc(20)
	ctx := r.Begin(f, q)
	r.Retire(f, q)
	require.Equal(t, 0, r.ActiveCount())
	require.Equal(t, 1, r.RetiredCount())
	require.Equal(t, 0, r.UnpinnedRetiredCount())
	require.Equal(t, 1, r.PinnedRetiredCount())
	tt := int64(100)
	ctx.Complete(true, &tt, 1, 0, 0)
	require.Equal(t, 1, r.RetiredCount())
	require.Equal(t, 1, r.UnpinnedRetiredCount())
	require.Equal(t, 0, r.PinnedRetiredCount())
	a, _, _, _, _, _, _, _, _, ok := r.CellStats(f, q)
	require.True(t, ok)
	require.Equal(t, int64(1), a)
}

func TestQualityRecorder_PGOutage(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	f := fp(30)
	q := qc(30)
	for i := 0; i < 5; i++ {
		ctx := r.Begin(f, q)
		tt := int64(80)
		ctx.Complete(true, &tt, 1, 0, 0)
	}
	a, _, _, _, _, _, _, _, _, ok := r.CellStats(f, q)
	require.True(t, ok)
	require.Equal(t, int64(5), a)
	require.Equal(t, int64(0), r.QualityOverflow())
	require.Equal(t, int64(0), r.FlowOverflow())
}

func TestQualityRecorder_PinnedOverflow(t *testing.T) {
	r, err := NewRecorder(10)
	require.NoError(t, err)
	r.retiredCap = 2
	r.pendingCapBytes = 10 * EstimatedCellBytes
	r.minuteCap = 2

	var pinned []AttemptContext
	for i := 0; i < 5; i++ {
		f := fp(byte(40 + i))
		q := qc(byte(40 + i))
		ctx := r.Begin(f, q)
		pinned = append(pinned, ctx)
		r.Retire(f, q)
	}
	require.Equal(t, 5, r.RetiredCount())
	require.Equal(t, 0, r.UnpinnedRetiredCount())
	require.Equal(t, 5, r.PinnedRetiredCount())
	require.Equal(t, int64(0), r.QualityOverflow())

	f6 := fp(99)
	q6 := qc(99)
	ctx6 := r.Begin(f6, q6)
	tt := int64(100)
	ctx6.Complete(true, &tt, 1, 0, 0)
	require.Equal(t, int64(0), r.QualityOverflow())

	for i := range pinned {
		tt2 := int64(100)
		pinned[i].Complete(true, &tt2, 1, 0, 0)
	}
	require.Equal(t, 2, r.UnpinnedRetiredCount())
	require.Equal(t, 2, r.RetiredCount())
	require.Greater(t, r.QualityOverflow(), int64(0))
}

func TestQualityRecorder_Overflow(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	r.retiredCap = 2
	for i := 0; i < 5; i++ {
		f := fp(byte(50 + i))
		q := qc(byte(50 + i))
		ctx := r.Begin(f, q)
		tt := int64(100)
		ctx.Complete(true, &tt, 1, 0, 0)
		r.Retire(f, q)
	}
	require.LessOrEqual(t, r.UnpinnedRetiredCount(), 2)
	require.Greater(t, r.QualityOverflow(), int64(0))
}

func TestQualityRecorderOverflow_Minute(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	r.minuteCap = 2
	r.pendingCapBytes = 2 * EstimatedMinuteBytes
	for i := 0; i < 5; i++ {
		f := fp(byte(60 + i))
		q := qc(byte(60 + i))
		ctx := r.Begin(f, q)
		tt := int64(100)
		ctx.Complete(true, &tt, 1, 0, 0)
		r.mu.Lock()
		min := int64(1000 + i)
		if _, ok := r.minuteBuckets[min]; !ok {
			r.minuteBuckets[min] = &minuteBucket{minute: min}
			r.pendingBytes.Add(EstimatedMinuteBytes)
		}
		r.mu.Unlock()
		r.tryConverge()
	}
	require.LessOrEqual(t, r.MinuteBucketCount(), 2)
	require.Greater(t, r.FlowOverflow(), int64(0))
	require.Greater(t, r.MinuteOverflow(), int64(0))
}

func TestQualityRecorderOverflow_Quality(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	r.retiredCap = 1
	for i := 0; i < 10; i++ {
		f := fp(byte(70 + i))
		q := qc(byte(70 + i))
		ctx := r.Begin(f, q)
		tt := int64(100)
		ctx.Complete(true, &tt, 1, 0, 0)
		r.Retire(f, q)
	}
	require.Greater(t, r.QualityOverflow(), int64(0))
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
			ctx := r.Begin(f, q)
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

func TestQualityRecorder_Close(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	f := fp(80)
	q := qc(80)
	ctx := r.Begin(f, q)
	require.NoError(t, r.Close())
	tt := int64(100)
	ctx.Complete(true, &tt, 1, 0, 0)
	a, _, _, _, _, _, _, _, _, ok := r.CellStats(f, q)
	require.True(t, ok)
	require.Equal(t, int64(1), a)
	ctx2 := r.Begin(f, q)
	require.True(t, ctx2.IsZero())
}

func TestQualityRecorder_CountsAndHist(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	f := fp(90)
	q := qc(90)
	cases := []struct {
		success bool
		tt      int64
		tokens  int64
		calls   int64
		images  int64
	}{
		{true, 50, 10, 1, 0},
		{true, 200, 20, 0, 1},
		{false, 0, 5, 1, 1},
		{true, 1500, 30, 2, 2},
	}
	for _, c := range cases {
		ctx := r.Begin(f, q)
		var tt *int64
		if c.tt > 0 {
			tt = &c.tt
		}
		ctx.Complete(c.success, tt, c.tokens, c.calls, c.images)
	}
	a, s, tc, tok, calls, images, sumLog, sumSq, hist, ok := r.CellStats(f, q)
	require.True(t, ok)
	require.Equal(t, int64(4), a)
	require.Equal(t, int64(3), s)
	require.Equal(t, int64(3), tc)
	require.Equal(t, int64(65), tok)
	require.Equal(t, int64(4), calls)
	require.Equal(t, int64(4), images)
	require.Greater(t, sumLog, 0.0)
	require.Greater(t, sumSq, 0.0)
	require.Equal(t, int64(1), hist[0])
	require.Equal(t, int64(1), hist[1])
	require.Equal(t, int64(0), hist[2])
	require.Equal(t, int64(1), hist[3])
	expLog := math.Log(50) + math.Log(200) + math.Log(1500)
	require.InDelta(t, expLog, sumLog, 1e-9)
}

func TestQualityRecorder(t *testing.T) {
	t.Run("ExactOnce", TestQualityRecorder_ExactOnce)
	t.Run("Cancel", TestQualityRecorder_Cancel)
	t.Run("LocalReject", TestQualityRecorder_LocalReject)
	t.Run("PostCommit", TestQualityRecorder_PostCommit)
	t.Run("Minute", TestQualityRecorder_MinuteBucket)
	t.Run("Retire", TestQualityRecorder_Retire)
	t.Run("PGOutage", TestQualityRecorder_PGOutage)
	t.Run("PinnedOverflow", TestQualityRecorder_PinnedOverflow)
	t.Run("Race", TestQualityRecorder_Race)
	t.Run("Close", TestQualityRecorder_Close)
}
