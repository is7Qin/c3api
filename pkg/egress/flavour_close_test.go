// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package egress

import (
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// 口味关闭契约（设计 §6）。`http.Client.CloseIdleConnections` 是幂等的 ⇒ "少关了一个
// 连接池"不会报错、不会失败，只会缓慢涨内存。所以这条契约必须**数得出来**：
// 用可计数的底座 transport，断言"每个口味恰好关一次"。
//
// 替身必须实现 `proxyCloner`：否则 `cloneTransport` 会退回 `http.DefaultTransport`
// （那道降级规则是给"底座类型未知"准备的），spy 会跟着丢掉，门禁静默失效。

// spyTransport 可派生、可计数的底座：同时实现 proxyCloner 与 compressionDisabler，
// 于是一个底座能派生出"标准"与"关闭自动压缩"两份，各自计数器共享。
type spyTransport struct {
	closes     *atomic.Int64
	compressed bool
}

func (t *spyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, http.ErrHandlerTimeout // 用例不发请求
}

func (t *spyTransport) CloneWithProxy(func(*http.Request) (*url.URL, error)) http.RoundTripper {
	return &spyTransport{closes: t.closes, compressed: t.compressed}
}

func (t *spyTransport) CloneWithCompressionDisabled() http.RoundTripper {
	return &spyTransport{closes: t.closes, compressed: false}
}

func (t *spyTransport) CloseIdleConnections() { t.closes.Add(1) }

// plainSpy 只实现 proxyCloner（不能派生关闭压缩的副本）——覆盖"底座能力不足 ⇒
// 两个口味共用同一 client"的旧语义。
type plainSpy struct{ closes *atomic.Int64 }

func (t *plainSpy) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, http.ErrHandlerTimeout
}

func (t *plainSpy) CloneWithProxy(func(*http.Request) (*url.URL, error)) http.RoundTripper {
	return &plainSpy{closes: t.closes}
}

func (t *plainSpy) CloseIdleConnections() { t.closes.Add(1) }

// TestFlavourCloseCoversEverySlot 每个口味都被关闭——新增口味忘派生或忘关闭都会红。
func TestFlavourCloseCoversEverySlot(t *testing.T) {
	t.Run("derivable", func(t *testing.T) {
		var closes atomic.Int64
		p := NewPool(&http.Client{Transport: &spyTransport{closes: &closes}})
		require.True(t, p.canDeriveNoCompression, "底座实现了 compressionDisabler ⇒ 应当可派生")

		direct := p.Direct()
		require.NotNil(t, direct.HTTP(), "直连直出口味不得为 nil")
		require.NotNil(t, direct.NoCompression(), "直连的关闭压缩口味不得为 nil")
		require.NotSame(t, direct.HTTP(), direct.NoCompression(), "可派生 ⇒ 两个独立连接池")

		c := p.For(New("socks5", "10.0.0.1", 1080, "u", "p"))
		for f := range c.clients {
			require.NotNil(t, c.clients[f], "口味 %d 未派生 ⇒ 它的连接池永远不会被关闭", f)
		}
		require.NotSame(t, c.HTTP(), c.NoCompression())

		before := closes.Load()
		CloseClients([]*Client{c})
		require.Equal(t, int64(numFlavours), closes.Load()-before,
			"每个口味恰好关一次（当前 numFlavours=%d）", numFlavours)
	})

	t.Run("not derivable", func(t *testing.T) {
		var closes atomic.Int64
		p := NewPool(&http.Client{Transport: &plainSpy{closes: &closes}})
		require.False(t, p.canDeriveNoCompression)

		direct := p.Direct()
		require.NotNil(t, direct.HTTP())
		require.Same(t, direct.HTTP(), direct.NoCompression(),
			"底座不能派生 ⇒ 两个口味就是同一个 *http.Client（旧语义）")

		before := closes.Load()
		CloseClients([]*Client{direct})
		require.Equal(t, int64(numFlavours), closes.Load()-before,
			"同一 client 出现在两个槽位也各关一次：CloseIdleConnections 幂等，不需要判重")
	})
}

// TestFlavourAccessorsNeverNil 直连单例与代理出口的每个口味都必须可用：nil 槽位会让
// 调用点在运行期 panic，而 `For` 的循环填表是本设计"不可能漏"的根据。
func TestFlavourAccessorsNeverNil(t *testing.T) {
	var closes atomic.Int64
	p := NewPool(&http.Client{Transport: &spyTransport{closes: &closes}})

	for _, c := range []*Client{p.Direct(), p.For(New("http", "127.0.0.1", 8080, "", ""))} {
		require.NotNil(t, c.HTTP())
		require.NotNil(t, c.NoCompression())
	}
	require.Len(t, p.Detach(nil), 1, "Detach 应摘出全部已注册出口（直连不算）")
}
