// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestRoutingCompilerModelOperationSeparation(t *testing.T) {
	// Same model "m" serving both chat and responses via same account template supporting both formats.
	// Quality must be keyed by RouteClassID: chat quality should not bleed into responses route.
	tpl := &domain.Template{SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatOpenAIResponses}, Models: []string{"m"}}
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	price := domain.ResolvedPrices{InputPerM: pricePtr(1000)}
	prices := map[string]domain.ResolvedPrices{"m": price}
	// Chat: account 1 high success -> should be Primary, account 2 low -> Degraded
	chatQ := map[CandidateQualityKey]CandidateQualityInput{}
	for _, a := range accs {
		key := qualityKeyFor(10, domain.FormatOpenAIChat, "m", a)
		if a.ID == 1 {
			logged := math.Log(100)
			chatQ[key] = CandidateQualityInput{Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2900, OutputTokens: 2900}
		} else {
			logged := math.Log(500)
			chatQ[key] = CandidateQualityInput{Counts: Counts{Attempts: 30, Successes: 15, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 1500, OutputTokens: 1500}
		}
	}
	// Responses: reverse quality - account 1 low, account 2 high
	respQ := map[CandidateQualityKey]CandidateQualityInput{}
	for _, a := range accs {
		key := qualityKeyFor(10, domain.FormatOpenAIResponses, "m", a)
		if a.ID == 2 {
			logged := math.Log(100)
			respQ[key] = CandidateQualityInput{Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2900, OutputTokens: 2900}
		} else {
			logged := math.Log(500)
			respQ[key] = CandidateQualityInput{Counts: Counts{Attempts: 30, Successes: 15, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 1500, OutputTokens: 1500}
		}
	}
	// Merge both route qualities into one map (complete key preserves separation)
	merged := make(map[CandidateQualityKey]CandidateQualityInput, len(chatQ)+len(respQ))
	for k, v := range chatQ {
		merged[k] = v
	}
	for k, v := range respQ {
		merged[k] = v
	}
	c := NewRoutingCompiler()
	view, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: merged, Prices: prices})
	require.NoError(t, err)
	rrChat := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	rrResp := RouteRefFor(10, string(domain.FormatOpenAIResponses), "m")
	chatDec, ok := view.routes[rrChat]
	require.True(t, ok)
	respDec, ok := view.routes[rrResp]
	require.True(t, ok)
	// v4-S2: op separation lives in the interned per-route hex, not the normalized key.
	require.NotEmpty(t, chatDec.RouteClassID)
	require.NotEmpty(t, respDec.RouteClassID)
	require.NotEqual(t, chatDec.RouteClassID, respDec.RouteClassID)
	require.Contains(t, compiledAccountIDs(chatDec.Primary), int64(1))
	require.Contains(t, compiledAccountIDs(respDec.Primary), int64(2))
	require.NotEqual(t, compiledAccountIDs(chatDec.Primary), compiledAccountIDs(respDec.Primary), "operation separation must give different Primary")
}

func TestRoutingCompilerCandidateFingerprintSeparation(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	a1 := accWithEnabled(1, tpl, true, 10000)
	a1.UpstreamKey = "k1"
	a1.LifecycleRevision = 1
	a2 := accWithEnabled(2, tpl, true, 10000)
	a2.UpstreamKey = "k2"
	a2.LifecycleRevision = 1
	// Same model but different fingerprints; quality per fingerprint distinct
	m := newMemLoader(map[int64][]*domain.Account{10: {a1, a2}})
	s := newSched(t, m)
	price := domain.ResolvedPrices{InputPerM: pricePtr(1000)}
	prices := map[string]domain.ResolvedPrices{"m": price}
	q := make(map[CandidateQualityKey]CandidateQualityInput)
	for _, a := range []*domain.Account{a1, a2} {
		key := qualityKeyFor(10, domain.FormatOpenAIChat, "m", a)
		if a.ID == 1 {
			logged := math.Log(100)
			q[key] = CandidateQualityInput{Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2900, OutputTokens: 2900}
		} else {
			logged := math.Log(500)
			q[key] = CandidateQualityInput{Counts: Counts{Attempts: 30, Successes: 15, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 1500, OutputTokens: 1500}
		}
	}
	c := NewRoutingCompiler()
	view, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices})
	require.NoError(t, err)
	rd, ok := view.routes[RouteRefFor(10, string(domain.FormatOpenAIChat), "m")]
	require.True(t, ok)
	require.Contains(t, compiledAccountIDs(rd.Primary), int64(1))
	require.Contains(t, compiledAccountIDs(rd.Degraded), int64(2))
	// Ensure swapping fingerprint keys swaps classification
	qSwap := make(map[CandidateQualityKey]CandidateQualityInput)
	for _, a := range []*domain.Account{a1, a2} {
		key := qualityKeyFor(10, domain.FormatOpenAIChat, "m", a)
		if a.ID == 2 {
			logged := math.Log(100)
			qSwap[key] = CandidateQualityInput{Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2900, OutputTokens: 2900}
		} else {
			logged := math.Log(500)
			qSwap[key] = CandidateQualityInput{Counts: Counts{Attempts: 30, Successes: 15, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 1500, OutputTokens: 1500}
		}
	}
	view2, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: qSwap, Prices: prices})
	require.NoError(t, err)
	rd2, ok := view2.routes[RouteRefFor(10, string(domain.FormatOpenAIChat), "m")]
	require.True(t, ok)
	require.Contains(t, compiledAccountIDs(rd2.Primary), int64(2))
	require.Contains(t, compiledAccountIDs(rd2.Degraded), int64(1))
}

func TestRoutingCompilerHealthLatchFencing(t *testing.T) {
	// v5-§5.1A (COMPILED-HEALTH-FREE): live health/latch state must NOT leak
	// into compilation — the compiled plan includes every statically eligible
	// candidate; serving gates live solely in reserveOnView (pinned unmodified
	// by TestSchedulerReserveAttemptUsesDynamicCandidateGates).
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000)}
	accs[0].LifecycleRevision = 1
	accs[1].LifecycleRevision = 1
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	price := domain.ResolvedPrices{InputPerM: pricePtr(1000)}
	prices := map[string]domain.ResolvedPrices{"m": price}
	q := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{1: qualityInput(30, 29, 100, 100), 2: qualityInput(30, 29, 100, 100)}, accs)
	c := NewRoutingCompiler()
	// v5-§5.1A: live OPEN health on account 2 (exact + stale-revision entries
	// alike) must NOT exclude — compile is health-free.
	h := NewRuntimeHealth(nil, "self", nil, nil)
	hkGood := compilerHealthKeyFor(accs[1], domain.FormatOpenAIChat, "m")
	hkStale := HealthKey{AccountID: 1, Quality: compilerHealthKeyFor(accs[0], domain.FormatOpenAIChat, "m").Quality, Revision: 99}
	h.view.Store(&healthView{entries: map[HealthKey]healthEntry{
		hkGood:  {Key: hkGood, State: StateOPEN},
		hkStale: {Key: hkStale, State: StateOPEN},
	}})
	s.health = h
	view, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices})
	require.NoError(t, err)
	rd, ok := view.routes[RouteRefFor(10, string(domain.FormatOpenAIChat), "m")]
	require.True(t, ok)
	all := append(append([]int64{}, compiledAccountIDs(rd.Primary)...), compiledAccountIDs(rd.Explore.Ordered)...)
	all = append(all, compiledAccountIDs(rd.Degraded)...)
	require.Contains(t, all, int64(1), "stale-revision OPEN must not exclude post-v5")
	require.Contains(t, all, int64(2), "exact OPEN must not exclude post-v5")
	// v5-§5.1A: live latch on account 1 (stale-revision — the old fail-closed
	// case) must NOT exclude either.
	lk := compilerLatchKeyFor(accs[0])
	require.True(t, s.TryLatch(accs[0].ID, lk.Fingerprint, 99))
	view2, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices})
	require.NoError(t, err)
	rd2, ok := view2.routes[RouteRefFor(10, string(domain.FormatOpenAIChat), "m")]
	require.True(t, ok)
	all2 := append(append([]int64{}, compiledAccountIDs(rd2.Primary)...), compiledAccountIDs(rd2.Explore.Ordered)...)
	all2 = append(all2, compiledAccountIDs(rd2.Degraded)...)
	require.Contains(t, all2, int64(1), "latched account stays compiled post-v5")
	require.Contains(t, all2, int64(2))
}

func TestRoutingCompilerZeroMultiplierUnion(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	// zero cost-multiplier accounts must still be included in the union
	a1 := accWithEnabled(1, tpl, true, 0)
	a2 := accWithEnabled(2, tpl, true, 0)
	a3 := accWithEnabled(3, tpl, true, 10000)
	accs := []*domain.Account{a1, a2, a3}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	price := domain.ResolvedPrices{InputPerM: pricePtr(1000)}
	prices := map[string]domain.ResolvedPrices{"m": price}
	q := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{
		1: qualityInput(10, 5, 100, 100),
		2: qualityInput(10, 5, 100, 100),
		3: qualityInput(10, 5, 100, 100),
	}, accs)
	c := NewRoutingCompiler()
	view, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices})
	require.NoError(t, err)
	rd, ok := view.routes[RouteRefFor(10, string(domain.FormatOpenAIChat), "m")]
	require.True(t, ok)
	all := append(append([]int64{}, compiledAccountIDs(rd.Primary)...), compiledAccountIDs(rd.Explore.Ordered)...)
	all = append(all, compiledAccountIDs(rd.Degraded)...)
	require.ElementsMatch(t, []int64{1, 2, 3}, all, "zero-multiplier accounts must be in union")
}

func TestRoutingCompilerOverflowUnionNotTruncated(t *testing.T) {
	// Simulate overflow: many accounts; the compiled union must still be complete.
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	var accs []*domain.Account
	for i := 1; i <= 10; i++ {
		accs = append(accs, accWithEnabled(int64(i), tpl, true, 10000))
	}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	price := domain.ResolvedPrices{InputPerM: pricePtr(1000)}
	prices := map[string]domain.ResolvedPrices{"m": price}
	raw := make(map[int64]CandidateQualityInput)
	for i := 1; i <= 10; i++ {
		raw[int64(i)] = qualityInput(10, 5, 100, 100)
	}
	q := buildQuality(10, domain.FormatOpenAIChat, "m", raw, accs)
	c := NewRoutingCompiler()
	view, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices})
	require.NoError(t, err)
	rd, ok := view.routes[RouteRefFor(10, string(domain.FormatOpenAIChat), "m")]
	require.True(t, ok)
	all := append(append([]int64{}, compiledAccountIDs(rd.Primary)...), compiledAccountIDs(rd.Explore.Ordered)...)
	all = append(all, compiledAccountIDs(rd.Degraded)...)
	require.Len(t, all, 10)
	require.ElementsMatch(t, []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, all)
}

func TestRoutingCompilerImmutabilityDeep(t *testing.T) {
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
	// mutate underlying slices/maps should not affect other views
	primaryCopy := compiledAccountIDs(view.routes[rr].Primary)
	view.routes[rr].Primary = append(view.routes[rr].Primary, CompiledCandidate{AccountID: 999})
	view.routes[rr].Explore.Weights[999] = 100
	view.routes[rr].Explore.Ordered = append(view.routes[rr].Explore.Ordered, CompiledCandidate{AccountID: 999})
	view2, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices})
	require.NoError(t, err)
	require.Equal(t, primaryCopy, compiledAccountIDs(view2.routes[rr].Primary))
	require.NotContains(t, compiledAccountIDs(view2.routes[rr].Explore.Ordered), int64(999))
	require.NotContains(t, compiledAccountIDs(view2.routes[rr].Primary), int64(999))
}

func TestRoutingCompilerRouteRefCollisions(t *testing.T) {
	// Responses vs WS with same model "m" must be distinct RouteRefs
	tpl := &domain.Template{SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses, domain.FormatOpenAIResponsesWS}, Models: []string{"m"}}
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	c := NewRoutingCompiler()
	view, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Prices: map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}}})
	require.NoError(t, err)
	rrResp := RouteRefFor(10, string(domain.FormatOpenAIResponses), "m")
	rrWS := RouteRefFor(10, string(domain.FormatOpenAIResponsesWS), "m")
	require.NotEqual(t, rrResp.OperationTag, rrWS.OperationTag)
	respDec, ok1 := view.routes[rrResp]
	wsDec, ok2 := view.routes[rrWS]
	require.True(t, ok1)
	require.True(t, ok2)
	// v4-S2: op-distinctness lives in the interned per-route hex, not the normalized key.
	require.NotEmpty(t, respDec.RouteClassID)
	require.NotEmpty(t, wsDec.RouteClassID)
	require.NotEqual(t, respDec.RouteClassID, wsDec.RouteClassID)
	// Images generations vs edits also distinct
	rrGen := RouteRef{GroupID: 10, Format: string(domain.FormatOpenAIImages), Model: "m", OperationTag: string(domain.OpImagesGenerations)}
	rcGen, _ := domain.RouteClassID(10, domain.FormatOpenAIImages, "m", domain.OpImagesGenerations)
	rrGen.RouteClassID = domain.RouteClassIDHex(rcGen)
	rrEdit := RouteRef{GroupID: 10, Format: string(domain.FormatOpenAIImages), Model: "m", OperationTag: string(domain.OpImagesEdits)}
	rcEdit, _ := domain.RouteClassID(10, domain.FormatOpenAIImages, "m", domain.OpImagesEdits)
	rrEdit.RouteClassID = domain.RouteClassIDHex(rcEdit)
	require.NotEqual(t, rrGen.RouteClassID, rrEdit.RouteClassID)
}
