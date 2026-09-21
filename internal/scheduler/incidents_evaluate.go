// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"

	"github.com/is7qin/c3api/internal/domain"
)

// Incident evaluation (incident-wiring): DETECT + SURFACE ONLY — no
// lane reordering, no throttling, no probe triggering. The pure vote
// (evaluateRouteIncident) resolves per-candidate evidence from the windowed
// current + PG baseline; the lane tracker (incidentTracker, serial compile
// lane only) applies the 2-cycle state machine with input-change gating:
// a cycle is one evaluation where (evaluated minute, quality evidence)
// advanced — re-fires with identical inputs freeze (no streak burn), and an
// idle fleet freezes incident state.
//
// Comparability mirrors the classify gate: current AND baseline need
// attempts ≥ 30 AND TTFT samples ≥ 30 (stream-only TTFT contract); failures
// and unparseable origins ABSTAIN (excluded, never counted healthy). Sparse
// routes (< 2 comparable domains) cast healthy votes through the ordinary
// Update(false) path — two fresh healthy evaluations recover.

const (
	IncidentKindDomain = "domain"
	IncidentKindModel  = "model"
	IncidentKindBoth   = "both"
)

// RouteIncident is the expose-only incident mark on a route decision and plan
// route. Zero value = no incident.
type RouteIncident struct {
	Active          bool
	Kind            string // "" | "domain" | "model" | "both" ("" when inactive)
	Comparable      int
	Degraded        int
	Domains         int
	EvaluatedMinute int64
}

// incidentVote is one route's pure evaluation result: corroborated degradation
// plus the evidence hash that gates lane cycles.
type incidentVote struct {
	cands         []IncidentCandidate
	evidence      [32]byte
	degraded      bool
	kind          string
	comparable    int
	degradedCount int
	domains       int
}

// IncidentEvalFunc stamps one route's incident. Nil in direct-compile tests
// (zero incident); the compile lane always installs the tracker closure so
// full and scoped fires share the identical transition function.
type IncidentEvalFunc func(route RouteRef, vote incidentVote, minute int64) RouteIncident

// evaluateRouteIncident resolves the pure per-route vote from filtered static
// facts, the windowed current map, and the PG baseline map. Deterministic in,
// deterministic out (candidates sorted by full evidence before hashing).
func evaluateRouteIncident(facts []compilerCandidateFacts, routeRC domain.RouteClassIDVal, cur map[CandidateQualityKey]CandidateQualityInput, base map[CandidateQualityKey]Counts) incidentVote {
	type evidenceRow struct {
		domain                      string
		comparable                  bool
		degraded                    bool
		curAttempts, curSuccesses   int
		curTTFT                     int
		baseAttempts, baseSuccesses int
		baseTTFT                    int
	}
	rows := make([]evidenceRow, 0, len(facts))
	for _, f := range facts {
		key := CandidateQualityKey{RouteClassID: routeRC, Fingerprint: f.identityFingerprint}
		var cc Counts
		if qin, ok := cur[key]; ok {
			cc = qin.Counts
		}
		bc, ok := base[key]
		if !ok {
			bc = Counts{}
		}
		origin, err := CanonicalOrigin(f.baseURL)
		if err != nil {
			continue // empty/unparseable origin abstains fail-closed
		}
		// Comparability is attempts-only (charter: 双方n>=30, n = attempts).
		// TTFT is deliberately NOT gated: incident detection is a success-rate
		// signal, and non-streaming traffic (the bulk of gateway volume) carries
		// no TTFT samples — gating on it would blind the system to non-stream
		// outages. (Lane promotion still requires TTFT for "fastest" ranking;
		// that is a different question with different data needs.)
		comparable := cc.Attempts >= 30 && bc.Attempts >= 30
		degraded := false
		if comparable {
			degraded = IsDegraded(
				Wilson95(cc.Successes, cc.Attempts),
				Wilson95(bc.Successes, bc.Attempts),
				cc.Attempts, bc.Attempts)
		}
		rows = append(rows, evidenceRow{
			domain: origin, comparable: comparable, degraded: degraded,
			curAttempts: cc.Attempts, curSuccesses: cc.Successes, curTTFT: cc.TTFTCount,
			baseAttempts: bc.Attempts, baseSuccesses: bc.Successes, baseTTFT: bc.TTFTCount,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.domain != b.domain {
			return a.domain < b.domain
		}
		if a.curAttempts != b.curAttempts {
			return a.curAttempts < b.curAttempts
		}
		if a.baseAttempts != b.baseAttempts {
			return a.baseAttempts < b.baseAttempts
		}
		if a.curSuccesses != b.curSuccesses {
			return a.curSuccesses < b.curSuccesses
		}
		return a.baseSuccesses < b.baseSuccesses
	})
	vote := incidentVote{}
	h := sha256.New()
	var scratch [binary.MaxVarintLen64]byte
	putInt := func(v int) {
		n := binary.PutVarint(scratch[:], int64(v))
		h.Write(scratch[:n])
	}
	for _, r := range rows {
		vote.cands = append(vote.cands, IncidentCandidate{Domain: r.domain, Comparable: r.comparable, Degraded: r.degraded})
		h.Write([]byte(r.domain))
		h.Write([]byte{0})
		if r.comparable {
			h.Write([]byte{1})
			vote.comparable++
		} else {
			h.Write([]byte{0})
		}
		if r.degraded {
			h.Write([]byte{1})
			vote.degradedCount++
		} else {
			h.Write([]byte{0})
		}
		putInt(r.curAttempts)
		putInt(r.curSuccesses)
		putInt(r.curTTFT)
		putInt(r.baseAttempts)
		putInt(r.baseSuccesses)
		putInt(r.baseTTFT)
	}
	copy(vote.evidence[:], h.Sum(nil))
	seen := make(map[string]struct{})
	for _, r := range rows {
		if !r.comparable {
			continue
		}
		seen[r.domain] = struct{}{}
	}
	vote.domains = len(seen)
	domainFired := DomainIncident(vote.cands)
	modelFired := ModelIncident(vote.cands)
	vote.degraded = domainFired || modelFired
	switch {
	case domainFired && modelFired:
		vote.kind = IncidentKindBoth
	case domainFired:
		vote.kind = IncidentKindDomain
	case modelFired:
		vote.kind = IncidentKindModel
	}
	return vote
}

// incidentEvalKey is one route's last evaluation inputs: identical
// (minute, evidence) means no new evidence — the state update is skipped.
type incidentEvalKey struct {
	minute   int64
	evidence [32]byte
}

// incidentTracker is the per-route incident state. Owner: serial compile
// lane only (no lock); created in Scheduler.New.
type incidentTracker struct {
	states map[RouteRef]IncidentState
	out    map[RouteRef]RouteIncident
	active map[RouteRef]RouteIncident
	keys   map[RouteRef]incidentEvalKey
}

func newIncidentTracker() *incidentTracker {
	return &incidentTracker{
		states: make(map[RouteRef]IncidentState),
		out:    make(map[RouteRef]RouteIncident),
		active: make(map[RouteRef]RouteIncident),
		keys:   make(map[RouteRef]incidentEvalKey),
	}
}

func (t *incidentTracker) evaluate(route RouteRef, vote incidentVote, minute int64) RouteIncident {
	if t == nil {
		return RouteIncident{}
	}
	key := incidentEvalKey{minute: minute, evidence: vote.evidence}
	if prev, ok := t.keys[route]; ok && prev == key {
		if last, ok := t.out[route]; ok {
			return last
		}
		return RouteIncident{}
	}
	st := t.states[route]
	nowActive := st.Update(vote.degraded)
	t.states[route] = st
	var res RouteIncident
	if nowActive {
		if vote.degraded {
			res = RouteIncident{
				Active: true, Kind: vote.kind,
				Comparable: vote.comparable, Degraded: vote.degradedCount,
				Domains: vote.domains, EvaluatedMinute: minute,
			}
			t.active[route] = res
		} else if det, ok := t.active[route]; ok {
			res = det // recovery streak: detection evidence stands until cleared
		}
	} else {
		delete(t.active, route)
	}
	t.keys[route] = key
	t.out[route] = res
	return res
}

// pruneAlive drops tracker entries for routes gone from a full compile.
// Scoped fires never prune (carry-forward keeps survivors verbatim).
func (t *incidentTracker) pruneAlive(keep map[RouteRef]*RouteDecision) {
	if t == nil {
		return
	}
	for r := range t.states {
		if _, ok := keep[r]; !ok {
			delete(t.states, r)
			delete(t.out, r)
			delete(t.active, r)
			delete(t.keys, r)
		}
	}
}

func (t *incidentTracker) activeCount() int {
	if t == nil {
		return 0
	}
	n := 0
	for _, s := range t.states {
		if s.Active {
			n++
		}
	}
	return n
}

// validateRouteIncident enforces the incident value contract: kind enum,
// degraded ≤ comparable, non-negative evidence, and the zero value when
// inactive.
func validateRouteIncident(inc RouteIncident) error {
	if !inc.Active {
		if inc.Kind != "" || inc.Comparable != 0 || inc.Degraded != 0 || inc.Domains != 0 || inc.EvaluatedMinute != 0 {
			return &InvalidRouteDecisionError{Field: "incident.inactive"}
		}
		return nil
	}
	switch inc.Kind {
	case IncidentKindDomain, IncidentKindModel, IncidentKindBoth:
	default:
		return &InvalidRouteDecisionError{Field: "incident.kind"}
	}
	if inc.Comparable < 0 || inc.Degraded < 0 || inc.Domains < 0 || inc.EvaluatedMinute < 0 {
		return &InvalidRouteDecisionError{Field: "incident.counts"}
	}
	if inc.Degraded > inc.Comparable {
		return &InvalidRouteDecisionError{Field: "incident.degraded"}
	}
	return nil
}
