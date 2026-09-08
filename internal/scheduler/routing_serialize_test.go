// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

type barrierLoader struct {
	inner       Loader
	block       chan struct{}
	started     chan struct{}
	targetGroup int64
	stale       []*domain.Account
}

func (b *barrierLoader) LoadGroupsAccounts(ctx context.Context) (map[int64][]*domain.Account, error) {
	return b.inner.LoadGroupsAccounts(ctx)
}

func (b *barrierLoader) LoadGroupAccounts(ctx context.Context, id int64) ([]*domain.Account, error) {
	if id == b.targetGroup {
		select {
		case b.started <- struct{}{}:
		default:
		}
		<-b.block
		if b.stale != nil {
			return b.stale, nil
		}
	}
	return b.inner.LoadGroupAccounts(ctx, id)
}

// TestInvalidateGroupSerializesLoadBarrier 验证 publisher.mu 串行化：组级重载
// （阻塞在 loader 加载）与全量 reload + 决策发布并发时，静态视图不被陈旧数据
// 覆盖，且最新 DecisionView 被保留（structurally shared root）。
func TestInvalidateGroupSerializesLoadBarrier(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl, 4)}})
	s := newSched(t, m)

	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	v1 := s.View()
	require.NotNil(t, v1)
	require.NotNil(t, v1.DecisionView(), "setup publishes a complete pair")

	staleAccs := []*domain.Account{acc(1, tpl, 4)}
	block := make(chan struct{})
	started := make(chan struct{}, 1)
	bl := &barrierLoader{
		inner:       m,
		block:       block,
		started:     started,
		targetGroup: 10,
		stale:       staleAccs,
	}
	s.loader = bl

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.InvalidateGroup(10)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("InvalidateGroup did not reach barrier")
	}

	m.mu.Lock()
	m.byGroup[10] = []*domain.Account{acc(1, tpl, 4), acc(2, tpl, 4)}
	m.mu.Unlock()

	reloadDone := make(chan struct{})
	decisionDone := make(chan struct{})
	base := s.View()
	go func() {
		defer close(reloadDone)
		_ = s.reload(context.Background())
	}()
	go func() {
		defer close(decisionDone)
		s.publisher.publishWithBase(base.Generation(), base.StaticView(), func(cur *RoutingView) *DecisionView {
			return &DecisionView{routes: map[RouteRef]*RouteDecision{route: {Primary: ccPrimary(1, 2)}}}
		})
	}()

	close(block)
	wg.Wait()

	select {
	case <-reloadDone:
	case <-time.After(2 * time.Second):
		t.Fatal("reload did not complete")
	}
	select {
	case <-decisionDone:
	case <-time.After(2 * time.Second):
		t.Fatal("decision publish did not complete")
	}

	s.loader = m

	// Atomic publication: the stale group load only stages (never published);
	// the racy decision-only publish still rebases onto the current static
	// (same root, new generation), and the fresh full reload stays staged as
	// pending — the old static is never torn or overwritten by stale data.
	vFinal := s.View()
	require.NotNil(t, vFinal)
	require.Same(t, v1.StaticView(), vFinal.StaticView(), "staging must not touch the published static")
	require.NotNil(t, vFinal.StaticView())
	_, has2 := vFinal.ByID()[2]
	require.False(t, has2, "staged account 2 not yet published")
	_, ok := vFinal.DecisionView().routes[route]
	require.True(t, ok, "racy decision-only publish lands on the still-current static")
	s.publisher.mu.Lock()
	pending := s.publisher.pending
	s.publisher.mu.Unlock()
	require.NotNil(t, pending, "fresh reload stays staged as pending")
	_, has2 = pending.byID[2]
	require.True(t, has2, "no stale static overwrite: pending holds fresh account 2")

	// The pending root pairs on the next compile: generations match, leaves
	// point into the new static.
	s.compileOnce()
	paired := s.View()
	require.NotSame(t, v1, paired)
	require.Equal(t, paired.Generation(), paired.StaticView().Generation(), "one generation for the pair")
	require.Equal(t, paired.Generation(), paired.DecisionView().Generation(), "one generation for the pair")
	_, has2 = paired.ByID()[2]
	require.True(t, has2, "paired publish carries fresh account 2")
	grp10 := paired.Groups()[10]
	require.NotNil(t, grp10)
	found2 := false
	for _, a := range grp10.accounts {
		if a.static.Load().acc.ID == 2 {
			found2 = true
			break
		}
	}
	require.True(t, found2, "group 10 must contain fresh account after paired publish")
}
