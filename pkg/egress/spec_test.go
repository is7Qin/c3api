// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package egress_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/pkg/egress"
)

// TestEgressSpecOf 钉住 Spec 的**判据边界**：同一代理的两种写法必须同摘要
// （否则静态键抖动、在途计划被白白作废）；换线（scheme/端口/凭据）必须换摘要
// （否则换了出口却复用旧计划 = 上游看到的还是旧 IP）。
func TestEgressSpecOf(t *testing.T) {
	base := egress.New("socks5", "127.0.0.1", 1080, "u", "p")

	t.Run("same spec same key", func(t *testing.T) {
		require.Equal(t, base.Key(), egress.New("socks5", "127.0.0.1", 1080, "u", "p").Key())
	})

	t.Run("IPv6 brackets are canonicalized", func(t *testing.T) {
		bracketed := egress.New("http", "[::1]", 3128, "", "")
		bare := egress.New("http", "::1", 3128, "", "")
		require.Equal(t, bare.Key(), bracketed.Key(), "[::1] 与 ::1 是同一条线")
		require.Equal(t, "[::1]:3128", bare.Addr(), "Addr 负责补方括号")
	})

	t.Run("socks5 and socks5h are distinct", func(t *testing.T) {
		// 二者在 Go 里语义相同，但用户的拼法原样落库 ⇒ 摘要各自独立（不做归一改写）。
		require.NotEqual(t, base.Key(), egress.New("socks5h", "127.0.0.1", 1080, "u", "p").Key())
	})

	t.Run("missing username equals empty username", func(t *testing.T) {
		anon := egress.New("socks5", "127.0.0.1", 1080, "", "")
		require.Equal(t, anon.Key(), egress.New("socks5", "127.0.0.1", 1080, "", "").Key())
		require.NotEqual(t, anon.Key(), base.Key(), "有认证 ≠ 无认证")
	})

	t.Run("port and scheme and password change the key", func(t *testing.T) {
		require.NotEqual(t, base.Key(), egress.New("socks5", "127.0.0.1", 1081, "u", "p").Key())
		require.NotEqual(t, base.Key(), egress.New("http", "127.0.0.1", 1080, "u", "p").Key())
		require.NotEqual(t, base.Key(), egress.New("socks5", "127.0.0.1", 1080, "u", "p2").Key())
		require.NotEqual(t, base.Key(), egress.New("socks5", "host", 1080, "u", "p").Key())
	})

	t.Run("field concatenation must not collide", func(t *testing.T) {
		// 长度前缀编码的意义：("ab","c") 不得与 ("a","bc") 同摘要。
		require.NotEqual(t,
			egress.New("http", "ab", 1, "c", "").Key(),
			egress.New("http", "a", 1, "bc", "").Key())
	})

	t.Run("zero spec is direct", func(t *testing.T) {
		var zero egress.Spec
		require.Equal(t, "direct", zero.String())
		require.NotEqual(t, zero.Key(), base.Key(), "直连 ≠ 任何真实代理")
	})

	t.Run("String never leaks credentials", func(t *testing.T) {
		s := egress.New("https", "proxy.example.com", 8443, "alice", "s3cr3t")
		require.Equal(t, "https://proxy.example.com:8443", s.String())
		require.NotContains(t, s.String(), "alice")
		require.NotContains(t, s.String(), "s3cr3t")
		require.Equal(t, "proxy.example.com:8443", s.Addr())
	})
}
