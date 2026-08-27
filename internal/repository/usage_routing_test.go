// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestIsPartitionUnavailableMapping(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "23514", Message: `no partition of relation "usage_logs" found for row`}
	wrapped := &pgconn.PgError{Code: "23514", Message: "no partition of relation \"usage_logs\" found for row"}
	require.True(t, isPartitionUnavailable(wrapped))
	require.True(t, isPartitionUnavailable(pgErr))
	require.False(t, isPartitionUnavailable(errors.New("generic")))
	require.False(t, isPartitionUnavailable(&pgconn.PgError{Code: "23514", Message: "duplicate key"}))
	require.False(t, isPartitionUnavailable(&pgconn.PgError{Code: "23505", Message: `no partition of relation "usage_logs" found for row`}))
	require.False(t, isPartitionUnavailable(&pgconn.PgError{Code: "23514", Message: `no partition of relation "other_table" found for row`}))
	require.False(t, isPartitionUnavailable(&pgconn.PgError{Code: "23514", Message: `no partition of relation "usage_logs_archive" found for row`}))
}

func TestUsageRepoMapsPartitionUnavailable(t *testing.T) {
	orig := &pgconn.PgError{Code: "23514", Message: `no partition of relation "usage_logs" found for row`}
	mapped := mapPartitionError(orig)
	require.Error(t, mapped)
	require.True(t, errors.Is(mapped, domain.ErrPartitionUnavailable))
	var target *pgconn.PgError
	require.True(t, errors.As(mapped, &target))
	require.NotNil(t, target)
	require.Equal(t, "23514", target.Code)
	plain := errors.New("some other error")
	require.Equal(t, plain, mapPartitionError(plain))
	require.Nil(t, mapPartitionError(nil))
}
