// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"math"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWilson95Exact(t *testing.T) {
	iv := Wilson95(80, 100)
	require.InDelta(t, 0.711, iv.LCB, 0.02)
	require.InDelta(t, 0.867, iv.UCB, 0.02)
	require.True(t, iv.LCB <= 0.8 && 0.8 <= iv.UCB)
	iv2 := Wilson95(0, 30)
	require.GreaterOrEqual(t, iv2.LCB, 0.0)
	require.LessOrEqual(t, iv2.UCB, 0.12)
	iv3 := Wilson95(30, 30)
	require.Greater(t, iv3.LCB, 0.88)
	require.Equal(t, 1.0, iv3.UCB)
}

func TestWilson95ZConstant(t *testing.T) {
	require.Equal(t, 1.959963984540054, WilsonZ)
	require.InDelta(t, 0, Wilson95(50, 100).LCB, 1)
}

func TestWilson95Boundaries(t *testing.T) {
	require.Equal(t, Interval{}, Wilson95(0, 0))
	require.Equal(t, Wilson95(5, 10), Wilson95(5, 10))
	iv := Wilson95(-5, 10)
	require.Equal(t, Wilson95(0, 10), iv)
	iv = Wilson95(15, 10)
	require.Equal(t, Wilson95(10, 10), iv)
}

func TestLogTTFTInterval(t *testing.T) {
	samples := make([]int64, 30)
	for i := range samples {
		samples[i] = 100
	}
	iv, ok := LogTTFTFromSamples(samples)
	require.True(t, ok)
	require.InDelta(t, 100, iv.LCB, 1)
	require.InDelta(t, 100, iv.UCB, 1)
	samples2 := make([]int64, 30)
	for i := 0; i < 15; i++ {
		samples2[i] = 50
	}
	for i := 15; i < 30; i++ {
		samples2[i] = 200
	}
	iv2, ok := LogTTFTFromSamples(samples2)
	require.True(t, ok)
	require.Less(t, iv2.LCB, iv2.UCB)
	require.Greater(t, iv2.LCB, 50.0)
	require.Less(t, iv2.UCB, 300.0)
	iv3, ok := LogTTFTFromSamples(make([]int64, 29))
	require.False(t, ok)
	require.Equal(t, Interval{}, iv3)
	samples4 := make([]int64, 30)
	for i := range samples4 {
		samples4[i] = 0
	}
	iv4, ok := LogTTFTFromSamples(samples4)
	require.True(t, ok)
	require.InDelta(t, 1, iv4.LCB, 0.5)
}

func TestLogTTFTMath(t *testing.T) {
	sum := 0.0
	sumSq := 0.0
	for i := 0; i < 30; i++ {
		y := math.Log(100)
		sum += y
		sumSq += y * y
	}
	iv, ok := LogTTFTInterval(sum, sumSq, 30)
	require.True(t, ok)
	require.InDelta(t, 100, iv.LCB, 1e-4)
	require.InDelta(t, 100, iv.UCB, 1e-4)
	_, ok = LogTTFTInterval(0, 0, 29)
	require.False(t, ok)
}

func TestQualityWindowBoundaries(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 34, 56, 123456789, time.UTC)
	ss, se, ls, le := CurrentWindow(now)
	m := time.Date(2026, 8, 28, 12, 34, 0, 0, time.UTC)
	require.Equal(t, m.Add(-5*time.Minute), ss)
	require.Equal(t, m, se)
	require.Equal(t, m, ls)
	require.Equal(t, now.UTC(), le)
	bs, be := BaselineWindow(now)
	require.Equal(t, m.Add(-24*time.Hour), bs)
	require.Equal(t, m.Add(-5*time.Minute), be)
	require.True(t, ss.Before(se))
	require.True(t, bs.Before(be))
	require.Equal(t, be, ss)
}

func TestQualityAccumulation(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 10, 0, 0, time.UTC)
	m := time.Date(2026, 8, 28, 12, 10, 0, 0, time.UTC)
	buckets := []MinuteBucket{
		{Start: m.Add(-6 * time.Minute), Counts: Counts{Attempts: 100}},
		{Start: m.Add(-4 * time.Minute), Counts: Counts{Attempts: 10, Successes: 9}},
		{Start: m.Add(-2 * time.Minute), Counts: Counts{Attempts: 20, Successes: 18}},
		{Start: m.Add(-1 * time.Minute), Counts: Counts{Attempts: 30, Successes: 27}},
		{Start: m.Add(-30 * time.Minute), Counts: Counts{Attempts: 50}},
	}
	live := Counts{Attempts: 5, Successes: 5}
	cur := AccumulateCurrent(buckets, live, now)
	require.Equal(t, 65, cur.Attempts)
	all := []MinuteBucket{
		{Start: m.Add(-10 * time.Minute), Counts: Counts{Attempts: 20}},
		{Start: m.Add(-20 * time.Minute), Counts: Counts{Attempts: 20}},
		{Start: m.Add(-30 * time.Minute), Counts: Counts{Attempts: 20}},
		{Start: m.Add(-6 * time.Minute), Counts: Counts{Attempts: 20}},
	}
	baseline := AccumulateBaseline(all, now)
	require.Equal(t, 40, baseline.Attempts)
	require.GreaterOrEqual(t, baseline.Attempts, 30)
	empty := AccumulateBaseline(nil, now)
	require.Equal(t, 0, empty.Attempts)
}

func TestQualityExploreThreshold(t *testing.T) {
	require.True(t, IsExplore(0))
	require.True(t, IsExplore(29))
	require.False(t, IsExplore(30))
	require.False(t, IsExplore(1000))
}

func TestExplorationBP(t *testing.T) {
	require.Equal(t, 10000, ExploreBP(10, 5, 0))
	require.Equal(t, 100, ExploreBP(10, 0, 1))
	require.Equal(t, 0, ExploreBP(0, 0, 1))
	require.Equal(t, 100+400, ExploreBP(10, 10, 1))
	require.Equal(t, 300, ExploreBP(10, 5, 1))
	require.Equal(t, 500, ExploreBP(10, 20, 1))
}

func TestExplorationWeight(t *testing.T) {
	require.Equal(t, 5000, ExploreWeight(0, 0))
	require.Equal(t, 100, ExploreWeight(0, 100000))
	require.Equal(t, 9902, ExploreWeight(100, 100))
	require.GreaterOrEqual(t, ExploreWeight(0, 0), 100)
	require.Equal(t, ExploreWeight(99, 99), ExploreWeight(99, 99))
}

func TestExplorationCumulativeWeights(t *testing.T) {
	cands := []ExploreCandidate{{AccountID: 3, Weight: 100}, {AccountID: 1, Weight: 200}, {AccountID: 2, Weight: 300}}
	cum, total, err := CumulativeWeights(cands)
	require.NoError(t, err)
	require.Equal(t, uint64(600), total)
	require.Equal(t, []uint64{200, 500, 600}, cum)
	_, _, err = CumulativeWeights([]ExploreCandidate{{AccountID: 1, Weight: -1}})
	require.Error(t, err)
	_, _, err = CumulativeWeights([]ExploreCandidate{{AccountID: 1, Weight: 1000000}})
	require.NoError(t, err)
	cum2, _, err := CumulativeWeights(nil)
	require.NoError(t, err)
	require.Nil(t, cum2)
}

func TestExplorationCumulativeOverflow(t *testing.T) {
	_, _, err := CumulativeWeights([]ExploreCandidate{{AccountID: 1, Weight: 100}, {AccountID: 2, Weight: -5}})
	require.Error(t, err)
}

func TestExplorationFallbackOrder(t *testing.T) {
	cands := []FallbackCandidate{
		{AccountID: 3, Successes: 9, Attempts: 10},
		{AccountID: 1, Successes: 9, Attempts: 10},
		{AccountID: 2, Successes: 5, Attempts: 10},
		{AccountID: 4, Successes: 9, Attempts: 9},
	}
	ordered := FallbackOrder(cands)
	require.Equal(t, int64(4), ordered[0].AccountID)
	require.Equal(t, int64(1), ordered[1].AccountID)
	require.Equal(t, int64(3), ordered[2].AccountID)
	require.Equal(t, int64(2), ordered[3].AccountID)
	ordered2 := FallbackOrder(cands)
	require.Equal(t, ordered, ordered2)
}

func TestExplorationFallbackDeterministic(t *testing.T) {
	cands := []FallbackCandidate{{AccountID: 2, Successes: 5, Attempts: 10}, {AccountID: 1, Successes: 5, Attempts: 10}}
	o1 := FallbackOrder(cands)
	o2 := FallbackOrder(cands)
	require.Equal(t, o1, o2)
	require.Equal(t, int64(1), o1[0].AccountID)
}

func TestIncidentFailureDomain(t *testing.T) {
	o1, err := CanonicalOrigin("https://api.example.com/v1/chat")
	require.NoError(t, err)
	require.Equal(t, "https://api.example.com:443", o1)
	o2, err := CanonicalOrigin("HTTPS://API.EXAMPLE.COM:443/path?x=1")
	require.NoError(t, err)
	require.Equal(t, o1, o2)
	o3, err := CanonicalOrigin("http://example.com")
	require.NoError(t, err)
	require.Equal(t, "http://example.com:80", o3)
	_, err = CanonicalOrigin("")
	require.Error(t, err)
	_, err = CanonicalOrigin("not-a-url")
	require.Error(t, err)
	id1 := FailureDomainID("https://api.example.com/v1")
	id2 := FailureDomainID("https://api.example.com/other")
	require.Equal(t, id1, id2)
	id3 := FailureDomainID("https://other.example.com/v1")
	require.NotEqual(t, id1, id3)
	require.NotEqual(t, FailureDomainID("http://a.com"), FailureDomainID("https://a.com"))
}

func TestIncidentDegraded(t *testing.T) {
	cur := Interval{0.9, 0.95}
	base := Interval{0.7, 0.8}
	require.True(t, IsDegraded(cur, base, 30, 30))
	require.False(t, IsDegraded(cur, base, 29, 30))
	require.False(t, IsDegraded(cur, base, 30, 29))
	overlap := Interval{0.75, 0.85}
	require.False(t, IsDegraded(overlap, base, 30, 30))
}

func TestIncidentDomain(t *testing.T) {
	cands := []IncidentCandidate{
		{Domain: "a", Comparable: true, Degraded: true},
		{Domain: "a", Comparable: true, Degraded: true},
		{Domain: "a", Comparable: true, Degraded: false},
	}
	require.True(t, DomainIncident(cands))
	cands2 := []IncidentCandidate{
		{Domain: "a", Comparable: true, Degraded: true},
		{Domain: "a", Comparable: true, Degraded: false},
	}
	require.False(t, DomainIncident(cands2))
	require.False(t, DomainIncident([]IncidentCandidate{{Domain: "a", Comparable: true, Degraded: true}}))
}

func TestIncidentModel(t *testing.T) {
	cands := []IncidentCandidate{
		{Domain: "a.com", Comparable: true, Degraded: true},
		{Domain: "b.com", Comparable: true, Degraded: true},
		{Domain: "c.com", Comparable: true, Degraded: false},
	}
	require.True(t, ModelIncident(cands))
	cands2 := []IncidentCandidate{
		{Domain: "a.com", Comparable: true, Degraded: true},
		{Domain: "b.com", Comparable: true, Degraded: false},
	}
	require.False(t, ModelIncident(cands2))
	require.False(t, ModelIncident([]IncidentCandidate{{Domain: "a.com", Comparable: true, Degraded: true}}))
}

func TestIncidentTwoCycleRecovery(t *testing.T) {
	var s IncidentState
	require.False(t, s.Active)
	s.Update(true)
	require.True(t, s.Active)
	s.Update(false)
	require.True(t, s.Active)
	require.Equal(t, 1, s.HealthyStreak)
	s.Update(false)
	require.False(t, s.Active)
	require.Equal(t, 0, s.HealthyStreak)
	s.Update(true)
	s.Update(false)
	s.Update(true)
	require.True(t, s.Active)
	require.Equal(t, 0, s.HealthyStreak)
}

func TestQualityClassification(t *testing.T) {
	sum := func(v int64, n int) (float64, float64) {
		y := math.Log(float64(v))
		return y * float64(n), y * y * float64(n)
	}
	s1log, s1sq := sum(100, 30)
	s2log, s2sq := sum(200, 30)
	cands := []QualityCandidate{
		{AccountID: 1, Successes: 28, Attempts: 30, SumLog: s1log, SumSq: s1sq, TTFTCount: 30, Cost: 10},
		{AccountID: 2, Successes: 15, Attempts: 30, SumLog: s2log, SumSq: s2sq, TTFTCount: 30, Cost: 5},
		{AccountID: 3, Successes: 5, Attempts: 10},
	}
	primary, degraded, explore := ClassifyQuality(cands)
	require.Len(t, explore, 1)
	require.Equal(t, int64(3), explore[0].AccountID)
	require.GreaterOrEqual(t, len(primary)+len(degraded), 1)
	primary2, degraded2, explore2 := ClassifyQuality(cands)
	require.Equal(t, primary, primary2)
	require.Equal(t, degraded, degraded2)
	require.Equal(t, explore, explore2)
	require.True(t, sort.SliceIsSorted(explore, func(i, j int) bool { return explore[i].AccountID < explore[j].AccountID }))
}

func TestQualityDeterministic(t *testing.T) {
	cands := []QualityCandidate{
		{AccountID: 2, Successes: 25, Attempts: 30, SumLog: math.Log(100) * 30, SumSq: math.Log(100) * math.Log(100) * 30, TTFTCount: 30, Cost: 20},
		{AccountID: 1, Successes: 25, Attempts: 30, SumLog: math.Log(100) * 30, SumSq: math.Log(100) * math.Log(100) * 30, TTFTCount: 30, Cost: 20},
	}
	p1, d1, e1 := ClassifyQuality(cands)
	p2, d2, e2 := ClassifyQuality(cands)
	require.Equal(t, p1, p2)
	require.Equal(t, d1, d2)
	require.Equal(t, e1, e2)
	all := []QualityCandidate{
		{AccountID: 3, Successes: 10, Attempts: 30, Cost: 30},
		{AccountID: 1, Successes: 29, Attempts: 30, Cost: 10},
		{AccountID: 2, Successes: 29, Attempts: 30, Cost: 10},
	}
	p3, _, _ := ClassifyQuality(all)
	require.Equal(t, int64(1), p3[0].AccountID)
}

func TestQualityNoPrimaryExploreAll(t *testing.T) {
	cands := []QualityCandidate{
		{AccountID: 1, Successes: 0, Attempts: 10},
		{AccountID: 2, Successes: 1, Attempts: 10},
	}
	_, _, explore := ClassifyQuality(cands)
	require.Len(t, explore, 2)
}

func FuzzWilson95(f *testing.F) {
	f.Add(10, 20)
	f.Add(0, 0)
	f.Add(100, 100)
	f.Fuzz(func(t *testing.T, successes, attempts int) {
		iv := Wilson95(successes, attempts)
		require.GreaterOrEqual(t, iv.LCB, 0.0)
		require.LessOrEqual(t, iv.UCB, 1.0)
		require.LessOrEqual(t, iv.LCB, iv.UCB+1e-9)
		if attempts < 0 {
			attempts = -attempts
		}
		if attempts > 1000000 {
			attempts = attempts % 1000000
		}
		_ = Wilson95(attempts, attempts)
	})
}

func FuzzExploreWeight(f *testing.F) {
	f.Add(5, 10)
	f.Fuzz(func(t *testing.T, successes, attempts int) {
		w := ExploreWeight(successes, attempts)
		require.GreaterOrEqual(t, w, 100)
		require.LessOrEqual(t, w, 20000)
	})
}

func BenchmarkWilson95(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = Wilson95(80, 100)
	}
}

func BenchmarkLogTTFT(b *testing.B) {
	sum := math.Log(100) * 30
	sumSq := math.Log(100) * math.Log(100) * 30
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = LogTTFTInterval(sum, sumSq, 30)
	}
}

func BenchmarkQualityClassify(b *testing.B) {
	sum := math.Log(100) * 30
	sumSq := math.Log(100) * math.Log(100) * 30
	cands := []QualityCandidate{
		{AccountID: 1, Successes: 28, Attempts: 30, SumLog: sum, SumSq: sumSq, TTFTCount: 30, Cost: 10},
		{AccountID: 2, Successes: 25, Attempts: 30, SumLog: sum, SumSq: sumSq, TTFTCount: 30, Cost: 20},
		{AccountID: 3, Successes: 5, Attempts: 10, Cost: 5},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, _ = ClassifyQuality(cands)
	}
}
