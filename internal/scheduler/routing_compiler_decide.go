// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"math"
	"math/bits"
	"sort"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
)

func compileRouteDecision(filtered []*accountSnapshot, rk routeKey, routeRC domain.RouteClassIDVal, quality map[CandidateQualityKey]CandidateQualityInput, prices map[string]domain.ResolvedPrices) *RouteDecision {
	qcs := make([]QualityCandidate, 0, len(filtered))
	costKnown := make(map[int64]bool, len(filtered))
	for _, a := range filtered {
		av := a.static.Load()
		id := av.acc.ID
		fp, ferr := candidateFingerprint(&av.acc)
		var fpVal domain.CandidateFingerprintVal
		if ferr == nil && fp != "" {
			if v, err := domain.HexToID(fp); err == nil {
				fpVal = domain.CandidateFingerprintVal(v)
			}
		} else {
			var b [32]byte
			b[0] = byte(id >> 56)
			b[1] = byte(id >> 48)
			b[2] = byte(id >> 40)
			b[3] = byte(id >> 32)
			b[4] = byte(id >> 24)
			b[5] = byte(id >> 16)
			b[6] = byte(id >> 8)
			b[7] = byte(id)
			fpVal = domain.CandidateFingerprintVal(b)
		}
		key := CandidateQualityKey{RouteClassID: routeRC, Fingerprint: fpVal}
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
			avgIn := avgTokens(inTok, int64(cnt.Successes))
			avgOut := avgTokens(outTok, int64(cnt.Successes))
			avgCr := avgTokens(crTok, int64(cnt.Successes))
			avgCc := avgTokens(ccTok, int64(cnt.Successes))
			raw := billing.CostFromResolved(price, avgIn, avgOut, avgCr, avgCc)
			mult := av.acc.UpstreamCostMultiplierBp
			if mult < 0 {
				mult = 0
			}
			if mult > 100000 {
				mult = 100000
			}
			cost = saturatingMulDiv(raw, int64(mult), 10000)
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
	return &RouteDecision{Primary: primaryIDs, Degraded: degradedIDs, Explore: ExploreDecision{IDs: exploreIDs, Weights: weights, Cumulative: cumulative, Total: total, Fallback: fallbackIDs}}
}

func avgTokens(sum int64, successes int64) int64 {
	if successes <= 0 {
		return 0
	}
	if sum > math.MaxInt64-successes/2 {
		return sum / successes
	}
	return (sum + successes/2) / successes
}

func saturatingMulDiv(a, b, divisor int64) int64 {
	if divisor == 0 {
		return math.MaxInt64
	}
	if a < 0 {
		a = 0
	}
	if b < 0 {
		b = 0
	}
	hi, lo := mul64(uint64(a), uint64(b))
	if hi != 0 {
		return math.MaxInt64
	}
	return int64(lo / uint64(divisor))
}

func mul64(x, y uint64) (hi, lo uint64) { hi, lo = bits.Mul64(x, y); return }
