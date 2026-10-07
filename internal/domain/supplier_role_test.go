// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRoleSupplierValid 三级角色 Valid（spec 2026-10-09 §2.6）。
func TestRoleSupplierValid(t *testing.T) {
	require.True(t, RolePlatformAdmin.Valid())
	require.True(t, RoleSupplier.Valid())
	require.True(t, RoleUser.Valid())
	require.False(t, Role("nope").Valid())
}

// TestSupplierSurfaceRoles 可达集单一事实源（supplier + platform_admin）。
func TestSupplierSurfaceRoles(t *testing.T) {
	require.True(t, CanAccessSupplierSurface(RoleSupplier))
	require.True(t, CanAccessSupplierSurface(RolePlatformAdmin))
	require.False(t, CanAccessSupplierSurface(RoleUser), "user 不可达供应商面")
	require.False(t, CanAccessSupplierSurface(Role("nope")))
	require.ElementsMatch(t, []Role{RoleSupplier, RolePlatformAdmin}, SupplierSurfaceRoles())
}
