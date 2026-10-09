// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"errors"
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
