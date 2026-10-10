// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"fmt"
	"math"
	"sort"

	"github.com/is7qin/c3api/internal/domain"
)

func compileRouteDecision(filtered []compilerCandidateFacts, rk routeKey, routeRC domain.RouteClassIDVal, quality map[CandidateQualityKey]CandidateQualityInput, prices map[string]domain.ResolvedPrices, rr RouteRef) (*RouteDecision, error) {
	qcs, costKnown := compileRouteQualityCandidates(filtered, routeRC, quality, prices, rk)
	cl := classifyRouteCandidates(qcs, costKnown)
	exploreBP, weights, cumulative, total, exploreIDs := compileExplorePlan(len(filtered), cl)
	fallbackIDs := compileFallbackIDs(cl.Explore, exploreIDs)
	primaryIDs, degradedIDs := compileLaneIDs(cl.Primary, cl.Degraded)
	return assembleRouteDecision(filtered, rk, rr, primaryIDs, degradedIDs, exploreIDs, fallbackIDs, weights, cumulative, total, exploreBP)
}

// compileRouteQualityCandidates maps every filtered candidate fact to a
// QualityCandidate, resolving quality window counts and purchase cost (with the
// upstream multiplier clamp and MaxInt64/zero fallbacks). costKnown[id] records
// whether the cost came from a real priced sample.
func compileRouteQualityCandidates(filtered []compilerCandidateFacts, routeRC domain.RouteClassIDVal, quality map[CandidateQualityKey]CandidateQualityInput, prices map[string]domain.ResolvedPrices, rk routeKey) ([]QualityCandidate, map[int64]bool) {
	qcs := make([]QualityCandidate, 0, len(filtered))
	costKnown := make(map[int64]bool, len(filtered))
	for _, facts := range filtered {
		id := facts.accountID
		key := CandidateQualityKey{RouteClassID: routeRC, Fingerprint: facts.identityFingerprint}
		qin, hasQ := quality[key]
		var cnt Counts
		var inTok, crTok int64
		if hasQ {
			cnt = qin.Counts
			inTok = qin.InputTokens
			crTok = qin.CacheReadTokens
		}
		price, hasPrice := prices[rk.model]
		known := hasPrice && cnt.Successes > 0
		var cost int64
		if known {
			billable := inTok
			if billable < 0 {
				billable = 0
			}
			cached := crTok
			if cached < 0 {
				cached = 0
			}
			denom := billable + cached
			if denom == 0 {
				cost = math.MaxInt64
				known = false
			} else {
				mult := facts.upstreamCostMultiplierBp
				if mult < 0 {
					mult = 0
				}
				if mult > 100000 {
					mult = 100000
				}
				cost = InputUnitPurchaseCost(uint64(mult), uint64(billable), uint64(cached), NonNegPrice(price.InputPerM), NonNegPrice(price.CacheReadPerM), uint64(denom))
				if cost < 0 {
					cost = 0
				}
			}
		} else {
			cost = math.MaxInt64
			known = false
		}
		costKnown[id] = known
		qcs = append(qcs, QualityCandidate{AccountID: id, Successes: cnt.Successes, Attempts: cnt.Attempts, SumLog: cnt.SumLog, SumSq: cnt.SumSq, TTFTCount: cnt.TTFTCount, Cost: cost})
	}
	return qcs, costKnown
}

// classifyRouteCandidates runs the deterministic quality classification and
// demotes any cost-unknown primary/degraded candidate into the explore set,
// preserving the accountID ordering at each sort boundary.
func classifyRouteCandidates(qcs []QualityCandidate, costKnown map[int64]bool) QualityClassification {
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
	return cl
}

// compileExplorePlan computes the steady-state exploration share and the explore
// weight/cumulative table. Steady-state share (charter): computed from the
// window inputs already read — eligible route candidates, unknown (low-sample,
// incl. cost-unknown demotees) and serving primaries.
// No Primary → 10000bp (all explore); unknown==0 → 100bp; cap 500bp.
func compileExplorePlan(filteredCount int, cl QualityClassification) (exploreBP int, weights map[int64]int, cumulative []uint64, total uint64, exploreIDs []int64) {
	exploreBP = ExploreBP(filteredCount, len(cl.Explore), len(cl.Primary))
	weights = make(map[int64]int, len(cl.Explore))
	var exploreCandidates []ExploreCandidate
	for _, qc := range cl.Explore {
		w, err := ExploreWeight(qc.Successes, qc.Attempts)
		if err != nil {
			w = 100
		}
		weights[qc.AccountID] = w
		exploreCandidates = append(exploreCandidates, ExploreCandidate{AccountID: qc.AccountID, Weight: w})
	}
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
	return exploreBP, weights, cumulative, total, exploreIDs
}

// compileFallbackIDs derives the ordered fallback account IDs from the explore
// set, falling back to the raw explore order if FallbackTail errors.
func compileFallbackIDs(explore []QualityCandidate, exploreIDs []int64) []int64 {
	fallbackCandidates := make([]FallbackCandidate, 0, len(explore))
	for _, qc := range explore {
		fallbackCandidates = append(fallbackCandidates, FallbackCandidate{AccountID: qc.AccountID, Successes: qc.Successes, Attempts: qc.Attempts})
	}
	if len(fallbackCandidates) > 0 {
		out, err := FallbackTail(fallbackCandidates, -1, make([]FallbackCandidate, 0, len(fallbackCandidates)))
		if err == nil {
			fallbackIDs := make([]int64, len(out))
			for i, fc := range out {
				fallbackIDs[i] = fc.AccountID
			}
			return fallbackIDs
		}
		return exploreIDs
	}
	return []int64{}
}

// compileLaneIDs extracts account IDs for the primary and degraded lanes.
func compileLaneIDs(primary, degraded []QualityCandidate) ([]int64, []int64) {
	var primaryIDs []int64
	if len(primary) > 0 {
		primaryIDs = make([]int64, len(primary))
		for i, qc := range primary {
			primaryIDs[i] = qc.AccountID
		}
	}
	var degradedIDs []int64
	if len(degraded) > 0 {
		degradedIDs = make([]int64, len(degraded))
		for i, qc := range degraded {
			degradedIDs[i] = qc.AccountID
		}
	}
	return primaryIDs, degradedIDs
}

// assembleRouteDecision normalizes empty explore slices to nil, builds the cache
// domain plan and compiled lanes, and assembles/validates the RouteDecision.
func assembleRouteDecision(filtered []compilerCandidateFacts, rk routeKey, rr RouteRef, primaryIDs, degradedIDs, exploreIDs, fallbackIDs []int64, weights map[int64]int, cumulative []uint64, total uint64, exploreBP int) (*RouteDecision, error) {
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
	ring, accounts, err := compileCacheDomainPlan(filtered)
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
		Explore:             ExploreDecision{Ordered: ordered, Weights: weights, Cumulative: cumulative, Total: total, Fallback: fallback, ExploreBP: exploreBP},
		CacheDomainRing:     ring,
		CacheDomainAccounts: accounts,
	}
	if err := validateRouteDecision(decision); err != nil {
		return nil, err
	}
	decision.validated = true
	return decision, nil
}

func compileCacheDomainPlan(filtered []compilerCandidateFacts) (CacheDomainRing, []CacheDomainAccount, error) {
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
	// ring.Domains is a fresh sorted slice owned by the ring; the caller
	// discarded the old defensive copy of it (only the ring + accounts are
	// consumed), so it is dropped here.
	return ring, accounts, err
}
