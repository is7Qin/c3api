// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/latch"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/pkg/redisx"
)

func TestHealthControllerThrottlePropagatesRedisError(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })
	h := NewRuntimeHealth(c, "self-a", nil, nil)
	sink := NewLatchSink(h, latch.NewLatchStore(), latch.NewHub())
	th := domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: int64Ptr(5000)}
	ev := rule.Event{AccountID: 1, ExpectedIdentityRevision: 1, RouteClassID: "r1", QualityClassID: "q1"}
	// barrier: ensure Redis error propagates via returned error, not discarded
	mr.Close()
	err = sink.Throttle(ev, th)
	require.Error(t, err, "throttle must propagate Redis error, never discard")
	// local fail-closed: after Redis error, sync view must not be assumed succeeded
	// EffectiveState still READY because Sync failed, but throttle call returned error observable
}

func TestHealthControllerThrottleSuccessWithBarrier(t *testing.T) {
	mr, c := newHealthTestRedis(t)
	_ = mr
	h := NewRuntimeHealth(c, "self-a", nil, nil)
	sink := NewLatchSink(h, latch.NewLatchStore(), latch.NewHub())
	th := domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: int64Ptr(3000)}
	done := make(chan error, 1)
	go func() {
		done <- sink.Throttle(rule.Event{AccountID: 9, ExpectedIdentityRevision: 1}, th)
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "barrier timeout: Throttle did not return")
	}
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateOPEN, h.EffectiveState(9, "*", 1))
}
