// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import "fmt"

type AttemptID string

func (a AttemptID) Validate() error {
	if a == "" {
		return fmt.Errorf("AttemptID empty")
	}
	return nil
}

type RouteClassID string

type CandidateFingerprint string

type LifecycleRevision int64

type LaneID string

const (
	LanePrimary  LaneID = "primary"
	LaneExplore  LaneID = "explore"
	LaneDegraded LaneID = "degraded"
)

func (l LaneID) Valid() bool {
	switch l {
	case LanePrimary, LaneExplore, LaneDegraded, "":
		return true
	}
	return false
}

type Generation int64

type CommitState int

const (
	CommitNotSent CommitState = iota
	CommitSentAmbiguous
	CommitResponseStarted
	CommitClientCommitted
)

func (c CommitState) String() string {
	switch c {
	case CommitNotSent:
		return "not_sent"
	case CommitSentAmbiguous:
		return "sent_ambiguous"
	case CommitResponseStarted:
		return "response_started"
	case CommitClientCommitted:
		return "client_committed"
	default:
		return "unknown"
	}
}

func (c CommitState) Valid() bool {
	switch c {
	case CommitNotSent, CommitSentAmbiguous, CommitResponseStarted, CommitClientCommitted:
		return true
	}
	return false
}

type AttemptResult int

const (
	ResultUnknown AttemptResult = iota
	ResultSuccess
	ResultFailed
	ResultClientCancel
	ResultLocalReject
)

func (r AttemptResult) Valid() bool {
	switch r {
	case ResultSuccess, ResultFailed, ResultClientCancel, ResultLocalReject:
		return true
	}
	return false
}

type AttemptStatus int

type AttemptTiming struct {
	LatencyMS int64
	TTFTMS    *int64
}

type AttemptUsage struct {
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	CallCount           int64
}

type CallerCategory string

const (
	CallerChat          CallerCategory = "chat"
	CallerResponses     CallerCategory = "responses"
	CallerAnthropic     CallerCategory = "anthropic"
	CallerConverted     CallerCategory = "converted"
	CallerImages        CallerCategory = "images"
	CallerImagesCodex   CallerCategory = "images_codex"
	CallerCodexHTTP     CallerCategory = "codex_http"
	CallerResponsesWS   CallerCategory = "responses_ws"
	CallerCodexWS       CallerCategory = "codex_ws"
	CallerSearch        CallerCategory = "search"
)

func AllCallerCategories() []CallerCategory {
	return []CallerCategory{
		CallerChat,
		CallerResponses,
		CallerAnthropic,
		CallerConverted,
		CallerImages,
		CallerImagesCodex,
		CallerCodexHTTP,
		CallerResponsesWS,
		CallerCodexWS,
		CallerSearch,
	}
}

func (c CallerCategory) Valid() bool {
	switch c {
	case CallerChat, CallerResponses, CallerAnthropic, CallerConverted, CallerImages, CallerImagesCodex, CallerCodexHTTP, CallerResponsesWS, CallerCodexWS, CallerSearch:
		return true
	}
	return false
}

func (c CallerCategory) IsHardContinuation() bool {
	switch c {
	case CallerResponsesWS, CallerCodexWS:
		return true
	default:
		return false
	}
}

type AttemptOutcome struct {
	ID                   AttemptID
	RouteClassID         RouteClassID
	Fingerprint          CandidateFingerprint
	LifecycleRevision    LifecycleRevision
	Lane                 LaneID
	Generation           Generation
	Commit               CommitState
	Result               AttemptResult
	HTTPStatus           AttemptStatus
	Timing               AttemptTiming
	Usage                AttemptUsage
	PreviousAttemptID    *AttemptID
	Terminal             bool
	IsWSDial             bool
	HasSentBusinessFrame bool
	IsMalformed          bool
}

func (o AttemptOutcome) Validate() error {
	if err := o.ID.Validate(); err != nil {
		return err
	}
	if !o.Commit.Valid() {
		return fmt.Errorf("invalid CommitState %d", o.Commit)
	}
	if !o.Lane.Valid() {
		return fmt.Errorf("invalid Lane %q", o.Lane)
	}
	if o.Commit == CommitResponseStarted || o.Commit == CommitClientCommitted {
		if o.HTTPStatus == 0 && o.Result != ResultClientCancel {
			// response_started must have status or be client cancel
		}
	}
	if o.Commit == CommitNotSent && o.HasSentBusinessFrame {
		return fmt.Errorf("not_sent cannot have sent business frame")
	}
	if o.IsWSDial && o.HasSentBusinessFrame && o.Commit == CommitNotSent {
		return fmt.Errorf("WS dial after business frame cannot be not_sent")
	}
	if o.Commit != CommitNotSent && o.Result == ResultLocalReject {
		return fmt.Errorf("local reject must be not_sent")
	}
	if o.IsMalformed && o.Result != ResultFailed {
		return fmt.Errorf("malformed must be failed")
	}
	return nil
}

func (o AttemptOutcome) IsDispatched() bool {
	if o.Result == ResultLocalReject {
		return false
	}
	return true
}

func (o AttemptOutcome) IsCountedForQuality() bool {
	if !o.IsDispatched() {
		return false
	}
	if o.Result == ResultClientCancel {
		return false
	}
	return true
}

func (o AttemptOutcome) IsFailed() bool {
	if !o.IsCountedForQuality() {
		return false
	}
	switch o.Result {
	case ResultFailed:
		return true
	case ResultSuccess:
		return false
	default:
		return false
	}
}

func (o AttemptOutcome) HasPossiblyWrittenBytes() bool {
	switch o.Commit {
	case CommitResponseStarted, CommitClientCommitted:
		return true
	case CommitSentAmbiguous:
		return true
	default:
		return false
	}
}
