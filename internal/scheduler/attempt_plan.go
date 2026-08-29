// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import "errors"

const MaxAttemptPlanAccounts = 8

var ErrAttemptsExhausted = errors.New("scheduler: attempt plan exhausted")

type AttemptPlanIdentity struct {
	RequestID         string
	UserID            int64
	RouteClassID      string
	RoutingGeneration uint64
}

type AttemptLane string

const (
	AttemptLanePrimary  AttemptLane = "primary"
	AttemptLaneExplore  AttemptLane = "explore"
	AttemptLaneDegraded AttemptLane = "degraded"
)

type Attempt struct {
	AccountID int64
	Lane      AttemptLane
	Ordinal   uint8
}

type AttemptReservation func(accountID int64) bool

type attemptPlanCandidate struct {
	accountID int64
	lane      AttemptLane
}

type AttemptPlan struct {
	identity       AttemptPlanIdentity
	candidates     [MaxAttemptPlanAccounts]attemptPlanCandidate
	candidateCount uint8
	cursor         uint8
	attempted      [MaxAttemptPlanAccounts]int64
	attemptedCount uint8
	ordinal        uint8
}

func NewAttemptPlan(identity AttemptPlanIdentity, decision RouteDecision) *AttemptPlan {
	p := &AttemptPlan{identity: identity}
	for _, accountID := range decision.Primary {
		p.addCandidate(accountID, AttemptLanePrimary)
	}
	if len(decision.Explore.IDs) > 0 {
		p.addCandidate(decision.Explore.IDs[0], AttemptLaneExplore)
	}
	for _, accountID := range decision.Explore.Fallback {
		p.addCandidate(accountID, AttemptLaneExplore)
	}
	for _, accountID := range decision.Degraded {
		p.addCandidate(accountID, AttemptLaneDegraded)
	}
	return p
}

func (p *AttemptPlan) Identity() AttemptPlanIdentity { return p.identity }

func (p *AttemptPlan) Reserve(reserve AttemptReservation) (Attempt, error) {
	if p.candidateCount == 0 {
		return Attempt{}, ErrNoAvailable
	}
	for p.cursor < p.candidateCount {
		candidate := p.candidates[p.cursor]
		p.cursor++
		p.attempted[p.attemptedCount] = candidate.accountID
		p.attemptedCount++
		if !reserve(candidate.accountID) {
			continue
		}
		p.ordinal++
		return Attempt{AccountID: candidate.accountID, Lane: candidate.lane, Ordinal: p.ordinal}, nil
	}
	return Attempt{}, ErrAttemptsExhausted
}

func (p *AttemptPlan) addCandidate(accountID int64, lane AttemptLane) {
	if p.candidateCount == MaxAttemptPlanAccounts || p.contains(accountID) {
		return
	}
	p.candidates[p.candidateCount] = attemptPlanCandidate{accountID: accountID, lane: lane}
	p.candidateCount++
}

func (p *AttemptPlan) contains(accountID int64) bool {
	for i := uint8(0); i < p.candidateCount; i++ {
		if p.candidates[i].accountID == accountID {
			return true
		}
	}
	return false
}
