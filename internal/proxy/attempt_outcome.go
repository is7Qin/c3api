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
	case LanePrimary, LaneExplore, LaneDegraded:
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
	ResultReservationReject
)

func (r AttemptResult) Valid() bool {
	switch r {
	case ResultSuccess, ResultFailed, ResultClientCancel, ResultLocalReject, ResultReservationReject:
		return true
	}
	return false
}

func (r AttemptResult) String() string {
	switch r {
	case ResultSuccess:
		return "success"
	case ResultFailed:
		return "failed"
	case ResultClientCancel:
		return "client_cancel"
	case ResultLocalReject:
		return "local_reject"
	case ResultReservationReject:
		return "reservation_reject"
	default:
		return "unknown"
	}
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
	CallerChat        CallerCategory = "chat"
	CallerResponses   CallerCategory = "responses"
	CallerAnthropic   CallerCategory = "anthropic"
	CallerConverted   CallerCategory = "converted"
	CallerImages      CallerCategory = "images"
	CallerImagesCodex CallerCategory = "images_codex"
	CallerCodexHTTP   CallerCategory = "codex_http"
	CallerResponsesWS CallerCategory = "responses_ws"
	CallerCodexWS     CallerCategory = "codex_ws"
	CallerSearch      CallerCategory = "search"
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

type AttemptOutcome struct {
	ID                AttemptID
	RouteClassID      RouteClassID
	Fingerprint       CandidateFingerprint
	LifecycleRevision LifecycleRevision
	Lane              LaneID
	Generation        Generation
	Commit            CommitState
	Result            AttemptResult
	HTTPStatus        AttemptStatus
	Timing            AttemptTiming
	Usage             AttemptUsage
	PreviousAttemptID *AttemptID
	Terminal          bool
	HardContinuation  bool
	BusinessFrameSent bool
	IsMalformed       bool
}

func (o AttemptOutcome) IsDispatched() bool {
	switch o.Result {
	case ResultLocalReject, ResultReservationReject, ResultUnknown:
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
	if o.Result == ResultUnknown {
		return false
	}
	return true
}

func (o AttemptOutcome) IsFailed() bool {
	if !o.IsCountedForQuality() {
		return false
	}
	return o.Result == ResultFailed
}

func (o AttemptOutcome) HasPossiblyWrittenBytes() bool {
	switch o.Commit {
	case CommitResponseStarted, CommitClientCommitted, CommitSentAmbiguous:
		return true
	default:
		return false
	}
}

func (o AttemptOutcome) Validate() error {
	if err := o.ID.Validate(); err != nil {
		return err
	}
	if !o.Commit.Valid() {
		return fmt.Errorf("invalid CommitState %d", o.Commit)
	}
	if !o.Result.Valid() {
		return fmt.Errorf("invalid Result %d", o.Result)
	}
	if o.Result == ResultUnknown {
		return fmt.Errorf("ResultUnknown is not valid")
	}
	if o.Terminal && o.Commit == CommitNotSent {
		return fmt.Errorf("terminal not_sent is invalid")
	}
	if o.Commit == CommitNotSent && o.BusinessFrameSent {
		return fmt.Errorf("not_sent cannot have BusinessFrameSent")
	}
	if o.BusinessFrameSent && o.Commit == CommitNotSent {
		return fmt.Errorf("BusinessFrameSent requires commit != not_sent")
	}
	if o.Commit == CommitResponseStarted || o.Commit == CommitClientCommitted {
		if !o.BusinessFrameSent {
			return fmt.Errorf("response_started/client_committed requires BusinessFrameSent")
		}
	}
	if o.Result == ResultLocalReject || o.Result == ResultReservationReject {
		if o.Commit != CommitNotSent {
			return fmt.Errorf("local/reservation reject must be not_sent")
		}
		if o.HTTPStatus != 0 {
			return fmt.Errorf("local/reservation reject must have status 0")
		}
		if o.BusinessFrameSent {
			return fmt.Errorf("local/reservation reject cannot have BusinessFrameSent")
		}
		if o.IsMalformed {
			return fmt.Errorf("local/reservation reject cannot be malformed")
		}
	}
	if o.IsMalformed {
		if o.Result != ResultFailed {
			return fmt.Errorf("malformed must be failed")
		}
		if o.Commit != CommitNotSent {
			return fmt.Errorf("malformed must be not_sent")
		}
		if o.BusinessFrameSent {
			return fmt.Errorf("malformed cannot have BusinessFrameSent")
		}
		if o.Result == ResultClientCancel {
			return fmt.Errorf("malformed cannot be client_cancel")
		}
	}
	if o.Result == ResultClientCancel {
		if o.Commit != CommitNotSent && o.Commit != CommitResponseStarted {
			// client cancel is flow-only; allow either before or after start but must not be ambiguous terminal inconsistency
		}
		if o.IsMalformed {
			return fmt.Errorf("client_cancel cannot be malformed")
		}
	}
	if o.IsDispatched() {
		if o.RouteClassID == "" {
			return fmt.Errorf("RouteClassID required for dispatched attempt")
		}
		if o.Fingerprint == "" {
			return fmt.Errorf("Fingerprint required for dispatched attempt")
		}
		if !o.Lane.Valid() {
			return fmt.Errorf("Lane required and must be valid for dispatched attempt")
		}
		if o.Generation < 0 {
			return fmt.Errorf("Generation must be >=0")
		}
		if o.LifecycleRevision < 0 {
			return fmt.Errorf("LifecycleRevision must be >=0")
		}
	}
	if o.Timing.LatencyMS < 0 {
		return fmt.Errorf("LatencyMS must be >=0")
	}
	if o.Timing.TTFTMS != nil && *o.Timing.TTFTMS < 0 {
		return fmt.Errorf("TTFTMS must be >=0")
	}
	if o.Usage.InputTokens < 0 || o.Usage.OutputTokens < 0 || o.Usage.CacheReadTokens < 0 || o.Usage.CacheCreationTokens < 0 || o.Usage.CallCount < 0 {
		return fmt.Errorf("usage tokens must be >=0")
	}
	if o.Result == ResultSuccess {
		if o.HTTPStatus < 200 || o.HTTPStatus >= 300 {
			return fmt.Errorf("success must have 2xx status")
		}
		if o.Commit != CommitResponseStarted && o.Commit != CommitClientCommitted && o.Commit != CommitNotSent {
			// success after response started or committed; allow NotSent for immediate success? keep lenient
		}
	}
	if o.Result == ResultFailed {
		if o.HTTPStatus == 0 && o.Commit == CommitNotSent && !o.IsMalformed {
			// allow status 0 for network failure not_sent
		}
	}
	if o.Result == ResultUnknown {
		return fmt.Errorf("unknown result")
	}
	return nil
}
