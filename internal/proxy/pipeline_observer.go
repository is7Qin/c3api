// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"context"
	"net/http"

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
// the dispatch owner before the upstream call, plus the base identity the
// caller overlays terminal facts onto. fromPlan marks plan-canonical metadata
// (canonical IDs / ordinal / lane / generation / lifecycle revision /
// previous linkage); until the routing compiler merges, legacy selections
// carry loop-managed chain metadata with placeholder identity — the same
// interim contract documented on AttemptID.
type dispatchObservation struct {
	observer *AttemptObserver
	base     AttemptOutcome
	fromPlan bool
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

func (p *Proxy) pipelineObserver(sel *scheduler.Selection, attempt scheduler.Attempt) (*AttemptObserver, bool) {
	if sel == nil || attempt.Validate() != nil {
		return nil, false
	}
	return p.observerFor(pipelineBase(attempt)), true
}

// observerFor begins the quality AttemptContext at dispatch time and wires the
// shared flow append seam. markHealth/release stay nil: failover classification
// and lease release remain the loop's/caller's single owners.
func (p *Proxy) observerFor(base AttemptOutcome) *AttemptObserver {
	var qualityContext *quality.AttemptContext
	if p.qualityRecorder != nil {
		qualityContext = p.qualityRecorder.Begin(quality.CanonicalKey(
			pipelineID(string(base.RouteClassID)),
			pipelineID(string(base.QualityClassID)),
			pipelineID(string(base.Fingerprint)),
		))
	}
	return NewAttemptObserver(qualityContext, nil, p.pipelineFlowAppend, nil)
}

// beginDispatch creates the one owner observation for one real upstream
// dispatch, before attempt.call. dispatched is the 1-based chain position.
func (p *Proxy) beginDispatch(ctx context.Context, sel *scheduler.Selection, plan *scheduler.AttemptPlan, reqID string, dispatched int, reqModel string, st attemptState, format, selectFormat domain.RequestFormat) (context.Context, *dispatchObservation) {
	if sel == nil {
		return ctx, nil
	}
	d := &dispatchObservation{}
	if plan != nil {
		if attempt, ok := plan.CurrentAttempt(); ok && attempt.Validate() == nil {
			if observer, ok := p.pipelineObserver(sel, attempt); ok {
				d.observer = observer
				d.base = pipelineBase(attempt)
				d.fromPlan = true
			}
		}
	}
	if d.observer == nil {
		d.base = selDispatchBase(sel, reqID, dispatched, reqModel, st, format, selectFormat)
		d.observer = p.observerFor(d.base)
	}
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
