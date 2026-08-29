// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package usage

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

type idempotentFakeInserter struct {
	mu         sync.Mutex
	requestIDs []string
}

func (f *idempotentFakeInserter) InsertBatch(_ context.Context, logs []*domain.UsageLog) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range logs {
		f.requestIDs = append(f.requestIDs, l.RequestID)
	}
	return nil
}

func TestUsageRecorderMixedBatchDrains(t *testing.T) {
	fake := &idempotentFakeInserter{}
	r := New(UsageConfig{BatchSize: 4, Workers: 1}, fake, nil)
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	ids := []string{"old-1", "new-1", "new-2", "new-3"}
	for _, id := range ids {
		r.Record(&domain.UsageLog{RequestID: id, CreatedAt: now})
	}
	require.Equal(t, 4, r.Pending())
	drained := r.flushLogs(context.Background())
	require.Equal(t, int64(4), drained)
	require.Equal(t, 0, r.Pending())
	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Len(t, fake.requestIDs, 4)
	require.ElementsMatch(t, ids, fake.requestIDs)
}
