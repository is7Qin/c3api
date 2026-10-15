// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

// HEAD-side routing-plan-memory performance harness (spec §6). Fixture builders
// and the env-selected model matrix (RPM_MODELS, default 100; spec matrix
// 100/300/693) live in routing_plan_memory_fixture_test.go and are
// BASE-COMPATIBLE; the measured loops here use HEAD-only symbols
// (decisionDigester). The BASE twin measures the same fixture with
// encode+bytes.Equal and the same forced-scope fire structure.
//
// All benchmarks: single process, serial, warm (3 pre-timer iterations), setup
// outside the timer. Run per §6 pre-registration: GOMAXPROCS=1 GOGC=100, ≥5
// ABBA interleaved groups per model size, ≥1s benchtime.
//
// O5 harness guard: every measured loop asserts its expected outcome equals
// b.N (commits / publishes / equal_skips), each real-fire loop captured the
// skipCount before timing and asserts a zero delta afterwards (so a fireSkip
// never silently replaces the measured guard work), and each warm-up is
// asserted to have actually executed (published for changed, executed the
// guard without publishing and without fireSkip for equal).

import (
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

var (
	benchmarkRPMCount  int
	benchmarkRPMBool   bool
	benchmarkRPMDigest decisionDigest
)

// rpmQuality builds a quality map for one (group, format, model) route over the
// given accounts; with rpmPrices the candidates are cost-known (primaries).
func rpmQuality(gid int64, format domain.RequestFormat, model string, accs []*domain.Account, attempts, successes int) map[CandidateQualityKey]CandidateQualityInput {
	raw := make(map[int64]CandidateQualityInput, len(accs))
	for _, a := range accs {
		raw[a.ID] = qualityInput(attempts, successes, 1000, 1000)
	}
	return buildQuality(gid, format, model, raw, accs)
}

// rpmAssertDigestDiffers fails the benchmark if qA/qB do not actually change the
// canonical digest — otherwise the "changed" scenarios would silently measure
// equal iterations.
func rpmAssertDigestDiffers(b *testing.B, s *Scheduler, qA, qB map[CandidateQualityKey]CandidateQualityInput, prices map[string]domain.ResolvedPrices) {
	b.Helper()
	c := NewRoutingCompiler()
	dvA, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: qA, Prices: prices})
	require.NoError(b, err)
	dvB, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: qB, Prices: prices})
	require.NoError(b, err)
	var d decisionDigester
	require.NotEqual(b, d.digest(dvA), d.digest(dvB), "qA/qB must produce different canonical digests")
}

func rpmQualitySources(q map[CandidateQualityKey]CandidateQualityInput, prices map[string]domain.ResolvedPrices) *CompilerSources {
	return &CompilerSources{
		Quality: func(time.Time) WindowedQuality { return WindowedQuality{Current: q} },
		Prices:  func() map[string]domain.ResolvedPrices { return prices },
	}
}

// BenchmarkRPMGuardEqual measures the production equal-path guard: SHA-256
// digest + equality compare over the published view each iteration (the guard
// always executes; no fireSkip). The retained digest is a value commit.
func BenchmarkRPMGuardEqual(b *testing.B) {
	models := rpmModels(rpmBenchModels())
	s, _ := rpmHomogeneousScheduler(b, models)
	dv := s.View().DecisionView()
	require.NotNil(b, dv)
	var d decisionDigester
	last := d.digest(dv)
	for range 3 {
		last = d.digest(dv)
	}
	matches := 0
	b.ReportAllocs()
	runtime.GC()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cur := d.digest(dv)
		if cur == last {
			benchmarkRPMBool = true
			matches++
		} else {
			last = cur
			benchmarkRPMDigest = cur
			benchmarkRPMBool = false
		}
	}
	b.StopTimer()
	runtime.KeepAlive(s)
	runtime.KeepAlive(dv)
	runtime.KeepAlive(d)
	runtime.KeepAlive(last)
	require.Equal(b, b.N, matches, "every equal-guard iteration must compare equal")
	b.ReportMetric(float64(matches), "matches")
}

// BenchmarkRPMGuardChanged alternates two distinct equal-scale plans, forcing a
// real digest compare + commit (value copy) each iteration.
func BenchmarkRPMGuardChanged(b *testing.B) {
	models := rpmModels(rpmBenchModels())
	s, m := rpmHomogeneousScheduler(b, models)
	prices := rpmPrices(models)
	qA := rpmQuality(10, domain.FormatOpenAIChat, models[0], m.byGroup[10], 30, 29)
	qB := rpmQuality(10, domain.FormatOpenAIChat, models[0], m.byGroup[10], 10, 5)
	rpmAssertDigestDiffers(b, s, qA, qB, prices)
	c := NewRoutingCompiler()
	dvA, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: qA, Prices: prices})
	require.NoError(b, err)
	dvB, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: qB, Prices: prices})
	require.NoError(b, err)
	var d decisionDigester
	last := d.digest(dvA)
	commits := 0
	skips0 := s.skipCount.Load()
	for range 3 {
		last = d.digest(dvA)
		last = d.digest(dvB)
	}
	b.ReportAllocs()
	runtime.GC()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var cur decisionDigest
		if i&1 == 0 {
			cur = d.digest(dvA)
		} else {
			cur = d.digest(dvB)
		}
		if cur == last {
			benchmarkRPMBool = true
		} else {
			last = cur
			benchmarkRPMDigest = cur
			benchmarkRPMBool = false
			commits++
		}
	}
	b.StopTimer()
	runtime.KeepAlive(s)
	runtime.KeepAlive(dvA)
	runtime.KeepAlive(dvB)
	runtime.KeepAlive(d)
	runtime.KeepAlive(last)
	require.Equal(b, b.N, commits, "every alternation must commit a changed digest")
	require.Zero(b, s.skipCount.Load()-skips0, "the guard microbenchmark must not fireSkip")
	b.ReportMetric(float64(commits), "commits")
}

// BenchmarkRPMFireEqualGuarded measures one real equal fire end-to-end: a group
// scope forces a scoped recompute (never fireSkip, no root staging) whose digest
// matches the last published digest, so the fire recompiles + digests but does
// not publish.
func BenchmarkRPMFireEqualGuarded(b *testing.B) {
	models := rpmModels(rpmBenchModels())
	s, _ := rpmHomogeneousScheduler(b, models)
	skips := 0
	genWarm0 := s.View().Generation()
	skipsWarm0 := s.skipCount.Load()
	for range 3 {
		s.enqueueCompileScope([]int64{10}, nil, scopeCauseGroup)
		s.compileOnce()
	}
	require.Zero(b, s.skipCount.Load()-skipsWarm0, "equal warm-up must execute real fires (no fireSkip)")
	require.Equal(b, genWarm0, s.View().Generation(), "equal warm-up must not publish")
	skips0 := s.skipCount.Load()
	b.ReportAllocs()
	runtime.GC()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		before := s.View().Generation()
		s.enqueueCompileScope([]int64{10}, nil, scopeCauseGroup)
		s.compileOnce()
		if s.View().Generation() == before {
			skips++
		}
	}
	b.StopTimer()
	runtime.KeepAlive(s)
	require.Equal(b, b.N, skips, "every equal fire must hit the guard and skip publication")
	require.Zero(b, s.skipCount.Load()-skips0, "no fireSkip in the measured loop")
	b.ReportMetric(float64(skips), "equal_skips")
}

// BenchmarkRPMFireChangedGuarded alternates two equal-scale quality inputs over
// a whole group so each fire commits a new digest and publishes.
func BenchmarkRPMFireChangedGuarded(b *testing.B) {
	models := rpmModels(rpmBenchModels())
	s, m := rpmHomogeneousScheduler(b, models)
	prices := rpmPrices(models)
	qA := rpmQuality(10, domain.FormatOpenAIChat, models[0], m.byGroup[10], 30, 29)
	qB := rpmQuality(10, domain.FormatOpenAIChat, models[0], m.byGroup[10], 10, 5)
	rpmAssertDigestDiffers(b, s, qA, qB, prices)
	srcA := rpmQualitySources(qA, prices)
	srcB := rpmQualitySources(qB, prices)
	s.sources = srcA
	s.compileOnce()
	warm := func(src *CompilerSources) {
		s.sources = src
		s.enqueueCompileScope([]int64{10}, nil, scopeCauseGroup)
		s.compileOnce()
	}
	genWarm0 := s.View().Generation()
	skipsWarm0 := s.skipCount.Load()
	for range 3 {
		warm(srcB)
		warm(srcA)
	}
	require.Greater(b, s.View().Generation(), genWarm0, "changed warm-up must execute real publishing fires")
	require.Zero(b, s.skipCount.Load()-skipsWarm0, "changed warm-up must not fireSkip")
	pubs := 0
	skips0 := s.skipCount.Load()
	b.ReportAllocs()
	runtime.GC()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		before := s.View().Generation()
		if i&1 == 0 {
			s.sources = srcB
		} else {
			s.sources = srcA
		}
		s.enqueueCompileScope([]int64{10}, nil, scopeCauseGroup)
		s.compileOnce()
		if s.View().Generation() > before {
			pubs++
		}
	}
	b.StopTimer()
	runtime.KeepAlive(s)
	require.Equal(b, b.N, pubs, "every alternated changed fire must publish")
	require.Zero(b, s.skipCount.Load()-skips0, "no fireSkip in the measured loop")
	b.ReportMetric(float64(pubs), "publishes")
}

// BenchmarkRPMScopedChangedSmall isolates the scoped seed overhead: only ONE
// route's quality changes, so the session seeds from carry (O(U) key builds over
// R carry routes) and recomputes a single route.
func BenchmarkRPMScopedChangedSmall(b *testing.B) {
	models := rpmModels(rpmBenchModels())
	s, m := rpmHomogeneousScheduler(b, models)
	prices := rpmPrices(models)
	qA := rpmQuality(10, domain.FormatOpenAIChat, models[0], m.byGroup[10], 30, 29)
	qB := rpmQuality(10, domain.FormatOpenAIChat, models[0], m.byGroup[10], 10, 5)
	rpmAssertDigestDiffers(b, s, qA, qB, prices)
	srcA := rpmQualitySources(qA, prices)
	srcB := rpmQualitySources(qB, prices)
	s.sources = srcA
	s.compileOnce()
	genWarm0 := s.View().Generation()
	skipsWarm0 := s.skipCount.Load()
	for range 3 {
		s.sources = srcB
		s.compileOnce()
		s.sources = srcA
		s.compileOnce()
	}
	require.Greater(b, s.View().Generation(), genWarm0, "scoped warm-up must execute real publishing fires")
	require.Zero(b, s.skipCount.Load()-skipsWarm0, "scoped warm-up must not fireSkip")
	pubs := 0
	skips0 := s.skipCount.Load()
	b.ReportAllocs()
	runtime.GC()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		before := s.View().Generation()
		if i&1 == 0 {
			s.sources = srcB
		} else {
			s.sources = srcA
		}
		s.compileOnce()
		if s.View().Generation() > before {
			pubs++
		}
	}
	b.StopTimer()
	runtime.KeepAlive(s)
	require.Equal(b, b.N, pubs, "every alternated scoped change must publish")
	require.Zero(b, s.skipCount.Load()-skips0, "no fireSkip in the measured loop")
	b.ReportMetric(float64(pubs), "publishes")
}

// BenchmarkRPMFullFireEqualGuarded forces the full-fidelity fallback
// (scopeOverflow) each fire: full compile of the whole plan + digest + equal
// guard skip, no publish.
func BenchmarkRPMFullFireEqualGuarded(b *testing.B) {
	models := rpmModels(rpmBenchModels())
	s, _ := rpmHomogeneousScheduler(b, models)
	skips := 0
	genWarm0 := s.View().Generation()
	skipsWarm0 := s.skipCount.Load()
	for range 3 {
		s.scopeOverflow.Store(true)
		s.compileOnce()
	}
	require.Zero(b, s.skipCount.Load()-skipsWarm0, "full equal warm-up must execute real fires (no fireSkip)")
	require.Equal(b, genWarm0, s.View().Generation(), "full equal warm-up must not publish")
	skips0 := s.skipCount.Load()
	b.ReportAllocs()
	runtime.GC()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		before := s.View().Generation()
		s.scopeOverflow.Store(true)
		s.compileOnce()
		if s.View().Generation() == before {
			skips++
		}
	}
	b.StopTimer()
	runtime.KeepAlive(s)
	require.Equal(b, b.N, skips, "every full equal fire must hit the guard and skip publication")
	require.Zero(b, s.skipCount.Load()-skips0, "no fireSkip in the measured loop")
	b.ReportMetric(float64(skips), "equal_skips")
}

// BenchmarkRPMFullFireChangedGuarded forces a full compile + changed digest +
// publish (commit copy) each fire; the spec's full end-to-end path.
func BenchmarkRPMFullFireChangedGuarded(b *testing.B) {
	models := rpmModels(rpmBenchModels())
	s, m := rpmHomogeneousScheduler(b, models)
	prices := rpmPrices(models)
	qA := rpmQuality(10, domain.FormatOpenAIChat, models[0], m.byGroup[10], 30, 29)
	qB := rpmQuality(10, domain.FormatOpenAIChat, models[0], m.byGroup[10], 10, 5)
	rpmAssertDigestDiffers(b, s, qA, qB, prices)
	srcA := rpmQualitySources(qA, prices)
	srcB := rpmQualitySources(qB, prices)
	s.sources = srcA
	s.compileOnce()
	warm := func(src *CompilerSources) {
		s.sources = src
		s.scopeOverflow.Store(true)
		s.compileOnce()
	}
	genWarm0 := s.View().Generation()
	skipsWarm0 := s.skipCount.Load()
	for range 3 {
		warm(srcB)
		warm(srcA)
	}
	require.Greater(b, s.View().Generation(), genWarm0, "full changed warm-up must execute real publishing fires")
	require.Zero(b, s.skipCount.Load()-skipsWarm0, "full changed warm-up must not fireSkip")
	pubs := 0
	skips0 := s.skipCount.Load()
	b.ReportAllocs()
	runtime.GC()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		before := s.View().Generation()
		if i&1 == 0 {
			s.sources = srcB
		} else {
			s.sources = srcA
		}
		s.scopeOverflow.Store(true)
		s.compileOnce()
		if s.View().Generation() > before {
			pubs++
		}
	}
	b.StopTimer()
	runtime.KeepAlive(s)
	require.Equal(b, b.N, pubs, "every alternated full changed fire must publish")
	require.Zero(b, s.skipCount.Load()-skips0, "no fireSkip in the measured loop")
	b.ReportMetric(float64(pubs), "publishes")
}

// BenchmarkRPMFullCompileDigest is the component view: full RoutingCompiler
// compile of the whole plan + production digest (no publish), setup excluded.
func BenchmarkRPMFullCompileDigest(b *testing.B) {
	models := rpmModels(rpmBenchModels())
	s, _ := rpmHomogeneousScheduler(b, models)
	prices := rpmPrices(models)
	c := NewRoutingCompiler()
	in := CompilerInputs{Static: s.View().StaticView(), Prices: prices}
	var d decisionDigester
	for range 3 {
		dv, err := c.Compile(in)
		require.NoError(b, err)
		benchmarkRPMCount = int(d.digest(dv).count)
	}
	require.Positive(b, benchmarkRPMCount, "warm compile must encode a non-empty plan")
	b.ReportAllocs()
	runtime.GC()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dv, err := c.Compile(in)
		if err != nil {
			b.Fatal(err)
		}
		benchmarkRPMDigest = d.digest(dv)
	}
	b.StopTimer()
	runtime.KeepAlive(s)
	runtime.KeepAlive(c)
	runtime.KeepAlive(in)
	runtime.KeepAlive(d)
}
