// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package egressapi

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件只钉**本包自己的**契约：槽位表的装配语义与"关全部口味"的边界。口味的派生
// （哪个口味该是 DisableCompression、连接配额怎么翻倍）属实现侧，用例在 pkg/egress。

// countingTransport 只数关闭次数：CloseIdleConnections 幂等且不报错，"少关一个连接池"
// 不会失败、只会缓慢涨内存，所以这条契约必须数得出来。
type countingTransport struct{ closes *atomic.Int64 }

func (t *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, http.ErrHandlerTimeout // 用例不发请求
}

func (t *countingTransport) CloseIdleConnections() { t.closes.Add(1) }

func clientOf(closes *atomic.Int64) *http.Client {
	return &http.Client{Transport: &countingTransport{closes: closes}}
}

// TestNewClientCopiesSlots：槽位表按值拷入——调用方事后改自己的数组不得影响已装配的
// 出口（实现侧 `For` 是先填本地数组再装配，若这里存的是指针/切片就会出现"装配完还能被
// 别人改槽位"的静默竞态）。
func TestNewClientCopiesSlots(t *testing.T) {
	var closes atomic.Int64
	httpHC, codexHC := clientOf(&closes), clientOf(&closes)

	var slots [NumFlavours]*http.Client
	slots[FlavourHTTP] = httpHC
	slots[FlavourNoCompression] = codexHC

	c := NewClient(Spec{Scheme: "socks5", Host: "10.0.0.1", Port: 1080}, slots)
	require.Same(t, httpHC, c.HTTP(), "HTTP 读的是标准口味槽位")
	require.Same(t, codexHC, c.NoCompression(), "NoCompression 读的是关闭压缩口味槽位")

	slots[FlavourHTTP] = nil
	slots[FlavourNoCompression] = nil
	require.Same(t, httpHC, c.HTTP(), "装配后改调用方的数组不得影响出口")
	require.Same(t, codexHC, c.NoCompression())
	require.Equal(t, "socks5", c.Spec.Scheme)
}

// TestClientZeroValueIsDirectAndNilSafe：零值 Client = 直连（Spec 零值、槽位全 nil），
// 且**所有读取面都对 nil 安全**——直连是正常状态，不是"缺配置"，热路径不得为它付一次
// nil 判断之外的东西。
func TestClientZeroValueIsDirectAndNilSafe(t *testing.T) {
	var c Client
	require.Equal(t, Spec{}, c.Spec, "零值 = 直连")
	require.Nil(t, c.HTTP(), "未派生 ⇒ 该口味没有 client")
	require.Nil(t, c.NoCompression())
	require.NotPanics(t, c.CloseIdle, "空槽位可关（无可关的连接）")

	var nilClient *Client
	require.NotPanics(t, nilClient.CloseIdle, "nil 出口也要能关：CloseClients 依赖这条")
}

// TestCloseIdleCoversEverySlot：每个槽位恰好关一次，同一 client 出现在两个槽位就关两次
// （CloseIdleConnections 幂等，不判重）。新增口味时槽位数变了、这条计数随之变化——
// "加了口味忘了关"因此在用例里也是可数的。
func TestCloseIdleCoversEverySlot(t *testing.T) {
	var closes atomic.Int64
	hc := clientOf(&closes)

	slots := [NumFlavours]*http.Client{hc, hc}
	NewClient(Spec{}, slots).CloseIdle()

	require.Equal(t, int64(NumFlavours), closes.Load(),
		"每个口味恰好关一次（当前 NumFlavours=%d）", NumFlavours)
}

// TestResourceKey：直连（零值 Spec，含 nil client）→ 零值；代理 → Spec.Key()。
// 两者必须在键空间里可分辨，否则配置权威无法表达"直连这条出口保留"。
func TestResourceKey(t *testing.T) {
	require.Equal(t, [16]byte{}, ResourceKey(nil), "nil client = 直连 = 零值键")
	require.Equal(t, [16]byte{}, ResourceKey(&Client{}), "零值 Spec = 直连 = 零值键")

	spec := Spec{Scheme: "http", Host: "10.0.0.7", Port: 3128}
	got := ResourceKey(&Client{Spec: spec})
	require.Equal(t, spec.Key(), got, "代理出口的资源键就是规格摘要")
	require.NotEqual(t, [16]byte{}, got)
}
