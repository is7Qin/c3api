// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/pkg/egress"
)

// hostPortOf 取 URL 的 `host:port` 形态（CONNECT 目标就是这个形态）。
func hostPortOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	return u.Host
}

// TestEgressHTTPConnectProxyTunnel spec §9.3 / §4.2：https 目标走 `CONNECT host:port`，
// 代理凭据经 `Proxy-Authorization`（Basic）送达，隧道建立后请求内容原样穿过。
// 这是"出口 client 真的会把流量交给代理"的最小实证——`Pool.For(spec)` 造出的
// client 只在 `Transport.Proxy` 上动手，`CONNECT` 与凭据都由 Go 的 transport 写出，
// 所以断言只能落在代理替身的**接线层**（CONNECT 行 + 头）。
func TestEgressHTTPConnectProxyTunnel(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/responses", r.URL.Path, "隧道内的路径原样到达目标")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "tunneled-ok")
	}))
	defer up.Close()

	proxy := newEgressConnectProxy(t)
	proxy.requireAuth = basicAuthValue("egress-user", "egress-pass")

	// 底座用 TLS 替身自己的 client（信任其证书）：出口 client 按值克隆底座、只换
	// Transport.Proxy，所以 TLS 信任链随克隆保留。
	pool := egress.NewPool(up.Client())
	spec := specOfProxyURL(t, "http", proxy.URL, "egress-user", "egress-pass")
	c := pool.For(spec)
	require.Equal(t, spec.Key(), c.Spec.Key(), "同一规格复用同一出口 client")

	resp, err := c.HTTP.Get(up.URL + "/v1/responses")
	require.NoError(t, err, "CONNECT 隧道必须建立")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "tunneled-ok", string(body))

	targets, auths := proxy.recorded()
	require.Len(t, targets, 1, "https 目标必须发 CONNECT")
	require.Equal(t, hostPortOf(t, up.URL), targets[0], "CONNECT 目标是目标 host:port")
	require.Len(t, auths, 1)
	require.Equal(t, basicAuthValue("egress-user", "egress-pass"), auths[0],
		"代理凭据以 Basic Proxy-Authorization 送达（出口规格里的用户名/密码只在这条线上出现）")
}

// TestEgressHTTPPlainProxySendsAbsoluteURI plain http 目标不经 CONNECT：Go 的
// transport 把**绝对 URI** 发给代理，凭据同样在 `Proxy-Authorization` 上。
func TestEgressHTTPPlainProxySendsAbsoluteURI(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	proxy := newEgressForwardProxy(t)
	proxy.requireAuth = basicAuthValue("u", "p")

	pool := egress.NewPool(up.Client())
	c := pool.For(specOfProxyURL(t, "http", proxy.URL, "u", "p"))

	resp, err := c.HTTP.Get(up.URL + "/v1/chat/completions")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "/v1/chat/completions", gotPath, "目标侧看到的仍是原始路径")

	seen := proxy.requested()
	require.Len(t, seen, 1)
	require.True(t, strings.HasPrefix(seen[0], "GET http://"), "代理收到绝对 URI：%s", seen[0])
	require.Contains(t, seen[0], "/v1/chat/completions")
}

// TestEgressHTTPProxyAuthRequired407 CONNECT 被代理以 407 拒绝 ⇒ 请求失败（分类面
// 由 egress_failure_test.go 钉 network；这里钉"确实是被 407 拒的"）。
func TestEgressHTTPProxyAuthRequired407(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	proxy := newEgressConnectProxy(t)
	proxy.requireAuth = basicAuthValue("right", "right") // 客户端带的是另一组

	pool := egress.NewPool(up.Client())
	c := pool.For(specOfProxyURL(t, "http", proxy.URL, "wrong", "wrong"))

	resp, err := c.HTTP.Get(up.URL + "/v1/responses")
	require.Error(t, err, "407 必须变成请求错误（不静默直连）")
	if resp != nil {
		resp.Body.Close()
	}
	// Go 把 CONNECT 的非 200 响应变成 `resp.Status` 文本（不是数字码），断言用
	// 状态原因短语——它比数字更能说明"是代理在拒绝认证"。
	require.Contains(t, err.Error(), "Proxy Authentication Required",
		"错误里能看出是代理拒绝认证：%v", err)

	targets, auths := proxy.recorded()
	require.Len(t, targets, 1)
	require.Equal(t, basicAuthValue("wrong", "wrong"), auths[0], "代理看到的是客户端凭据")
}

// TestEgressHTTPConnectFailed CONNECT 被代理拒绝（502）⇒ 请求错误。这是"连不上
// 目标"与"代理本身不可用"之间最容易被静默吞掉的一条路径。
func TestEgressHTTPConnectFailed(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	proxy := newEgressConnectProxy(t)
	proxy.status = http.StatusBadGateway

	pool := egress.NewPool(up.Client())
	c := pool.For(specOfProxyURL(t, "http", proxy.URL, "", ""))

	_, err := c.HTTP.Get(up.URL + "/v1/responses")
	require.Error(t, err, "CONNECT 失败必须变成请求错误")
	require.Contains(t, err.Error(), "Bad Gateway",
		"错误里能看出是代理拒绝建隧道：%v", err)
}

// TestEgressHTTPProxyRefused 代理端口无人监听（拒连）⇒ 请求错误。三种代理链路失败
// （拒连 / 407 / CONNECT 失败）在分类面上等价：都是 statusOf=0 的传输层错误。
func TestEgressHTTPProxyRefused(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	// 取一个刚关闭的端口当地址：先监听再立刻关，端口短期内不会被复用。
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	pool := egress.NewPool(up.Client())
	c := pool.For(specOfProxyURL(t, "http", deadURL, "", ""))
	_, err := c.HTTP.Get(up.URL + "/v1/chat/completions")
	require.Error(t, err, "代理拒连必须变成请求错误")
}
