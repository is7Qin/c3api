// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package egress

import (
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// splitHostPort 把 httptest 的 `127.0.0.1:port` 拆成规格的三元组。
func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)
	return host, port
}

// TestPoolForReusesSameClient 钉住"按摘要复用同一 *Client 指针"——这是连接池
// 跨快照重建保留的唯一依据：若每次 For 都建新 client，代理连接池随每次 reload
// 重建，持久复用就是空话。
func TestPoolForReusesSameClient(t *testing.T) {
	pool := NewPool(nil)
	spec := New("Socks5", "10.0.0.1", 1080, "u", "p")

	first := pool.For(spec)
	require.NotNil(t, first)
	require.Equal(t, spec, first.Spec)
	// 同一份规格的不同构造（大小写/方括号写法）必须命中同一 client。
	second := pool.For(New("socks5", "[10.0.0.1]", 1080, "u", "p"))
	require.Same(t, first, second, "同键必须复用同一 *Client（含大小写与 IPv6 方括号归一）")
	require.Same(t, first, pool.For(spec))
	require.Equal(t, 1, pool.Size())

	// 摘要的任一维变化 ⇒ 新 client。
	other := pool.For(New("socks5", "10.0.0.1", 1080, "u", "p2"))
	require.NotSame(t, first, other, "改凭据 = 换一条出口线")
	require.Equal(t, 2, pool.Size())
}

// TestPoolDirectSingleton 直连出口是构造期单例：零值 spec 与 For 的零值入参都
// 返回同一个指针（未绑定账号的 Egress 指针同一性由这个单例保证）。
func TestPoolDirectSingleton(t *testing.T) {
	pool := NewPool(nil)
	direct := pool.Direct()
	require.NotNil(t, direct)
	require.NotNil(t, direct.HTTP)
	require.Equal(t, Spec{}, direct.Spec, "直连 = 零值 Spec")
	require.Same(t, direct, pool.Direct())
	require.Same(t, direct, pool.For(Spec{}), "For 零值 spec ⇒ Direct()")
	require.Same(t, http.DefaultClient, direct.HTTP, "base == nil ⇒ http.DefaultClient")
	require.Equal(t, 0, pool.Size(), "直连不占注册表条目")
}

// TestPoolNilBaseDoesNotPanic：base == nil 是测试路径常态，必须可用（不 panic、
// 且仍能按规格建 client）。
func TestPoolNilBaseDoesNotPanic(t *testing.T) {
	pool := NewPool(nil)
	require.NotPanics(t, func() {
		c := pool.For(New("http", "127.0.0.1", 3128, "", ""))
		require.NotNil(t, c)
		require.NotNil(t, c.HTTP)
		require.NotNil(t, c.HTTP.Transport)
	})
}

// TestPoolSpecsGetDistinctTransports：每条规格必须有**自己的** Transport（连接池
// 不可共享），且直连与代理不是同一个 Transport。
func TestPoolSpecsGetDistinctTransports(t *testing.T) {
	// 底座显式直连（与 main 的 httpx.NewClient{Proxy: nil} 同形态），便于断言
	// 「底座没被代理装配污染」。
	base := &http.Client{Transport: &http.Transport{Proxy: nil}}
	pool := NewPool(base)

	a := pool.For(New("http", "127.0.0.1", 3128, "", ""))
	b := pool.For(New("socks5", "127.0.0.1", 1080, "", ""))
	require.NotSame(t, a.HTTP.Transport, b.HTTP.Transport)

	tr, ok := a.HTTP.Transport.(*http.Transport)
	require.True(t, ok)
	require.NotNil(t, tr.Proxy, "代理 client 必须装配 Transport.Proxy")
	req, err := http.NewRequest(http.MethodGet, "http://upstream.example/", nil)
	require.NoError(t, err)
	proxyURL, err := tr.Proxy(req)
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:3128", proxyURL.String())

	// 克隆底座不得污染底座本身（底座的连接池仍归直连用）。
	baseTr, ok := base.Transport.(*http.Transport)
	require.True(t, ok)
	require.Nil(t, baseTr.Proxy, "底座 Transport 不得被代理装配改写")
	// 直连 client 的 Transport 就是底座本身（零改造）。
	require.Same(t, base.Transport, pool.Direct().HTTP.Transport)
}

// TestPoolPruneKeepsActiveAndRemovesRest 钉住 Prune 的取舍与幂等：活跃项保留
// （同一指针），其余移除（之后再 For 必然新建），重复调用不 panic。
func TestPoolPruneKeepsActiveAndRemovesRest(t *testing.T) {
	pool := NewPool(nil)
	keepSpec := New("socks5", "10.0.0.1", 1080, "", "")
	dropSpec := New("http", "10.0.0.2", 3128, "", "")
	keep := pool.For(keepSpec)
	drop := pool.For(dropSpec)
	require.Equal(t, 2, pool.Size())

	pool.Prune(map[[16]byte]struct{}{keepSpec.Key(): {}})
	require.Equal(t, 1, pool.Size())
	require.Same(t, keep, pool.For(keepSpec), "活跃规格必须保留同一 client")
	require.NotSame(t, drop, pool.For(dropSpec), "被移除的规格必须重建（旧 client 已关闭）")
	require.Equal(t, 2, pool.Size())

	require.NotPanics(t, func() {
		pool.Prune(nil)
		pool.Prune(nil)
	})
	require.Equal(t, 0, pool.Size(), "空 active 集 ⇒ 清空注册表")
}

// TestPoolPruneClosesIdleConnections 证明 Prune **先关闭空闲连接**再移除（不只是
// 丢引用）：经代理建立一条 keep-alive 连接后 prune，代理侧必须观测到连接关闭。
func TestPoolPruneClosesIdleConnections(t *testing.T) {
	var closed atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	proxy.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateClosed || st == http.StateHijacked {
			closed.Add(1)
		}
	}
	defer proxy.Close()
	host, port := splitHostPort(t, proxy.Listener.Addr().String())

	pool := NewPool(nil)
	spec := New("http", host, port, "", "")
	c := pool.For(spec)
	resp, err := c.HTTP.Get("http://upstream.invalid/x")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	pool.Prune(nil)
	require.Eventually(t, func() bool { return closed.Load() > 0 }, 2*time.Second, 10*time.Millisecond,
		"Prune 必须 CloseIdleConnections（代理侧应观测到连接关闭）")
	require.Equal(t, 0, pool.Size())
}

// TestPoolForRoutesThroughProxy：For 建出的 client 真的把请求交给代理（不是直连
// 目标），且认证头按规格生成。出口是否"生效"的最短可观测形态。
func TestPoolForRoutesThroughProxy(t *testing.T) {
	type seen struct {
		uri  string
		auth string
	}
	got := make(chan seen, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- seen{uri: r.RequestURI, auth: r.Header.Get("Proxy-Authorization")}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()
	host, port := splitHostPort(t, proxy.Listener.Addr().String())

	pool := NewPool(nil)
	c := pool.For(New("http", host, port, "alice", "s3cret"))
	resp, err := c.HTTP.Get("http://upstream.invalid/hello")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	select {
	case s := <-got:
		require.Equal(t, "http://upstream.invalid/hello", s.uri, "代理必须收到绝对 URI（说明请求确实经代理）")
		require.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("alice:s3cret")), s.auth)
	case <-time.After(2 * time.Second):
		t.Fatal("代理没有收到请求：出口 client 没有走代理")
	}
}
