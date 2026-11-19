// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/pkg/logx"
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
	status  int
	kind    sdkbridge.BPSFailureKind
}

// Error 带状态/分类：WS 面直接把它写进错误帧，err_logs 也落这一句；固定前缀 +
// 数值状态，不透传上游正文。HTTP 面仍由 pipeline 统一归一（见 bpsHTTPError）。
func (e *bpsAttemptError) Error() string {
	if e.status > 0 {
		return fmt.Sprintf("BPS upstream rejected request (status %d, %s)", e.status, e.kind)
	}
	return "BPS upstream rejected request"
}
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
	return status, &bpsAttemptError{cause: err, noRetry: noRetry, status: status, kind: failure.Kind}
}

func isBPSLocalReject(err error) bool {
	return sdkbridge.ClassifyBPSFailure(err).Kind == sdkbridge.BPSFailureLocalReject
}

// errBPSContinuationUnsupported is the pre-SDK rejection of previous_response_id:
// BPS has no server-side continuation, so the client must resend the full input.
var errBPSContinuationUnsupported = &formatError{
	status: http.StatusBadRequest,
	msg:    sdkbridge.BPSCodePreviousResponseUnsupported,
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
	p.logBPSFailure(reqID, sel, reqModel, err)
	code, safe := bpsErrorResponse(err)
	return code, nil, false, markGatewayBudget(ctx, safe)
}

// logBPSFailure 是 BPS 上游失败的运维面：只记状态/分类/上游错误码/上游 request id，
// 不记上游正文。没有这行，客户端只能拿到网关默认的 502，排障无入口。
func (p *Proxy) logBPSFailure(reqID string, sel *scheduler.Selection, reqModel string, err error) {
	if p.log == nil {
		return
	}
	failure := sdkbridge.ClassifyBPSFailure(err)
	var accountID int64
	if sel != nil {
		accountID = sel.AccountID
	}
	p.log.Warn("bps upstream failure",
		logx.String("request_id", reqID),
		logx.Int64("account_id", accountID),
		logx.String("model", reqModel),
		logx.Int("status", failure.Status),
		logx.String("kind", failure.Kind.String()),
		logx.String("upstream_code", failure.Code),
		logx.String("upstream_request_id", failure.RequestID))
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
