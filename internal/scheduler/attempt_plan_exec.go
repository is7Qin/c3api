// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"fmt"
	"sort"

	"github.com/is7qin/c3api/internal/domain"
)

// AttemptPlan is the compact request-local session over one immutable
// RouteDecision. It owns only cursor, selected explore/affinity state,
// attempted identities and lifecycle bookkeeping; all candidate metadata lives
// in the published route. The initial success inspects one candidate; later
// candidates activate lazily after retry classification.
type AttemptPlan struct {
	identity       AttemptPlanIdentity
	route          *RouteDecision
	generation     uint64
	maxAttempts    uint8
	sampleIdx      int
	sampleValid    bool
	walkSeg        uint8
	walkPos        int
	affinitySet    bool
	affinityDom    string
	affinPhase     uint8
	total          int
	emitted        [MaxAttemptPlanAccounts]int64
	emittedCnt     uint8
	attempted      [MaxAttemptPlanAccounts]int64
	attemptIDs     [MaxAttemptPlanAccounts]string
	currentAttempt Attempt
	attemptedCnt   uint8
	ordinal        uint8
	// reservationStarted is monotonic execution state: set once after the
	// first successful reservation, never cleared (AbandonLastAttempt must
	// not reset it — attemptedCnt rewinds, execution history does not).
	// A decision-generation mismatch is tolerated only before execution
	// starts; afterwards the strict fence applies.
	reservationStarted bool
}

func normalizeMaxAttempts(n uint8) uint8 {
	if n == 0 || n > MaxAttemptPlanAccounts {
		return MaxAttemptPlanAccounts
	}
	return n
}

// NewAttemptPlan binds an identity to one compiled route: primary order, then
// the deterministic explore sample followed by the complete fallback tail,
// then degraded. No per-request slices/maps; the overflow tail is walked
// lazily and never truncated.
func NewAttemptPlan(identity AttemptPlanIdentity, decision *RouteDecision) (*AttemptPlan, error) {
	if decision == nil {
		return nil, &InvalidRouteDecisionError{Field: "decision", Index: -1}
	}
	if !decision.validated {
		if err := validateRouteDecision(decision); err != nil {
			return nil, err
		}
	}
	p := &AttemptPlan{identity: identity, route: decision, generation: identity.RoutingGeneration, maxAttempts: normalizeMaxAttempts(identity.MaxAttempts)}
	if len(decision.Explore.Ordered) > 0 && decision.Explore.Total > 0 && len(decision.Explore.Cumulative) == len(decision.Explore.Ordered) {
		ticket := exploreHashForPlan(identity, 0) % decision.Explore.Total
		idx := sort.Search(len(decision.Explore.Cumulative), func(i int) bool {
			return decision.Explore.Cumulative[i] > ticket
		})
		if idx >= 0 && idx < len(decision.Explore.Ordered) {
			p.sampleIdx, p.sampleValid = idx, true
		}
	}
	p.total = len(decision.Primary) + len(decision.Explore.Fallback) + len(decision.Degraded)
	if p.sampleValid {
		p.total++
	}
	return p, nil
}

// ApplyCacheAffinity arms the soft preference without materializing order:
// the walker serves preferred-domain candidates first, then spill, preserving
// lane order within each phase and consulting every candidate at most once.
func (p *AttemptPlan) ApplyCacheAffinity(hash uint64) bool {
	if p == nil || p.route == nil {
		return false
	}
	dom, ok := p.route.CacheDomainRing.Lookup(hash)
	if !ok {
		return false
	}
	p.affinitySet = true
	p.affinityDom = dom
	p.affinPhase = 0
	p.walkSeg, p.walkPos = 0, 0
	return true
}

func (p *AttemptPlan) domainOf(id int64) string {
	d, _ := cacheDomainAccountDomain(p.route.CacheDomainAccounts, id)
	return d
}

// next returns the next compiled candidate in virtual order, honouring the
// affinity phases without allocation. Skips the sampled ID inside fallback.
func (p *AttemptPlan) next() (CompiledCandidate, bool) {
	if p == nil || p.route == nil {
		return CompiledCandidate{}, false
	}
	var sampleID int64
	if p.sampleValid {
		sampleID = p.route.Explore.Ordered[p.sampleIdx].AccountID
	}
	for {
		if p.affinitySet && p.affinPhase > 1 {
			return CompiledCandidate{}, false
		}
		if !p.affinitySet && p.walkSeg > 3 {
			return CompiledCandidate{}, false
		}
		if p.affinitySet && p.walkSeg > 3 {
			p.affinPhase++
			if p.affinPhase > 1 {
				return CompiledCandidate{}, false
			}
			p.walkSeg, p.walkPos = 0, 0
			continue
		}
		var c CompiledCandidate
		var ok bool
		switch p.walkSeg {
		case 0:
			if p.walkPos < len(p.route.Primary) {
				c = p.route.Primary[p.walkPos]
				p.walkPos++
				ok = true
			} else {
				p.walkSeg, p.walkPos = 1, 0
				continue
			}
		case 1:
			p.walkSeg, p.walkPos = 2, 0
			if p.sampleValid {
				c = p.route.Explore.Ordered[p.sampleIdx]
				ok = true
			} else {
				continue
			}
		case 2:
			if p.walkPos < len(p.route.Explore.Fallback) {
				idx := p.route.Explore.Fallback[p.walkPos]
				p.walkPos++
				if int(idx) >= len(p.route.Explore.Ordered) {
					continue
				}
				c = p.route.Explore.Ordered[idx]
				if p.sampleValid && c.AccountID == sampleID {
					continue
				}
				ok = true
			} else {
				p.walkSeg, p.walkPos = 3, 0
				continue
			}
		case 3:
			if p.walkPos < len(p.route.Degraded) {
				c = p.route.Degraded[p.walkPos]
				p.walkPos++
				ok = true
			} else {
				p.walkSeg = 4
				continue
			}
		}
		if !ok {
			continue
		}
		if p.affinitySet {
			want := p.domainOf(c.AccountID) == p.affinityDom
			if (p.affinPhase == 0) != want {
				continue
			}
		}
		duplicate := false
		for i := uint8(0); i < p.emittedCnt; i++ {
			if p.emitted[i] == c.AccountID {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		if p.emittedCnt < MaxAttemptPlanAccounts {
			p.emitted[p.emittedCnt] = c.AccountID
			p.emittedCnt++
		}
		return c, true
	}
}

func (p *AttemptPlan) projectCandidate(c CompiledCandidate) CompiledCandidate {
	if c.RequestedModel != "" || p.identity.RequestedModel == "" {
		return c
	}
	c.RequestedModel = p.identity.RequestedModel
	c.MappedModel = c.RequestedModel
	c.MappingMode = domain.ModelMappingModeInvalid
	if c.Static != nil && c.Static.tpl != nil {
		if mapping, ok := c.Static.tpl.ModelMapping[c.RequestedModel]; ok {
			c.MappedModel = mapping.MappedModel
			c.MappingMode = mapping.Mode
		}
	}
	format := domain.RequestFormat(p.route.Format)
	op := domain.OperationTag(p.route.OperationTag)
	c.Quality = qualityClassHexForWithOp(format, c.MappedModel, op)
	c.QualityRaw = qualityClassHexForWithOp(format, c.RequestedModel, op)
	return c
}

func (p *AttemptPlan) Identity() AttemptPlanIdentity { return p.identity }

func (p *AttemptPlan) hasStaticChange(v *RoutingView) bool {
	if v == nil || v.static == nil {
		return false
	}
	check := func(c CompiledCandidate) bool {
		return c.Leaf != nil && v.static.byID[c.AccountID] != c.Leaf
	}
	for _, c := range p.route.Primary {
		if check(c) {
			return true
		}
	}
	for _, c := range p.route.Explore.Ordered {
		if check(c) {
			return true
		}
	}
	for _, c := range p.route.Degraded {
		if check(c) {
			return true
		}
	}
	return false
}

func (p *AttemptPlan) CurrentAttempt() (Attempt, bool) {
	if p == nil || p.attemptedCnt == 0 {
		return Attempt{}, false
	}
	return p.currentAttempt, true
}

// AbandonLastAttempt refunds the most recent reservation that was never
// dispatched. The scan is not rewound.
func (p *AttemptPlan) AbandonLastAttempt() {
	if p == nil || p.attemptedCnt == 0 {
		return
	}
	p.attemptedCnt--
	p.ordinal--
	p.attempted[p.attemptedCnt] = 0
	p.attemptIDs[p.attemptedCnt] = ""
	p.currentAttempt = Attempt{}
}

func (p *AttemptPlan) Reserve(reserve AttemptReservation) (Attempt, error) {
	attempt, _, err := p.reserve(func(c CompiledCandidate) bool { return reserve(c.AccountID) })
	return attempt, err
}

func (p *AttemptPlan) reserve(reserve func(CompiledCandidate) bool) (Attempt, CompiledCandidate, error) {
	if p.total == 0 {
		return Attempt{}, CompiledCandidate{}, ErrNoAvailable
	}
	if p.ordinal >= p.maxAttempts {
		return Attempt{}, CompiledCandidate{}, ErrAttemptsExhausted
	}
	for {
		c, ok := p.next()
		if !ok {
			return Attempt{}, CompiledCandidate{}, ErrAttemptsExhausted
		}
		c = p.projectCandidate(c)
		if !reserve(c) {
			continue
		}
		ordinal := p.ordinal + 1
		var attemptID string
		if p.identity.RequestID != "" {
			attemptID = fmt.Sprintf("%s:%d", p.identity.RequestID, ordinal)
		} else {
			attemptID = fmt.Sprintf("attempt-%d", ordinal)
		}
		var prev *string
		var prevAccount *int64
		if p.ordinal > 0 && p.attemptedCnt > 0 {
			prev = &p.attemptIDs[p.attemptedCnt-1]
			prevAccount = &p.attempted[p.attemptedCnt-1]
		}
		mapped := c.MappedModel
		if !p.identity.ApplyModelMapping {
			mapped = c.RequestedModel
		}
		quality := c.Quality
		if !p.identity.ApplyModelMapping {
			quality = c.QualityRaw
		}
		p.attempted[p.attemptedCnt] = c.AccountID
		p.attemptIDs[p.attemptedCnt] = attemptID
		p.ordinal = ordinal
		attempt := Attempt{
			AttemptID: attemptID, RouteClassID: p.route.RouteClassID,
			QualityClassID: quality, CandidateFingerprint: c.Fingerprint,
			TemplateID: c.TemplateID, AccountID: c.AccountID,
			RequestedModel: c.RequestedModel, MappedModel: mapped, Lane: c.Lane,
			Ordinal: ordinal, RoutingGeneration: p.generation,
			LifecycleRevision: c.LifecycleRevision,
			PreviousAttemptID: prev, PreviousAccountID: prevAccount,
			CallerCategory: p.route.CallerCategory, OperationTag: p.route.OperationTag,
		}
		p.currentAttempt = attempt
		p.attemptedCnt++
		p.reservationStarted = true
		return attempt, c, nil
	}
}
