// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/pkg/egress"
)

// TestEgressSOCKS5RemoteResolvesDomainAndAuth spec §9.3：socks5 代理下**目标以域名
// 到达代理**（Go 对 socks 恒远程解析——这正是 spec L1 的已知限制），且 RFC1929
// 用户名/口令认证凭据正确。替身把域名记下来后忽略地址、连真实上游替身，所以断言
// 能落在"客户端究竟让代理去解析什么"这件事上。
func TestEgressSOCKS5RemoteResolvesDomainAndAuth(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/chat/completions", r.URL.Path, "隧道内的请求路径原样到达上游")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "socks-ok")
	}))
	defer up.Close()

	socks := newEgressSOCKS5Server(t, hostPortOf(t, up.URL), "socks-user", "socks-pass")
	host, port := socks.hostPort()

	// 底座用默认 client（Transport=nil ⇒ 池克隆 http.DefaultTransport）：socks
	// 支持由 net/http 的 Proxy 分支提供，零新依赖。
	pool := egress.NewPool(nil)
	c := pool.For(egress.New("socks5", host, port, "socks-user", "socks-pass"))

	// 目标写成一个**不存在于本机 DNS 的域名**：如果客户端本地解析，这里一定失败；
	// 它成功了就证明"域名原样交给代理"（远程解析）。
	resp, err := c.HTTP.Get("http://egress-socks-target.test:8080/v1/chat/completions")
	require.NoError(t, err, "socks5 出口必须经代理建连（本地无法解析该域名）")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "socks-ok", string(body))

	domains, methods, auths, requests := socks.snapshot()
	require.Equal(t, [][2]string{{"egress-socks-target.test", "8080"}}, domains,
		"目标必须以**域名**（ATYP=0x03）到达代理：远程解析，端口原样")
	require.Len(t, auths, 1)
	require.Equal(t, [2]string{"socks-user", "socks-pass"}, auths[0], "RFC1929 凭据正确到达")
	require.Len(t, methods, 1)
	require.Contains(t, methods[0], byte(socks5MethodUserPass),
		"有凭据时客户端必须提供用户名/口令方法（0x02）")
	require.Equal(t, 1, requests)
}

// TestEgressSOCKS5WrongCredentialsFail 认证失败 ⇒ 请求错误（不静默直连、不退化成
// 匿名出口）。
func TestEgressSOCKS5WrongCredentialsFail(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	socks := newEgressSOCKS5Server(t, hostPortOf(t, up.URL), "right-user", "right-pass")
	host, port := socks.hostPort()

	pool := egress.NewPool(nil)
	c := pool.For(egress.New("socks5", host, port, "right-user", "wrong-pass"))

	_, err := c.HTTP.Get("http://egress-socks-target.test:8080/v1/chat/completions")
	require.Error(t, err, "socks5 认证失败必须变成请求错误")

	_, _, auths, _ := socks.snapshot()
	require.Len(t, auths, 1)
	require.Equal(t, [2]string{"right-user", "wrong-pass"}, auths[0],
		"代理看到的正是客户端凭据（错的就是错的）")
}

// TestEgressSOCKS5NoAuth 未配置凭据 ⇒ 客户端提供 0x00（匿名）且不发起子协商。
func TestEgressSOCKS5NoAuth(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	socks := newEgressSOCKS5Server(t, hostPortOf(t, up.URL), "", "")
	host, port := socks.hostPort()

	pool := egress.NewPool(nil)
	c := pool.For(egress.New("socks5", host, port, "", ""))
	resp, err := c.HTTP.Get("http://egress-socks-target.test/v1/responses")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	_, methods, auths, _ := socks.snapshot()
	require.Len(t, methods, 1)
	require.Contains(t, methods[0], byte(socks5MethodNoAuth))
	require.Empty(t, auths, "无凭据 ⇒ 不发起 RFC1929 子协商")
}
