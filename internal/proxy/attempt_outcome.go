// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import "fmt"

// AttemptID is a per-attempt identifier. Dispatched metadata
// (RouteClassID/Fingerprint/Lane/Generation/LifecycleRevision) currently
// uses placeholder values; canonical stable IDs will be provided by Task4/7/11
// (RouteClass/QualityClass/candidate fingerprint) and wired in Task13. Do
// not loosen to legacy Selection fields.
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
	CommitUpstreamResponded
	CommitResponseStarted
	CommitClientCommitted
)

func (c CommitState) String() string {
	switch c {
	case CommitNotSent:
		return "not_sent"
	case CommitSentAmbiguous:
		return "sent_ambiguous"
	case CommitUpstreamResponded:
		return "upstream_responded"
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
	case CommitNotSent, CommitSentAmbiguous, CommitUpstreamResponded, CommitResponseStarted, CommitClientCommitted:
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
	if o.Terminal && o.Commit == CommitNotSent && o.Result != ResultClientCancel {
		return fmt.Errorf("terminal not_sent is invalid")
	}
	// BusinessFrameSent consistency
	if o.Commit == CommitNotSent && o.BusinessFrameSent {
		return fmt.Errorf("not_sent cannot have BusinessFrameSent")
	}
	if o.BusinessFrameSent && o.Commit == CommitNotSent {
		return fmt.Errorf("BusinessFrameSent requires commit != not_sent")
	}
	if (o.Commit == CommitResponseStarted || o.Commit == CommitClientCommitted) && !o.BusinessFrameSent {
		return fmt.Errorf("response_started/client_committed requires BusinessFrameSent")
	}
	if o.Commit == CommitUpstreamResponded && o.BusinessFrameSent {
		return fmt.Errorf("upstream_responded cannot have BusinessFrameSent")
	}
	if o.Commit == CommitSentAmbiguous && !o.BusinessFrameSent {
		return fmt.Errorf("sent_ambiguous requires BusinessFrameSent")
	}
	// Generation / LifecycleRevision >0 for dispatched (placeholder zero rejected; canonical values from Task4/7/11)
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
		if o.Generation <= 0 {
			return fmt.Errorf("Generation must be >0")
		}
		if o.LifecycleRevision <= 0 {
			return fmt.Errorf("LifecycleRevision must be >0")
		}
	} else {
		// non-dispatched must have no dispatch metadata and status0 not_sent
		if o.RouteClassID != "" || o.Fingerprint != "" || o.Lane != "" || o.Generation != 0 || o.LifecycleRevision != 0 {
			return fmt.Errorf("non-dispatched must have no dispatch metadata")
		}
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
		if o.Terminal {
			return fmt.Errorf("local/reservation reject cannot be terminal")
		}
	}
	// Malformed: never retry, commit upstream_responded/response_started/client_committed, never not_sent/sent_ambiguous
	if o.IsMalformed {
		if o.Result != ResultFailed {
			return fmt.Errorf("malformed must be failed")
		}
		switch o.Commit {
		case CommitUpstreamResponded, CommitResponseStarted, CommitClientCommitted:
		default:
			return fmt.Errorf("malformed must be upstream_responded/response_started/client_committed")
		}
		if o.Result == ResultClientCancel || o.Result == ResultLocalReject || o.Result == ResultReservationReject {
			return fmt.Errorf("malformed cannot be cancel/local/reservation")
		}
	}
	// Timing / usage
	if o.Timing.LatencyMS < 0 {
		return fmt.Errorf("LatencyMS must be >=0")
	}
	if o.Timing.TTFTMS != nil && *o.Timing.TTFTMS < 0 {
		return fmt.Errorf("TTFTMS must be >=0")
	}
	if o.Usage.InputTokens < 0 || o.Usage.OutputTokens < 0 || o.Usage.CacheReadTokens < 0 || o.Usage.CacheCreationTokens < 0 || o.Usage.CallCount < 0 {
		return fmt.Errorf("usage tokens must be >=0")
	}
	// HTTP status ranges
	if o.HTTPStatus < 0 || o.HTTPStatus > 599 {
		return fmt.Errorf("HTTPStatus out of range")
	}
	if o.HTTPStatus >= 100 && o.HTTPStatus <= 199 {
		return fmt.Errorf("1xx status not allowed")
	}
	if o.HTTPStatus >= 300 && o.HTTPStatus <= 399 {
		return fmt.Errorf("3xx status not allowed")
	}
	// Exhaustive result/status/commit combinations, no empty branches
	switch o.Result {
	case ResultSuccess:
		if o.IsMalformed {
			return fmt.Errorf("success cannot be malformed")
		}
		if o.HTTPStatus < 200 || o.HTTPStatus >= 300 {
			return fmt.Errorf("success must have 2xx status")
		}
		if o.Commit != CommitResponseStarted && o.Commit != CommitClientCommitted {
			return fmt.Errorf("success must be response_started or client_committed")
		}
		if !o.Terminal {
			return fmt.Errorf("success must be terminal")
		}
		if !o.BusinessFrameSent {
			return fmt.Errorf("success must have BusinessFrameSent")
		}
	case ResultFailed:
		if o.IsMalformed {
			// malformed already validated above; ensure status not negative/1xx/3xx already checked
			if o.HTTPStatus != 0 && (o.HTTPStatus < 200 || o.HTTPStatus > 599) {
				return fmt.Errorf("malformed status invalid")
			}
			if o.Terminal != true {
				return fmt.Errorf("malformed must be terminal")
			}
			break
		}
		switch {
		case o.HTTPStatus == 429:
			if o.Commit != CommitUpstreamResponded {
				return fmt.Errorf("429 must be upstream_responded")
			}
			// terminal explicit per retryability: ordinary retryable => Terminal false, hard => true
			if o.HardContinuation && !o.Terminal {
				return fmt.Errorf("hard 429 must be terminal")
			}
			if !o.HardContinuation && o.Terminal {
				return fmt.Errorf("ordinary 429 must not be terminal")
			}
		case o.HTTPStatus >= 400 && o.HTTPStatus <= 499:
			// ordinary 4xx except 429 already handled
			if o.Commit != CommitUpstreamResponded {
				return fmt.Errorf("4xx must be upstream_responded")
			}
			if !o.Terminal {
				return fmt.Errorf("4xx must be terminal")
			}
		case o.HTTPStatus >= 500 && o.HTTPStatus <= 599:
			if o.Commit != CommitUpstreamResponded {
				return fmt.Errorf("5xx must be upstream_responded")
			}
			if !o.Terminal {
				return fmt.Errorf("5xx must be terminal")
			}
		case o.HTTPStatus == 0:
			// network: only not_sent or sent_ambiguous
			if o.Commit != CommitNotSent && o.Commit != CommitSentAmbiguous {
				return fmt.Errorf("status0 network must be not_sent or sent_ambiguous")
			}
			if o.Commit == CommitSentAmbiguous && !o.Terminal {
				return fmt.Errorf("sent_ambiguous must be terminal")
			}
			if o.Commit == CommitNotSent && o.Terminal {
				return fmt.Errorf("not_sent network must not be terminal")
			}
		case o.HTTPStatus >= 200 && o.HTTPStatus <= 299:
			return fmt.Errorf("failed cannot have 2xx status")
		default:
			// 0 already handled, 2xx handled, 4xx/5xx handled, 429 handled
			return fmt.Errorf("failed with unexpected status %d", o.HTTPStatus)
		}
	case ResultClientCancel:
		if o.IsMalformed {
			return fmt.Errorf("client_cancel cannot be malformed")
		}
		if o.HTTPStatus != 0 {
			return fmt.Errorf("client_cancel must have status 0")
		}
		// explicitly allowed states: not_sent, sent_ambiguous, response_started, client_committed (flow-only)
		switch o.Commit {
		case CommitNotSent, CommitSentAmbiguous, CommitResponseStarted, CommitClientCommitted:
		default:
			return fmt.Errorf("client_cancel has invalid commit %v", o.Commit)
		}
		if o.Commit == CommitNotSent && o.BusinessFrameSent {
			return fmt.Errorf("client_cancel not_sent cannot have BusinessFrameSent")
		}
		if (o.Commit == CommitResponseStarted || o.Commit == CommitClientCommitted) && !o.BusinessFrameSent {
			return fmt.Errorf("client_cancel response_started/committed requires BusinessFrameSent")
		}
		if o.Commit == CommitSentAmbiguous && !o.BusinessFrameSent {
			return fmt.Errorf("client_cancel sent_ambiguous requires BusinessFrameSent")
		}
		if o.Commit == CommitUpstreamResponded {
			return fmt.Errorf("client_cancel cannot be upstream_responded")
		}
		if !o.Terminal {
			return fmt.Errorf("client_cancel must be terminal")
		}
	case ResultLocalReject, ResultReservationReject:
		// already validated non-dispatched branch
	default:
		return fmt.Errorf("unexpected result %v", o.Result)
	}
	// Success+ambiguous invalid
	if o.Result == ResultSuccess && o.Commit == CommitSentAmbiguous {
		return fmt.Errorf("success cannot be sent_ambiguous")
	}
	if o.Result == ResultSuccess && o.Commit == CommitUpstreamResponded {
		return fmt.Errorf("success cannot be upstream_responded")
	}
	return nil
}
