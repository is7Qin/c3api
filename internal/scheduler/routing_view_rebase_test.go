// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestRoutingViewStaticUpdateRetainsLatestDecision(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl, 4)}})
	s := newSched(t, m)
	v1 := s.View()
	require.NotNil(t, v1)
	dec1 := v1.DecisionView()
	// Decision may be nil initially; ensure we create one via publishWithBase
	if dec1 == nil {
		s.publisher.publishWithBase(v1.Generation(), func(cur *RoutingView) *DecisionView {
			return &DecisionView{generation: 1, decisions: map[int64]*decisionLeaf{1: {weight: 10}}}
		})
		v1 = s.View()
		dec1 = v1.DecisionView()
	}
	gen1 := v1.Generation()
	// Static reload: add account 2, should retain dec1
	m.mu.Lock()
	m.byGroup[10] = append(m.byGroup[10], acc(2, tpl, 4))
	m.mu.Unlock()
	require.NoError(t, s.reload(nilContext()))
	v2 := s.View()
	require.NotNil(t, v2)
	require.Greater(t, v2.Generation(), gen1)
	require.Same(t, dec1, v2.DecisionView(), "static update retains latest DecisionView")
	require.NotNil(t, v2.StaticView())
	require.Contains(t, v2.ByID(), int64(2), "new static has new account")
}

func TestRoutingViewDecisionStaleRebasesOntoLatestStatic(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl, 4)}})
	s := newSched(t, m)
	v1 := s.View()
	baseGen := v1.Generation()
	dec1 := v1.DecisionView()
	if dec1 == nil {
		s.publisher.publishWithBase(baseGen, func(cur *RoutingView) *DecisionView {
			return &DecisionView{generation: 1, decisions: map[int64]*decisionLeaf{1: {weight: 10}}}
		})
		v1 = s.View()
		baseGen = v1.Generation()
		dec1 = v1.DecisionView()
	}
	// Simulate concurrent static update: add account 2
	m.mu.Lock()
	m.byGroup[10] = append(m.byGroup[10], acc(2, tpl, 4))
	m.mu.Unlock()
	require.NoError(t, s.reload(nilContext()))
	v2 := s.View()
	require.NotSame(t, v1.StaticView(), v2.StaticView(), "static view updated")
	require.Same(t, dec1, v2.DecisionView(), "static retains decision")

	// Decision submit with stale base (baseGen from v1) should rebase onto latest static (v2) and replace only decision
	staleBase := baseGen
	s.publisher.publishWithBase(staleBase, func(cur *RoutingView) *DecisionView {
		// cur should be latest (v2), not stale
		require.Same(t, v2.StaticView(), cur.StaticView(), "rebase onto latest StaticView")
		return &DecisionView{generation: cur.DecisionView().Generation() + 1, decisions: map[int64]*decisionLeaf{1: {weight: 20}}}
	})
	v3 := s.View()
	require.Same(t, v2.StaticView(), v3.StaticView(), "static preserved, only decision replaced")
	require.NotSame(t, dec1, v3.DecisionView(), "decision replaced")
	require.Equal(t, 20, v3.DecisionView().decisions[1].weight)
}

func TestRoutingViewPartialInvalidatePreservesOtherGroupIDs(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	a := acc(1, tpl, 4)
	m := newMemLoader(map[int64][]*domain.Account{10: {a}, 20: {a}})
	s := newSched(t, m)
	v1 := s.View()
	require.ElementsMatch(t, []int64{10, 20}, v1.ByID()[1].static.Load().groupIDs)
	oldLeaf := v1.ByID()[1]
	oldView := v1
	// Partial InvalidateGroup 10: remove account from group 10 but keep in 20
	// Simulate DB after removal: group 10 empty, group 20 still has a
	m.mu.Lock()
	m.byGroup[10] = []*domain.Account{}
	m.mu.Unlock()
	s.InvalidateGroup(10)
	v2 := s.View()
	require.NotNil(t, v2)
	// Other groupID 20 must be preserved from old immutable root
	require.ElementsMatch(t, []int64{20}, v2.ByID()[1].static.Load().groupIDs, "partial invalidate preserves other groupIDs")
	// Old leaf stable
	require.ElementsMatch(t, []int64{10, 20}, oldLeaf.static.Load().groupIDs, "old leaf stable")
	require.Same(t, oldLeaf, oldView.ByID()[1], "old view stable")
	require.NotSame(t, oldLeaf, v2.ByID()[1], "new leaf for changed account")
	require.Same(t, oldLeaf.runtime, v2.ByID()[1].runtime, "shared runtime")
}

func nilContext() context.Context {
	// Use background; scheduler reload needs context, nil is not allowed, use Background
	return context.Background()
}
