// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// TestRed_RedisAckRemovesExactlyPublished verifies blocker (1):
// successful Redis publication must ACK/remove exactly the published minuteAbs state,
// while retaining newer contributions; historical minutes must drain once.
func TestRed_RedisAckRemovesExactlyPublished(t *testing.T) {
	mr, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	base := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-ack1", BatchSize: 10}, nil, nil)
	w.SetClock(func() time.Time { return base })

	k1 := keyOf(fp(101), qc(101))
	cell1 := rec.GetOrCreateCell(k1)
	var ac1 AttemptContext
	require.True(t, rec.InitAttemptContext(cell1, &ac1))
	tt := int64(100)
	ac1.Complete(true, &tt, 5, 1, 0)

	// first publish should drain minute 12:00
	w.doRedis(context.Background())
	// barrier: ensure redis got it
	require.GreaterOrEqual(t, len(mr.Keys()), 1)
	w.mu.Lock()
	_, hasOld := w.minuteAbs[base.Unix()]
	w.mu.Unlock()
	require.False(t, hasOld, "successful publish must ACK/remove minuteAbs for published minute")

	// second publish without new data must not republish (no keys growth for same minute)
	keysBefore := len(mr.Keys())
	w.doRedis(context.Background())
	keysAfter := len(mr.Keys())
	require.Equal(t, keysBefore, keysAfter, "historical minute must drain once and not republish forever")
	w.mu.Lock()
	_, hasAgain := w.minuteAbs[base.Unix()]
	w.mu.Unlock()
	require.False(t, hasAgain, "second empty publish must keep minuteAbs empty")

	// newer contribution for same minute after ack must be retained and published once
	cell1b := rec.GetOrCreateCell(k1)
	var ac2 AttemptContext
	require.True(t, rec.InitAttemptContext(cell1b, &ac2))
	ac2.Complete(true, &tt, 7, 1, 0)
	w.doRedis(context.Background())
	w.mu.Lock()
	_, hasAfterNew := w.minuteAbs[base.Unix()]
	w.mu.Unlock()
	require.False(t, hasAfterNew, "newer contribution must be published and then acked")

	// verify newer contribution not lost: need second minute distinct
	baseNext := base.Add(time.Minute)
	w.SetClock(func() time.Time { return baseNext })
	// create historical minute 12:00 again with new key that was not acked? Actually test historical drain
	kHist := keyOf(fp(102), qc(102))
	// simulate historical minuteAbs entry for base (old minute) that should have been drained
	// we already verified historical drain, now test multiple minutes drain
	// Prepare two minutes worth of data: one for baseNext and one historical
	// Use barrier to ensure both drain
	rec2, _ := NewRecorder(50000)
	w2 := NewSyncWorker(rec2, rdb, pg, SyncConfig{InstanceSrc: "red-ack2", BatchSize: 10}, nil, nil)
	// Use active path: create cells for both minutes via clock tricks
	w2.SetClock(func() time.Time { return base })
	cellH := rec2.GetOrCreateCell(kHist)
	var ach AttemptContext
	require.True(t, rec2.InitAttemptContext(cellH, &ach))
	ach.Complete(true, &tt, 3, 1, 0)
	w2.doRedis(context.Background())
	// advance to next minute and add new data
	w2.SetClock(func() time.Time { return baseNext })
	var acNext AttemptContext
	cell1b2 := rec2.GetOrCreateCell(k1)
	require.True(t, rec2.InitAttemptContext(cell1b2, &acNext))
	acNext.Complete(true, &tt, 4, 1, 0)
	w2.doRedis(context.Background())
	w2.mu.Lock()
	_, hasHistAfter := w2.minuteAbs[base.Unix()]
	_, hasNextAfter := w2.minuteAbs[baseNext.Unix()]
	w2.mu.Unlock()
	require.False(t, hasHistAfter, "historical minute must drain once")
	require.False(t, hasNextAfter, "current minute after success must be acked")
	_ = mr
	_ = rdb
	_ = redis.NewClient
	_ = miniredis.RunT
}

// TestRed_StartCloseConcurrentRaceFree verifies blocker (2):
// Start/Close concurrent lifecycle must be race-free and Close cannot return before a started loop is stopped.
func TestRed_StartCloseConcurrentRaceFree(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, _ := NewRecorder(50000)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-lifecycle", BatchSize: 10}, nil, nil)

	barrier := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	var startErr, closeErr error
	go func() {
		defer wg.Done()
		<-barrier
		startErr = w.Start(context.Background())
	}()
	go func() {
		defer wg.Done()
		<-barrier
		// Close with background that has timeout to detect hang
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		closeErr = w.Close(ctx)
	}()
	close(barrier)
	wg.Wait()
	// At most one should succeed in starting, close must not race
	if startErr != nil {
		require.True(t, containsAny(startErr.Error(), "already started", "closed"), "start error must be already started or closed, got %v", startErr)
	}
	require.NoError(t, closeErr)

	// If Start succeeded, Close must have waited for loop to stop: loopDoneCh must be closed
	select {
	case <-w.loopDoneCh:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Close returned before loopDoneCh closed - race: Close did not wait for started loop")
	}
	select {
	case <-w.loopDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Close returned before loopDone closed")
	}

	// Second Close must be idempotent and race-free
	require.NoError(t, w.Close(context.Background()))

	// Start after Close must fail (closed)
	err := w.Start(context.Background())
	require.Error(t, err)
}

// TestRed_CloseWaitsForStartedLoop ensures Close blocks until loop exits.
func TestRed_CloseWaitsForStartedLoop(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, _ := NewRecorder(50000)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-wait", BatchSize: 10}, nil, nil)
	require.NoError(t, w.Start(context.Background()))
	// loop should be running
	require.NotNil(t, w.loopDone)
	require.NotNil(t, w.loopDoneCh)
	done := make(chan error, 1)
	go func() {
		done <- w.Close(context.Background())
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
		// verify loop closed
		select {
		case <-w.loopDone:
		default:
			t.Fatal("loopDone not closed after Close")
		}
		select {
		case <-w.loopDoneCh:
		default:
			t.Fatal("loopDoneCh not closed after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after started loop - must wait with timeout, but should complete quickly")
	}
}
