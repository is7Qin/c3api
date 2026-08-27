// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package notification

import (
	"context"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func newTestRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(func() { mr.Close() })
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return client, mr
}

func warningEvent(userID, threshold int64, email string) domain.BalanceWarningEvent {
	return domain.BalanceWarningEvent{
		EventType:       domain.NotificationBalanceWarningCrossed,
		EntityType:      domain.NotificationUser,
		EntityID:        userID,
		BalanceMillis:   threshold - 100,
		ThresholdMillis: threshold,
		Email:           email,
	}
}

func TestCooldownTryClaimAtomic(t *testing.T) {
	client, _ := newTestRedis(t)
	cd := NewCooldown(client)
	ctx := context.Background()
	ev := warningEvent(42, 100000, "u@example.com")

	token, claimed, err := cd.TryClaim(ctx, ev)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NotEmpty(t, token)

	// Second claim for same user/threshold must be suppressed.
	_, claimed2, err := cd.TryClaim(ctx, ev)
	require.NoError(t, err)
	require.False(t, claimed2, "second claim must be suppressed by NX")

	// Different threshold must be independent.
	ev2 := warningEvent(42, 200000, "u@example.com")
	_, claimed3, err := cd.TryClaim(ctx, ev2)
	require.NoError(t, err)
	require.True(t, claimed3, "different threshold must be independent")

	// Different user must be independent.
	ev3 := warningEvent(99, 100000, "other@example.com")
	_, claimed4, err := cd.TryClaim(ctx, ev3)
	require.NoError(t, err)
	require.True(t, claimed4, "different user must be independent")
}

func TestCooldownCompareDeleteOnlyOwnToken(t *testing.T) {
	client, _ := newTestRedis(t)
	cd := NewCooldown(client)
	ctx := context.Background()
	ev := warningEvent(1, 50000, "a@example.com")

	token, claimed, err := cd.TryClaim(ctx, ev)
	require.NoError(t, err)
	require.True(t, claimed)

	// Wrong token must not delete.
	require.NoError(t, cd.Release(ctx, ev, "wrong-token"))
	// Original claim must still hold -> next claim still suppressed.
	_, claimed2, err := cd.TryClaim(ctx, ev)
	require.NoError(t, err)
	require.False(t, claimed2, "wrong token release must not free the slot")

	// Correct token must release.
	require.NoError(t, cd.Release(ctx, ev, token))
	_, claimed3, err := cd.TryClaim(ctx, ev)
	require.NoError(t, err)
	require.True(t, claimed3, "correct token release must free the slot")
}

func TestCooldownClearRetiredThreshold(t *testing.T) {
	client, _ := newTestRedis(t)
	cd := NewCooldown(client)
	ctx := context.Background()
	ev := warningEvent(2, 75_000, "retired@example.com")

	_, claimed, err := cd.TryClaim(ctx, ev)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, cd.Clear(ctx, ev.EntityID, ev.ThresholdMillis))
	_, claimed, err = cd.TryClaim(ctx, ev)
	require.NoError(t, err)
	require.True(t, claimed)
}

func TestCooldownConcurrentClaimAdmitsOne(t *testing.T) {
	client, _ := newTestRedis(t)
	cd := NewCooldown(client)
	ctx := context.Background()
	ev := warningEvent(7, 100000, "c@example.com")

	const goroutines = 20
	var claimed int
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			_, ok, err := cd.TryClaim(ctx, ev)
			if err == nil && ok {
				mu.Lock()
				claimed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	require.Equal(t, 1, claimed, "concurrent claims for same user/threshold must admit exactly one")
}

func TestCooldownReleaseEmptyTokenNoop(t *testing.T) {
	client, _ := newTestRedis(t)
	cd := NewCooldown(client)
	require.NoError(t, cd.Release(context.Background(), warningEvent(1, 100, "a@b.com"), ""))
}

func TestCooldownKeyIncludesChannelThreshold(t *testing.T) {
	// Key format must include channel (email), user ID, and threshold.
	ev := warningEvent(123, 99999, "x@example.com")
	k := cooldownKey(ev)
	require.Contains(t, k, "email")
	require.Contains(t, k, "123")
	require.Contains(t, k, "99999")
	// Different email but same user/threshold currently shares key (user ID +
	// threshold is the dedup dimension per spec). Ensure key is deterministic.
	require.Equal(t, k, cooldownKey(ev))
}
