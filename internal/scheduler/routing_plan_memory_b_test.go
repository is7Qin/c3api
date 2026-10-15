// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

// Routing-plan-memory B (bounded streaming SHA-256 publish guard) acceptance
// tests, spec §5 B1–B3. Package-internal on purpose (asserts guard state).

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// --- B1 ---

func rpmSyntheticView(baseURL string) *DecisionView {
	ref := RouteRef{GroupID: 1, Format: "openai-chat", Model: "m", OperationTag: "chat_completions"}
	return &DecisionView{routes: map[RouteRef]*RouteDecision{
		ref: {
			Format: "openai-chat", RequestedModel: "m", RouteClassID: "00",
			CallerCategory: "chat", OperationTag: "chat_completions",
			Primary: []CompiledCandidate{{
				AccountID: 1, Lane: AttemptLanePrimary, TemplateID: 1, BaseURL: baseURL,
				Fingerprint: "fp", RequestedModel: "m", MappedModel: "m", Quality: "q", QualityRaw: "q",
			}},
			CacheDomainRing:     CacheDomainRing{Nodes: []CacheDomainNode{{Hash: 1, Domain: "d"}}, Domains: []string{"d"}},
			CacheDomainAccounts: []CacheDomainAccount{{AccountID: 1, Domain: "d"}},
		},
	}}
}

func TestRPM_B1_DigestMatchesFrozenOracle(t *testing.T) {
	var d decisionDigester
	large := rpmSyntheticView(strings.Repeat("x", 5*digestScratchSize))
	cases := map[string]*DecisionView{
		"nil":          nil,
		"empty-routes": {routes: map[RouteRef]*RouteDecision{}},
		"small":        rpmSyntheticView("https://u/v1"),
		"large":        large,
	}
	for name, dv := range cases {
		got := d.digest(dv)
		wantBytes := baseCanonicalBytesOracle(dv)
		require.Equal(t, sha256.Sum256(wantBytes), got.sum, "%s digest must equal sha256(BASE oracle bytes)", name)
		require.Equal(t, int64(len(wantBytes)), got.count, "%s byte count", name)
		require.Equal(t, dv == nil, got.isNil, "%s nil marker", name)
	}

	nilD := d.digest(nil)
	emptyD := d.digest(&DecisionView{routes: map[RouteRef]*RouteDecision{}})
	require.True(t, nilD.isNil)
	require.False(t, emptyD.isNil)
	require.Zero(t, nilD.count)
	require.Equal(t, int64(1), emptyD.count, "non-nil empty routes is one zero varint byte")
	require.NotEqual(t, nilD.sum, emptyD.sum, "nil and empty must stay distinguishable")

	// Reuse across sizes must not contaminate (scratch + refs/ids reset).
	small := rpmSyntheticView("https://u/v1")
	first := d.digest(small)
	_ = d.digest(large)
	second := d.digest(small)
	require.Equal(t, first, second)
}

// --- B2 ---

// rpmAdvanceGenCompiler compiles normally but, once, advances the published
// generation mid-compile so the fire's base-generation CAS fails.
type rpmAdvanceGenCompiler struct {
	inner *RoutingCompiler
	sched *Scheduler
	once  bool
}

func (c *rpmAdvanceGenCompiler) Compile(in CompilerInputs) (*DecisionView, error) {
	dv, err := c.inner.Compile(in)
	if err != nil {
		return nil, err
	}
	if !c.once {
		c.once = true
		v := c.sched.View()
		c.sched.publisher.publishWithBase(v.Generation(), v.StaticView(), func(cur *RoutingView) *DecisionView {
			return cur.decision
		})
	}
	return dv, nil
}

// rpmSupersedeCompiler stages a fresh static root mid-compile so the fire's
// pending snapshot is stale (superseded path).
type rpmSupersedeCompiler struct {
	inner *RoutingCompiler
	sched *Scheduler
	once  bool
}

func (c *rpmSupersedeCompiler) Compile(in CompilerInputs) (*DecisionView, error) {
	if !c.once {
		c.once = true
		_ = c.sched.reload(context.Background())
	}
	return c.inner.Compile(in)
}

func TestRPM_B2_DigestGuardPublishState(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSchedStatic(t, m)
	qA := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{
		1: qualityInput(30, 29, 100, 100), 2: qualityInput(10, 5, 100, 100),
	}, accs)
	prices := map[string]domain.ResolvedPrices{"m": {InputPerM: new(int64(1000))}}
	wireSources(s, qA, prices)
	s.compileOnce()

	var dig decisionDigester
	require.True(t, s.lastDigestSet)
	require.Equal(t, dig.digest(s.View().DecisionView()), s.lastDigest)
	gen0 := s.View().Generation()
	dig0 := s.lastDigest

	// Equal fire with a forced route scope (not fireSkip, no root staging):
	// digest equal + same root -> no publish.
	s.enqueueCompileScope([]int64{10}, nil, scopeCauseGroup)
	s.compileOnce()
	require.Equal(t, gen0, s.View().Generation(), "equal digest + same root must not publish")
	require.Equal(t, dig0, s.lastDigest)

	// Changed content publishes and advances the digest.
	qB := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{
		1: qualityInput(10, 5, 100, 100), 2: qualityInput(30, 29, 100, 100),
	}, accs)
	wireSources(s, qB, prices)
	s.compileOnce()
	require.Greater(t, s.View().Generation(), gen0, "changed digest must publish")
	require.NotEqual(t, dig0, s.lastDigest)
	require.Equal(t, dig.digest(s.View().DecisionView()), s.lastDigest)

	// Compile error retains the exact digest/static.
	genErr := s.View().Generation()
	digErr := s.lastDigest
	s.compiler = &failCompiler{err: context.DeadlineExceeded}
	s.enqueueCompileScope([]int64{10}, nil, scopeCauseGroup)
	s.compileOnce()
	require.Equal(t, genErr, s.View().Generation())
	require.Equal(t, digErr, s.lastDigest)
	require.True(t, s.lastDigestSet)
}

func TestRPM_B2_SupersededLeavesGuard(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	dig0 := s.lastDigest
	gen0 := s.View().Generation()

	s.compiler = &rpmSupersedeCompiler{inner: NewRoutingCompiler(), sched: s}
	s.compileOnce()
	require.Equal(t, dig0, s.lastDigest, "superseded fire must not advance the digest")
	require.Equal(t, gen0, s.View().Generation(), "superseded fire must not publish")
}

func TestRPM_B2_BaseCASFailureLeavesGuard(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSchedStatic(t, m)
	qA := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{1: qualityInput(30, 29, 100, 100)}, accs)
	prices := map[string]domain.ResolvedPrices{"m": {InputPerM: new(int64(1000))}}
	wireSources(s, qA, prices)
	s.compileOnce()
	dig0 := s.lastDigest

	// Changed content, but the generation is advanced mid-compile so the
	// base-CAS fails: the guard must stay on the old digest.
	qB := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{1: qualityInput(10, 5, 100, 100)}, accs)
	wireSources(s, qB, prices)
	s.compiler = &rpmAdvanceGenCompiler{inner: NewRoutingCompiler(), sched: s}
	s.enqueueCompileScope([]int64{10}, nil, scopeCauseGroup)
	s.compileOnce()
	require.Equal(t, dig0, s.lastDigest, "failed base-CAS must not advance the digest")

	// A subsequent valid compile still publishes.
	s.compiler = NewRoutingCompiler()
	s.enqueueCompileScope([]int64{10}, nil, scopeCauseGroup)
	s.compileOnce()
	require.NotEqual(t, dig0, s.lastDigest, "valid compile must publish a new digest")
}

// --- B3 ---

func TestRPM_B3_NoEncodingSizedBuffer(t *testing.T) {
	// Scheduler must not own a bytes.Buffer or a stream-sized []byte field.
	rt := reflect.TypeOf(Scheduler{})
	for i := range rt.NumField() {
		f := rt.Field(i)
		require.NotEqual(t, "bytes.Buffer", f.Type.String(), "field %s", f.Name)
		require.NotEqual(t, "[]uint8", f.Type.String(), "field %s", f.Name)
	}

	s, _ := rpmHomogeneousScheduler(t, rpmModels(20))
	_ = s.digester.digest(s.View().DecisionView())
	require.Equal(t, digestScratchSize, len(s.digester.sink.scratch), "scratch is fixed-bounded")
	require.Zero(t, s.digester.sink.n)
	require.Zero(t, cap(s.digester.sink.out), "production digest path never accumulates bytes")
	require.False(t, s.digester.sink.collect)

	// Source-level: the removed retained buffer name must not reappear in the
	// production compile-lane files.
	for _, name := range []string{"scheduler.go", "routing_compiler_wire.go"} {
		raw, err := os.ReadFile(filepath.Join(".", name))
		require.NoError(t, err)
		require.NotContains(t, string(raw), "lastDecisionBytes  []byte", name)
		require.NotContains(t, string(raw), "decisionEncoder", name)
	}
}

// --- B2 nil publication protocol ---

// rpmNilCompiler is a CUSTOM TEST SEAM that returns a nil decision with no
// error. A nil DecisionView is not a production promise here — the production
// compiler always returns a non-nil view or an error — but the publish guard is
// nil-aware by design (spec §4), so this seam pins those branches. It
// deliberately implements neither scoped seam, so a scope-triggered fire takes
// the exact full-fidelity fallback and still returns nil.
type rpmNilCompiler struct{}

func (rpmNilCompiler) Compile(CompilerInputs) (*DecisionView, error) { return nil, nil }

// TestRPM_B2_NilDecisionPendingPairProtocol pins the publication protocol for a
// successful nil decision (custom seam): the first pending pair publishes and
// tracks the publisher-RETURNED static wrapper; an equal nil decision against
// the same static root does not republish (digest+root guard); an equal nil
// decision against a freshly staged root publishes. This is the same protocol
// BASE implements with `bytes.Equal(lastDecisionBytes, b) && lastCompiledStatic
// == target` (raw BASE evidence: .omo/reports/routing-plan-memory-fix-r1.md).
func TestRPM_B2_NilDecisionPendingPairProtocol(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSchedStatic(t, m)
	s.compiler = rpmNilCompiler{}

	var dig decisionDigester
	nilDigest := dig.digest(nil)
	require.True(t, nilDigest.isNil)
	require.Zero(t, nilDigest.count)

	// The first pending-pair publish: a static root is staged and no decision
	// has ever been published, so the nil decision must still publish. The
	// published static root is the publisher-RETURNED wrapper, not the staged
	// input root (publishPairLocked re-wraps an already-published static root).
	staged := s.publisher.pending
	require.NotNil(t, staged, "reload stages a pending static root")
	require.Nil(t, s.View().DecisionView())
	gen0 := s.View().Generation()
	s.compileOnce()
	require.Greater(t, s.View().Generation(), gen0, "first nil decision must publish via the pending pair")
	require.Nil(t, s.View().DecisionView())
	require.Nil(t, s.publisher.pending, "a paired publish clears the staged root")
	require.Same(t, s.View().StaticView(), s.lastCompiledStatic,
		"the guard must track the publisher-returned static root")
	require.NotSame(t, staged, s.lastCompiledStatic,
		"the published wrapper is not the staged input root")
	require.True(t, s.lastDigestSet)
	require.Equal(t, nilDigest, s.lastDigest)

	// Equal nil / same root: the guard must skip the republish. Force a route
	// scope so the fire is not a fireSkip and actually reaches the digest guard.
	gen1 := s.View().Generation()
	s.enqueueCompileScope([]int64{10}, nil, scopeCauseGroup)
	s.compileOnce()
	require.Equal(t, gen1, s.View().Generation(), "equal nil + same root must not republish")
	require.Equal(t, nilDigest, s.lastDigest)

	// Equal nil / fresh staged root: the root changed, so it must publish.
	require.NoError(t, s.reload(context.Background()))
	s.compileOnce()
	require.Greater(t, s.View().Generation(), gen1, "equal nil + changed root must publish")
	require.Nil(t, s.View().DecisionView())
	require.Equal(t, nilDigest, s.lastDigest)
}
