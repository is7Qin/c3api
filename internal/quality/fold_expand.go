// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

// Tick-time expansion core (v3 F2, spec §5.2): the existing 2Hz/0.2Hz ticks
// expand each counter cell into today's identical RoutingFlowRows. Only the
// expansion SOURCE changed (cells, not chains); cadence, sinks, and the
// Upsert shape are untouched.
//
// OWNERSHIP: per-minute cumulative shells are TICK-OWNED (o.mu). Requests
// never touch them. Every tick-owned read folds pending cells first
// (drainFoldLocked), so cells (request deltas) and shells (cumulative view)
// are two phases of ONE mechanism, not two mechanisms: submitters only Add,
// the tick owner folds — no submitter ever reads-modifies-writes a row.
//
// Field derivation (normative §5.2 table): account/prevAccount+hasPrev/
// lane/outcome/transition/ordinal/generation/version/fingerprint direct from
// the fact; ChainCount = cell SUM at expansion (exact — atomic adds, never
// estimated, never sampled); TerminalMinute = minuteBucket (same bucket, same
// overwrite as today); InstanceSrc = self and AbsoluteSequence = the
// SyncWorker per-(InstanceSrc, minute) sequence, both stamped by the existing
// flowRowsFromMinute flush path (flush stamps only — the tick never mints,
// the request path never mints). IsTerminal follows option (a): the terminal
// edge's fact is emitted at completion, when terminality is final — there is
// never a provisional increment.

import (
	"sort"
	"time"

	"github.com/is7qin/c3api/internal/repository"
)

const (
	minuteWidth           = time.Minute
	flowLateCutoffSeconds = 600
	// foldShellRetainSeconds bounds tick-owned shell retention: clean
	// unleased shells older than this behind the newest observed minute are
	// reclaimed (their counts are durable in PG — clean means acked). The
	// window must exceed the 30-minute idle-merge contract pinned by the
	// prune test (old rows merge into later replacements); it is measured
	// behind the newest shell minute — never wall-clock — so week-old test
	// minutes cluster safely while production tails stay bounded.
	foldShellRetainSeconds = 3600
)

// foldShell is the tick-owned cumulative state for one minute: per-edge
// counts keyed by the packed fact, legacy whole-minute arrays preserved
// verbatim, the empty marker, and the lease/version/dirty handshake the
// sync flush settles through. Zero-count entries are deleted at fold time
// (hygiene); cumulative counts are otherwise retained until the minute is
// clean, past the horizon, and unleased (tick reclamation — the sole
// reclamation alongside cell clear-after-expansion).
type foldShell struct {
	minute      int64
	counts      map[attemptFact]int64
	edges       [8]int64
	counts8     [8]int64
	emptyMarked bool

	leased  bool
	leaseID uint64
	version uint64
	dirty   bool

	everPersisted      bool
	acceptedContrib    int64
	persistedWatermark int64
}

// foldSnapshotToken binds minute, lease identity, captured version, and the
// accepted watermark. Private value passed from snapshotForPG to ackPG /
// releasePG through sync; never persisted or logged with row contents.
type foldSnapshotToken struct {
	minute            int64
	leaseID           uint64
	capturedVersion   uint64
	acceptedWatermark int64
}

// getOrCreateShell returns the tick-owned shell for a minute, creating it on
// demand. Shells are never capacity-rejected (no eviction); clean shells past
// the horizon are reclaimed by the tick, never by pressure.
func (o *FlowOwner) getOrCreateShell(minute int64) *foldShell {
	if shell, ok := o.shells[minute]; ok {
		return shell
	}
	shell := &foldShell{minute: minute, counts: make(map[attemptFact]int64)}
	o.shells[minute] = shell
	return shell
}

// drainFoldLocked folds every pending cell into its minute shell exactly
// once, then reclaims clean past-horizon shells. Caller must hold o.mu.
// accepted folds mark the minute dirty (a PG candidate); residual folds and
// the seal-sweep path never dirty. A fold whose add-class (key residual bit)
// disagrees with the fold-class (sealed now) reconciles the class counters —
// the seal race nets exactly.
func (o *FlowOwner) drainFoldLocked() {
	start := o.rec.now()
	curMinute := o.horizonNow().UTC().Truncate(minuteWidth).Unix()
	o.drainSeq++
	seq := o.drainSeq
	sealed := o.sealed.Load()
	o.cells.drain(curMinute, seq, func(f attemptFact, count int64) {
		// No absent-shell age guard here by design (v3-F1 review finding 3,
		// disputed with evidence): admission is replay-tolerant — buckets are
		// minted at completion or carried from live FlowMinutes, so production
		// has no ancient-minute producer, and the ingest seams already refuse
		// old-absent minutes when the owner clock is explicit
		// (fold_consume.go admissionCutoff). A drain-time refusal would break
		// the pinned replay contract (TestQualitySync_EmptyFlowSnapshotPreserved
		// ingests fixed old minutes under a live clock). Zero-count entries
		// are deleted at fold, so a rebuilt shell always carries the folded
		// counts — never an empty shell over PG-held cumulatives.
		shell := o.getOrCreateShell(f.minuteBucket)
		shell.emptyMarked = false
		shell.counts[f] += count
		if shell.counts[f] == 0 {
			delete(shell.counts, f)
		}
		shell.version++
		if f.residual {
			// Post-seal origin: already tallied at Add, never dirty, never
			// processed as a submission-equivalent.
			return
		}
		o.processed.Add(count)
		if sealed {
			// Pre-seal origin folded under seal: reconcile to residual.
			o.edgeRowsAccepted.Add(-count)
			o.residualRows.Add(count)
			return
		}
		shell.dirty = true
		shell.acceptedContrib += count
	})
	o.lastMergeMs.Store(o.rec.now().Sub(start).Milliseconds())
	if !sealed {
		var newest int64
		for minute := range o.shells {
			if minute > newest {
				newest = minute
			}
		}
		for minute, shell := range o.shells {
			if !shell.dirty && !shell.leased && minute < newest-foldShellRetainSeconds {
				delete(o.shells, minute)
			}
		}
	}
}

// materializeShell projects one shell into a fresh *FlowMinute tick scratch:
// per-edge counts become rows with ChainCount = the exact folded sum, sorted
// deterministically (ordinal, lane, account, linkage, outcome…). The scratch
// is allocated fresh per tick and GC-freed after Upsert — never pooled, never
// request-visible. TerminalMinute/InstanceSrc/AbsoluteSequence are flush
// stamps applied by flowRowsFromMinute, as today.
func materializeShell(shell *foldShell) *FlowMinute {
	fm := &FlowMinute{
		minute:        shell.minute,
		edges:         shell.edges,
		counts:        shell.counts8,
		emptySnapshot: shell.emptyMarked && len(shell.counts) == 0 && !hasLegacyCounts(shell),
	}
	if len(shell.counts) > 0 {
		fm.flowRows = make([]repository.RoutingFlowRow, 0, len(shell.counts))
		terminalMinute := time.Unix(shell.minute, 0).UTC()
		for k, c := range shell.counts {
			lane, _ := foldLaneString(k.lane)
			outcome, _ := foldOutcomeString(k.outcome)
			// No token means an empty previous outcome (never the
			// zero-code string): byte-identical with the old rows.
			prevOutcome := ""
			if k.hasPrevOutcome {
				prevOutcome, _ = foldOutcomeString(k.prevOutcome)
			}
			transition, _ := foldTransitionString(k.transition)
			row := repository.RoutingFlowRow{
				IdentityVersion:      int16(k.identityVersion),
				RouteClassID:         k.route,
				TerminalMinute:       terminalMinute,
				Ordinal:              int16(k.ordinal),
				Lane:                 lane,
				AccountID:            k.accountID,
				PreviousOutcome:      prevOutcome,
				TransitionReason:     transition,
				Outcome:              outcome,
				IsTerminal:           k.isTerminal,
				Generation:           k.generation,
				CandidateFingerprint: k.fingerprint,
				ChainCount:           c,
			}
			if k.hasPrev {
				v := k.prevAccount
				row.PreviousAccountID = &v
			}
			fm.flowRows = append(fm.flowRows, row)
		}
		sort.Slice(fm.flowRows, func(a, b int) bool {
			ra, rb := fm.flowRows[a], fm.flowRows[b]
			if ra.Ordinal != rb.Ordinal {
				return ra.Ordinal < rb.Ordinal
			}
			if ra.Lane != rb.Lane {
				return ra.Lane < rb.Lane
			}
			if ra.AccountID != rb.AccountID {
				return ra.AccountID < rb.AccountID
			}
			pa, pb := int64(-1), int64(-1)
			if ra.PreviousAccountID != nil {
				pa = *ra.PreviousAccountID
			}
			if rb.PreviousAccountID != nil {
				pb = *rb.PreviousAccountID
			}
			if pa != pb {
				return pa < pb
			}
			if ra.PreviousOutcome != rb.PreviousOutcome {
				return ra.PreviousOutcome < rb.PreviousOutcome
			}
			if ra.TransitionReason != rb.TransitionReason {
				return ra.TransitionReason < rb.TransitionReason
			}
			if ra.Outcome != rb.Outcome {
				return ra.Outcome < rb.Outcome
			}
			if ra.Generation != rb.Generation {
				return ra.Generation < rb.Generation
			}
			for i := range ra.CandidateFingerprint {
				if ra.CandidateFingerprint[i] != rb.CandidateFingerprint[i] {
					return ra.CandidateFingerprint[i] < rb.CandidateFingerprint[i]
				}
			}
			return false
		})
	}
	return fm
}

func hasLegacyCounts(shell *foldShell) bool {
	return anyLegacyCounts(shell.edges, shell.counts8)
}

func anyLegacyCounts(edges, counts [8]int64) bool {
	for i := 0; i < 8; i++ {
		if edges[i] != 0 || counts[i] != 0 {
			return true
		}
	}
	return false
}
