// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

type failingBucketStore struct {
	*fakeLedgerStore
	mu           sync.Mutex
	failBucket   int
	failLane     string
	shouldFail   bool
	balanceCalls map[int]int
	fefoCalls    map[int]int
}

func newFailingBucketStore(base *fakeLedgerStore, failLane string, failBucket int) *failingBucketStore {
	return &failingBucketStore{
		fakeLedgerStore: base,
		failBucket:      failBucket,
		failLane:        failLane,
		shouldFail:      true,
		balanceCalls:    map[int]int{},
		fefoCalls:       map[int]int{},
	}
}

func (s *failingBucketStore) SettleBalanceBatch(ctx context.Context, limit, k, bucket int) (domain.SettlementSummary, error) {
	s.mu.Lock()
	s.balanceCalls[bucket]++
	should := s.shouldFail && s.failLane == "balance" && bucket == s.failBucket
	s.mu.Unlock()
	if should {
		return domain.SettlementSummary{}, errors.New("injected deterministic 22003")
	}
	return s.fakeLedgerStore.SettleBalanceBatch(ctx, limit, k, bucket)
}

func (s *failingBucketStore) SettleFefoBatch(ctx context.Context, limit, k, bucket int) (domain.SettlementSummary, error) {
	s.mu.Lock()
	s.fefoCalls[bucket]++
	should := s.shouldFail && s.failLane == "fefo" && bucket == s.failBucket
	s.mu.Unlock()
	if should {
		return domain.SettlementSummary{}, errors.New("injected fefo failure")
	}
	return s.fakeLedgerStore.SettleFefoBatch(ctx, limit, k, bucket)
}

func (s *failingBucketStore) callCount(bucket int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.balanceCalls[bucket]
}

func TestDrain_FailedBucketSuppressedUntilNextCycle(t *testing.T) {
	base := newFakeLedgerStore()
	uids := []int64{4, 5, 6, 7}
	for i, uid := range uids {
		base.seedRow(int64(i+1), uid, 100, time.Now())
		base.setBalance(uid, 1000)
	}
	store := newFailingBucketStore(base, "balance", 1)
	logger, out := newTestLogger(t)
	f := newFlusherWith(store.fakeLedgerStore, 4, map[int64]int64{4: 1000, 5: 1000, 6: 1000, 7: 1000})
	f.store = store
	f.log = logger
	oldBudget := drainCycleBudget
	drainCycleBudget = time.Hour
	t.Cleanup(func() { drainCycleBudget = oldBudget })

	n := f.consumeCycle(context.Background(), false)
	require.Equal(t, int64(3), n, "healthy buckets must commit")
	require.True(t, base.isBilled(1), "bucket 0 must commit")
	require.False(t, base.isBilled(2), "bucket 1 must remain unbilled not retried within same cycle")
	require.True(t, base.isBilled(3))
	require.True(t, base.isBilled(4))
	require.Equal(t, 1, store.callCount(1), "failed bucket must be tried exactly once this cycle")
	require.Equal(t, int64(1000), base.balanceOf(5), "no deduction for failed bucket yet")
	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(b), "billing settle lane failed")
	require.Contains(t, string(b), "\"lane\":\"balance\"")
	require.Contains(t, strings.ToLower(string(b)), "\"bucket\":1")
	require.Contains(t, string(b), "\"retry_scope\":\"next_cycle\"")

	store.mu.Lock()
	store.shouldFail = false
	store.mu.Unlock()
	n2 := f.consumeCycle(context.Background(), false)
	require.Equal(t, int64(1), n2, "next cycle retries failed bucket")
	require.True(t, base.isBilled(2))
	require.Equal(t, int64(900), base.balanceOf(5))
	require.GreaterOrEqual(t, store.callCount(1), 2, "failed bucket retried next cycle")
}
