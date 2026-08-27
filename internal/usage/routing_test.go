// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package usage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

type routingDecisionStore struct {
	mu     sync.Mutex
	decide func([]*domain.UsageLog) error
	logs   []*domain.UsageLog
}

func (m *routingDecisionStore) InsertBatch(ctx context.Context, logs []*domain.UsageLog) error {
	if m.decide != nil {
		if err := m.decide(logs); err != nil {
			return err
		}
	}
	m.mu.Lock()
	m.logs = append(m.logs, logs...)
	m.mu.Unlock()
	return nil
}

func (m *routingDecisionStore) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.logs)
}

func TestUsageInitialRoutingPreservesPending(t *testing.T) {
	logger, out := usageTestLogger(t)
	fake := &routingDecisionStore{
		decide: func(logs []*domain.UsageLog) error {
			return fmt.Errorf("%w: no partition of relation \"usage_logs\" found for row", domain.ErrPartitionUnavailable)
		},
	}
	r := New(UsageConfig{BatchSize: 4, Workers: 1}, fake, logger)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	far := time.Date(2099, 1, 1, 12, 0, 0, 0, time.UTC)
	orig := make([]*domain.UsageLog, 4)
	for i := 0; i < 4; i++ {
		at := now
		if i >= 2 {
			at = far
		}
		orig[i] = &domain.UsageLog{RequestID: fmt.Sprintf("req-%d", i), Model: "m", Format: domain.FormatOpenAIChat, CreatedAt: at, TotalTokens: 1}
		r.Record(orig[i])
	}
	drained := r.flushLogs(context.Background())
	require.Equal(t, int64(0), drained, "routing failure must not drain")
	require.Equal(t, 4, r.Pending(), "all rows must be refilled")
	require.Zero(t, fake.count(), "no rows should be persisted on routing failure")
	for i, l := range r.pending {
		require.Equal(t, orig[i].CreatedAt, l.CreatedAt, "CreatedAt must be preserved")
		require.Equal(t, orig[i].RequestID, l.RequestID)
	}
	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(b), "retry_scope")
	require.Contains(t, string(b), "next_flush")
	require.NotContains(t, string(b), "dropping poison")
}

func TestUsageInitialRoutingLaterChunkPartialSuccess(t *testing.T) {
	logger, out := usageTestLogger(t)
	calls := 0
	fake := &routingDecisionStore{
		decide: func(logs []*domain.UsageLog) error {
			calls++
			if calls == 1 {
				return nil
			}
			return fmt.Errorf("%w: no partition", domain.ErrPartitionUnavailable)
		},
	}
	r := New(UsageConfig{BatchSize: 2, Workers: 1}, fake, logger)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		r.Record(&domain.UsageLog{RequestID: fmt.Sprintf("k-%d", i), Model: "m", Format: domain.FormatOpenAIChat, CreatedAt: now, TotalTokens: 1})
	}
	drained := r.flushLogs(context.Background())
	require.Equal(t, int64(2), drained, "first chunk should persist")
	require.Equal(t, 2, r.Pending(), "second chunk refilled")
	require.Equal(t, 2, fake.count())
	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(b), "retry_scope")
}

func TestUsageBisectRoutingKeepsSibling(t *testing.T) {
	logger, out := usageTestLogger(t)
	fake := &routingDecisionStore{
		decide: func(logs []*domain.UsageLog) error {
			if len(logs) == 4 {
				return errors.New("generic failure to trigger bisect")
			}
			hasRouting := false
			for _, l := range logs {
				if l.RequestID == "route-a" || l.RequestID == "route-b" {
					hasRouting = true
					break
				}
			}
			if hasRouting {
				return fmt.Errorf("%w: no partition", domain.ErrPartitionUnavailable)
			}
			return nil
		},
	}
	r := New(UsageConfig{BatchSize: 4, Workers: 1}, fake, logger)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	far := time.Date(2099, 1, 1, 12, 0, 0, 0, time.UTC)
	logs := []*domain.UsageLog{
		{RequestID: "good-a", Model: "m", Format: domain.FormatOpenAIChat, CreatedAt: now},
		{RequestID: "good-b", Model: "m", Format: domain.FormatOpenAIChat, CreatedAt: now},
		{RequestID: "route-a", Model: "m", Format: domain.FormatOpenAIChat, CreatedAt: far},
		{RequestID: "route-b", Model: "m", Format: domain.FormatOpenAIChat, CreatedAt: far},
	}
	for _, l := range logs {
		r.Record(l)
	}
	drained := r.flushLogs(context.Background())
	require.Equal(t, int64(2), drained, "successful sibling must be counted")
	require.Equal(t, 2, r.Pending(), "routing half must be refilled")
	require.Equal(t, 2, fake.count(), "only good half persisted")
	for _, l := range r.pending {
		require.True(t, l.RequestID == "route-a" || l.RequestID == "route-b")
		require.Equal(t, far, l.CreatedAt, "timestamp not mutated")
	}
	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(b), "retry_scope")
	require.NotContains(t, string(b), "dropping poison")
}

func TestUsageTimestampPreservedAfterRouting(t *testing.T) {
	logger, _ := usageTestLogger(t)
	fake := &routingDecisionStore{
		decide: func(logs []*domain.UsageLog) error {
			return fmt.Errorf("%w", domain.ErrPartitionUnavailable)
		},
	}
	r := New(UsageConfig{BatchSize: 2, Workers: 1}, fake, logger)
	far := time.Date(2099, 1, 1, 3, 4, 5, 0, time.UTC)
	orig := &domain.UsageLog{RequestID: "req-ts", Model: "m", Format: domain.FormatOpenAIChat, CreatedAt: far, TotalTokens: 5}
	r.Record(orig)
	r.flushLogs(context.Background())
	require.Equal(t, 1, r.Pending())
	require.Equal(t, far, r.pending[0].CreatedAt)
	require.Equal(t, "req-ts", r.pending[0].RequestID)
}

func TestUsageRoutingDoesNotAlterSuccessPath(t *testing.T) {
	fake := &routingDecisionStore{}
	r := New(UsageConfig{BatchSize: 2, Workers: 1}, fake, nil)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		r.Record(&domain.UsageLog{RequestID: fmt.Sprintf("ok-%d", i), Model: "m", Format: domain.FormatOpenAIChat, CreatedAt: now})
	}
	drained := r.flushLogs(context.Background())
	require.Equal(t, int64(4), drained)
	require.Zero(t, r.Pending())
	require.Equal(t, 4, fake.count())
}
