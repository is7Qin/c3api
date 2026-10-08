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

// TestNewManagementKey 管理 key 明文：mk- 前缀 + 32 hex（长度 35）+ 随机性。
func TestNewManagementKey(t *testing.T) {
	raw := NewManagementKey()
	require.Len(t, raw, 35) // mk- + 32 hex
	require.Equal(t, "mk-", raw[:3])
	require.NotEqual(t, raw, NewManagementKey(), "两次生成随机性（明文互不相同）")
	require.NotEqual(t, "ck-", raw[:3], "前缀与管理 face 客户端 key 不重叠")
}
