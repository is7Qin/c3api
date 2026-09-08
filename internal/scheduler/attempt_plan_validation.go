// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"errors"
	"fmt"
)

var ErrInvalidRouteDecision = errors.New("scheduler: invalid route decision")

// InvalidRouteDecisionError identifies a malformed published route before a
// request-local plan can borrow its fixed dedupe storage.
type InvalidRouteDecisionError struct {
	Field         string
	Index         int
	AccountID     int64
	FallbackIndex uint16
}

func (e *InvalidRouteDecisionError) Error() string {
	if e == nil {
		return ErrInvalidRouteDecision.Error()
	}
	return fmt.Sprintf("%s: %s", ErrInvalidRouteDecision, e.Field)
}

func (e *InvalidRouteDecisionError) Unwrap() error { return ErrInvalidRouteDecision }

func validateRouteDecision(decision *RouteDecision) error {
	if decision == nil {
		return &InvalidRouteDecisionError{Field: "decision", Index: -1}
	}
	for i, candidate := range decision.Primary {
		if containsCandidateID(decision.Primary[:i], candidate.AccountID) {
			return &InvalidRouteDecisionError{Field: fmt.Sprintf("primary[%d]", i), Index: i, AccountID: candidate.AccountID}
		}
	}
	for i, candidate := range decision.Explore.Ordered {
		if containsCandidateID(decision.Primary, candidate.AccountID) || containsCandidateID(decision.Explore.Ordered[:i], candidate.AccountID) {
			return &InvalidRouteDecisionError{Field: fmt.Sprintf("explore.ordered[%d]", i), Index: i, AccountID: candidate.AccountID}
		}
	}
	for i, candidate := range decision.Degraded {
		if containsCandidateID(decision.Primary, candidate.AccountID) || containsCandidateID(decision.Explore.Ordered, candidate.AccountID) || containsCandidateID(decision.Degraded[:i], candidate.AccountID) {
			return &InvalidRouteDecisionError{Field: fmt.Sprintf("degraded[%d]", i), Index: i, AccountID: candidate.AccountID}
		}
	}
	for i, fallbackIndex := range decision.Explore.Fallback {
		if int(fallbackIndex) >= len(decision.Explore.Ordered) {
			return &InvalidRouteDecisionError{Field: fmt.Sprintf("explore.fallback[%d]", i), Index: i, FallbackIndex: fallbackIndex}
		}
		for j := 0; j < i; j++ {
			if decision.Explore.Fallback[j] == fallbackIndex {
				return &InvalidRouteDecisionError{Field: fmt.Sprintf("explore.fallback[%d]", i), Index: i, FallbackIndex: fallbackIndex}
			}
		}
	}
	explore := decision.Explore
	if len(explore.Ordered) == 0 {
		if len(explore.Weights) != 0 || len(explore.Cumulative) != 0 || explore.Total != 0 || len(explore.Fallback) != 0 {
			return &InvalidRouteDecisionError{Field: "explore.empty"}
		}
		return nil
	}
	if len(explore.Cumulative) != len(explore.Ordered) {
		return &InvalidRouteDecisionError{Field: "explore.cumulative"}
	}
	var previous uint64
	for i, candidate := range explore.Ordered {
		weight, ok := explore.Weights[candidate.AccountID]
		if !ok || weight <= 0 {
			return &InvalidRouteDecisionError{Field: fmt.Sprintf("explore.weights[%d]", candidate.AccountID), Index: i, AccountID: candidate.AccountID}
		}
		cumulative := explore.Cumulative[i]
		if cumulative <= previous {
			return &InvalidRouteDecisionError{Field: fmt.Sprintf("explore.cumulative[%d]", i), Index: i}
		}
		if cumulative-previous != uint64(weight) {
			return &InvalidRouteDecisionError{Field: fmt.Sprintf("explore.cumulative[%d]", i), Index: i}
		}
		previous = cumulative
	}
	if len(explore.Weights) != len(explore.Ordered) {
		return &InvalidRouteDecisionError{Field: "explore.weights"}
	}
	if explore.Total == 0 || explore.Total != previous {
		return &InvalidRouteDecisionError{Field: "explore.total"}
	}
	return nil
}

func containsCandidateID(candidates []CompiledCandidate, accountID int64) bool {
	for _, candidate := range candidates {
		if candidate.AccountID == accountID {
			return true
		}
	}
	return false
}
