// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"crypto/sha256"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// --- wiring test helpers ---

// failCompiler is a compile-lane seam that always errors (retention contract).
type failCompiler struct{ err error }

func (f *failCompiler) Compile(CompilerInputs) (*DecisionView, error) { return nil, f.err }

// countingCompiler wraps the real compiler and reports each Compile call.
type countingCompiler struct {
	inner *RoutingCompiler
	calls chan struct{}
}

func (c *countingCompiler) Compile(in CompilerInputs) (*DecisionView, error) {
	select {
	case c.calls <- struct{}{}:
	default:
	}
	return c.inner.Compile(in)
}

func wireSources(s *Scheduler, q map[CandidateQualityKey]CandidateQualityInput, prices map[string]domain.ResolvedPrices) {
	s.SetCompilerSources(func() map[CandidateQualityKey]CandidateQualityInput { return q },
		func() map[string]domain.ResolvedPrices { return prices })
}

func allLaneIDs(rd *RouteDecision) []int64 {
	all := append([]int64{}, rd.Primary...)
	all = append(all, rd.Degraded...)
	all = append(all, rd.Explore.IDs...)
	return all
}

// --- deterministic serialization ---

func TestRoutingCompilerWireDeterministicBytes(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000), accWithEnabled(3, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	logged := math.Log(100)
	loggedSlow := math.Log(500)
	prices := map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000), OutputPerM: pricePtr(1000)}}
	qraw := map[int64]CandidateQualityInput{
		1: {Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2900, OutputTokens: 2900},
		2: {Counts: Counts{Attempts: 30, Successes: 15, TTFTCount: 30, SumLog: loggedSlow * 30, SumSq: loggedSlow * loggedSlow * 30}, InputTokens: 1500, OutputTokens: 1500},
		3: {Counts: Counts{Attempts: 10, Successes: 5}, InputTokens: 500, OutputTokens: 500},
	}
	c := NewRoutingCompiler()
	// Insert quality in two different map orders; compiled bytes must match.
	q1 := buildQuality(10, domain.FormatOpenAIChat, "m", qraw, accs)
	q2 := make(map[CandidateQualityKey]CandidateQualityInput, len(q1))
	for k, v := range q1 {
		q2[k] = v
	}
	for k, v := range q1 {
		delete(q2, k)
		q2[k] = v
	}
	v1, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q1, Prices: prices})
	require.NoError(t, err)
	v2, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q2, Prices: prices})
	require.NoError(t, err)
	b1 := decisionViewBytes(v1)
	b2 := decisionViewBytes(v2)
	require.NotEmpty(t, b1)
	require.Equal(t, b1, b2, "same inputs must serialize byte-identically")
	// A semantic change must change bytes (account 3 gains full samples -> lane shift).
	q3 := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{
		1: qraw[1], 2: qraw[2],
		3: {Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2900, OutputTokens: 2900},
	}, accs)
	v3, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q3, Prices: prices})
	require.NoError(t, err)
	require.NotEqual(t, b1, decisionViewBytes(v3), "different plan must change bytes")
}

func TestRoutingCompilerWireGoldenSHA(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	logged := math.Log(100)
	q := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{
		1: {Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2900, OutputTokens: 2900},
		2: {Counts: Counts{Attempts: 10, Successes: 5}, InputTokens: 500, OutputTokens: 500},
	}, accs)
	prices := map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000), OutputPerM: pricePtr(2000)}}
	c := NewRoutingCompiler()
	v, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices})
	require.NoError(t, err)
	sum := sha256.Sum256(decisionViewBytes(v))
	require.Equal(t, "890fc14523730c5355b4e2fab4ca251f7a69a28e1b02d47f5c1f98241f453770", hexOf(sum[:]), "golden serialized DecisionView")
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, x := range b {
		out = append(out, digits[x>>4], digits[x&0xf])
	}
	return string(out)
}

// --- publish wiring: deterministic published DecisionView, static never rolled back ---

func TestRoutingCompilerWirePublishesCompiledView(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	q := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{
		1: qualityInput(30, 29, 100, 100),
		2: qualityInput(30, 29, 100, 100),
	}, accs)
	wireSources(s, q, map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}})

	before := s.View()
	require.Nil(t, before.DecisionView().Routes()[RouteRefFor(10, string(domain.FormatOpenAIChat), "m")])
	s.compileOnce()

	after := s.View()
	require.Same(t, before.StaticView(), after.StaticView(), "static must not roll back or rebuild")
	require.Greater(t, after.Generation(), before.Generation(), "generation monotonic")
	rd, ok := after.DecisionView().Routes()[RouteRefFor(10, string(domain.FormatOpenAIChat), "m")]
	require.True(t, ok, "compiled route must be published")
	require.Len(t, allLaneIDs(rd), 2)
}

func TestRoutingCompilerWireByteEqualityNoPublish(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	q := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{1: qualityInput(30, 29, 100, 100)}, accs)
	wireSources(s, q, map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}})

	s.compileOnce()
	gen1 := s.View().Generation()
	s.compileOnce()
	require.Equal(t, gen1, s.View().Generation(), "identical bytes must not publish")

	// Changed inputs must publish again.
	q2 := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{1: qualityInput(30, 10, 100, 100)}, accs)
	wireSources(s, q2, map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}})
	s.compileOnce()
	require.Greater(t, s.View().Generation(), gen1, "changed bytes must publish")
}

func TestRoutingCompilerWireCompileFailureKeepsOldView(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	q := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{1: qualityInput(30, 29, 100, 100)}, accs)
	wireSources(s, q, map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}})
	s.compileOnce()
	good := s.View()
	require.NotEmpty(t, good.DecisionView().Routes())

	s.compiler = &failCompiler{err: context.DeadlineExceeded}
	s.compileOnce()
	bad := s.View()
	require.Same(t, good, bad, "compile failure must retain the exact old view")
	require.Equal(t, good.Generation(), bad.Generation())
}

// --- live health/latch wiring ---

func TestRoutingCompilerWireHealthLatchExclusion(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000), accWithEnabled(3, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	q := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{
		1: qualityInput(30, 29, 100, 100), 2: qualityInput(30, 29, 100, 100), 3: qualityInput(30, 29, 100, 100),
	}, accs)
	wireSources(s, q, map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}})

	// Account 2 OPEN via live RuntimeHealth view; account 3 latched.
	h := NewRuntimeHealth(nil, "self", nil, nil, nil)
	hk := compilerHealthKeyFor(accs[1], domain.FormatOpenAIChat, "m")
	h.view.Store(&healthView{entries: map[HealthKey]healthEntry{hk: {Key: hk, State: StateOPEN}}})
	s.SetRuntimeHealth(h)
	require.True(t, s.TryLatch(accs[2].ID, compilerLatchKeyFor(accs[2]).Fingerprint, accs[2].LifecycleRevision))

	s.compileOnce()
	rd, ok := s.View().DecisionView().Routes()[RouteRefFor(10, string(domain.FormatOpenAIChat), "m")]
	require.True(t, ok)
	all := allLaneIDs(rd)
	require.Contains(t, all, int64(1))
	require.NotContains(t, all, int64(2), "live health OPEN must exclude via wiring")
	require.NotContains(t, all, int64(3), "live latch must exclude via wiring")
}

func TestRoutingCompilerWireResolvedModelQualityIdentity(t *testing.T) {
	tpl := &domain.Template{SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"req"}, ModelMapping: map[string]domain.ModelMappingEntry{"req": {MappedModel: "resolved", Mode: domain.ModelMappingModeExplicit}}}
	acc := accWithEnabled(1, tpl, true, 10000)
	acc.LifecycleRevision = 1
	m := newMemLoader(map[int64][]*domain.Account{10: {acc}})
	s := newSched(t, m)
	logged := math.Log(100)
	q := buildQuality(10, domain.FormatOpenAIChat, "req", map[int64]CandidateQualityInput{
		1: {Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2900},
	}, []*domain.Account{acc})
	wireSources(s, q, map[string]domain.ResolvedPrices{"req": {InputPerM: pricePtr(1000)}})

	op := operationTagForFormat(string(domain.FormatOpenAIChat))
	qcResolved, _ := domain.QualityClassID(callerKindForFormat(domain.FormatOpenAIChat), domain.FormatOpenAIChat, "resolved", op)
	h := NewRuntimeHealth(nil, "self", nil, nil, nil)
	hk := HealthKey{AccountID: 1, Quality: domain.QualityClassIDHex(qcResolved), Revision: 1}
	h.view.Store(&healthView{entries: map[HealthKey]healthEntry{hk: {Key: hk, State: StateOPEN}}})
	s.SetRuntimeHealth(h)

	s.compileOnce()
	rd, ok := s.View().DecisionView().Routes()[RouteRefFor(10, string(domain.FormatOpenAIChat), "req")]
	require.True(t, ok)
	require.NotContains(t, allLaneIDs(rd), int64(1), "resolved-model health must exclude mapped candidate")
}

// --- full candidate union + overflow tail through wiring ---

func TestRoutingCompilerWireFullUnionOverflowTail(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := make([]*domain.Account, 0, 12)
	qraw := make(map[int64]CandidateQualityInput, 12)
	for i := 1; i <= 12; i++ {
		a := accWithEnabled(int64(i), tpl, true, 10000)
		accs = append(accs, a)
		qraw[int64(i)] = CandidateQualityInput{Counts: Counts{Attempts: 5, Successes: 2}, InputTokens: 50, OutputTokens: 50}
	}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	wireSources(s, buildQuality(10, domain.FormatOpenAIChat, "m", qraw, accs), map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}})

	s.compileOnce()
	rd, ok := s.View().DecisionView().Routes()[RouteRefFor(10, string(domain.FormatOpenAIChat), "m")]
	require.True(t, ok)
	all := allLaneIDs(rd)
	require.Len(t, all, 12, "full union: every candidate exactly once")
	require.ElementsMatch(t, []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}, all)
	require.Len(t, rd.Explore.Fallback, 12, "overflow tail must not truncate")
	require.Equal(t, rd.Explore.IDs, rd.Explore.Fallback, "low-sample tail covers full explore set")
}

// --- debounce loop ---

func TestRoutingCompilerWireDebounceCoalesces(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	q := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{1: qualityInput(30, 29, 100, 100)}, accs)
	wireSources(s, q, map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}})
	cc := &countingCompiler{inner: NewRoutingCompiler(), calls: make(chan struct{}, 16)}
	s.compiler = cc

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		s.compileLoop(ctx)
	}()
	for i := 0; i < 5; i++ {
		s.RequestCompile()
	}
	select {
	case <-cc.calls:
	case <-time.After(3 * time.Second):
		t.Fatal("compile loop did not run after triggers")
	}
	// Serial lane: cancelling and joining proves the single compile+publish completed.
	cancel()
	select {
	case <-loopDone:
	case <-time.After(3 * time.Second):
		t.Fatal("compile loop did not exit")
	}
	require.Len(t, cc.calls, 0, "burst of triggers must coalesce into one compile")
	_, ok := s.View().DecisionView().Routes()[RouteRefFor(10, string(domain.FormatOpenAIChat), "m")]
	require.True(t, ok, "loop must publish the compiled view")
}

func TestRoutingCompilerWireRequestCompileNonBlocking(t *testing.T) {
	m := newMemLoader(map[int64][]*domain.Account{10: {}})
	s := newSched(t, m)
	wireSources(s, nil, nil)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			s.RequestCompile()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RequestCompile blocked without a consumer")
	}
}
