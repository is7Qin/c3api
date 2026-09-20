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

// CompilerSources 是编译道的双输入源（W3-T1：SetWindowedQualitySource /
// SetPricesSource 双回填已删，改为 Start 期结构注入）。两源一次性给齐
// （both-or-nothing）；nil 字段按缺席处理（compileOnce 跳过）；nil 整体 =
// 未装配（RequestCompile armed 门 no-op，fireOnMinuteAdvance 同理）。
type CompilerSources struct {
	Quality func(time.Time) WindowedQuality
	Prices  func() map[string]domain.ResolvedPrices
}

// RequestCompile signals the compile lane (non-blocking; coalesced by the
// debounce window). No-op until sources are armed (Start received non-nil)
// so legacy/unwired schedulers never publish compiled views.
func (s *Scheduler) RequestCompile() {
	if s.sources == nil {
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
// full-fidelity fallback with recorded reason; a fire whose inputs are
// identical to the last successful compile (same static root, zero drift)
// takes fireSkip and compiles nothing at all. exec/observe boundary: compile
// EXECUTES off-path; request observation only READS the published view.
func (s *Scheduler) compileOnce() {
	scopes, scopeOverflow := s.drainCompileScopes()
	// now is read ONCE per fire (single-M discipline): the fire-minute stamp
	// below and the windowed provider share it, so a boundary straddle can
	// never split one fire across two minutes.
	fireNow := s.timeNow()
	s.lastFireMinute.Store(fireNow.UTC().Truncate(time.Minute).Unix())
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
	// now is the fire's single clock read above, threaded through the
	// windowed provider (single-M discipline: settled reads, live filter,
	// incident minute).
	now := fireNow
	in := CompilerInputs{
		Static: target,
	}
	src := s.sources
	if src != nil && src.Quality != nil {
		wq := src.Quality(now)
		in.Quality = wq.Current
		in.Baseline = wq.Baseline
		in.EvaluatedMinute = wq.SettledBoundary.Unix()
	}
	in.IncidentEval = s.incidentEvaluator()
	if src != nil && src.Prices != nil {
		in.Prices = src.Prices()
	}
	fire := &compileFire{input: in, carry: carry, scopes: scopes, overflow: scopeOverflow}
	mode, fullCause := s.resolveCompileScope(fire)
	if mode == fireSkip {
		// No-op recheck (the M-advance wake with zero drift): inputs are
		// identical to the last successful compile, so the compile is elided.
		// Not a fallback and not a success: no decision is produced, so the
		// compile-ok / incident-eval stamps stay untouched.
		s.skipCount.Add(1)
		if s.log != nil && s.log.DebugEnabled() {
			total := 0
			if target.routeIndex != nil {
				total = target.routeIndex.totalRoutes
			}
			s.log.Debug("compile skipped: inputs identical to last successful compile",
				logx.Int("total_routes", total))
		}
		return
	}
	tookFull := mode == fireFull
	var dv *DecisionView
	var err error
	if mode == fireFull {
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
			tookFull = true
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
	// Incident lane bookkeeping: full compiles prune routes gone from static
	// facts; every successful fire refreshes the expose-only counters.
	if tookFull && dv != nil {
		s.incidents.pruneAlive(dv.routes)
	}
	s.incidentActive.Store(int64(s.incidents.activeCount()))
	s.incidentEvalMs.Store(s.timeNow().UnixMilli())
	b := s.decisionEnc.encode(dv)

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
		s.lastDecisionBytes = append(s.lastDecisionBytes[:0], b...)
		s.lastCompiledStatic = published
		s.publisher.mu.Unlock()
		return
	}
	s.publisher.mu.Unlock()
	if s.publisher.publishWithBase(baseGen, baseStatic, func(*RoutingView) *DecisionView { return dv }) {
		s.lastDecisionBytes = append(s.lastDecisionBytes[:0], b...)
		s.lastCompiledStatic = target
	}
}

// incidentEvaluator builds the lane's per-fire incident closure over the
// serial-lane-owned tracker. Full and scoped fires share it, so scoped output
// equals the full-recompile oracle bit-for-bit by construction (frozen
// evaluations reuse stored state without burning streaks).
func (s *Scheduler) incidentEvaluator() IncidentEvalFunc {
	return func(route RouteRef, vote incidentVote, minute int64) RouteIncident {
		return s.incidents.evaluate(route, vote, minute)
	}
}

// decisionEncoder 是编译道单所有者（compileOnce 串行调用）的复用编码器：
// refs/ids 暂存与输出缓冲跨 fire 复用，varint 零分配——把旧路径的
// "每 fire 新缓冲增长（可达数十 MB 容量）+ 每整数一次小分配"压成稳态零分配。
type decisionEncoder struct {
	buf  bytes.Buffer
	refs []RouteRef
	ids  []int64
}

// decisionViewBytes 编码为规范字节（测试与一次性调用面；每次新建编码器）。
// 编译道热路径用 decisionEncoder.encode 复用缓冲与暂存。
func decisionViewBytes(d *DecisionView) []byte {
	var e decisionEncoder
	return e.encode(d)
}

// encode encodes the routes of a DecisionView into canonical deterministic
// bytes: routes sorted by full RouteRef identity, compiled lanes in published
// order with request-independent metadata, weights sorted by account ID. Used
// as the publish byte-equality guard. Returned bytes alias the encoder buffer
// (valid until the next encode) — retain via copy.
func (e *decisionEncoder) encode(d *DecisionView) []byte {
	if d == nil {
		return nil
	}
	e.refs = e.refs[:0]
	for k := range d.routes {
		e.refs = append(e.refs, k)
	}
	sort.Slice(e.refs, func(i, j int) bool { return lessRouteRef(e.refs[i], e.refs[j]) })
	e.buf.Reset()
	writeUvarint(&e.buf, uint64(len(e.refs)))
	for _, ref := range e.refs {
		rd := d.routes[ref]
		writeVarint(&e.buf, ref.GroupID)
		writeStr(&e.buf, ref.Format)
		writeStr(&e.buf, ref.Model)
		writeStr(&e.buf, ref.OperationTag)
		writeStr(&e.buf, ref.RouteClassID)
		writeStr(&e.buf, rd.Format)
		writeStr(&e.buf, rd.RequestedModel)
		writeStr(&e.buf, rd.RouteClassID)
		writeStr(&e.buf, rd.CallerCategory)
		writeStr(&e.buf, rd.OperationTag)
		writeCompiled(&e.buf, rd.Primary)
		writeCompiled(&e.buf, rd.Degraded)
		writeCompiled(&e.buf, rd.Explore.Ordered)
		e.ids = e.ids[:0]
		for id := range rd.Explore.Weights {
			e.ids = append(e.ids, id)
		}
		sort.Slice(e.ids, func(i, j int) bool { return e.ids[i] < e.ids[j] })
		writeUvarint(&e.buf, uint64(len(e.ids)))
		for _, id := range e.ids {
			writeVarint(&e.buf, id)
			writeVarint(&e.buf, int64(rd.Explore.Weights[id]))
		}
		writeUvarint(&e.buf, uint64(len(rd.Explore.Cumulative)))
		for _, c := range rd.Explore.Cumulative {
			writeUvarint(&e.buf, c)
		}
		writeUvarint(&e.buf, rd.Explore.Total)
		writeVarint(&e.buf, int64(rd.Explore.ExploreBP))
		writeCompiledIndices(&e.buf, rd.Explore.Ordered, rd.Explore.Fallback)
		writeCacheDomainPlan(&e.buf, rd)
		writeIncident(&e.buf, rd.Incident)
	}
	return e.buf.Bytes()
}

// writeIncident encodes the expose-only mark in fixed field order (sorted
// routes + fixed order = deterministic flips).
func writeIncident(buf *bytes.Buffer, inc RouteIncident) {
	if inc.Active {
		writeUvarint(buf, 1)
	} else {
		writeUvarint(buf, 0)
	}
	writeStr(buf, inc.Kind)
	writeUvarint(buf, uint64(inc.Comparable))
	writeUvarint(buf, uint64(inc.Degraded))
	writeUvarint(buf, uint64(inc.Domains))
	writeVarint(buf, inc.EvaluatedMinute)
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

// writeUvarint 把 uvarint 写进缓冲：栈上数组编码后一次 Write——旧实现
// binary.AppendUvarint(nil, v) 每次分配一个小切片（视图级编码实测 27 万
// allocs/op 的主源）。
func writeUvarint(buf *bytes.Buffer, v uint64) {
	var tmp [binary.MaxVarintLen64]byte
	buf.Write(tmp[:binary.PutUvarint(tmp[:], v)])
}

func writeVarint(buf *bytes.Buffer, v int64) {
	var tmp [binary.MaxVarintLen64]byte
	buf.Write(tmp[:binary.PutVarint(tmp[:], v)])
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
	writeVarint(buf, c.IdentityRevision)
}
