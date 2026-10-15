// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

// HEAD-side routing-plan-memory performance harness (spec §6). The fixture
// builders live in routing_plan_memory_fixture_test.go and are BASE-compatible;
// the measured loops here use HEAD-only symbols (decisionDigester). The BASE
// twin measures the same fixture with encode+bytes.Equal; see the implementation
// report for the frozen BASE harness source and the interleaved run commands.
//
// All benchmarks are single-process, serial, fixed-fixture, warm (3 pre-timer
// iterations) and setup outside the timer. Run per §6 pre-registration:
// GOMAXPROCS=1 GOGC=100, ≥5 ABBA interleaved groups, ≥1s benchtime.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

var benchmarkRPMCount int

// rpmQuality builds a quality map for one (group, format, model) route over the
// given accounts, so the compiled candidate metadata (and therefore the
// canonical digest) differs from an unqualified compile.
func rpmQuality(gid int64, format domain.RequestFormat, model string, accs []*domain.Account, attempts, successes int) map[CandidateQualityKey]CandidateQualityInput {
	raw := make(map[int64]CandidateQualityInput, len(accs))
	for _, a := range accs {
		raw[a.ID] = qualityInput(attempts, successes, 1000, 1000)
	}
	return buildQuality(gid, format, model, raw, accs)
}

// BenchmarkRPMGuardEqual measures the production equal-path guard: one
// SHA-256 digest over the published view per iteration (no fireSkip, the guard
// always executes). Warm loop reuses the sink's refs/ids scratch.
func BenchmarkRPMGuardEqual(b *testing.B) {
	s, _ := rpmHomogeneousScheduler(b, rpmModels(100))
	dv := s.View().DecisionView()
	require.NotNil(b, dv)
	var d decisionDigester
	for range 3 {
		benchmarkRPMCount = int(d.digest(dv).count)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkRPMCount = int(d.digest(dv).count)
	}
}

// BenchmarkRPMGuardChanged alternates two distinct equal-scale plans, forcing a
// full digest commit each iteration.
func BenchmarkRPMGuardChanged(b *testing.B) {
	s, m := rpmHomogeneousScheduler(b, rpmModels(100))
	c := NewRoutingCompiler()
	q := rpmQuality(10, domain.FormatOpenAIChat, "m000", m.byGroup[10], 30, 29)
	dvA, err := c.Compile(CompilerInputs{Static: s.View().StaticView()})
	require.NoError(b, err)
	dvB, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q})
	require.NoError(b, err)
	var d decisionDigester
	for range 3 {
		benchmarkRPMCount = int(d.digest(dvA).count)
		benchmarkRPMCount = int(d.digest(dvB).count)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i&1 == 0 {
			benchmarkRPMCount = int(d.digest(dvA).count)
		} else {
			benchmarkRPMCount = int(d.digest(dvB).count)
		}
	}
}

// BenchmarkRPMFireEqualGuarded measures one real equal fire end-to-end: a group
// scope forces a scoped recompute (never fireSkip) whose digest matches the
// last published digest, so the fire recompiles + digests but does not publish.
// enqueueCompileScope is used directly (no static-root staging), so the fire
// stays an equal-digest same-root fire.
func BenchmarkRPMFireEqualGuarded(b *testing.B) {
	s, _ := rpmHomogeneousScheduler(b, rpmModels(100))
	warm := func() {
		s.enqueueCompileScope([]int64{10}, nil, scopeCauseGroup)
		s.compileOnce()
	}
	for range 3 {
		warm()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.enqueueCompileScope([]int64{10}, nil, scopeCauseGroup)
		s.compileOnce()
	}
}

// BenchmarkRPMFireChangedGuarded alternates two equal-scale quality inputs so
// each fire commits a new digest and publishes.
func BenchmarkRPMFireChangedGuarded(b *testing.B) {
	s, m := rpmHomogeneousScheduler(b, rpmModels(100))
	qA := rpmQuality(10, domain.FormatOpenAIChat, "m000", m.byGroup[10], 30, 29)
	qB := rpmQuality(10, domain.FormatOpenAIChat, "m000", m.byGroup[10], 10, 5)
	srcA := &CompilerSources{Quality: func(time.Time) WindowedQuality { return WindowedQuality{Current: qA} }}
	srcB := &CompilerSources{Quality: func(time.Time) WindowedQuality { return WindowedQuality{Current: qB} }}
	s.sources = srcA
	s.compileOnce()
	warm := func(src *CompilerSources) {
		s.sources = src
		s.enqueueCompileScope([]int64{10}, nil, scopeCauseGroup)
		s.compileOnce()
	}
	for range 3 {
		warm(srcB)
		warm(srcA)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i&1 == 0 {
			s.sources = srcB
		} else {
			s.sources = srcA
		}
		s.enqueueCompileScope([]int64{10}, nil, scopeCauseGroup)
		s.compileOnce()
	}
}

// BenchmarkRPMFullCompileDigest measures a full compile of the whole plan plus
// the production digest (the guard's new work), setup excluded.
func BenchmarkRPMFullCompileDigest(b *testing.B) {
	s, _ := rpmHomogeneousScheduler(b, rpmModels(100))
	c := NewRoutingCompiler()
	in := CompilerInputs{Static: s.View().StaticView()}
	var d decisionDigester
	for range 3 {
		dv, err := c.Compile(in)
		require.NoError(b, err)
		benchmarkRPMCount = int(d.digest(dv).count)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dv, err := c.Compile(in)
		if err != nil {
			b.Fatal(err)
		}
		benchmarkRPMCount = int(d.digest(dv).count)
	}
}
