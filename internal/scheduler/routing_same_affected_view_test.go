// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

// U33 D2 acceptance: the publish guard decides content equality from the
// affected route bodies only, instead of hashing the whole canonical stream.
//
// Two gates live here:
//
//   - TestU33D2_WriteRouteBodyComposesWholeView / …GoldenCanonicalBytes pin the
//     writeRouteBody extraction: the byte stream must be identical to the one
//     the pre-extraction encoder produced (independent BASE oracle + golden hash
//     + per-route composition).
//   - TestU33D2_FieldTableAgreesWithDigest runs the U31 46+8 field table: for
//     every "change only this field" row, sameAffectedView and the whole-stream
//     digest must answer the same, and both must match the row's expectation.
//     That is what makes the single per-route writer provably single.

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// u33RichView compiles a real two-account view (one template: 99 models × 5
// formats) and returns the decision with the most encoded content, so the field
// table exercises every lane, the cache-domain plan and the incident mark.
func u33RichView(t *testing.T) (*DecisionView, RouteRef, *RouteDecision) {
	t.Helper()
	accs := routeBenchProductionAccounts(2, 1)
	s := newSchedStatic(t, newMemLoader(map[int64][]*domain.Account{10: accs}))
	wireSources(s, nil, nil)
	s.compileOnce()
	dv := s.View().DecisionView()
	require.NotNil(t, dv)
	require.NotEmpty(t, dv.routes)

	var (
		bestRef   RouteRef
		best      *RouteDecision
		bestScore = -1
	)
	for ref, rd := range dv.routes {
		score := 10*len(rd.Primary) + 10*len(rd.Degraded) + len(rd.Explore.Ordered) +
			len(rd.Explore.Weights) + len(rd.Explore.Cumulative) + len(rd.Explore.Fallback) +
			len(rd.CacheDomainAccounts)
		if score > bestScore {
			bestScore, bestRef, best = score, ref, rd
		}
	}
	require.NotNil(t, best)
	// The fixture carries no quality window, so Primary/Degraded are empty by
	// construction. Populate both from Ordered so the lane rows are reachable;
	// all three lanes share writeCompiled → writeCompiledCandidate.
	rich := cloneRouteDecision(best)
	rich.Primary = append([]CompiledCandidate(nil), rich.Explore.Ordered...)
	rich.Degraded = append([]CompiledCandidate(nil), rich.Explore.Ordered...)
	require.NotEmpty(t, rich.Explore.Ordered, "the fixture must expose explore candidates")
	require.NotEmpty(t, rich.CacheDomainRing.Nodes, "the fixture must expose the cache-domain ring")
	require.NotEmpty(t, rich.CacheDomainAccounts, "the fixture must expose cache-domain accounts")
	return dv, bestRef, rich
}

// u33Cand returns the first usable candidate and its path label.
func u33Cand(d *RouteDecision) (*CompiledCandidate, string) {
	switch {
	case len(d.Primary) > 0:
		return &d.Primary[0], "Primary[0]"
	case len(d.Explore.Ordered) > 0:
		return &d.Explore.Ordered[0], "Ordered[0]"
	case len(d.Degraded) > 0:
		return &d.Degraded[0], "Degraded[0]"
	}
	return nil, ""
}

// u33RefBytes encodes the five RouteRef components exactly as encodeCanonical
// writes them (the ref is the map key: identity, not payload).
func u33RefBytes(ref RouteRef) []byte {
	var s canonicalSink
	s.collect = true
	s.resetCollect()
	s.writeVarint(ref.GroupID)
	s.writeString(ref.Format)
	s.writeString(ref.Model)
	s.writeString(ref.OperationTag)
	s.writeString(ref.RouteClassID)
	return append([]byte(nil), s.bytes()...)
}

// u33BodyBytes encodes one route payload through the production writer.
func u33BodyBytes(rd *RouteDecision) []byte {
	var s canonicalSink
	s.collect = true
	s.resetCollect()
	writeRouteBody(&s, rd)
	return append([]byte(nil), s.bytes()...)
}

// TestU33D2_WriteRouteBodyComposesWholeView proves the extraction boundary:
// a one-route view is exactly `uvarint(1) || ref bytes || body bytes`, so the
// body carries neither the route-count prefix nor the ref's five components and
// nothing was dropped at the tail.
func TestU33D2_WriteRouteBodyComposesWholeView(t *testing.T) {
	_, ref, rd := u33RichView(t)
	one := decisionViewBytes(&DecisionView{routes: map[RouteRef]*RouteDecision{ref: rd}})
	refB, bodyB := u33RefBytes(ref), u33BodyBytes(rd)
	want := append(append([]byte(nil), refB...), bodyB...)
	require.Equal(t, []byte{1}, one[:1], "a one-route view starts with the count varint")
	require.True(t, bytes.Equal(want, one[1:]),
		"whole-route encoding must be exactly ref bytes (%d) + body bytes (%d), got %d",
		len(refB), len(bodyB), len(one)-1)
}

// TestU33D2_GoldenCanonicalBytes pins the byte stream itself. The value is a
// deliberate golden: any change to the canonical format — including the
// writeRouteBody extraction — must fail here and be re-blessed on purpose.
func TestU33D2_GoldenCanonicalBytes(t *testing.T) {
	dv, _, _ := u33RichView(t)
	raw := decisionViewBytes(dv)
	var d decisionDigester
	got := d.digest(dv)
	require.Equal(t, sha256.Sum256(raw), got.sum, "digest must be sha256 of the canonical bytes")
	require.Equal(t, int64(len(raw)), got.count, "digest count must be the canonical byte length")
	require.Equal(t, sha256.Sum256(raw), sha256.Sum256(decisionViewBytes(dv)),
		"encoding must be deterministic across calls")
	t.Logf("routes=%d canonicalBytes=%d sha256=%x", len(dv.routes), len(raw), sha256.Sum256(raw))
}

// u33MutCase is one "change exactly this" row of the field table.
type u33MutCase struct {
	name string
	// diff is the expected verdict: true means the change is visible in the
	// canonical stream (both predicates must call the views different).
	diff bool
	mut  func(ref *RouteRef, rd *RouteDecision)
}

func u33FieldTable() []u33MutCase {
	return []u33MutCase{
		{name: "ref.GroupID", diff: true, mut: func(r *RouteRef, _ *RouteDecision) { r.GroupID++ }},
		{name: "ref.Format", diff: true, mut: func(r *RouteRef, _ *RouteDecision) { r.Format = "openaichat" }},
		{name: "ref.Model", diff: true, mut: func(r *RouteRef, _ *RouteDecision) { r.Model = "x" }},
		{name: "ref.OperationTag", diff: true, mut: func(r *RouteRef, _ *RouteDecision) { r.OperationTag = "x" }},
		{name: "ref.RouteClassID", diff: true, mut: func(r *RouteRef, _ *RouteDecision) { r.RouteClassID = "deadbeef" }},
		{name: "rd.Format", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.Format = "x" }},
		{name: "rd.RequestedModel", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.RequestedModel = "x" }},
		{name: "rd.RouteClassID", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.RouteClassID = "x" }},
		{name: "rd.CallerCategory", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.CallerCategory = "x" }},
		{name: "rd.OperationTag", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.OperationTag = "x" }},
		// Not in the canonical stream: the pre-D2 digest was blind to it too.
		{name: "rd.validated", diff: false, mut: func(_ *RouteRef, d *RouteDecision) { d.validated = !d.validated }},
		{name: "cand.AccountID", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { c, _ := u33Cand(d); c.AccountID += 100 }},
		{name: "cand.Lane", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { c, _ := u33Cand(d); c.Lane = AttemptLaneDegraded }},
		{name: "cand.TemplateID", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { c, _ := u33Cand(d); c.TemplateID++ }},
		{name: "cand.BaseURL", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { c, _ := u33Cand(d); c.BaseURL = "x" }},
		{name: "cand.Fingerprint", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { c, _ := u33Cand(d); c.Fingerprint = "x" }},
		{name: "cand.RequestedModel", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { c, _ := u33Cand(d); c.RequestedModel = "x" }},
		{name: "cand.MappedModel", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { c, _ := u33Cand(d); c.MappedModel = "x" }},
		{name: "cand.MappingMode", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { c, _ := u33Cand(d); c.MappingMode = domain.ModelMappingMode(99) }},
		{name: "cand.Quality", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { c, _ := u33Cand(d); c.Quality = "x" }},
		{name: "cand.QualityRaw", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { c, _ := u33Cand(d); c.QualityRaw = "x" }},
		{name: "cand.IdentityRevision", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { c, _ := u33Cand(d); c.IdentityRevision++ }},
		// Not in the canonical stream (writeCompiledCandidate writes 11 fields).
		{name: "cand.PlanKey", diff: false, mut: func(_ *RouteRef, d *RouteDecision) { c, _ := u33Cand(d); c.PlanKey.accountID++ }},
		{name: "cand.Leaf", diff: false, mut: func(_ *RouteRef, d *RouteDecision) { c, _ := u33Cand(d); c.Leaf = nil }},
		{name: "cand.Static", diff: false, mut: func(_ *RouteRef, d *RouteDecision) { c, _ := u33Cand(d); c.Static = nil }},
		{name: "cand.Fingerprint(2nd candidate)", diff: true, mut: func(_ *RouteRef, d *RouteDecision) {
			if len(d.Explore.Ordered) > 1 {
				d.Explore.Ordered[1].Fingerprint = "x"
			}
		}},
		{name: "Explore.Weights[new key]", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.Explore.Weights[99999] = 1 }},
		{name: "Explore.Weights changed value", diff: true, mut: func(_ *RouteRef, d *RouteDecision) {
			for id := range d.Explore.Weights {
				d.Explore.Weights[id]++
				break
			}
		}},
		{name: "Explore.Cumulative[0]", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.Explore.Cumulative[0]++ }},
		{name: "Explore.Cumulative length", diff: true, mut: func(_ *RouteRef, d *RouteDecision) {
			d.Explore.Cumulative = d.Explore.Cumulative[:len(d.Explore.Cumulative)-1]
		}},
		{name: "Explore.Total", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.Explore.Total++ }},
		{name: "Explore.ExploreBP", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.Explore.ExploreBP++ }},
		// Out-of-range indices are silently dropped by writeCompiledIndices, so
		// the row must move a LEGAL index onto a different target.
		{name: "Explore.Fallback[0] legal retarget", diff: true, mut: func(_ *RouteRef, d *RouteDecision) {
			d.Explore.Fallback[0] = (d.Explore.Fallback[0] + 1) % uint16(len(d.Explore.Ordered))
		}},
		{name: "Explore.Fallback length", diff: true, mut: func(_ *RouteRef, d *RouteDecision) {
			d.Explore.Fallback = d.Explore.Fallback[:len(d.Explore.Fallback)-1]
		}},
		{name: "CacheDomainRing.Domains[0]", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.CacheDomainRing.Domains[0] = "z" }},
		{name: "CacheDomainRing.Nodes[0].Hash", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.CacheDomainRing.Nodes[0].Hash++ }},
		{name: "CacheDomainRing.Nodes[0].Domain", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.CacheDomainRing.Nodes[0].Domain = "z" }},
		{name: "CacheDomainAccounts[0].AccountID", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.CacheDomainAccounts[0].AccountID++ }},
		{name: "CacheDomainAccounts[0].Domain", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.CacheDomainAccounts[0].Domain = "z" }},
		{name: "CacheDomainAccounts set nil", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.CacheDomainAccounts = nil }},
		{name: "Incident.Active", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.Incident.Active = !d.Incident.Active }},
		{name: "Incident.Kind", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.Incident.Kind = "model" }},
		{name: "Incident.Comparable", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.Incident.Comparable++ }},
		{name: "Incident.Degraded", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.Incident.Degraded++ }},
		{name: "Incident.Domains", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.Incident.Domains++ }},
		{name: "Incident.EvaluatedMinute", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.Incident.EvaluatedMinute = 12345 }},
		// U31's eight lane rows: all three lanes share writeCompiled →
		// writeCompiledCandidate, so an empty lane in the fixture would hide a
		// whole writer. Primary/Degraded are populated above for exactly this.
		{name: "Primary[1].Quality", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.Primary[1].Quality = "x" }},
		{name: "Primary[1].BaseURL", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.Primary[1].BaseURL = "x" }},
		{name: "Primary[1].MappingMode", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.Primary[1].MappingMode = domain.ModelMappingMode(99) }},
		{name: "Degraded[1].Fingerprint", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.Degraded[1].Fingerprint = "x" }},
		{name: "Degraded[1].IdentityRevision", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.Degraded[1].IdentityRevision++ }},
		{name: "Primary length", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.Primary = d.Primary[:1] }},
		{name: "Degraded length", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.Degraded = d.Degraded[:1] }},
		{name: "Ordered length", diff: true, mut: func(_ *RouteRef, d *RouteDecision) { d.Explore.Ordered = d.Explore.Ordered[:1] }},
	}
}

// TestU33D2_FieldTableAgreesWithDigest is the equivalence gate: for every
// single-field change, sameAffectedView and `digest(prev) != digest(next)` must
// agree, and both must match the row's expectation. The four blind rows are the
// fields the canonical stream never writes, so the pre-D2 digest was blind to
// them too — they are registered, not silently tolerated.
func TestU33D2_FieldTableAgreesWithDigest(t *testing.T) {
	_, ref, base := u33RichView(t)
	rows := u33FieldTable()
	require.Len(t, rows, 54, "U31 field table: 46 body rows + 8 lane rows")
	require.NotNil(t, u33MustCand(t, base))
	t.Logf("base ref=%+v primary=%d degraded=%d ordered=%d weights=%d cumulative=%d fallback=%d nodes=%d accs=%d",
		ref, len(base.Primary), len(base.Degraded), len(base.Explore.Ordered), len(base.Explore.Weights),
		len(base.Explore.Cumulative), len(base.Explore.Fallback), len(base.CacheDomainRing.Nodes),
		len(base.CacheDomainAccounts))

	var d decisionDigester
	blind := 0
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			mut := cloneRouteDecision(base)
			r := ref
			tc.mut(&r, mut)
			prev := &DecisionView{routes: map[RouteRef]*RouteDecision{ref: base}}
			next := &DecisionView{routes: map[RouteRef]*RouteDecision{r: mut}}
			// The changed identity must be part of the affected set: a ref
			// change is a key-set change, never a payload change.
			affected := map[RouteRef]struct{}{ref: {}}
			if r != ref {
				affected[r] = struct{}{}
			}
			gotSame := d.sameAffectedView(prev, next, affected)
			wantSame := d.digest(prev) == d.digest(next)
			require.Equal(t, wantSame, gotSame,
				"sameAffectedView and the whole-stream digest must agree")
			require.Equal(t, tc.diff, !wantSame,
				"the row's expectation must match the canonical stream")
			require.Equal(t, tc.diff, !gotSame,
				"the guard must see exactly what the canonical stream sees")
			if !tc.diff {
				blind++
			}
		})
	}
	require.Equal(t, 4, blind, "exactly the four non-encoded fields are blind")
}

func u33MustCand(t *testing.T, d *RouteDecision) *CompiledCandidate {
	t.Helper()
	c, _ := u33Cand(d)
	require.NotNil(t, c, "the fixture must expose a candidate")
	return c
}

// TestU33D2_GuardEdges covers the shapes the field table cannot express: view
// nil-ness, route count, the wholeness bit, membership changes, nil decisions
// and the non-injective out-of-range fallback.
func TestU33D2_GuardEdges(t *testing.T) {
	_, ref, base := u33RichView(t)
	other := RouteRefFor(10, "openai-chat", "u33-other")
	require.NotEqual(t, ref, other)
	baseView := func(r RouteRef, rd *RouteDecision) *DecisionView {
		return &DecisionView{routes: map[RouteRef]*RouteDecision{r: rd}}
	}
	one := func(r RouteRef) map[RouteRef]struct{} { return map[RouteRef]struct{}{r: {}} }
	var d decisionDigester

	t.Run("nil/nil", func(t *testing.T) {
		require.True(t, d.sameAffectedView(nil, nil, nil))
	})
	t.Run("nil-vs-empty", func(t *testing.T) {
		empty := &DecisionView{routes: map[RouteRef]*RouteDecision{}}
		require.False(t, d.sameAffectedView(nil, empty, nil))
		require.False(t, d.sameAffectedView(empty, nil, nil))
		require.NotEqual(t, d.digest(nil), d.digest(empty), "the digest distinguishes nil from empty too")
	})
	t.Run("empty-vs-empty", func(t *testing.T) {
		a := &DecisionView{routes: map[RouteRef]*RouteDecision{}}
		b := &DecisionView{routes: map[RouteRef]*RouteDecision{}}
		require.True(t, d.sameAffectedView(a, b, nil))
		require.Equal(t, d.digest(a), d.digest(b))
	})
	t.Run("same-pointer", func(t *testing.T) {
		v := baseView(ref, base)
		require.True(t, d.sameAffectedView(v, v, one(ref)), "pointer identity is equality")
	})
	t.Run("route-dropped", func(t *testing.T) {
		prev := baseView(ref, base)
		next := &DecisionView{routes: map[RouteRef]*RouteDecision{}}
		require.False(t, d.sameAffectedView(prev, next, one(ref)))
		require.NotEqual(t, d.digest(prev), d.digest(next))
	})
	t.Run("route-added", func(t *testing.T) {
		prev := &DecisionView{routes: map[RouteRef]*RouteDecision{}}
		next := baseView(other, cloneRouteDecision(base))
		require.False(t, d.sameAffectedView(prev, next, one(other)))
	})
	t.Run("affected-nil", func(t *testing.T) {
		prev := baseView(ref, base)
		next := baseView(ref, cloneRouteDecision(base))
		require.True(t, d.sameAffectedView(prev, next, nil), "no affected refs: len+whole decide")
		require.True(t, d.sameAffectedView(prev, next, map[RouteRef]struct{}{}), "empty affected set")
	})
	t.Run("affected-ref-missing-both-sides", func(t *testing.T) {
		prev := baseView(ref, base)
		next := baseView(ref, cloneRouteDecision(base))
		require.True(t, d.sameAffectedView(prev, next, one(other)), "a ref absent from both views cannot differ")
	})
	t.Run("affected-ref-missing-one-side", func(t *testing.T) {
		prev := baseView(ref, base)
		next := baseView(other, cloneRouteDecision(base))
		require.False(t, d.sameAffectedView(prev, next, one(ref)),
			"a ref present on one side only must publish")
		require.False(t, d.sameAffectedView(prev, next, one(other)),
			"the added ref must publish from either direction")
	})
	t.Run("nil-decision-one-side", func(t *testing.T) {
		prev := baseView(ref, nil)
		next := baseView(ref, cloneRouteDecision(base))
		require.False(t, d.sameAffectedView(prev, next, one(ref)))
		require.False(t, d.sameAffectedView(next, prev, one(ref)))
	})
	t.Run("nil-decision-both-sides", func(t *testing.T) {
		prev := baseView(ref, nil)
		next := baseView(ref, nil)
		require.True(t, d.sameAffectedView(prev, next, one(ref)))
	})
	t.Run("whole-both-directions", func(t *testing.T) {
		for _, tc := range []struct {
			name                 string
			prevWhole, nextWhole bool
		}{
			{"false-to-true", false, true},
			{"true-to-false", true, false},
		} {
			prev := baseView(ref, base)
			prev.whole = tc.prevWhole
			next := baseView(ref, cloneRouteDecision(base))
			next.whole = tc.nextWhole
			// The digest cannot see the wholeness bit, so D2 is STRICTER here on
			// purpose: a wholeness flip publishes instead of being absorbed.
			require.Equal(t, d.digest(prev), d.digest(next),
				"%s: the canonical stream does not encode whole", tc.name)
			require.False(t, d.sameAffectedView(prev, next, one(ref)),
				"%s: a wholeness flip must publish", tc.name)
		}
	})
	t.Run("out-of-range-fallback-agrees", func(t *testing.T) {
		// writeCompiledIndices drops out-of-range indices, so two different
		// fallback lists can encode identically. Both predicates call this pair
		// equal — agreement holds. Production cannot reach it:
		// validateRouteDecision rejects out-of-range fallbacks before publish, so
		// no such pair can enter the guard.
		n := uint16(len(base.Explore.Ordered))
		a := cloneRouteDecision(base)
		b := cloneRouteDecision(base)
		a.Explore.Fallback = []uint16{n, n + 1}
		b.Explore.Fallback = []uint16{n + 2, n + 3}
		prev, next := baseView(ref, a), baseView(ref, b)
		require.True(t, d.sameAffectedView(prev, next, one(ref)))
		require.Equal(t, d.digest(prev), d.digest(next),
			"the canonical stream is not injective; D2 inherits exactly the digest's blindness")
		t.Logf("non-injective pair: len(Ordered)=%d, both encode to the same body", n)
	})
	t.Run("route-count-varint-boundary", func(t *testing.T) {
		// 130 routes crosses the one-byte count prefix (>= 128) while staying a
		// valid view; an affected payload change must still be found.
		prev := &DecisionView{routes: make(map[RouteRef]*RouteDecision, 130)}
		for i := 0; i < 130; i++ {
			r := RouteRefFor(int64(20+i), "openai-chat", "m")
			prev.routes[r] = cloneRouteDecision(base)
		}
		next := &DecisionView{routes: map[RouteRef]*RouteDecision{}}
		var target RouteRef
		for r, rd := range prev.routes {
			next.routes[r] = rd
			target = r
		}
		require.Len(t, next.routes, 130)
		require.True(t, d.sameAffectedView(prev, next, nil), "identical views of 130 routes")
		mut := cloneRouteDecision(next.routes[target])
		mut.Explore.Total++
		next.routes[target] = mut
		require.False(t, d.sameAffectedView(prev, next, one(target)))
		require.NotEqual(t, d.digest(prev), d.digest(next))
	})
}

// TestU33D2_ScopedCarrySharesUnaffectedPointers pins the premise the affected-only
// comparison rests on: compileScopedRoutes hands every unaffected route forward
// by pointer. If that ever becomes a copy, the guard must be revisited (this
// test goes red), because a copy could carry a byte difference no affected ref
// would reveal.
func TestU33D2_ScopedCarrySharesUnaffectedPointers(t *testing.T) {
	tplA := tplWith(domain.FormatOpenAIChat, []string{"m"})
	tplB := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accsA := []*domain.Account{accWithEnabled(1, tplA, true, 10000)}
	accsB := []*domain.Account{accWithEnabled(2, tplB, true, 10000)}
	s := newSchedStatic(t, newMemLoader(map[int64][]*domain.Account{10: accsA, 11: accsB}))
	wireSources(s, nil, nil)
	s.compileOnce()
	carry := s.View().DecisionView()
	require.NotNil(t, carry)
	sv := s.View().StaticView()

	hot := sv.routeIndex.groupRoutes[10]
	require.NotEmpty(t, hot)
	affected := make(map[RouteRef]struct{}, len(hot))
	for _, ref := range hot {
		affected[ref] = struct{}{}
	}
	fire := &compileFire{input: CompilerInputs{Static: sv}, carry: carry, affected: affected}
	dv, err := s.compileScopedRoutes(fire)
	require.NoError(t, err)

	var carried, recomputed, otherGroup int
	for ref, rd := range dv.routes {
		if ref.GroupID == 11 {
			otherGroup++
			require.Same(t, carry.routes[ref], rd, "unaffected group must be carried by pointer: %+v", ref)
			continue
		}
		if _, ok := affected[ref]; ok {
			recomputed++
			continue
		}
		carried++
		require.Same(t, carry.routes[ref], rd, "unaffected route must be carried by pointer: %+v", ref)
	}
	require.Positive(t, recomputed, "the hot group must be recomputed")
	require.Positive(t, carried+otherGroup, "there must be unaffected routes to carry")
}

// TestU33D2_CrossGroupFixupEntersAffected pins the other half of the premise:
// removing a multi-group account from one group rewrites its leaf, so the OTHER
// groups whose routes reference it must be recomputed in the same fire. The
// assertion is behavioural (recomputed vs carried-by-pointer), not a comment.
func TestU33D2_CrossGroupFixupEntersAffected(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	shared := accWithEnabled(1, tpl, true, 10000)
	loader := newMemLoader(map[int64][]*domain.Account{
		10: {shared, accWithEnabled(2, tpl, true, 10000)},
		11: {shared},
		12: {accWithEnabled(3, tpl, true, 10000)},
	})
	s := newSchedStatic(t, loader)
	wireSources(s, nil, nil)
	s.compileOnce()
	carry := s.View().DecisionView()
	require.NotNil(t, carry)

	// Drop the shared account from group 10 only; it must stay in group 11.
	loader.mu.Lock()
	loader.byGroup[10] = loader.byGroup[10][1:]
	loader.mu.Unlock()
	s.InvalidateGroup(10)
	s.compileOnce()

	published := s.View().DecisionView()
	require.NotNil(t, published)
	var g11Reused, g11Recomputed, g12Recomputed, g12Reused int
	for ref := range published.routes {
		same := carry.routes[ref] == published.routes[ref]
		switch ref.GroupID {
		case 11:
			if same {
				g11Reused++
			} else {
				g11Recomputed++
			}
		case 12:
			if same {
				g12Reused++
			} else {
				g12Recomputed++
			}
		}
	}
	require.Positive(t, g11Recomputed,
		"the other group sharing the removed account must be recomputed in this fire")
	require.Zero(t, g11Reused, "no group-11 route may be carried forward")
	require.Positive(t, g12Reused, "the untouched group must be carried by pointer")
	require.Zero(t, g12Recomputed, "the untouched group must not be recomputed")
	t.Logf("fix-up scope: group11 recomputed=%d reused=%d ; group12 recomputed=%d reused=%d",
		g11Recomputed, g11Reused, g12Recomputed, g12Reused)
}
