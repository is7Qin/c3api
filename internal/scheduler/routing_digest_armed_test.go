// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

// U31 D1 acceptance tests: the decision digest is computed ONLY when the
// publish-skip guard could actually match (same static root). These are
// state/count assertions — no wall clock — so they stay deterministic on any
// machine; the wall-clock counterpart lives in the U27 end-to-end benchmark.
//
// Witness chosen here: a *not computed* digest is the zero decisionDigest
// (sum all-zero, count 0, isNil false). On the U27 fixture a computed digest
// carries count = 1,895,403,513 (1.9 GB of canonical bytes), so "count == 0"
// is a direct proof that the canonical encoding was never streamed.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// u31d1Sched builds the two-account single-group fixture used by the B2 guard
// tests. Sources are wired BEFORE the first compile, so the first published
// decision already reflects quality A and a later same-root fire over the same
// inputs compiles byte-identical content.
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

// TestU31D1_StagedFireComputesNoDigest is acceptance §4.1.1: every staged fire
// (reload and InvalidateGroup both stage a brand-new root) publishes without
// hashing the decision, because the skip guard is structurally unreachable.
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

// TestU31D1_SameRootDriftRearmsThenSkips is acceptance §4.1.2: after a staged
// publish (guard unloaded) the first same-root fire hashes the decision and
// re-arms; the next same-root fire with identical bytes skips the publish.
func TestU31D1_SameRootDriftRearmsThenSkips(t *testing.T) {
	s, accs, prices := u31d1Sched(t)
	u31d1RequireNoDigest(t, s, "staged publish")
	gen0 := s.View().Generation()

	// Same root, changed content: armed -> digest computed -> publish -> armed.
	wireSources(s, u31d1QualityB(accs), prices)
	s.enqueueCompileScope([]int64{10}, nil, scopeCauseGroup)
	s.compileOnce()
	require.True(t, s.lastDigestSet, "a same-root publish must arm the guard")
	require.Positive(t, s.lastDigest.count, "an armed fire must hash the decision")
	require.Equal(t, s.digester.digest(s.View().DecisionView()), s.lastDigest,
		"the armed digest must describe the published view")
	gen1 := s.View().Generation()
	require.Greater(t, gen1, gen0, "changed content must publish")
	dig1 := s.lastDigest

	// Same root, identical content, forced scope: armed -> equal -> skip.
	s.enqueueCompileScope([]int64{10}, nil, scopeCauseGroup)
	s.compileOnce()
	require.Equal(t, gen1, s.View().Generation(), "equal digest + same root must not publish")
	require.Equal(t, dig1, s.lastDigest)
	require.True(t, s.lastDigestSet)
}

// TestU31D1_StagedPublishCostsOneIdenticalPublish pins the accepted semantic
// difference of D1: a staged publish cannot hash the view it publishes, so the
// NEXT same-root fire re-arms by publishing once even when the bytes are
// identical (generation +1). Pre-D1 that fire skipped the publish.
func TestU31D1_StagedPublishCostsOneIdenticalPublish(t *testing.T) {
	s, _, _ := u31d1Sched(t)
	u31d1RequireNoDigest(t, s, "staged publish")
	gen0 := s.View().Generation()

	// No source change, no staging: the compiled bytes are identical.
	s.enqueueCompileScope([]int64{10}, nil, scopeCauseGroup)
	s.compileOnce()
	gen1 := s.View().Generation()
	require.Greater(t, gen1, gen0,
		"D1 cost: the re-arming fire publishes byte-identical content once")
	require.True(t, s.lastDigestSet, "that publish re-arms the guard")

	// From here the guard is in charge again: the same content no longer publishes.
	s.enqueueCompileScope([]int64{10}, nil, scopeCauseGroup)
	s.compileOnce()
	require.Equal(t, gen1, s.View().Generation(),
		"the guard must absorb every later byte-identical fire")
}

// TestU31D1_StaleDigestNeverHidesAChangedView is the reason the staged publish
// must UNLOAD the guard instead of keeping the previous digest: after A -> B
// (staged) a fire that reverts to A must publish, never be mistaken for the
// already-published B.
func TestU31D1_StaleDigestNeverHidesAChangedView(t *testing.T) {
	s, accs, prices := u31d1Sched(t)

	// Arm on content A.
	s.enqueueCompileScope([]int64{10}, nil, scopeCauseGroup)
	s.compileOnce()
	require.True(t, s.lastDigestSet)
	digA := s.lastDigest
	require.Positive(t, digA.count)

	// Staged fire publishes B (quality flip stages nothing, so stage explicitly
	// with a reload to keep the staged shape).
	wireSources(s, u31d1QualityB(accs), prices)
	require.NoError(t, s.reload(context.Background()))
	s.compileOnce()
	u31d1RequireNoDigest(t, s, "staged publish of B")
	genB := s.View().Generation()
	require.NotEqual(t, digA, s.lastDigest)

	// Revert the content to A without staging: the compiled bytes equal A's.
	wireSources(s, u31d1QualityA(accs), prices)
	s.enqueueCompileScope([]int64{10}, nil, scopeCauseGroup)
	s.compileOnce()
	require.Greater(t, s.View().Generation(), genB,
		"a stale digest must never hide a changed view")
	require.Equal(t, digA, s.lastDigest, "the re-published view has A's digest again")
	require.True(t, s.lastDigestSet)
}
