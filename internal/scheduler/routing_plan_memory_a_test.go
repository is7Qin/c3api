// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

// Routing-plan-memory A (per-compile ring interning) acceptance tests, spec §5
// A1–A3. Package-internal: backing sharing is asserted on internal decisions
// (public accessors intentionally clone).

import (
	"strconv"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func rpmNodesBacking(r CacheDomainRing) uintptr {
	if len(r.Nodes) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(unsafe.SliceData(r.Nodes)))
}

func rpmDomainsBacking(r CacheDomainRing) uintptr {
	if len(r.Domains) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(unsafe.SliceData(r.Domains)))
}

func rpmUniqueNodesBackings(dv *DecisionView) int {
	seen := map[uintptr]struct{}{}
	for _, rd := range dv.routes {
		if rd == nil || len(rd.CacheDomainRing.Nodes) == 0 {
			continue
		}
		seen[rpmNodesBacking(rd.CacheDomainRing)] = struct{}{}
	}
	return len(seen)
}

// --- A1 ---

func TestRPM_A1_ShareAcrossModelFormatGroup(t *testing.T) {
	s, _ := rpmHomogeneousScheduler(t, rpmModels(2))
	dv := s.View().DecisionView()
	mk := func(gid int64, format domain.RequestFormat, model string) RouteRef {
		return RouteRefFor(gid, string(format), model)
	}
	refs := []RouteRef{
		mk(10, domain.FormatOpenAIChat, "m000"),
		mk(10, domain.FormatOpenAIResponses, "m000"),
		mk(10, domain.FormatAnthropic, "m000"),
		mk(10, domain.FormatOpenAIResponsesWS, "m000"),
		mk(20, domain.FormatOpenAIChat, "m000"),
		mk(10, domain.FormatOpenAIChat, "m001"),
	}
	var wantNodes, wantDomains uintptr
	for i, ref := range refs {
		rd := dv.routes[ref]
		require.NotNil(t, rd, "route %v", ref)
		require.NotEmpty(t, rd.CacheDomainRing.Nodes)
		if i == 0 {
			wantNodes = rpmNodesBacking(rd.CacheDomainRing)
			wantDomains = rpmDomainsBacking(rd.CacheDomainRing)
		}
		require.Equal(t, wantNodes, rpmNodesBacking(rd.CacheDomainRing),
			"same actual domain set must share Nodes backing across model/format/group: %v", ref)
		require.Equal(t, wantDomains, rpmDomainsBacking(rd.CacheDomainRing),
			"same actual domain set must share Domains backing: %v", ref)
	}
	require.Equal(t, 1, rpmUniqueNodesBackings(dv), "homogeneous fixture must be U=1")

	// Value equals a fresh builder for the same actual domain set.
	domains := make([]string, 0, rpmAccounts)
	for id := int64(1); id <= rpmAccounts; id++ {
		domains = append(domains, privateCacheDomain(id))
	}
	want, err := buildCacheDomainRing(domains)
	require.NoError(t, err)
	require.Equal(t, want, dv.routes[refs[0]].CacheDomainRing)
}

func TestRPM_A1_DifferentSetsNotShared_SameDomainDifferentAccounts(t *testing.T) {
	shared := "shared.example"
	tplA := &domain.Template{ID: 1, BaseURL: "https://u/v1", CredentialType: "api_key", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"a"}}
	tplB := &domain.Template{ID: 2, BaseURL: "https://u/v1", CredentialType: "api_key", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"b"}}
	tplC := &domain.Template{ID: 3, BaseURL: "https://u/v1", CredentialType: "api_key", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"c"}}
	a1 := acc(1, tplA, 4)
	a1.CacheDomain = &shared
	a2 := acc(2, tplB, 4)
	a2.CacheDomain = &shared
	a3 := acc(3, tplC, 4) // no explicit domain -> private:3
	s := newSched(t, newMemLoader(map[int64][]*domain.Account{10: {a1, a2, a3}}))
	dv := s.View().DecisionView()

	rA := dv.routes[RouteRefFor(10, string(domain.FormatOpenAIChat), "a")]
	rB := dv.routes[RouteRefFor(10, string(domain.FormatOpenAIChat), "b")]
	rC := dv.routes[RouteRefFor(10, string(domain.FormatOpenAIChat), "c")]
	require.NotNil(t, rA)
	require.NotNil(t, rB)
	require.NotNil(t, rC)

	// Same explicit domain set {"shared.example"} across different account IDs:
	// the ring may be shared...
	require.Equal(t, rpmNodesBacking(rA.CacheDomainRing), rpmNodesBacking(rB.CacheDomainRing))
	require.Equal(t, []string{"shared.example"}, rA.CacheDomainRing.Domains)
	// ...but account mappings must NOT be conflated.
	require.Equal(t, []CacheDomainAccount{{AccountID: 1, Domain: "shared.example"}}, rA.CacheDomainAccounts)
	require.Equal(t, []CacheDomainAccount{{AccountID: 2, Domain: "shared.example"}}, rB.CacheDomainAccounts)
	// A different actual set must not share.
	require.NotEqual(t, rpmNodesBacking(rA.CacheDomainRing), rpmNodesBacking(rC.CacheDomainRing))
	require.Equal(t, []string{"private:3"}, rC.CacheDomainRing.Domains)
}

// --- A2 ---

func TestRPM_A2_InternerKeyExactness(t *testing.T) {
	k1 := cacheDomainSetKey(normalizeCacheDomains([]string{"a", "bc"}))
	k2 := cacheDomainSetKey(normalizeCacheDomains([]string{"ab", "c"}))
	require.NotEqual(t, k1, k2, "length-prefixed key must not be delimiter-ambiguous")
	require.Equal(t, "", cacheDomainSetKey(nil))
	require.Equal(t, k1, cacheDomainSetKey(normalizeCacheDomains([]string{"bc", "a", "a", ""})))

	sess := newRingInterner()
	r1, err := sess.intern([]string{"a", "bc"})
	require.NoError(t, err)
	r2, err := sess.intern([]string{"ab", "c"})
	require.NoError(t, err)
	require.NotEqual(t, rpmNodesBacking(r1), rpmNodesBacking(r2))
	r1b, err := sess.intern([]string{"bc", "a", "a", ""})
	require.NoError(t, err)
	require.Equal(t, rpmNodesBacking(r1), rpmNodesBacking(r1b), "dedup/sort/reorder must hit the same interned ring")
}

func TestRPM_A2_LookupMatchesFreshBuilder(t *testing.T) {
	domains := []string{"z.example", "a.example", "a.example", "", "m.example"}
	fresh, err := buildCacheDomainRing(domains)
	require.NoError(t, err)
	sess := newRingInterner()
	interned, err := sess.intern(domains)
	require.NoError(t, err)
	require.Equal(t, fresh, interned)
	require.Equal(t, fresh.Domains, interned.Domains, "private/duplicate/empty domains normalize identically")
	for _, h := range []uint64{0, 1, 7, 42, 1 << 40} {
		fd, fok := fresh.Lookup(h)
		id, iok := interned.Lookup(h)
		require.Equal(t, fok, iok)
		require.Equal(t, fd, id)
	}
}

func TestRPM_A2_CapErrorPropagatesAndNotRegistered(t *testing.T) {
	over := MaxCacheDomainRingNodes/CacheDomainVirtualNodes + 1
	domains := make([]string, 0, over)
	for i := range over {
		domains = append(domains, "d"+strconv.Itoa(i))
	}
	sess := newRingInterner()
	_, err := sess.intern(domains)
	require.ErrorIs(t, err, ErrCacheDomainRingCapExceeded)
	require.Empty(t, sess.rings, "a cap failure must not register a ring")
}

// --- A3 ---

func rpmSharedDomainScheduler(tb testing.TB, models []string, dom string) (*Scheduler, *memLoader) {
	tb.Helper()
	tpl := rpmTemplate(1, models)
	byGroup := map[int64][]*domain.Account{}
	for gid, base := range map[int64]int64{10: 0, 20: 1000} {
		accs := make([]*domain.Account, 0, rpmAccounts)
		for i := range rpmAccounts {
			a := acc(base+int64(i)+1, tpl, 100000)
			d := dom
			a.CacheDomain = &d
			accs = append(accs, a)
		}
		byGroup[gid] = accs
	}
	m := newMemLoader(byGroup)
	s := rpmLoadScheduler(tb, m)
	return s, m
}

func TestRPM_A3_ScopedSharesCarryAndStableU(t *testing.T) {
	const dom = "shared.example"
	s, _ := rpmSharedDomainScheduler(t, rpmModels(1), dom)
	ref10 := RouteRefFor(10, string(domain.FormatOpenAIChat), "m000")
	ref20 := RouteRefFor(20, string(domain.FormatOpenAIChat), "m000")
	oldView := s.View()

	for fire := 0; fire < 3; fire++ {
		s.enqueueCompileScope([]int64{10}, nil, scopeCauseGroup)
		s.compileOnce()
		dv := s.View().DecisionView()
		rd10 := dv.routes[ref10]
		rd20 := dv.routes[ref20]
		require.NotNil(t, rd10)
		require.NotNil(t, rd20)
		require.Equal(t, rpmNodesBacking(rd10.CacheDomainRing), rpmNodesBacking(rd20.CacheDomainRing),
			"affected route must reuse the carry ring backing (fire %d)", fire)
		require.Equal(t, rpmDomainsBacking(rd10.CacheDomainRing), rpmDomainsBacking(rd20.CacheDomainRing))
		require.Equal(t, 1, rpmUniqueNodesBackings(dv), "U must not grow across fires (fire %d)", fire)
	}

	// Old snapshot kept alive and unchanged.
	require.NotNil(t, oldView.DecisionView().routes[ref10])
	require.Equal(t, []string{dom}, oldView.DecisionView().routes[ref10].CacheDomainRing.Domains)

	// External clone mutation must not poison the shared internal ring.
	internal := s.View().DecisionView().routes[ref10]
	clone := cloneRouteDecision(internal)
	clone.CacheDomainRing.Nodes[0].Domain = "HACKED"
	require.NotEqual(t, "HACKED", internal.CacheDomainRing.Nodes[0].Domain)
	require.NotEqual(t, "HACKED", s.View().DecisionView().routes[ref20].CacheDomainRing.Nodes[0].Domain)
}
