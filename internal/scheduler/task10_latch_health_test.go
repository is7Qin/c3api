// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/pkg/redisx"
)

func newTestHealthWithLatch(t *testing.T) (*RuntimeHealth, *latchStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })
	h := NewRuntimeHealth(c, "self-a", nil, nil, nil)
	ls := newLatchStore()
	return h, ls, mr
}

func TestThrottleAccountWildcard(t *testing.T) {
	h, _, mr := newTestHealthWithLatch(t)
	_ = mr
	ctrl := NewHealthController(h, newLatchStore())
	ev := rule.Event{AccountID: 1, ExpectedRevision: 5, RouteClassID: "r1", QualityClassID: "q1"}
	th := domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: int64Ptr(5000), UseReset: false}
	ctrl.Throttle(ev, th)
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateOPEN, h.EffectiveState(1, "any", 5))
	require.Equal(t, StateOPEN, h.EffectiveState(1, "q1", 5))
	require.Equal(t, StateOPEN, h.EffectiveState(1, "other", 5))
	require.Equal(t, StateReady, h.EffectiveState(1, "any", 6))
}

func TestThrottleAccountRouteRequiresIDsAndPropagation(t *testing.T) {
	h, _, _ := newTestHealthWithLatch(t)
	ctrl := NewHealthController(h, newLatchStore())
	th := domain.ThrottleAction{Scope: domain.ThrottleScopeAccountRoute, Mode: domain.ThrottleModeOpen, DurationMs: int64Ptr(5000), UseReset: false}
	ctrl.Throttle(rule.Event{AccountID: 2, ExpectedRevision: 3, RouteClassID: "", QualityClassID: "q1"}, th)
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateReady, h.EffectiveState(2, "q1", 3))
	ctrl.Throttle(rule.Event{AccountID: 2, ExpectedRevision: 3, RouteClassID: "r1", QualityClassID: ""}, th)
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateReady, h.EffectiveState(2, "q1", 3))
	ctrl.Throttle(rule.Event{AccountID: 2, ExpectedRevision: 3, RouteClassID: "r1", QualityClassID: "q1"}, th)
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateOPEN, h.EffectiveState(2, "q1", 3))
	require.Equal(t, StateReady, h.EffectiveState(2, "other", 3))
	require.Equal(t, StateReady, h.EffectiveState(2, "*", 3))
}

func TestLatchFailClosedAndRevisionFence(t *testing.T) {
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := New(testCfg(), m, re, nil)
	require.NoError(t, s.reload(context.Background()))
	ls := s.LatchStore()
	require.NotNil(t, ls)
	ctrl := NewHealthControllerWithScheduler(nil, s)
	ev := rule.Event{AccountID: 1, ExpectedRevision: 1, RouteClassID: "r1", QualityClassID: "q1", ErrorMessage: "boom"}
	ctrl.FailAccount(ev)
	require.True(t, ls.IsLatched(1, accountFingerprint(m.byGroup[10][0])))
	_, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable)
	// same revision reload must not clear latch
	require.NoError(t, s.reload(context.Background()))
	require.True(t, ls.IsLatched(1, accountFingerprint(m.byGroup[10][0])))
	_, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable)
	// new revision clears latch
	m.byGroup[10][0].LifecycleRevision = 2
	require.NoError(t, s.reload(context.Background()))
	require.False(t, ls.IsLatched(1, accountFingerprint(m.byGroup[10][0])))
	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID)
	s.Release(sel.AccountID)
}

func TestLatchFingerprintAndRemoveReaddFence(t *testing.T) {
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := New(testCfg(), m, re, nil)
	require.NoError(t, s.reload(context.Background()))
	ls := s.LatchStore()
	ctrl := NewHealthControllerWithScheduler(nil, s)
	ev := rule.Event{AccountID: 1, ExpectedRevision: 1, ErrorMessage: "boom"}
	ctrl.FailAccount(ev)
	fp1 := accountFingerprint(m.byGroup[10][0])
	require.True(t, ls.IsLatched(1, fp1))
	// fingerprint change clears old latch
	m.byGroup[10][0].UpstreamKey = "new-key"
	require.NoError(t, s.reload(context.Background()))
	require.False(t, ls.IsLatched(1, fp1))
	require.False(t, ls.IsLatched(1, accountFingerprint(m.byGroup[10][0])))
	// re-latch with new fingerprint
	ev2 := rule.Event{AccountID: 1, ExpectedRevision: 2, ErrorMessage: "boom2"}
	// update revision to 2 to match new account revision? need set revision
	m.byGroup[10][0].LifecycleRevision = 2
	require.NoError(t, s.reload(context.Background()))
	ctrl.FailAccount(ev2)
	fp2 := accountFingerprint(m.byGroup[10][0])
	require.True(t, ls.IsLatched(1, fp2))
	// remove account clears latch
	delete(m.byGroup, 10)
	require.NoError(t, s.reload(context.Background()))
	require.False(t, ls.IsLatched(1, fp2))
	// re-add same ID with new revision should not be latched
	m.byGroup[10] = []*domain.Account{acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}
	m.byGroup[10][0].LifecycleRevision = 5
	require.NoError(t, s.reload(context.Background()))
	require.False(t, ls.IsLatched(1, accountFingerprint(m.byGroup[10][0])))
	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	s.Release(sel.AccountID)
}

func TestHealthControllerProbeAndEffectiveStateWithLatch(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })
	h := NewRuntimeHealth(c, "self-a", nil, nil, nil)
	ls := newLatchStore()
	ctrl := NewHealthController(h, ls)
	// barrier for concurrent throttle and select
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := New(testCfg(), m, re, nil)
	s.latch = ls
	s.health = h
	require.NoError(t, s.reload(context.Background()))
	th := domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: int64Ptr(3000), UseReset: false}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ctrl.Throttle(rule.Event{AccountID: 1, ExpectedRevision: 1}, th)
	}()
	go func() {
		defer wg.Done()
		_, _ = s.Select(10, domain.FormatOpenAIChat, "m")
	}()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "barrier timeout")
	}
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateOPEN, h.EffectiveState(1, "*", 1))
}

func int64Ptr(v int64) *int64 { return &v }
