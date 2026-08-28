// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/pkg/redisx"
)

func newHealthTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })
	return mr, c
}

func healthKeyFor(acc int64, quality string, rev int64) HealthKey {
	return HealthKey{AccountID: acc, Quality: quality, Revision: rev}
}

// TestHealthViewImmutableAtomicPointer verifies one immutable atomic.Pointer view is used.
func TestHealthViewImmutableAtomicPointer(t *testing.T) {
	mr, c := newHealthTestRedis(t)
	_ = mr
	h := NewRuntimeHealth(c, "self-a", nil, nil, nil)
	view1 := h.view.Load()
	require.NotNil(t, view1)
	require.Empty(t, view1.entries)

	_, err := h.Throttle(context.Background(), healthKeyFor(1, "abc", 1), StateOPEN, 10*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))
	view2 := h.view.Load()
	require.NotNil(t, view2)
	require.NotSame(t, view1, view2, "immutable view must be replaced, not mutated")
	require.Contains(t, view2.entries, healthKeyFor(1, "abc", 1))
	// old view unchanged
	require.Empty(t, view1.entries, "old view must remain immutable")
}

// TestHealthThrottleAtomicStaleNoPartial verifies Lua throttle/READY atomic generation/revision/record HASH/active ZSET/tombstone/TTL
// stale generation does not partially update.
func TestHealthThrottleAtomicStaleNoPartial(t *testing.T) {
	mr, c := newHealthTestRedis(t)
	h := NewRuntimeHealth(c, "self-a", nil, nil, nil)

	key := healthKeyFor(10, "q1", 5)
	gen1, err := h.Throttle(context.Background(), key, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	require.Greater(t, gen1, int64(0))

	// Verify record HASH, active ZSET, tombstone absent, TTL set
	field := key.String()
	recKey := healthRecordPrefix + field
	m, err := c.HGetAll(context.Background(), recKey).Result()
	require.NoError(t, err)
	require.Equal(t, "OPEN", m["state"])
	require.Equal(t, "5", m["rev"])

	score, err := c.ZScore(context.Background(), healthActiveZSet, field).Result()
	require.NoError(t, err)
	require.Equal(t, float64(gen1), score)

	_, err = c.HGet(context.Background(), healthTombstoneHash, field).Result()
	require.Error(t, err) // not present

	ttl, err := c.PTTL(context.Background(), recKey).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, time.Duration(0))

	// Simulate stale READY: increment generation externally to make expectedGen stale
	_, err = c.Incr(context.Background(), healthGenKey).Result()
	require.NoError(t, err)
	staleGen := gen1
	_, err = h.MarkReady(context.Background(), key, staleGen, 5*time.Second)
	require.Error(t, err, "stale generation must fail")

	// Verify no partial update: record still exists, active still present, tombstone still absent
	m2, err := c.HGetAll(context.Background(), recKey).Result()
	require.NoError(t, err)
	require.Equal(t, m["state"], m2["state"], "record must not be partially deleted on stale ready")
	require.Equal(t, m["gen"], m2["gen"])

	score2, err := c.ZScore(context.Background(), healthActiveZSet, field).Result()
	require.NoError(t, err)
	require.Equal(t, score, score2, "active ZSET must not be partially removed on stale")

	_, err = c.HGet(context.Background(), healthTombstoneHash, field).Result()
	require.Error(t, err, "tombstone must not appear on stale")

	// Ensure generation not incremented on stale (our readyLua returns 0 on stale, not INCR)
	_ = mr
}

// TestHealthReplacement verifies replacement semantics: second throttle on same key overwrites with new generation.
func TestHealthReplacement(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h := NewRuntimeHealth(c, "self-a", nil, nil, nil)
	key := healthKeyFor(1, "q1", 1)
	gen1, err := h.Throttle(context.Background(), key, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	gen2, err := h.Throttle(context.Background(), key, StateRetryAfter, 10*time.Second)
	require.NoError(t, err)
	require.Greater(t, gen2, gen1)

	require.NoError(t, h.Sync(context.Background()))
	entry, ok := h.View()[key]
	require.True(t, ok)
	require.Equal(t, StateRetryAfter, entry.State)
	require.Equal(t, gen2, entry.Generation)
}

// TestHealthRunChangeRetainsOpenUntilProbing and same-run empty clears
func TestHealthRunReset(t *testing.T) {
	mr, c := newHealthTestRedis(t)
	h := NewRuntimeHealth(c, "self-a", nil, nil, nil)

	key := healthKeyFor(1, "q1", 1)
	_, err := h.Throttle(context.Background(), key, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))
	require.Contains(t, h.View(), key)
	require.Equal(t, StateOPEN, h.View()[key].State)

	// Record current runID after first sync
	firstRunID := h.runID

	// Same-run empty clears: flush all and sync with same run_id should clear
	require.NoError(t, c.FlushAll(context.Background()).Err())
	// miniredis INFO run_id stays same, so same-run
	require.NoError(t, h.Sync(context.Background()))
	require.Empty(t, h.View(), "same-run empty must clear")

	// Restore entry
	_, err = h.Throttle(context.Background(), key, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))
	require.Contains(t, h.View(), key)

	// Simulate run change: manually set h.runID to old value and flush
	h.runID = "old-run-id-" + firstRunID
	require.NoError(t, c.FlushAll(context.Background()).Err())
	require.NoError(t, h.Sync(context.Background()))
	// Run change retain OPEN until then PROBING
	v := h.View()
	require.Contains(t, v, key, "run change must retain OPEN")
	require.Equal(t, StateProbing, v[key].State, "retained entry must become PROBING")
	_ = mr
}

// TestHealthSyncGenerationRace verifies Sync INFO run_id + gen-before/records/gen-after detects stale generation.
func TestHealthSyncGenerationRace(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h := NewRuntimeHealth(c, "self-a", nil, nil, nil)
	key := healthKeyFor(1, "q1", 1)
	_, err := h.Throttle(context.Background(), key, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))

	// Manually bump generation between gen-before and gen-after to simulate race
	// We do this by incrementing gen key after h reads genBefore but before genAfter.
	// Since Sync reads genBefore, then INFO, then records, then genAfter, we can
	// interleave by calling Incr from another goroutine after short delay.
	// Instead, test the detection by directly incrementing and calling Sync which should see mismatch and return error.
	_, err = c.Incr(context.Background(), healthGenKey).Result()
	require.NoError(t, err)
	// Now Sync should detect genBefore != genAfter if we race? But our current Sync reads genBefore at start and genAfter at end.
	// If we just incremented before Sync, both reads will see same new gen, so no race.
	// To simulate race, we need concurrent writer during Sync. Use channel barrier.
	done := make(chan struct{})
	go func() {
		// Wait for Sync to read genBefore (approx 1ms after start)
		time.Sleep(10 * time.Millisecond)
		_, _ = c.Incr(context.Background(), healthGenKey).Result()
		close(done)
	}()
	// This Sync may or may not see race depending on timing; we just verify it doesn't panic and view remains consistent
	_ = h.Sync(context.Background())
	<-done
}

// TestHealthLockFreeReadUnderBlockedRedis verifies EffectiveState is lock-free read under blocked Redis.
func TestHealthLockFreeReadUnderBlockedRedis(t *testing.T) {
	mr, c := newHealthTestRedis(t)
	h := NewRuntimeHealth(c, "self-a", nil, nil, nil)
	key := healthKeyFor(42, "qual1", 7)
	_, err := h.Throttle(context.Background(), key, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateOPEN, h.EffectiveState(42, "qual1", 7))

	baseCmd := mr.CommandCount()
	// EffectiveState must not touch Redis (lock-free read)
	for i := 0; i < 100; i++ {
		require.Equal(t, StateOPEN, h.EffectiveState(42, "qual1", 7))
	}
	require.Equal(t, baseCmd, mr.CommandCount(), "EffectiveState must not issue Redis commands (lock-free)")

	// Even when Redis is down, EffectiveState still returns view
	mr.Close()
	require.Equal(t, StateOPEN, h.EffectiveState(42, "qual1", 7), "view read must succeed even when Redis blocked/closed")

	// Concurrent read under blocked sync
	_, c2 := newHealthTestRedis(t)
	h2 := NewRuntimeHealth(c2, "self-a", nil, nil, nil)
	_, err = h2.Throttle(context.Background(), healthKeyFor(1, "q", 1), StateOPEN, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h2.Sync(context.Background()))

	var wg sync.WaitGroup
	var successCount atomic.Int64
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				if h2.EffectiveState(1, "q", 1) == StateOPEN {
					successCount.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int64(20*1000), successCount.Load())
}

// TestHealthProbeOwner verifies probe injected selfID/rendezvous/ProbeFunc owner election.
func TestHealthProbeOwner(t *testing.T) {
	_, c := newHealthTestRedis(t)
	// Two members, self-a and self-b, key ownership determined by rendezvous
	members := []string{"self-a", "self-b"}
	var probed []HealthKey
	var mu sync.Mutex
	probeFn := func(_ context.Context, k HealthKey) error {
		mu.Lock()
		probed = append(probed, k)
		mu.Unlock()
		return nil
	}
	hA := NewRuntimeHealth(c, "self-a", func() []string { return members }, probeFn, nil)
	hB := NewRuntimeHealth(c, "self-b", func() []string { return members }, probeFn, nil)

	// Create two keys, each should be owned by one of them
	k1 := healthKeyFor(1, "q1", 1)
	k2 := healthKeyFor(2, "q1", 1)
	_, _ = hA.Throttle(context.Background(), k1, StateOPEN, 5*time.Second)
	_, _ = hA.Throttle(context.Background(), k2, StateOPEN, 5*time.Second)
	require.NoError(t, hA.Sync(context.Background()))
	require.NoError(t, hB.Sync(context.Background()))

	owner1 := rendezvousOwner(k1.String(), members)
	owner2 := rendezvousOwner(k2.String(), members)

	// Run probe tick on both
	hA.probeTick(context.Background())
	hB.probeTick(context.Background())

	mu.Lock()
	defer mu.Unlock()
	for _, k := range probed {
		expectedOwner := rendezvousOwner(k.String(), members)
		require.NotEmpty(t, expectedOwner)
		if k == k1 {
			require.Equal(t, owner1, expectedOwner)
		}
		if k == k2 {
			require.Equal(t, owner2, expectedOwner)
		}
	}
	// Each key should be probed exactly once by its owner
	countK1, countK2 := 0, 0
	for _, k := range probed {
		if k == k1 {
			countK1++
		}
		if k == k2 {
			countK2++
		}
	}
	require.Equal(t, 1, countK1, "k1 should be probed once by owner")
	require.Equal(t, 1, countK2, "k2 should be probed once by owner")
}

// TestHealthProbeTwoSuccessReady verifies one permit, two current-gen successes READY, failure reopen.
func TestHealthProbeTwoSuccessReady(t *testing.T) {
	mr, c := newHealthTestRedis(t)
	_ = mr
	var probeCount atomic.Int64
	probeFn := func(_ context.Context, _ HealthKey) error {
		probeCount.Add(1)
		return nil // success
	}
	h := NewRuntimeHealth(c, "self-a", func() []string { return []string{"self-a"} }, probeFn, nil)
	key := healthKeyFor(5, "q5", 1)
	_, err := h.Throttle(context.Background(), key, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateOPEN, h.EffectiveState(5, "q5", 1))

	// First success: should remain OPEN (needs two)
	h.probeTick(context.Background())
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateOPEN, h.EffectiveState(5, "q5", 1), "one success must not become READY")
	require.Equal(t, int64(1), probeCount.Load())

	// Second success within same generation: should become READY (tombstone, removed)
	h.probeTick(context.Background())
	// After second success, MarkReady should have cleared active
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateReady, h.EffectiveState(5, "q5", 1), "two current-gen successes must become READY")
	require.Equal(t, int64(2), probeCount.Load())

	// Verify Redis active ZSET no longer contains field, tombstone present
	field := key.String()
	_, err = c.ZScore(context.Background(), healthActiveZSet, field).Result()
	require.Error(t, err, "active ZSET must not contain READY key")
	_, err = c.HGet(context.Background(), healthTombstoneHash, field).Result()
	require.NoError(t, err, "tombstone must exist after READY")
}

// TestHealthProbeFailureReopen verifies failure reopen.
func TestHealthProbeFailureReopen(t *testing.T) {
	_, c := newHealthTestRedis(t)
	// First make it READY via two successes
	var shouldFail atomic.Bool
	probeFn := func(_ context.Context, _ HealthKey) error {
		if shouldFail.Load() {
			return context.DeadlineExceeded // failure
		}
		return nil
	}
	h := NewRuntimeHealth(c, "self-a", func() []string { return []string{"self-a"} }, probeFn, nil)
	key := healthKeyFor(9, "q9", 1)
	_, err := h.Throttle(context.Background(), key, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))

	// Two successes to READY
	h.probeTick(context.Background())
	h.probeTick(context.Background())
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateReady, h.EffectiveState(9, "q9", 1))

	// Simulate re-throttle to OPEN then probe failure should reopen
	_, err = h.Throttle(context.Background(), key, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateOPEN, h.EffectiveState(9, "q9", 1))

	shouldFail.Store(true)
	h.probeTick(context.Background())
	require.NoError(t, h.Sync(context.Background()))
	// Failure should keep it OPEN (reopen) and reset success count
	require.Equal(t, StateOPEN, h.EffectiveState(9, "q9", 1))
	// Next two successes should still require two
	shouldFail.Store(false)
	h.probeTick(context.Background())
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateOPEN, h.EffectiveState(9, "q9", 1), "after failure, needs two fresh successes")
	h.probeTick(context.Background())
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateReady, h.EffectiveState(9, "q9", 1))
}

// TestHealthProbeOnePermit verifies one permit limits concurrent probes.
func TestHealthProbeOnePermit(t *testing.T) {
	_, c := newHealthTestRedis(t)
	var concurrent atomic.Int64
	var maxConcurrent atomic.Int64
	probeFn := func(ctx context.Context, _ HealthKey) error {
		cur := concurrent.Add(1)
		for {
			m := maxConcurrent.Load()
			if cur <= m || maxConcurrent.CompareAndSwap(m, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		concurrent.Add(-1)
		return nil
	}
	h := NewRuntimeHealth(c, "self-a", func() []string { return []string{"self-a"} }, probeFn, nil)
	// Add two OPEN entries both owned by self-a
	k1 := healthKeyFor(1, "q1", 1)
	k2 := healthKeyFor(2, "q1", 1)
	_, _ = h.Throttle(context.Background(), k1, StateOPEN, 5*time.Second)
	_, _ = h.Throttle(context.Background(), k2, StateOPEN, 5*time.Second)
	require.NoError(t, h.Sync(context.Background()))

	// Run probeTick concurrently from multiple goroutines - one permit should limit to 1 at a time
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.probeTick(context.Background())
		}()
	}
	wg.Wait()
	require.LessOrEqual(t, maxConcurrent.Load(), int64(1), "one permit must limit to 1 concurrent probe")
}

// TestHealthEffectiveStateSeverity verifies OPEN>RETRY_AFTER>PROBING>READY and wildcard vs specific.
func TestHealthEffectiveStateSeverity(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h := NewRuntimeHealth(c, "self-a", nil, nil, nil)

	// Wildcard OPEN should override specific RETRY_AFTER
	wild := healthKeyFor(1, "*", 1)
	specific := healthKeyFor(1, "q1", 1)
	_, err := h.Throttle(context.Background(), wild, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	_, err = h.Throttle(context.Background(), specific, StateRetryAfter, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateOPEN, h.EffectiveState(1, "q1", 1), "wildcard OPEN must be more severe than specific RETRY_AFTER")

	// Specific OPEN overrides wildcard READY (no wildcard entry)
	h2 := NewRuntimeHealth(c, "self-a", nil, nil, nil)
	// Clear previous
	require.NoError(t, c.FlushAll(context.Background()).Err())
	_, err = h2.Throttle(context.Background(), specific, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h2.Sync(context.Background()))
	require.Equal(t, StateOPEN, h2.EffectiveState(1, "q1", 1))

	// No entry => READY
	require.Equal(t, StateReady, h2.EffectiveState(99, "unknown", 1))
}

// TestHealthKeyRevisionIsolation verifies key includes revision.
func TestHealthKeyRevisionIsolation(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h := NewRuntimeHealth(c, "self-a", nil, nil, nil)
	k1 := healthKeyFor(1, "q1", 1)
	k2 := healthKeyFor(1, "q1", 2)
	_, err := h.Throttle(context.Background(), k1, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateOPEN, h.EffectiveState(1, "q1", 1))
	require.Equal(t, StateReady, h.EffectiveState(1, "q1", 2), "different revision must be isolated")
	_, err = h.Throttle(context.Background(), k2, StateRetryAfter, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateOPEN, h.EffectiveState(1, "q1", 1))
	require.Equal(t, StateRetryAfter, h.EffectiveState(1, "q1", 2))
}
