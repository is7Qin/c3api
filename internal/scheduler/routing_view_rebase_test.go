// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestRoutingViewStaticUpdatePublishesAtomicPair(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl, 4)}})
	s := newSched(t, m)
	v1 := s.View()
	require.NotNil(t, v1)
	dec1 := v1.DecisionView()
	require.NotNil(t, dec1, "newSched 已武装编译道，决策视图非空")
	m.mu.Lock()
	m.byGroup[10] = append(m.byGroup[10], acc(2, tpl, 4))
	m.mu.Unlock()
	require.NoError(t, s.reload(nilContext()))
	// Atomic publication: the old complete pair stays visible while pending.
	v2 := s.View()
	require.Same(t, v1, v2, "staging must not touch the published pair")
	require.NotContains(t, v2.ByID(), int64(2), "new account only staged, not published")

	s.compileOnce()
	v3 := s.View()
	require.NotSame(t, v1, v3, "paired publish replaces the view")
	require.NotSame(t, dec1, v3.DecisionView(), "new static pairs with a fresh decision")
	require.NotNil(t, v3.StaticView())
	require.Contains(t, v3.ByID(), int64(2), "new static has new account")
	require.Equal(t, v3.Generation(), v3.StaticView().Generation(), "one generation for the pair")
	require.Equal(t, v3.Generation(), v3.DecisionView().Generation(), "one generation for the pair")
}

func TestRoutingViewDecisionStaleIsRejected(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl, 4)}})
	s := newSched(t, m)
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	v1 := s.View()
	baseGen := v1.Generation()
	dec1 := v1.DecisionView()
	require.NotNil(t, dec1)
	m.mu.Lock()
	m.byGroup[10] = append(m.byGroup[10], acc(2, tpl, 4))
	m.mu.Unlock()
	require.NoError(t, s.reload(nilContext()))
	v2 := s.View()
	// Atomic publication: staging leaves the published pair untouched.
	require.Same(t, v1, v2, "staging must not touch the published pair")
	require.Same(t, dec1, v2.DecisionView(), "published decision retained while pending")
	called := false
	// Publish the staged pair first so the captured base goes stale.
	s.compileOnce()
	v2b := s.View()
	require.NotSame(t, v1, v2b, "paired publish replaces the view")
	s.publisher.publishWithBase(baseGen, v1.StaticView(), func(cur *RoutingView) *DecisionView {
		called = true
		return &DecisionView{routes: map[RouteRef]*RouteDecision{route: {Primary: ccPrimary(1, 2)}}}
	})
	v3 := s.View()
	require.False(t, called, "stale decision must be discarded before build")
	require.Same(t, v2b, v3, "stale publish must preserve the latest pair")
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
	m.mu.Lock()
	m.byGroup[10] = []*domain.Account{}
	m.mu.Unlock()
	s.InvalidateGroup(10)
	// Atomic publication: the old complete pair stays visible while pending.
	v2 := s.View()
	require.NotNil(t, v2)
	require.Same(t, oldView, v2, "staging must not touch the published pair")

	s.compileOnce()
	v3 := s.View()
	require.NotSame(t, oldView, v3, "paired publish replaces the view")
	require.ElementsMatch(t, []int64{20}, v3.ByID()[1].static.Load().groupIDs, "partial invalidate preserves other groupIDs")
	require.ElementsMatch(t, []int64{10, 20}, oldLeaf.static.Load().groupIDs, "old leaf stable")
	require.Same(t, oldLeaf, oldView.ByID()[1], "old view stable")
	require.NotSame(t, oldLeaf, v3.ByID()[1], "new leaf for changed account")
	require.Same(t, oldLeaf.runtime, v3.ByID()[1].runtime, "shared runtime")
}

func nilContext() context.Context { return context.Background() }
