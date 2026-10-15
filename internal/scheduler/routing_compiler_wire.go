// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"slices"
	"time"

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
// lastDigest/lastDigestSet/lastCompiledStatic and the digester are owned by that
// single lane.
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
	digest := s.digester.digest(dv)

	s.publisher.mu.Lock()
	if s.publisher.pending != pendingSnap {
		// A newer staging superseded this result: the fire's drained scopes
		// are lane work that must not evaporate — return them before re-arm.
		s.publisher.mu.Unlock()
		s.requeueCompileScopes(fire.scopes, fire.overflow)
		s.RequestCompile()
		return
	}
	if s.lastDigestSet && s.lastDigest == digest && s.lastCompiledStatic == target {
		s.publisher.mu.Unlock()
		return
	}
	if pendingSnap != nil {
		published := s.publisher.publishPairLocked(target, dv)
		s.lastDigest = digest
		s.lastDigestSet = true
		s.lastCompiledStatic = published
		s.publisher.mu.Unlock()
		return
	}
	s.publisher.mu.Unlock()
	if s.publisher.publishWithBase(baseGen, baseStatic, func(*RoutingView) *DecisionView { return dv }) {
		s.lastDigest = digest
		s.lastDigestSet = true
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

// digestScratchSize bounds the canonical-encoding scratch: the streaming
// SHA-256 sink never holds more than this many unhashed bytes, so no buffer
// grows with the canonical byte size (unlike the removed reusable encoder
// buffer + lastDecisionBytes copy).
const digestScratchSize = 32 * 1024

// decisionDigest is the publish-guard identity: the SHA-256 of the canonical
// stream plus the byte count and the nil marker (a nil view and a non-nil empty
// route map stay distinguishable even though both are cheap to emit). Digest
// equality with equal count/marker replaces the strict bytes.Equal guard; it
// accepts the theoretical cryptographic collision probability by design.
type decisionDigest struct {
	sum   [32]byte
	count int64
	isNil bool
}

// canonicalSink writes the canonical DecisionView byte stream. Production mode
// streams into a SHA-256 through the fixed digestScratchSize buffer; collect
// mode (test-only materialization via decisionViewBytes) accumulates bytes
// instead. One concrete sink keeps the traversal free of per-field interface
// dispatch and of unsafe string conversions.
type canonicalSink struct {
	scratch [digestScratchSize]byte
	n       int
	written int // flushed bytes; count includes the current scratch contents
	h       hash.Hash
	collect bool
	out     []byte
	// refs / ids sort scratch grow with route / max-candidate count, never with
	// the total encoding size, and are reused across fires.
	refs []RouteRef
	ids  []int64
}

func (s *canonicalSink) resetDigest() {
	s.n = 0
	s.written = 0
	if s.h == nil {
		s.h = sha256.New()
	} else {
		s.h.Reset()
	}
}

func (s *canonicalSink) resetCollect() {
	s.n = 0
	s.written = 0
	s.out = s.out[:0]
}

func (s *canonicalSink) flush() {
	if s.n == 0 {
		return
	}
	if s.collect {
		s.out = append(s.out, s.scratch[:s.n]...)
	} else {
		_, _ = s.h.Write(s.scratch[:s.n])
	}
	s.written += s.n
	s.n = 0
}

func (s *canonicalSink) writeBytes(b []byte) {
	if len(b) <= len(s.scratch)-s.n {
		s.n += copy(s.scratch[s.n:], b)
		return
	}
	for len(b) > 0 {
		if s.n == len(s.scratch) {
			s.flush()
		}
		k := copy(s.scratch[s.n:], b)
		s.n += k
		b = b[k:]
	}
}

// writeString emits the canonical string form: length-prefixed bytes.
func (s *canonicalSink) writeString(str string) {
	// Most fields have a one-byte length. Reserve prefix and payload together
	// so their common path needs only one capacity check and no scalar call.
	n := s.n
	if len(str) < 128 && len(str) < len(s.scratch)-n {
		s.scratch[n] = byte(len(str))
		s.n = n + 1 + copy(s.scratch[n+1:], str)
		return
	}
	s.writeStringSlow(str)
}

func (s *canonicalSink) writeStringSlow(str string) {
	s.writeUvarint(uint64(len(str)))
	s.writeStringRaw(str)
}

// writeStringRaw copies a string into the scratch without a []byte conversion
// (copy accepts a string source directly), so it never allocates regardless of
// length.
func (s *canonicalSink) writeStringRaw(str string) {
	if len(str) <= len(s.scratch)-s.n {
		s.n += copy(s.scratch[s.n:], str)
		return
	}
	for len(str) > 0 {
		if s.n == len(s.scratch) {
			s.flush()
		}
		k := copy(s.scratch[s.n:], str)
		s.n += k
		str = str[k:]
	}
}

func (s *canonicalSink) writeUvarint(v uint64) {
	if len(s.scratch)-s.n < binary.MaxVarintLen64 {
		s.flush()
	}
	n := binary.PutUvarint(s.scratch[s.n:], v)
	s.n += n
}

func (s *canonicalSink) writeVarint(v int64) {
	if len(s.scratch)-s.n < binary.MaxVarintLen64 {
		s.flush()
	}
	n := binary.PutVarint(s.scratch[s.n:], v)
	s.n += n
}

// count is the total canonical stream length written so far.
func (s *canonicalSink) count() int { return s.written + s.n }

func (s *canonicalSink) sum() [32]byte {
	s.flush()
	var sum [32]byte
	// Write the digest into sum's own backing (cap(sum[:0]) == 32): no per-fire
	// [32]byte allocation from h.Sum(nil).
	s.h.Sum(sum[:0])
	return sum
}

func (s *canonicalSink) bytes() []byte {
	s.flush()
	return s.out
}

// decisionDigester is the compile-lane-owned production digest state: a fixed
// 32 KiB scratch sink plus reusable sort scratch, with no buffer sized by the
// canonical stream. Single owner: the serial compile lane (compileOnce).
type decisionDigester struct {
	sink canonicalSink
}

func (d *decisionDigester) digest(dv *DecisionView) decisionDigest {
	d.sink.collect = false
	d.sink.resetDigest()
	encodeCanonical(&d.sink, dv)
	sum := d.sink.sum()
	return decisionDigest{sum: sum, count: int64(d.sink.count()), isNil: dv == nil}
}

// decisionViewBytes 编码为规范字节（测试与一次性调用面；每次新建收集 sink）。
// 生产摘要路径不调用物化方法——它直接把同一规范遍历流写进有界 SHA-256。
func decisionViewBytes(d *DecisionView) []byte {
	var s canonicalSink
	s.collect = true
	s.resetCollect()
	encodeCanonical(&s, d)
	return s.bytes()
}

// encodeCanonical writes the canonical deterministic byte stream of d: routes
// sorted by full RouteRef identity, compiled lanes in published order with
// request-independent metadata, weights sorted by account ID. This is the
// single traversal shared by the production digest and test materialization;
// the returned count is the encoded length (0 for a nil view). A nil view exits
// before writing; a non-nil empty route map writes the count zero.
func encodeCanonical(s *canonicalSink, d *DecisionView) int {
	if d == nil {
		return 0
	}
	// Zero the previous fire's RouteRef entries before reuse: a plain
	// refs[:0] would leave the retired RouteRef string fields live in the
	// backing array past len, retaining route strings across fires.
	clear(s.refs)
	s.refs = s.refs[:0]
	for k := range d.routes {
		s.refs = append(s.refs, k)
	}
	// slices.SortFunc is generic (no reflect.Swapper allocation) unlike
	// sort.Slice, keeping the steady-state digest allocation-free.
	slices.SortFunc(s.refs, func(a, b RouteRef) int {
		switch {
		case lessRouteRef(a, b):
			return -1
		case lessRouteRef(b, a):
			return 1
		default:
			return 0
		}
	})
	s.writeUvarint(uint64(len(s.refs)))
	for _, ref := range s.refs {
		rd := d.routes[ref]
		s.writeVarint(ref.GroupID)
		s.writeString(ref.Format)
		s.writeString(ref.Model)
		s.writeString(ref.OperationTag)
		s.writeString(ref.RouteClassID)
		s.writeString(rd.Format)
		s.writeString(rd.RequestedModel)
		s.writeString(rd.RouteClassID)
		s.writeString(rd.CallerCategory)
		s.writeString(rd.OperationTag)
		writeCompiled(s, rd.Primary)
		writeCompiled(s, rd.Degraded)
		writeCompiled(s, rd.Explore.Ordered)
		s.ids = s.ids[:0]
		for id := range rd.Explore.Weights {
			s.ids = append(s.ids, id)
		}
		slices.Sort(s.ids)
		s.writeUvarint(uint64(len(s.ids)))
		for _, id := range s.ids {
			s.writeVarint(id)
			s.writeVarint(int64(rd.Explore.Weights[id]))
		}
		s.writeUvarint(uint64(len(rd.Explore.Cumulative)))
		for _, c := range rd.Explore.Cumulative {
			s.writeUvarint(c)
		}
		s.writeUvarint(rd.Explore.Total)
		s.writeVarint(int64(rd.Explore.ExploreBP))
		writeCompiledIndices(s, rd.Explore.Ordered, rd.Explore.Fallback)
		writeCacheDomainPlan(s, rd)
		writeIncident(s, rd.Incident)
	}
	return s.count()
}

// writeIncident encodes the expose-only mark in fixed field order (sorted
// routes + fixed order = deterministic flips).
func writeIncident(s *canonicalSink, inc RouteIncident) {
	if inc.Active {
		s.writeUvarint(1)
	} else {
		s.writeUvarint(0)
	}
	s.writeString(inc.Kind)
	s.writeUvarint(uint64(inc.Comparable))
	s.writeUvarint(uint64(inc.Degraded))
	s.writeUvarint(uint64(inc.Domains))
	s.writeVarint(inc.EvaluatedMinute)
}

func writeCacheDomainPlan(s *canonicalSink, decision *RouteDecision) {
	s.writeUvarint(uint64(len(decision.CacheDomainRing.Domains)))
	for _, domain := range decision.CacheDomainRing.Domains {
		s.writeString(domain)
	}
	s.writeUvarint(uint64(len(decision.CacheDomainRing.Nodes)))
	for _, node := range decision.CacheDomainRing.Nodes {
		s.writeUvarint(node.Hash)
		s.writeString(node.Domain)
	}
	s.writeUvarint(uint64(len(decision.CacheDomainAccounts)))
	for _, account := range decision.CacheDomainAccounts {
		s.writeVarint(account.AccountID)
		s.writeString(account.Domain)
	}
}

func writeCompiled(s *canonicalSink, cs []CompiledCandidate) {
	s.writeUvarint(uint64(len(cs)))
	for _, c := range cs {
		writeCompiledCandidate(s, c)
	}
}

func writeCompiledIndices(s *canonicalSink, cs []CompiledCandidate, indexes []uint16) {
	s.writeUvarint(uint64(len(indexes)))
	for _, idx := range indexes {
		if int(idx) < len(cs) {
			writeCompiledCandidate(s, cs[idx])
		}
	}
}

func writeCompiledCandidate(s *canonicalSink, c CompiledCandidate) {
	s.writeVarint(c.AccountID)
	s.writeString(string(c.Lane))
	s.writeVarint(c.TemplateID)
	s.writeString(c.BaseURL)
	s.writeString(c.Fingerprint)
	s.writeString(c.RequestedModel)
	s.writeString(c.MappedModel)
	s.writeString(string(c.MappingMode))
	s.writeString(c.Quality)
	s.writeString(c.QualityRaw)
	s.writeVarint(c.IdentityRevision)
}
