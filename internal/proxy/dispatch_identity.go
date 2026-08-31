// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"encoding/hex"
	"strconv"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/quality"
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

// selDispatchBase is the legacy (plan-less) dispatch identity: real
// template/account/model from the selection with loop-managed chain linkage.
// It feeds the quality/observation path only — flow chains require
// plan-canonical identity (see flowChainOwner).
func selDispatchBase(sel *scheduler.Selection, reqID string, dispatched int, reqModel string, st attemptState, format, selectFormat domain.RequestFormat) AttemptOutcome {
	fp := sel.CandidateFingerprint
	if fp == "" {
		fp = "fp-placeholder"
	}
	ordinal := uint8(dispatched)
	if ordinal < 1 {
		ordinal = 1
	}
	id := AttemptID(reqID + ":1")
	var prev *AttemptID
	if ordinal > 1 {
		id = AttemptID(reqID + ":" + strconv.Itoa(int(ordinal)))
		p := AttemptID(reqID + ":" + strconv.Itoa(int(ordinal)-1))
		prev = &p
	}
	mapped := sel.Model
	if mapped == "" {
		mapped = reqModel
	}
	if mapped == "" {
		mapped = "unknown"
	}
	requested := reqModel
	if requested == "" {
		requested = mapped
	}
	cat := dispatchCallerCategory(sel, st, format, selectFormat)
	return AttemptOutcome{
		ID: id, RouteClassID: "rc1", QualityClassID: "qc1", Fingerprint: CandidateFingerprint(fp),
		TemplateID: sel.TemplateID, AccountID: sel.AccountID, RequestedModel: requested, MappedModel: mapped,
		CallerCategory: cat, OperationTag: dispatchOperationTag(cat, st), Ordinal: ordinal,
		LifecycleRevision: 1, Lane: LanePrimary, Generation: 1, PreviousAttemptID: prev,
	}
}

// dispatchCallerCategory resolves the ten-way category including the
// credential-type specializations visible at dispatch time.
func dispatchCallerCategory(sel *scheduler.Selection, st attemptState, format, selectFormat domain.RequestFormat) CallerCategory {
	codex := isCodexCredentialType(sel.CredentialType)
	if _, ok := st.caller.(*convertedCaller); ok && format != selectFormat {
		return CallerConverted
	}
	switch format {
	case domain.FormatOpenAIChat:
		return CallerChat
	case domain.FormatOpenAIResponses:
		if codex {
			return CallerCodexHTTP
		}
		return CallerResponses
	case domain.FormatAnthropic:
		return CallerAnthropic
	case domain.FormatOpenAIImages:
		if codex {
			return CallerImagesCodex
		}
		return CallerImages
	case domain.FormatOpenAIResponsesWS:
		if codex {
			return CallerCodexWS
		}
		return CallerResponsesWS
	case domain.FormatOpenAISearch:
		return CallerSearch
	default:
		return CallerChat
	}
}

func dispatchOperationTag(cat CallerCategory, st attemptState) OperationTag {
	switch cat {
	case CallerChat:
		return OperationTag(domain.OpChatCompletions)
	case CallerResponses, CallerCodexHTTP:
		return OperationTag(domain.OpResponses)
	case CallerAnthropic:
		return OperationTag(domain.OpAnthropicMessages)
	case CallerImages, CallerImagesCodex:
		if ic, ok := st.caller.(*imagesCaller); ok {
			return OperationTag(ic.operationTag())
		}
		return OperationTag(domain.OpImagesGenerations)
	case CallerResponsesWS, CallerCodexWS:
		return OperationTag(domain.OpResponsesWS)
	case CallerSearch:
		return OperationTag(domain.OpSearch)
	default:
		return OperationTag(domain.OpChatCompletions)
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

// flowDispatchFromAttempt projects one completed plan-backed dispatch onto
// the canonical flow edge. Every identity field (route/quality/fingerprint/
// template/account/models/ordinal/lane/generation/revision/previous IDs)
// comes from the real scheduler.Attempt; terminal facts come from the
// completed outcome. PreviousOutcome/TransitionReason are classifications of
// the real recorded transition (empty previous outcome until a prior edge was
// appended), never fabricated identifiers.
func flowDispatchFromAttempt(a scheduler.Attempt, o AttemptOutcome, prevOutcome string) quality.FlowDispatch {
	d := quality.FlowDispatch{
		RouteClassID:      a.RouteClassID,
		QualityClassID:    a.QualityClassID,
		Fingerprint:       a.CandidateFingerprint,
		TemplateID:        a.TemplateID,
		AccountID:         a.AccountID,
		RequestedModel:    a.RequestedModel,
		MappedModel:       a.MappedModel,
		Generation:        int64(a.RoutingGeneration),
		LifecycleRevision: a.LifecycleRevision,
		Ordinal:           a.Ordinal,
		Lane:              string(a.Lane),
		PreviousAttemptID: a.PreviousAttemptID,
		PreviousAccountID: a.PreviousAccountID,
		PreviousOutcome:   prevOutcome,
		Outcome:           flowOutcomeToken(o),
		IsTerminal:        o.Terminal,
	}
	if a.Ordinal == 1 {
		d.TransitionReason = "initial"
	} else {
		d.TransitionReason = "failover"
	}
	return d
}
