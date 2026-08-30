// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"encoding/hex"
	"strconv"

	"github.com/is7qin/c3api/internal/domain"
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

// selDispatchBase is the legacy (pre-compiler) dispatch identity: real
// template/account/model from the selection, loop-managed chain linkage,
// placeholder canonical fields until routing dispatch provides them.
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
