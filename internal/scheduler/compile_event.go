// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"errors"
	"maps"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// Event-driven incremental compilation (v5): the single clean mechanism
// REPLACING the unconditional 30s full rebuild outright (no dual-track, no
// flags). Invalidation/price/quality events enqueue SCOPED work; the 30s tick
// is demoted to an O(1)-probe staleness backstop; scoped fires recompute only
// affected routes with pointer reuse while publish stays whole-snapshot
// atomic. Every new structure below declares ONE owner + lifecycle.

// ---- scoped enqueue seam (freeze-safe naming) ----

// scopedCompileReq is one affected-scope work item: the groups/accounts an
// event names plus its cause. Owner: compile lane. Lifecycle: fire-owned (one
// consume per fire, dies at return).
type scopedCompileReq struct {
	groups   []int64
	accounts []int64
	cause    string
}

// Scope causes. "full-stage" marks a full staging (reload/InvalidateAll) whose
// changes no group scope covers — the lane takes the full-fidelity fallback.
const (
	scopeCauseFullStage = "full-stage"
	scopeCauseGroup     = "invalidate-group"
)

// Fallback record causes (unknown/unscoped → FULL recompile + recorded reason).
const (
	fallbackFullStage     = "full-stage"
	fallbackUnscoped      = "unscoped"
	fallbackScopeOverflow = "scope-overflow"
	fallbackCompilerSeam  = "compiler-seam"
	fallbackNoCarry       = "no-carry"
)

// compileFireMode is how one fire proceeds. Single owner: resolveCompileScope.
type compileFireMode uint8

const (
	// fireScoped recomputes ONLY affected routes (pointer reuse, whole-snapshot
	// atomic publish downstream).
	fireScoped compileFireMode = iota
	// fireFull is the full-fidelity fallback: every route recomputed, reason
	// recorded via recordCompileFallback.
	fireFull
	// fireSkip is a no-work fire: the fire's entire input (static root +
	// quality/baseline/price maps) is identical to the last successful
	// compile's, so recompiling provably reproduces the published bytes (the
	// byte-guard at the publish seam proves this on every fire already).
	// Canonical source: the M-advance recheck (scheduler.fireOnMinuteAdvance)
	// — it must still wake the lane so the diffs can catch newly settled
	// minutes, but with zero drift the compile itself is pure waste (13MB
	// alloc per rebuild measured, once per minute on an idle gateway).
	fireSkip
)

// scopeChCap bounds burst scope backlog; overflow degrades to the exact full
// fallback (never a miss). Producers never block either way.
const scopeChCap = 16

// enqueueCompileScope records affected scope and wakes the compile lane via
// the unchanged RequestCompile signal. Non-blocking: on a full channel the
// scopeOverflow flag forces the exact full fallback. request/tick boundary:
// the tick/event side never touches request state and no producer blocks on
// the compile lane.
func (s *Scheduler) enqueueCompileScope(groups []int64, accounts []int64, cause string) {
	select {
	case s.scopeCh <- scopedCompileReq{groups: groups, accounts: accounts, cause: cause}:
	default:
		s.scopeOverflow.Store(true)
	}
}

// requeueCompileScopes returns a superseded fire's drained scopes to scopeCh:
// scopes are fire-owned work units that cannot evaporate with the dropped
// result. Same non-blocking select/default + overflow-flag discipline as
// enqueueCompileScope (no new channels); a preserved overflow flag is
// restored. Serial compile lane only. Owner: compile lane. Lifecycle:
// fire-owned (moves scopes from the dead fire back to the lane).
func (s *Scheduler) requeueCompileScopes(scopes []scopedCompileReq, overflow bool) {
	for _, sc := range scopes {
		s.enqueueCompileScope(sc.groups, sc.accounts, sc.cause)
	}
	if overflow {
		s.scopeOverflow.Store(true)
	}
}

// drainCompileScopes collects every pending scope for one fire (fire-owned)
// and consumes the overflow flag. Called once per compileOnce, never per
// request: no per-request work linear in any cardinality.
func (s *Scheduler) drainCompileScopes() ([]scopedCompileReq, bool) {
	var out []scopedCompileReq
	for {
		select {
		case sc := <-s.scopeCh:
			out = append(out, sc)
		default:
			return out, s.scopeOverflow.CompareAndSwap(true, false)
		}
	}
}

// ---- lane-local dynamic-input diffs (closed lanes stay read-only) ----

// diffRouteInputs unites added/deleted/changed input keys into affected
// routes via the target index; callers vary only in equality and the
// key→refs mapping (quality: hex→single route; prices: model→routes).
func diffRouteInputs[K comparable, V any](fresh, last map[K]V, equal func(a, b V) bool, refs func(k K, idx *compileRouteIndex) []RouteRef, idx *compileRouteIndex) map[RouteRef]struct{} {
	out := make(map[RouteRef]struct{})
	if idx == nil {
		return out
	}
	for k, v := range fresh {
		if lv, ok := last[k]; ok && equal(lv, v) {
			continue
		}
		for _, ref := range refs(k, idx) {
			out[ref] = struct{}{}
		}
	}
	for k := range last {
		if _, ok := fresh[k]; !ok {
			for _, ref := range refs(k, idx) {
				out[ref] = struct{}{}
			}
		}
	}
	return out
}

// refsForQualityKey maps a (route, fingerprint) quality key to its affected
// route via the target index (hex→single route). Shared by the current and
// baseline diffs.
func refsForQualityKey(k CandidateQualityKey, idx *compileRouteIndex) []RouteRef {
	if ref, ok := idx.rcRoutes[domain.RouteClassIDHex(k.RouteClassID)]; ok {
		return []RouteRef{ref}
	}
	return nil
}

// diffCompilerQuality maps fresh-vs-last quality keys to affected routes via
// the target index. The lane pulls the full maps each fire (as before) and
// diffs against the snapshot it already holds; quality-sync/pricing producers
// expose no hooks and stay read-only.
func diffCompilerQuality(fresh, last map[CandidateQualityKey]CandidateQualityInput, idx *compileRouteIndex) map[RouteRef]struct{} {
	return diffRouteInputs(fresh, last,
		func(a, b CandidateQualityInput) bool { return a == b },
		refsForQualityKey, idx)
}

// diffCompilerBaseline maps fresh-vs-last incident baseline keys to affected
// routes. Counts hold floats but never NaN (provider conversions are finite
// and fail-closed), so == is an exact change detector like the quality diff.
func diffCompilerBaseline(fresh, last map[CandidateQualityKey]Counts, idx *compileRouteIndex) map[RouteRef]struct{} {
	return diffRouteInputs(fresh, last,
		func(a, b Counts) bool { return a == b },
		refsForQualityKey, idx)
}

// diffCompilerPrices maps fresh-vs-last per-model prices to affected routes.
// Pointers are dereferenced: every pull births fresh pointers, so == would
// report phantom changes on every fire.
func diffCompilerPrices(fresh, last map[string]domain.ResolvedPrices, idx *compileRouteIndex) map[RouteRef]struct{} {
	return diffRouteInputs(fresh, last, EqualResolvedPrices,
		func(model string, idx *compileRouteIndex) []RouteRef { return idx.modelRoutes[model] }, idx)
}

func int64PtrEqual(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// EqualResolvedPrices compares resolved prices by value across every price
// field the compiler consumes (billing.CostFromResolved inputs + routing).
// Single owner of the compiler price surface: service reuses this for its
// reload change gate instead of a hand-rolled counterpart.
func EqualResolvedPrices(a, b domain.ResolvedPrices) bool {
	if a.Mode != b.Mode {
		return false
	}
	if !int64PtrEqual(a.InputPerM, b.InputPerM) ||
		!int64PtrEqual(a.OutputPerM, b.OutputPerM) ||
		!int64PtrEqual(a.CacheReadPerM, b.CacheReadPerM) ||
		!int64PtrEqual(a.CacheWritePerM, b.CacheWritePerM) ||
		!int64PtrEqual(a.PricePerCall, b.PricePerCall) ||
		!int64PtrEqual(a.ImgInTokPerM, b.ImgInTokPerM) ||
		!int64PtrEqual(a.ImgOutTokPerM, b.ImgOutTokPerM) ||
		!int64PtrEqual(a.PricePerImage, b.PricePerImage) {
		return false
	}
	if (a.VariantSeq == nil) != (b.VariantSeq == nil) {
		return false
	}
	if a.VariantSeq != nil && *a.VariantSeq != *b.VariantSeq {
		return false
	}
	if (a.Provider == nil) != (b.Provider == nil) {
		return false
	}
	if a.Provider != nil && *a.Provider != *b.Provider {
		return false
	}
	return true
}

// ---- scope resolution + affected-only recompute ----

// compileFire is one lane fire's scope-resolution state: the target static
// root, the published carry-forward, drained channel scopes, lane-local
// inputs, and the resolved affected set. Owner: compile lane. Lifecycle:
// single-fire (born in compileOnce, dies at return).
type compileFire struct {
	input    CompilerInputs // Static is the target root
	carry    *DecisionView
	scopes   []scopedCompileReq
	overflow bool
	affected map[RouteRef]struct{}
}

// indexRoutesForAccount unions the routes of every group a leaf belongs to
// (multi-group accounts fan out through their full groupIDs set).
func indexRoutesForAccount(idx *compileRouteIndex, target *StaticView, aid int64, out map[RouteRef]struct{}) {
	snap, ok := target.byID[aid]
	if !ok || snap == nil {
		return
	}
	st := snap.static.Load()
	if st == nil {
		return
	}
	for _, gid := range st.groupIDs {
		for _, ref := range idx.groupRoutes[gid] {
			out[ref] = struct{}{}
		}
	}
}

// fallbackReason records why a fire took the full-fidelity path (counter +
// warn fields: cause, scope-size, route-count). Observed metric, never silent.
type fallbackReason struct {
	cause          string
	scopeEntries   int
	affectedRoutes int
	totalRoutes    int
}

// recordCompileFallback counts every full recompile with its reason; Warn on
// unexpected causes only (routine full stages stay quiet).
func (s *Scheduler) recordCompileFallback(cause string, scopeEntries, affectedRoutes, totalRoutes int) {
	s.fallbackCount.Add(1)
	s.lastFallback.Store(&fallbackReason{cause: cause, scopeEntries: scopeEntries, affectedRoutes: affectedRoutes, totalRoutes: totalRoutes})
	switch cause {
	case fallbackFullStage:
		return
	default:
		if s.log != nil {
			s.log.Warn("compile full-fidelity fallback",
				logx.String("cause", cause),
				logx.Int("scope_entries", scopeEntries),
				logx.Int("affected_routes", affectedRoutes),
				logx.Int("total_routes", totalRoutes))
		}
	}
}

// resolveCompileScope unites channel scopes with lane-local quality/price
// diffs into the fire's affected route set. Returns the fire mode
// (scoped/full/skip) plus the recorded fallback cause when full.
//
// Ordering is load-bearing: overflow and full-stage marks outrank everything;
// the compiler-seam and missing-carry guards follow, and ONLY THEN the no-work
// test. Seam/carry must precede the skip — a compiler without the scoped seam
// (test doubles and any non-scoped implementation) or a lane without a
// published carry has an exact-full contract that skipping would silently turn
// into a no-op.
//
// Positive trigger rule (v5-§5.1A): static + price + quality ONLY — health/
// latch deltas never enter the index and never enqueue work.
func (s *Scheduler) resolveCompileScope(f *compileFire) (compileFireMode, string) {
	target := f.input.Static
	idx := target.routeIndex
	// Lane-local diffs first: baselines track the last PULLED inputs (shallow
	// copies — producers hand fresh maps per pull), so a fire that wakes for
	// any reason also picks up dynamic drift in the same pass. They advance on
	// every fire — including a skip — so the next diff stays exact.
	qRefs := diffCompilerQuality(f.input.Quality, s.lastQuality, idx)
	bRefs := diffCompilerBaseline(f.input.Baseline, s.lastBaseline, idx)
	pRefs := diffCompilerPrices(f.input.Prices, s.lastPrices, idx)
	s.lastQuality = maps.Clone(f.input.Quality)
	s.lastBaseline = maps.Clone(f.input.Baseline)
	s.lastPrices = maps.Clone(f.input.Prices)

	affected := make(map[RouteRef]struct{}, len(qRefs)+len(bRefs)+len(pRefs)+len(f.scopes))
	for ref := range qRefs {
		affected[ref] = struct{}{}
	}
	for ref := range bRefs {
		affected[ref] = struct{}{}
	}
	for ref := range pRefs {
		affected[ref] = struct{}{}
	}
	hasFullMark := false
	if idx != nil {
		for _, sc := range f.scopes {
			if sc.cause == scopeCauseFullStage {
				hasFullMark = true
				continue
			}
			for _, gid := range sc.groups {
				for _, ref := range idx.groupRoutes[gid] {
					affected[ref] = struct{}{}
				}
			}
			for _, aid := range sc.accounts {
				indexRoutesForAccount(idx, target, aid, affected)
			}
		}
	}
	f.affected = affected
	switch {
	case f.overflow:
		return fireFull, fallbackScopeOverflow
	case hasFullMark || idx == nil:
		return fireFull, fallbackFullStage
	}
	if _, ok := s.compiler.(scopedRouteCompiler); !ok {
		return fireFull, fallbackCompilerSeam
	}
	if f.carry == nil {
		return fireFull, fallbackNoCarry
	}
	if len(affected) == 0 {
		// Pure recheck: same static root as the last successful compile and
		// zero dynamic drift — no route can change, so the compile (13MB alloc
		// measured) is pure waste. Every OTHER unscoped wake carries work the
		// diffs cannot see (a newly staged root — target != lastCompiledStatic
		// via reload/InvalidateGroup staging — or a bare re-arm) and keeps the
		// exact full-fidelity fallback.
		if target == s.lastCompiledStatic {
			return fireSkip, ""
		}
		return fireFull, fallbackUnscoped
	}
	return fireScoped, ""
}

// scopedRouteCompiler is the per-route seam for affected-routes-only
// recompute. Only *RoutingCompiler implements it; test doubles take the
// full-fidelity fallback.
type scopedRouteCompiler interface {
	compileSingleRoute(in CompilerInputs, gid int64, rk routeKey, op domain.OperationTag) (*RouteDecision, error)
}

// errScopedUnsupported is the defensive seam failure inside
// compileScopedRoutes (resolveCompileScope checks the seam first, so this is
// unreachable in the lane — a full recompile stays exact, never partial).
var errScopedUnsupported = errors.New("scheduler: scoped compiler seam unsupported")

// compileScopedRoutes recomputes ONLY affected routes against the target
// static root and carries every unaffected published decision forward VERBATIM
// (pointer reuse — survivors keep identity). Publish stays whole-snapshot
// atomic downstream (one generation, matched pair). transition/reconciliation
// boundary: the fence + publish guard decide transitions — no reconciliation
// pass, no health/latch read, no request-state touch inside recompute.
// exact/estimate boundary: every carried or recomputed byte is exact (the
// full-fidelity fallback recomputes fully, never estimates). Facts discipline:
// the lane reads the lane-internal target maps and the published decision map
// directly into a FRESH merged map — never the public validated-clearing
// clones, never in-place facts mutation (replace the route entry only).
func (s *Scheduler) compileScopedRoutes(f *compileFire) (*DecisionView, error) {
	src, ok := s.compiler.(scopedRouteCompiler)
	if !ok {
		return nil, errScopedUnsupported
	}
	merged := make(map[RouteRef]*RouteDecision, len(f.carry.routes)+len(f.affected))
	for k, v := range f.carry.routes {
		merged[k] = v
	}
	for ref := range f.affected {
		rk := routeKey{format: domain.RequestFormat(ref.Format), model: ref.Model}
		op := domain.OperationTag(ref.OperationTag)
		dec, err := src.compileSingleRoute(f.input, ref.GroupID, rk, op)
		if err != nil {
			return nil, err
		}
		if dec == nil {
			// Route gone from the target root: drop it so scoped output
			// cannot diverge from the full-recompile oracle.
			delete(merged, ref)
			continue
		}
		merged[ref] = dec
	}
	// Conservative default: a scoped-carry publish marks partial (whole=false)
	// even when the fire recomputed every route — only the full path marks
	// whole (v5 §5.2 wholeness bit).
	return &DecisionView{routes: merged, whole: false}, nil
}
