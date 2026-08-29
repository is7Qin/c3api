// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"math"
	"math/bits"
	"sort"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
)

// CandidateQualityInput is per-candidate quality/usage snapshot for current window [M-5m, now).
// Downstream dependency: when per-RouteClass segregated quality rollups are queried,
// quality must be keyed by (RouteClassID, CandidateFingerprint) not just AccountID.
// Current minimal typed input is account-scoped; upgrade path is to add RouteClassID to key.
type CandidateQualityInput struct {
	Counts            Counts
	InputTokens       int64
	OutputTokens      int64
	CacheReadTokens   int64
	CacheCreateTokens int64
}

// CompilerInputs is immutable loaded inputs for deterministic compilation.
type CompilerInputs struct {
	Static  *StaticView
	Quality map[int64]CandidateQualityInput // accountID -> quality
	Prices  map[string]domain.ResolvedPrices // model -> resolved prices
	Health  map[int64]HealthState            // accountID -> effective health, missing=READY
	Latched map[int64]bool                   // accountID latched
}

// RoutingCompiler is stateless deterministic compiler, separate from publisher.
type RoutingCompiler struct{}

// NewRoutingCompiler creates compiler.
func NewRoutingCompiler() *RoutingCompiler { return &RoutingCompiler{} }

// Compile produces immutable DecisionView. Input maps not retained. Deterministic for same inputs.
func (c *RoutingCompiler) Compile(in CompilerInputs) (*DecisionView, error) {
	if in.Static == nil {
		return &DecisionView{routes: map[RouteRef]*RouteDecision{}}, nil
	}
	// Defensive copy of static groups iteration sorted for determinism
	groupIDs := make([]int64, 0, len(in.Static.groups))
	for gid := range in.Static.groups {
		groupIDs = append(groupIDs, gid)
	}
	sort.Slice(groupIDs, func(i, j int) bool { return groupIDs[i] < groupIDs[j] })

	routes := make(map[RouteRef]*RouteDecision)

	// Pre-sort prices keys for deterministic? Prices map lookup is deterministic regardless of order.

	for _, gid := range groupIDs {
		gs := in.Static.groups[gid]
		if gs == nil || gs.routes == nil {
			continue
		}
		// Collect sorted routeKeys for determinism
		routeKeys := make([]routeKey, 0, len(gs.routes))
		for rk := range gs.routes {
			routeKeys = append(routeKeys, rk)
		}
		sort.Slice(routeKeys, func(i, j int) bool {
			if string(routeKeys[i].format) != string(routeKeys[j].format) {
				return string(routeKeys[i].format) < string(routeKeys[j].format)
			}
			return routeKeys[i].model < routeKeys[j].model
		})
		for _, rk := range routeKeys {
			route := gs.routes[rk]
			if route == nil {
				continue
			}
			// Build unique candidate union from both tiers
			seen := make(map[int64]*accountSnapshot)
			if route.tier1 != nil {
				for _, a := range route.tier1.seq {
					if a == nil {
						continue
					}
					id := a.static.Load().acc.ID
					seen[id] = a
				}
			}
			if route.tier2 != nil {
				for _, a := range route.tier2.seq {
					if a == nil {
						continue
					}
					id := a.static.Load().acc.ID
					seen[id] = a
				}
			}
			// Also include accounts list to ensure union complete when seq empty due to no weight? fallback to gs.accounts filtered by format/model?
			// Ensure complete union even if weightedSeq empty: iterate gs.accounts and check Serves/FormatSupports
			// For determinism we already have union from seq; but seq may be empty for some routes. Use gs.accounts as fallback.
			if len(seen) == 0 {
				for _, a := range gs.accounts {
					if a == nil {
						continue
					}
					tpl := a.static.Load().tpl
					if tpl == nil {
						continue
					}
					model := rk.model
					format := rk.format
					supports := false
					if model == "" {
						// default bucket: only all-model accounts already in check, but we treat as format supports
						if tpl.HasModelSpace() {
							continue
						}
						supports = tplSupportsFormat(tpl, format)
					} else {
						if !tpl.FormatSupports(format, model) {
							continue
						}
						if tpl.Serves(model) {
							supports = true
						} else if !tpl.HasModelSpace() {
							supports = true
						}
					}
					if supports {
						seen[a.static.Load().acc.ID] = a
					}
				}
			}

			// Filter by enabled/health/latch exclusion (fencing)
			filtered := make([]*accountSnapshot, 0, len(seen))
			idsSorted := make([]int64, 0, len(seen))
			for id := range seen {
				idsSorted = append(idsSorted, id)
			}
			sort.Slice(idsSorted, func(i, j int) bool { return idsSorted[i] < idsSorted[j] })
			for _, id := range idsSorted {
				a := seen[id]
				av := a.static.Load()
				// enabled fencing: disabled accounts excluded
				if !av.acc.Enabled {
					// Need to check lifecycle: disabled via Enabled false; but legacy StatusDisabled also uses runtime? Use Enabled.
					// If Enabled false, exclude.
					continue
				}
				// also check FailedAt? keep enabled as source.
				if av.acc.LifecycleRevision < 0 {
					continue
				}
				if in.Latched != nil && in.Latched[id] {
					continue
				}
				if in.Health != nil {
					if st, ok := in.Health[id]; ok && st != StateReady {
						continue
					}
				}
				filtered = append(filtered, a)
			}

			if len(filtered) == 0 {
				rr := RouteRef{GroupID: gid, Format: string(rk.format), Model: rk.model}
				routes[rr] = &RouteDecision{
					Explore: ExploreDecision{Weights: map[int64]int{}, Fallback: []int64{}},
				}
				continue
			}

			// Build QualityCandidate list with cost
			qcs := make([]QualityCandidate, 0, len(filtered))
			costKnown := make(map[int64]bool, len(filtered))
			// For Explore weight need successes/attempts
			for _, a := range filtered {
				id := a.static.Load().acc.ID
				qin, hasQ := in.Quality[id]
				var cnt Counts
				var inTok, outTok, crTok, ccTok int64
				if hasQ {
					cnt = qin.Counts
					inTok = qin.InputTokens
					outTok = qin.OutputTokens
					crTok = qin.CacheReadTokens
					ccTok = qin.CacheCreateTokens
				} else {
					cnt = Counts{Attempts: 0, Successes: 0, TTFTCount: 0}
				}
				// Cost computation: if no successes -> unknown, missing price -> unknown
				price, hasPrice := in.Prices[rk.model]
				// default bucket model "" likely no price -> unknown
				known := hasPrice && cnt.Successes > 0
				var cost int64
				if known {
					// round half up: (sum + successes/2)/successes
					avgIn := avgTokens(inTok, int64(cnt.Successes))
					avgOut := avgTokens(outTok, int64(cnt.Successes))
					avgCr := avgTokens(crTok, int64(cnt.Successes))
					avgCc := avgTokens(ccTok, int64(cnt.Successes))
					raw := billing.CostFromResolved(price, avgIn, avgOut, avgCr, avgCc)
					mult := a.static.Load().acc.UpstreamCostMultiplierBp
					if mult < 0 {
						mult = 0
					}
					if mult > 100000 {
						mult = 100000
					}
					// checked saturating: raw * mult / 10000
					cost = saturatingMulDiv(raw, int64(mult), 10000)
					// Clamp negative?
					if cost < 0 {
						cost = 0
					}
				} else {
					cost = math.MaxInt64
					known = false
				}
				costKnown[id] = known

				qc := QualityCandidate{
					AccountID: id,
					Successes: cnt.Successes,
					Attempts:  cnt.Attempts,
					SumLog:    cnt.SumLog,
					SumSq:     cnt.SumSq,
					TTFTCount: cnt.TTFTCount,
					Cost:      cost,
				}
				qcs = append(qcs, qc)
			}

			// Deterministic sort of input for classify: sort by accountID before classify? ClassifyQuality expects capacity but not order; but for determinism we sort qcs by accountID before classify to ensure same workspace? However classify internal picks bestLCB independent of order, but primary sorting after is deterministic. To ensure map-order independence, we sort qcs by accountID first.
			sort.Slice(qcs, func(i, j int) bool { return qcs[i].AccountID < qcs[j].AccountID })

			ws := NewQualityWorkspace(len(qcs))
			cl := NewQualityClassification(len(qcs))
			if err := ClassifyQuality(qcs, &ws, &cl); err != nil {
				// On error, treat all as explore
				cl = QualityClassification{}
				for _, qc := range qcs {
					cl.Explore = append(cl.Explore, qc)
				}
				sort.Slice(cl.Explore, func(i, j int) bool { return cl.Explore[i].AccountID < cl.Explore[j].AccountID })
			}

			// Move cost-unknown Primary/Degraded to Explore
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
			// Re-sort Explore by accountID for determinism before weight table
			sort.Slice(cl.Explore, func(i, j int) bool { return cl.Explore[i].AccountID < cl.Explore[j].AccountID })

			// Build Explore weights and fallback
			exploreIDs := make([]int64, 0, len(cl.Explore))
			for _, qc := range cl.Explore {
				exploreIDs = append(exploreIDs, qc.AccountID)
			}
			// explore weights
			weights := make(map[int64]int, len(exploreIDs))
			var exploreCandidates []ExploreCandidate
			for _, qc := range cl.Explore {
				w, err := ExploreWeight(qc.Successes, qc.Attempts)
				if err != nil {
					w = 100
				}
				weights[qc.AccountID] = w
				exploreCandidates = append(exploreCandidates, ExploreCandidate{AccountID: qc.AccountID, Weight: w})
			}
			// Build ExploreTable for cumulative
			var cumulative []uint64
			var total uint64
			if len(exploreCandidates) > 0 {
				tab := NewExploreTable(len(exploreCandidates))
				if err := tab.Build(exploreCandidates); err == nil {
					cumulative = append([]uint64(nil), tab.Cumulative()...)
					total = tab.Total()
					// IDs already sorted by accountID via Build internal sort; extract order
					exploreIDs = exploreIDs[:0]
					// tab.candidates sorted by accountID after Build
					// We need to reflect that order; rebuild exploreIDs from tab's candidates ordering
					// tab.candidates is private; instead we sort exploreIDs and weights correspond; cumulative already in accountID order due to Build sorting.
					// So reconstruct IDs sorted
					sortedIDs := make([]int64, len(exploreCandidates))
					for i, ec := range exploreCandidates {
						_ = ec
						sortedIDs[i] = exploreCandidates[i].AccountID
					}
					// exploreCandidates was built in accountID order (cl.Explore sorted), and Build sorts again by accountID (stable), so order preserved
					// Just keep exploreIDs as sorted
					idsCopy := make([]int64, len(cl.Explore))
					for i, qc := range cl.Explore {
						idsCopy[i] = qc.AccountID
					}
					sort.Slice(idsCopy, func(i, j int) bool { return idsCopy[i] < idsCopy[j] })
					exploreIDs = idsCopy
				} else {
					// fallback: manual cumulative
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

			// Fallback tail: sorted by posterior mean desc etc.
			fallbackCandidates := make([]FallbackCandidate, 0, len(cl.Explore))
			for _, qc := range cl.Explore {
				fallbackCandidates = append(fallbackCandidates, FallbackCandidate{AccountID: qc.AccountID, Successes: qc.Successes, Attempts: qc.Attempts})
			}
			// Sort via FallbackTail logic without selection dedupe: we want full sorted order
			var fallbackIDs []int64
			if len(fallbackCandidates) > 0 {
				// Use same comparator as fallback: posterior mean desc, attempts asc, accountID
				// Use FallbackTail with no selected to get sorted order
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

			// Build primary/degraded ID slices already sorted via ClassifyQuality (cost ordering etc)
			primaryIDs := make([]int64, len(cl.Primary))
			for i, qc := range cl.Primary {
				primaryIDs[i] = qc.AccountID
			}
			degradedIDs := make([]int64, len(cl.Degraded))
			for i, qc := range cl.Degraded {
				degradedIDs[i] = qc.AccountID
			}

			// Union verification: all IDs should be unique and count == filtered length
			// Primary+Explore+Degraded already partition filtered set (moved cost unknown)
			rr := RouteRef{GroupID: gid, Format: string(rk.format), Model: rk.model}
			routes[rr] = &RouteDecision{
				Primary:  primaryIDs,
				Degraded: degradedIDs,
				Explore: ExploreDecision{
					IDs:        exploreIDs,
					Weights:    weights,
					Cumulative: cumulative,
					Total:      total,
					Fallback:   fallbackIDs,
				},
			}
		}
	}

	// Deep copy routes for immutability: already new maps/slices, ensure no alias to input
	view := &DecisionView{
		routes: routes,
		// decisions kept nil for new compiler path
	}
	return view, nil
}

func avgTokens(sum int64, successes int64) int64 {
	if successes <= 0 {
		return 0
	}
	// round half up: (sum + successes/2)/successes
	// checked: sum may be large, successes up to large, use int64 with overflow guard?
	// saturating addition
	if sum > math.MaxInt64-successes/2 {
		return sum / successes // fallback without rounding
	}
	return (sum + successes/2) / successes
}

func saturatingMulDiv(a, b, divisor int64) int64 {
	if divisor == 0 {
		return math.MaxInt64
	}
	// a * b may overflow int64, use 128-bit via manual?
	// Use bits.Mul64 for unsigned, but a,b signed >=0
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

func mul64(x, y uint64) (hi, lo uint64) {
	hi, lo = bits.Mul64(x, y)
	return hi, lo
}

func tplSupportsFormat(tpl *domain.Template, format domain.RequestFormat) bool {
	for _, f := range tpl.SupportedFormats {
		if f == format {
			return true
		}
	}
	return false
}
