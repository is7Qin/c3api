// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

var benchmarkHeldBytes []byte

// --- T3: bounded streaming compare sink ---

// TestCompareSinkExactness pins the exact equality predicate of the compare
// sink: equal ⇒ true; any single-byte difference (first/middle/last), a longer
// stream, a shorter stream (remaining old suffix) and an empty old all ⇒ false.
func TestCompareSinkExactness(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)})
	dv := s.View().DecisionView()
	require.NotNil(t, dv)
	old := decisionViewBytes(dv)
	require.NotEmpty(t, old)

	compare := func(target []byte, mutate func([]byte)) bool {
		cpy := append([]byte(nil), target...)
		if mutate != nil {
			mutate(cpy)
		}
		var e decisionEncoder
		return e.compareCanonical(dv, cpy)
	}

	require.True(t, compare(old, nil), "identical bytes must be equal")
	require.False(t, compare(old, func(b []byte) { b[0] ^= 0xff }), "first-byte difference ⇒ unequal")
	require.False(t, compare(old, func(b []byte) { b[len(b)/2] ^= 0xff }), "middle-byte difference ⇒ unequal")
	require.False(t, compare(old, func(b []byte) { b[len(b)-1] ^= 0xff }), "last-byte difference ⇒ unequal")
	require.False(t, compare(old[:len(old)-1], nil), "a shorter old leaves a remaining suffix ⇒ unequal")
	require.False(t, compare(append(append([]byte(nil), old...), 0x00), nil), "a longer old ⇒ unequal")
	require.False(t, compare(nil, nil), "an empty old with non-empty output ⇒ unequal")
}

// TestCompareSinkNilAndEmptyView: a nil DecisionView encodes to the empty byte
// string, so it compares equal only to an empty old — never panics — while an
// empty (non-nil) view still encodes one uvarint route count and is thereby
// distinct from nil (spec §2 T3.9: keep the d==nil vs empty-view distinction).
func TestCompareSinkNilAndEmptyView(t *testing.T) {
	var e decisionEncoder
	require.Nil(t, e.encode(nil))
	require.True(t, e.compareCanonical(nil, nil), "nil dv vs empty old ⇒ equal")
	require.False(t, e.compareCanonical(nil, []byte{0x01}), "nil dv vs non-empty old ⇒ unequal, no panic")

	empty := &DecisionView{routes: map[RouteRef]*RouteDecision{}}
	emptyBytes := decisionViewBytes(empty)
	require.NotEmpty(t, emptyBytes, "an empty view still encodes a route-count uvarint")
	require.True(t, e.compareCanonical(empty, emptyBytes))
	require.False(t, e.compareCanonical(empty, nil), "empty view vs nil old ⇒ unequal")
	require.False(t, e.compareCanonical(nil, emptyBytes), "nil vs empty-view bytes ⇒ unequal")
}

// TestCompareSinkRecordsTrueFirstDiff: the compare sink records the TRUE
// absolute offset of the first differing byte (only on mismatch) as well as the
// total stream length — proving the offset is not merely the block start.
func TestCompareSinkRecordsTrueFirstDiff(t *testing.T) {
	s := schedulerWithAccounts(t, 40, domain.ModelMapping{})
	dv := s.View().DecisionView()
	require.NotNil(t, dv)
	old := decisionViewBytes(dv)

	for _, k := range []int{0, 1, len(old) / 2, len(old) - 1} {
		cpy := append([]byte(nil), old...)
		cpy[k] ^= 0xff
		var e decisionEncoder
		eb := encBuf{compare: true, old: cpy, equal: true, firstDiff: -1}
		e.writeCanonical(&eb, dv)
		require.False(t, eb.result())
		require.Equal(t, k, eb.firstDiff, "first differing offset must be exact")
		require.Equal(t, len(old), eb.total, "total stream length must be recorded")
	}

	// Equal run: no mismatch ⇒ firstDiff stays the sentinel, total == len(old).
	var e decisionEncoder
	eb := encBuf{compare: true, old: old, equal: true, firstDiff: -1}
	e.writeCanonical(&eb, dv)
	require.True(t, eb.result())
	require.Equal(t, -1, eb.firstDiff)
	require.Equal(t, len(old), eb.total)

	// New stream longer than old: divergence is exactly at len(old).
	shorter := old[:len(old)-1]
	eb2 := encBuf{compare: true, old: shorter, equal: true, firstDiff: -1}
	e2 := decisionEncoder{}
	e2.writeCanonical(&eb2, dv)
	require.False(t, eb2.result())
	require.Equal(t, len(shorter), eb2.firstDiff, "stream longer than old: divergence at end of old")

	// Old longer than stream: the new stream is a proper prefix of old ⇒ an
	// unequal length is recorded as firstDiff == total (the new stream length),
	// not the -1 equal sentinel.
	longer := append(append([]byte(nil), old...), 0x00)
	eb3 := encBuf{compare: true, old: longer, equal: true, firstDiff: -1}
	e2.writeCanonical(&eb3, dv)
	require.False(t, eb3.result())
	require.Equal(t, len(old), eb3.firstDiff, "new stream is a prefix of old — firstDiff is the new stream length")
	require.Equal(t, len(old), eb3.total, "total stream length still recorded")
}

// sameBacking reports whether two non-empty byte slices share a backing array
// (a stable substitute for pointer identity on []byte).
func sameBacking(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == 0 && len(b) == 0
	}
	return &a[0] == &b[0]
}

// TestCompareSinkCrossBlockVarintAndLargeString exercises a multi-byte varint
// compared across a mismatch and a difference buried deep inside a >64KB string
// written as its own chunk — the offset must be located inside that chunk.
func TestCompareSinkCrossBlockVarintAndLargeString(t *testing.T) {
	// Multi-byte uvarint (0x80 0x01 == 128) with its second byte differing.
	old := []byte{0x80, 0x01}
	var e decisionEncoder
	eb := encBuf{compare: true, old: append([]byte(nil), old...), equal: true, firstDiff: -1}
	_, _ = eb.Write(old)
	require.True(t, eb.result())
	cpy := append([]byte(nil), old...)
	cpy[1] = 0x02
	eb2 := encBuf{compare: true, old: cpy, equal: true, firstDiff: -1}
	_, _ = eb2.Write(old)
	require.False(t, eb2.result())
	require.Equal(t, 1, eb2.firstDiff, "difference inside a multi-byte varint chunk")

	// Deep difference inside a large string field of the real encoding.
	big := strings.Repeat("z", 70000)
	tpl := tplWith(domain.FormatOpenAIChat, []string{big})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 4)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	dv := s.View().DecisionView()
	require.NotNil(t, dv)
	out := decisionViewBytes(dv)
	require.Greater(t, len(out), 70000)
	k := len(out) - 5
	cpyOut := append([]byte(nil), out...)
	cpyOut[k] ^= 0xff
	eb4 := encBuf{compare: true, old: cpyOut, equal: true, firstDiff: -1}
	e.writeCanonical(&eb4, dv)
	require.False(t, eb4.result())
	require.Equal(t, k, eb4.firstDiff, "difference deep inside a large string chunk")
}

// TestCompareSinkSameRootContentChangeRepublishes: same static root, changed
// content ⇒ the changed encoding publishes and commits a FRESH held slice.
func TestCompareSinkSameRootContentChangeRepublishes(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	q := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{1: qualityInput(30, 29, 100, 100)}, accs)
	wireSources(s, q, map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}})
	s.compileOnce()
	root := s.View().StaticView()
	held := s.lastDecisionBytes
	gen := s.View().Generation()
	require.NotNil(t, held)

	// Content change on the SAME static root (quality window shift only).
	q2 := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{1: qualityInput(30, 3, 100, 100)}, accs)
	wireSources(s, q2, map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}})
	s.compileOnce()
	require.Same(t, root, s.View().StaticView(), "static root must be unchanged")
	require.Greater(t, s.View().Generation(), gen, "same-root content change must publish")
	require.False(t, sameBacking(held, s.lastDecisionBytes), "changed bytes must commit a fresh held slice")
}

// TestCompareSinkRootOnlyChangePublishesWithoutReencode: bytes identical but the
// static root changed ⇒ still publish, WITHOUT re-encoding (the held slice is
// reused byte-for-byte).
func TestCompareSinkRootOnlyChangePublishesWithoutReencode(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {accWithEnabled(1, tpl, true, 4)}})
	s := newSched(t, m)
	published := s.View()
	held := s.lastDecisionBytes
	require.NotNil(t, held)

	m.mu.Lock()
	m.byGroup[10] = []*domain.Account{accWithEnabled(1, tpl, true, 1)}
	m.mu.Unlock()
	require.NoError(t, s.reload(context.Background()))
	s.compileOnce()

	after := s.View()
	require.NotSame(t, published.DecisionView(), after.DecisionView(), "root change must bypass the byte cache and publish")
	require.Equal(t, decisionViewBytes(published.DecisionView()), decisionViewBytes(after.DecisionView()))
	require.True(t, sameBacking(held, s.lastDecisionBytes), "equal bytes on a changed root must reuse the held slice (no re-encode)")
}

// TestCompareSinkCompileFailureKeepsHeldBytes: a compile failure retains the
// old published pair and never touches the byte cache.
func TestCompareSinkCompileFailureKeepsHeldBytes(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {accWithEnabled(1, tpl, true, 4)}})
	s := newSched(t, m)
	held := s.lastDecisionBytes
	require.NotNil(t, held)

	m.mu.Lock()
	m.byGroup[10] = append(m.byGroup[10], accWithEnabled(2, tpl, true, 4))
	m.mu.Unlock()
	require.NoError(t, s.reload(context.Background()))

	s.compiler = &failCompiler{err: context.DeadlineExceeded}
	s.compileOnce()
	require.True(t, sameBacking(held, s.lastDecisionBytes), "a failed compile must not touch the byte cache")
}

// TestCompareSinkSupersededCompileKeepsHeldBytes: a compile superseded by a
// newer staging is dropped BEFORE any cache commit, so the held bytes survive.
func TestCompareSinkSupersededCompileKeepsHeldBytes(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {accWithEnabled(1, tpl, true, 4)}})
	s := newSchedStatic(t, m)
	wireSources(s, nil, nil)
	s.compileOnce() // publish a first paired decision
	held := append([]byte(nil), s.lastDecisionBytes...)
	require.NotEmpty(t, held)

	bc := &blockingCompiler{entered: make(chan struct{}, 1), release: make(chan struct{}), inner: NewRoutingCompiler()}
	s.compiler = bc
	t.Cleanup(func() {
		select {
		case <-bc.release:
		default:
			close(bc.release)
		}
	})
	done := make(chan struct{})
	go func() {
		s.compileOnce()
		close(done)
	}()
	<-bc.entered

	m.mu.Lock()
	m.byGroup[10] = []*domain.Account{accWithEnabled(1, tpl, true, 1)}
	m.mu.Unlock()
	s.InvalidateGroup(10) // stages a newer pending root, superseding the in-flight compile
	close(bc.release)
	<-done

	require.Equal(t, held, s.lastDecisionBytes, "a superseded compile must not touch the byte cache")
}

// nilCompiler is a compile-lane seam that SUCCEEDS with a nil decision view
// (whose canonical encoding is the empty byte string).
type nilCompiler struct{}

func (nilCompiler) Compile(CompilerInputs) (*DecisionView, error) { return nil, nil }

// TestCompareSinkNilDecisionCommitsNilCache: a SUCCESSFUL compile whose decision
// view is nil encodes to the empty byte string; the cache must commit that nil
// (replacing a previously non-empty held slice), so a non-empty→nil change can
// never keep a stale, non-empty cache. The published view is fail-closed (nil
// decision).
func TestCompareSinkNilDecisionCommitsNilCache(t *testing.T) {
	s := schedulerWithAccounts(t, 200, domain.ModelMapping{})
	require.NotEmpty(t, s.lastDecisionBytes, "precondition: a non-empty decision is held")
	require.NoError(t, s.reload(context.Background())) // stage a fresh pending root
	s.compiler = nilCompiler{}
	s.compileOnce()
	require.Nil(t, s.View().DecisionView(), "a nil decision publishes fail-closed")
	require.Nil(t, s.lastDecisionBytes, "the nil encoding must be committed, not skipped")
}

// TestCompareSinkPublishBaseFailureIsolatesCache: on the decision-only publish
// path (no staged root) the compile captures the published base under
// publisher.mu, releases it, compiles, then calls publishWithBase. If that base
// changed in the meantime, publishWithBase fails, re-arms a compile and the
// fresh (unequal) encoding must be DISCARDED — the held byte cache survives
// untouched (spec §2 T3.7).
func TestCompareSinkPublishBaseFailureIsolatesCache(t *testing.T) {
	s := schedulerWithAccounts(t, 200, domain.ModelMapping{})
	require.True(t, s.publishedViewWhole())
	require.Nil(t, s.publisher.pending, "precondition: no staged root (decision-only path)")
	before := s.lastDecisionBytes
	require.NotEmpty(t, before)
	baseStatic := s.View().StaticView()

	// Real dynamic drift (quality appears for account 1) so the fire is NOT
	// skipped and the recompile's bytes DIFFER from the held encoding.
	accs := make([]*domain.Account, 0, len(s.View().ByID()))
	for _, as := range s.View().ByID() {
		a := as.static.Load().acc
		accs = append(accs, &a)
	}
	wireSources(s, buildQuality(10, domain.FormatOpenAIChat, mappingRequestModel,
		map[int64]CandidateQualityInput{1: qualityInput(30, 3, 100, 100)}, accs), nil)

	bc := &blockingCompiler{entered: make(chan struct{}, 1), release: make(chan struct{}), inner: NewRoutingCompiler()}
	s.compiler = bc
	t.Cleanup(func() {
		select {
		case <-bc.release:
		default:
			close(bc.release)
		}
	})

	done := make(chan struct{})
	go func() {
		s.compileOnce()
		close(done)
	}()
	<-bc.entered

	// Bump the published generation while the compile is in flight so the base
	// captured before the compile goes stale.
	s.publisher.mu.Lock()
	cur := s.view.Load()
	s.view.Store(&RoutingView{generation: cur.generation + 1, static: cur.static, decision: cur.decision})
	s.gen.Store(cur.generation + 1)
	s.publisher.mu.Unlock()

	close(bc.release)
	<-done

	require.Same(t, baseStatic, s.View().StaticView(), "a failed decision-only publish must not touch the static root")
	require.True(t, sameBacking(before, s.lastDecisionBytes),
		"a failed publishWithBase must not update the byte cache")
}

// TestDecisionEncoderHoldsNoOutputBuffer: the encoder must not retain any output
// buffer across fires (T3 removes the ~64MB resident bytes.Buffer). Only the
// refs/ids scratch slices remain.
func TestDecisionEncoderHoldsNoOutputBuffer(t *testing.T) {
	rt := reflect.TypeOf(decisionEncoder{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		require.NotEqual(t, "bytes.Buffer", f.Type.String(),
			"decisionEncoder must not hold an output buffer field %q", f.Name)
		require.Equal(t, reflect.Slice, f.Type.Kind(),
			"decisionEncoder field %q must be a scratch slice", f.Name)
	}
}

func BenchmarkDecisionViewCompareEqual(b *testing.B) {
	s := schedulerWithAccounts(b, 5000, domain.ModelMapping{})
	dv := s.View().DecisionView()
	if dv == nil {
		b.Fatal("missing decision view")
	}
	old := decisionViewBytes(dv)
	var e decisionEncoder
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkCompareEqual = e.compareCanonical(dv, old)
	}
}

// BenchmarkDecisionViewCompareChanged measures the FULL changed path per
// iteration: the compare pass AND — on the (always-taken) unequal branch — the
// encode of the same dv whose fresh slice is held. This is the number the
// ≤2.5× threshold is judged against; a compare-only loop would understate it.
func BenchmarkDecisionViewCompareChanged(b *testing.B) {
	s := schedulerWithAccounts(b, 5000, domain.ModelMapping{})
	dv := s.View().DecisionView()
	if dv == nil {
		b.Fatal("missing decision view")
	}
	old := decisionViewBytes(dv)
	old[len(old)/2] ^= 0xff // first difference at the mid-stream byte
	var e decisionEncoder
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !e.compareCanonical(dv, old) {
			benchmarkHeldBytes = e.encode(dv)
		}
	}
}

// BenchmarkDecisionViewCompareChangedTail measures the changed path when the
// FIRST difference is at the LAST byte (the compare must stream the whole old
// encoding before the mismatch) — compare + encode + hold, like the mid-stream
// bench. Reported alongside the mid-stream changed number for both thresholds.
func BenchmarkDecisionViewCompareChangedTail(b *testing.B) {
	s := schedulerWithAccounts(b, 5000, domain.ModelMapping{})
	dv := s.View().DecisionView()
	if dv == nil {
		b.Fatal("missing decision view")
	}
	old := decisionViewBytes(dv)
	old[len(old)-1] ^= 0xff // first difference at the final byte
	var e decisionEncoder
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !e.compareCanonical(dv, old) {
			benchmarkHeldBytes = e.encode(dv)
		}
	}
}

// BenchmarkDecisionViewRootOnlyChange drives the real root-only publish path:
// bytes stay byte-identical while the static root changes, so the sink must
// publish once per iteration (generation advances) WITHOUT re-encoding (the
// held slice is reused). It asserts publish count == b.N and held-slice
// identity stability instead of micro-benching an equal-only compare loop.
func BenchmarkDecisionViewRootOnlyChange(b *testing.B) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {accWithEnabled(1, tpl, true, 4)}})
	s := New(testCfg(), m, newTestRuleEngine(b), nil, nil, nil, nil)
	if err := s.reload(context.Background()); err != nil {
		b.Fatal(err)
	}
	wireSources(s, nil, nil)
	s.compileOnce()
	held := s.lastDecisionBytes
	if len(held) == 0 {
		b.Fatal("missing held bytes")
	}
	gen0 := s.View().Generation()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Root-only change: the account's lifecycle revision moves but every
		// canonical decision byte stays identical.
		m.mu.Lock()
		a := accWithEnabled(1, tpl, true, 4)
		a.LifecycleRevision = int64(i + 2)
		m.byGroup[10] = []*domain.Account{a}
		m.mu.Unlock()
		if err := s.reload(context.Background()); err != nil {
			b.Fatal(err)
		}
		s.compileOnce()
	}
	b.StopTimer()
	if publishes := int(s.View().Generation() - gen0); publishes != b.N {
		b.Fatalf("root-only publishes = %d, want %d (one per iteration)", publishes, b.N)
	}
	if !sameBacking(held, s.lastDecisionBytes) {
		b.Fatal("root-only equal bytes must reuse the held slice (re-encode count must be 0)")
	}
}
