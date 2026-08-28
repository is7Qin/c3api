// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

func CanRetry(cat CallerCategory, o AttemptOutcome) bool {
	if !cat.Valid() {
		return false
	}
	if err := o.Validate(); err != nil {
		return false
	}
	if o.Terminal {
		return false
	}
	if o.Result == ResultLocalReject || o.Result == ResultReservationReject || o.Result == ResultUnknown {
		return false
	}
	if o.Result == ResultClientCancel {
		return false
	}
	if o.IsMalformed {
		return false
	}
	if o.HasPossiblyWrittenBytes() {
		return false
	}
	switch o.Commit {
	case CommitSentAmbiguous, CommitResponseStarted, CommitClientCommitted:
		return false
	}
	// Explicit pre-response 429: must be upstream_responded, ordinary non-hard
	if o.HTTPStatus == 429 && o.Commit == CommitUpstreamResponded {
		if o.HardContinuation {
			return false
		}
		return true
	}
	// 5xx never retry (no idempotency capability)
	if o.HTTPStatus >= 500 && o.HTTPStatus <= 599 {
		return false
	}
	// network not_sent may retry
	if o.HTTPStatus == 0 && o.Commit == CommitNotSent && !o.BusinessFrameSent && o.Result == ResultFailed && !o.IsMalformed {
		return true
	}
	return false
}

func RetryMatrixGolden() map[CallerCategory]map[string]bool {
	cats := AllCallerCategories()
	kinds := []string{"not_sent", "429", "5xx", "sent_ambiguous", "response_started", "client_committed", "client_cancel", "malformed"}
	out := make(map[CallerCategory]map[string]bool)
	for _, cat := range cats {
		m := make(map[string]bool)
		for _, k := range kinds {
			o := outcomeForKind(k, cat, false)
			m[k] = CanRetry(cat, o)
		}
		out[cat] = m
	}
	return out
}

func outcomeForKind(kind string, cat CallerCategory, hard bool) AttemptOutcome {
	base := AttemptOutcome{
		ID:                "attempt-1",
		RouteClassID:      "rc1",
		Fingerprint:       "fp1",
		LifecycleRevision: 1,
		Lane:              LanePrimary,
		Generation:        1,
		HardContinuation:  hard,
		BusinessFrameSent: false,
		Terminal:          false,
	}
	switch kind {
	case "not_sent":
		base.Commit = CommitNotSent
		base.Result = ResultFailed
		base.HTTPStatus = 0
		base.BusinessFrameSent = false
		base.Terminal = false
	case "429":
		base.Commit = CommitUpstreamResponded
		base.Result = ResultFailed
		base.HTTPStatus = 429
		base.BusinessFrameSent = false
		// Terminal explicit per hard/ordinary
		if hard {
			base.Terminal = true
		} else {
			base.Terminal = false
		}
	case "5xx":
		base.Commit = CommitUpstreamResponded
		base.Result = ResultFailed
		base.HTTPStatus = 500
		base.Terminal = true
	case "sent_ambiguous":
		base.Commit = CommitSentAmbiguous
		base.Result = ResultFailed
		base.HTTPStatus = 0
		base.BusinessFrameSent = true
		base.Terminal = true
	case "response_started":
		base.Commit = CommitResponseStarted
		base.Result = ResultFailed
		base.HTTPStatus = 500
		base.BusinessFrameSent = true
		base.Terminal = true
	case "client_committed":
		base.Commit = CommitClientCommitted
		base.Result = ResultFailed
		base.HTTPStatus = 200
		base.BusinessFrameSent = true
		base.Terminal = true
	case "client_cancel":
		base.Commit = CommitNotSent
		base.Result = ResultClientCancel
		base.HTTPStatus = 0
		base.Terminal = true
	case "malformed":
		base.Commit = CommitUpstreamResponded
		base.Result = ResultFailed
		base.HTTPStatus = 400
		base.IsMalformed = true
		base.Terminal = true
	}
	_ = cat
	return base
}
