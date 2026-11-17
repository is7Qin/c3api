// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package egress

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// proxyOf 读出 client 装配的 Transport.Proxy 对给定请求给出的代理 URL（测试专用）。
//
// 前置条件：底座显式给了 *http.Transport（测试里都这样），于是 cloneTransport 走
// 常规克隆分支，出口 Transport 仍是 *http.Transport、Proxy 是 http.ProxyURL 的结果。
func proxyOf(t *testing.T, hc *http.Client) *url.URL {
	t.Helper()
	tr, ok := hc.Transport.(*http.Transport)
	require.True(t, ok, "底座是 *http.Transport ⇒ 出口必须是它的克隆（类型不变）")
	require.NotNil(t, tr.Proxy, "出口必须装配 Transport.Proxy")
	req, err := http.NewRequest(http.MethodGet, "https://api.ip.sb/ip", nil)
	require.NoError(t, err)
	u, err := tr.Proxy(req)
	require.NoError(t, err)
	require.NotNil(t, u)
	return u
}

// TestProxyURL 钉住代理 URL 的形态（这是 NewClient 与 Pool.For 唯一的实现点）：
// 凭据齐全 / 无用户名 / 有用户名无密码 / IPv6 方括号。
//
// 负例：把"有用户名无密码"写成 url.UserPassword(user, "") ⇒ URL 变 `user:@host`，
// 本用例红；漏掉 Spec.Addr 的方括号 ⇒ IPv6 两条红。
func TestProxyURL(t *testing.T) {
	cases := []struct {
		name string
		spec Spec
		want string
		// wantUser 是 url.Userinfo.String() 的期望值（"" = 无 userinfo）。
		wantUser string
		// wantPassword 报告 URL 是否带密码段——"有用户名无密码"必须没有 `:`。
		wantPassword bool
	}{
		{
			name:         "凭据齐全",
			spec:         New("http", "10.0.0.7", 3128, "u1", "p1"),
			want:         "http://u1:p1@10.0.0.7:3128",
			wantUser:     "u1:p1",
			wantPassword: true,
		},
		{
			name:     "无用户名",
			spec:     New("socks5", "10.0.0.7", 1080, "", ""),
			want:     "socks5://10.0.0.7:1080",
			wantUser: "",
		},
		{
			name:     "只有密码没有用户名",
			spec:     New("http", "10.0.0.7", 3128, "", "p1"),
			want:     "http://10.0.0.7:3128",
			wantUser: "",
		},
		{
			name:     "有用户名无密码",
			spec:     New("http", "10.0.0.7", 3128, "alice", ""),
			want:     "http://alice@10.0.0.7:3128",
			wantUser: "alice",
		},
		{
			name:     "IPv6 输入带方括号（New 归一）",
			spec:     New("socks5", "[::1]", 1080, "", ""),
			want:     "socks5://[::1]:1080",
			wantUser: "",
		},
		{
			name:         "IPv6 裸地址 + 凭据",
			spec:         New("https", "2001:db8::1", 8443, "u", "p"),
			want:         "https://u:p@[2001:db8::1]:8443",
			wantUser:     "u:p",
			wantPassword: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := proxyURL(tc.spec)
			require.Equal(t, tc.want, u.String())
			require.Equal(t, tc.spec.Scheme, u.Scheme)
			require.Equal(t, tc.spec.Addr(), u.Host, "Host 只由 Spec.Addr 拼（IPv6 方括号由此而来）")
			require.Equal(t, tc.wantUser, u.User.String(), "userinfo 形态")
			if u.User == nil {
				return
			}
			_, hasPassword := u.User.Password()
			require.Equal(t, tc.wantPassword, hasPassword, "无密码 ⇒ 不臆造空密码段")
		})
	}
}

// TestNewClientWiresSpecProxy 钉住 NewClient 的装配契约：按值克隆底座 + 专属
// Transport + Proxy 指向规格地址（探测取出口 IP 依赖这条）。
func TestNewClientWiresSpecProxy(t *testing.T) {
	base := &http.Client{Transport: &http.Transport{Proxy: nil}}
	got := NewClient(base, New("http", "10.0.0.7", 3128, "alice", "s3cret"))

	require.NotNil(t, got)
	require.NotSame(t, base, got, "按值克隆：返回的不是入参 base 本身")
	require.NotSame(t, base.Transport, got.Transport, "出口必须有专属连接池（换出口必须换池）")
	require.Equal(t, "http://alice:s3cret@10.0.0.7:3128", proxyOf(t, got).String())
}

// TestNewClientDoesNotMutateBase 钉住"装配只写克隆"：底座是直连基准（与 main 的
// httpx.NewClient{Proxy: nil} 同形态），探测造的 client 不得把代理写回底座——
// 否则**所有**直连流量都会被悄悄改道经某个代理。
func TestNewClientDoesNotMutateBase(t *testing.T) {
	baseTr := &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 3, TLSHandshakeTimeout: 7 * time.Second}
	base := &http.Client{
		Transport: baseTr,
		Timeout:   9 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	first := NewClient(base, New("socks5", "10.0.0.8", 1080, "u", "p"))
	second := NewClient(base, New("http", "10.0.0.9", 3128, "", ""))

	// 入参 base 与它那条 transport 一个字段都没变（指针同一 + Proxy 仍 nil）。
	baseTrAfter, ok := base.Transport.(*http.Transport)
	require.True(t, ok)
	require.Same(t, baseTr, baseTrAfter, "底座 Transport 指针必须原样（装配只改克隆）")
	require.Nil(t, baseTrAfter.Proxy, "底座不得被装上代理")
	require.Equal(t, 3, baseTrAfter.MaxIdleConnsPerHost)
	require.Equal(t, 7*time.Second, baseTrAfter.TLSHandshakeTimeout)
	require.Equal(t, 9*time.Second, base.Timeout)

	// 底座行为按值沿用（Timeout / CheckRedirect 的**行为**，不只是非 nil）。
	require.Equal(t, 9*time.Second, first.Timeout, "Timeout 沿用底座")
	require.NotNil(t, first.CheckRedirect, "CheckRedirect 沿用底座")
	require.ErrorIs(t, first.CheckRedirect(&http.Request{}, nil), http.ErrUseLastResponse)

	// 每次调用一个新的克隆与新的连接池（一次性 client 不复用、不注册）。
	require.NotSame(t, first, second, "两次调用得到两个 client")
	require.NotSame(t, first.Transport, second.Transport, "两次装配各有独立连接池")
	require.NotSame(t, first.Transport, baseTr, "出口 Transport 是克隆，不是底座")
}

// TestNewClientNilBase：base == nil ⇒ http.DefaultClient（与 NewPool 同款缺省），
// 且必须仍是**克隆**——把 DefaultClient 原样返回就等于让探测的
// CloseIdleConnections 关掉全局默认连接池。
func TestNewClientNilBase(t *testing.T) {
	got := NewClient(nil, New("http", "127.0.0.1", 3128, "", ""))

	require.NotNil(t, got)
	require.NotSame(t, http.DefaultClient, got, "nil base ⇒ 以 DefaultClient 为底座克隆，不返回它本身")
	require.Equal(t, http.DefaultClient.Timeout, got.Timeout)
	require.NotSame(t, http.DefaultTransport, got.Transport, "不得与全局默认 transport 共享连接池")
	require.Equal(t, "http://127.0.0.1:3128", proxyOf(t, got).String())

	// 一次性 client 由调用方收尾（关闭只影响自己那份连接池）。
	require.NotPanics(t, got.CloseIdleConnections)
}

// TestNewClientSharesProxyURLWithPoolFor 钉住"唯一实现点"：池里那条出口与
// NewClient 现造的出口，对同一 Spec 必须给出逐字节相同的代理 URL。
func TestNewClientSharesProxyURLWithPoolFor(t *testing.T) {
	spec := New("socks5", "10.0.0.11", 1080, "bob", "pw")
	pool := NewPool(&http.Client{Transport: &http.Transport{Proxy: nil}})

	fromPool := proxyOf(t, pool.For(spec).HTTP())
	fromNew := proxyOf(t, NewClient(nil, spec))

	require.Equal(t, proxyURL(spec).String(), fromPool.String(),
		"Pool.For 必须复用 proxyURL（唯一实现点）")
	require.Equal(t, fromPool.String(), fromNew.String(),
		"池里的出口与一次性出口必须同形")
	require.Equal(t, "socks5://bob:pw@10.0.0.11:1080", fromPool.String())
}
