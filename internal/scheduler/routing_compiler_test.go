// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"math"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/latch"
)

func accWithEnabled(id int64, tpl *domain.Template, enabled bool, mult int) *domain.Account {
	a := acc(id, tpl, 10)
	a.Enabled = enabled
	a.UpstreamCostMultiplierBp = mult
	a.LifecycleRevision = 1
	return a
}

func qualityInput(attempts, successes int, inTok, outTok int64) CandidateQualityInput {
	var cnt Counts
	cnt.Attempts = attempts
	cnt.Successes = successes
	if successes >= 30 {
		logged := math.Log(100)
		cnt.TTFTCount = successes
		cnt.SumLog = logged * float64(successes)
		cnt.SumSq = logged * logged * float64(successes)
	}
	return CandidateQualityInput{Counts: cnt, InputTokens: inTok, OutputTokens: outTok}
}

func pricePtr(v int64) *int64 { return &v }

func qualityKeyFor(gid int64, format domain.RequestFormat, model string, acc *domain.Account) CandidateQualityKey {
	op := operationTagForFormat(string(format))
	rc, _ := domain.RouteClassID(gid, format, model, op)
	fpStr, err := candidateFingerprint(acc)
	var fpVal domain.CandidateFingerprintVal
	if err == nil && fpStr != "" {
		if v, err := domain.HexToID(fpStr); err == nil {
			fpVal = domain.CandidateFingerprintVal(v)
		}
	} else {
		// fallback distinct per account when template lacks baseURL (test helper): encode accountID
		var b [32]byte
		b[0] = byte(acc.ID >> 56)
		b[1] = byte(acc.ID >> 48)
		b[2] = byte(acc.ID >> 40)
		b[3] = byte(acc.ID >> 32)
		b[4] = byte(acc.ID >> 24)
		b[5] = byte(acc.ID >> 16)
		b[6] = byte(acc.ID >> 8)
		b[7] = byte(acc.ID)
		fpVal = domain.CandidateFingerprintVal(b)
	}
	return CandidateQualityKey{RouteClassID: rc, Fingerprint: fpVal}
}

func buildQuality(gid int64, format domain.RequestFormat, model string, m map[int64]CandidateQualityInput, accs []*domain.Account) map[CandidateQualityKey]CandidateQualityInput {
	out := make(map[CandidateQualityKey]CandidateQualityInput, len(m))
	byID := make(map[int64]*domain.Account, len(accs))
	for _, a := range accs {
		byID[a.ID] = a
	}
	for id, q := range m {
		if a, ok := byID[id]; ok {
			out[qualityKeyFor(gid, format, model, a)] = q
		}
	}
	return out
}

func compilerHealthKeyFor(acc *domain.Account, format domain.RequestFormat, model string) HealthKey {
	resolved := model
	if acc.Template != nil {
		if mapping, ok := acc.Template.ModelMapping[model]; ok {
			resolved = mapping.MappedModel
		}
	}
	return HealthKey{
		AccountID:        acc.ID,
		Quality:          qualityClassHexForWithOp(format, resolved, operationTagForFormat(string(format))),
		IdentityRevision: acc.IdentityRevision,
	}
}

func compilerLatchKeyFor(acc *domain.Account) latch.LatchKey {
	fp, _ := candidateFingerprint(acc)
	return latch.LatchKey{AccountID: acc.ID, Fingerprint: fp, Revision: acc.LifecycleRevision}
}

func TestRoutingCompilerDeterministicMapOrder(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	price := domain.ResolvedPrices{InputPerM: pricePtr(1000), OutputPerM: pricePtr(1000)}
	m1 := newMemLoader(map[int64][]*domain.Account{10: {accWithEnabled(2, tpl, true, 10000), accWithEnabled(1, tpl, true, 10000)}})
	s1 := newSched(t, m1)
	m2 := newMemLoader(map[int64][]*domain.Account{10: {accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000)}})
	s2 := newSched(t, m2)
	accs1 := m1.byGroup[10]
	accs2 := m2.byGroup[10]
	q1raw := map[int64]CandidateQualityInput{2: qualityInput(30, 29, 1000, 1000), 1: qualityInput(30, 29, 1000, 1000)}
	q2raw := map[int64]CandidateQualityInput{1: qualityInput(30, 29, 1000, 1000), 2: qualityInput(30, 29, 1000, 1000)}
	q1 := buildQuality(10, domain.FormatOpenAIChat, "m", q1raw, accs1)
	q2 := buildQuality(10, domain.FormatOpenAIChat, "m", q2raw, accs2)
	prices := map[string]domain.ResolvedPrices{"m": price}
	c := NewRoutingCompiler()
	v1, err := c.Compile(CompilerInputs{Static: s1.View().StaticView(), Quality: q1, Prices: prices})
	require.NoError(t, err)
	v2, err := c.Compile(CompilerInputs{Static: s2.View().StaticView(), Quality: q2, Prices: prices})
	require.NoError(t, err)
	rr := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	d1, ok1 := v1.routes[rr]
	d2, ok2 := v2.routes[rr]
	require.True(t, ok1)
	require.True(t, ok2)
	require.Equal(t, d1.Primary, d2.Primary)
	require.Equal(t, compiledAccountIDs(d1.Explore.Ordered), compiledAccountIDs(d2.Explore.Ordered))
	require.Equal(t, d1.Explore.Cumulative, d2.Explore.Cumulative)
	require.Equal(t, d1.Degraded, d2.Degraded)
}

func TestRoutingCompilerLaneClassificationAndWeightsTail(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	price := domain.ResolvedPrices{InputPerM: pricePtr(1000), OutputPerM: pricePtr(1000)}
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000), accWithEnabled(3, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	logged := math.Log(100)
	loggedSlow := math.Log(500)
	qraw := map[int64]CandidateQualityInput{
		1: {Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2900, OutputTokens: 2900},
		2: {Counts: Counts{Attempts: 30, Successes: 15, TTFTCount: 30, SumLog: loggedSlow * 30, SumSq: loggedSlow * loggedSlow * 30}, InputTokens: 1500, OutputTokens: 1500},
		3: {Counts: Counts{Attempts: 10, Successes: 5, TTFTCount: 5}, InputTokens: 500, OutputTokens: 500},
	}
	q := buildQuality(10, domain.FormatOpenAIChat, "m", qraw, accs)
	prices := map[string]domain.ResolvedPrices{"m": price}
	c := NewRoutingCompiler()
	view, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices})
	require.NoError(t, err)
	rd, ok := view.routes[RouteRefFor(10, string(domain.FormatOpenAIChat), "m")]
	require.True(t, ok)
	all := append(append([]int64{}, compiledAccountIDs(rd.Primary)...), compiledAccountIDs(rd.Explore.Ordered)...)
	all = append(all, compiledAccountIDs(rd.Degraded)...)
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	require.Equal(t, []int64{1, 2, 3}, all)
	require.Len(t, rd.Primary, 1)
	require.Contains(t, compiledAccountIDs(rd.Primary), int64(1))
	require.Len(t, rd.Degraded, 1)
	require.Contains(t, compiledAccountIDs(rd.Degraded), int64(2))
	require.Len(t, rd.Explore.Ordered, 1)
	require.Contains(t, compiledAccountIDs(rd.Explore.Ordered), int64(3))
	require.NotEmpty(t, rd.Explore.Cumulative)
	require.Equal(t, len(rd.Explore.Ordered), len(rd.Explore.Cumulative))
	require.NotZero(t, rd.Explore.Total)
	require.Equal(t, compiledAccountIDs(rd.Explore.Ordered), fallbackIDs(rd.Explore))
	for i := 1; i < len(rd.Explore.Cumulative); i++ {
		require.Greater(t, rd.Explore.Cumulative[i], rd.Explore.Cumulative[i-1])
	}
}

func TestRoutingCompilerCostTieBreakDeterministic(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	price := domain.ResolvedPrices{InputPerM: pricePtr(1000), OutputPerM: pricePtr(1000)}
	accs := []*domain.Account{accWithEnabled(3, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000), accWithEnabled(1, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	logged := math.Log(100)
	qraw := map[int64]CandidateQualityInput{
		1: {Counts: Counts{Attempts: 30, Successes: 28, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2800, OutputTokens: 2800},
		2: {Counts: Counts{Attempts: 30, Successes: 28, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2800, OutputTokens: 2800},
		3: {Counts: Counts{Attempts: 30, Successes: 28, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2800, OutputTokens: 2800},
	}
	q := buildQuality(10, domain.FormatOpenAIChat, "m", qraw, accs)
	prices := map[string]domain.ResolvedPrices{"m": price}
	c := NewRoutingCompiler()
	view, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices})
	require.NoError(t, err)
	rd, ok := view.routes[RouteRefFor(10, string(domain.FormatOpenAIChat), "m")]
	require.True(t, ok)
	require.Equal(t, []int64{1, 2, 3}, compiledAccountIDs(rd.Primary))
}

func TestRoutingCompilerMissingPriceUnknown(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	qraw := map[int64]CandidateQualityInput{
		1: {Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 29, SumLog: math.Log(100) * 29, SumSq: math.Log(100) * math.Log(100) * 29}, InputTokens: 2900, OutputTokens: 2900},
		2: {Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 29, SumLog: math.Log(100) * 29, SumSq: math.Log(100) * math.Log(100) * 29}, InputTokens: 2900, OutputTokens: 2900},
	}
	q := buildQuality(10, domain.FormatOpenAIChat, "m", qraw, accs)
	prices := map[string]domain.ResolvedPrices{"other": {InputPerM: pricePtr(1000)}}
	c := NewRoutingCompiler()
	view, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices})
	require.NoError(t, err)
	rd, ok := view.routes[RouteRefFor(10, string(domain.FormatOpenAIChat), "m")]
	require.True(t, ok)
	require.Empty(t, rd.Primary)
	require.Empty(t, rd.Degraded)
	require.Len(t, rd.Explore.Ordered, 2)
}

func TestRoutingCompilerInvalidInputs(t *testing.T) {
	c := NewRoutingCompiler()
	view, err := c.Compile(CompilerInputs{Static: nil})
	require.NoError(t, err)
	require.NotNil(t, view)
	require.Empty(t, view.routes)
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	a := accWithEnabled(1, tpl, true, -5)
	m := newMemLoader(map[int64][]*domain.Account{10: {a}})
	s := newSched(t, m)
	qraw := map[int64]CandidateQualityInput{1: {Counts: Counts{Attempts: -1, Successes: -1}}}
	q := buildQuality(10, domain.FormatOpenAIChat, "m", qraw, []*domain.Account{a})
	prices := map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}}
	view, err = c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices})
	require.NoError(t, err)
	rd, ok := view.routes[RouteRefFor(10, string(domain.FormatOpenAIChat), "m")]
	require.True(t, ok)
	require.NotEmpty(t, rd.Explore.Ordered)
}

func TestRoutingCompilerImmutability(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000)}
	price := domain.ResolvedPrices{InputPerM: pricePtr(1000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	qraw := map[int64]CandidateQualityInput{
		1: {Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 30, SumLog: math.Log(100) * 30, SumSq: math.Log(100) * math.Log(100) * 30}, InputTokens: 100, OutputTokens: 100},
		2: {Counts: Counts{Attempts: 10, Successes: 5}, InputTokens: 50, OutputTokens: 50},
	}
	q := buildQuality(10, domain.FormatOpenAIChat, "m", qraw, accs)
	prices := map[string]domain.ResolvedPrices{"m": price}
	c := NewRoutingCompiler()
	view, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices})
	require.NoError(t, err)
	rr := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	orig := append([]int64(nil), compiledAccountIDs(view.routes[rr].Primary)...)
	q[qualityKeyFor(10, domain.FormatOpenAIChat, "m", accs[0])] = CandidateQualityInput{Counts: Counts{Attempts: 100, Successes: 100}}
	prices["m"] = domain.ResolvedPrices{InputPerM: pricePtr(9999)}
	require.Equal(t, orig, compiledAccountIDs(view.routes[rr].Primary))
	if len(orig) > 0 {
		view.routes[rr].Primary[0] = CompiledCandidate{AccountID: 999}
	}
	view2, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{
		1: {Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 30, SumLog: math.Log(100) * 30, SumSq: math.Log(100) * math.Log(100) * 30}, InputTokens: 100, OutputTokens: 100},
		2: {Counts: Counts{Attempts: 10, Successes: 5}, InputTokens: 50, OutputTokens: 50},
	}, accs), Prices: map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}}})
	require.NoError(t, err)
	require.NotEqual(t, view.routes[rr].Primary, view2.routes[rr].Primary)
}
