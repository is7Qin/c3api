// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"fmt"
	"math"
	"sort"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
)

func compileRouteDecision(filtered []compilerCandidateFacts, rk routeKey, routeRC domain.RouteClassIDVal, quality map[CandidateQualityKey]CandidateQualityInput, prices map[string]domain.ResolvedPrices, rr RouteRef) (*RouteDecision, error) {
	qcs := make([]QualityCandidate, 0, len(filtered))
	costKnown := make(map[int64]bool, len(filtered))
	for _, facts := range filtered {
		id := facts.accountID
		key := CandidateQualityKey{RouteClassID: routeRC, Fingerprint: facts.identityFingerprint}
		qin, hasQ := quality[key]
		var cnt Counts
		var inTok, outTok, crTok, ccTok int64
		if hasQ {
			cnt = qin.Counts
			inTok = qin.InputTokens
			outTok = qin.OutputTokens
			crTok = qin.CacheReadTokens
			ccTok = qin.CacheCreateTokens
		}
		price, hasPrice := prices[rk.model]
		known := hasPrice && cnt.Successes > 0
		var cost int64
		if known {
			avgIn := AvgTokens(inTok, int64(cnt.Successes))
			avgOut := AvgTokens(outTok, int64(cnt.Successes))
			avgCr := AvgTokens(crTok, int64(cnt.Successes))
			avgCc := AvgTokens(ccTok, int64(cnt.Successes))
			raw := billing.CostFromResolved(price, avgIn, avgOut, avgCr, avgCc)
			mult := facts.static.acc.UpstreamCostMultiplierBp
			if mult < 0 {
				mult = 0
			}
			if mult > 100000 {
				mult = 100000
			}
			cost = SaturatingMulDiv(raw, int64(mult), 10000)
			if cost < 0 {
				cost = 0
			}
		} else {
			cost = math.MaxInt64
			known = false
		}
		costKnown[id] = known
		qcs = append(qcs, QualityCandidate{AccountID: id, Successes: cnt.Successes, Attempts: cnt.Attempts, SumLog: cnt.SumLog, SumSq: cnt.SumSq, TTFTCount: cnt.TTFTCount, Cost: cost})
	}
	sort.Slice(qcs, func(i, j int) bool { return qcs[i].AccountID < qcs[j].AccountID })
	ws := NewQualityWorkspace(len(qcs))
	cl := NewQualityClassification(len(qcs))
	if err := ClassifyQuality(qcs, &ws, &cl); err != nil {
		cl = QualityClassification{}
		for _, qc := range qcs {
			cl.Explore = append(cl.Explore, qc)
		}
		sort.Slice(cl.Explore, func(i, j int) bool { return cl.Explore[i].AccountID < cl.Explore[j].AccountID })
	}
	var newPrimary []QualityCandidate
	var newDegraded []QualityCandidate
	for _, qc := range cl.Primary {
		if !costKnown[qc.AccountID] {
			cl.Explore = append(cl.Explore, qc)
		} else {
			newPrimary = append(newPrimary, qc)
		}
	}
	for _, qc := range cl.Degraded {
		if !costKnown[qc.AccountID] {
			cl.Explore = append(cl.Explore, qc)
		} else {
			newDegraded = append(newDegraded, qc)
		}
	}
	cl.Primary = newPrimary
	cl.Degraded = newDegraded
	sort.Slice(cl.Explore, func(i, j int) bool { return cl.Explore[i].AccountID < cl.Explore[j].AccountID })
	weights := make(map[int64]int, len(cl.Explore))
	var exploreCandidates []ExploreCandidate
	for _, qc := range cl.Explore {
		w, err := ExploreWeight(qc.Successes, qc.Attempts)
		if err != nil {
			w = 100
		}
		weights[qc.AccountID] = w
		exploreCandidates = append(exploreCandidates, ExploreCandidate{AccountID: qc.AccountID, Weight: w})
	}
	var cumulative []uint64
	var total uint64
	var exploreIDs []int64
	if len(exploreCandidates) > 0 {
		tab := NewExploreTable(len(exploreCandidates))
		if err := tab.Build(exploreCandidates); err == nil {
			cumulative = append([]uint64(nil), tab.Cumulative()...)
			total = tab.Total()
			exploreIDs = make([]int64, len(cl.Explore))
			for i, qc := range cl.Explore {
				exploreIDs[i] = qc.AccountID
			}
			sort.Slice(exploreIDs, func(i, j int) bool { return exploreIDs[i] < exploreIDs[j] })
		} else {
			sort.Slice(exploreCandidates, func(i, j int) bool { return exploreCandidates[i].AccountID < exploreCandidates[j].AccountID })
			cumulative = make([]uint64, len(exploreCandidates))
			var sum uint64
			for i, ec := range exploreCandidates {
				sum += uint64(ec.Weight)
				cumulative[i] = sum
			}
			total = sum
			exploreIDs = make([]int64, len(exploreCandidates))
			for i, ec := range exploreCandidates {
				exploreIDs[i] = ec.AccountID
			}
		}
	}
	fallbackCandidates := make([]FallbackCandidate, 0, len(cl.Explore))
	for _, qc := range cl.Explore {
		fallbackCandidates = append(fallbackCandidates, FallbackCandidate{AccountID: qc.AccountID, Successes: qc.Successes, Attempts: qc.Attempts})
	}
	var fallbackIDs []int64
	if len(fallbackCandidates) > 0 {
		out, err := FallbackTail(fallbackCandidates, -1, make([]FallbackCandidate, 0, len(fallbackCandidates)))
		if err == nil {
			fallbackIDs = make([]int64, len(out))
			for i, fc := range out {
				fallbackIDs[i] = fc.AccountID
			}
		} else {
			fallbackIDs = exploreIDs
		}
	} else {
		fallbackIDs = []int64{}
	}
	var primaryIDs []int64
	if len(cl.Primary) > 0 {
		primaryIDs = make([]int64, len(cl.Primary))
		for i, qc := range cl.Primary {
			primaryIDs[i] = qc.AccountID
		}
	}
	var degradedIDs []int64
	if len(cl.Degraded) > 0 {
		degradedIDs = make([]int64, len(cl.Degraded))
		for i, qc := range cl.Degraded {
			degradedIDs[i] = qc.AccountID
		}
	}
	if len(exploreIDs) == 0 {
		exploreIDs = nil
	}
	if len(fallbackIDs) == 0 {
		fallbackIDs = nil
	}
	if len(cumulative) == 0 {
		cumulative = nil
	}
	if len(weights) == 0 {
		weights = nil
	}
	ring, _, accounts, err := compileCacheDomainPlan(filtered)
	if err != nil {
		return nil, err
	}
	caller := string(callerKindForFormat(rk.format))
	op := domain.OperationTag(rr.OperationTag)
	byID := make(map[int64]compilerCandidateFacts, len(filtered))
	for _, facts := range filtered {
		byID[facts.accountID] = facts
	}
	mk := func(id int64, lane AttemptLane) CompiledCandidate {
		return compileCandidate(byID[id], lane)
	}
	primary := make([]CompiledCandidate, 0, len(primaryIDs))
	for _, id := range primaryIDs {
		primary = append(primary, mk(id, AttemptLanePrimary))
	}
	degraded := make([]CompiledCandidate, 0, len(degradedIDs))
	for _, id := range degradedIDs {
		degraded = append(degraded, mk(id, AttemptLaneDegraded))
	}
	ordered := make([]CompiledCandidate, 0, len(exploreIDs))
	for _, id := range exploreIDs {
		ordered = append(ordered, mk(id, AttemptLaneExplore))
	}
	if len(ordered) > int(^uint16(0)) {
		return nil, fmt.Errorf("explore candidate count %d exceeds uint16 index range", len(ordered))
	}
	orderedIndex := make(map[int64]uint16, len(ordered))
	for i, c := range ordered {
		orderedIndex[c.AccountID] = uint16(i)
	}
	fallback := make([]uint16, 0, len(fallbackIDs))
	for _, id := range fallbackIDs {
		if idx, ok := orderedIndex[id]; ok {
			fallback = append(fallback, idx)
		}
	}
	decision := &RouteDecision{
		Format: string(rk.format), RequestedModel: rk.model, RouteClassID: rr.RouteClassID, CallerCategory: caller, OperationTag: string(op),
		Primary: primary, Degraded: degraded,
		Explore:             ExploreDecision{Ordered: ordered, Weights: weights, Cumulative: cumulative, Total: total, Fallback: fallback},
		CacheDomainRing:     ring,
		CacheDomainAccounts: accounts,
	}
	if err := validateRouteDecision(decision); err != nil {
		return nil, err
	}
	decision.validated = true
	return decision, nil
}

func compileCacheDomainPlan(filtered []compilerCandidateFacts) (CacheDomainRing, []string, []CacheDomainAccount, error) {
	domains := make([]string, 0, len(filtered))
	accounts := make([]CacheDomainAccount, 0, len(filtered))
	seen := make(map[int64]struct{}, len(filtered))
	for _, facts := range filtered {
		if _, ok := seen[facts.accountID]; ok {
			continue
		}
		seen[facts.accountID] = struct{}{}
		domain := cacheDomainForAccount(facts.accountID, facts.static.acc.CacheDomain)
		domains = append(domains, domain)
		accounts = append(accounts, CacheDomainAccount{AccountID: facts.accountID, Domain: domain})
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].AccountID < accounts[j].AccountID })
	ring, err := buildCacheDomainRing(domains)
	return ring, append([]string(nil), ring.Domains...), accounts, err
}
