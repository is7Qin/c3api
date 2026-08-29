// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"context"
	"encoding/hex"
	"net/http"

	"github.com/is7qin/c3api/internal/quality"
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
	appendFlow := p.pipelineFlowAppend
	return NewAttemptObserver(qualityContext, nil, appendFlow, nil), true
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

func (p *Proxy) observePipelineAttempt(ctx context.Context, sel *scheduler.Selection, plan *scheduler.AttemptPlan, code int) {
	if plan == nil {
		return
	}
	attempt, ok := plan.CurrentAttempt()
	if !ok {
		return
	}
	observer, ok := p.pipelineObserver(sel, attempt)
	if !ok {
		return
	}
	if code == 0 && ctx.Err() != nil {
		_ = observer.Cancel(pipelineOutcome(attempt, 0, true, true))
		return
	}
	terminal := code != 0 && code != http.StatusTooManyRequests
	_ = observer.Complete(pipelineOutcome(attempt, code, false, terminal), nil)
}
