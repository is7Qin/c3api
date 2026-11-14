// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

// IsRetryable5xx 是 5xx 换号重试的白名单（唯一事实源）：只收明确的瞬时/
// 网关侧故障。
//
//   - 500/502/503/504：上游内部错、网关坏响应、过载/维护、超时——换号有意义。
//   - 520–527、529、530：Cloudflare 源站错误族（源站返回未知/不可达/TLS/超时/
//     连接/回源 DNS 等），换号同样有意义。
//
// 永久排除的 5xx：
//   - 501 Not Implemented：上游**不支持**该能力（端点/参数/协议面），换到任何
//     同模板账号都不会变得支持——换号只是把确定性拒绝放大成 N 次调用。
//   - 505 HTTP Version Not Supported：协议版本协商失败，与账号无关，换号无意义。
//
// 其余 5xx（506/507/508/510/511 等）同样不在集合内：语义上是确定性/资源性
// 拒绝，不做未经验证的换号。冷却（seed-5xx）与本集合是两套独立机制：白名单
// 内的 5xx 照常按 RuleKindOf 命中 seed-5xx，本集合只决定「能不能换号」。
func IsRetryable5xx(status AttemptStatus) bool {
	switch status {
	case 500, 502, 503, 504:
		return true
	case 520, 521, 522, 523, 524, 525, 526, 527, 529, 530:
		return true
	}
	return false
}

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
	case CommitSentAmbiguous, CommitResponseStarted:
		return false
	}
	// Explicit pre-response 429: must be upstream_responded, ordinary non-hard
	if o.HTTPStatus == 429 && o.Commit == CommitUpstreamResponded {
		if o.HardContinuation {
			return false
		}
		return true
	}
	// Retryable upstream 5xx: the upstream answered with a transient/gateway
	// failure before a single business byte was written, so a different account
	// can still serve the request. Every non-idempotent prerequisite is already
	// excluded at the top of this function (Terminal / ResultLocalReject /
	// ResultReservationReject / ResultUnknown / ResultClientCancel / IsMalformed /
	// HasPossiblyWrittenBytes / Commit ∈ {sent_ambiguous, response_started}).
	if IsRetryable5xx(o.HTTPStatus) {
		if o.Commit != CommitUpstreamResponded {
			return false
		}
		if o.BusinessFrameSent {
			return false
		}
		if o.HardContinuation {
			return false
		}
		return true
	}
	// 5xx never retry: fallback for the 5xx codes outside the retryable set
	// (501/505 are permanently incompatible; the rest are deterministic or
	// resource rejections no account switch can fix).
	if o.HTTPStatus >= 500 && o.HTTPStatus <= 599 {
		return false
	}
	// network not_sent may retry — never for a hard continuation: the bound
	// account is the only valid target, another account cannot resolve the
	// referenced response.
	if o.HTTPStatus == 0 && o.Commit == CommitNotSent && !o.BusinessFrameSent && o.Result == ResultFailed && !o.IsMalformed && !o.HardContinuation {
		return true
	}
	return false
}

func outcomeForKind(kind string, cat CallerCategory, hard bool) AttemptOutcome {
	base := AttemptOutcome{
		ID:                "attempt-1",
		RouteClassID:      "rc1",
		QualityClassID:    "qc1",
		Fingerprint:       "fp1",
		TemplateID:        1,
		AccountID:         1,
		RequestedModel:    "gpt-4o",
		MappedModel:       "gpt-4o",
		CallerCategory:    cat,
		OperationTag:      "chat_completions",
		Ordinal:           1,
		IdentityRevision:  1, // synthetic placeholder K; not part of the continuation (I,K) comparison (see AttemptOutcome.IdentityRevision)
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
		// 零业务字节的 5xx 可换号（Terminal 不短路）；已写字节的 5xx 由矩阵
		// 前置的 HasPossiblyWrittenBytes / BusinessFrameSent 挡住，仍是终态。
		base.Terminal = false
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
	return base
}
