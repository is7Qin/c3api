// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestRoutingViewSingleRoot(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl, 2)}})
	s := newSched(t, m)
	v1 := s.View()
	require.NotNil(t, v1, "view must exist after reload")
	gen1 := v1.Generation()
	byID1 := v1.ByID()
	groups1 := v1.Groups()
	require.NotNil(t, byID1)
	require.NotNil(t, groups1)

	m.mu.Lock()
	m.byGroup[10] = append(m.byGroup[10], acc(2, tpl, 2))
	m.mu.Unlock()
	s.InvalidateGroup(10)
	v2 := s.View()
	require.NotNil(t, v2)
	require.Greater(t, v2.Generation(), gen1, "generation must increase")
	require.Same(t, byID1[1], v2.ByID()[1], "identity-stable pointer for existing account")
}

func TestLeaseExactReleaseIdempotent(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tpl, UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4},
	})
	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	ri, _ := s.Runtime(1)
	require.Equal(t, int64(1), ri.Concurrency)

	sel.Release()
	ri, _ = s.Runtime(1)
	require.Equal(t, int64(0), ri.Concurrency, "first release decrements")

	sel.Release()
	ri, _ = s.Runtime(1)
	require.Equal(t, int64(0), ri.Concurrency, "second release no-op")
	require.GreaterOrEqual(t, ri.Concurrency, int64(0), "never negative")

	sel.Release()
	require.Equal(t, int64(0), ri.Concurrency, "third release still no-op")
}

func TestLeaseSameIDReaddExactObject(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl, 4)}})
	s := newSched(t, m)

	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID)
	oldLeaseAcc := sel.LeaseAccountForTest()

	m.mu.Lock()
	m.byGroup[10] = []*domain.Account{}
	m.mu.Unlock()
	s.InvalidateGroup(10)

	_, ok := s.Runtime(1)
	require.False(t, ok, "account removed from view")

	m.mu.Lock()
	m.byGroup[10] = []*domain.Account{acc(1, tpl, 4)}
	m.mu.Unlock()
	s.InvalidateGroup(10)
	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, int64(0), ri.Concurrency, "readded account starts 0")

	sel.Release()
	ri, _ = s.Runtime(1)
	require.Equal(t, int64(0), ri.Concurrency, "old lease release must not affect readded object")

	v := s.View()
	newAcc := v.ByID()[1]
	require.NotSame(t, oldLeaseAcc, newAcc, "readded account must be new object")

	sel2, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	ri, _ = s.Runtime(1)
	require.Equal(t, int64(1), ri.Concurrency)
	sel2.Release()
	ri, _ = s.Runtime(1)
	require.Equal(t, int64(0), ri.Concurrency)
}

func TestLeaseDeletedInFlight(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl, 4)}})
	s := newSched(t, m)

	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)

	m.mu.Lock()
	m.byGroup[10] = []*domain.Account{}
	m.mu.Unlock()
	s.InvalidateGroup(10)

	require.NotPanics(t, func() { sel.Release() })
	v := s.View()
	_, ok := v.ByID()[1]
	require.False(t, ok)

	require.NotPanics(t, func() { sel.Release() })
}

func TestLeaseReloadRaceBarrier(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl, 1000)}})
	s := newSched(t, m)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.writebackLoop(ctx)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = s.reload(context.Background())
		}
	}()
	for i := 0; i < 500; i++ {
		sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
		if err != nil {
			continue
		}
		sel.Release()
		ri, _ := s.Runtime(1)
		require.GreaterOrEqual(t, ri.Concurrency, int64(0))
	}
	wg.Wait()
	ri, _ := s.Runtime(1)
	require.GreaterOrEqual(t, ri.Concurrency, int64(0))
	require.LessOrEqual(t, ri.Concurrency, int64(1000))
}

func TestRoutingViewAccSyncRuntimeGroupModelsSameRoot(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl, 4)}, 20: {acc(2, tpl, 4)}})
	s := newSched(t, m)
	v := s.View()
	require.NotNil(t, v)

	_, ok := s.GroupModels(10)
	require.True(t, ok)

	ri, ok := s.Runtime(1)
	require.True(t, ok, "Runtime must read same root view")
	_ = ri

	v2 := s.View()
	require.Equal(t, v.Generation(), v2.Generation(), "same root for concurrent reads")
}

func TestLeasePanicEarlyPath(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tpl, UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4},
	})
	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)

	func() {
		defer func() {
			_ = recover()
			sel.Release()
		}()
		panic("simulated panic")
	}()

	ri, _ := s.Runtime(1)
	require.Equal(t, int64(0), ri.Concurrency)
	sel.Release()
	ri, _ = s.Runtime(1)
	require.Equal(t, int64(0), ri.Concurrency)
}
