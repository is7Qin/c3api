// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestCacheDomainRing_compilesSharedAndPrivateDomains(t *testing.T) {
	shared := "shared.example.com"
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accounts := []*domain.Account{
		accWithEnabled(1, tpl, true, 10000),
		accWithEnabled(2, tpl, true, 10000),
		accWithEnabled(3, tpl, true, 10000),
	}
	accounts[0].CacheDomain = &shared
	accounts[1].CacheDomain = &shared

	s := newSched(t, newMemLoader(map[int64][]*domain.Account{10: accounts}))
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	rd, ok := s.View().DecisionView().Route(10, string(domain.FormatOpenAIChat), "m")
	require.True(t, ok)
	require.Len(t, rd.CacheDomainRing.Nodes, 2*CacheDomainVirtualNodes)
	require.Equal(t, []string{"private:3", shared}, rd.CacheDomainRing.Domains)
	require.Len(t, rd.CacheDomainAccounts, 3)
	require.Equal(t, route, RouteRefFor(10, string(domain.FormatOpenAIChat), "m"))
}

func TestCacheDomainRing_hashStableAcrossGenerations(t *testing.T) {
	ring1, err := buildCacheDomainRing([]string{"alpha.example.com", "beta.example.com"})
	require.NoError(t, err)
	ring2, err := buildCacheDomainRing([]string{"beta.example.com", "alpha.example.com"})
	require.NoError(t, err)
	require.Equal(t, ring1, ring2)

	keyHash := CacheAffinityHash("opaque-request-key")
	domain1, ok := ring1.Lookup(keyHash)
	require.True(t, ok)
	domain2, ok := ring2.Lookup(keyHash)
	require.True(t, ok)
	require.Equal(t, domain1, domain2)
}

func TestCacheDomainRing_addRemoveMovesOnlyRingOwnership(t *testing.T) {
	base, err := buildCacheDomainRing([]string{"alpha.example.com", "beta.example.com"})
	require.NoError(t, err)
	withGamma, err := buildCacheDomainRing([]string{"alpha.example.com", "beta.example.com", "gamma.example.com"})
	require.NoError(t, err)

	var moved uint64
	for _, node := range withGamma.Nodes {
		baseDomain, baseOK := base.Lookup(node.Hash)
		withGammaDomain, withGammaOK := withGamma.Lookup(node.Hash)
		if baseOK && withGammaOK && baseDomain != withGammaDomain {
			moved = node.Hash
			break
		}
	}
	require.NotZero(t, moved, "adding a domain must move at least one ring interval")

	withoutGamma, err := buildCacheDomainRing([]string{"alpha.example.com", "beta.example.com"})
	require.NoError(t, err)
	baseDomain, baseOK := base.Lookup(moved)
	withoutGammaDomain, withoutGammaOK := withoutGamma.Lookup(moved)
	require.True(t, baseOK)
	require.True(t, withoutGammaOK)
	require.Equal(t, baseDomain, withoutGammaDomain)
}

func TestCacheDomainRing_rejectsGlobalNodeCap(t *testing.T) {
	domains := make([]string, MaxCacheDomainRingNodes/CacheDomainVirtualNodes+1)
	for i := range domains {
		domains[i] = "domain-" + strconv.Itoa(i) + ".example.com"
	}
	_, err := buildCacheDomainRing(domains)
	require.ErrorIs(t, err, ErrCacheDomainRingCapExceeded)
}

func TestAttemptPlan_explicitAffinityPrefersDomainAndSpillsWithoutDuplicates(t *testing.T) {
	sharedA := "a.example.com"
	sharedB := "b.example.com"
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	a1 := accWithEnabled(1, tpl, true, 1)
	a2 := accWithEnabled(2, tpl, true, 1)
	a3 := accWithEnabled(3, tpl, true, 1)
	a1.CacheDomain = &sharedA
	a2.CacheDomain = &sharedA
	a3.CacheDomain = &sharedB
	s := newTestScheduler(t, []*domain.Account{a1, a2, a3})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: []int64{1, 2, 3}, CacheDomainRing: mustCacheDomainRing(t, []string{sharedA, sharedB}), CacheDomainAccounts: []CacheDomainAccount{{AccountID: 1, Domain: sharedA}, {AccountID: 2, Domain: sharedA}, {AccountID: 3, Domain: sharedB}}})

	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-affinity", MaxAttempts: 3}, route)
	require.NoError(t, err)
	require.True(t, plan.ApplyCacheAffinity(CacheAffinityHashForDomain(t, plan.decision.CacheDomainRing, sharedB)))

	first, attempt, err := s.ReserveAttempt(plan)
	require.NoError(t, err)
	require.Equal(t, int64(3), attempt.AccountID)
	first.Release()

	// Given the preferred account is saturated, the complete global order is
	// still available as spill candidates and each account is consulted once.
	s.View().ByID()[3].runtime.concurrency.Store(1)
	second, attempt, err := s.ReserveAttempt(plan)
	require.NoError(t, err)
	require.Contains(t, []int64{1, 2}, attempt.AccountID)
	require.NotEqual(t, int64(3), attempt.AccountID)
	second.Release()
}

func TestAttemptPlan_noAffinityPreservesCompiledOrder(t *testing.T) {
	plan := NewAttemptPlan(AttemptPlanIdentity{}, RouteDecision{Primary: []int64{3, 1, 2}})
	var got []int64
	_, err := plan.Reserve(func(id int64) bool {
		got = append(got, id)
		return true
	})
	require.NoError(t, err)
	require.Equal(t, []int64{3}, got)
}

func mustCacheDomainRing(t *testing.T, domains []string) CacheDomainRing {
	t.Helper()
	ring, err := buildCacheDomainRing(domains)
	require.NoError(t, err)
	return ring
}

func CacheAffinityHashForDomain(t *testing.T, ring CacheDomainRing, domain string) uint64 {
	t.Helper()
	for _, node := range ring.Nodes {
		if node.Domain == domain {
			return node.Hash
		}
	}
	t.Fatalf("domain %q has no ring node", domain)
	return 0
}
