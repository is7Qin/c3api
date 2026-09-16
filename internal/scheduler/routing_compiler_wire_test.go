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
	s.sources = &CompilerSources{
		Quality: func(time.Time) WindowedQuality { return WindowedQuality{Current: q} },
		Prices:  func() map[string]domain.ResolvedPrices { return prices },
	}
}

func allLaneIDs(rd *RouteDecision) []int64 {
	all := make([]int64, 0, len(rd.Primary)+len(rd.Degraded)+len(rd.Explore.Ordered))
	for _, c := range rd.Primary {
		all = append(all, c.AccountID)
	}
	for _, c := range rd.Degraded {
		all = append(all, c.AccountID)
	}
	for _, c := range rd.Explore.Ordered {
		all = append(all, c.AccountID)
	}
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
	// v4-S2: table keys are normalized (hex lives in the interned decision
	// values) — golden regenerated for the normalized key form.
	// Explore-share wiring: the serialized form now carries ExploreBP per
	// route (1 primary + 1 unknown of 2 eligible → 100+ceil(400*1/2)=300bp)
	// — golden regenerated for the bp-carrying form.
	// Incident wiring: the serialized form now carries the expose-only
	// incident mark per route (zero here — no baseline/evaluator in this
	// direct compile) — golden regenerated for the incident-carrying form.
	require.Equal(t, "985be50a3bfde2c065a4543a678a0c48848ce2ed58f5a6e196853aa6e5eb6e09", hexOf(sum[:]), "golden serialized DecisionView")
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
	s := newSchedStatic(t, m)
	q := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{
		1: qualityInput(30, 29, 100, 100),
		2: qualityInput(30, 29, 100, 100),
	}, accs)
	wireSources(s, q, map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}})

	before := s.View()
	require.Nil(t, before.DecisionView().Routes()[RouteRefFor(10, string(domain.FormatOpenAIChat), "m")])
	s.compileOnce()

	after := s.View()
	require.NotSame(t, before.StaticView(), after.StaticView(), "initial pairing wraps the static-only root fresh, never mutates it")
	require.Same(t, before.ByID()[1], after.ByID()[1], "static must not roll back or rebuild: shared immutable leaves")
	require.Equal(t, after.Generation(), after.StaticView().Generation(), "one generation for the pair")
	require.Equal(t, after.Generation(), after.DecisionView().Generation(), "one generation for the pair")
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

func TestRoutingCompilerWireStaticRootForcesPublish(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {accWithEnabled(1, tpl, true, 4)}})
	s := newSched(t, m)
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	before := s.View()
	oldBytes := decisionViewBytes(before.DecisionView())

	m.mu.Lock()
	m.byGroup[10] = []*domain.Account{accWithEnabled(1, tpl, true, 1)}
	m.mu.Unlock()
	require.NoError(t, s.reload(context.Background()))
	reloaded := s.View()
	// Atomic publication: staging leaves the published pair untouched.
	require.Same(t, before, reloaded, "staging must not touch the published pair")
	require.Same(t, before.DecisionView(), reloaded.DecisionView(), "published decision retained while pending")
	require.Equal(t, oldBytes, decisionViewBytes(reloaded.DecisionView()), "staged static keeps detached decision bytes identical")

	s.compileOnce()
	after := s.View()
	require.NotSame(t, reloaded.DecisionView(), after.DecisionView(), "static root identity must bypass byte cache")
	require.Equal(t, oldBytes, decisionViewBytes(after.DecisionView()))
	rd, ok := after.DecisionView().routes[route]
	require.True(t, ok)
	require.Len(t, rd.Explore.Ordered, 1)
	require.Same(t, after.ByID()[1], rd.Explore.Ordered[0].Leaf)
	require.Same(t, after.ByID()[1].static.Load(), rd.Explore.Ordered[0].Static)

	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err, "fresh compiled candidate must reserve after static-only reload")
	require.Equal(t, int64(1), sel.AccountID)
	sel.Release()
}

// TestRoutingCompilerWireInitialPairKeepsStaticOnlyRootImmutable pins the
// published-root immutability contract: the startup static-only view is
// staged as pending, and the paired publish wraps it fresh (shared immutable
// maps) instead of mutating its generation in place. The old view's pointer
// and generation are unchanged; the new pair shares one generation and its
// lane candidates lease the current leaves.
func TestRoutingCompilerWireInitialPairKeepsStaticOnlyRootImmutable(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {accWithEnabled(1, tpl, true, 4)}})
	s := newSchedStatic(t, m)
	q := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{1: qualityInput(30, 29, 100, 100)}, m.byGroup[10])
	wireSources(s, q, map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")

	before := s.View()
	require.NotNil(t, before)
	require.Nil(t, before.DecisionView(), "startup is static-only")
	oldStatic := before.StaticView()
	require.NotNil(t, oldStatic)
	oldGen := before.Generation()

	s.compileOnce()
	after := s.View()
	require.NotSame(t, before, after, "paired publish replaces the view")
	require.Equal(t, oldGen, before.Generation(), "old view generation never mutated")
	require.Equal(t, oldGen, oldStatic.Generation(), "published root never mutated")
	require.Equal(t, after.Generation(), after.StaticView().Generation(), "one generation for the pair")
	require.Equal(t, after.Generation(), after.DecisionView().Generation(), "one generation for the pair")
	require.NotSame(t, oldStatic, after.StaticView(), "fresh wrapper, shared maps")
	require.Same(t, oldStatic.byID[1], after.ByID()[1], "wrapper shares immutable leaves")

	rd, ok := after.DecisionView().routes[route]
	require.True(t, ok)
	require.NotEmpty(t, allLaneIDs(rd), "paired decision carries candidates")
	for _, c := range append(append(append([]CompiledCandidate{}, rd.Primary...), rd.Explore.Ordered...), rd.Degraded...) {
		leaf, ok := after.ByID()[c.AccountID]
		require.True(t, ok)
		require.Same(t, leaf, c.Leaf, "lane candidate leases the current leaf")
	}

	// No churn: recompiling the unchanged root skips publish.
	gen := after.Generation()
	s.compileOnce()
	require.Equal(t, gen, s.View().Generation(), "identical inputs must not republish")
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

func TestRoutingCompilerWireRejectsStaleStaticCompile(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {accWithEnabled(1, tpl, true, 4)}})
	s := newSchedStatic(t, m)
	wireSources(s, nil, nil)
	bc := &blockingCompiler{entered: make(chan struct{}, 1), release: make(chan struct{}), inner: NewRoutingCompiler()}
	s.compiler = bc
	t.Cleanup(func() {
		select {
		case <-bc.release:
		default:
			close(bc.release)
		}
	})
	old := s.View()
	compileDone := make(chan struct{})
	go func() {
		s.compileOnce()
		close(compileDone)
	}()
	<-bc.entered

	m.mu.Lock()
	m.byGroup[10] = []*domain.Account{accWithEnabled(1, tpl, true, 1)}
	m.mu.Unlock()
	s.InvalidateGroup(10)
	current := s.View()
	require.NotSame(t, old.StaticView(), current.StaticView())
	select {
	case <-s.compileCh:
	default:
		t.Fatal("static invalidation did not request a compile")
	}

	close(bc.release)
	<-compileDone
	require.Same(t, current, s.View(), "stale compile must not publish onto a newer static root")
	select {
	case <-s.compileCh:
	default:
		t.Fatal("stale compile rejection did not request a fresh compile")
	}

	s.compileOnce()
	fresh := s.View()
	require.NotSame(t, current, fresh)
	require.NotNil(t, fresh.DecisionView())
	rd, ok := fresh.DecisionView().routes[RouteRefFor(10, string(domain.FormatOpenAIChat), "m")]
	require.True(t, ok)
	require.Len(t, rd.Explore.Ordered, 1)
	require.Same(t, fresh.ByID()[1], rd.Explore.Ordered[0].Leaf)
}

// --- atomic publication: staged static pairs only with its own compile ---

// TestRoutingCompilerWirePendingRetainsCompletePair pins the atomic-pair
// contract under a compiler/publish race: while a compile is in flight, a
// static invalidation stages pending without touching the published pair;
// the in-flight result (compiled from the superseded root) is dropped with a
// fresh compile requested; the next compile pairs the pending root — one
// generation, leaves pointing into the new static.
func TestRoutingCompilerWirePendingRetainsCompletePair(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {accWithEnabled(1, tpl, true, 4)}})
	s := newSched(t, m)
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	old := s.View()
	require.NotNil(t, old.DecisionView(), "setup publishes a complete pair")

	bc := &blockingCompiler{entered: make(chan struct{}, 1), release: make(chan struct{}), inner: NewRoutingCompiler()}
	s.compiler = bc
	t.Cleanup(func() {
		select {
		case <-bc.release:
		default:
			close(bc.release)
		}
	})
	compileDone := make(chan struct{})
	go func() {
		s.compileOnce()
		close(compileDone)
	}()
	<-bc.entered // compile in flight against the published root

	m.mu.Lock()
	m.byGroup[10] = []*domain.Account{accWithEnabled(1, tpl, true, 1), accWithEnabled(2, tpl, true, 1)}
	m.mu.Unlock()
	s.InvalidateGroup(10)
	during := s.View()
	require.Same(t, old, during, "old complete pair visible while pending")

	close(bc.release)
	select {
	case <-compileDone:
	case <-time.After(2 * time.Second):
		t.Fatal("superseded compile did not complete")
	}
	require.Same(t, old, s.View(), "superseded compile must not publish")
	select {
	case <-s.compileCh:
	default:
		t.Fatal("superseded compile did not request a fresh compile")
	}

	s.compileOnce()
	fresh := s.View()
	require.NotSame(t, old, fresh)
	require.Equal(t, fresh.Generation(), fresh.StaticView().Generation(), "one generation for the pair")
	require.Equal(t, fresh.Generation(), fresh.DecisionView().Generation(), "one generation for the pair")
	rd, ok := fresh.DecisionView().routes[route]
	require.True(t, ok)
	require.Contains(t, allLaneIDs(rd), int64(2), "new account enters the paired plan")
	for _, c := range append(append(append([]CompiledCandidate{}, rd.Primary...), rd.Explore.Ordered...), rd.Degraded...) {
		leaf, ok := fresh.ByID()[c.AccountID]
		require.True(t, ok)
		require.Same(t, leaf, c.Leaf, "lane candidate leases the current leaf")
	}
}

// TestRoutingCompilerWireCompileFailureRetainsPendingPair pins failure
// retention with a staged root: the exact old pair and the pending root both
// survive, and the retained pending root still pairs on the next success.
func TestRoutingCompilerWireCompileFailureRetainsPendingPair(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {accWithEnabled(1, tpl, true, 4)}})
	s := newSched(t, m)
	old := s.View()

	m.mu.Lock()
	m.byGroup[10] = append(m.byGroup[10], accWithEnabled(2, tpl, true, 4))
	m.mu.Unlock()
	require.NoError(t, s.reload(context.Background()))
	require.Same(t, old, s.View(), "staging retains the old pair")

	s.compiler = &failCompiler{err: context.DeadlineExceeded}
	s.compileOnce()
	require.Same(t, old, s.View(), "compile failure retains the exact old pair")
	require.NotNil(t, s.publisher.pending, "pending root retained for the next compile")
	_, has2 := s.publisher.pending.byID[2]
	require.True(t, has2, "staged account survives the failure")

	s.compiler = NewRoutingCompiler()
	s.compileOnce()
	fresh := s.View()
	require.NotSame(t, old, fresh)
	require.Contains(t, fresh.ByID(), int64(2))
	require.Equal(t, fresh.Generation(), fresh.StaticView().Generation(), "one generation for the pair")
	require.Equal(t, fresh.Generation(), fresh.DecisionView().Generation(), "one generation for the pair")
}

// --- live health/latch wiring ---

func TestRoutingCompilerWireHealthLatchExclusion(t *testing.T) {
	// v5-§5.1A (COMPILED-HEALTH-FREE): live OPEN health + live latch must NOT
	// exclude from the compiled plan — serving exclusion lives solely in
	// reserveOnView (identical gates per attempt, strictly fresher). The
	// compiled plan therefore carries all three accounts while Select (via
	// reserve gates) serves only the ungated one.
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000), accWithEnabled(3, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	q := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{
		1: qualityInput(30, 29, 100, 100), 2: qualityInput(30, 29, 100, 100), 3: qualityInput(30, 29, 100, 100),
	}, accs)
	wireSources(s, q, map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}})

	// Account 2 OPEN via live RuntimeHealth view; account 3 latched.
	h := NewRuntimeHealth(nil, "self", nil, nil)
	hk := compilerHealthKeyFor(accs[1], domain.FormatOpenAIChat, "m")
	h.view.Store(&healthView{entries: map[HealthKey]healthEntry{hk: {Key: hk, State: StateOPEN}}})
	s.health = h
	require.True(t, s.TryLatch(accs[2].ID, compilerLatchKeyFor(accs[2]).Fingerprint, accs[2].LifecycleRevision))

	s.compileOnce()
	rd, ok := s.View().DecisionView().Routes()[RouteRefFor(10, string(domain.FormatOpenAIChat), "m")]
	require.True(t, ok)
	require.ElementsMatch(t, []int64{1, 2, 3}, allLaneIDs(rd), "compiled plan is health/latch-free post-v5")

	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err, "reserve gates skip 2 and 3 within the same request")
	require.Equal(t, int64(1), sel.AccountID, "only the ungated account serves")
	sel.Release()
}

func TestRoutingCompilerWireResolvedModelQualityIdentity(t *testing.T) {
	// v5-§5.1A: resolved-model OPEN health no longer excludes the mapped
	// candidate from compilation; reserveOnView still skips it at serve time,
	// so Select exhausts on the single gated candidate.
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
	h := NewRuntimeHealth(nil, "self", nil, nil)
	hk := HealthKey{AccountID: 1, Quality: domain.QualityClassIDHex(qcResolved), Revision: 1}
	h.view.Store(&healthView{entries: map[HealthKey]healthEntry{hk: {Key: hk, State: StateOPEN}}})
	s.health = h

	s.compileOnce()
	rd, ok := s.View().DecisionView().Routes()[RouteRefFor(10, string(domain.FormatOpenAIChat), "req")]
	require.True(t, ok)
	require.Contains(t, allLaneIDs(rd), int64(1), "resolved-model health must not exclude post-v5")
	_, err := s.Select(10, domain.FormatOpenAIChat, "req")
	require.Error(t, err, "reserve gate skips the only candidate at serve time")
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
	require.Equal(t, compiledIDs(rd.Explore.Ordered), fallbackIDs(rd.Explore), "low-sample tail covers full explore set")
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

// --- Task18 observability: compile-lane stats + failure retention ---

func TestRoutingCompilerWireStatsCompileLane(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSchedStatic(t, m)
	fixed := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	s.timeNow = func() time.Time { return fixed }
	q := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{1: qualityInput(30, 29, 100, 100)}, accs)
	wireSources(s, q, map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}})

	st := s.Stats().(SchedulerStats)
	require.Equal(t, 1, st.CompileCap, "compile trigger channel is cap-1 coalescing")
	require.Zero(t, st.CompilePending)
	require.Zero(t, st.LastCompileOKUnixMs, "0 = never compiled")
	require.Zero(t, st.LastCompileErrUnixMs)
	require.Zero(t, st.DecisionRoutes, "unpublished decision view has no routes")

	s.RequestCompile()
	st = s.Stats().(SchedulerStats)
	require.Equal(t, 1, st.CompilePending, "pending trigger visible without a consumer")

	s.compileOnce()
	st = s.Stats().(SchedulerStats)
	require.Equal(t, fixed.UnixMilli(), st.LastCompileOKUnixMs, "success freshness = injected clock")
	require.Zero(t, st.LastCompileErrUnixMs)
	require.NotZero(t, st.DecisionRoutes, "published plan visible on ops face")
	require.Equal(t, s.View().DecisionView().Generation(), st.DecisionGeneration)

	// Compile failure: old view retained AND the failure is observable (freshness
	// of the last success is not erased; the error stamp moves).
	s.compiler = &failCompiler{err: context.DeadlineExceeded}
	before := s.View()
	s.compileOnce()
	st = s.Stats().(SchedulerStats)
	require.Same(t, before, s.View(), "compile failure must retain the exact old view")
	require.Equal(t, fixed.UnixMilli(), st.LastCompileErrUnixMs)
	require.Equal(t, fixed.UnixMilli(), st.LastCompileOKUnixMs, "failure does not erase last success")
	require.NotZero(t, st.DecisionRoutes, "retained plan still published on ops face")
}

// blockingCompiler signals entry into Compile and blocks until released —
// barrier for the Close-join contract.
type blockingCompiler struct {
	entered chan struct{}
	release chan struct{}
	inner   *RoutingCompiler
}

func (b *blockingCompiler) Compile(in CompilerInputs) (*DecisionView, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	return b.inner.Compile(in)
}

// TestSchedulerCloseJoinsCompileLoop pins the orderly-shutdown contract:
// Close must not return while a compile is in flight (the loop is joined,
// same discipline as runtime-health / quality-sync). Barriers + one bounded
// watchdog (quality-sync precedent), no sleep-race masking.
func TestSchedulerCloseJoinsCompileLoop(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000)}
	s := newSched(t, newMemLoader(map[int64][]*domain.Account{10: accs}))
	bc := &blockingCompiler{entered: make(chan struct{}, 1), release: make(chan struct{}), inner: NewRoutingCompiler()}
	s.compiler = bc

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, s.Start(ctx, &CompilerSources{
		Prices: func() map[string]domain.ResolvedPrices {
			return map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}}
		},
	}))
	s.RequestCompile()
	<-bc.entered // compile in flight, loop cannot exit until released
	cancel()     // base ctx gone: the loop exits only after compileOnce returns

	closeDone := make(chan error, 1)
	go func() { closeDone <- s.Close(context.Background()) }()
	select {
	case err := <-closeDone:
		close(bc.release)
		t.Fatalf("Close returned while a compile was in flight: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(bc.release)
	require.NoError(t, <-closeDone)
	d := s.compileDone.Load()
	require.NotNil(t, d)
	select {
	case <-*d:
	default:
		t.Fatal("compile loop still running after Close returned")
	}
}

// TestSchedulerCloseUnstartedSafe pins the worker contract: Close before
// Start never blocks and never panics (compileDone absent).
func TestSchedulerCloseUnstartedSafe(t *testing.T) {
	s := newSched(t, newMemLoader(nil))
	require.NoError(t, s.Close(context.Background()))
}

// TestDecisionEncoderReuseNoContamination 复用编码器（编译道热路径）跨 fire
// 不得互相污染：同一 encoder 交替编码两个视图，结果必须与一次性编码器逐字节
// 一致（refs/ids 暂存与输出缓冲的 Reset 正确性；返回字节 alias 缓冲，消费方
// 需自行拷贝——生产路径以 append 复用 lastDecisionBytes）。
func TestDecisionEncoderReuseNoContamination(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	c := NewRoutingCompiler()
	prices := map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}}
	logged := math.Log(100)
	full := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{
		1: {Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2900, OutputTokens: 2900},
		2: {Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2900, OutputTokens: 2900},
	}, accs)
	thin := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{
		1: {Counts: Counts{Attempts: 10, Successes: 5}, InputTokens: 500, OutputTokens: 500},
	}, accs)
	vFull, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: full, Prices: prices})
	require.NoError(t, err)
	vThin, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: thin, Prices: prices})
	require.NoError(t, err)

	var e decisionEncoder
	gotFull := append([]byte(nil), e.encode(vFull)...)
	gotThin := append([]byte(nil), e.encode(vThin)...)
	gotFull2 := append([]byte(nil), e.encode(vFull)...)
	require.Equal(t, decisionViewBytes(vFull), gotFull)
	require.Equal(t, decisionViewBytes(vThin), gotThin)
	require.Equal(t, gotFull, gotFull2, "reuse must not contaminate a later encode of the same view")
	require.NotEqual(t, gotFull, gotThin, "different views must not collide")
}
