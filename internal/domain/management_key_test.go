// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestManagementKeyStatusValid 管理 key 状态值域（active/disabled；其余非法）。
func TestManagementKeyStatusValid(t *testing.T) {
	require.True(t, ManagementKeyStatusActive.Valid())
	require.True(t, ManagementKeyStatusDisabled.Valid())
	require.False(t, ManagementKeyStatus("bogus").Valid())
	require.False(t, ManagementKeyStatus("").Valid())
}
