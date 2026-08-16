// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package cryptox

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewGroupKey(t *testing.T) {
	raw := NewGroupKey()
	require.Len(t, raw, 35) // ck- + 32 hex
	require.Equal(t, "ck-", raw[:3])
	require.NotEqual(t, raw, NewGroupKey(), "两次生成随机性（明文互不相同）")
}
