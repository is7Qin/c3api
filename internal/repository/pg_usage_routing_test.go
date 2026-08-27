// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestUsageLogMissingPartitionMapsToDomain(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	far := time.Date(2099, 1, 1, 12, 0, 0, 0, time.UTC)
	log := &domain.UsageLog{
		RequestID: "routing-fail-1",
		Model:     "m",
		Format:    domain.FormatOpenAIChat,
		ErrorType: domain.ErrNone,
		CreatedAt: far,
	}
	err := repos.Usages.InsertBatch(ctx, []*domain.UsageLog{log})
	require.Error(t, err)
	require.True(t, errors.Is(err, domain.ErrPartitionUnavailable), "missing partition must map to domain.ErrPartitionUnavailable, got %v", err)
	require.Contains(t, err.Error(), "no partition of relation")
	// ensure no rows persisted and original timestamp not mutated
	require.Equal(t, far, log.CreatedAt)
	pool := pgTestPool(t)
	require.Equal(t, int64(0), pgCount(t, pool, `SELECT COUNT(*) FROM usage_logs WHERE request_id='routing-fail-1'`))
}
