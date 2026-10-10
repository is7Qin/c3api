// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRoleAtLeast 角色层级 user ⊂ supplier ⊂ platform_admin（spec 2026-10-09 §2.1）：
// 正例（同级/升级）真，负例（降级）假，非法角色 fail-closed 假。
func TestRoleAtLeast(t *testing.T) {
	require.True(t, AtLeast(RoleUser, RoleUser))
	require.True(t, AtLeast(RoleSupplier, RoleUser))
	require.True(t, AtLeast(RolePlatformAdmin, RoleSupplier))
	require.True(t, AtLeast(RoleSupplier, RoleSupplier))
	require.True(t, AtLeast(RolePlatformAdmin, RolePlatformAdmin))
	require.False(t, AtLeast(RoleUser, RoleSupplier))
	require.False(t, AtLeast(RoleUser, RolePlatformAdmin))
	require.False(t, AtLeast(RoleSupplier, RolePlatformAdmin))
	// 非法角色 fail-closed：任一实参非法 ⇒ false。
	require.False(t, AtLeast(Role("bogus"), RoleUser))
	require.False(t, AtLeast(RoleUser, Role("")))
	require.False(t, AtLeast(Role(""), Role("")))
}

// TestManagementKeyStatusValid 管理 key 状态值域（active/disabled；其余非法）。
func TestManagementKeyStatusValid(t *testing.T) {
	require.True(t, ManagementKeyStatusActive.Valid())
	require.True(t, ManagementKeyStatusDisabled.Valid())
	require.False(t, ManagementKeyStatus("bogus").Valid())
	require.False(t, ManagementKeyStatus("").Valid())
}
