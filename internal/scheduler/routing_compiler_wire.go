// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"bytes"
	"context"
	"encoding/binary"
	"sort"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// compileDebounce is the trailing-edge coalescing window for compile triggers
// (plan Task11: 200ms debounce, publish only when bytes change).
const compileDebounce = 200 * time.Millisecond

// routeCompiler is the compile-lane seam (production: RoutingCompiler;
// tests inject failure to prove old-view retention).
type routeCompiler interface {
	Compile(in CompilerInputs) (*DecisionView, error)
}

// SetCompilerSources injects the dynamic quality/pricing input providers.
// Assembly-time only (before Start), like SetRuntimeHealth; wiring the real
// sources (quality sync, pricing snapshot) into these is Task18's job.
// Arming enables reload-triggered compiles.
func (s *Scheduler) SetCompilerSources(quality func() map[CandidateQualityKey]CandidateQualityInput, prices func() map[string]domain.ResolvedPrices) {
	s.qualityFn = quality
	s.pricesFn = prices
	s.compileArmed = true
}

// RequestCompile signals the compile lane (non-blocking; coalesced by the
// debounce window). No-op until sources are armed so legacy/unwired
// schedulers never publish compiled views.
func (s *Scheduler) RequestCompile() {
	if !s.compileArmed {
		return
	}
	select {
	case s.compileCh <- struct{}{}:
	default:
	}
}

func (s *Scheduler) compileLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.compileCh:
		}
		if !debounceWait(ctx, compileDebounce, s.compileCh) {
			return
		}
		s.compileOnce()
	}
}

// debounceWait is trailing-edge coalescing: waits the window, consuming any
// further triggers. Returns false on ctx cancellation.
func debounceWait(ctx context.Context, window time.Duration, ch <-chan struct{}) bool {
	t := time.NewTimer(window)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		case <-ch:
		}
	}
}

// compileOnce collects live inputs, compiles against exactly one static root,
// and publishes through the single routingPublisher. A staged root takes
// precedence over the published root; the compile itself runs outside
// publisher.mu. Publish pairs the compiled decision with the static root it
// was compiled from: a newer staging supersedes the result (dropped, with a
// fresh compile requested). Byte-identical decisions skip publish unless the
// static root identity changed. A compile error retains the exact old
// published pair and the pending root.
// Called from the serial compile lane (or synchronously in tests);
// lastDecisionBytes/lastCompiledStatic are owned by that single lane.
//
// v5-C1/C2: the lane drains fire-owned scope, unites it with lane-local
// quality/price diffs, and recompiles ONLY affected routes (pointer reuse,
// whole-snapshot atomic publish); unknown/unscoped causes take the
// full-fidelity fallback with recorded reason. exec/observe boundary: compile
// EXECUTES off-path; request observation only READS the published view.
func (s *Scheduler) compileOnce() {
	scopes, scopeOverflow := s.drainCompileScopes()
	s.publisher.mu.Lock()
	cur := s.view.Load()
	pendingSnap := s.publisher.pending
	target := pendingSnap
	if target == nil && cur != nil {
		target = cur.static
	}
	var baseGen uint64
	var baseStatic *StaticView
	var carry *DecisionView
	if cur != nil {
		baseGen = cur.generation
		baseStatic = cur.static
		carry = cur.decision
	}
	s.publisher.mu.Unlock()

	if target == nil {
		return
	}
	// v5-§5.1A: compiled-health-free — Health/Latched DELETED from inputs
	// (and from filterCandidates); serving gates live solely in reserveOnView.
	in := CompilerInputs{
		Static: target,
	}
	if s.qualityFn != nil {
		in.Quality = s.qualityFn()
	}
	if s.pricesFn != nil {
		in.Prices = s.pricesFn()
	}
	fire := &compileFire{input: in, carry: carry, scopes: scopes, overflow: scopeOverflow}
	wantFull, fullCause := s.resolveCompileScope(fire)
	var dv *DecisionView
	var err error
	if wantFull {
		dv, err = s.compiler.Compile(in)
		if err == nil {
			// Full-fidelity path: every route recomputed — the view is whole.
			if dv != nil {
				dv.whole = true
			}
			total := 0
			if target.routeIndex != nil {
				total = target.routeIndex.totalRoutes
			}
			s.recordCompileFallback(fullCause, len(fire.scopes), len(fire.affected), total)
		}
	} else {
		dv, err = s.compileScopedRoutes(fire)
		if err == errScopedUnsupported {
			dv, err = s.compiler.Compile(in)
			if err == nil {
				// Full-fidelity path: every route recomputed — the view is whole.
				if dv != nil {
					dv.whole = true
				}
				total := 0
				if target.routeIndex != nil {
					total = target.routeIndex.totalRoutes
				}
				s.recordCompileFallback(fallbackCompilerSeam, len(fire.scopes), len(fire.affected), total)
			}
		}
	}
	if err != nil {
		s.compileErrMs.Store(s.timeNow().UnixMilli())
		if s.log != nil {
			s.log.Warn("routing compile failed; retaining previous decision view", logx.Error(err))
		}
		return
	}
	s.compileOKMs.Store(s.timeNow().UnixMilli())
	b := decisionViewBytes(dv)

	s.publisher.mu.Lock()
	if s.publisher.pending != pendingSnap {
		// A newer staging superseded this result: the fire's drained scopes
		// are lane work that must not evaporate — return them before re-arm.
		s.publisher.mu.Unlock()
		s.requeueCompileScopes(fire.scopes, fire.overflow)
		s.RequestCompile()
		return
	}
	if bytes.Equal(s.lastDecisionBytes, b) && s.lastCompiledStatic == target {
		s.publisher.mu.Unlock()
		return
	}
	if pendingSnap != nil {
		published := s.publisher.publishPairLocked(target, dv)
		s.lastDecisionBytes = b
		s.lastCompiledStatic = published
		s.publisher.mu.Unlock()
		return
	}
	s.publisher.mu.Unlock()
	if s.publisher.publishWithBase(baseGen, baseStatic, func(*RoutingView) *DecisionView { return dv }) {
		s.lastDecisionBytes = b
		s.lastCompiledStatic = target
	}
}

// decisionViewBytes encodes the routes of a DecisionView into canonical
// deterministic bytes: routes sorted by full RouteRef identity, compiled
// lanes in published order with request-independent metadata, weights sorted
// by account ID. Used as the publish byte-equality guard.
func decisionViewBytes(d *DecisionView) []byte {
	if d == nil {
		return nil
	}
	refs := make([]RouteRef, 0, len(d.routes))
	for k := range d.routes {
		refs = append(refs, k)
	}
	sort.Slice(refs, func(i, j int) bool { return lessRouteRef(refs[i], refs[j]) })
	var buf bytes.Buffer
	writeUvarint(&buf, uint64(len(refs)))
	for _, ref := range refs {
		rd := d.routes[ref]
		writeVarint(&buf, ref.GroupID)
		writeStr(&buf, ref.Format)
		writeStr(&buf, ref.Model)
		writeStr(&buf, ref.OperationTag)
		writeStr(&buf, ref.RouteClassID)
		writeStr(&buf, rd.Format)
		writeStr(&buf, rd.RequestedModel)
		writeStr(&buf, rd.RouteClassID)
		writeStr(&buf, rd.CallerCategory)
		writeStr(&buf, rd.OperationTag)
		writeCompiled(&buf, rd.Primary)
		writeCompiled(&buf, rd.Degraded)
		writeCompiled(&buf, rd.Explore.Ordered)
		ids := make([]int64, 0, len(rd.Explore.Weights))
		for id := range rd.Explore.Weights {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		writeUvarint(&buf, uint64(len(ids)))
		for _, id := range ids {
			writeVarint(&buf, id)
			writeVarint(&buf, int64(rd.Explore.Weights[id]))
		}
		writeUvarint(&buf, uint64(len(rd.Explore.Cumulative)))
		for _, c := range rd.Explore.Cumulative {
			writeUvarint(&buf, c)
		}
		writeUvarint(&buf, rd.Explore.Total)
		writeCompiledIndices(&buf, rd.Explore.Ordered, rd.Explore.Fallback)
		writeCacheDomainPlan(&buf, rd)
	}
	return buf.Bytes()
}

func writeCacheDomainPlan(buf *bytes.Buffer, decision *RouteDecision) {
	writeUvarint(buf, uint64(len(decision.CacheDomainRing.Domains)))
	for _, domain := range decision.CacheDomainRing.Domains {
		writeStr(buf, domain)
	}
	writeUvarint(buf, uint64(len(decision.CacheDomainRing.Nodes)))
	for _, node := range decision.CacheDomainRing.Nodes {
		writeUvarint(buf, node.Hash)
		writeStr(buf, node.Domain)
	}
	writeUvarint(buf, uint64(len(decision.CacheDomainAccounts)))
	for _, account := range decision.CacheDomainAccounts {
		writeVarint(buf, account.AccountID)
		writeStr(buf, account.Domain)
	}
}

func writeUvarint(buf *bytes.Buffer, v uint64) {
	buf.Write(binary.AppendUvarint(nil, v))
}

func writeVarint(buf *bytes.Buffer, v int64) {
	buf.Write(binary.AppendVarint(nil, v))
}

func writeStr(buf *bytes.Buffer, s string) {
	writeUvarint(buf, uint64(len(s)))
	buf.WriteString(s)
}

func writeCompiled(buf *bytes.Buffer, cs []CompiledCandidate) {
	writeUvarint(buf, uint64(len(cs)))
	for _, c := range cs {
		writeCompiledCandidate(buf, c)
	}
}

func writeCompiledIndices(buf *bytes.Buffer, cs []CompiledCandidate, indexes []uint16) {
	writeUvarint(buf, uint64(len(indexes)))
	for _, idx := range indexes {
		if int(idx) < len(cs) {
			writeCompiledCandidate(buf, cs[idx])
		}
	}
}

func writeCompiledCandidate(buf *bytes.Buffer, c CompiledCandidate) {
	writeVarint(buf, c.AccountID)
	writeStr(buf, string(c.Lane))
	writeVarint(buf, c.TemplateID)
	writeStr(buf, c.BaseURL)
	writeStr(buf, c.Fingerprint)
	writeStr(buf, c.RequestedModel)
	writeStr(buf, c.MappedModel)
	writeStr(buf, string(c.MappingMode))
	writeStr(buf, c.Quality)
	writeStr(buf, c.QualityRaw)
	writeVarint(buf, c.LifecycleRevision)
}
