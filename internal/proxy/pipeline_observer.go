// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"encoding/hex"
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/quality"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
)

func (p *Proxy) pipelineObserver(sel *scheduler.Selection, attempt scheduler.Attempt) (*AttemptObserver, bool) {
	if sel == nil || attempt.Validate() != nil {
		return nil, false
	}
	var qualityContext *quality.AttemptContext
	if p.qualityRecorder != nil {
		qualityContext = p.qualityRecorder.Begin(quality.CanonicalKey(
			pipelineID(attempt.RouteClassID),
			pipelineID(attempt.QualityClassID),
			pipelineID(attempt.CandidateFingerprint),
		))
	}
	markHealth := func(outcome AttemptOutcome, event AttemptHealthEvent) {
		if p.sched != nil {
			_, punish := p.sched.Classify(rule.Event{AccountID: outcome.AccountID, Kind: event.Kind, HTTPStatus: healthStatus(outcome.HTTPStatus), Model: outcome.MappedModel, ErrorMessage: event.ErrorMessage})
			if punish {
				p.sched.MarkResult(outcome.AccountID, event.Kind, event.ResetAt, int(outcome.HTTPStatus), event.ErrorMessage, outcome.MappedModel)
			}
		}
	}
	appendFlow := p.pipelineFlowAppend
	return NewAttemptObserver(qualityContext, markHealth, appendFlow, sel.Release), true
}

func healthStatus(status AttemptStatus) *int {
	if status == 0 {
		return nil
	}
	value := int(status)
	return &value
}

func pipelineID(raw string) [32]byte {
	var id [32]byte
	decoded, err := hex.DecodeString(raw)
	if err == nil && len(decoded) == len(id) {
		copy(id[:], decoded)
	}
	return id
}

func pipelineOutcome(attempt scheduler.Attempt, code int, clientCancel bool, terminal bool) AttemptOutcome {
	outcome := AttemptOutcome{
		ID: AttemptID(attempt.AttemptID), RouteClassID: RouteClassID(attempt.RouteClassID),
		QualityClassID: QualityClassID(attempt.QualityClassID), Fingerprint: CandidateFingerprint(attempt.CandidateFingerprint),
		TemplateID: attempt.TemplateID, AccountID: attempt.AccountID, RequestedModel: attempt.RequestedModel,
		MappedModel: attempt.MappedModel, CallerCategory: CallerCategory(attempt.CallerCategory),
		OperationTag: OperationTag(attempt.OperationTag), Ordinal: attempt.Ordinal,
		LifecycleRevision: LifecycleRevision(attempt.LifecycleRevision), Lane: LaneID(attempt.Lane),
		Generation: Generation(attempt.RoutingGeneration), HTTPStatus: AttemptStatus(code), Terminal: terminal,
		PreviousAttemptID: attemptPreviousID(attempt.PreviousAttemptID),
	}
	if clientCancel {
		outcome.Result = ResultClientCancel
		outcome.Commit = CommitNotSent
		return outcome
	}
	outcome.Result = ResultFailed
	outcome.Commit = CommitUpstreamResponded
	if code == 0 {
		outcome.Commit = CommitNotSent
	}
	return outcome
}

func attemptPreviousID(raw *string) *AttemptID {
	if raw == nil {
		return nil
	}
	id := AttemptID(*raw)
	return &id
}

func pipelineHealth(code int, message string) *AttemptHealthEvent {
	kind := scheduler.RuleKindOf(code)
	if code == 429 {
		kind = rule.Kind429
	}
	return &AttemptHealthEvent{Kind: kind, ErrorMessage: message}
}

func (p *Proxy) completePipelineObservation(observer *AttemptObserver, outcome AttemptOutcome, health *AttemptHealthEvent) {
	if observer == nil {
		return
	}
	if outcome.Result == ResultClientCancel {
		_ = observer.Cancel(outcome)
		return
	}
	_ = observer.Complete(outcome, health)
}

func (p *Proxy) observePipelineAttempt(r *http.Request, sel *scheduler.Selection, plan *scheduler.AttemptPlan, code int, body []byte, callErr error) bool {
	if plan == nil {
		return false
	}
	attempt, ok := plan.CurrentAttempt()
	if !ok {
		return false
	}
	observer, ok := p.pipelineObserver(sel, attempt)
	if !ok {
		return false
	}
	if code == 0 && r.Context().Err() != nil {
		p.completePipelineObservation(observer, pipelineOutcome(attempt, 0, true, true), nil)
		return true
	}
	terminal := code != 0 && code != http.StatusTooManyRequests
	message := domain.TruncateErrMsg(upstreamErrMsg(body))
	if message == "" && callErr != nil {
		message = domain.TruncateErrMsg(callErr.Error())
	}
	p.completePipelineObservation(observer, pipelineOutcome(attempt, code, false, terminal), pipelineHealth(code, message))
	return true
}
