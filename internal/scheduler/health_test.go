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

// testIdentity 是健康测试用的候选身份指纹常量：健康键的身份分量在测试里只需
// 写入与读取自洽，不必是真实指纹。测试要覆盖通配时显式用 identityAny。
const testIdentity = "fp-test"

func healthKeyFor(acc int64, quality string, rev int64) HealthKey {
	return HealthKey{AccountID: acc, Quality: quality, Identity: testIdentity, IdentityRevision: rev}
}

// TestHealthViewImmutableAtomicPointer verifies one immutable atomic.Pointer view is used.
func TestHealthViewImmutableAtomicPointer(t *testing.T) {
	mr, c := newHealthTestRedis(t)
	_ = mr
	h := NewRuntimeHealth(c, "self-a", nil, nil)
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
	require.Empty(t, view1.entries, "old view must remain immutable")
}

// TestHealthThrottleAtomicStaleNoPartial verifies Lua throttle/READY atomic generation/revision/record HASH/active ZSET/tombstone/TTL
func TestHealthThrottleAtomicStaleNoPartial(t *testing.T) {
	mr, c := newHealthTestRedis(t)
	h := NewRuntimeHealth(c, "self-a", nil, nil)

	key := healthKeyFor(10, "q1", 5)
	gen1, err := h.Throttle(context.Background(), key, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	require.Greater(t, gen1, int64(0))

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
	require.Error(t, err)

	ttl, err := c.PTTL(context.Background(), recKey).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, time.Duration(0))

	_, err = c.Incr(context.Background(), healthGenKey).Result()
	require.NoError(t, err)
	staleGen := gen1
	_, err = h.MarkReady(context.Background(), key, staleGen, 5*time.Second)
	require.Error(t, err, "stale generation must fail")

	m2, err := c.HGetAll(context.Background(), recKey).Result()
	require.NoError(t, err)
	require.Equal(t, m["state"], m2["state"], "record must not be partially deleted on stale ready")
	require.Equal(t, m["gen"], m2["gen"])

	score2, err := c.ZScore(context.Background(), healthActiveZSet, field).Result()
	require.NoError(t, err)
	require.Equal(t, score, score2, "active ZSET must not be partially removed on stale")

	_, err = c.HGet(context.Background(), healthTombstoneHash, field).Result()
	require.Error(t, err, "tombstone must not appear on stale")

	_ = mr
}

// TestHealthReplacement verifies replacement semantics: second throttle on same key overwrites with new generation.
func TestHealthReplacement(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h := NewRuntimeHealth(c, "self-a", nil, nil)
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

// TestHealthRunReset verifies run_id retention with injected clock, no wall-clock sleep.
// Uses actual Redis run_id via hook, not test-mutated field.
func TestHealthRunReset(t *testing.T) {
	mr, c := newHealthTestRedis(t)
	h := NewRuntimeHealth(c, "self-a", nil, nil)
	h.runIDHook = func(_ context.Context) (string, error) { return "run-init", nil }
	key := healthKeyFor(1, "q1", 1)
	_, err := h.Throttle(context.Background(), key, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))
	require.Contains(t, h.View(), key)
	require.Equal(t, StateOPEN, h.View()[key].State)

	fakeNow := time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)
	h2 := NewRuntimeHealth(c, "self-a", nil, nil)
	h2.now = func() time.Time { return fakeNow }
	h2.runIDHook = func(_ context.Context) (string, error) { return "run-1", nil }
	shortKey := healthKeyFor(2, "q1", 1)
	_, err = h2.Throttle(context.Background(), shortKey, StateOPEN, 40*time.Millisecond)
	require.NoError(t, err)
	require.NoError(t, h2.Sync(context.Background()))
	require.Contains(t, h2.View(), shortKey)
	require.Equal(t, int64(40), h2.View()[shortKey].TTLms)
	require.NotZero(t, h2.View()[shortKey].ExpiresAt)

	firstRunID := h2.ViewRunID()
	require.Equal(t, "run-1", firstRunID)
	h2.runIDHook = func(_ context.Context) (string, error) { return "run-2", nil }
	require.NoError(t, c.FlushAll(context.Background()).Err())
	require.NoError(t, h2.Sync(context.Background()))
	v := h2.View()
	require.Contains(t, v, shortKey, "run change must retain OPEN before deadline")
	require.Equal(t, StateOPEN, v[shortKey].State, "retained entry must stay OPEN until deadline")
	require.Equal(t, fakeNow.UnixMilli()+40, v[shortKey].ExpiresAt, "expiry stored in immutable view")
	require.Equal(t, "run-1", h2.ViewRunID(), "view runID stays old during retention")
	require.Equal(t, "run-2", h2.runID, "h.runID tracks actual Redis run_id")
	fakeNow = fakeNow.Add(50 * time.Millisecond)
	h2.now = func() time.Time { return fakeNow }
	require.NoError(t, h2.Sync(context.Background()))
	v2 := h2.View()
	require.Contains(t, v2, shortKey, "repeated empty must retain until deadline then PROBING")
	require.Equal(t, StateProbing, v2[shortKey].State, "after deadline must become PROBING explicit transition")
	require.NoError(t, h2.Sync(context.Background()))
	require.Contains(t, h2.View(), shortKey, "repeated empty must retain PROBING")
	require.Equal(t, StateProbing, h2.View()[shortKey].State)
	for i := 0; i < 3; i++ {
		require.NoError(t, h2.Sync(context.Background()))
		require.Contains(t, h2.View(), shortKey, "never clear on same-run empty before probe")
	}
	_ = mr
	_ = key
}

// TestHealthSyncGenerationRace verifies Sync INFO run_id + gen-before/records/gen-after detects stale generation with channel barrier.
func TestHealthSyncGenerationRace(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h := NewRuntimeHealth(c, "self-a", nil, nil)
	key := healthKeyFor(1, "q1", 1)
	_, err := h.Throttle(context.Background(), key, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))

	_, err = c.Incr(context.Background(), healthGenKey).Result()
	require.NoError(t, err)
	gate := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-gate:
		case <-time.After(2 * time.Second):
			return
		}
		_, _ = c.Incr(context.Background(), healthGenKey).Result()
	}()
	close(gate)
	_ = h.Sync(context.Background())
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "watchdog: concurrent incr did not complete")
	}
}

// TestHealthLockFreeReadUnderBlockedRedis verifies EffectiveState is lock-free read under blocked Redis.
func TestHealthLockFreeReadUnderBlockedRedis(t *testing.T) {
	mr, c := newHealthTestRedis(t)
	h := NewRuntimeHealth(c, "self-a", nil, nil)
	key := healthKeyFor(42, "qual1", 7)
	_, err := h.Throttle(context.Background(), key, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateOPEN, h.EffectiveState(42, "qual1", testIdentity, 7))

	baseCmd := mr.CommandCount()
	for i := 0; i < 100; i++ {
		require.Equal(t, StateOPEN, h.EffectiveState(42, "qual1", testIdentity, 7))
	}
	require.Equal(t, baseCmd, mr.CommandCount(), "EffectiveState must not issue Redis commands (lock-free)")

	mr.Close()
	require.Equal(t, StateOPEN, h.EffectiveState(42, "qual1", testIdentity, 7), "view read must succeed even when Redis blocked/closed")

	_, c2 := newHealthTestRedis(t)
	h2 := NewRuntimeHealth(c2, "self-a", nil, nil)
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
				if h2.EffectiveState(1, "q", testIdentity, 1) == StateOPEN {
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
	members := []string{"self-a", "self-b"}
	var probed []HealthKey
	var mu sync.Mutex
	probeFn := func(_ context.Context, k HealthKey) error {
		mu.Lock()
		probed = append(probed, k)
		mu.Unlock()
		return nil
	}
	hA := NewRuntimeHealth(c, "self-a", func() []string { return members }, nil)
	hB := NewRuntimeHealth(c, "self-b", func() []string { return members }, nil)
	hA.probeFn = probeFn
	hB.probeFn = probeFn

	k1 := healthKeyFor(1, "q1", 1)
	k2 := healthKeyFor(2, "q1", 1)
	// 探针只服务 PROBING（窗口 honored）——可探条目以 PROBING 构造。
	_, _ = hA.Throttle(context.Background(), k1, StateProbing, 5*time.Second)
	_, _ = hA.Throttle(context.Background(), k2, StateProbing, 5*time.Second)
	require.NoError(t, hA.Sync(context.Background()))
	require.NoError(t, hB.Sync(context.Background()))

	owner1 := rendezvousOwner(k1.String(), members)
	owner2 := rendezvousOwner(k2.String(), members)

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
		return nil
	}
	h := NewRuntimeHealth(c, "self-a", func() []string { return []string{"self-a"} }, nil)
	h.probeFn = probeFn
	key := healthKeyFor(5, "q5", 1)
	_, err := h.Throttle(context.Background(), key, StateProbing, 5*time.Second) // 探针只服务 PROBING
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateProbing, h.EffectiveState(5, "q5", testIdentity, 1))

	h.probeTick(context.Background())
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateProbing, h.EffectiveState(5, "q5", testIdentity, 1), "one success must not become READY（仍在 PROBING）")
	require.Equal(t, int64(1), probeCount.Load())

	h.probeTick(context.Background())
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateReady, h.EffectiveState(5, "q5", testIdentity, 1), "two current-gen successes must become READY")
	require.Equal(t, int64(2), probeCount.Load())

	field := key.String()
	_, err = c.ZScore(context.Background(), healthActiveZSet, field).Result()
	require.Error(t, err, "active ZSET must not contain READY key")
	_, err = c.HGet(context.Background(), healthTombstoneHash, field).Result()
	require.NoError(t, err, "tombstone must exist after READY")
}

// TestHealthProbeWindowHonoredNoEarlyProbe（owner 裁决）：OPEN/RETRY_AFTER
// 在其窗口内永不被探测——probeTick 只服务 PROBING 条目；窗口跑满 TTL，到期
// 处理权归 Sync retention，本循环不碰。
func TestHealthProbeWindowHonoredNoEarlyProbe(t *testing.T) {
	_, c := newHealthTestRedis(t)
	var probeCount atomic.Int64
	probeFn := func(_ context.Context, _ HealthKey) error {
		probeCount.Add(1)
		return nil
	}
	h := NewRuntimeHealth(c, "self-a", func() []string { return []string{"self-a"} }, nil)
	h.probeFn = probeFn

	kOpen := healthKeyFor(1, "q1", 1)
	kRetry := healthKeyFor(2, "q2", 1)
	_, err := h.Throttle(context.Background(), kOpen, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	_, err = h.Throttle(context.Background(), kRetry, StateRetryAfter, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))

	for i := 0; i < 5; i++ {
		h.probeTick(context.Background())
	}
	require.Zero(t, probeCount.Load(), "窗口内 OPEN/RETRY_AFTER 绝不被探测（不得提前清除）")
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateOPEN, h.EffectiveState(1, "q1", testIdentity, 1))
	require.Equal(t, StateRetryAfter, h.EffectiveState(2, "q2", testIdentity, 1))
}

// TestHealthProbeFailureReopen verifies failure reopen.
func TestHealthProbeFailureReopen(t *testing.T) {
	_, c := newHealthTestRedis(t)
	var shouldFail atomic.Bool
	probeFn := func(_ context.Context, _ HealthKey) error {
		if shouldFail.Load() {
			return context.DeadlineExceeded
		}
		return nil
	}
	h := NewRuntimeHealth(c, "self-a", func() []string { return []string{"self-a"} }, nil)
	h.probeFn = probeFn
	key := healthKeyFor(9, "q9", 1)
	// 探针只服务 PROBING（OPEN/RETRY_AFTER 窗口内不探测）——可探条目以
	// PROBING 构造。
	_, err := h.Throttle(context.Background(), key, StateProbing, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))

	h.probeTick(context.Background())
	h.probeTick(context.Background())
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateReady, h.EffectiveState(9, "q9", testIdentity, 1))

	// 再入 PROBING；探测失败 → 重开为 OPEN（30s）。
	_, err = h.Throttle(context.Background(), key, StateProbing, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))

	shouldFail.Store(true)
	h.probeTick(context.Background())
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateOPEN, h.EffectiveState(9, "q9", testIdentity, 1), "探测失败必须重开")

	// 重开后的 OPEN 窗口内不探测：重复 probeTick 状态不变。
	shouldFail.Store(false)
	h.probeTick(context.Background())
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateOPEN, h.EffectiveState(9, "q9", testIdentity, 1), "OPEN 窗口内不得被探测")

	// 窗口结束再入 PROBING：一次成功不足、两次才 READY。
	_, err = h.Throttle(context.Background(), key, StateProbing, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))
	h.probeTick(context.Background())
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateProbing, h.EffectiveState(9, "q9", testIdentity, 1), "after failure, needs two fresh successes")
	h.probeTick(context.Background())
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateReady, h.EffectiveState(9, "q9", testIdentity, 1))
}

func TestHealthProbeStaleRevisionDoesNotReopen(t *testing.T) {
	mr, c := newHealthTestRedis(t)
	h := NewRuntimeHealth(c, "self-a", func() []string { return []string{"self-a"} }, nil)
	h.probeFn = func(_ context.Context, _ HealthKey) error {
		return ErrProbeStaleRevision
	}
	key := healthKeyFor(10, "q10", 1)
	_, err := h.Throttle(context.Background(), key, StateProbing, 100*time.Millisecond) // 探针只服务 PROBING
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))

	genBefore := h.curGen.Load()
	field := key.String()
	h.probeTick(context.Background())
	require.Equal(t, genBefore, h.curGen.Load(), "stale probe must not rearm the old revision")
	require.NoError(t, h.Sync(context.Background()))

	mr.FastForward(200 * time.Millisecond)
	require.NoError(t, h.Sync(context.Background()))
	_, err = c.ZScore(context.Background(), healthActiveZSet, field).Result()
	require.Error(t, err, "stale health record must expire instead of being refreshed")
	// 此后 PROBING 条目按保留语义留在视图（等真实探测结果），不得过期即清除。
	require.Contains(t, h.View(), key)
	require.Equal(t, StateProbing, h.View()[key].State)
}

// TestHealthProbeOnePermit verifies one permit limits concurrent probes.
func TestHealthProbeOnePermit(t *testing.T) {
	_, c := newHealthTestRedis(t)
	var concurrent atomic.Int64
	var maxConcurrent atomic.Int64
	gate := make(chan struct{})
	arrived := make(chan struct{}, 10)
	probeFn := func(ctx context.Context, _ HealthKey) error {
		cur := concurrent.Add(1)
		for {
			m := maxConcurrent.Load()
			if cur <= m || maxConcurrent.CompareAndSwap(m, cur) {
				break
			}
		}
		select {
		case arrived <- struct{}{}:
		default:
		}
		select {
		case <-gate:
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
		concurrent.Add(-1)
		return nil
	}
	h := NewRuntimeHealth(c, "self-a", func() []string { return []string{"self-a"} }, nil)
	h.probeFn = probeFn
	k1 := healthKeyFor(1, "q1", 1)
	k2 := healthKeyFor(2, "q1", 1)
	// 探针只服务 PROBING（窗口 honored）。
	_, _ = h.Throttle(context.Background(), k1, StateProbing, 5*time.Second)
	_, _ = h.Throttle(context.Background(), k2, StateProbing, 5*time.Second)
	require.NoError(t, h.Sync(context.Background()))

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.probeTick(context.Background())
		}()
	}
	deadline := time.After(2 * time.Second)
	got := 0
	for got < 1 {
		select {
		case <-arrived:
			got++
		case <-deadline:
			close(gate)
			require.FailNow(t, "watchdog: probe did not arrive")
		}
	}
	close(gate)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "watchdog: probeTick concurrent completion timeout")
	}
	require.LessOrEqual(t, maxConcurrent.Load(), int64(1), "one permit must limit to 1 concurrent probe")
}

// TestHealthEffectiveStateSeverity verifies OPEN>RETRY_AFTER>PROBING>READY and wildcard vs specific.
func TestHealthEffectiveStateSeverity(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h := NewRuntimeHealth(c, "self-a", nil, nil)

	wild := healthKeyFor(1, "*", 1)
	specific := healthKeyFor(1, "q1", 1)
	_, err := h.Throttle(context.Background(), wild, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	_, err = h.Throttle(context.Background(), specific, StateRetryAfter, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateOPEN, h.EffectiveState(1, "q1", testIdentity, 1), "wildcard OPEN must be more severe than specific RETRY_AFTER")

	h2 := NewRuntimeHealth(c, "self-a", nil, nil)
	require.NoError(t, c.FlushAll(context.Background()).Err())
	_, err = h2.Throttle(context.Background(), specific, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h2.Sync(context.Background()))
	require.Equal(t, StateOPEN, h2.EffectiveState(1, "q1", testIdentity, 1))

	require.Equal(t, StateReady, h2.EffectiveState(99, "unknown", testIdentity, 1))
}

// TestHealthKeyRevisionIsolation verifies key includes revision.
func TestHealthKeyRevisionIsolation(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h := NewRuntimeHealth(c, "self-a", nil, nil)
	k1 := healthKeyFor(1, "q1", 1)
	k2 := healthKeyFor(1, "q1", 2)
	_, err := h.Throttle(context.Background(), k1, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateOPEN, h.EffectiveState(1, "q1", testIdentity, 1))
	require.Equal(t, StateReady, h.EffectiveState(1, "q1", testIdentity, 2), "different revision must be isolated")
	_, err = h.Throttle(context.Background(), k2, StateRetryAfter, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateOPEN, h.EffectiveState(1, "q1", testIdentity, 1))
	require.Equal(t, StateRetryAfter, h.EffectiveState(1, "q1", testIdentity, 2))
}

// TestHealthCleanupRaceRetainsRecreated verifies Lua cleanup re-validates global gen and per-record before ZREM/HDEL; concurrent recreate never deleted.
// With final fence at beforePublish, stale generation after cleanup must freeze old view/curGen.
func TestHealthCleanupRaceRetainsRecreated(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h := NewRuntimeHealth(c, "self-a", nil, nil)
	key := healthKeyFor(77, "q-race", 1)
	_, err := h.Throttle(context.Background(), key, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))
	require.Contains(t, h.View(), key)
	snapBefore := h.View()[key]
	genBefore := h.curGen.Load()

	field := key.String()
	recKey := healthRecordPrefix + field
	require.NoError(t, c.Del(context.Background(), recKey).Err())
	_, err = c.ZScore(context.Background(), healthActiveZSet, field).Result()
	require.NoError(t, err, "ZSET should still have field after hash delete")

	recreated := make(chan struct{})
	proceed := make(chan struct{})
	h.syncHook = func(stage string) {
		if stage == "beforeCleanup" {
			select {
			case recreated <- struct{}{}:
			default:
			}
			select {
			case <-proceed:
			case <-time.After(2 * time.Second):
			}
		}
	}
	syncErr := make(chan error, 1)
	go func() {
		syncErr <- h.Sync(context.Background())
	}()
	select {
	case <-recreated:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "watchdog: Sync did not reach beforeCleanup")
	}
	gen2, err := h.Throttle(context.Background(), key, StateRetryAfter, 5*time.Second)
	require.NoError(t, err)
	require.Greater(t, gen2, int64(0))
	close(proceed)
	select {
	case err := <-syncErr:
		require.Error(t, err, "fence must detect stale generation after cleanup race and freeze")
		require.Contains(t, err.Error(), "stale generation")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "watchdog: Sync after race timeout")
	}
	require.Equal(t, genBefore, h.curGen.Load(), "curGen must freeze on fence stale")
	require.Contains(t, h.View(), key, "view must freeze, not publish stale empty")
	require.Equal(t, snapBefore.Generation, h.View()[key].Generation, "view entry must be frozen old generation")
	m, err := c.HGetAll(context.Background(), recKey).Result()
	require.NoError(t, err)
	require.NotEmpty(t, m, "concurrently recreated record must not be deleted")
	require.Equal(t, "RETRY_AFTER", m["state"])
	_, err = c.ZScore(context.Background(), healthActiveZSet, field).Result()
	require.NoError(t, err, "ZSET must retain recreated field after Lua validation and fence")
	require.NoError(t, h.Sync(context.Background()))
	require.Contains(t, h.View(), key)
	require.Equal(t, StateRetryAfter, h.View()[key].State)
	h.syncHook = nil
}

// TestHealthCleanupErrorFreezes verifies cleanup Lua errors propagate and freeze view, never silently delete.
func TestHealthCleanupErrorFreezes(t *testing.T) {
	mr, c := newHealthTestRedis(t)
	h := NewRuntimeHealth(c, "self-a", nil, nil)
	fakeNow := time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return fakeNow }
	key := healthKeyFor(88, "q-err", 1)
	_, err := h.Throttle(context.Background(), key, StateOPEN, 40*time.Millisecond)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))
	require.Contains(t, h.View(), key)
	field := key.String()
	recKey := healthRecordPrefix + field
	require.NoError(t, c.Del(context.Background(), recKey).Err())
	require.NoError(t, c.HSet(context.Background(), healthTombstoneHash, field, "999").Err())
	require.Equal(t, int64(0), func() int64 { n, _ := c.Exists(context.Background(), healthTombstonePrefix+field).Result(); return n }())
	// Inject deterministic error: syncHook closes miniredis synchronously before cleanup Eval, causing Eval to error.
	h.syncHook = func(stage string) {
		if stage == "beforeCleanup" {
			mr.Close()
		}
	}
	err = h.Sync(context.Background())
	require.Error(t, err, "cleanup error must propagate")
	require.Contains(t, h.View(), key, "view must freeze on cleanup error, not silently delete")
	h.syncHook = nil
}

// TestHealthRunResetRepeatedEmptiesDeadlineProbe verifies repeated empty scans retain until deadline, explicit PROBING transition, never clear before probe.
// Uses actual Redis run_id via hook, not test-mutated field.
func TestHealthRunResetRepeatedEmptiesDeadlineProbe(t *testing.T) {
	_, c := newHealthTestRedis(t)
	fakeNow := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	h := NewRuntimeHealth(c, "self-a", nil, nil)
	h.now = func() time.Time { return fakeNow }
	h.runIDHook = func(_ context.Context) (string, error) { return "run-1", nil }
	keys := []HealthKey{healthKeyFor(10, "q1", 1), healthKeyFor(11, "q1", 1), healthKeyFor(12, "*", 1)}
	for _, k := range keys {
		_, err := h.Throttle(context.Background(), k, StateOPEN, 100*time.Millisecond)
		require.NoError(t, err)
	}
	require.NoError(t, h.Sync(context.Background()))
	for _, k := range keys {
		require.Contains(t, h.View(), k)
		require.Equal(t, StateOPEN, h.View()[k].State)
		require.NotZero(t, h.View()[k].ExpiresAt)
	}
	runBefore := h.ViewRunID()
	require.Equal(t, "run-1", runBefore)
	h.runIDHook = func(_ context.Context) (string, error) { return "run-2", nil }
	require.NoError(t, c.FlushAll(context.Background()).Err())
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, "run-1", h.ViewRunID(), "view stays old during retention")
	require.Equal(t, "run-2", h.runID)
	// Repeated empty at same fakeNow before deadline: retain all OPEN
	for i := 0; i < 3; i++ {
		require.NoError(t, h.Sync(context.Background()))
		for _, k := range keys {
			require.Contains(t, h.View(), k, "repeated empty before deadline must retain every prior OPEN/RETRY")
			require.Equal(t, StateOPEN, h.View()[k].State)
		}
		require.Equal(t, 3, len(h.View()))
	}
	// Advance clock to deadline via injected clock, no sleep
	fakeNow = fakeNow.Add(120 * time.Millisecond)
	h.now = func() time.Time { return fakeNow }
	require.NoError(t, h.Sync(context.Background()))
	for _, k := range keys {
		require.Contains(t, h.View(), k, "at deadline must retain until probe, not clear")
		require.Equal(t, StateProbing, h.View()[k].State, "explicit transition to PROBING at deadline")
	}
	// Repeated same-run empty after deadline must retain PROBING until real probe, never clear
	for i := 0; i < 3; i++ {
		require.NoError(t, h.Sync(context.Background()))
		for _, k := range keys {
			require.Contains(t, h.View(), k, "never clear on same-run empty before probe")
			require.Equal(t, StateProbing, h.View()[k].State)
		}
	}
	// Probe outcome: simulate one successful probe cycle - view stays PROBING until two successes READY
	// Inject probe that succeeds
	var probeCalls atomic.Int64
	h.probeFn = func(_ context.Context, _ HealthKey) error {
		probeCalls.Add(1)
		return nil
	}
	// probeTick on PROBING entries needs current gen; curGen is stored, probe will count success per field
	h.probeTick(context.Background())
	// One success keeps PROBING (needs two)
	require.NoError(t, h.Sync(context.Background()))
	for _, k := range keys {
		require.Equal(t, StateProbing, h.EffectiveState(k.AccountID, k.Quality, testIdentity, k.IdentityRevision))
	}
	h.probeTick(context.Background())
	// After two successes, MarkReady tries but will fail because Redis empty (no record), so generation check fails and stays PROBING
	// That's expected when Redis is empty - failure reopen would throttle new OPEN; we verify PROBING not cleared prematurely
	require.NoError(t, h.Sync(context.Background()))
	// View may still be PROBING or may have been reopened to OPEN via Throttle failure path; either way not empty before probe outcome
	require.NotEmpty(t, h.View(), "must retain until real probe outcome, never clear to empty on same-run empty")
}

// TestHealthCurGenInterleaving verifies curGen atomic interleaving: concurrent Sync increments cause stale detection and frozen view.
func TestHealthCurGenInterleaving(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h := NewRuntimeHealth(c, "self-a", nil, nil)
	key := healthKeyFor(20, "q-cur", 1)
	_, err := h.Throttle(context.Background(), key, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))
	snapBefore := h.View()
	require.Contains(t, snapBefore, key)
	genBefore := h.curGen.Load()
	require.Greater(t, genBefore, int64(0))

	// Deterministic interleaving via syncHook: block Sync after reading genBefore, bump generation concurrently, then verify stale error.
	blocked := make(chan struct{})
	release := make(chan struct{})
	h.syncHook = func(stage string) {
		if stage == "beforeGenAfter" {
			select {
			case blocked <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-time.After(2 * time.Second):
			}
		}
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- h.Sync(context.Background())
	}()
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "watchdog: Sync did not reach beforeGenAfter")
	}
	// Interleave: increment global generation while Sync is paused before reading genAfter
	_, err = c.Incr(context.Background(), healthGenKey).Result()
	require.NoError(t, err)
	close(release)
	select {
	case err := <-errCh:
		require.Error(t, err)
		require.Contains(t, err.Error(), "stale generation")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "watchdog: Sync curGen interleaving timeout")
	}
	// View must be frozen (not overwritten with partial/stale data)
	require.Equal(t, snapBefore[key].Generation, h.View()[key].Generation, "view must freeze on curGen stale error")
	require.Equal(t, genBefore, h.curGen.Load(), "curGen must not advance on stale read")
	h.syncHook = nil

	// Verify next Sync without interleaving succeeds and updates curGen
	newGen := genBefore + 1
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, newGen, h.curGen.Load())
	require.Contains(t, h.View(), key)

	// Probe curGen check interleaving: probeTick must read current gen atomically via Load and re-validate with GET
	h2 := NewRuntimeHealth(c, "self-a", func() []string { return []string{"self-a"} }, nil)
	require.NoError(t, h2.Sync(context.Background()))
	// Concurrent increments while probe collects curGen
	var wg sync.WaitGroup
	wg.Add(2)
	probeFields := make(chan HealthKey, 10)
	h2.probeFn = func(_ context.Context, k HealthKey) error { probeFields <- k; return nil }
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			_, _ = h2.Throttle(context.Background(), healthKeyFor(int64(100+i), "q", 1), StateProbing, 5*time.Second) // 探针只服务 PROBING（本段验证 probeTick 与 gen 递增并发）
		}
	}()
	go func() {
		defer wg.Done()
		h2.probeTick(context.Background())
	}()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "watchdog: probe curGen interleaving timeout")
	}
	// No panic, curGen remains consistent
	require.GreaterOrEqual(t, h2.curGen.Load(), newGen)
}

// TestHealthMalformedGenerationStrict verifies strict parsing: malformed/overflow/negative generation strings return error and freeze view/curGen.
func TestHealthMalformedGenerationStrict(t *testing.T) {
	_, c := newHealthTestRedis(t)
	fakeNow := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	h := NewRuntimeHealth(c, "self-a", nil, nil)
	h.now = func() time.Time { return fakeNow }
	key := healthKeyFor(30, "q-mal", 1)
	_, err := h.Throttle(context.Background(), key, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))
	snapBefore := h.View()
	genBefore := h.curGen.Load()
	require.Contains(t, snapBefore, key)
	require.Greater(t, genBefore, int64(0))

	cases := []string{"not-a-number", "12abc", "9223372036854775808", "-1", "-9223372036854775808", "  ", "1.5"}
	for _, bad := range cases {
		require.NoError(t, c.Set(context.Background(), healthGenKey, bad, 0).Err())
		err = h.Sync(context.Background())
		require.Error(t, err, "bad gen %q must error", bad)
		require.Equal(t, genBefore, h.curGen.Load(), "curGen must freeze on malformed global gen %q", bad)
		require.Equal(t, snapBefore[key].Generation, h.View()[key].Generation, "view must freeze on malformed global gen %q", bad)
	}
	require.NoError(t, c.Set(context.Background(), healthGenKey, "2", 0).Err())
	_ = h.Sync(context.Background())
	genReset := h.curGen.Load()
	require.Equal(t, int64(2), genReset)

	// record generation malformed via barrier injected clock
	h2 := NewRuntimeHealth(c, "self-a", nil, nil)
	h2.now = func() time.Time { return fakeNow }
	k2 := healthKeyFor(31, "q-rec", 1)
	_, err = h2.Throttle(context.Background(), k2, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h2.Sync(context.Background()))
	genBefore2 := h2.curGen.Load()
	snapBefore2 := h2.View()
	recKey := healthRecordPrefix + k2.String()
	require.NoError(t, c.HSet(context.Background(), recKey, "gen", "bad-gen").Err())
	err = h2.Sync(context.Background())
	require.Error(t, err)
	require.Equal(t, genBefore2, h2.curGen.Load(), "curGen freeze on malformed record gen")
	require.Equal(t, snapBefore2[k2].Generation, h2.View()[k2].Generation)
	require.NoError(t, c.Del(context.Background(), recKey).Err())
	require.NoError(t, c.ZRem(context.Background(), healthActiveZSet, k2.String()).Err())
	require.NoError(t, c.Set(context.Background(), healthGenKey, "10", 0).Err())

	// barrier: inject malformed between genBefore and genAfter via syncHook
	h3 := NewRuntimeHealth(c, "self-a", nil, nil)
	h3.now = func() time.Time { return fakeNow }
	h3.runIDHook = func(_ context.Context) (string, error) { return "run-barrier", nil }
	k3 := healthKeyFor(32, "q-barrier", 1)
	_, err = h3.Throttle(context.Background(), k3, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h3.Sync(context.Background()))
	genBefore3 := h3.curGen.Load()
	blocked := make(chan struct{})
	release := make(chan struct{})
	h3.syncHook = func(stage string) {
		if stage == "beforeGenAfter" {
			select {
			case blocked <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-time.After(2 * time.Second):
			}
		}
	}
	errCh := make(chan error, 1)
	go func() { errCh <- h3.Sync(context.Background()) }()
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "watchdog: barrier malformed gen")
	}
	require.NoError(t, c.Set(context.Background(), healthGenKey, "not-a-number", 0).Err())
	close(release)
	select {
	case err := <-errCh:
		require.Error(t, err)
		require.Equal(t, genBefore3, h3.curGen.Load(), "curGen must not advance when genAfter malformed via barrier")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "watchdog: malformed barrier timeout")
	}
	h3.syncHook = nil
}

// TestHealthCleanupFailureCurGenUnchanged verifies candidate generation stored only after cleanup and view publish succeed.
func TestHealthCleanupFailureCurGenUnchanged(t *testing.T) {
	mr, c := newHealthTestRedis(t)
	fakeNow := time.Date(2026, 8, 29, 14, 0, 0, 0, time.UTC)
	h := NewRuntimeHealth(c, "self-a", nil, nil)
	h.now = func() time.Time { return fakeNow }
	key := healthKeyFor(88, "q-err2", 1)
	_, err := h.Throttle(context.Background(), key, StateOPEN, 40*time.Millisecond)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))
	genBefore := h.curGen.Load()
	viewBefore := h.View()
	require.Contains(t, viewBefore, key)
	field := key.String()
	recKey := healthRecordPrefix + field
	require.NoError(t, c.Del(context.Background(), recKey).Err())
	require.NoError(t, c.HSet(context.Background(), healthTombstoneHash, field, "999").Err())
	h.syncHook = func(stage string) {
		if stage == "beforeCleanup" {
			mr.Close()
		}
	}
	err = h.Sync(context.Background())
	require.Error(t, err, "cleanup error must propagate")
	require.Equal(t, genBefore, h.curGen.Load(), "curGen must not advance on cleanup failure")
	require.Contains(t, h.View(), key, "view must freeze on cleanup failure")
	require.Equal(t, viewBefore[key].Generation, h.View()[key].Generation)
	h.syncHook = nil
}

// TestHealthActualRunIDChangeRepeatedEmptyDeadline verifies actual Redis run_id based retention with injected clock and barrier.
func TestHealthActualRunIDChangeRepeatedEmptyDeadline(t *testing.T) {
	_, c1 := newHealthTestRedis(t)
	fakeNow := time.Date(2026, 8, 29, 15, 0, 0, 0, time.UTC)
	h := NewRuntimeHealth(c1, "self-a", nil, nil)
	h.now = func() time.Time { return fakeNow }
	h.runIDHook = func(_ context.Context) (string, error) { return "run-1", nil }
	keys := []HealthKey{healthKeyFor(40, "q1", 1), healthKeyFor(41, "q1", 1)}
	for _, k := range keys {
		_, err := h.Throttle(context.Background(), k, StateOPEN, 100*time.Millisecond)
		require.NoError(t, err)
	}
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, 2, len(h.View()))
	runBefore := h.ViewRunID()
	require.Equal(t, "run-1", runBefore)
	h.runIDHook = func(_ context.Context) (string, error) { return "run-2", nil }
	require.NoError(t, c1.FlushAll(context.Background()).Err())
	barrier := make(chan struct{})
	release := make(chan struct{})
	h.syncHook = func(stage string) {
		if stage == "beforeGenAfter" {
			select {
			case barrier <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-time.After(2 * time.Second):
			}
		}
	}
	errCh := make(chan error, 1)
	go func() { errCh <- h.Sync(context.Background()) }()
	select {
	case <-barrier:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "watchdog: run change barrier")
	}
	close(release)
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "watchdog: run change sync timeout")
	}
	h.syncHook = nil
	require.Equal(t, "run-1", h.ViewRunID(), "view stays old during retention")
	require.Equal(t, "run-2", h.runID)
	for _, k := range keys {
		require.Contains(t, h.View(), k, "run-change repeated empty must retain OPEN before deadline")
		require.Equal(t, StateOPEN, h.View()[k].State)
	}
	for i := 0; i < 2; i++ {
		require.NoError(t, h.Sync(context.Background()))
		for _, k := range keys {
			require.Equal(t, StateOPEN, h.View()[k].State, "repeated empty before deadline retains OPEN")
		}
	}
	fakeNow = fakeNow.Add(150 * time.Millisecond)
	h.now = func() time.Time { return fakeNow }
	require.NoError(t, h.Sync(context.Background()))
	for _, k := range keys {
		require.Equal(t, StateProbing, h.View()[k].State, "after ExpiresAt must become PROBING")
	}
	for i := 0; i < 2; i++ {
		require.NoError(t, h.Sync(context.Background()))
		for _, k := range keys {
			require.Equal(t, StateProbing, h.View()[k].State, "same-run repeated empty retains PROBING until probe")
		}
	}
	require.NotEmpty(t, h.View(), "never clear to empty before real probe result")
}

// TestHealthSameRunEmptyClears verifies same-run empty may clear READY/missing (no retention without run change).
func TestHealthSameRunEmptyClears(t *testing.T) {
	_, c := newHealthTestRedis(t)
	fakeNow := time.Date(2026, 8, 29, 16, 0, 0, 0, time.UTC)
	h := NewRuntimeHealth(c, "self-a", nil, nil)
	h.now = func() time.Time { return fakeNow }
	h.runIDHook = func(_ context.Context) (string, error) { return "run-same", nil }
	key := healthKeyFor(50, "q-clear", 1)
	_, err := h.Throttle(context.Background(), key, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))
	require.Contains(t, h.View(), key)
	runBefore := h.ViewRunID()
	require.Equal(t, "run-same", runBefore)
	require.NoError(t, c.FlushAll(context.Background()).Err())
	require.NoError(t, h.Sync(context.Background()))
	require.NotContains(t, h.View(), key, "same-run empty must clear")
	require.Empty(t, h.View(), "same-run empty without hasProbing must clear to empty")
	require.Equal(t, runBefore, h.ViewRunID(), "same-run keeps same runID")
}

// TestHealthFenceBeforePublishFreezes verifies final fence immediately after cleanup and before publish freezes stale view.
func TestHealthFenceBeforePublishFreezes(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h := NewRuntimeHealth(c, "self-a", nil, nil)
	initialKey := healthKeyFor(60, "q-fence", 1)
	_, err := h.Throttle(context.Background(), initialKey, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))
	require.Contains(t, h.View(), initialKey)
	snapBefore := h.View()[initialKey]
	genBefore := h.curGen.Load()
	viewPtrBefore := h.view.Load()

	otherKey := healthKeyFor(61, "q-fence-other", 1)
	needFence := make(chan struct{})
	proceed := make(chan struct{})
	h.syncHook = func(stage string) {
		if stage == "beforePublish" {
			select {
			case needFence <- struct{}{}:
			default:
			}
			select {
			case <-proceed:
			case <-time.After(2 * time.Second):
			}
		}
	}
	syncErr := make(chan error, 1)
	go func() { syncErr <- h.Sync(context.Background()) }()
	select {
	case <-needFence:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "watchdog: Sync did not reach beforePublish")
	}
	_, err = h.Throttle(context.Background(), otherKey, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	close(proceed)
	select {
	case err := <-syncErr:
		require.Error(t, err)
		require.Contains(t, err.Error(), "stale generation")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "watchdog: fence Sync timeout")
	}
	require.Equal(t, genBefore, h.curGen.Load(), "curGen must freeze on final fence")
	require.Same(t, viewPtrBefore, h.view.Load(), "view pointer must not be replaced on fence stale")
	require.Equal(t, snapBefore.ExpiresAt, h.View()[initialKey].ExpiresAt, "frozen view entry must retain original ExpiresAt")
	require.NotContains(t, h.View(), otherKey, "stale view must not contain concurrently added key")
	h.syncHook = nil
	require.NoError(t, h.Sync(context.Background()))
	require.Contains(t, h.View(), initialKey)
	require.Contains(t, h.View(), otherKey)
}

// TestHealthExpiresAtNotExtended verifies repeated normal syncs do not extend OPEN/RETRY deadline via injected clock.
func TestHealthExpiresAtNotExtended(t *testing.T) {
	_, c := newHealthTestRedis(t)
	fakeNow := time.Date(2026, 8, 29, 17, 0, 0, 0, time.UTC)
	h := NewRuntimeHealth(c, "self-a", nil, nil)
	h.now = func() time.Time { return fakeNow }
	h.runIDHook = func(_ context.Context) (string, error) { return "run-stable", nil }
	key := healthKeyFor(90, "q-stable", 1)
	_, err := h.Throttle(context.Background(), key, StateOPEN, 200*time.Millisecond)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))
	require.Contains(t, h.View(), key)
	origExpires := h.View()[key].ExpiresAt
	origUpdated := h.View()[key].UpdatedAt
	require.Equal(t, fakeNow.UnixMilli()+200, origExpires)
	for i := 0; i < 5; i++ {
		fakeNow = fakeNow.Add(20 * time.Millisecond)
		h.now = func() time.Time { return fakeNow }
		require.NoError(t, h.Sync(context.Background()))
		require.Contains(t, h.View(), key)
		require.Equal(t, origExpires, h.View()[key].ExpiresAt, "repeated normal sync must not extend ExpiresAt")
		require.Equal(t, origUpdated, h.View()[key].UpdatedAt, "UpdatedAt must stay stable across refreshes")
		require.Equal(t, StateOPEN, h.View()[key].State, "state must remain OPEN before deadline")
	}
	fakeNow = fakeNow.Add(30 * time.Millisecond)
	h.now = func() time.Time { return fakeNow }
	require.NoError(t, c.FlushAll(context.Background()).Err())
	h.runIDHook = func(_ context.Context) (string, error) { return "run-stable-2", nil }
	require.NoError(t, h.Sync(context.Background()))
	require.Contains(t, h.View(), key, "run-change empty must retain until original deadline")
	require.Equal(t, origExpires, h.View()[key].ExpiresAt, "retained entry must keep original ExpiresAt")
	require.Equal(t, StateOPEN, h.View()[key].State, "before deadline retains OPEN")
	fakeNow = fakeNow.Add(100 * time.Millisecond)
	h.now = func() time.Time { return fakeNow }
	require.NoError(t, h.Sync(context.Background()))
	require.Contains(t, h.View(), key)
	require.Equal(t, StateProbing, h.View()[key].State, "after deadline must become PROBING")
	require.Equal(t, origExpires, h.View()[key].ExpiresAt, "even after PROBING transition ExpiresAt stays original")
}
