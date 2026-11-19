// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/sdkbridge"
)

// A BPS selection must not fall through to the generic static-key or Codex
// caller when the adapter is missing or its selection cannot be prepared.
var errBPSResponsesNotIntegrated = &formatError{
	status: http.StatusServiceUnavailable,
	msg:    "BPS responses unavailable (adapter not wired)",
}

// bpsAttemptError carries failure classification without exposing the upstream
// error envelope or internal transport diagnostics to the client or logs.
type bpsAttemptError struct {
	cause   error
	noRetry bool
}

// RawJSON 透出上游错误体：pipeline 的规则匹配（when.error_message_contains）与
// err_logs 的 error_message 都从这里取，和 codex/静态 key 路径同形。客户端文案
// 仍由 Error()/规则默认给固定句，不直接回显这段正文。
func (e *bpsAttemptError) RawJSON() string {
	var raw interface{ RawJSON() string }
	if errors.As(e.cause, &raw) {
		return raw.RawJSON()
	}
	return ""
}

func (e *bpsAttemptError) Error() string { return "BPS upstream rejected request" }
func (e *bpsAttemptError) Unwrap() error { return e.cause }

var errBPSDispatched = &bpsAttemptError{noRetry: true}

func bpsErrorResponse(err error) (int, error) {
	failure := sdkbridge.ClassifyBPSFailure(err)
	status := failure.Status
	if status < 400 || status > 599 {
		status = 0
	}
	if failure.Kind == sdkbridge.BPSFailureQuota && status == 0 {
		status = http.StatusTooManyRequests
	}
	noRetry := failure.Dispatched || failure.Kind == sdkbridge.BPSFailureQuota ||
		failure.Kind == sdkbridge.BPSFailureAmbiguous || failure.Kind == sdkbridge.BPSFailureIdleTimeout ||
		failure.Kind == sdkbridge.BPSFailureIncomplete
	return status, &bpsAttemptError{cause: err, noRetry: noRetry}
}

func isBPSLocalReject(err error) bool {
	return sdkbridge.ClassifyBPSFailure(err).Kind == sdkbridge.BPSFailureLocalReject
}

// bpsSkipsContinuation reports whether the selected account must skip
// continuation resolution: BPS has no server-side continuation, so the id is
// left in the request for the SDK to drop. c3api deliberately does no body
// work here (no scan/copy/marshal) — stripping is the SDK's job.
func bpsSkipsContinuation(sel *scheduler.Selection) bool {
	return sel != nil && sel.CredentialType.IsBPS()
}

// bpsInvalidBody builds the client-facing 400 for a pre-dial rejection. The SDK
// knows which field is wrong, so the message names it instead of the opaque
// "invalid request body" that gave operators nothing to act on.
func bpsInvalidBody(err error) *formatError {
	return &formatError{status: http.StatusBadRequest, msg: sdkbridge.BPSLocalRejectMessage(err)}
}

func writeBPSInvalidBodyFrame(ctx context.Context, client *websocket.Conn, err error) error {
	return writeCodexInvalidBodyFrameMessage(ctx, client, sdkbridge.BPSLocalRejectMessage(err))
}

func (p *Proxy) bpsHTTPError(ctx context.Context, w http.ResponseWriter, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, err error) (int, []byte, bool, error) {
	if isBPSLocalReject(err) {
		p.finishBPSLocalReject(ctx, reqID, groupID, start, sel, reqModel, domain.FormatOpenAIResponses)
		writeErr(w, bpsInvalidBody(err))
		return 0, nil, true, nil
	}
	code, safe := bpsErrorResponse(err)
	// 与 codex 路径一致：把上游错误体交给 pipeline，规则可以按 error.message 匹配，
	// err_logs 也能落真实原因；没有规则命中时仍是网关的固定 502 文案。
	return code, upstreamBody(err), false, markGatewayBudget(ctx, safe)
}

func (p *Proxy) finishBPSLocalReject(ctx context.Context, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, format domain.RequestFormat) {
	p.finish(sel, withCommit(logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), format, http.StatusBadRequest, domain.Err4xx, usageTuple{}, start)), CommitNotSent))
}

func (p *Proxy) rejectBPSHTTP(w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, format domain.RequestFormat) {
	if sel == nil {
		writeErr(w, errBPSResponsesNotIntegrated)
		return
	}
	sel.Release()
	p.recordRejected(r.Context(), reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), format, errBPSResponsesNotIntegrated.status, domain.ErrBilling, 0, usageTuple{}, start, errBPSResponsesNotIntegrated.msg)
	writeErr(w, errBPSResponsesNotIntegrated)
}

// rejectBPSWS is the corresponding post-upgrade seam. WebSocket requests carry
// the local error in a standard Responses error frame rather than an HTTP 503.
func (p *Proxy) rejectBPSWS(client *websocket.Conn, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string) {
	if sel != nil {
		p.recordRejected(r.Context(), reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), domain.FormatOpenAIResponsesWS, errBPSResponsesNotIntegrated.status, domain.ErrBilling, 0, usageTuple{}, start, errBPSResponsesNotIntegrated.msg)
		sel.Release()
	}
	wsWriteError(client, errBPSResponsesNotIntegrated.msg)
}
