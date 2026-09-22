// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package httpx

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClientReusesTransport(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := NewClient(TransportConfig{
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     10 * time.Second,
		DialTimeout:         5 * time.Second,
		ForceHTTP2:          false,
	})
	for i := 0; i < 3; i++ {
		resp, err := c.Get(srv.URL)
		require.NoError(t, err)
		resp.Body.Close()
	}
	require.Equal(t, 3, hits)
}

// TestTransportProxyDefaultsToDirect 默认直连：TransportConfig.Proxy 零值
// （nil）→ 产物 transport 的 Proxy 为 nil——不再隐式装配
// http.ProxyFromEnvironment（HTTP_PROXY 设置即静默改道上游请求，含凭据）。
func TestTransportProxyDefaultsToDirect(t *testing.T) {
	tr := NewTransport(TransportConfig{})
	require.Nil(t, tr.Proxy, "默认直连：不得隐式装配 http.ProxyFromEnvironment")
}

// TestTransportNilProxyIgnoresEnv Proxy=nil 直连：即便 HTTP_PROXY 环境变量
// 指向代理，上游请求仍直连目标（代理零命中），行为不随部署环境漂移。
func TestTransportNilProxyIgnoresEnv(t *testing.T) {
	var proxyHits int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHits++
	}))
	defer proxy.Close()
	// 全形态环境代理变量都指向代理；NO_PROXY 置空避免宿主环境豁免干扰。
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("https_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	var targetHits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits++
		_, _ = w.Write([]byte("ok"))
	}))
	defer target.Close()

	// Proxy 不设 = nil 直连（显式零值，无代理依赖断言）。
	c := NewClient(TransportConfig{
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     10 * time.Second,
		DialTimeout:         5 * time.Second,
		ForceHTTP2:          false,
	})
	resp, err := c.Get(target.URL)
	require.NoError(t, err)
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, 1, targetHits)
	require.Zero(t, proxyHits, "Proxy=nil 时 HTTP_PROXY 环境变量不得改道上游请求")
}

// TestTransportExplicitProxy Proxy 显式设置生效：请求经配置的代理转发
// （绝对 URI 形式），目标不被直连。
func TestTransportExplicitProxy(t *testing.T) {
	var proxyHits int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits++
		require.True(t, r.URL.IsAbs(), "经代理的请求应为绝对 URI（代理转发形态）")
		_, _ = w.Write([]byte("via-proxy"))
	}))
	defer proxy.Close()

	var targetHits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits++
		_, _ = w.Write([]byte("ok"))
	}))
	defer target.Close()

	// 显式代理来自配置字段；宿主环境代理变量置空避免歧义。
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	proxyURL, err := url.Parse(proxy.URL)
	require.NoError(t, err)
	c := NewClient(TransportConfig{
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     10 * time.Second,
		DialTimeout:         5 * time.Second,
		ForceHTTP2:          false,
		Proxy:               func(*http.Request) (*url.URL, error) { return proxyURL, nil },
	})
	resp, err := c.Get(target.URL)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, "via-proxy", string(body))
	require.Equal(t, 1, proxyHits)
	require.Zero(t, targetHits)
}
