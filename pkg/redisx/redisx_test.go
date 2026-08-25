// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package redisx

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
)

// TestOpenSuccess miniredis 下构造 + Ping 通过：客户端非 nil 且可用（foundation
// spec §5——pkg/redisx 两例门禁之一）。
func TestOpenSuccess(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := Open(Options{Addr: mr.Addr()})
	require.NoError(t, err)
	require.NotNil(t, c)
	t.Cleanup(func() { _ = Close(c) })
	require.NoError(t, c.Ping(t.Context()).Err(), "交付的客户端可继续 Ping")
}

// TestOpenPingFailure 对端不可达 → error（含 addr，不含密码明文）且客户端已回收。
func TestOpenPingFailure(t *testing.T) {
	mr := miniredis.RunT(t)
	addr := mr.Addr()
	mr.Close() // 先关服务再构造 → Ping 必败（v2.38 Close 无返回值）

	c, err := Open(Options{Addr: addr, Password: "super-secret"})
	require.Error(t, err)
	require.Nil(t, c)
	require.Contains(t, err.Error(), addr, "错误链含 addr 可归因")
	require.NotContains(t, err.Error(), "super-secret", "密码不入错误链（foundation spec §2.2 纪律 3）")
}
