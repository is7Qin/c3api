// SPDX-License-Identifier: AGPL-3.0-or-later
package repository

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestErrLogSearchPG(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	schema := "c3api_test_errlog_search"
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + schema
	} else {
		dsn += "?search_path=" + schema
	}
	ctx := context.Background()
	pool, err := OpenPG(ctx, dsn, 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE; CREATE SCHEMA `+schema+`;`)
	require.NoError(t, err)
	repos, err := New(entsql.OpenDB(dialect.Postgres, db), true)
	require.NoError(t, err)
	require.NoError(t, repos.EnsureErrLogPartitioned(ctx, time.Now()))
	require.NoError(t, repos.EnsureUsageLogPartitioned(ctx, time.Now()))

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
	rows, err := repos.ErrLogs.QueryErrLogs(ctx, ErrLogQuery{Format: string(domain.FormatOpenAISearch), From: &from, To: &to, Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, domain.FormatOpenAISearch, rows[0].Format)
	require.Equal(t, 429, rows[0].StatusCode)
	require.NotNil(t, rows[0].ErrorMessage)
	require.Equal(t, msg, *rows[0].ErrorMessage)
}
