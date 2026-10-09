// SPDX-License-Identifier: AGPL-3.0-or-later
// Hard-continuation wiring for the Responses REST/WS callers. The binding
// authority is internal/continuation (single store, single key format).
//
// REST streams are no longer gated: the first valid response id frame is
// snapshotted at the write seam and enqueued to the async bind worker
// (ContBindWorker) — business frames go out immediately, binding is batched
// and best-effort, and its failures are only observable (never written back to
// the current response). A previous_response_id request resolves that binding
// and pins the dispatch to it — missing, expired, conflicting, revision-stale
// or Redis-unavailable all fail closed, and a hard-continuation request never
// migrates to another account. WS keeps its synchronous ACK-before-visible
// gate (wsContFrame/ws_relay.go), and non-streaming Responses keep the
// synchronous bind. Ordinary requests (no previous_response_id, non-Responses
// formats, codex credential branches) issue zero Redis commands.

package proxy

import (
	"bytes"
	"context"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/continuation"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/sserelay"
)

const (
	// contProtocolREST / contProtocolWS are the protocol tags of the canonical
	// continuation tuple. REST and WS are separate continuation surfaces: an id
	// produced on one is not resolvable on the other (fail-closed on missing).
	contProtocolREST = "responses"
	contProtocolWS   = "responses-ws"
)

var (
	// contOpTimeout bounds one Redis round trip (ACK or lookup). var = test
	// seam (Redis-outage cases fail fast without sleeps).
	contOpTimeout = 2 * time.Second
	// contWSAckTimeout bounds the upstream read while a WS session still has
	// no ACKed binding (no overall WS stream timeout exists by design).
	contWSAckTimeout = 10 * time.Second
)

// contMaxBuffer caps bytes held invisible before the first response id is ACKed.
// WS pending buffer only — REST streams are ungated since ①. const: no test
// rewrites it (unlike contOpTimeout/contWSAckTimeout).
const contMaxBuffer = 1 << 20

// Fail-closed continuation errors. Messages carry no raw ids, keys or
// upstream detail (anti-pattern 7): the client learns the outcome class only.
var (
	errContUnavailable = &formatError{status: http.StatusServiceUnavailable, msg: "continuation state unavailable"}
	errContNotFound    = &formatError{status: http.StatusGone, msg: "previous response not found"}
	errContStale       = &formatError{status: http.StatusConflict, msg: "previous response binding is stale"}
	errContConflict    = &formatError{status: http.StatusBadGateway, msg: "response binding conflict"}
)

// contBind records respID → canonical dispatched-attempt identity. The
// identity is read from the loop-owned dispatch observation (plan-canonical
// route/fingerprint/revision — never a placeholder); without it the bind fails
// closed. nil = store unwired (tests / not deployed) or respID empty → no-op.
func (p *Proxy) contBind(ctx context.Context, protocolTag, respID string, groupID int64) *formatError {
	if p.cont == nil || respID == "" {
		return nil
	}
	d := dispatchFromContext(ctx)
	if d == nil {
		return errContUnavailable
	}
	rm, ok := ctx.Value(ctxKeyReqMeta{}).(*reqMeta)
	if !ok || rm.meta.UserID <= 0 {
		return errContUnavailable
	}
	fpBytes, err := hex.DecodeString(string(d.base.Fingerprint))
	if err != nil || len(fpBytes) != len(domain.CandidateFingerprintVal{}) {
		return errContUnavailable
	}
	var fp domain.CandidateFingerprintVal
	copy(fp[:], fpBytes)
	opCtx, cancel := context.WithTimeout(ctx, contOpTimeout)
	defer cancel()
	st, err := p.cont.CreateOrRefresh(opCtx, rm.meta.UserID, groupID,
		domain.RouteClassIDVal(pipelineID(string(d.base.RouteClassID))), protocolTag, respID,
		d.base.AccountID, fp, int64(d.base.IdentityRevision))
	if err != nil {
		return errContUnavailable
	}
	switch st {
	case "created", "refreshed":
		return nil
	case "conflict":
		return errContConflict
	default:
		return errContUnavailable
	}
}

// contBindWired reports whether the REST streaming async bind path is wired
// (store + worker). Call sites use it to keep the unwired path zero-behaviour
// (no enqueue, no counting, no Mapper wrapper).
func (p *Proxy) contBindWired() bool { return p.cont != nil && p.contBinder != nil }

// contBindMapper wraps a Responses SSE mapper so the first valid response id is
// snapshotted and enqueued BEFORE the frame is written (M1: the ① enqueue seam
// is pre-write for both the native wrapped Mapper and the converted Mapper).
// Unwired (store or worker nil) returns base unchanged — no wrapper, zero
// behaviour change. base == nil (no model rewrite) still enqueues and forwards
// the raw frame (matches sserelay's nil-Mapper pass-through).
func (p *Proxy) contBindMapper(ctx context.Context, done *bool, groupID int64, base func(sserelay.Event) ([]byte, bool)) func(sserelay.Event) ([]byte, bool) {
	if !p.contBindWired() {
		return base
	}
	return func(ev sserelay.Event) ([]byte, bool) {
		if !*done {
			if id := contFrameID(ev.Data); id != "" {
				p.contEnqueue(ctx, contProtocolREST, id, groupID)
				*done = true
			}
		}
		if base == nil {
			return ev.Raw, false
		}
		return base(ev)
	}
}

// contEnqueue snapshots the bindable response id at the SSE write seam and
// hands it to the async bind worker. Unwired (store or worker nil) is a no-op
// with zero behaviour change. When wired, an id that cannot be attributed (empty
// id, no loop-owned dispatch observation, no request meta, undecodable
// fingerprint) is counted on the worker and never written to Redis. The snapshot
// is taken here (values are copied) — the worker never sees the request ctx,
// dispatch or relay slices.
func (p *Proxy) contEnqueue(ctx context.Context, protocolTag, respID string, groupID int64) {
	if p.cont == nil || p.contBinder == nil {
		return
	}
	if respID == "" {
		p.contBinder.DropUnattributed()
		return
	}
	d := dispatchFromContext(ctx)
	if d == nil {
		p.contBinder.DropUnattributed()
		return
	}
	rm, ok := ctx.Value(ctxKeyReqMeta{}).(*reqMeta)
	if !ok || rm.meta.UserID <= 0 {
		p.contBinder.DropUnattributed()
		return
	}
	fpBytes, err := hex.DecodeString(string(d.base.Fingerprint))
	if err != nil || len(fpBytes) != len(domain.CandidateFingerprintVal{}) {
		p.contBinder.DropUnattributed()
		return
	}
	var fp domain.CandidateFingerprintVal
	copy(fp[:], fpBytes)
	p.contBinder.Enqueue(continuation.BindRequest{
		UserID:           rm.meta.UserID,
		GroupID:          groupID,
		RouteClassID:     domain.RouteClassIDVal(pipelineID(string(d.base.RouteClassID))),
		ProtocolTag:      protocolTag,
		ContinuationID:   respID,
		AccountID:        d.base.AccountID,
		Fingerprint:      fp,
		IdentityRevision: int64(d.base.IdentityRevision),
	})
}

// contResolve looks up the binding for a previous_response_id. The route class
// is the plan-canonical identity of the request (the same value contBind wrote
// — never a re-derivation that can diverge from the compiled bucket the plan
// actually resolved). An unwired store fails closed: without the binding
// authority the continuation cannot be pinned to its owning account.
//
// the settled attempt threads in (no CurrentAttempt on the hot path).
func (p *Proxy) contResolve(ctx context.Context, userID, groupID int64, protocolTag, prevID string, attempt scheduler.Attempt) (*continuation.Binding, *formatError) {
	if p.cont == nil {
		return nil, errContUnavailable
	}
	rc := domain.RouteClassIDVal(pipelineID(attempt.RouteClassID))
	opCtx, cancel := context.WithTimeout(ctx, contOpTimeout)
	defer cancel()
	b, ok, err := p.cont.Lookup(opCtx, userID, groupID, rc, protocolTag, prevID)
	if err != nil {
		return nil, errContUnavailable
	}
	if !ok {
		return nil, errContNotFound
	}
	return b, nil
}

// contPin advances the request plan until the reserved attempt carries the
// binding's account/fingerprint/revision. Reservations for other accounts are
// released, refunded (AbandonLastAttempt: a skipped unbound candidate never
// consumes maxAttempts/ordinal — it was never dispatched) and skipped (the
// plan is walked, never re-selected); the bound account itself mutating
// (fingerprint/revision drift) or being undispatchable fails closed — a hard
// continuation never migrates.
//
// session-local projection — the loop threads the settled Attempt
// values (entry attempt in, next attempt per advance) and never value-returns
// CurrentAttempt on the hot path.
func (p *Proxy) contPin(plan *scheduler.AttemptPlan, sel *scheduler.Selection, attempt scheduler.Attempt, b *continuation.Binding) (*scheduler.Selection, scheduler.Attempt, *formatError) {
	for {
		if attempt.AccountID == b.AccountID {
			if attempt.CandidateFingerprint == hex.EncodeToString(b.Fingerprint[:]) &&
				attempt.IdentityRevision == b.IdentityRevision {
				return sel, attempt, nil
			}
			sel.Release()
			return nil, scheduler.Attempt{}, errContStale
		}
		sel.Release()
		plan.AbandonLastAttempt()
		next, nextAttempt, err := p.selectNextWithPlan(plan)
		if err != nil {
			return nil, scheduler.Attempt{}, errContStale
		}
		sel, attempt = next, nextAttempt
	}
}

// contFrameID extracts the response id a frame makes continuable: the
// response object id (created/in_progress/completed events) or the flat
// response_id (delta events). bytes.Contains prefilter keeps non-id frames
// zero-parse (hot-path discipline).
func contFrameID(frame []byte) string {
	if !bytes.Contains(frame, []byte(`"id"`)) && !bytes.Contains(frame, []byte(`"response_id"`)) {
		return ""
	}
	if id := gjson.GetBytes(frame, "response.id").String(); id != "" {
		return id
	}
	return gjson.GetBytes(frame, "response_id").String()
}

// wsContFrame 一条被闸门扣住的上游 WS 帧（typ + 模型改写后的载荷）：首个
// response id 的 Redis ACK 前不得转发客户端；ACK 后按序放出。
type wsContFrame struct {
	typ   websocket.MessageType
	frame []byte
}
