// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// t1Loader is a fault-injectable mem loader that counts full vs per-group loads
// so the batch path can be asserted (one LoadGroupAccounts per group; a real
// LoadGroupsAccounts on every forced full reload).
type t1Loader struct {
	mu         sync.Mutex
	byGroup    map[int64][]*domain.Account
	failGroup  map[int64]bool
	failFull   bool
	fullCalls  int
	groupCalls []int64
}

func newT1Loader(byGroup map[int64][]*domain.Account) *t1Loader {
	return &t1Loader{byGroup: byGroup, failGroup: map[int64]bool{}}
}

func (l *t1Loader) LoadGroupsAccounts(ctx context.Context) (map[int64][]*domain.Account, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fullCalls++
	if l.failFull {
		return nil, errors.New("t1Loader: full load failed")
	}
	out := make(map[int64][]*domain.Account, len(l.byGroup))
	for k, v := range l.byGroup {
		out[k] = v
	}
	return out, nil
}

func (l *t1Loader) LoadGroupAccounts(ctx context.Context, id int64) ([]*domain.Account, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.groupCalls = append(l.groupCalls, id)
	if l.failGroup[id] {
		return nil, errors.New("t1Loader: group load failed")
	}
	return l.byGroup[id], nil
}

func (l *t1Loader) reset() {
	l.mu.Lock()
	l.fullCalls, l.groupCalls = 0, nil
	l.mu.Unlock()
}
func (l *t1Loader) fullCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fullCalls
}
func (l *t1Loader) groupCallsCopy() []int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]int64(nil), l.groupCalls...)
}

// TestInvalidateGroupsMatchesSingleGroupTopology pins the equivalence contract
// against a FIXED expected topology (independent of the shared helper): batch
// over an unsorted id set must equal the single-group path executed in the same
// sorted order, including per-account groupIDs ORDER (construction order, not
// sorted order) and shared-account/removed-account handling.
func TestInvalidateGroupsMatchesSingleGroupTopology(t *testing.T) {
	tp := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	a1 := acc(1, tp, 4) // shared across G1 and G2
	a2 := acc(2, tp, 4) // G1 only
	a3 := acc(3, tp, 4) // G2 only
	a4 := acc(4, tp, 4) // added to G1 by the reload

	ldr := newT1Loader(map[int64][]*domain.Account{
		1: {a1, a2},
		2: {a1, a3},
	})
	s := New(testCfg(), ldr, newTestRuleEngine(t), nil, nil, nil, nil)
	require.NoError(t, s.reload(context.Background()))

	// Server-side change: G1 becomes {a1, a4} (a2 removed everywhere).
	ldr.mu.Lock()
	ldr.byGroup[1] = []*domain.Account{a1, a4}
	ldr.mu.Unlock()

	s.InvalidateGroups([]int64{2, 1}) // unsorted → sorted [1,2]

	// One load per group, in sorted order.
	require.Equal(t, []int64{1, 2}, ldr.groupCallsCopy(), "batch must load each group once, in sorted id order")

	sv := s.publisher.pending
	require.NotNil(t, sv, "a successful batch must stage exactly one frozen root")

	// a1: gid=1 step → [1,2]; gid=2 step → [2,1] (construction order).
	require.Equal(t, []int64{2, 1}, sv.byID[1].static.Load().groupIDs, "shared account groupIDs must follow single-group construction order")
	// a2 removed from every group → gone from byID.
	_, ok := sv.byID[2]
	require.False(t, ok, "account removed from all groups must leave byID")
	// a3 unchanged → [2].
	require.Equal(t, []int64{2}, sv.byID[3].static.Load().groupIDs)
	// a4 added → [1].
	require.Equal(t, []int64{1}, sv.byID[4].static.Load().groupIDs)
}

// TestInvalidateGroupsFailureSkipsStageKeepsRoot: a batch whose every group load
// fails stages nothing and keeps the published root, while marking the sticky
// retry obligation.
func TestInvalidateGroupsFailureSkipsStageKeepsRoot(t *testing.T) {
	tp := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	ldr := newT1Loader(map[int64][]*domain.Account{10: {acc(1, tp, 4)}})
	s := New(testCfg(), ldr, newTestRuleEngine(t), nil, nil, nil, nil)
	require.NoError(t, s.reload(context.Background()))
	beforeView := s.view.Load()
	beforePending := s.publisher.pending

	ldr.mu.Lock()
	ldr.failGroup[10] = true
	ldr.mu.Unlock()

	s.InvalidateGroups([]int64{10})

	require.Same(t, beforePending, s.publisher.pending, "an all-failed batch must not stage a new root")
	require.Same(t, beforeView, s.view.Load(), "the published root must be retained")
	require.True(t, s.reloadRequired, "a failed group load must set the sticky obligation")
}

type fakeStalenessProbe struct {
	mu     sync.Mutex
	counts domain.CompileStaleness
	err    error
}

func (p *fakeStalenessProbe) CompileStalenessSnapshot(ctx context.Context) (domain.CompileStaleness, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counts, p.err
}

// TestReloadRequiredStickyForcesBackstopFullReload: a probe-invisible failed
// group load (equal-count membership swap) must not be able to freeze the view —
// the backstop tick, even on a probe hit, must attempt a real full reload while
// the sticky obligation is set, and clear it on success.
func TestReloadRequiredStickyForcesBackstopFullReload(t *testing.T) {
	tp := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	ldr := newT1Loader(map[int64][]*domain.Account{10: {acc(1, tp, 4)}})
	probe := &fakeStalenessProbe{counts: domain.CompileStaleness{Accounts: 1, Groups: 1, Templates: 1}}
	s := New(Config{SyncInterval: time.Hour, StalenessProbe: probe}, ldr, newTestRuleEngine(t), nil, nil, nil, nil)
	require.NoError(t, s.reload(context.Background()))
	wireSources(s, nil, nil)
	s.compileOnce()
	require.True(t, s.publishedViewWhole(), "precondition: a whole view is published")

	// A group load fails → sticky obligation set, nothing staged.
	ldr.mu.Lock()
	ldr.failGroup[10] = true
	ldr.mu.Unlock()
	s.InvalidateGroups([]int64{10})
	require.True(t, s.reloadRequired)

	// A probe-HIT tick must NOT skip: it must attempt a real full reload.
	ldr.reset()
	s.backstopTick(context.Background())
	require.Equal(t, 1, ldr.fullCount(), "sticky reloadRequired must force a full loader call on a probe-hit tick")
	require.False(t, s.reloadRequired, "a successful full reload clears the sticky obligation")
}

// --- C1/I1: the public reload path must also set the sticky obligation ---

// TestPublicReloadFailureSetsStickyAndRetries is the direct regression for the
// gap where only the backstop tick re-marked reloadRequired: a public
// InvalidateAll* that fails after the baseline advanced must still leave the
// sticky obligation so the next probe-hit tick performs a real full reload.
func TestPublicReloadFailureSetsStickyAndRetries(t *testing.T) {
	tp := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	ldr := newT1Loader(map[int64][]*domain.Account{10: {acc(1, tp, 4)}})
	probe := &fakeStalenessProbe{counts: domain.CompileStaleness{Accounts: 1, Groups: 1, Templates: 1}}
	s := New(Config{SyncInterval: time.Hour, StalenessProbe: probe}, ldr, newTestRuleEngine(t), nil, nil, nil, nil)
	require.NoError(t, s.reload(context.Background()))
	wireSources(s, nil, nil)
	s.compileOnce()
	require.True(t, s.publishedViewWhole(), "precondition: a whole view is published")

	// Server change (probe-invisible: counts fixed) with the full load failing.
	ldr.mu.Lock()
	ldr.failFull = true
	ldr.byGroup[10] = []*domain.Account{acc(1, tp, 4), acc(2, tp, 4)}
	ldr.mu.Unlock()
	require.Error(t, s.InvalidateAllSyncCtx(context.Background()))
	require.True(t, s.reloadRequired, "a failed public reload must set the sticky obligation")

	// Heal; a probe-HIT tick must NOT skip — it must run the real full loader.
	ldr.mu.Lock()
	ldr.failFull = false
	ldr.mu.Unlock()
	ldr.reset()
	s.backstopTick(context.Background())
	require.Equal(t, 1, ldr.fullCount(), "sticky obligation must force a real full reload")
	require.False(t, s.reloadRequired)
	s.compileOnce()
	_, ok := s.View().ByID()[2]
	require.True(t, ok, "the recovered change must finally take effect")
}

// TestStickyForcesFullReloadOnProbeInvisibleSwap covers failure convergence (1):
// an equal-count membership swap is invisible to the count-only probe, so a
// failed group load must not be able to freeze the view — one healthy tick has
// to force a full reload and the swap must take effect.
func TestStickyForcesFullReloadOnProbeInvisibleSwap(t *testing.T) {
	tp := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	ldr := newT1Loader(map[int64][]*domain.Account{10: {acc(1, tp, 4), acc(2, tp, 4)}})
	probe := &fakeStalenessProbe{counts: domain.CompileStaleness{Accounts: 2, Groups: 1, Templates: 1}}
	s := New(Config{SyncInterval: time.Hour, StalenessProbe: probe}, ldr, newTestRuleEngine(t), nil, nil, nil, nil)
	require.NoError(t, s.reload(context.Background()))
	wireSources(s, nil, nil)
	s.compileOnce()
	require.True(t, s.publishedViewWhole())

	// Swap (1,2) -> (2,3): same count, different members. Group load fails.
	ldr.mu.Lock()
	ldr.byGroup[10] = []*domain.Account{acc(2, tp, 4), acc(3, tp, 4)}
	ldr.failGroup[10] = true
	ldr.mu.Unlock()
	s.InvalidateGroups([]int64{10})
	require.True(t, s.reloadRequired)

	ldr.mu.Lock()
	ldr.failGroup[10] = false
	ldr.mu.Unlock()
	ldr.reset()
	s.backstopTick(context.Background())
	require.Equal(t, 1, ldr.fullCount(), "one tick must force a full reload despite the probe hit")
	require.False(t, s.reloadRequired)
	s.compileOnce()
	byID := s.View().ByID()
	_, gone := byID[1]
	require.False(t, gone, "swapped-out account must be gone after convergence")
	require.Contains(t, byID, int64(3), "swapped-in account must appear")
}

// TestPartialSuccessKeepsStickyObligation covers failure convergence (2) AND the
// shared-account fix-up under partial failure (spec §2 T1.3 / spec:312): G1's
// load FAILS while G2 SUCCEEDS and drops their SHARED account A. The batch must
// keep the sticky obligation yet still fold G2's change — including the fix-up
// of A's reference in the FAILED G1 (A retained in G1 only, a NEW leaf sharing
// the runtime) — and the FINAL compiled published pair must reflect it. It also
// pins the two negatives the reviewer required: the enqueued scope is FIXED
// (covers the failed group's shared-account fix-up) and the OLD published
// pair/members are left untouched (the batch only stages).
func TestPartialSuccessKeepsStickyObligation(t *testing.T) {
	tp := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	shared := acc(1, tp, 4) // A: shared by G1 and G2
	g1only := acc(2, tp, 4)
	g2added := acc(3, tp, 4)
	ldr := newT1Loader(map[int64][]*domain.Account{
		1: {shared, g1only},
		2: {shared},
	})
	s := New(testCfg(), ldr, newTestRuleEngine(t), nil, nil, nil, nil)
	require.NoError(t, s.reload(context.Background()))
	wireSources(s, nil, nil)
	s.compileOnce()
	require.True(t, s.publishedViewWhole())
	beforeView := s.View()
	rtShared := beforeView.ByID()[1].runtime
	beforeShared := beforeView.ByID()[1]
	beforeG1Only := beforeView.ByID()[2]

	// G1 load fails; G2 succeeds and drops the shared A (G2 -> {g2added}).
	ldr.mu.Lock()
	ldr.failGroup[1] = true
	ldr.byGroup[2] = []*domain.Account{g2added}
	ldr.mu.Unlock()

	s.InvalidateGroups([]int64{1, 2})
	require.True(t, s.reloadRequired, "a partial failure must keep the sticky obligation")
	require.NotNil(t, s.publisher.pending, "the successful group must still stage")

	// The OLD published pair and its members must be untouched: a partial-failure
	// batch only stages a new root, it never republishes the old one.
	require.Same(t, beforeView, s.View(), "a partial-failure batch must not disturb the published pair")
	require.Same(t, beforeShared, s.View().ByID()[1], "the published shared leaf must stay stable")
	require.Same(t, beforeG1Only, s.View().ByID()[2], "the published failed-group leaf must stay stable")

	// The batched scope is FIXED and MUST cover the FAILED group G1: the successful
	// G2 dropped the shared A, so A's retained leaf in the failed G1 needs its
	// shared-account fix-up (scope in construction order: G2 then G1).
	scopes, overflow := s.drainCompileScopes()
	require.False(t, overflow)
	require.Len(t, scopes, 1, "one batched staging yields exactly one scope")
	require.Equal(t, scopeCauseGroup, scopes[0].cause)
	require.Equal(t, []int64{2, 1}, scopes[0].groups,
		"the scope must cover the failed group's shared-account fix-up (construction order)")
	require.Nil(t, scopes[0].accounts)
	s.requeueCompileScopes(scopes, overflow) // preserve the fire-owned scope for the compile below

	// Compile and assert the FINAL published pair (not merely a staged signal).
	s.compileOnce()
	byID := s.View().ByID()
	require.Contains(t, byID, int64(3), "G2's added account must be published")
	require.Contains(t, byID, int64(1), "the shared account must survive in the failed group")
	require.Equal(t, []int64{1}, byID[1].static.Load().groupIDs,
		"shared A, dropped by the successful G2, must retain only the failed G1")
	require.Contains(t, byID, int64(2), "G1's load failed, so its exclusive account is kept")
	require.Same(t, rtShared, byID[1].runtime, "the shared runtime must survive the partial-failure fix-up")
	// The failed G1 must reference the retained A leaf (its shared fix-up ran).
	var g1HasA bool
	for _, as := range s.View().StaticView().groups[1].accounts {
		if as.static.Load().acc.ID == 1 {
			require.Same(t, byID[1], as, "the failed group must point at A's retained leaf")
			g1HasA = true
		}
	}
	require.True(t, g1HasA, "the failed group must still contain A")
}

// TestStickySurvivesRepeatedFailuresThenClears covers failure convergence (3):
// a repeated full-load failure keeps the obligation; the next success clears it.
// The change is a probe-INVISIBLE equal-count membership swap, so only a real
// full reload can converge it — asserted against the FINAL compiled pair.
func TestStickySurvivesRepeatedFailuresThenClears(t *testing.T) {
	tp := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	ldr := newT1Loader(map[int64][]*domain.Account{10: {acc(1, tp, 4), acc(2, tp, 4)}})
	probe := &fakeStalenessProbe{counts: domain.CompileStaleness{Accounts: 2, Groups: 1, Templates: 1}}
	s := New(Config{SyncInterval: time.Hour, StalenessProbe: probe}, ldr, newTestRuleEngine(t), nil, nil, nil, nil)
	require.NoError(t, s.reload(context.Background()))
	wireSources(s, nil, nil)
	s.compileOnce()
	require.True(t, s.publishedViewWhole())
	require.Contains(t, s.View().ByID(), int64(1))

	// Probe-invisible equal-count swap (1,2)->(2,3) with the full load failing.
	ldr.mu.Lock()
	ldr.byGroup[10] = []*domain.Account{acc(2, tp, 4), acc(3, tp, 4)}
	ldr.failFull = true
	ldr.mu.Unlock()
	require.Error(t, s.InvalidateAllSyncCtx(context.Background()))
	require.True(t, s.reloadRequired)
	ldr.reset()
	s.backstopTick(context.Background())
	require.Equal(t, 1, ldr.fullCount())
	require.True(t, s.reloadRequired, "a repeated full-load failure keeps the obligation")

	// Heal: the next tick succeeds and the swap must finally take effect.
	ldr.mu.Lock()
	ldr.failFull = false
	ldr.mu.Unlock()
	ldr.reset()
	s.backstopTick(context.Background())
	require.Equal(t, 1, ldr.fullCount())
	require.False(t, s.reloadRequired, "a successful full reload clears the obligation")

	s.compileOnce()
	byID := s.View().ByID()
	require.NotContains(t, byID, int64(1), "the swapped-out account must be gone after convergence")
	require.Contains(t, byID, int64(3), "the swapped-in account must be published")
}

// TestStickySurvivesUnrelatedLocalSuccessThenHealthyTick covers failure
// convergence (1)+(2) as the full SEQUENCE the reviewer required: a batch in
// which G1's load FAILS but an UNRELATED G2 load SUCCEEDS. The unrelated local
// success must NOT clear the sticky obligation, so a later probe-HIT healthy
// tick still runs a REAL full loader (not merely a compile request) and the
// FINAL published pair reflects both equal-count swaps. Every membership change
// here is an EQUAL-COUNT swap, invisible to the count-only probe — only a real
// full reload can converge it.
func TestStickySurvivesUnrelatedLocalSuccessThenHealthyTick(t *testing.T) {
	tp := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	ldr := newT1Loader(map[int64][]*domain.Account{1: {acc(1, tp, 4)}, 2: {acc(2, tp, 4)}})
	probe := &fakeStalenessProbe{counts: domain.CompileStaleness{Accounts: 2, Groups: 2, Templates: 1}}
	s := New(Config{SyncInterval: time.Hour, StalenessProbe: probe}, ldr, newTestRuleEngine(t), nil, nil, nil, nil)
	require.NoError(t, s.reload(context.Background()))
	wireSources(s, nil, nil)
	s.compileOnce()
	require.True(t, s.publishedViewWhole())
	beforeView := s.View()
	require.Contains(t, beforeView.ByID(), int64(1))
	require.Contains(t, beforeView.ByID(), int64(2))

	// Probe-invisible equal-count swaps: G1 {a1}->{a3}, G2 {a2}->{a4}. G1's load
	// fails, G2's succeeds — an UNRELATED local success inside the same batch.
	ldr.mu.Lock()
	ldr.byGroup[1] = []*domain.Account{acc(3, tp, 4)}
	ldr.byGroup[2] = []*domain.Account{acc(4, tp, 4)}
	ldr.failGroup[1] = true
	ldr.mu.Unlock()

	s.InvalidateGroups([]int64{1, 2})
	require.True(t, s.reloadRequired, "an unrelated local success must NOT clear the sticky obligation")
	require.Same(t, beforeView, s.View(), "the batch only stages; the published pair must stay put")

	// The staged root holds the unrelated local success (G2) but not the failed
	// group's swap (G1): G2's a4 in, a2 out; G1's a1 kept, a3 absent.
	pending := s.publisher.pending
	require.NotNil(t, pending, "the successful G2 must still stage a working root")
	require.Contains(t, pending.byID, int64(4), "G2's unrelated success must fold into the staged root")
	require.NotContains(t, pending.byID, int64(2), "G2's swapped-out account must leave the staged root")
	require.Contains(t, pending.byID, int64(1), "the failed G1 must keep its old account in the staged root")
	require.NotContains(t, pending.byID, int64(3), "the failed G1's swap must NOT be applied")

	// Heal G1: a probe-HIT healthy tick must run a REAL full loader.
	ldr.mu.Lock()
	ldr.failGroup[1] = false
	ldr.mu.Unlock()
	ldr.reset()
	s.backstopTick(context.Background())
	require.Equal(t, 1, ldr.fullCount(), "the healthy tick must call the real full loader, not just request a compile")
	require.False(t, s.reloadRequired, "a successful full reload clears the obligation")

	// The FINAL compiled published pair must reflect BOTH swaps.
	s.compileOnce()
	require.True(t, s.publishedViewWhole(), "the final decision must be whole")
	final := s.View()
	require.NotContains(t, final.ByID(), int64(1), "G1's swapped-out account must be gone")
	require.NotContains(t, final.ByID(), int64(2), "G2's swapped-out account must be gone")
	require.Contains(t, final.ByID(), int64(3), "G1's swapped-in account must be published")
	require.Contains(t, final.ByID(), int64(4), "G2's swapped-in account must be published")
	require.Equal(t, []int64{3}, accountIDsOf(final.StaticView().groups[1]), "G1 must publish exactly its swapped-in account")
	require.Equal(t, []int64{4}, accountIDsOf(final.StaticView().groups[2]), "G2 must publish exactly its swapped-in account")
	require.NotNil(t, final.DecisionView(), "the published pair must carry a decision")
}

// --- I2: the batch shares one deadline and releases the publish lock ---

// deadlineLoader blocks one group's load until the batch context is cancelled.
type deadlineLoader struct {
	*t1Loader
	blockGroup int64
	entered    chan struct{}
}

func (l *deadlineLoader) LoadGroupAccounts(ctx context.Context, id int64) ([]*domain.Account, error) {
	if id == l.blockGroup {
		select {
		case l.entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return l.t1Loader.LoadGroupAccounts(ctx, id)
}

// gateProbe is a probe-entered/release handshake. Once arm() has been called,
// its NEXT CompileStalenessSnapshot signals `entered` and then blocks until
// `release` is closed. backstopTick calls the probe immediately before it takes
// publisher.mu, so a received `entered` proves a tick has reached the tick body
// (the last step before the lock) without any time-window guess; releasing it
// lets the tick proceed to a lock it must then wait on.
type gateProbe struct {
	counts  domain.CompileStaleness
	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	release chan struct{}
}

func (p *gateProbe) arm() {
	p.mu.Lock()
	p.armed = true
	p.mu.Unlock()
}

func (p *gateProbe) CompileStalenessSnapshot(ctx context.Context) (domain.CompileStaleness, error) {
	p.mu.Lock()
	gate := p.armed
	if gate {
		p.armed = false
	}
	p.mu.Unlock()
	if gate {
		select {
		case p.entered <- struct{}{}:
		default:
		}
		<-p.release
	}
	return p.counts, nil
}

// TestInvalidateGroupsBatchDeadlineReleasesLock proves the shared batch deadline
// fires AND covers failure-convergence case (4): a REAL backstop tick started
// while the batch holds publisher.mu (its loader stalled under the lock, i.e.
// between the batch refresh and its failure handling) must BLOCK on the lock —
// it cannot skip or rebuild until the batch returns. Entry into the lock wait is
// proven by a probe-entered/release channel handshake (NOT a 25ms time window):
// the armed probe fires exactly at the tick body's pre-lock step while the batch
// still holds the lock. Once the deadline fires the batch leaves the failure
// branch (sticky obligation set) and the unblocked tick runs a real full reload
// that finally publishes the probe-invisible equal-count swap.
func TestInvalidateGroupsBatchDeadlineReleasesLock(t *testing.T) {
	orig := batchStageTimeout
	batchStageTimeout = 100 * time.Millisecond
	defer func() { batchStageTimeout = orig }()

	tp := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	base := newT1Loader(map[int64][]*domain.Account{10: {acc(1, tp, 4)}})
	ldr := &deadlineLoader{t1Loader: base, blockGroup: 10, entered: make(chan struct{}, 1)}
	probe := &gateProbe{
		counts:  domain.CompileStaleness{Accounts: 1, Groups: 1, Templates: 1},
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	s := New(Config{SyncInterval: time.Hour, StalenessProbe: probe}, ldr, newTestRuleEngine(t), nil, nil, nil, nil)
	require.NoError(t, s.reload(context.Background()))
	wireSources(s, nil, nil)
	s.compileOnce()
	require.True(t, s.publishedViewWhole())
	require.Contains(t, s.View().ByID(), int64(1))
	base.reset()

	// Fixture change: an equal-count swap {a1}->{a2} the count-only probe cannot
	// see, so only a real full reload (forced by the sticky obligation) converges.
	ldr.mu.Lock()
	ldr.byGroup[10] = []*domain.Account{acc(2, tp, 4)}
	ldr.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.InvalidateGroups([]int64{10})
		close(done)
	}()
	select {
	case <-ldr.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("loader never entered the batch")
	}

	// Arm the gate so the NEXT probe call (the tick's) blocks at the tick body's
	// pre-lock step. A REAL tick started while the batch holds publisher.mu must
	// reach that step; the handshake proves it (no time-window guess).
	probe.arm()
	tickDone := make(chan struct{})
	go func() {
		s.backstopTick(context.Background())
		close(tickDone)
	}()
	select {
	case <-probe.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("tick never reached the probe")
	}
	// The tick is at the pre-lock step while the batch still holds the lock, so
	// the batch must not have returned yet.
	select {
	case <-done:
		t.Fatal("batch returned before the tick reached the lock")
	default:
	}
	// Release the probe: the tick now takes publisher.mu and must WAIT on it
	// (the batch holds it until its shared deadline fires), so the tick's full
	// reload can only run after the batch returns.
	close(probe.release)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("batch did not return after the shared deadline")
	}

	select {
	case <-tickDone:
	case <-time.After(5 * time.Second):
		t.Fatal("backstopTick did not complete after the batch released the lock")
	}
	// The probe returns the same counts as the baseline, so the ONLY reason the
	// unblocked tick did not skip is that the timed-out batch set the sticky
	// obligation — a real full reload proves it (and that it was then cleared).
	require.Equal(t, 1, base.fullCount(), "the unblocked tick must run a real full reload (implies the batch set the sticky obligation)")
	s.publisher.mu.Lock()
	cleared := !s.reloadRequired
	s.publisher.mu.Unlock()
	require.True(t, cleared, "the successful full reload clears the sticky obligation")

	// The FINAL published pair must reflect the swap.
	s.compileOnce()
	require.True(t, s.publishedViewWhole(), "the final decision must be whole")
	final := s.View()
	require.NotContains(t, final.ByID(), int64(1), "the swapped-out account must be gone")
	require.Contains(t, final.ByID(), int64(2), "the swapped-in account must be published")
	require.Equal(t, []int64{2}, accountIDsOf(final.StaticView().groups[10]), "group 10 must publish exactly its swapped-in account")
	require.NotNil(t, final.DecisionView(), "the published pair must carry a decision")

	locked := make(chan struct{})
	go func() {
		s.publisher.mu.Lock()
		s.publisher.mu.Unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(2 * time.Second):
		t.Fatal("publisher.mu was not released after the batch returned")
	}
}

// --- T1 pool lifecycle ---

// TestInvalidateGroupsPoolLifecycle drives the step-wise identity-pool
// conversion through delete→re-add, codex→non-codex→codex and K change→change
// back, keeping an in-flight busy slot to prove departed pools are never
// resurrected and identity continuity is preserved on resize.
func TestInvalidateGroupsPoolLifecycle(t *testing.T) {
	a := codexAcc(1, domain.FormatOpenAIResponses, "gpt-5", 2, "inst-1")
	ldr := newT1Loader(map[int64][]*domain.Account{10: {a}})
	s := New(testCfg(), ldr, newTestRuleEngine(t), nil, nil, nil, nil)
	require.NoError(t, s.reload(context.Background()))

	pool1 := s.publisher.pending.identityPools.pools[1]
	require.NotNil(t, pool1)
	require.Len(t, pool1.slots, 2)

	// In-flight slot on the original pool.
	require.True(t, pool1.slots[0].busy.CompareAndSwap(false, true))

	// Delete the codex account → its pool is gone.
	ldr.mu.Lock()
	ldr.byGroup[10] = nil
	ldr.mu.Unlock()
	s.InvalidateGroups([]int64{10})
	require.Nil(t, s.publisher.pending.identityPools.pools[1], "departed account's pool must GC")

	// Re-add → a fresh pool, never the old (still-busy) one.
	ldr.mu.Lock()
	ldr.byGroup[10] = []*domain.Account{a}
	ldr.mu.Unlock()
	s.InvalidateGroups([]int64{10})
	pool2 := s.publisher.pending.identityPools.pools[1]
	require.NotNil(t, pool2)
	require.NotSame(t, pool1, pool2, "a re-added account must not reuse the departed pool")

	// K change 2 → 1 then back 2 resizes the pool slice.
	a.MaxConcurrency = 1
	s.InvalidateGroups([]int64{10})
	require.Len(t, s.publisher.pending.identityPools.pools[1].slots, 1)
	a.MaxConcurrency = 2
	s.InvalidateGroups([]int64{10})
	require.Len(t, s.publisher.pending.identityPools.pools[1].slots, 2)

	// codex → non-codex → codex drops and recreates the pool.
	nonCodex := acc(1, tpl(1, domain.FormatOpenAIResponses, []string{"gpt-5"}), 2)
	ldr.mu.Lock()
	ldr.byGroup[10] = []*domain.Account{nonCodex}
	ldr.mu.Unlock()
	s.InvalidateGroups([]int64{10})
	require.Nil(t, s.publisher.pending.identityPools.pools[1], "non-codex account has no pool")

	ldr.mu.Lock()
	ldr.byGroup[10] = []*domain.Account{a}
	ldr.mu.Unlock()
	s.InvalidateGroups([]int64{10})
	require.NotNil(t, s.publisher.pending.identityPools.pools[1], "codex account gets a pool again")
}

// --- T2: delete-group (old :771) and clone (old :826) leaf-key paths ---

// TestInvalidateGroupLeafKeysMatchFrozenOracle exercises the retained-strip path
// (invalidate_batch.go:174 — an account REMOVED from the reloaded group but
// still retained in another group), asserting the new leaf's cached planKey
// equals the frozen oracle over its own (stripped) groupIDs, that it is a NEW
// immutable leaf sharing the old runtime, and that the other group points at it.
func TestInvalidateGroupLeafKeysMatchFrozenOracle(t *testing.T) {
	tp := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	a1 := acc(1, tp, 4) // shared G1+G2
	a2 := acc(2, tp, 4) // G1 only
	a3 := acc(3, tp, 4) // G2 only
	ldr := newT1Loader(map[int64][]*domain.Account{1: {a1, a2}, 2: {a1, a3}})
	s := New(testCfg(), ldr, newTestRuleEngine(t), nil, nil, nil, nil)
	require.NoError(t, s.reload(context.Background()))

	oldA1 := s.publisher.pending.byID[1]
	oldLeaf := oldA1
	require.ElementsMatch(t, []int64{1, 2}, oldLeaf.static.Load().groupIDs)
	rtA1 := oldLeaf.runtime

	// G1 loses a1 (a1 REMAINS in G2 → retained strip) and gains a4.
	a4 := acc(4, tp, 4)
	ldr.mu.Lock()
	ldr.byGroup[1] = []*domain.Account{a2, a4}
	ldr.mu.Unlock()
	s.InvalidateGroups([]int64{1})

	sv := s.publisher.pending
	require.NotNil(t, sv)

	// a1 removed from G1 but retained → NEW leaf, only G2, sharing runtime.
	st1 := sv.byID[1].static.Load()
	require.Equal(t, []int64{2}, st1.groupIDs, "a1 removed from G1 must retain only G2 (retained-strip branch)")
	require.NotSame(t, oldLeaf, sv.byID[1], "the strip must build a new immutable leaf (not clone in place)")
	require.Same(t, rtA1, sv.byID[1].runtime, "the strip must preserve the shared runtime")
	require.Equal(t, frozenPlanKey(*a1, a1.Template, []int64{2}), planKeyOf(st1),
		"the stripped leaf key must equal the frozen oracle over its own groupIDs")

	// a2 still in G1; a4 added.
	require.Equal(t, []int64{1}, sv.byID[2].static.Load().groupIDs)
	require.Equal(t, []int64{1}, sv.byID[4].static.Load().groupIDs)
	// Remaining retained leaves' keys also match the frozen oracle.
	for id, a := range map[int64]*domain.Account{2: a2, 3: a3, 4: a4} {
		st := sv.byID[id].static.Load()
		require.Equal(t, frozenPlanKey(*a, a.Template, st.groupIDs), planKeyOf(st),
			"leaf %d cached key must equal the frozen oracle", id)
	}
	// G2 must now reference a1's NEW retained leaf (shared-instance discipline).
	var g2HasA1 bool
	for _, as := range sv.groups[2].accounts {
		if as.static.Load().acc.ID == 1 {
			require.Same(t, sv.byID[1], as, "G2 must point at the retained a1 leaf")
			g2HasA1 = true
		}
	}
	require.True(t, g2HasA1, "G2 must still contain a1")
}

// --- T1: differential observables + independent full-rebuild oracle ---

// TestInvalidateGroupsDifferentialObservables locks the full observable set
// (groupIDs construction order, eventGID, shared runtime, staged root, armed
// compile) rather than only groupIDs.
func TestInvalidateGroupsDifferentialObservables(t *testing.T) {
	tp := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	a1 := acc(1, tp, 4)
	a2 := acc(2, tp, 4)
	a3 := acc(3, tp, 4)
	a4 := acc(4, tp, 4)
	ldr := newT1Loader(map[int64][]*domain.Account{1: {a1, a2}, 2: {a1, a3}})
	s := New(testCfg(), ldr, newTestRuleEngine(t), nil, nil, nil, nil)
	require.NoError(t, s.reload(context.Background()))
	wireSources(s, nil, nil)
	rt0 := s.publisher.pending.byID[1].runtime
	require.Equal(t, int64(1), s.publisher.pending.byID[1].static.Load().eventGID())

	ldr.mu.Lock()
	ldr.byGroup[1] = []*domain.Account{a1, a4}
	ldr.mu.Unlock()
	s.InvalidateGroups([]int64{2, 1})

	sv := s.publisher.pending
	require.Equal(t, []int64{2, 1}, sv.byID[1].static.Load().groupIDs, "construction order")
	require.Equal(t, []int64{1}, sv.byID[4].static.Load().groupIDs)
	require.Equal(t, []int64{2}, sv.byID[3].static.Load().groupIDs)
	require.Equal(t, int64(1), sv.byID[1].static.Load().eventGID(), "eventGID == min(groupIDs)")
	require.Same(t, rt0, sv.byID[1].runtime, "shared runtime must survive the batch")
	_, ok := sv.byID[2]
	require.False(t, ok, "removed account leaves byID")
	select {
	case <-s.compileCh:
	default:
		t.Fatal("a successful batch must request a compile")
	}
}

// TestInvalidateGroupsCanonicalBytesMatchFullRebuild uses an INDEPENDENT oracle:
// a full reload over the same final loader state, compiled and serialized. The
// batch's incremental result must produce byte-identical canonical decisions
// (groupIDs order is order-independent in the encoding) and the same topology.
func TestInvalidateGroupsCanonicalBytesMatchFullRebuild(t *testing.T) {
	tp := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	batchLdr := newT1Loader(map[int64][]*domain.Account{1: {acc(1, tp, 4), acc(2, tp, 4)}, 2: {acc(1, tp, 4), acc(3, tp, 4)}})
	bs := New(testCfg(), batchLdr, newTestRuleEngine(t), nil, nil, nil, nil)
	require.NoError(t, bs.reload(context.Background()))

	oracleLdr := newT1Loader(map[int64][]*domain.Account{1: {acc(1, tp, 4), acc(4, tp, 4)}, 2: {acc(1, tp, 4), acc(3, tp, 4)}})
	os := New(testCfg(), oracleLdr, newTestRuleEngine(t), nil, nil, nil, nil)
	require.NoError(t, os.reload(context.Background()))

	// Server change: G1 {a1,a2} -> {a1,a4}.
	batchLdr.mu.Lock()
	batchLdr.byGroup[1] = []*domain.Account{acc(1, tp, 4), acc(4, tp, 4)}
	batchLdr.mu.Unlock()
	bs.InvalidateGroups([]int64{2, 1})

	wireSources(bs, nil, nil)
	bs.compileOnce()
	wireSources(os, nil, nil)
	os.compileOnce()

	require.Equal(t, decisionViewBytes(os.View().DecisionView()), decisionViewBytes(bs.View().DecisionView()),
		"batch canonical bytes must equal an independent full-rebuild oracle")
	require.Equal(t, accountIDSet(os.View().ByID()), accountIDSet(bs.View().ByID()), "topology must match")
}

func accountIDSet(byID map[int64]*accountSnapshot) []int64 {
	ids := make([]int64, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// accountIDsOf returns the sorted account IDs of one group snapshot.
func accountIDsOf(gs *groupSnapshot) []int64 {
	if gs == nil {
		return nil
	}
	out := make([]int64, 0, len(gs.accounts))
	for _, as := range gs.accounts {
		out = append(out, as.static.Load().acc.ID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
