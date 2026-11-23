// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

// Publish-guard acceptance tests for the compile lane's decision guard.
//
// U31 D1: the whole-view digest is computed only when the skip guard could
// actually match (same static root). U33 D2: a same-root fire that recompiled
// only the affected routes does not hash the whole view either — it compares the
// affected route bodies (the scoped merge hands every unaffected route forward by
// pointer), so the guard is exact without a 1.9 GB traversal per fire. The
// whole-stream digest survives as the fallback for a fire that had to rebuild
// every route.
//
// These are state/count/generation assertions — no wall clock — so they stay
// deterministic on any machine; the wall-clock counterpart lives in the U27/U33
// benchmarks. Witness for "no digest was computed": the zero decisionDigest (sum
// all-zero, count 0, isNil false). On the U27 fixture a computed digest carries
// count = 1,895,419,353 (1.9 GB of canonical bytes), so "count == 0" is a direct
// proof that the canonical encoding was never streamed.

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// u31d1Sched builds the two-account single-group fixture used by the guard tests.
// Sources are wired BEFORE the first compile, so the first published decision
// already reflects quality A and a later same-root fire over the same inputs
// compiles byte-identical content.
func u31d1Sched(t *testing.T) (*Scheduler, []*domain.Account, map[string]domain.ResolvedPrices) {
	t.Helper()
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000)}
	s := newSchedStatic(t, newMemLoader(map[int64][]*domain.Account{10: accs}))
	prices := map[string]domain.ResolvedPrices{"m": {InputPerM: new(int64(1000))}}
	wireSources(s, u31d1QualityA(accs), prices)
	s.compileOnce()
	return s, accs, prices
}

// u31d1QualityA prefers account 1; u31d1QualityB prefers account 2, so flipping
// between them changes the compiled decision bytes without staging a root.
func u31d1QualityA(accs []*domain.Account) map[CandidateQualityKey]CandidateQualityInput {
	return buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{
		1: qualityInput(30, 29, 100, 100), 2: qualityInput(10, 5, 100, 100),
	}, accs)
}

func u31d1QualityB(accs []*domain.Account) map[CandidateQualityKey]CandidateQualityInput {
	return buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{
		1: qualityInput(10, 5, 100, 100), 2: qualityInput(30, 29, 100, 100),
	}, accs)
}

// u31d1RequireNoDigest pins the unloaded guard: publish happened, but no
// canonical encoding was streamed.
func u31d1RequireNoDigest(t *testing.T, s *Scheduler, why string) {
	t.Helper()
	require.False(t, s.lastDigestSet, "%s: the digest guard must be unloaded", why)
	require.Equal(t, decisionDigest{}, s.lastDigest, "%s: no digest may be computed", why)
	require.Zero(t, s.lastDigest.count, "%s: canonical bytes must not be streamed", why)
}

// TestU31D1_StagedFireComputesNoDigest is D1 acceptance §4.1.1: every staged
// fire (reload and InvalidateGroup both stage a brand-new root) publishes
// without hashing the decision, because the skip guard is unreachable.
func TestU31D1_StagedFireComputesNoDigest(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage func(*Scheduler) error
	}{
		{"reload", func(s *Scheduler) error { return s.reload(context.Background()) }},
		{"invalidate-group", func(s *Scheduler) error { s.InvalidateGroup(10); return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := u31d1Sched(t)
			// newSched already published one staged root; assert it, then run a
			// second staged fire through the chosen staging entry point.
			u31d1RequireNoDigest(t, s, "initial staged publish")
			gen0 := s.View().Generation()

			require.NoError(t, tc.stage(s))
			require.NotNil(t, s.publisher.pending, "staging must leave a pending root")
			s.compileOnce()
			require.Nil(t, s.publisher.pending, "a staged fire must publish the pending root")
			require.Greater(t, s.View().Generation(), gen0, "a staged fire must publish")
			u31d1RequireNoDigest(t, s, tc.name)
			require.Same(t, s.View().StaticView(), s.lastCompiledStatic,
				"the guard must track the published static root even while unloaded")
		})
	}
}

// TestU33D2_SameRootDriftPublishesOnlyRealChanges is the D2 determinism gate: on
// the published root, identical content is absorbed and changed content
// publishes — and neither fire streams the canonical byte stream.
//
// The first same-root fire after a staged (full) publish still publishes once
// even with identical bytes: the published view is whole=true while a scoped fire
// can only produce whole=false. That is D2's deliberate strengthening — the old
// digest could not see the wholeness bit, so it would have skipped.
func TestU33D2_SameRootDriftPublishesOnlyRealChanges(t *testing.T) {
	s, accs, prices := u31d1Sched(t)
	u31d1RequireNoDigest(t, s, "staged publish")
	require.True(t, s.View().DecisionView().whole, "the staged publish came from a full compile")
	gen0 := s.View().Generation()

	// (1) Same root, identical bytes, wholeness flips => publish once.
	s.enqueueCompileScope([]int64{10}, nil, scopeCauseGroup)
	s.compileOnce()
	gen1 := s.View().Generation()
	require.Greater(t, gen1, gen0,
		"a wholeness flip must publish (D2 is stricter than the digest, never looser)")
	require.False(t, s.View().DecisionView().whole, "a scoped publish is never whole")
	u31d1RequireNoDigest(t, s, "scoped publish")

	// (2) Same root, identical content AND wholeness => absorbed.
	s.enqueueCompileScope([]int64{10}, nil, scopeCauseGroup)
	s.compileOnce()
	require.Equal(t, gen1, s.View().Generation(),
		"identical content on the same root must not publish")
	u31d1RequireNoDigest(t, s, "absorbed same-root fire")

	// (3) Same root, changed content (the quality flip is its own scope) => publish.
	wireSources(s, u31d1QualityB(accs), prices)
	s.compileOnce()
	gen2 := s.View().Generation()
	require.Greater(t, gen2, gen1, "changed content must publish")
	u31d1RequireNoDigest(t, s, "changed-content publish")

	// (4) Same root, identical again => absorbed.
	s.enqueueCompileScope([]int64{10}, nil, scopeCauseGroup)
	s.compileOnce()
	require.Equal(t, gen2, s.View().Generation(),
		"identical content after a publish must not publish again")
	require.Same(t, s.View().StaticView(), s.lastCompiledStatic,
		"the guard must keep tracking the published static root")
}

// TestU33D2_RevertAfterScopedStagedPublishStillPublishes is the anti-staleness
// gate: A -> B (staged root, scoped fire) -> revert to A must publish. The guard
// keeps no digest that could go stale — it re-derives equality from the published
// bytes every fire — so the revert cannot be mistaken for the published B.
func TestU33D2_RevertAfterScopedStagedPublishStillPublishes(t *testing.T) {
	s, accs, prices := u31d1Sched(t)
	// Scoped baseline for content A (also flips wholeness to false).
	s.enqueueCompileScope([]int64{10}, nil, scopeCauseGroup)
	s.compileOnce()
	require.False(t, s.View().DecisionView().whole)
	bytesA := decisionViewBytes(s.View().DecisionView())

	// Staged root + scoped fire publishes content B. InvalidateGroup stages a
	// group-scoped root (not a full-stage mark), so the fire stays scoped and the
	// published view keeps whole=false — the comparison under test is the bytes,
	// not the wholeness bit.
	wireSources(s, u31d1QualityB(accs), prices)
	s.InvalidateGroup(10)
	require.NotNil(t, s.publisher.pending)
	s.compileOnce()
	genB := s.View().Generation()
	require.False(t, s.View().DecisionView().whole)
	require.False(t, bytes.Equal(bytesA, decisionViewBytes(s.View().DecisionView())),
		"fixture shape: the staged fire must change the canonical bytes")
	u31d1RequireNoDigest(t, s, "scoped publish of B on a staged root")

	// Revert to A without staging.
	wireSources(s, u31d1QualityA(accs), prices)
	s.compileOnce()
	require.Greater(t, s.View().Generation(), genB,
		"a revert must publish; the guard must never hide a changed view")
	require.True(t, bytes.Equal(bytesA, decisionViewBytes(s.View().DecisionView())),
		"the re-published view carries A's canonical bytes again")
	u31d1RequireNoDigest(t, s, "revert publish")
}

// TestU33D2_StaticOnlyInitialStateDoesNotArm pins the one state where the
// published root is already the staged root: stageLocked publishes a static-only
// view (decision nil) and keeps the root pending, so target == cur.static while
// carry == nil. The guard must not skip there — it has nothing to compare — and
// the first paired publish must still happen without hashing anything.
func TestU33D2_StaticOnlyInitialStateDoesNotArm(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000)}
	s := newSchedStatic(t, newMemLoader(map[int64][]*domain.Account{10: accs}))
	require.Nil(t, s.View().DecisionView(), "startup publishes the static root alone")
	require.NotNil(t, s.publisher.pending, "and keeps it pending for the paired compile")
	require.Same(t, s.View().StaticView(), s.publisher.pending,
		"static-only state: target == cur.static while carry is nil")
	u31d1RequireNoDigest(t, s, "static-only publish")

	wireSources(s, u31d1QualityA(accs), map[string]domain.ResolvedPrices{"m": {InputPerM: new(int64(1000))}})
	gen0 := s.View().Generation()
	s.compileOnce()
	require.Greater(t, s.View().Generation(), gen0, "the first paired publish must happen")
	require.NotNil(t, s.View().DecisionView())
	require.Nil(t, s.publisher.pending)
	u31d1RequireNoDigest(t, s, "first paired publish")
}

// u33FullOnlyCompiler hides the scoped seam, so every fire takes the exact
// full-fidelity fallback. That is the one shape with no pointer reuse to lean on,
// and therefore the one that must keep the whole-stream digest.
type u33FullOnlyCompiler struct{ inner *RoutingCompiler }

func (c *u33FullOnlyCompiler) Compile(in CompilerInputs) (*DecisionView, error) {
	return c.inner.Compile(in)
}

// TestU33D2_FullCompileFallsBackToDigestGuard keeps the fallback honest: with no
// scoped seam, an armed fire still hashes the whole view, still absorbs a
// byte-identical fire, and still publishes changed content.
func TestU33D2_FullCompileFallsBackToDigestGuard(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000)}
	s := newSchedStatic(t, newMemLoader(map[int64][]*domain.Account{10: accs}))
	s.compiler = &u33FullOnlyCompiler{inner: NewRoutingCompiler()}
	prices := map[string]domain.ResolvedPrices{"m": {InputPerM: new(int64(1000))}}
	wireSources(s, u31d1QualityA(accs), prices)
	s.compileOnce()
	u31d1RequireNoDigest(t, s, "staged full publish")

	// Same root, full fallback => the whole-stream digest is the only judgement.
	s.compileOnce()
	require.True(t, s.lastDigestSet, "the full fallback must arm the digest guard")
	require.Positive(t, s.lastDigest.count, "the full fallback must hash the decision")
	require.Equal(t, s.digester.digest(s.View().DecisionView()), s.lastDigest,
		"the armed digest must describe the published view")
	gen1 := s.View().Generation()
	dig1 := s.lastDigest

	// Byte-identical fire on the same root => absorbed.
	s.compileOnce()
	require.Equal(t, gen1, s.View().Generation(), "equal digest + same root must not publish")
	require.Equal(t, dig1, s.lastDigest)

	// Changed content => publish + digest advances.
	wireSources(s, u31d1QualityB(accs), prices)
	s.compileOnce()
	require.Greater(t, s.View().Generation(), gen1, "changed digest must publish")
	require.NotEqual(t, dig1, s.lastDigest)
	require.Equal(t, s.digester.digest(s.View().DecisionView()), s.lastDigest)
	require.True(t, s.lastDigestSet)
}
