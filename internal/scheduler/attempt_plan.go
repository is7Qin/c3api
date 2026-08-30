// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/is7qin/c3api/internal/domain"
)

const MaxAttemptPlanAccounts = 8

var ErrAttemptsExhausted = errors.New("scheduler: attempt plan exhausted")

type AttemptPlanIdentity struct {
	RequestID         string
	UserID            int64
	RouteClassID      string
	RoutingGeneration uint64
	// MaxAttempts bounds successful dispatches (1..8). 0 means unset and
	// falls back to the array bound MaxAttemptPlanAccounts; values >8 clamp
	// to 8. The proxy stamps its normalized proxy.failover_attempts here.
	MaxAttempts       uint8
	ApplyModelMapping bool
}

type AttemptLane string

const (
	AttemptLanePrimary  AttemptLane = "primary"
	AttemptLaneExplore  AttemptLane = "explore"
	AttemptLaneDegraded AttemptLane = "degraded"
)

func (l AttemptLane) Valid() bool {
	switch l {
	case AttemptLanePrimary, AttemptLaneExplore, AttemptLaneDegraded:
		return true
	}
	return false
}

type Attempt struct {
	AttemptID            string
	RouteClassID         string
	QualityClassID       string
	CandidateFingerprint string
	TemplateID           int64
	AccountID            int64
	RequestedModel       string
	MappedModel          string
	Lane                 AttemptLane
	Ordinal              uint8
	RoutingGeneration    uint64
	LifecycleRevision    int64
	PreviousAttemptID    *string
	PreviousAccountID    *int64
	CallerCategory       string
	OperationTag         string
}

func (a Attempt) Validate() error {
	if a.AttemptID == "" {
		return fmt.Errorf("AttemptID required")
	}
	if a.RouteClassID == "" {
		return fmt.Errorf("RouteClassID required")
	}
	if a.QualityClassID == "" {
		return fmt.Errorf("QualityClassID required")
	}
	if a.CandidateFingerprint == "" {
		return fmt.Errorf("CandidateFingerprint required")
	}
	if a.TemplateID == 0 {
		return fmt.Errorf("TemplateID required")
	}
	if a.AccountID == 0 {
		return fmt.Errorf("AccountID required")
	}
	if a.RequestedModel == "" {
		return fmt.Errorf("RequestedModel required")
	}
	if a.MappedModel == "" {
		return fmt.Errorf("MappedModel required")
	}
	if !a.Lane.Valid() {
		return fmt.Errorf("Lane must be valid")
	}
	if a.Ordinal == 0 {
		return fmt.Errorf("Ordinal must be >0")
	}
	if a.RoutingGeneration == 0 {
		return fmt.Errorf("RoutingGeneration must be >0")
	}
	if a.LifecycleRevision <= 0 {
		return fmt.Errorf("LifecycleRevision must be >0")
	}
	if a.CallerCategory == "" {
		return fmt.Errorf("CallerCategory required")
	}
	if c := domain.CallerKind(a.CallerCategory); !c.Valid() {
		return fmt.Errorf("CallerCategory invalid %q", a.CallerCategory)
	}
	if a.OperationTag == "" {
		return fmt.Errorf("OperationTag required")
	}
	if op := domain.OperationTag(a.OperationTag); !op.Valid() {
		return fmt.Errorf("OperationTag invalid %q", a.OperationTag)
	}
	if a.Ordinal == 1 && a.PreviousAttemptID != nil {
		return fmt.Errorf("PreviousAttemptID must be nil for ordinal 1")
	}
	if a.Ordinal > 1 {
		if a.PreviousAttemptID == nil || *a.PreviousAttemptID == "" {
			return fmt.Errorf("PreviousAttemptID required for ordinal >1")
		}
	}
	return nil
}

type AttemptReservation func(accountID int64) bool

type attemptPlanCandidate struct {
	accountID         int64
	lane              AttemptLane
	account           *accountSnapshot
	static            *snapshotStatic
	fingerprint       string
	quality           string
	templateID        int64
	requestedModel    string
	mappedModel       string
	mappingMode       domain.ModelMappingMode
	routeClassID      string
	callerCategory    string
	operationTag      string
	lifecycleRevision int64
	routingGeneration uint64
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
