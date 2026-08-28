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

func (b *barrierLoader) UpdateAccountStatus(ctx context.Context, id int64, s domain.AccountStatus, cooldown *time.Time, lastErr *string, weight *int) error {
	return b.inner.UpdateAccountStatus(ctx, id, s, cooldown, lastErr, weight)
}

func TestInvalidateGroupSerializesLoadBarrier(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl, 4)}})
	s := newSched(t, m)

	v1 := s.View()
	require.NotNil(t, v1)
	if v1.DecisionView() == nil {
		s.publisher.publishWithBase(v1.Generation(), func(cur *RoutingView) *DecisionView {
			return &DecisionView{generation: 1, decisions: map[int64]*decisionLeaf{1: {weight: 10}}}
		})
		v1 = s.View()
	}
	dec1 := v1.DecisionView()
	require.NotNil(t, dec1)

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
	go func() {
		defer close(reloadDone)
		_ = s.reload(context.Background())
	}()
	go func() {
		defer close(decisionDone)
		s.publisher.publishWithBase(s.View().Generation(), func(cur *RoutingView) *DecisionView {
			var gen uint64
			if cur != nil && cur.DecisionView() != nil {
				gen = cur.DecisionView().Generation()
			}
			return &DecisionView{generation: gen + 1, decisions: map[int64]*decisionLeaf{1: {weight: 20}}}
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

	vFinal := s.View()
	require.NotNil(t, vFinal)
	require.NotNil(t, vFinal.StaticView())
	_, has2 := vFinal.ByID()[2]
	require.True(t, has2, "no stale static overwrite: fresh account 2 must be present after serialized reload")
	grp10 := vFinal.Groups()[10]
	require.NotNil(t, grp10)
	found2 := false
	for _, a := range grp10.accounts {
		if a.static.Load().acc.ID == 2 {
			found2 = true
			break
		}
	}
	require.True(t, found2, "group 10 must contain fresh account after serialized reload")
	decFinal := vFinal.DecisionView()
	require.NotNil(t, decFinal)
	require.NotNil(t, decFinal.decisions[1])
	require.Equal(t, 20, decFinal.decisions[1].weight, "latest DecisionView retained")
	require.Greater(t, vFinal.Generation(), v1.Generation())
}
