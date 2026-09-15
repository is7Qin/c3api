// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"context"
	"net/http"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/quality"
	"github.com/is7qin/c3api/internal/scheduler"
)

// ctxKeyDispatch carries the single owner-created dispatch observation from
// the failover loop (the dispatch owner) to the caller executing the upstream
// call. Callers complete it on handled=true terminals; the loop completes it
// on handled=false classifications; anything left open is abandoned by the
// loop's deferred owner cleanup (local reject / panic — not an attempt).
type ctxKeyDispatch struct{}

// dispatchObservation is exactly one AttemptContext + one observer created by
// the dispatch owner before the upstream call, plus the plan-canonical base
// identity the caller overlays terminal facts onto. It exists only for
// plan-backed dispatches with a valid scheduler.Attempt — identity is the
// compiled attempt's own (canonical IDs / ordinal / lane / generation /
// lifecycle revision / previous linkage), never a placeholder.
type dispatchObservation struct {
	observer *AttemptObserver
	base     AttemptOutcome
}

func (d *dispatchObservation) abandon() {
	if d != nil {
		d.observer.Abandon()
	}
}

func dispatchFromContext(ctx context.Context) *dispatchObservation {
	d, _ := ctx.Value(ctxKeyDispatch{}).(*dispatchObservation)
	return d
}

// mergeDispatchBase re-anchors a caller-built outcome onto the dispatch
// identity (ID/ordinal/revision/chain linkage). The caller keeps authority
// over CallerCategory/OperationTag (credential-type specializations —
// images_codex / codex_http / codex_ws — and images generations-vs-edits that
// the loop cannot see) and over all terminal facts.
func mergeDispatchBase(ctx context.Context, own AttemptOutcome) AttemptOutcome {
	d := dispatchFromContext(ctx)
	if d == nil {
		return own
	}
	merged := own
	merged.ID = d.base.ID
	merged.RouteClassID = d.base.RouteClassID
	merged.QualityClassID = d.base.QualityClassID
	merged.Fingerprint = d.base.Fingerprint
	merged.TemplateID = d.base.TemplateID
	merged.AccountID = d.base.AccountID
	merged.RequestedModel = d.base.RequestedModel
	merged.MappedModel = d.base.MappedModel
	merged.Ordinal = d.base.Ordinal
	merged.LifecycleRevision = d.base.LifecycleRevision
	merged.Lane = d.base.Lane
	merged.Generation = d.base.Generation
	merged.PreviousAttemptID = d.base.PreviousAttemptID
	return merged
}

// observeDispatchOutcome is the single caller-side completion seam for
// handled=true terminals: it completes the owner dispatch observation (quality
// + bounded flow) exactly once and applies health marking. Client cancel is
// flow-only (no health, quality context cancelled). When no owner observation
// exists (direct caller invocation outside the loop) health marking still
// runs — observation ownership belongs to the dispatch owner.
func (p *Proxy) observeDispatchOutcome(ctx context.Context, outcome AttemptOutcome, health *AttemptHealthEvent) {
	if d := dispatchFromContext(ctx); d != nil {
		if outcome.Result == ResultClientCancel {
			_ = d.observer.Cancel(outcome)
		} else {
			_ = d.observer.Complete(outcome, nil)
		}
	}
	if health != nil && outcome.IsCountedForQuality() && p.sched != nil {
		p.sched.MarkResult(outcome.AccountID, health.Kind, health.ResetAt, int(outcome.HTTPStatus), health.ErrorMessage, outcome.MappedModel)
	}
}

func (p *Proxy) pipelineObserver(sel *scheduler.Selection, attempt scheduler.Attempt, appendFlow AttemptFlowAppend) (*AttemptObserver, bool) {
	if sel == nil || attempt.Validate() != nil {
		return nil, false
	}
	return p.observerFor(pipelineBase(attempt), appendFlow), true
}

// observerFor begins the quality AttemptContext at dispatch time and wires the
// flow append seam. markHealth/release stay nil: failover classification and
// lease release remain the loop's/caller's single owners.
func (p *Proxy) observerFor(base AttemptOutcome, appendFlow AttemptFlowAppend) *AttemptObserver {
	var qualityContext *quality.AttemptContext
	if p.qualityRecorder != nil {
		qualityContext = p.qualityRecorder.Begin(quality.CanonicalKey(
			pipelineID(string(base.RouteClassID)),
			pipelineID(string(base.QualityClassID)),
			pipelineID(string(base.Fingerprint)),
		))
	}
	return NewAttemptObserver(qualityContext, nil, appendFlow, nil)
}

// foldOwner is the request-local fold-at-source producer owned exclusively by
// failoverLoopWithPlan. It exists only for plan-backed dispatched requests
// with a wired quality recorder: beginDispatch arms it with the real
// scheduler.Attempt, so no synthetic fact can enter the cells. beginDispatch
// arms the current dispatch attempt; the stable seam closure (one per
// request) stashes each real dispatch outcome as a packed fact; settle emits
// the chain once through the completion walk or records the honest
// incomplete signal — exactly once per request. The stash holds minimal facts
// only (decoded IDs + §4 codes extracted at arm/append time — no Attempt, no
// token strings); facts fold directly into the owner's counter cells at
// settle.
type foldOwner struct {
	recorder *quality.Recorder
	tap      AttemptFlowAppend // observation seam (tests); nil in production
	seamFn   AttemptFlowAppend // stable per-request closure (one capture, reused by every dispatch)
	route    [32]byte          // plan-constant identity (attempt_plan_exec.go:315), decoded per arm
	gen      uint64            // plan-constant generation (attempt_plan_exec.go:319), bound per arm
	cur      packedArm         // identity of the dispatch in flight
	armed    bool
	edges    [8]packedEdge // packed stash, cap-8 (attempt 9+ counts cap-overflow)
	nedges   int
	hasLast  bool
	lastOut  uint8 // last stashed outcome code (real previous outcome)
	settled  bool
}

// packedArm is the in-flight dispatch identity: the walk inputs extracted
// from the real plan attempt at arm time (pointers dereferenced, lane coded,
// hex decoded — nothing referencing scheduler memory survives the arm).
type packedArm struct {
	fp       [32]byte
	account  int64
	prevAcct int64
	ordinal  uint8
	lane     uint8
	hasPrev  bool
}

// packedEdge is one stashed dispatch: the walk inputs only. Terminality is
// force-marked final at settle (option (a)).
type packedEdge struct {
	fp      [32]byte
	account int64
	prev    int64
	ordinal uint8
	lane    uint8
	outcome uint8
	flags   uint8 // bit0 terminal, bit1 hasPrev
}

const (
	foldEdgeTerminal uint8 = 1 << iota
	foldEdgeHasPrev
)

// Proxy-local §4 outcome codes (the walk consumes the token strings, so the
// stash codes map back 1:1 at settle, including code 6=`client_cancel`
// verbatim per Amendment A1 — census byte-identity requires it: the old tree
// writes client_cancel verbatim and folding it into error would corrupt
// PG/dashboards. Code 0 is the empty token, which the walk drops exactly as
// today.
const (
	foldOutEmpty uint8 = iota
	foldOutSuccess
	foldOutError
	foldOut4xx
	foldOut429
	foldOut5xx
	foldOutNetwork
	foldOutClientCancel
)

// Proxy-local §4 lane codes. Code 3 is unknown: settle emits "" so the walk
// drops the edge exactly as today (unknown lane string fails the codebook).
const (
	foldLanePrimaryCode uint8 = iota
	foldLaneExploreCode
	foldLaneDegradedCode
	foldLaneUnknownCode
)

func foldLaneCodeOf(lane scheduler.AttemptLane) uint8 {
	if code, ok := foldLaneCodes[lane]; ok {
		return code
	}
	return foldLaneUnknownCode
}

func foldLaneTokenOf(code uint8) string { return foldLaneTokens[code] }

// foldLaneCodes maps the plan lane to the packed stash code. Unknown lanes
// fall to foldLaneUnknownCode (non-zero, so comma-ok is required — a plain
// map read would silently yield primary).
var foldLaneCodes = map[scheduler.AttemptLane]uint8{
	scheduler.AttemptLanePrimary:  foldLanePrimaryCode,
	scheduler.AttemptLaneExplore:  foldLaneExploreCode,
	scheduler.AttemptLaneDegraded: foldLaneDegradedCode,
}

// foldLaneTokens is the reverse map; unknown codes fall to "" (zero value),
// so the walk drops the edge exactly as the previous switch default.
var foldLaneTokens = map[uint8]string{
	foldLanePrimaryCode:  "primary",
	foldLaneExploreCode:  "explore",
	foldLaneDegradedCode: "degraded",
}

func foldOutcomeCodeOf(token string) uint8 { return foldOutcomeCodes[token] }

func foldOutcomeTokenOf(code uint8) string { return foldOutcomeTokens[code] }

// foldOutcomeCodes maps the canonical flow token (flowOutcomeToken stays the
// single source) to the packed stash code. Unknown tokens fall to
// foldOutEmpty (the zero value), exactly as the previous switch default.
var foldOutcomeCodes = map[string]uint8{
	"success":       foldOutSuccess,
	"error":         foldOutError,
	"client_cancel": foldOutClientCancel,
	"4xx":           foldOut4xx,
	"429":           foldOut429,
	"5xx":           foldOut5xx,
	"network":       foldOutNetwork,
}

// foldOutcomeTokens is the reverse map, built once from the forward map so
// the pairing cannot drift; unknown codes fall to "" (zero value), as before.
var foldOutcomeTokens = func() map[uint8]string {
	m := make(map[uint8]string, len(foldOutcomeCodes))
	for tok, code := range foldOutcomeCodes {
		m[code] = tok
	}
	return m
}()

func newFoldOwner(recorder *quality.Recorder, tap AttemptFlowAppend) *foldOwner {
	f := &foldOwner{recorder: recorder, tap: tap}
	f.seamFn = f.append
	return f
}

// arm binds the next dispatch's flow append to one real plan attempt: a
// plan-backed request that never begins a dispatch (e.g. price-precheck
// rejection) records no flow and is never counted incomplete. Walk inputs are
// extracted now (pointers dereferenced, lane coded, hex decoded) so the stash
// never references scheduler memory.
func (f *foldOwner) arm(attempt scheduler.Attempt) {
	f.route = pipelineID(attempt.RouteClassID)
	f.gen = attempt.RoutingGeneration
	f.cur = packedArm{
		fp:      pipelineID(attempt.CandidateFingerprint),
		account: attempt.AccountID,
		ordinal: attempt.Ordinal,
		lane:    foldLaneCodeOf(attempt.Lane),
		hasPrev: attempt.PreviousAccountID != nil,
	}
	if attempt.PreviousAccountID != nil {
		f.cur.prevAcct = *attempt.PreviousAccountID
	}
	f.armed = true
}

// append is the single flow seam: plan-canonical dispatches stash one real
// edge; the observation tap (when wired) sees every completed outcome.
// Stash rejections (beyond cap-8) are counted at the same site by the
// owner's cap-overflow counter and never rewrite the last real edge.
func (f *foldOwner) append(outcome AttemptOutcome) {
	if f.armed {
		code := foldOutcomeCodeOf(flowOutcomeToken(outcome))
		if f.nedges >= len(f.edges) {
			if f.recorder != nil {
				f.recorder.FlowOwner().NoteCapOverflow()
			}
		} else {
			e := packedEdge{
				fp:      f.cur.fp,
				account: f.cur.account,
				prev:    f.cur.prevAcct,
				ordinal: f.cur.ordinal,
				lane:    f.cur.lane,
				outcome: code,
			}
			if outcome.Terminal {
				e.flags |= foldEdgeTerminal
			}
			if f.cur.hasPrev {
				e.flags |= foldEdgeHasPrev
			}
			f.edges[f.nedges] = e
			f.nedges++
			f.lastOut = code
			f.hasLast = true
		}
	}
	if f.tap != nil {
		f.tap(outcome)
	}
}

// settle runs once on loop exit. A chain that recorded dispatches is emitted
// through the completion walk (the last edge force-marked terminal on
// exhaustion, minute bucket minted once); an armed-but-empty, panicked, or
// terminal-less chain takes the Close rows (incomplete iff nonterminal).
// Handled success or failure always carries a terminal edge and emits.
func (f *foldOwner) settle(panicked bool) {
	if f == nil || f.settled {
		return
	}
	f.settled = true
	owner := foldRecorderOwner(f.recorder)
	if f.nedges == 0 {
		// Never recorded a dispatch: armed-but-abandoned closes as the
		// honest incomplete signal; never-armed records nothing at all.
		if f.armed && owner != nil {
			owner.NoteIncompleteChain()
		}
		return
	}
	terminal := false
	for i := 0; i < f.nedges; i++ {
		if f.edges[i].flags&foldEdgeTerminal != 0 {
			terminal = true
		}
	}
	if panicked || !f.hasLast {
		if !terminal && owner != nil {
			owner.NoteIncompleteChain()
		}
		return
	}
	if owner == nil {
		return
	}
	if !terminal {
		f.edges[f.nedges-1].flags |= foldEdgeTerminal
	}
	bucket := clockMinute()
	routeVal := domain.RouteClassIDVal(f.route)
	gen := int64(f.gen)
	owner.FoldChain(bucket, f.nedges, func(i int) (
		route domain.RouteClassIDVal,
		fp domain.CandidateFingerprintVal,
		accountID, prevAccount, generation int64,
		ordinal uint8,
		lane, outcome, prevOutcome string,
		isTerminal, hasPrev bool,
	) {
		e := f.edges[i]
		route = routeVal
		fp = domain.CandidateFingerprintVal(e.fp)
		accountID = e.account
		generation = gen
		ordinal = e.ordinal
		lane = foldLaneTokenOf(e.lane)
		outcome = foldOutcomeTokenOf(e.outcome)
		if i > 0 {
			prevOutcome = foldOutcomeTokenOf(f.edges[i-1].outcome)
		}
		isTerminal = e.flags&foldEdgeTerminal != 0
		if e.flags&foldEdgeHasPrev != 0 {
			prevAccount = e.prev
			hasPrev = true
		}
		return
	})
}

func foldRecorderOwner(recorder *quality.Recorder) *quality.FlowOwner {
	if recorder == nil {
		return nil
	}
	return recorder.FlowOwner()
}

// clockMinute mints the completion minute bucket (once per settle — every
// fact of the chain carries the identical bucket, so no cross-minute
// scatter at expansion).
func clockMinute() int64 { return time.Now().UTC().Truncate(time.Minute).Unix() }

// beginDispatch creates the one owner observation for one real upstream
// dispatch, before attempt.call. The observation exists only on the plan's
// canonical attempt identity (threaded settle value — v4-S1): a dispatch
// without a valid recorded attempt gets no observation and no flow binding —
// nothing is ever recorded under a fabricated identity.
func (p *Proxy) beginDispatch(ctx context.Context, sel *scheduler.Selection, attempt scheduler.Attempt, flow *foldOwner) (context.Context, *dispatchObservation) {
	if sel == nil {
		return ctx, nil
	}
	appendFlow := p.pipelineFlowAppend
	if flow != nil {
		appendFlow = flow.seamFn
	}
	observer, ok := p.pipelineObserver(sel, attempt, appendFlow)
	if !ok {
		return ctx, nil
	}
	if flow != nil {
		flow.arm(attempt)
	}
	d := &dispatchObservation{observer: observer, base: pipelineBase(attempt)}
	return context.WithValue(ctx, ctxKeyDispatch{}, d), d
}

// observeDispatchFailure completes the owner observation for a handled=false
// classification. Health marking stays with the loop's Classify→MarkResult
// path (rule punish gating), so health is nil here.
func (p *Proxy) observeDispatchFailure(ctx context.Context, d *dispatchObservation, code int) {
	if d == nil || d.observer == nil {
		return
	}
	if code == 0 && ctx.Err() != nil {
		_ = d.observer.Cancel(dispatchFailureOutcome(d.base, 0, true, true))
		return
	}
	terminal := code != 0 && code != http.StatusTooManyRequests
	_ = d.observer.Complete(dispatchFailureOutcome(d.base, code, false, terminal), nil)
}
