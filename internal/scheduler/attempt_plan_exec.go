// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"fmt"
	"sort"
)

// AttemptPlan executes one compiled RouteDecision. The first
// MaxAttemptPlanAccounts unique candidates are materialized as the hot
// prefix; the complete unique overflow tail stays in the immutable compiled
// lists and is walked lazily (never truncated, never copied per request).
// Successful dispatches are bounded by maxAttempts (1..8); reservation
// rejects advance the scan without consuming an attempt.
type AttemptPlan struct {
	identity     AttemptPlanIdentity
	format       string
	model        string
	operationTag string
	maxAttempts  uint8
	// hot prefix (build-time deduplicated, ≤8 slots, fixed storage)
	candidates  [MaxAttemptPlanAccounts]attemptPlanCandidate
	prefixCount uint8
	cursor      uint8
	// overflow: immutable compiled lane lists walked lazily after the prefix
	decision    RouteDecision
	sampleID    int64
	sampleValid bool
	walkSeg     uint8 // 0 primary, 1 explore sample, 2 explore fallback, 3 degraded, 4 done
	walkPos     int
	total       int
	// attempt bookkeeping (bounded by maxAttempts ≤ 8)
	attempted      [MaxAttemptPlanAccounts]int64
	attemptIDs     [MaxAttemptPlanAccounts]string
	attempts       [MaxAttemptPlanAccounts]Attempt
	attemptedCount uint8
	ordinal        uint8
}

// normalizeMaxAttempts clamps the dispatch bound into 1..8; unset (0) means
// "no explicit bound" and takes the array bound (the proxy always stamps its
// normalized config value, so production plans carry an explicit bound).
func normalizeMaxAttempts(n uint8) uint8 {
	if n == 0 || n > MaxAttemptPlanAccounts {
		return MaxAttemptPlanAccounts
	}
	return n
}

// NewAttemptPlan binds an identity to one compiled route decision:
// Primary order, then the deterministic explore sample (FNV ticket over the
// cumulative weights) followed by the complete explore fallback tail, then
// Degraded order. Duplicate account IDs across lanes are deduplicated once at
// materialization; the overflow tail is referenced, never copied.
func NewAttemptPlan(identity AttemptPlanIdentity, decision RouteDecision) *AttemptPlan {
	p := &AttemptPlan{identity: identity, decision: decision, maxAttempts: normalizeMaxAttempts(identity.MaxAttempts)}
	if len(decision.Explore.IDs) > 0 && decision.Explore.Total > 0 && len(decision.Explore.Cumulative) == len(decision.Explore.IDs) {
		hash := exploreHashForPlan(identity, 0)
		ticket := hash % decision.Explore.Total
		idx := sort.Search(len(decision.Explore.Cumulative), func(i int) bool {
			return decision.Explore.Cumulative[i] > ticket
		})
		if idx >= 0 && idx < len(decision.Explore.IDs) {
			p.sampleID, p.sampleValid = decision.Explore.IDs[idx], true
		}
	} else if len(decision.Explore.IDs) > 0 {
		p.sampleID, p.sampleValid = decision.Explore.IDs[0], true
	}
	p.countTotal()
	for p.prefixCount < MaxAttemptPlanAccounts {
		candidate, ok := p.nextEntry()
		if !ok {
			break
		}
		p.candidates[p.prefixCount] = candidate
		p.prefixCount++
	}
	return p
}

func (p *AttemptPlan) countTotal() {
	n := len(p.decision.Primary) + len(p.decision.Explore.Fallback) + len(p.decision.Degraded)
	if p.sampleValid {
		n++
	}
	p.total = n
}

// seen reports whether the account was already materialized into the hot
// prefix or already consumed an attempt (overflow dedupe without a per-plan
// seen-set allocation; the compiler guarantees lane exclusivity, so this only
// has to catch hand-authored duplicates).
func (p *AttemptPlan) seen(accountID int64) bool {
	for i := uint8(0); i < p.prefixCount; i++ {
		if p.candidates[i].accountID == accountID {
			return true
		}
	}
	for i := uint8(0); i < p.attemptedCount; i++ {
		if p.attempted[i] == accountID {
			return true
		}
	}
	return false
}

// nextEntry walks the virtual lane concatenation (Primary, explore sample,
// explore fallback, Degraded) lazily, skipping duplicates and the sampled ID
// inside the fallback. Positions persist across calls: the whole plan scans
// each unique candidate at most once.
func (p *AttemptPlan) nextEntry() (attemptPlanCandidate, bool) {
	for p.walkSeg <= 3 {
		switch p.walkSeg {
		case 0:
			if p.walkPos < len(p.decision.Primary) {
				id := p.decision.Primary[p.walkPos]
				p.walkPos++
				if !p.seen(id) {
					return attemptPlanCandidate{accountID: id, lane: AttemptLanePrimary}, true
				}
				continue
			}
			p.walkSeg, p.walkPos = 1, 0
		case 1:
			p.walkSeg, p.walkPos = 2, 0
			if p.sampleValid && !p.seen(p.sampleID) {
				return attemptPlanCandidate{accountID: p.sampleID, lane: AttemptLaneExplore}, true
			}
		case 2:
			if p.walkPos < len(p.decision.Explore.Fallback) {
				id := p.decision.Explore.Fallback[p.walkPos]
				p.walkPos++
				if id == p.sampleID && p.sampleValid {
					continue
				}
				if !p.seen(id) {
					return attemptPlanCandidate{accountID: id, lane: AttemptLaneExplore}, true
				}
				continue
			}
			p.walkSeg, p.walkPos = 3, 0
		case 3:
			if p.walkPos < len(p.decision.Degraded) {
				id := p.decision.Degraded[p.walkPos]
				p.walkPos++
				if !p.seen(id) {
					return attemptPlanCandidate{accountID: id, lane: AttemptLaneDegraded}, true
				}
				continue
			}
			p.walkSeg = 4
		}
	}
	return attemptPlanCandidate{}, false
}

// pull returns the next candidate: materialized prefix first (resolved), then
// the lazy overflow tail (account ID + lane only; the scheduler resolves on
// demand so overflow costs nothing until actually scanned).
func (p *AttemptPlan) pull() (attemptPlanCandidate, bool) {
	if p.cursor < p.prefixCount {
		candidate := p.candidates[p.cursor]
		p.cursor++
		return candidate, true
	}
	return p.nextEntry()
}

func (p *AttemptPlan) Identity() AttemptPlanIdentity { return p.identity }

func (p *AttemptPlan) CurrentAttempt() (Attempt, bool) {
	if p == nil || p.attemptedCount == 0 {
		return Attempt{}, false
	}
	return p.attempts[p.attemptedCount-1], true
}

func (p *AttemptPlan) Reserve(reserve AttemptReservation) (Attempt, error) {
	return p.reserve(func(candidate *attemptPlanCandidate) bool {
		return reserve(candidate.accountID)
	})
}

// reserve scans candidates from the current cursor. A reject advances the
// scan but never consumes an attempt; only a successful reservation
// increments the ordinal. ErrNoAvailable means the compiled route had zero
// candidates; ErrAttemptsExhausted means candidates existed but the scan or
// the 1..8 dispatch bound ran out.
func (p *AttemptPlan) reserve(reserve func(*attemptPlanCandidate) bool) (Attempt, error) {
	if p.total == 0 {
		return Attempt{}, ErrNoAvailable
	}
	if p.ordinal >= p.maxAttempts {
		return Attempt{}, ErrAttemptsExhausted
	}
	for {
		candidate, ok := p.pull()
		if !ok {
			return Attempt{}, ErrAttemptsExhausted
		}
		if !reserve(&candidate) {
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
		if p.ordinal > 0 && p.attemptedCount > 0 {
			prevCopy := p.attemptIDs[p.attemptedCount-1]
			prev = &prevCopy
			accountCopy := p.attempted[p.attemptedCount-1]
			prevAccount = &accountCopy
		}
		p.attempted[p.attemptedCount] = candidate.accountID
		p.attemptIDs[p.attemptedCount] = attemptID
		p.ordinal = ordinal
		attempt := Attempt{
			AttemptID:            attemptID,
			RouteClassID:         candidate.routeClassID,
			QualityClassID:       candidate.quality,
			CandidateFingerprint: candidate.fingerprint,
			TemplateID:           candidate.templateID,
			AccountID:            candidate.accountID,
			RequestedModel:       candidate.requestedModel,
			MappedModel:          candidate.mappedModel,
			Lane:                 candidate.lane,
			Ordinal:              ordinal,
			RoutingGeneration:    candidate.routingGeneration,
			LifecycleRevision:    candidate.lifecycleRevision,
			PreviousAttemptID:    prev,
			PreviousAccountID:    prevAccount,
			CallerCategory:       candidate.callerCategory,
			OperationTag:         candidate.operationTag,
		}
		p.attempts[p.attemptedCount] = attempt
		p.attemptedCount++
		return attempt, nil
	}
}
