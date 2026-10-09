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

// TestPartialSuccessKeepsStickyObligation covers failure convergence (2): a
// successful unrelated group in the same batch must not clear the obligation
// set by a failing group.
func TestPartialSuccessKeepsStickyObligation(t *testing.T) {
	tp := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	ldr := newT1Loader(map[int64][]*domain.Account{
		1: {acc(1, tp, 4)},
		2: {acc(2, tp, 4)},
	})
	s := New(testCfg(), ldr, newTestRuleEngine(t), nil, nil, nil, nil)
	require.NoError(t, s.reload(context.Background()))
	before := s.publisher.pending

	ldr.mu.Lock()
	ldr.failGroup[1] = true
	ldr.byGroup[2] = []*domain.Account{acc(2, tp, 4), acc(3, tp, 4)}
	ldr.mu.Unlock()

	s.InvalidateGroups([]int64{1, 2})
	require.True(t, s.reloadRequired, "a partial failure must keep the sticky obligation")
	require.NotSame(t, before, s.publisher.pending, "the successful group must still stage")
	_, ok := s.publisher.pending.byID[3]
	require.True(t, ok, "the successful group's change must be folded in")
}

// TestStickySurvivesRepeatedFailuresThenClears covers failure convergence (3):
// a repeated full-load failure keeps the obligation; the next success clears it.
func TestStickySurvivesRepeatedFailuresThenClears(t *testing.T) {
	tp := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	ldr := newT1Loader(map[int64][]*domain.Account{10: {acc(1, tp, 4)}})
	probe := &fakeStalenessProbe{counts: domain.CompileStaleness{Accounts: 1, Groups: 1, Templates: 1}}
	s := New(Config{SyncInterval: time.Hour, StalenessProbe: probe}, ldr, newTestRuleEngine(t), nil, nil, nil, nil)
	require.NoError(t, s.reload(context.Background()))
	wireSources(s, nil, nil)
	s.compileOnce()

	ldr.mu.Lock()
	ldr.failFull = true
	ldr.mu.Unlock()
	require.Error(t, s.InvalidateAllSyncCtx(context.Background()))
	require.True(t, s.reloadRequired)
	ldr.reset()
	s.backstopTick(context.Background())
	require.Equal(t, 1, ldr.fullCount())
	require.True(t, s.reloadRequired, "a repeated full-load failure keeps the obligation")

	ldr.mu.Lock()
	ldr.failFull = false
	ldr.mu.Unlock()
	ldr.reset()
	s.backstopTick(context.Background())
	require.Equal(t, 1, ldr.fullCount())
	require.False(t, s.reloadRequired, "a successful full reload clears the obligation")
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

// TestInvalidateGroupsBatchDeadlineReleasesLock proves the shared batch deadline
// fires: a load that waits on ctx.Done returns, the batch leaves the failure
// branch (sticky obligation set) and publisher.mu is released.
func TestInvalidateGroupsBatchDeadlineReleasesLock(t *testing.T) {
	orig := batchStageTimeout
	batchStageTimeout = 50 * time.Millisecond
	defer func() { batchStageTimeout = orig }()

	tp := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	base := newT1Loader(map[int64][]*domain.Account{10: {acc(1, tp, 4)}})
	ldr := &deadlineLoader{t1Loader: base, blockGroup: 10, entered: make(chan struct{}, 1)}
	s := New(testCfg(), ldr, newTestRuleEngine(t), nil, nil, nil, nil)
	require.NoError(t, s.reload(context.Background()))

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

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("batch did not return after the shared deadline")
	}
	require.True(t, s.reloadRequired, "a timed-out batch must set the sticky obligation")

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

// TestInvalidateGroupLeafKeysMatchFrozenOracle exercises both the delete-group
// strip path (an account removed from one group but retained in another) and the
// clone/replace path (a still-present account getting new groupIDs), asserting
// each new leaf's cached planKey equals the frozen oracle over its own groupIDs.
func TestInvalidateGroupLeafKeysMatchFrozenOracle(t *testing.T) {
	tp := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	a1 := acc(1, tp, 4) // shared G1+G2
	a2 := acc(2, tp, 4) // G1 only
	a3 := acc(3, tp, 4) // G2 only
	ldr := newT1Loader(map[int64][]*domain.Account{1: {a1, a2}, 2: {a1, a3}})
	s := New(testCfg(), ldr, newTestRuleEngine(t), nil, nil, nil, nil)
	require.NoError(t, s.reload(context.Background()))

	// G1 loses a1 (strip path) and gains a4 (clone path: a1 still in G2).
	a4 := acc(4, tp, 4)
	ldr.mu.Lock()
	ldr.byGroup[1] = []*domain.Account{a1, a4}
	ldr.mu.Unlock()
	s.InvalidateGroups([]int64{1})

	sv := s.publisher.pending
	require.NotNil(t, sv)

	// a2 removed everywhere → gone.
	_, ok := sv.byID[2]
	require.False(t, ok, "account removed from every group must leave byID")

	// Every retained leaf's key matches the frozen oracle over its own groupIDs.
	for id, acc := range map[int64]*domain.Account{1: a1, 3: a3, 4: a4} {
		st := sv.byID[id].static.Load()
		require.Equal(t, frozenPlanKey(*acc, acc.Template, st.groupIDs), planKeyOf(st),
			"leaf %d cached key must equal the frozen oracle", id)
	}
	require.Equal(t, []int64{1, 2}, sv.byID[1].static.Load().groupIDs, "shared account groupIDs follow construction order (gid first, then others)")
	require.Equal(t, []int64{1}, sv.byID[4].static.Load().groupIDs)
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
