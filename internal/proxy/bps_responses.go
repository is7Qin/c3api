// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

// errBPSResponsesNotIntegrated is deliberately a local deployment error. Until
// the durable TokenStore and the provider-neutral relay are wired at the
// composition root, a BPS selection must never fall through to the generic
// static-key caller or the Codex adapter.
var errBPSResponsesNotIntegrated = &formatError{
	status: http.StatusServiceUnavailable,
	msg:    "BPS responses unavailable (adapter not wired)",
}

// rejectBPSHTTP is the fail-closed HTTP seam. The selection is released here
// because the caller reports handled=true and therefore the failover loop will
// not own an upstream attempt to release it.
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
