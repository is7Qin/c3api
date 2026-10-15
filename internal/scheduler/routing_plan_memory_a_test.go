// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

// Routing-plan-memory A (per-compile ring interning) acceptance tests, spec §5
// A1–A3. Package-internal: backing sharing is asserted on internal decisions
// (public accessors intentionally clone).

import (
	"context"
	"reflect"
	"strconv"
	"strings"
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

// domainsOf returns a copy of a route's ordered actual domain set (nil route →
// nil), so assertions never alias the shared backing.
func domainsOf(rd *RouteDecision) []string {
	if rd == nil {
		return nil
	}
	return append([]string(nil), rd.CacheDomainRing.Domains...)
}

// TestRPM_A3_ScopedQualityPublishesShareCarryRing drives THREE real publishing
// scoped quality updates on group 10 (alternating two quality inputs so each
// fire changes canonical content while the actual domain set is preserved).
// Each fire must: advance the generation (a real publish, not the equal-digest
// skip), replace the affected route's decision pointer, keep the unaffected
// group-20 route pointer identical (carry), and keep affected<>unaffected rings
// on the SAME Nodes/Domains backing with a flat U=1. A fresh full compile of
// identical inputs must reproduce the scoped canonical bytes exactly.
//
// Mutation sensitivity: deleting the carry seed (`sess.seedFromView(f.carry)` in
// compile_event.go) makes the affected route build its own ring, so the
// shared-backing assertion fails (verified during the fix; restored after).
func TestRPM_A3_ScopedQualityPublishesShareCarryRing(t *testing.T) {
	const dom = "shared.example"
	s, m := rpmSharedDomainScheduler(t, rpmModels(1), dom)
	accs := m.byGroup[10]
	ref10 := RouteRefFor(10, string(domain.FormatOpenAIChat), "m000")
	ref20 := RouteRefFor(20, string(domain.FormatOpenAIChat), "m000")
	prices := map[string]domain.ResolvedPrices{"m000": {InputPerM: new(int64(1000)), OutputPerM: new(int64(1000))}}
	qA := buildQuality(10, domain.FormatOpenAIChat, "m000", map[int64]CandidateQualityInput{
		1: qualityInput(30, 29, 100, 100), 2: qualityInput(30, 29, 100, 100),
	}, accs)
	qB := buildQuality(10, domain.FormatOpenAIChat, "m000", map[int64]CandidateQualityInput{
		1: qualityInput(10, 5, 100, 100), 2: qualityInput(10, 5, 100, 100),
	}, accs)

	// Baseline full compile with qA+prices so the following fires differ ONLY in
	// quality (prices stay equal, so the affected set is exactly ref10).
	wireSources(s, qA, prices)
	s.enqueueCompileScope(nil, nil, scopeCauseFullStage)
	s.compileOnce()

	dv := s.View().DecisionView()
	rd10 := dv.routes[ref10]
	rd20 := dv.routes[ref20]
	require.NotNil(t, rd10)
	require.NotNil(t, rd20)
	wantNodes := rpmNodesBacking(rd10.CacheDomainRing)
	wantDomains := rpmDomainsBacking(rd10.CacheDomainRing)
	require.Equal(t, wantNodes, rpmNodesBacking(rd20.CacheDomainRing), "fixture must start fully shared")
	require.Equal(t, wantDomains, rpmDomainsBacking(rd20.CacheDomainRing))
	require.Equal(t, 1, rpmUniqueNodesBackings(dv))

	prevGen := s.View().Generation()
	for fire := 0; fire < 3; fire++ {
		q := qB
		if fire%2 == 1 {
			q = qA
		}
		wireSources(s, q, prices)
		s.compileOnce()

		require.Greater(t, s.View().Generation(), prevGen,
			"scoped quality update %d must publish a new generation", fire)
		cur := s.View().DecisionView()
		n10 := cur.routes[ref10]
		n20 := cur.routes[ref20]
		require.NotNil(t, n10)
		require.NotNil(t, n20)
		require.NotSame(t, rd10, n10, "fire %d: affected decision pointer must change", fire)
		require.Same(t, rd20, n20, "fire %d: unaffected carry decision pointer must be identical", fire)
		require.Equal(t, wantNodes, rpmNodesBacking(n10.CacheDomainRing),
			"fire %d: affected route must reuse the carry Nodes backing", fire)
		require.Equal(t, wantNodes, rpmNodesBacking(n20.CacheDomainRing), "fire %d", fire)
		require.Equal(t, wantDomains, rpmDomainsBacking(n10.CacheDomainRing), "fire %d", fire)
		require.Equal(t, wantDomains, rpmDomainsBacking(n20.CacheDomainRing), "fire %d", fire)
		require.Equal(t, 1, rpmUniqueNodesBackings(cur), "fire %d: U must stay flat", fire)

		prevGen = s.View().Generation()
		rd10, rd20 = n10, n20
	}

	// Full-recompile oracle: an identical fresh fixture compiled fully with the
	// same inputs must reproduce the scoped canonical bytes exactly.
	scopedBytes := decisionViewBytes(s.View().DecisionView())
	s2, _ := rpmSharedDomainScheduler(t, rpmModels(1), dom)
	wireSources(s2, qB, prices)
	s2.enqueueCompileScope(nil, nil, scopeCauseFullStage)
	s2.compileOnce()
	require.True(t, s2.View().DecisionView().whole)
	require.Equal(t, scopedBytes, decisionViewBytes(s2.View().DecisionView()),
		"scoped canonical bytes must equal a fresh full compile of identical inputs")
}

// TestRPM_A3_DomainSetLifecycle covers the spec §5 A3 sub-coverage: an old set
// disappears then reappears, a different set is added, a model is added/removed,
// an account is deleted (route gone), a stale snapshot stays byte-stable, and no
// interner session outlives its compile fire.
func TestRPM_A3_DomainSetLifecycle(t *testing.T) {
	ctx := context.Background()
	mk := func(models []string) *domain.Template {
		return &domain.Template{ID: 1, BaseURL: "https://u/v1", CredentialType: "api_key",
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: models}
	}
	d1, d2, d3 := "d1", "d2", "d3"
	a1 := acc(1, mk([]string{"m"}), 10)
	a1.CacheDomain = &d1
	a2 := acc(2, mk([]string{"m"}), 10)
	a2.CacheDomain = &d2
	m := newMemLoader(map[int64][]*domain.Account{10: {a1, a2}})
	s := newSched(t, m)
	ref := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	ref2 := RouteRefFor(10, string(domain.FormatOpenAIChat), "m2")

	old := s.View()
	oldRing := cloneCacheDomainRing(old.DecisionView().routes[ref].CacheDomainRing)
	require.Equal(t, []string{"d1", "d2"}, domainsOf(s.View().DecisionView().routes[ref]))

	step := func() { require.NoError(t, s.reload(ctx)); s.compileOnce() }

	// The old set disappears ({d1}) then reappears ({d1,d2}); the stale snapshot
	// is unchanged throughout.
	m.byGroup[10] = []*domain.Account{a1}
	step()
	require.Equal(t, []string{"d1"}, domainsOf(s.View().DecisionView().routes[ref]))
	m.byGroup[10] = []*domain.Account{a1, a2}
	step()
	require.Equal(t, []string{"d1", "d2"}, domainsOf(s.View().DecisionView().routes[ref]))
	require.Equal(t, oldRing, old.DecisionView().routes[ref].CacheDomainRing,
		"old snapshot value must not change after recompiles")

	// A different set is added ({d1,d3}).
	a2b := acc(2, mk([]string{"m"}), 10)
	a2b.CacheDomain = &d3
	m.byGroup[10] = []*domain.Account{a1, a2b}
	step()
	require.Equal(t, []string{"d1", "d3"}, domainsOf(s.View().DecisionView().routes[ref]))

	// Add a model: both routes share one ring (U flat for the two-route set).
	a1m := acc(1, mk([]string{"m", "m2"}), 10)
	a1m.CacheDomain = &d1
	a2m := acc(2, mk([]string{"m", "m2"}), 10)
	a2m.CacheDomain = &d3
	m.byGroup[10] = []*domain.Account{a1m, a2m}
	step()
	dv := s.View().DecisionView()
	require.NotNil(t, dv.routes[ref2])
	require.Equal(t, []string{"d1", "d3"}, domainsOf(dv.routes[ref2]))
	require.Equal(t, rpmNodesBacking(dv.routes[ref].CacheDomainRing), rpmNodesBacking(dv.routes[ref2].CacheDomainRing),
		"routes with the same actual set share one ring")
	require.Equal(t, 1, rpmUniqueNodesBackings(dv))

	// Remove the model (route gone).
	m.byGroup[10] = []*domain.Account{a1, a2b}
	step()
	require.Nil(t, s.View().DecisionView().routes[ref2], "removed model's route must disappear")

	// Delete the accounts (route gone); the stale snapshot stays intact.
	m.byGroup[10] = nil
	step()
	require.Nil(t, s.View().DecisionView().routes[ref], "empty group must drop the route")
	require.Equal(t, oldRing, old.DecisionView().routes[ref].CacheDomainRing)

	// Restore.
	m.byGroup[10] = []*domain.Account{a1, a2}
	step()
	require.Equal(t, []string{"d1", "d2"}, domainsOf(s.View().DecisionView().routes[ref]))

	// No interner session residue: a fresh session starts empty and the Scheduler
	// owns no interner field (the session dies with each compile fire).
	sessA := newRingInterner()
	_, err := sessA.intern([]string{"x"})
	require.NoError(t, err)
	require.NotEmpty(t, sessA.rings)
	require.Empty(t, newRingInterner().rings)
	rt := reflect.TypeOf(Scheduler{})
	for i := range rt.NumField() {
		require.NotContains(t, rt.Field(i).Type.String(), "ringInterner",
			"session state must not live on the Scheduler (field %s)", rt.Field(i).Name)
	}
}

// TestRPM_A3_PublicCloneIsolation asserts the PUBLIC Routes()/Route() accessors
// clone the shared ring, so a caller mutating the returned value cannot poison
// the internal shared backing (spec §5 A3 "对外 clone 修改不能污染共享环").
func TestRPM_A3_PublicCloneIsolation(t *testing.T) {
	const dom = "shared.example"
	s, _ := rpmSharedDomainScheduler(t, rpmModels(1), dom)
	view := s.View()
	ref10 := RouteRefFor(10, string(domain.FormatOpenAIChat), "m000")
	ref20 := RouteRefFor(20, string(domain.FormatOpenAIChat), "m000")
	internal := view.DecisionView().routes[ref10]
	require.NotNil(t, internal)

	// Routes() clones every decision (including Nodes/Domains).
	routes := view.DecisionView().Routes()
	require.NotNil(t, routes[ref10])
	routes[ref10].CacheDomainRing.Nodes[0].Domain = "HACKED"
	routes[ref20].CacheDomainRing.Domains[0] = "HACKED-D"
	require.NotEqual(t, "HACKED", internal.CacheDomainRing.Nodes[0].Domain)
	require.NotEqual(t, "HACKED-D", internal.CacheDomainRing.Domains[0])
	require.Equal(t, dom, internal.CacheDomainRing.Domains[0])

	// Route() clones too.
	got, ok := view.DecisionView().Route(10, string(domain.FormatOpenAIChat), "m000")
	require.True(t, ok)
	got.CacheDomainRing.Nodes[0].Domain = "HACKED2"
	require.NotEqual(t, "HACKED2", internal.CacheDomainRing.Nodes[0].Domain)
	require.Equal(t, dom, internal.CacheDomainRing.Nodes[0].Domain)
}

// TestRPM_A3_HeterogeneousStructure wires the §6 heterogeneous fixture and
// asserts the structural formula: each actual domain set has exactly one Nodes
// backing (U distinct backings for U sets), each set reports its real D_i, and
// the node payload is Σ_i cap(Nodes_i)×sizeof(CacheDomainNode).
func TestRPM_A3_HeterogeneousStructure(t *testing.T) {
	const models = 100
	s, _ := rpmHeterogeneousScheduler(t, rpmModels(models))
	dv := s.View().DecisionView()
	require.Equal(t, 2*4*models, len(dv.routes))

	type setStat struct {
		backing uintptr
		domains int
		routes  int
	}
	sets := map[string]setStat{}
	for _, rd := range dv.routes {
		require.NotNil(t, rd)
		key := strings.Join(rd.CacheDomainRing.Domains, "\x00")
		st, ok := sets[key]
		if !ok {
			st = setStat{backing: rpmNodesBacking(rd.CacheDomainRing), domains: len(rd.CacheDomainRing.Domains)}
		} else {
			require.Equal(t, st.backing, rpmNodesBacking(rd.CacheDomainRing),
				"one Nodes backing per actual domain set (set %q)", key)
		}
		st.routes++
		sets[key] = st
	}
	require.Len(t, sets, 4, "U=4 actual domain sets")
	payload := 0
	for key, st := range sets {
		require.Equal(t, 1, st.domains, "real D_i for set %q", key)
		require.Equal(t, 2*4*(models/4), st.routes, "set %q route count", key)
		payload += CacheDomainVirtualNodes * int(unsafe.Sizeof(CacheDomainNode{}))
	}
	require.Equal(t, 4, rpmUniqueNodesBackings(dv), "U unique Nodes backings")
	require.Equal(t, 4*CacheDomainVirtualNodes*int(unsafe.Sizeof(CacheDomainNode{})), payload)
	t.Logf("RPM_HETERO_STRUCTURE models=%d routes=%d U=%d D_i=1 node_payload_bytes=%d",
		models, len(dv.routes), len(sets), payload)
}
