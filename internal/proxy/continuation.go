// SPDX-License-Identifier: AGPL-3.0-or-later
// Hard-continuation wiring for the Responses REST/WS callers. The binding
// authority is internal/continuation (single store, single key format): every
// produced response id is bound to the canonical plan attempt identity
// (account / candidate fingerprint / lifecycle revision) BEFORE the id becomes
// visible to the client, and a previous_response_id request resolves that
// binding and pins the dispatch to it — missing, expired, conflicting,
// revision-stale or Redis-unavailable all fail closed, and a hard-continuation
// request never migrates to another account. Ordinary requests (no
// previous_response_id, non-Responses formats, codex credential branches)
// issue zero Redis commands.

package proxy

import (
	"bytes"
	"context"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/continuation"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
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
	// contMaxBuffer caps bytes held invisible before the first response id is
	// ACKed (stream gate / WS pending buffer). var = test seam.
	contMaxBuffer = 1 << 20
	// contWSAckTimeout bounds the upstream read while a WS session still has
	// no ACKed binding (no overall WS stream timeout exists by design).
	contWSAckTimeout = 10 * time.Second
)

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
		d.base.AccountID, fp, int64(d.base.LifecycleRevision))
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

// contResolve looks up the binding for a previous_response_id. The route class
// is the plan-canonical identity of the request (the same value contBind wrote
// — never a re-derivation that can diverge from the compiled bucket the plan
// actually resolved). An unwired store fails closed: without the binding
// authority the continuation cannot be pinned to its owning account.
//
// v4-S1: the settled attempt threads in (no CurrentAttempt on the hot path).
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
// v4-S1: session-local projection — the loop threads the settled Attempt
// values (entry attempt in, next attempt per advance) and never value-returns
// CurrentAttempt on the hot path.
func (p *Proxy) contPin(plan *scheduler.AttemptPlan, sel *scheduler.Selection, attempt scheduler.Attempt, b *continuation.Binding) (*scheduler.Selection, scheduler.Attempt, *formatError) {
	for {
		if attempt.AccountID == b.AccountID {
			if attempt.CandidateFingerprint == hex.EncodeToString(b.Fingerprint[:]) &&
				attempt.LifecycleRevision == b.Revision {
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

// contGateWriter buffers every relay byte until the first response id is
// ACKed (release) — ACK-before-visible for SSE streams. Header writes stay
// uncommitted while gated, so a fail-closed path can still emit a JSON error.
// Unwrap exposes the real writer to sserelay's ResponseController deadline
// watcher exactly as an unwrapped relay would. sserelay invokes Write/Flush
// from its flush-timer goroutine while the Observer (release/state) runs on
// the relay goroutine outside the relay mutex — every field is mu-guarded and
// all downstream writes are serialized through the same lock.
type contGateWriter struct {
	mu       sync.Mutex
	w        http.ResponseWriter
	buf      []byte
	released bool
	failed   bool
}

func (g *contGateWriter) Header() http.Header { return g.w.Header() }

func (g *contGateWriter) WriteHeader(code int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.released {
		g.w.WriteHeader(code)
	}
}

func (g *contGateWriter) Write(b []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.released {
		return g.w.Write(b)
	}
	if g.failed {
		return 0, errContUnavailable
	}
	if len(g.buf)+len(b) > contMaxBuffer {
		g.failed = true
		return 0, errContUnavailable
	}
	g.buf = append(g.buf, b...)
	return len(b), nil
}

func (g *contGateWriter) Flush() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.released {
		return
	}
	if f, ok := g.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *contGateWriter) Unwrap() http.ResponseWriter { return g.w }

// gateState snapshots the two flags the Observer discriminates on.
func (g *contGateWriter) gateState() (released, failed bool) {
	g.mu.Lock()
	released, failed = g.released, g.failed
	g.mu.Unlock()
	return released, failed
}

// notReleased reports whether the gate still holds bytes invisible to the
// client (nil gate = store unwired = never gated).
func (g *contGateWriter) notReleased() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	r := g.released
	g.mu.Unlock()
	return !r
}

// markFailed poisons the gate (bind failure): buffered bytes are discarded on
// the fail-closed terminal, further relay writes error out.
func (g *contGateWriter) markFailed() {
	g.mu.Lock()
	g.failed = true
	g.mu.Unlock()
}

// release flushes every buffered frame — the first point at which any byte of
// the response (and its id) becomes visible to the client.
func (g *contGateWriter) release() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.released || g.failed {
		return nil
	}
	g.released = true
	if len(g.buf) > 0 {
		if _, err := g.w.Write(g.buf); err != nil {
			g.failed = true
			g.buf = nil
			return err
		}
		g.buf = nil
	}
	if f, ok := g.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}
