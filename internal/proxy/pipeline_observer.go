// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"context"
	"net/http"

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

// flowChainOwner is the request-local FlowChain producer owned exclusively by
// failoverLoopWithPlan. It exists only for plan-backed dispatched requests
// with a wired quality recorder: beginDispatch arms it with the real
// scheduler.Attempt, so no synthetic row can enter a chain. beginDispatch
// arms the current dispatch attempt; the stable seam closure (one per
// request) appends each real dispatch outcome; settle finalizes+completes a
// recorded chain or closes an unrecorded/panicked one — exactly once per
// request.
type flowChainOwner struct {
	recorder *quality.Recorder
	chain    *quality.FlowChain
	tap      AttemptFlowAppend // observation seam (tests); nil in production
	seamFn   AttemptFlowAppend // stable per-request closure
	cur      scheduler.Attempt // identity of the dispatch in flight
	armed    bool
	last     quality.FlowDispatch // last appended edge (real previous outcome)
	hasLast  bool
	settled  bool
}

func newFlowChainOwner(recorder *quality.Recorder, tap AttemptFlowAppend) *flowChainOwner {
	f := &flowChainOwner{recorder: recorder, tap: tap}
	f.seamFn = f.append
	return f
}

// arm binds the next dispatch's flow append to one real plan attempt and
// materializes the chain: a plan-backed request that never begins a dispatch
// (e.g. price-precheck rejection) records no flow and is never counted
// incomplete.
func (f *flowChainOwner) arm(attempt scheduler.Attempt) {
	f.cur, f.armed = attempt, true
	if f.chain == nil {
		f.chain = quality.NewFlowChain(f.recorder, nil)
	}
}

// append is the single flow seam: plan-canonical dispatches append one real
// edge to the chain; the observation tap (when wired) sees every completed
// outcome. Append rejections (post-terminal, overflow) are recorded by the
// chain's own counters and never rewrite the last real edge.
func (f *flowChainOwner) append(outcome AttemptOutcome) {
	if f.armed {
		var prev string
		if f.hasLast {
			prev = f.last.Outcome
		}
		d := flowDispatchFromAttempt(f.cur, outcome, prev)
		if f.chain.Append(d) == nil {
			f.last = d
			f.hasLast = true
		}
	}
	if f.tap != nil {
		f.tap(outcome)
	}
}

// settle runs once on loop exit. A chain that recorded dispatches is
// finalized (marks the last edge terminal on exhaustion) and completed; a
// panicked chain or one that never recorded a dispatch (abandon / no
// terminal) is closed — the honest incomplete signal. Handled success or
// failure always carries a terminal edge and takes the complete path.
func (f *flowChainOwner) settle(panicked bool) {
	if f == nil || f.settled {
		return
	}
	f.settled = true
	if f.chain == nil {
		return // no dispatch ever began: no flow, no incomplete count
	}
	if panicked || !f.hasLast {
		f.chain.Close()
		return
	}
	if err := f.chain.Finalize(); err != nil {
		f.chain.Close()
		return
	}
	if err := f.chain.Complete(); err != nil {
		f.chain.Close()
	}
}

// beginDispatch creates the one owner observation for one real upstream
// dispatch, before attempt.call. The observation exists only on the plan's
// canonical attempt identity: a dispatch without a plan (or without a valid
// recorded attempt) gets no observation and no flow binding — nothing is
// ever recorded under a fabricated identity.
func (p *Proxy) beginDispatch(ctx context.Context, sel *scheduler.Selection, plan *scheduler.AttemptPlan, flow *flowChainOwner) (context.Context, *dispatchObservation) {
	if sel == nil || plan == nil {
		return ctx, nil
	}
	attempt, ok := plan.CurrentAttempt()
	if !ok {
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
