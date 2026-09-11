// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

// Fold-at-source facts (v3 F1, spec docs/superpowers/specs/flow-fold-at-source-v3.md
// §4–§5.1): the per-attempt fact replaces the per-request FlowChain heap box.
// An attemptFact is fixed-size, stack-passed BY VALUE, and doubles as the
// counter-cell table key (comparable struct — no strings, no hex/sha256, no
// pointers anywhere on the path). minuteBucket is minted ONCE at the
// completion walk and stamped on every fact of the chain, so all cells of one
// chain expand under a single TerminalMinute. residual is NOT a fact field —
// it is stamped at Add time by the seal check (§4 seal split).
//
// OWNERSHIP: facts are request-local stack values (proxy fold owner);
// counter cells are owned request-Add / tick-drain (fold_cells.go); per-minute
// cumulative shells are tick-owned (fold_expand.go). No structure here is
// shared ambiguously: each has exactly one writer per lifecycle phase.

import (
	"sync/atomic"

	"github.com/is7qin/c3api/internal/domain"
)

// Codebooks (normative §4 value tables — expansion maps codes back to today's
// exact row strings; any value outside these tables is a programming error).
// lane: 0=primary, 1=explore, 2=degraded.
// outcome/prevOutcome (shared): 0=success (incl. rule-ok), 1=error, 2=4xx,
// 3=429, 4=5xx, 5=network, 6=client_cancel (verbatim census string, v3-F1
// amendment: proxy emits it live via ResultClientCancel, old rows carry it).
// transition: 0=initial-or-init, 1=failover, 2=retry, 3=flow, 4=plan.
// ordinal 1–8 direct; isTerminal/hasPrev single bits.
const (
	foldLanePrimary uint8 = iota
	foldLaneExplore
	foldLaneDegraded
)

const (
	foldOutcomeSuccess uint8 = iota
	foldOutcomeError
	foldOutcome4xx
	foldOutcome429
	foldOutcome5xx
	foldOutcomeNetwork
	foldOutcomeClientCancel
)

const (
	foldTransitionInit uint8 = iota
	foldTransitionFailover
	foldTransitionRetry
	foldTransitionFlow
	foldTransitionPlan
)

const foldMaxOrdinal = 8

// attemptFact is the stack-passed per-attempt fact AND the counter-cell key.
type attemptFact struct {
	route           domain.RouteClassIDVal
	fingerprint     domain.CandidateFingerprintVal
	accountID       int64
	prevAccount     int64
	generation      int64
	minuteBucket    int64
	ordinal         uint8
	lane            uint8
	outcome         uint8
	prevOutcome     uint8
	transition      uint8
	identityVersion uint8
	isTerminal      bool
	hasPrev         bool
	// hasPrevOutcome records previous-outcome-token presence independently
	// of linkage: the old rows carry PreviousAccountID set with an empty
	// PreviousOutcome (first-stashed ordinal>1 edge) and vice versa, so
	// byte-identity needs both bits. Absent token expands to "" — never to
	// the zero-code string.
	hasPrevOutcome bool
	residual       bool
}

// canonicalFact forces the canonical linkage form: without a predecessor
// the carried account is zeroed, and without a token the carried code is
// zeroed, so keys cannot fragment on ignored values. Set predecessors and
// tokens are preserved verbatim at any ordinal (the old rows carried
// ordinal-1 predecessors in tests — byte-identical means keeping them).
func canonicalFact(f attemptFact) attemptFact {
	if !f.hasPrev {
		f.prevAccount = 0
	}
	if !f.hasPrevOutcome {
		f.prevOutcome = 0
	}
	return f
}

// Backward maps: code -> today's exact row string (byte-identical contract).
// Transition code 0 expands to "initial": the normative base tree (cca67f8)
// carries "initial" on every request-path edge (proxy dispatch shaping,
// pipeline producer tests, routing e2e vectors) — where the spec table's
// `init` label and the tree differ, the tree governs (spec §0). "init" is
// accepted on input as an alias and canonicalizes to the same cell.

func foldLaneString(lane uint8) (string, bool) {
	switch lane {
	case foldLanePrimary:
		return "primary", true
	case foldLaneExplore:
		return "explore", true
	case foldLaneDegraded:
		return "degraded", true
	}
	return "", false
}

func foldOutcomeString(outcome uint8) (string, bool) {
	switch outcome {
	case foldOutcomeSuccess:
		return "success", true
	case foldOutcomeError:
		return "error", true
	case foldOutcome4xx:
		return "4xx", true
	case foldOutcome429:
		return "429", true
	case foldOutcome5xx:
		return "5xx", true
	case foldOutcomeNetwork:
		return "network", true
	case foldOutcomeClientCancel:
		return "client_cancel", true
	}
	return "", false
}

func foldTransitionString(transition uint8) (string, bool) {
	switch transition {
	case foldTransitionInit:
		return "initial", true
	case foldTransitionFailover:
		return "failover", true
	case foldTransitionRetry:
		return "retry", true
	case foldTransitionFlow:
		return "flow", true
	case foldTransitionPlan:
		return "plan", true
	}
	return "", false
}

// Forward maps: row/token string -> code. "client_cancel" keeps its own code
// (verbatim round-trip: proxy emits it live via ResultClientCancel and old
// rows carry it, so folding it into error would break census byte-identity).
// "ok" folds into success per the codebook's incl.-rule-ok note.
func foldLaneCode(s string) (uint8, bool) {
	switch s {
	case "primary":
		return foldLanePrimary, true
	case "explore":
		return foldLaneExplore, true
	case "degraded":
		return foldLaneDegraded, true
	}
	return 0, false
}

func foldOutcomeCode(s string) (uint8, bool) {
	switch s {
	case "success", "ok":
		return foldOutcomeSuccess, true
	case "error":
		return foldOutcomeError, true
	case "client_cancel":
		return foldOutcomeClientCancel, true
	case "4xx":
		return foldOutcome4xx, true
	case "429":
		return foldOutcome429, true
	case "5xx":
		return foldOutcome5xx, true
	case "network":
		return foldOutcomeNetwork, true
	}
	return 0, false
}

func foldTransitionCode(s string) (uint8, bool) {
	switch s {
	case "initial", "init":
		return foldTransitionInit, true
	case "failover":
		return foldTransitionFailover, true
	case "retry":
		return foldTransitionRetry, true
	case "flow":
		return foldTransitionFlow, true
	case "plan":
		return foldTransitionPlan, true
	}
	return 0, false
}

// Counting table (§5.1): the same three unexported counters the chain
// maintained. Cap-overflow and incomplete keep their global atomics;
// table-full (shard overflow) sums the per-shard counters (fold_cells.go),
// so the service loss seam (Capacity+Enqueue) keeps working unchanged.
var flowChainCapacityOverflow atomic.Int64
var flowChainIncomplete atomic.Int64

const processCrashLossUnobservable = true

func FlowChainCapacityOverflow() int64   { return flowChainCapacityOverflow.Load() }
func FlowChainIncompleteObserved() int64 { return flowChainIncomplete.Load() }
func ProcessCrashLossUnobservable() bool { return processCrashLossUnobservable }

func ResetFlowChainCountersForTest() {
	flowChainCapacityOverflow.Store(0)
	flowChainIncomplete.Store(0)
	flowChainEnqueueOverflow.Store(0)
}
