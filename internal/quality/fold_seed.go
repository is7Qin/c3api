// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

// Fold-at-source seeds (v3): the unexported per-edge input shared by the
// request walk (FoldChain closure values) and the synchronous consumer seam
// (rows). One mapping function serves both paths — the string tables in
// fold_fact.go are the single source, never duplicated.

import (
	"errors"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// foldSeed is the unexported per-edge input shared by the request walk
// (FoldChain closure values) and the synchronous consumer seam (rows).
type foldSeed struct {
	route       domain.RouteClassIDVal
	accountID   int64
	prevAccount int64
	generation  int64
	ordinal     uint8
	lane        string
	outcome     string
	prevOutcome string
	transition  string
	terminal    bool
	hasPrev     bool
}

// makeFact maps one seed onto its packed fact. The string tables are the
// single source for every path (request walk and consumer seam alike); an
// empty transition derives init/failover from the ordinal (request walk —
// terminal facts mirror the old ordinal-1-initial rule), while an explicit
// transition maps directly (consumer rows carry retry/flow/plan). Anything
// outside the codebooks is a programming error.
func makeFact(seed foldSeed, bucket int64) (attemptFact, error) {
	laneCode, ok := foldLaneCode(seed.lane)
	if !ok {
		return attemptFact{}, errors.New("fold: unknown lane")
	}
	outcomeCode, ok := foldOutcomeCode(seed.outcome)
	if !ok {
		return attemptFact{}, errors.New("fold: unknown outcome")
	}
	var prevCode uint8
	hasPrevOutcome := seed.prevOutcome != ""
	if hasPrevOutcome {
		var ok bool
		prevCode, ok = foldOutcomeCode(seed.prevOutcome)
		if !ok {
			return attemptFact{}, errors.New("fold: unknown previous outcome")
		}
	}
	var transition uint8
	if seed.transition == "" {
		if seed.ordinal == 1 {
			transition = foldTransitionInit
		} else {
			transition = foldTransitionFailover
		}
	} else {
		var ok bool
		transition, ok = foldTransitionCode(seed.transition)
		if !ok {
			return attemptFact{}, errors.New("fold: unknown transition")
		}
	}
	if seed.ordinal == 0 || seed.ordinal > foldMaxOrdinal {
		return attemptFact{}, errors.New("fold: ordinal out of bounds")
	}
	return canonicalFact(attemptFact{
		route:           seed.route,
		accountID:       seed.accountID,
		prevAccount:     seed.prevAccount,
		generation:      seed.generation,
		minuteBucket:    bucket,
		ordinal:         seed.ordinal,
		lane:            laneCode,
		outcome:         outcomeCode,
		prevOutcome:     prevCode,
		transition:      transition,
		identityVersion: domain.RoutingIdentityVersion,
		isTerminal:      seed.terminal,
		hasPrev:         seed.hasPrev,
		hasPrevOutcome:  hasPrevOutcome,
	}), nil
}

// rowToSeed projects one consumer row onto a seed plus its event count.
// ChainCount is the event count (N events fold N adds — increment==event);
// an unspecified (zero) count folds one event — a row exists, so it carries
// at least one. Identity outside the codebooks or version drift is a
// programming error, never a silent widening.
func rowToSeed(row repository.RoutingFlowRow) (foldSeed, int64, error) {
	delta := row.ChainCount
	if delta < 1 {
		delta = 1
	}
	if row.Ordinal < 1 || row.Ordinal > foldMaxOrdinal {
		return foldSeed{}, 0, errors.New("fold: ordinal out of bounds")
	}
	if row.IdentityVersion != int16(domain.RoutingIdentityVersion) {
		return foldSeed{}, 0, errors.New("fold: identity version mismatch")
	}
	if row.Lane == "" || row.TransitionReason == "" || row.Outcome == "" {
		return foldSeed{}, 0, errors.New("fold: lane/transition/outcome required")
	}
	if _, ok := foldLaneCode(row.Lane); !ok {
		return foldSeed{}, 0, errors.New("fold: unknown lane")
	}
	if _, ok := foldOutcomeCode(row.Outcome); !ok {
		return foldSeed{}, 0, errors.New("fold: unknown outcome")
	}
	if _, ok := foldTransitionCode(row.TransitionReason); !ok {
		return foldSeed{}, 0, errors.New("fold: unknown transition")
	}
	if row.PreviousOutcome != "" {
		if _, ok := foldOutcomeCode(row.PreviousOutcome); !ok {
			return foldSeed{}, 0, errors.New("fold: unknown previous outcome")
		}
	}
	seed := foldSeed{
		route:       row.RouteClassID,
		accountID:   row.AccountID,
		generation:  row.Generation,
		ordinal:     uint8(row.Ordinal),
		lane:        row.Lane,
		outcome:     row.Outcome,
		prevOutcome: row.PreviousOutcome,
		transition:  row.TransitionReason,
		terminal:    row.IsTerminal,
	}
	if row.PreviousAccountID != nil {
		seed.prevAccount = *row.PreviousAccountID
		seed.hasPrev = true
	}
	return seed, delta, nil
}
