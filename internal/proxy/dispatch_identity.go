// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"encoding/hex"

	"github.com/is7qin/c3api/internal/scheduler"
)

func pipelineID(raw string) [32]byte {
	var id [32]byte
	decoded, err := hex.DecodeString(raw)
	if err == nil && len(decoded) == len(id) {
		copy(id[:], decoded)
	}
	return id
}

// pipelineBase projects the plan-canonical attempt identity onto an outcome
// skeleton (no terminal facts — the completer overlays those).
func pipelineBase(attempt scheduler.Attempt) AttemptOutcome {
	return AttemptOutcome{
		ID: AttemptID(attempt.AttemptID), RouteClassID: RouteClassID(attempt.RouteClassID),
		QualityClassID: QualityClassID(attempt.QualityClassID), Fingerprint: CandidateFingerprint(attempt.CandidateFingerprint),
		TemplateID: attempt.TemplateID, AccountID: attempt.AccountID, RequestedModel: attempt.RequestedModel,
		MappedModel: attempt.MappedModel, CallerCategory: CallerCategory(attempt.CallerCategory),
		OperationTag: OperationTag(attempt.OperationTag), Ordinal: attempt.Ordinal,
		LifecycleRevision: LifecycleRevision(attempt.LifecycleRevision), Lane: LaneID(attempt.Lane),
		Generation: Generation(attempt.RoutingGeneration), PreviousAttemptID: attemptPreviousID(attempt.PreviousAttemptID),
	}
}

// dispatchFailureOutcome overlays a handled=false classification onto the
// dispatch base.
func dispatchFailureOutcome(base AttemptOutcome, code int, clientCancel, terminal bool) AttemptOutcome {
	outcome := base
	outcome.HTTPStatus = AttemptStatus(code)
	outcome.Terminal = terminal
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

func pipelineOutcome(attempt scheduler.Attempt, code int, clientCancel bool, terminal bool) AttemptOutcome {
	return dispatchFailureOutcome(pipelineBase(attempt), code, clientCancel, terminal)
}

func attemptPreviousID(raw *string) *AttemptID {
	if raw == nil {
		return nil
	}
	id := AttemptID(*raw)
	return &id
}

// flowOutcomeToken classifies a completed dispatch into the canonical flow
// outcome token. Only dispatched results reach the flow lane; the token is a
// classification of the real terminal facts (Result + HTTP status), never a
// new identity.
func flowOutcomeToken(o AttemptOutcome) string {
	switch o.Result {
	case ResultSuccess:
		return "success"
	case ResultClientCancel:
		return "client_cancel"
	case ResultFailed:
		switch {
		case o.HTTPStatus == 429:
			return "429"
		case o.HTTPStatus >= 500 && o.HTTPStatus <= 599:
			return "5xx"
		case o.HTTPStatus == 0:
			return "network"
		default:
			return "4xx"
		}
	default:
		return ""
	}
}

// flowDispatchFromAttempt is superseded by the fold-at-source stash
// (foldOwner.append): dispatch attempt data flows into stack fact values at
// settle instead of heap Dispatch rows. The outcome token mapping above is
// the single token source.
