// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"sort"
	"strconv"

	"github.com/is7qin/c3api/internal/domain"
)

// selectSession is the stack selection session replacing the heap
// AttemptPlan box outright (single clean mechanism — no dual-track, no flags,
// no fallback path). Each request walks the shared published route through a
// stack value (~160B scalars, zero heap) and projects attempt identity on
// demand at settle/arm time instead of storing full-Attempt copies.
//
// Ownership + lifecycle: REQUEST-OWNED stack value — born at select entry
// (newSelectSession/selectSessionForView), dies at return; passed by value (or
// by stack pointer that never escapes) and never shared across goroutines.
// The interned RouteRef hex is ROUTE-OWNED (borrowed via route.RouteClassID);
// derived settle strings are CALLER-OWNED (born at settle/arm, die at return).
//
// Retained load-bearing split: tried[] is the never-rewound scan-dedup (the
// old emitted[] role — rejects stay marked, AbandonLastAttempt never rewinds
// it) while attempted[]/attemptIDs[] is the rewindable reservation history
// (abandon rewinds the attempted side only). PreviousAttemptID linkage derives
// from the rewindable history with FRESH backing per derived pointer, never
// aliasing session slots.
type selectSession struct {
	identity    AttemptPlanIdentity
	route       *RouteDecision
	generation  uint64
	maxAttempts uint8
	sampleIdx   int
	sampleValid bool
	// exploreFirst is the per-request lane decision (charter):
	// true serves the explore sample before Primary, false Primary first.
	// primaryServed tracks the primary lane inside the walk so the
	// explore-first chain runs sample → primary → fallback → degraded
	// exactly once per affinity phase. Stack scalars, zero heap.
	exploreFirst  bool
	primaryServed bool
	walkSeg       uint8
	walkPos       int
	affinitySet   bool
	// hashed affinity key only — the affinityDom heap string is deleted.
	// The domain string is borrowed per next() call from the route-owned ring.
	affinityHash uint64
	affinPhase   uint8
	total        int
	tried        [MaxAttemptPlanAccounts]int64
	triedCnt     uint8
	attempted    [MaxAttemptPlanAccounts]int64
	attemptIDs   [MaxAttemptPlanAccounts]string
	attemptedCnt uint8
	ordinal      uint8
	// lastSeg/lastPos locate the last settled candidate inside the immutable
	// route arrays, so CurrentAttempt derives (never carries) the attempt
	// instead of storing a full-Attempt copy. lastValid is cleared by
	// AbandonLastAttempt (abandoned session has no current attempt).
	lastSeg   uint8
	lastPos   int
	lastValid bool
	// hasStaticChange verdict cache — the full-lane scan runs at most
	// once per view generation per session (single fence-site entry via
	// cachedStaticVerdict; steady state never scans).
	staticChecked bool
	staticGen     uint64
	staticVerdict bool
	// reservationStarted is monotonic execution state: set once after the
	// first successful reservation, never cleared (AbandonLastAttempt must
	// not reset it — attemptedCnt rewinds, execution history does not).
	// A decision-generation mismatch is tolerated only before execution
	// starts; afterwards the strict fence applies.
	reservationStarted bool
}

// AttemptPlan is the pinned harness spelling of selectSession: the
// attempt_plan_* falsification vectors assert behavior through this name, so
// the alias keeps them compiling unmodified over the single session
// mechanism. Production code carries selectSession values; the alias declares
// no new structure and no second track.
type AttemptPlan = selectSession

func normalizeMaxAttempts(n uint8) uint8 {
	if n == 0 || n > MaxAttemptPlanAccounts {
		return MaxAttemptPlanAccounts
	}
	return n
}

// newSelectSession binds an identity to one compiled route: primary order,
// then the deterministic explore sample followed by the complete fallback
// tail, then degraded — unless the compiled exploration share elects
// explore-first for this request (laneHashForPlan % 10000 < ExploreBP), in
// which case the sample leads and Primary follows. Stack value, zero heap;
// no per-request slices/maps; the overflow tail is walked lazily and never
// truncated.
func newSelectSession(identity AttemptPlanIdentity, decision *RouteDecision) (selectSession, error) {
	if decision == nil {
		return selectSession{}, &InvalidRouteDecisionError{Field: "decision", Index: -1}
	}
	if !decision.validated {
		if err := validateRouteDecision(decision); err != nil {
			return selectSession{}, err
		}
	}
	p := selectSession{identity: identity, route: decision, generation: identity.RoutingGeneration, maxAttempts: normalizeMaxAttempts(identity.MaxAttempts)}
	if len(decision.Explore.Ordered) > 0 && decision.Explore.Total > 0 && len(decision.Explore.Cumulative) == len(decision.Explore.Ordered) {
		ticket := exploreHashForPlan(identity, 0) % decision.Explore.Total
		idx := sort.Search(len(decision.Explore.Cumulative), func(i int) bool {
			return decision.Explore.Cumulative[i] > ticket
		})
		if idx >= 0 && idx < len(decision.Explore.Ordered) {
			p.sampleIdx, p.sampleValid = idx, true
		}
	}
	// Steady-state exploration share: one lane draw per request at session
	// construction (re-rolling per attempt would break the deterministic
	// failover walk order and the tried[] dedup). No Primary → explore-first
	// (cold start unchanged — the primary lane is empty either way); no
	// sample or unset share → primary-first.
	if p.sampleValid {
		switch {
		case len(decision.Primary) == 0 || decision.Explore.ExploreBP >= 10000:
			p.exploreFirst = true
		case decision.Explore.ExploreBP > 0:
			p.exploreFirst = laneHashForPlan(identity)%10000 < uint64(decision.Explore.ExploreBP)
		}
	}
	if p.exploreFirst {
		p.walkSeg = 1
	}
	p.total = len(decision.Primary) + len(decision.Explore.Fallback) + len(decision.Degraded)
	if p.sampleValid {
		p.total++
	}
	return p, nil
}

// NewAttemptPlan is the retained test-harness spelling of the session
// constructor: it returns the stack newSelectSession VALUE (never boxed —
// the pre-v4 `return &sess` heap box is deleted; review finding 2).
// Production binds sessions via Scheduler.NewAttemptPlan (same value path).
func NewAttemptPlan(identity AttemptPlanIdentity, decision *RouteDecision) (AttemptPlan, error) {
	return newSelectSession(identity, decision)
}

// ApplyCacheAffinity arms the soft preference without materializing order:
// the walker serves preferred-domain candidates first, then spill, preserving
// lane order within each phase and consulting every candidate at most once.
// Only the hashed key is kept; the domain string is borrowed from the
// route-owned ring per next() call (: no affinityDom heap field).
func (p *selectSession) ApplyCacheAffinity(hash uint64) bool {
	if p == nil || p.route == nil {
		return false
	}
	if _, ok := p.route.CacheDomainRing.Lookup(hash); !ok {
		return false
	}
	p.affinitySet = true
	p.affinityHash = hash
	p.affinPhase = 0
	p.resetWalk()
	return true
}

func (p *selectSession) domainOf(id int64) string {
	d, _ := cacheDomainAccountDomain(p.route.CacheDomainAccounts, id)
	return d
}

// resetWalk re-arms the lane walk for an affinity phase: the explore-first
// lane decision applies per phase, so both the preferred-domain sweep and
// the spill sweep lead with the same lane.
func (p *selectSession) resetWalk() {
	p.walkSeg, p.walkPos = 0, 0
	p.primaryServed = false
	if p.exploreFirst {
		p.walkSeg = 1
	}
}

// candidateAt fetches the candidate at one walk position without advancing
// any cursor: the single fetch site shared by next() and the CurrentAttempt
// locator derivation, so the two can never diverge.
func (p *selectSession) candidateAt(seg uint8, pos int) (CompiledCandidate, bool) {
	switch seg {
	case 0:
		if pos < 0 || pos >= len(p.route.Primary) {
			return CompiledCandidate{}, false
		}
		return p.route.Primary[pos], true
	case 1:
		if !p.sampleValid || p.sampleIdx < 0 || p.sampleIdx >= len(p.route.Explore.Ordered) {
			return CompiledCandidate{}, false
		}
		return p.route.Explore.Ordered[p.sampleIdx], true
	case 2:
		if pos < 0 || pos >= len(p.route.Explore.Fallback) {
			return CompiledCandidate{}, false
		}
		idx := p.route.Explore.Fallback[pos]
		if int(idx) >= len(p.route.Explore.Ordered) {
			return CompiledCandidate{}, false
		}
		c := p.route.Explore.Ordered[idx]
		if p.sampleValid && c.AccountID == p.route.Explore.Ordered[p.sampleIdx].AccountID {
			return CompiledCandidate{}, false
		}
		return c, true
	case 3:
		if pos < 0 || pos >= len(p.route.Degraded) {
			return CompiledCandidate{}, false
		}
		return p.route.Degraded[pos], true
	}
	return CompiledCandidate{}, false
}

// next returns the next compiled candidate in virtual order, honouring the
// affinity phases without allocation. Skips the sampled ID inside fallback.
// Each yield records its route-array locator for the CurrentAttempt
// derivation (no Attempt copy is carried in the session).
func (p *selectSession) next() (CompiledCandidate, bool) {
	if p == nil || p.route == nil {
		return CompiledCandidate{}, false
	}
	// borrow the preferred domain per next() call from the route-owned
	// ring (string header only, zero heap) instead of a stored heap string.
	preferredDom := ""
	if p.affinitySet {
		dom, ok := p.route.CacheDomainRing.Lookup(p.affinityHash)
		if !ok {
			return CompiledCandidate{}, false
		}
		preferredDom = dom
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
			p.resetWalk()
			continue
		}
		var c CompiledCandidate
		var ok bool
		var seg uint8
		var pos int
		switch p.walkSeg {
		case 0:
			if p.walkPos < len(p.route.Primary) {
				if c, ok = p.candidateAt(0, p.walkPos); !ok {
					p.primaryServed = true
					if p.exploreFirst {
						p.walkSeg, p.walkPos = 2, 0
					} else {
						p.walkSeg, p.walkPos = 1, 0
					}
					continue
				}
				seg, pos = 0, p.walkPos
				p.walkPos++
			} else {
				p.primaryServed = true
				if p.exploreFirst {
					p.walkSeg, p.walkPos = 2, 0
				} else {
					p.walkSeg, p.walkPos = 1, 0
				}
				continue
			}
		case 1:
			// Explore-first sessions serve the sample before Primary;
			// primary-first sessions serve it after. Either way the sample
			// yields exactly once (tried[] dedups any revisit).
			if p.exploreFirst && !p.primaryServed {
				p.walkSeg, p.walkPos = 0, 0
			} else {
				p.walkSeg, p.walkPos = 2, 0
			}
			if p.sampleValid {
				if c, ok = p.candidateAt(1, 0); !ok {
					continue
				}
				seg, pos = 1, p.sampleIdx
			} else {
				continue
			}
		case 2:
			if p.walkPos < len(p.route.Explore.Fallback) {
				if c, ok = p.candidateAt(2, p.walkPos); !ok {
					p.walkPos++
					continue
				}
				seg, pos = 2, p.walkPos
				p.walkPos++
			} else {
				p.walkSeg, p.walkPos = 3, 0
				continue
			}
		case 3:
			if p.walkPos < len(p.route.Degraded) {
				if c, ok = p.candidateAt(3, p.walkPos); !ok {
					p.walkSeg = 4
					continue
				}
				seg, pos = 3, p.walkPos
				p.walkPos++
			} else {
				p.walkSeg = 4
				continue
			}
		}
		if !ok {
			continue
		}
		if p.affinitySet {
			want := p.domainOf(c.AccountID) == preferredDom
			if (p.affinPhase == 0) != want {
				continue
			}
		}
		duplicate := false
		for i := uint8(0); i < p.triedCnt; i++ {
			if p.tried[i] == c.AccountID {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		if p.triedCnt < MaxAttemptPlanAccounts {
			p.tried[p.triedCnt] = c.AccountID
			p.triedCnt++
		}
		p.lastSeg, p.lastPos = seg, pos
		return c, true
	}
}

func (p *selectSession) projectCandidate(c CompiledCandidate) CompiledCandidate {
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

func (p selectSession) Identity() AttemptPlanIdentity { return p.identity }

func (p *selectSession) hasStaticChange(v *RoutingView) bool {
	if v == nil || v.static == nil {
		return false
	}
	check := func(c CompiledCandidate) bool {
		if c.Leaf == nil {
			return false
		}
		if v.static.byID[c.AccountID] == nil {
			return true
		}
		// 判据同预留路径：读视图发布时预计算的逐账号 planKey，不现算摘要。
		fact, ok := v.static.facts[c.AccountID]
		if !ok {
			return true
		}
		return fact.planKey != c.PlanKey
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

// cachedStaticVerdict is the SINGLE fence-site entry for the static-change
// scan: the full-lane scan body is unchanged, but it runs at most
// once per view generation per session and its verdict is cached. Steady
// state (generation match) never scans — contributes zero bytes.
func (p *selectSession) cachedStaticVerdict(v *RoutingView) bool {
	if v != nil && p.staticChecked && p.staticGen == v.generation {
		return p.staticVerdict
	}
	verdict := p.hasStaticChange(v)
	if v != nil {
		p.staticGen = v.generation
	}
	p.staticVerdict, p.staticChecked = verdict, true
	return verdict
}

// CurrentAttempt derives (never carries) the current attempt from the
// rewindable history tail plus the locator recorded at settle time. The
// derivation reproduces the settled Attempt bit-for-bit: candidate fields
// come from the immutable route arrays, identity from the formulaic
// reqID:ordinal derivation, prev linkage fresh-backed. Test and seam use
// only — production threads the settled Attempt values instead, so no hot
// path value-returns a derived Attempt.
func (p selectSession) CurrentAttempt() (Attempt, bool) {
	if p.attemptedCnt == 0 {
		return Attempt{}, false
	}
	if !p.lastValid {
		// Abandoned without a newer reserve: the scan stands advanced but no
		// attempt is current (matches the cleared-currentAttempt quirk).
		return Attempt{}, true
	}
	c, ok := p.candidateAt(p.lastSeg, p.lastPos)
	if !ok {
		return Attempt{}, true
	}
	c = p.projectCandidate(c)
	top := p.attemptedCnt - 1
	var prevID string
	var prevAcc int64
	hasPrev := p.attemptedCnt >= 2
	if hasPrev {
		prevID, prevAcc = p.attemptIDs[top-1], p.attempted[top-1]
	}
	return p.buildAttempt(c, p.ordinal, p.attemptIDs[top], prevID, prevAcc, hasPrev), true
}

// AbandonLastAttempt refunds the most recent reservation that was never
// dispatched. The scan is not rewound: tried dedup and the walk cursor stand,
// only the attempted side (history, ordinal, current locator) rewinds.
func (p *selectSession) AbandonLastAttempt() {
	if p == nil || p.attemptedCnt == 0 {
		return
	}
	p.attemptedCnt--
	p.ordinal--
	p.attempted[p.attemptedCnt] = 0
	p.attemptIDs[p.attemptedCnt] = ""
	p.lastValid = false
}

func (p *selectSession) Reserve(reserve AttemptReservation) (Attempt, error) {
	attempt, _, err := p.reserve(func(c CompiledCandidate) bool { return reserve(c.AccountID) })
	return attempt, err
}

// deriveAttemptID projects attempt identity formulaically at settle/arm time
// (relocated with zero elimination credit): reqID:ordinal, or
// attempt-N when the request carries no ID. No fmt — plain concat reproduces
// the old Sprintf branches byte-for-byte.
func deriveAttemptID(requestID string, ordinal uint8) string {
	n := strconv.FormatUint(uint64(ordinal), 10)
	if requestID != "" {
		return requestID + ":" + n
	}
	return "attempt-" + n
}

// buildAttempt is the single settle/arm construction site: the
// returned Attempt/Selection VALUE shape at the boundary is preserved, only
// its construction moved from per-attempt store to settle-derive. Prev
// linkage gets FRESH backing per derived pointer (copied out of the session
// history into caller-owned storage, never aliasing session slots), so a
// later AbandonLastAttempt cannot corrupt an outstanding Attempt. The
// Validate contract holds by construction: nil PreviousAttemptID for ordinal
// 1, required non-empty beyond.
func (p *selectSession) buildAttempt(c CompiledCandidate, ordinal uint8, attemptID string, prevID string, prevAcc int64, hasPrev bool) Attempt {
	var prev *string
	var prevAccount *int64
	if hasPrev {
		id := prevID
		ac := prevAcc
		prev = &id
		prevAccount = &ac
	}
	mapped := c.MappedModel
	if !p.identity.ApplyModelMapping {
		mapped = c.RequestedModel
	}
	quality := c.Quality
	if !p.identity.ApplyModelMapping {
		quality = c.QualityRaw
	}
	return Attempt{
		AttemptID: attemptID, RouteClassID: p.route.RouteClassID,
		QualityClassID: quality, CandidateFingerprint: c.Fingerprint,
		TemplateID: c.TemplateID, AccountID: c.AccountID,
		RequestedModel: c.RequestedModel, MappedModel: mapped, Lane: c.Lane,
		Ordinal: ordinal, RoutingGeneration: p.generation,
		IdentityRevision:  c.IdentityRevision,
		PreviousAttemptID: prev, PreviousAccountID: prevAccount,
		CallerCategory: p.route.CallerCategory, OperationTag: p.route.OperationTag,
	}
}

func (p *selectSession) reserve(reserve func(CompiledCandidate) bool) (Attempt, CompiledCandidate, error) {
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
		attemptID := deriveAttemptID(p.identity.RequestID, ordinal)
		var prevID string
		var prevAcc int64
		hasPrev := p.attemptedCnt > 0
		if hasPrev {
			prevID, prevAcc = p.attemptIDs[p.attemptedCnt-1], p.attempted[p.attemptedCnt-1]
		}
		p.attempted[p.attemptedCnt] = c.AccountID
		p.attemptIDs[p.attemptedCnt] = attemptID
		p.ordinal = ordinal
		p.attemptedCnt++
		p.lastValid = true
		p.reservationStarted = true
		return p.buildAttempt(c, ordinal, attemptID, prevID, prevAcc, hasPrev), c, nil
	}
}
