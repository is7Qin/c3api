// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func TestErrLogSearchPG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	msg := "search upstream failure"
	log := &domain.UsageLog{
		RequestID:    "req-search-1",
		GroupID:      1,
		AccountID:    1,
		Model:        "gpt-4o",
		Format:       domain.FormatOpenAISearch,
		StatusCode:   429,
		ErrorType:    domain.Err429,
		ErrorMessage: &msg,
		LatencyMS:    42,
		CreatedAt:    time.Now().Truncate(time.Millisecond),
	}
	require.NoError(t, repos.ErrLogs.InsertBatch(ctx, []*domain.UsageLog{log}))
	from := log.CreatedAt.Add(-time.Second)
	to := log.CreatedAt.Add(time.Second)
	rows, err := repos.ErrLogs.QueryErrLogs(ctx, repository.ErrLogQuery{Format: string(domain.FormatOpenAISearch), From: &from, To: &to, Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, domain.FormatOpenAISearch, rows[0].Format)
	require.Equal(t, 429, rows[0].StatusCode)
	require.NotNil(t, rows[0].ErrorMessage)
	require.Equal(t, msg, *rows[0].ErrorMessage)
}
