// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestRed_Blocker1_ImagesDistinct(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIImages, []string{"m"})
	a1 := accWithEnabled(1, tpl, true, 10000)
	a2 := accWithEnabled(2, tpl, true, 10000)
	accs := []*domain.Account{a1, a2}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	c := NewRoutingCompiler()
	price := domain.ResolvedPrices{InputPerM: pricePtr(1000)}
	prices := map[string]domain.ResolvedPrices{"m": price}
	genRC, _ := domain.RouteClassID(10, domain.FormatOpenAIImages, "m", domain.OpImagesGenerations)
	editRC, _ := domain.RouteClassID(10, domain.FormatOpenAIImages, "m", domain.OpImagesEdits)
	require.NotEqual(t, domain.RouteClassIDHex(genRC), domain.RouteClassIDHex(editRC))
	fpVal1 := func(a *domain.Account) domain.CandidateFingerprintVal {
		fpStr, ferr := candidateFingerprint(a)
		if ferr == nil && fpStr != "" {
			if v, err := domain.HexToID(fpStr); err == nil {
				return domain.CandidateFingerprintVal(v)
			}
		}
		var b [32]byte
		b[0] = byte(a.ID >> 56)
		b[1] = byte(a.ID >> 48)
		b[2] = byte(a.ID >> 40)
		b[3] = byte(a.ID >> 32)
		b[4] = byte(a.ID >> 24)
		b[5] = byte(a.ID >> 16)
		b[6] = byte(a.ID >> 8)
		b[7] = byte(a.ID)
		return domain.CandidateFingerprintVal(b)
	}
	fp1 := fpVal1(a1)
	fp2 := fpVal1(a2)
	logged := math.Log(100)
	loggedSlow := math.Log(500)
	merged := make(map[CandidateQualityKey]CandidateQualityInput)
	merged[CandidateQualityKey{RouteClassID: genRC, Fingerprint: fp1}] = CandidateQualityInput{Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2900}
	merged[CandidateQualityKey{RouteClassID: genRC, Fingerprint: fp2}] = CandidateQualityInput{Counts: Counts{Attempts: 30, Successes: 15, TTFTCount: 30, SumLog: loggedSlow * 30, SumSq: loggedSlow * loggedSlow * 30}, InputTokens: 1500}
	merged[CandidateQualityKey{RouteClassID: editRC, Fingerprint: fp1}] = CandidateQualityInput{Counts: Counts{Attempts: 30, Successes: 15, TTFTCount: 30, SumLog: loggedSlow * 30, SumSq: loggedSlow * loggedSlow * 30}, InputTokens: 1500}
	merged[CandidateQualityKey{RouteClassID: editRC, Fingerprint: fp2}] = CandidateQualityInput{Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2900}
	view, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: merged, Prices: prices})
	require.NoError(t, err)
	rcGenHex := domain.RouteClassIDHex(genRC)
	rcEditHex := domain.RouteClassIDHex(editRC)
	rrGen := RouteRef{GroupID: 10, Format: string(domain.FormatOpenAIImages), Model: "m", OperationTag: string(domain.OpImagesGenerations), RouteClassID: rcGenHex}
	rrEdit := RouteRef{GroupID: 10, Format: string(domain.FormatOpenAIImages), Model: "m", OperationTag: string(domain.OpImagesEdits), RouteClassID: rcEditHex}
	require.NotEqual(t, rrGen.RouteClassID, rrEdit.RouteClassID)
	dGen, ok := view.Routes()[rrGen]
	require.True(t, ok, "generations route must exist")
	dEdit, ok := view.Routes()[rrEdit]
	require.True(t, ok, "edits route must exist distinct from generations")
	require.NotEqual(t, dGen.Primary, dEdit.Primary)
	require.Contains(t, compiledAccountIDs(dGen.Primary), int64(1))
	require.Contains(t, compiledAccountIDs(dEdit.Primary), int64(2))
}

func TestRed_Blocker2_HealthResolvedMappedModel(t *testing.T) {
	tpl := &domain.Template{SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"req"}, ModelMapping: map[string]domain.ModelMappingEntry{"req": {MappedModel: "resolved", Mode: domain.ModelMappingModeExplicit}}}
	acc := accWithEnabled(1, tpl, true, 10000)
	acc.LifecycleRevision = 1
	m := newMemLoader(map[int64][]*domain.Account{10: {acc}})
	s := newSched(t, m)
	price := domain.ResolvedPrices{InputPerM: pricePtr(1000)}
	prices := map[string]domain.ResolvedPrices{"req": price, "resolved": price}
	logged := math.Log(100)
	routeRC, _ := domain.RouteClassID(10, domain.FormatOpenAIChat, "req", domain.OpChatCompletions)
	fpStr, _ := candidateFingerprint(acc)
	var fpVal domain.CandidateFingerprintVal
	if v, err := domain.HexToID(fpStr); err == nil {
		fpVal = domain.CandidateFingerprintVal(v)
	}
	q := map[CandidateQualityKey]CandidateQualityInput{
		{RouteClassID: routeRC, Fingerprint: fpVal}: {Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2900},
	}
	c := NewRoutingCompiler()
	// Health entry for resolved model should exclude.
	callerKind := callerKindForFormat(domain.FormatOpenAIChat)
	op := operationTagForFormat(string(domain.FormatOpenAIChat))
	qcResolved, _ := domain.QualityClassID(callerKind, domain.FormatOpenAIChat, "resolved", op)
	qcRequested, _ := domain.QualityClassID(callerKind, domain.FormatOpenAIChat, "req", op)
	require.NotEqual(t, domain.QualityClassIDHex(qcResolved), domain.QualityClassIDHex(qcRequested))
	hkResolved := HealthKey{AccountID: 1, Quality: domain.QualityClassIDHex(qcResolved), Revision: 1}
	health := map[HealthKey]HealthState{hkResolved: StateOPEN}
	view, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices, Health: health})
	require.NoError(t, err)
	rr := RouteRefFor(10, string(domain.FormatOpenAIChat), "req")
	rd, ok := view.Routes()[rr]
	require.True(t, ok)
	all := append(append([]int64{}, compiledAccountIDs(rd.Primary)...), compiledAccountIDs(rd.Explore.Ordered)...)
	all = append(all, compiledAccountIDs(rd.Degraded)...)
	require.NotContains(t, all, int64(1), "health with resolved model must exclude")
	// Health entry for requested (unmapped) quality must NOT exclude when candidate maps to resolved.
	hkRequested := HealthKey{AccountID: 1, Quality: domain.QualityClassIDHex(qcRequested), Revision: 1}
	health2 := map[HealthKey]HealthState{hkRequested: StateOPEN}
	view2, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices, Health: health2})
	require.NoError(t, err)
	rd2, ok := view2.Routes()[rr]
	require.True(t, ok)
	all2 := append(append([]int64{}, compiledAccountIDs(rd2.Primary)...), compiledAccountIDs(rd2.Explore.Ordered)...)
	all2 = append(all2, compiledAccountIDs(rd2.Degraded)...)
	require.Contains(t, all2, int64(1), "unrelated requested quality must not exclude mapped candidate")
}

func TestRed_Blocker3_UnrelatedQualityNotExclude(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m1", "m2"})
	acc := accWithEnabled(1, tpl, true, 10000)
	acc.LifecycleRevision = 1
	m := newMemLoader(map[int64][]*domain.Account{10: {acc}})
	s := newSched(t, m)
	price := domain.ResolvedPrices{InputPerM: pricePtr(1000)}
	prices := map[string]domain.ResolvedPrices{"m1": price, "m2": price}
	logged := math.Log(100)
	routeRC, _ := domain.RouteClassID(10, domain.FormatOpenAIChat, "m1", domain.OpChatCompletions)
	fpStr, _ := candidateFingerprint(acc)
	var fpVal domain.CandidateFingerprintVal
	if v, err := domain.HexToID(fpStr); err == nil {
		fpVal = domain.CandidateFingerprintVal(v)
	}
	q := map[CandidateQualityKey]CandidateQualityInput{
		{RouteClassID: routeRC, Fingerprint: fpVal}: {Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2900},
	}
	c := NewRoutingCompiler()
	callerKind := callerKindForFormat(domain.FormatOpenAIChat)
	op := operationTagForFormat(string(domain.FormatOpenAIChat))
	qcM1, _ := domain.QualityClassID(callerKind, domain.FormatOpenAIChat, "m1", op)
	qcM2, _ := domain.QualityClassID(callerKind, domain.FormatOpenAIChat, "m2", op)
	require.NotEqual(t, domain.QualityClassIDHex(qcM1), domain.QualityClassIDHex(qcM2))
	hkOther := HealthKey{AccountID: 1, Quality: domain.QualityClassIDHex(qcM2), Revision: 1}
	health := map[HealthKey]HealthState{hkOther: StateOPEN}
	view, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices, Health: health})
	require.NoError(t, err)
	rr := RouteRefFor(10, string(domain.FormatOpenAIChat), "m1")
	rd, ok := view.Routes()[rr]
	require.True(t, ok)
	all := append(append([]int64{}, compiledAccountIDs(rd.Primary)...), compiledAccountIDs(rd.Explore.Ordered)...)
	all = append(all, compiledAccountIDs(rd.Degraded)...)
	require.Contains(t, all, int64(1), "unrelated quality must not exclude")
}

func TestRed_Blocker4_LatchedFalseNoExclude(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	acc := accWithEnabled(1, tpl, true, 10000)
	acc.LifecycleRevision = 1
	m := newMemLoader(map[int64][]*domain.Account{10: {acc}})
	s := newSched(t, m)
	price := domain.ResolvedPrices{InputPerM: pricePtr(1000)}
	prices := map[string]domain.ResolvedPrices{"m": price}
	routeRC, _ := domain.RouteClassID(10, domain.FormatOpenAIChat, "m", domain.OpChatCompletions)
	fpStr, _ := candidateFingerprint(acc)
	var fpVal domain.CandidateFingerprintVal
	if v, err := domain.HexToID(fpStr); err == nil {
		fpVal = domain.CandidateFingerprintVal(v)
	}
	logged := math.Log(100)
	q := map[CandidateQualityKey]CandidateQualityInput{
		{RouteClassID: routeRC, Fingerprint: fpVal}: {Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2900},
	}
	c := NewRoutingCompiler()
	lk := compilerLatchKeyFor(acc)
	latchedFalse := map[LatchKey]bool{lk: false}
	view, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices, Latched: latchedFalse})
	require.NoError(t, err)
	rr := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	rd, ok := view.Routes()[rr]
	require.True(t, ok)
	all := append(append([]int64{}, compiledAccountIDs(rd.Primary)...), compiledAccountIDs(rd.Explore.Ordered)...)
	all = append(all, compiledAccountIDs(rd.Degraded)...)
	require.Contains(t, all, int64(1), "latched false must not exclude")
	latchedTrue := map[LatchKey]bool{lk: true}
	view2, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices, Latched: latchedTrue})
	require.NoError(t, err)
	rd2, ok := view2.Routes()[rr]
	require.True(t, ok)
	all2 := append(append([]int64{}, compiledAccountIDs(rd2.Primary)...), compiledAccountIDs(rd2.Explore.Ordered)...)
	all2 = append(all2, compiledAccountIDs(rd2.Degraded)...)
	require.NotContains(t, all2, int64(1), "latched true must exclude")
}

func TestRed_Blocker5_ViewImmutability(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	price := domain.ResolvedPrices{InputPerM: pricePtr(1000)}
	prices := map[string]domain.ResolvedPrices{"m": price}
	q := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{1: qualityInput(30, 29, 100, 100), 2: qualityInput(30, 29, 100, 100)}, accs)
	c := NewRoutingCompiler()
	view, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices})
	require.NoError(t, err)
	rr := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	// Mutate via Routes() map copy should not affect original.
	routesCopy := view.Routes()
	origPrimary := append([]int64(nil), compiledAccountIDs(routesCopy[rr].Primary)...)
	routesCopy[rr].Primary = append(routesCopy[rr].Primary, CompiledCandidate{AccountID: 999})
	routesCopy[rr].Explore.Weights[999] = 100
	routesCopy[rr].Explore.Ordered = append(routesCopy[rr].Explore.Ordered, CompiledCandidate{AccountID: 999})
	routesAgain := view.Routes()
	require.Equal(t, origPrimary, compiledAccountIDs(routesAgain[rr].Primary))
	require.NotContains(t, compiledAccountIDs(routesAgain[rr].Explore.Ordered), int64(999))
	require.NotContains(t, compiledAccountIDs(routesAgain[rr].Primary), int64(999))
	// Mutate via Route() accessor should return copy.
	rd, ok := view.Route(10, string(domain.FormatOpenAIChat), "m")
	require.True(t, ok)
	rd.Primary = append(rd.Primary, CompiledCandidate{AccountID: 888})
	rd2, ok := view.Route(10, string(domain.FormatOpenAIChat), "m")
	require.True(t, ok)
	require.NotContains(t, compiledAccountIDs(rd2.Primary), int64(888))
	require.NotContains(t, compiledAccountIDs(rd2.Explore.Ordered), int64(888))
	// StaticView Groups/ByID copy protection
	sv := s.View().StaticView()
	gCopy := sv.Groups()
	gCopy[999] = nil
	require.NotContains(t, sv.Groups(), int64(999))
	bCopy := sv.ByID()
	bCopy[999] = nil
	require.NotContains(t, sv.ByID(), int64(999))
	// RoutingView copy protection
	rv := s.View()
	rgCopy := rv.Groups()
	rgCopy[999] = nil
	require.NotContains(t, rv.Groups(), int64(999))
}

func TestRoutingCompiler_candidateFactsStayConsistentAcrossFilterAndCompile(t *testing.T) {
	// Given
	tpl := &domain.Template{
		ID: 7, BaseURL: "https://up.example.com",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		Models:           []string{"requested"},
		ModelMapping: map[string]domain.ModelMappingEntry{
			"requested": {MappedModel: "mapped", Mode: domain.ModelMappingModeExplicit},
		},
	}
	account := accWithEnabled(1, tpl, true, 10000)
	s := newSched(t, newMemLoader(map[int64][]*domain.Account{10: {account}}))
	snapshot, ok := s.View().StaticView().Account(1)
	require.True(t, ok)
	route := routeKey{format: domain.FormatOpenAIChat, model: "requested"}
	op := domain.OpChatCompletions

	// When
	facts := buildCandidateFacts([]*accountSnapshot{snapshot}, route, op)
	filtered := filterCandidates(facts, nil, nil)
	routeClass, err := domain.RouteClassID(10, route.format, route.model, op)
	require.NoError(t, err)
	rr := RouteRefForOp(10, string(route.format), route.model, op)
	quality := map[CandidateQualityKey]CandidateQualityInput{
		{RouteClassID: routeClass, Fingerprint: facts[0].identityFingerprint}: qualityInput(30, 29, 2900, 2900),
	}
	decision, err := compileRouteDecision(filtered, route, domain.RouteClassIDVal(routeClass), quality, map[string]domain.ResolvedPrices{
		"requested": {InputPerM: pricePtr(1000)},
	}, rr)

	// Then
	require.NoError(t, err)
	require.Len(t, facts, 1)
	require.Same(t, snapshot, facts[0].account)
	require.Equal(t, int64(1), facts[0].accountID)
	require.Equal(t, int64(1), facts[0].revision)
	require.Equal(t, "requested", facts[0].requestedModel)
	require.Equal(t, "mapped", facts[0].mappedModel)
	require.Equal(t, domain.ModelMappingModeExplicit, facts[0].mappingMode)
	require.Equal(t, qualityClassHexForWithOp(route.format, "requested", op), facts[0].qualityRaw)
	require.Equal(t, qualityClassHexForWithOp(route.format, "mapped", op), facts[0].quality)
	compiled := append(append(append([]CompiledCandidate{}, decision.Primary...), decision.Explore.Ordered...), decision.Degraded...)
	require.Len(t, compiled, 1)
	require.Equal(t, facts[0].fingerprint, compiled[0].Fingerprint)
	require.Equal(t, facts[0].mappedModel, compiled[0].MappedModel)
	require.Equal(t, facts[0].mappingMode, compiled[0].MappingMode)
	require.Equal(t, facts[0].quality, compiled[0].Quality)
	require.Equal(t, facts[0].qualityRaw, compiled[0].QualityRaw)

	health := map[HealthKey]HealthState{{AccountID: facts[0].accountID, Quality: facts[0].quality, Revision: facts[0].revision}: StateOPEN}
	require.Empty(t, filterCandidates(facts, health, nil))
}
