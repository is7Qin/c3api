// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package pgretry

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

// TestIsRetryableTxErr 只认死锁(40P01)与锁不可用(55P03)；其余一律不可重试。
func TestIsRetryableTxErr(t *testing.T) {
	require.True(t, IsRetryableTxErr(&pgconn.PgError{Code: "40P01"}), "死锁可重试")
	require.True(t, IsRetryableTxErr(&pgconn.PgError{Code: "55P03"}), "锁不可用可重试")

	require.False(t, IsRetryableTxErr(&pgconn.PgError{Code: "23505"}), "唯一冲突不可重试")
	require.False(t, IsRetryableTxErr(&pgconn.PgError{Code: "40001"}), "序列化失败未列入白名单")
	require.False(t, IsRetryableTxErr(errors.New("boom")), "普通错误不重试")
	require.False(t, IsRetryableTxErr(nil), "nil 不重试")

	// 包装后仍可识别（errors.As 语义，调用方常在事务错误上包一层）。
	wrapped := fmt.Errorf("apply credit tx: %w", &pgconn.PgError{Code: "40P01"})
	require.True(t, IsRetryableTxErr(wrapped), "包装后的死锁仍可重试")
}
