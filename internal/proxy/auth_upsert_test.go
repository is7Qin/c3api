// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestAuthUpsertUserImmediate 本地增量插入后 RequireJWT 依赖的 UserSnapshot 立即可查。
func TestAuthUpsertUserImmediate(t *testing.T) {
	a := NewAuth(nil, nil, nil)
	// 初始空快照 → 查找缺失 → 401 fail-closed
	_, ok := a.UserSnapshot(42)
	require.False(t, ok)

	a.UpsertUser(42, domain.UserSnapshot{Status: domain.UserStatusActive, Role: domain.RoleUser})
	snap, ok := a.UserSnapshot(42)
	require.True(t, ok)
	require.Equal(t, domain.UserStatusActive, snap.Status)
	require.Equal(t, domain.RoleUser, snap.Role)

	// 覆盖状态变更（禁用）立即可见
	a.UpsertUser(42, domain.UserSnapshot{Status: domain.UserStatusDisabled, Role: domain.RoleUser})
	snap, ok = a.UserSnapshot(42)
	require.True(t, ok)
	require.Equal(t, domain.UserStatusDisabled, snap.Status)
	require.Equal(t, domain.UserStatusDisabled, snap.Status)
}

// TestAuthUpsertUserDoesNotClobberOthers 多用户互不影响。
func TestAuthUpsertUserDoesNotClobberOthers(t *testing.T) {
	a := NewAuth(nil, nil, nil)
	a.UpsertUser(1, domain.UserSnapshot{Status: domain.UserStatusActive, Role: domain.RoleUser})
	a.UpsertUser(2, domain.UserSnapshot{Status: domain.UserStatusActive, Role: domain.RolePlatformAdmin})

	s1, _ := a.UserSnapshot(1)
	s2, _ := a.UserSnapshot(2)
	require.Equal(t, domain.RoleUser, s1.Role)
	require.Equal(t, domain.RolePlatformAdmin, s2.Role)
}
