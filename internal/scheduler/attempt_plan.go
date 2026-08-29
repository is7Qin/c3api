// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"errors"
	"sort"
	"strconv"
)

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
	accountID   int64
	lane        AttemptLane
	account     *accountSnapshot
	static      *snapshotStatic
	fingerprint string
	quality     string
}

type AttemptPlan struct {
	identity       AttemptPlanIdentity
	format         string
	model          string
	candidates     [MaxAttemptPlanAccounts]attemptPlanCandidate
	candidateCount uint8
	cursor         uint8
	attempted      [MaxAttemptPlanAccounts]int64
	attemptedCount uint8
	ordinal        uint8
}

const fnvOffset64 = 14695981039346656037
const fnvPrime64 = 1099511628211

func ExploreHash(label, requestID string, userID int64, routeClassID string, generation uint64, ordinal uint8) uint64 {
	h := uint64(fnvOffset64)
	writeString := func(s string) {
		for i := 0; i < len(s); i++ {
			h ^= uint64(s[i])
			h *= fnvPrime64
		}
		h ^= 0
		h *= fnvPrime64
	}
	writeString(label)
	writeString(requestID)
	writeString(strconv.FormatInt(userID, 10))
	writeString(routeClassID)
	writeString(strconv.FormatUint(generation, 10))
	writeString(strconv.FormatUint(uint64(ordinal), 10))
	return h
}

func exploreHashForPlan(identity AttemptPlanIdentity, ordinal uint8) uint64 {
	return ExploreHash("explore", identity.RequestID, identity.UserID, identity.RouteClassID, identity.RoutingGeneration, ordinal)
}

func NewAttemptPlan(identity AttemptPlanIdentity, decision RouteDecision) *AttemptPlan {
	p := &AttemptPlan{identity: identity}
	for _, accountID := range decision.Primary {
		p.addCandidate(accountID, AttemptLanePrimary)
	}
	if len(decision.Explore.IDs) > 0 && decision.Explore.Total > 0 && len(decision.Explore.Cumulative) == len(decision.Explore.IDs) {
		hash := exploreHashForPlan(identity, 0)
		ticket := hash % decision.Explore.Total
		idx := sort.Search(len(decision.Explore.Cumulative), func(i int) bool {
			return decision.Explore.Cumulative[i] > ticket
		})
		if idx >= 0 && idx < len(decision.Explore.IDs) {
			p.addCandidate(decision.Explore.IDs[idx], AttemptLaneExplore)
		}
		for _, accountID := range decision.Explore.Fallback {
			p.addCandidate(accountID, AttemptLaneExplore)
		}
	} else {
		if len(decision.Explore.IDs) > 0 {
			p.addCandidate(decision.Explore.IDs[0], AttemptLaneExplore)
		}
		for _, accountID := range decision.Explore.Fallback {
			p.addCandidate(accountID, AttemptLaneExplore)
		}
	}
	for _, accountID := range decision.Degraded {
		p.addCandidate(accountID, AttemptLaneDegraded)
	}
	return p
}

func (p *AttemptPlan) Identity() AttemptPlanIdentity { return p.identity }

func (p *AttemptPlan) Reserve(reserve AttemptReservation) (Attempt, error) {
	return p.reserve(func(candidate attemptPlanCandidate) bool {
		return reserve(candidate.accountID)
	})
}

func (p *AttemptPlan) reserve(reserve func(attemptPlanCandidate) bool) (Attempt, error) {
	if p.candidateCount == 0 {
		return Attempt{}, ErrNoAvailable
	}
	for p.cursor < p.candidateCount {
		candidate := p.candidates[p.cursor]
		p.cursor++
		if !reserve(candidate) {
			continue
		}
		p.attempted[p.attemptedCount] = candidate.accountID
		p.attemptedCount++
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
