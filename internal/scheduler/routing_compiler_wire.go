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

// compileOnce collects live inputs, compiles, and publishes through the
// single routingPublisher only when the canonical bytes changed. A compile
// error retains the current view untouched (no publish, no byte update).
// Called from the serial compile lane (or synchronously in tests);
// lastDecisionBytes is owned by that single lane.
func (s *Scheduler) compileOnce() {
	cur := s.view.Load()
	if cur == nil || cur.static == nil {
		return
	}
	in := CompilerInputs{
		Static:  cur.static,
		Health:  s.compilerHealthSnapshot(),
		Latched: s.latch.Snapshot(),
	}
	if s.qualityFn != nil {
		in.Quality = s.qualityFn()
	}
	if s.pricesFn != nil {
		in.Prices = s.pricesFn()
	}
	dv, err := s.compiler.Compile(in)
	if err != nil {
		s.compileErrMs.Store(s.timeNow().UnixMilli())
		if s.log != nil {
			s.log.Warn("routing compile failed; retaining previous decision view", logx.Error(err))
		}
		return
	}
	s.compileOKMs.Store(s.timeNow().UnixMilli())
	b := decisionViewBytes(dv)
	if bytes.Equal(s.lastDecisionBytes, b) {
		return
	}
	s.publisher.publishWithBase(cur.generation, func(*RoutingView) *DecisionView { return dv })
	s.lastDecisionBytes = b
}

// compilerHealthSnapshot converts the live RuntimeHealth view into compiler
// input. The view is already the pruned authoritative set (sync loop owns
// expiry), so entries pass through as-is.
func (s *Scheduler) compilerHealthSnapshot() map[HealthKey]HealthState {
	if s.health == nil {
		return nil
	}
	v := s.health.View()
	if len(v) == 0 {
		return nil
	}
	out := make(map[HealthKey]HealthState, len(v))
	for k, e := range v {
		out[k] = e.State
	}
	return out
}

// decisionViewBytes encodes the routes of a DecisionView into canonical
// deterministic bytes: routes sorted by full RouteRef identity, lane IDs in
// published order, weights sorted by account ID. Used as the publish
// byte-equality guard; generation is excluded (publish order, not content).
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
		writeVarint(&buf, ref.GroupID)
		writeStr(&buf, ref.Format)
		writeStr(&buf, ref.Model)
		writeStr(&buf, ref.OperationTag)
		writeStr(&buf, ref.RouteClassID)
		writeIDs(&buf, d.routes[ref].Primary)
		writeIDs(&buf, d.routes[ref].Degraded)
		writeIDs(&buf, d.routes[ref].Explore.IDs)
		ids := make([]int64, 0, len(d.routes[ref].Explore.Weights))
		for id := range d.routes[ref].Explore.Weights {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		writeUvarint(&buf, uint64(len(ids)))
		for _, id := range ids {
			writeVarint(&buf, id)
			writeVarint(&buf, int64(d.routes[ref].Explore.Weights[id]))
		}
		writeUvarint(&buf, uint64(len(d.routes[ref].Explore.Cumulative)))
		for _, c := range d.routes[ref].Explore.Cumulative {
			writeUvarint(&buf, c)
		}
		writeUvarint(&buf, d.routes[ref].Explore.Total)
		writeIDs(&buf, d.routes[ref].Explore.Fallback)
		writeCacheDomainPlan(&buf, d.routes[ref])
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

func writeIDs(buf *bytes.Buffer, ids []int64) {
	writeUvarint(buf, uint64(len(ids)))
	for _, id := range ids {
		writeVarint(buf, id)
	}
}
