// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/egress"
)

// sentinelTransport 一个必然失败的 RoundTripper：错误文本带标记，用来证明"这次
// 出栈到底用的是哪条 transport"——不是靠读私有字段，而是靠请求真的走了它。
type sentinelTransport struct {
	marker string
	hits   int
}

func (s *sentinelTransport) RoundTrip(*http.Request) (*http.Response, error) {
	s.hits++
	return nil, fmt.Errorf("sentinel-%s", s.marker)
}

// sdkClosedAddr 已释放的本机端口（拨它必然 ECONNREFUSED）。
func sdkClosedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

// TestCodexEgressTransportIsBindingOwned 钉住 spec §4.6 的两件事：
//
//  1. **transport 由出口决定**：直连出口用池底座，代理出口用 `Pool.For(spec)` 那条
//     （从底座克隆 + `Proxy` 指向代理）。判据不是读私有字段，而是看请求真的走了谁：
//     直连时底座的哨兵错误浮出；代理出口时错误里出现代理地址、且不再有底座哨兵。
//  2. **同传输不同出口必须是不同 client**：`clientKey` 的 `proxy` 维是 16 字节
//     摘要，两条出口各自命中各自的条目；同出口重复取仍是同一个 client（复用）。
//
// 反例（漏接线时的形态）：换代理后复用旧条目 ⇒ 配置换了、连接没换，请求继续打旧出口。
func TestCodexEgressTransportIsBindingOwned(t *testing.T) {
	base := &sentinelTransport{marker: "direct"}
	pool := egress.NewPool(&http.Client{Transport: base})
	a := NewCodex(nil, pool, RotationDeps{})
	cred := oauthCred(9, "at-1", "rt-1")
	params := &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"}

	// 1) 直连出口 ⇒ 请求经池底座（哨兵错误浮出）。
	_, err := a.GenerateImage(context.Background(), cred, pool.Direct(), params)
	require.Error(t, err)
	require.Contains(t, err.Error(), "sentinel-direct", "直连出口必须使用池底座的 transport")
	require.Equal(t, 1, base.hits)

	// 2) 代理出口 ⇒ 从底座克隆 + Proxy 指向代理；拨代理失败，错误里出现该地址，
	//    且**不再**是底座哨兵（说明走的不是底座那条 transport）。
	dead := sdkClosedAddr(t)
	host, portStr, err := net.SplitHostPort(dead)
	require.NoError(t, err)
	port, err := net.LookupPort("tcp", portStr)
	require.NoError(t, err)
	eg := pool.For(egress.New("http", host, port, "", ""))
	_, err = a.GenerateImage(context.Background(), cred, eg, params)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "sentinel-direct", "代理出口不得退回底座 transport")
	require.Contains(t, err.Error(), dead, "错误必须指向代理地址：error=%v", err)
	require.Equal(t, 1, base.hits, "底座不该被代理出口触达")

	// 3) 缓存分桶：同身份不同出口 ⇒ 不同 client；同出口重复 ⇒ 同一个 client。
	e, cDirect, err := a.clientFor(cred, pool.Direct(), domain.CodexResponsesTransportDefault)
	require.NoError(t, err)
	_, cProxy, err := a.clientFor(cred, eg, domain.CodexResponsesTransportDefault)
	require.NoError(t, err)
	_, cProxyAgain, err := a.clientFor(cred, eg, domain.CodexResponsesTransportDefault)
	require.NoError(t, err)
	require.NotSame(t, cDirect, cProxy, "换出口必须是新 client（构造期 transport 无法替换）")
	require.Same(t, cProxy, cProxyAgain, "同出口必须复用同一 client（连接池不重建）")

	// 4) 键里的 proxy 维确实是摘要维（不是同一个键命中两次）。
	keyDirect := clientKey{proxy: egressDigest(pool.Direct())}
	keyProxy := clientKey{proxy: egressDigest(eg)}
	require.NotEqual(t, keyDirect, keyProxy, "clientKey 必须含出口摘要维")
	require.Same(t, cProxy, testCachedClient(e, keyProxy), "代理出口的条目按含 digest 的键落桶")
	require.Same(t, cDirect, testCachedClient(e, keyDirect), "直连出口的条目按零值 digest 落桶")
}

// TestCodexEgressNotWiredIsExplicit 池未装配时**不静默直连**：没有出口可解析就
// 返回明确错误（装配期错误在运行期的唯一残影），而不是悄悄用 SDK 默认 transport。
func TestCodexEgressNotWiredIsExplicit(t *testing.T) {
	a := NewCodex(nil, nil, RotationDeps{})
	cred := oauthCred(9, "at-1", "rt-1")
	_, err := a.GenerateImage(context.Background(), cred, nil, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.ErrorIs(t, err, errEgressNotWired)
	require.Nil(t, a.DirectEgress(), "未装配没有直连出口可给")
}
