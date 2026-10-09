// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"bytes"
	"context"
	"encoding/binary"
	"sort"
	"time"
	"unsafe"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// compileDebounce is the trailing-edge coalescing window for compile triggers
// (plan: 200ms debounce, publish only when bytes change).
const compileDebounce = 200 * time.Millisecond

// routeCompiler is the compile-lane seam (production: RoutingCompiler;
// tests inject failure to prove old-view retention).
type routeCompiler interface {
	Compile(in CompilerInputs) (*DecisionView, error)
}

// CompilerSources 是编译道的双输入源（SetWindowedQualitySource /
// SetPricesSource 双回填已删，改为 Start 期结构注入）。两源一次性给齐
// （both-or-nothing）；nil 字段按缺席处理（compileOnce 跳过）；nil 整体 =
// 未装配（RequestCompile armed 门 no-op，fireOnMinuteAdvance 同理）。
type CompilerSources struct {
	Quality func(time.Time) WindowedQuality
	Prices  func() map[string]domain.ResolvedPrices
}

// RequestCompile signals the compile lane (non-blocking; coalesced by the
// debounce window). No-op until sources are armed (Start received non-nil)
// so unwired schedulers never publish compiled views.
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
// the lane drains fire-owned scope, unites it with lane-local
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
			s.markFullAndRecord(dv, fullCause, fire, target)
		}
	} else {
		dv, err = s.compileScopedRoutes(fire)
		if err == errScopedUnsupported {
			dv, err = s.compiler.Compile(in)
			tookFull = true
			if err == nil {
				s.markFullAndRecord(dv, fallbackCompilerSeam, fire, target)
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
	// Compare pass FIRST: stream the canonical encoding against the previous
	// bytes through a bounded sink (no output retained, exact; a first-diff
	// short-circuits to unequal). Materialize the final held slice only when
	// unequal.
	equal := s.decisionEnc.compareCanonical(dv, s.lastDecisionBytes)

	s.publisher.mu.Lock()
	if s.publisher.pending != pendingSnap {
		// A newer staging superseded this result: the fire's drained scopes
		// are lane work that must not evaporate — return them before re-arm.
		s.publisher.mu.Unlock()
		s.requeueCompileScopes(fire.scopes, fire.overflow)
		s.RequestCompile()
		return
	}
	if equal && s.lastCompiledStatic == target {
		// Byte-identical decision on the SAME static root: nothing to publish
		// and nothing to retain. (Equal bytes on a CHANGED root still publish
		// below — without re-encoding.)
		s.publisher.mu.Unlock()
		return
	}
	// Single held copy: the ONLY pass that materializes bytes — same immutable
	// dv, no recompile, no re-read of clock/quality. The fresh slice never
	// shares a backing array with any cross-fire buffer.
	var b []byte
	if !equal {
		b = s.decisionEnc.encode(dv)
	}
	if pendingSnap != nil {
		published := s.publisher.publishPairLocked(target, dv)
		if b != nil {
			s.lastDecisionBytes = b
		}
		s.lastCompiledStatic = published
		s.publisher.mu.Unlock()
		return
	}
	s.publisher.mu.Unlock()
	if s.publisher.publishWithBase(baseGen, baseStatic, func(*RoutingView) *DecisionView { return dv }) {
		if b != nil {
			s.lastDecisionBytes = b
		}
		s.lastCompiledStatic = target
	}
}

// markFullAndRecord finalizes a successful full-fidelity compile: it flags the
// decision view whole and records the fallback with its cause. Shared by the
// native full path (cause=fullCause) and the scoped-unsupported seam fallback
// (cause=fallbackCompilerSeam) so both exit points cannot drift.
func (s *Scheduler) markFullAndRecord(dv *DecisionView, cause string, fire *compileFire, target *StaticView) {
	if dv != nil {
		dv.whole = true
	}
	total := 0
	if target.routeIndex != nil {
		total = target.routeIndex.totalRoutes
	}
	s.recordCompileFallback(cause, len(fire.scopes), len(fire.affected), total)
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

// encBuf is the single canonical-encoding write target. In materialize mode it
// owns a bytes.Buffer whose bytes become the held slice; in compare mode it
// streams against `old` WITHOUT retaining any output, recording the first
// differing offset. ONE concrete type (not an interface) so the per-write
// `tmp[:n]` stack arrays never escape to the heap.
//
// Exactness: a first-byte difference short-circuits to unequal, which is
// indistinguishable from a full byte compare for the equality predicate — never
// a truncation / prefix / hash approximation. A shorter stream (remaining old
// suffix), a longer stream and an empty old all compare unequal.
type encBuf struct {
	buf     bytes.Buffer
	compare bool
	old     []byte
	n       int  // bytes matched so far (== offset of the next written byte)
	equal   bool // still equal so far (false once any byte differs)
}

func (b *encBuf) Write(p []byte) (int, error) {
	if !b.compare {
		return b.buf.Write(p)
	}
	if b.equal && len(p) > 0 {
		// Compare whole chunks with memcmp (bytes.Equal) instead of a byte
		// loop; a chunk that overruns the old suffix is unequal by length.
		if b.n+len(p) <= len(b.old) {
			if !bytes.Equal(b.old[b.n:b.n+len(p)], p) {
				b.equal = false
			} else {
				b.n += len(p)
			}
		} else {
			b.equal = false
		}
	}
	return len(p), nil
}

func (b *encBuf) WriteString(s string) (int, error) {
	if !b.compare {
		return b.buf.WriteString(s)
	}
	if b.equal && len(s) > 0 {
		if b.n+len(s) <= len(b.old) {
			if !bytes.Equal(b.old[b.n:b.n+len(s)], sbytes(s)) {
				b.equal = false
			} else {
				b.n += len(s)
			}
		} else {
			b.equal = false
		}
	}
	return len(s), nil
}

// sbytes returns a read-only byte view of s WITHOUT copying. Safe here: the
// view is immediately consumed by bytes.Equal and never retained or mutated.
func sbytes(s string) []byte {
	if len(s) == 0 {
		return nil
	}
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

// result reports whether the compared stream was byte-identical to old: every
// byte matched AND the total length matched.
func (b *encBuf) result() bool { return b.equal && b.n == len(b.old) }

// decisionEncoder 是编译道单所有者（compileOnce 串行调用）的编码 scratch：
// 仅保留 refs/ids 暂存跨 fire 复用（varint 零分配）。**不保留输出缓冲**——旧实现
// 的 `buf bytes.Buffer` 跨 fire 常驻（峰值可达数十 MB），T3 改为比较流式 sink +
// 成功后单份持有，故此处不再持有 any buffer。
type decisionEncoder struct {
	refs []RouteRef
	ids  []int64
}

// decisionViewBytes 编码为规范字节（测试与一次性调用面；每次新建编码器）。
func decisionViewBytes(d *DecisionView) []byte {
	var e decisionEncoder
	return e.encode(d)
}

// encode materializes the canonical bytes of d into a FRESH owned slice (the
// only copy held on publish success). Returned bytes never alias any cross-fire
// buffer. Used by the changed path and one-shot callers.
func (e *decisionEncoder) encode(d *DecisionView) []byte {
	if d == nil {
		return nil
	}
	var eb encBuf
	e.writeCanonical(&eb, d)
	return eb.buf.Bytes()
}

// compareCanonical streams d's canonical encoding against old through a bounded
// compare buffer (no output retained; exact; first-diff short-circuits) and
// reports byte-equality. The compare pass never materializes the output.
func (e *decisionEncoder) compareCanonical(d *DecisionView, old []byte) bool {
	eb := encBuf{compare: true, old: old, equal: true}
	e.writeCanonical(&eb, d)
	return eb.result()
}

// writeCanonical streams the canonical deterministic encoding of a DecisionView
// into sink: routes sorted by full RouteRef identity, compiled lanes in
// published order with request-independent metadata, weights sorted by account
// ID. Shared by the compare sink (equality guard) and the materializing encoder.
// Returned bytes alias sink (valid until the next encode) — retain via copy.
func (e *decisionEncoder) writeCanonical(sink *encBuf, d *DecisionView) {
	e.refs = e.refs[:0]
	for k := range d.routes {
		e.refs = append(e.refs, k)
	}
	sort.Slice(e.refs, func(i, j int) bool { return lessRouteRef(e.refs[i], e.refs[j]) })
	writeUvarint(sink, uint64(len(e.refs)))
	for _, ref := range e.refs {
		rd := d.routes[ref]
		writeVarint(sink, ref.GroupID)
		writeStr(sink, ref.Format)
		writeStr(sink, ref.Model)
		writeStr(sink, ref.OperationTag)
		writeStr(sink, ref.RouteClassID)
		writeStr(sink, rd.Format)
		writeStr(sink, rd.RequestedModel)
		writeStr(sink, rd.RouteClassID)
		writeStr(sink, rd.CallerCategory)
		writeStr(sink, rd.OperationTag)
		writeCompiled(sink, rd.Primary)
		writeCompiled(sink, rd.Degraded)
		writeCompiled(sink, rd.Explore.Ordered)
		e.ids = e.ids[:0]
		for id := range rd.Explore.Weights {
			e.ids = append(e.ids, id)
		}
		sort.Slice(e.ids, func(i, j int) bool { return e.ids[i] < e.ids[j] })
		writeUvarint(sink, uint64(len(e.ids)))
		for _, id := range e.ids {
			writeVarint(sink, id)
			writeVarint(sink, int64(rd.Explore.Weights[id]))
		}
		writeUvarint(sink, uint64(len(rd.Explore.Cumulative)))
		for _, c := range rd.Explore.Cumulative {
			writeUvarint(sink, c)
		}
		writeUvarint(sink, rd.Explore.Total)
		writeVarint(sink, int64(rd.Explore.ExploreBP))
		writeCompiledIndices(sink, rd.Explore.Ordered, rd.Explore.Fallback)
		writeCacheDomainPlan(sink, rd)
		writeIncident(sink, rd.Incident)
	}
}

// writeIncident encodes the expose-only mark in fixed field order (sorted
// routes + fixed order = deterministic flips).
func writeIncident(buf *encBuf, inc RouteIncident) {
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

func writeCacheDomainPlan(buf *encBuf, decision *RouteDecision) {
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
func writeUvarint(buf *encBuf, v uint64) {
	var tmp [binary.MaxVarintLen64]byte
	buf.Write(tmp[:binary.PutUvarint(tmp[:], v)])
}

func writeVarint(buf *encBuf, v int64) {
	var tmp [binary.MaxVarintLen64]byte
	buf.Write(tmp[:binary.PutVarint(tmp[:], v)])
}

func writeStr(buf *encBuf, s string) {
	writeUvarint(buf, uint64(len(s)))
	buf.WriteString(s)
}

func writeCompiled(buf *encBuf, cs []CompiledCandidate) {
	writeUvarint(buf, uint64(len(cs)))
	for _, c := range cs {
		writeCompiledCandidate(buf, c)
	}
}

func writeCompiledIndices(buf *encBuf, cs []CompiledCandidate, indexes []uint16) {
	writeUvarint(buf, uint64(len(indexes)))
	for _, idx := range indexes {
		if int(idx) < len(cs) {
			writeCompiledCandidate(buf, cs[idx])
		}
	}
}

func writeCompiledCandidate(buf *encBuf, c CompiledCandidate) {
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
