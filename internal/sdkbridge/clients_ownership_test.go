// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

// 传输资源归属与活跃集的边界回归（§3.2 凭据替换 / 账号移除 / 出口·transport 变更）。
// 这四条都是**单线程确定性交错**：不依赖并发概率，交错顺序就是生产里两条请求的真实
// 先后。

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/egress"
)

// TestClientTableStaleEntryCannotSeedLiveBucket 凭据替换窗口内的旧条目句柄不得把 client
// 写进**活桶**（B4/B1 的永久用例）。
//
// 交错（生产里两次请求的真实先后，确定性复现）：
//
//	请求 A: entryFor(old) → 拿到条目 e_old（尚未取 client）
//	请求 B: entryFor(new) → 条目重建 + 全桶摘除
//	请求 A: Client(e_old, ...) → 迟到写入
//	请求 C: Client(当前条目, ...) → 必须重建
//
// 桶若挂在**账号**上，A 的迟到写入会落进活桶并被 C 命中——新凭据的请求持续用旧 auth
// 出网（PAT 轮换 = 旧 token 继续承载流量；CodexAccountID 改绑 = 跨上游账号串台）。
// 桶挂在**条目**上时，A 的写入只能落进死条目自己的表，对 C 不可见。
func TestClientTableStaleEntryCannotSeedLiveBucket(t *testing.T) {
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{})
	oldCred := &domain.AccountCredential{AccountID: 9, PATKey: "pat-old"}
	newCred := &domain.AccountCredential{AccountID: 9, PATKey: "pat-new"}

	eOld, err := a.entryFor(oldCred)
	require.NoError(t, err)
	eNew, err := a.entryFor(newCred)
	require.NoError(t, err)
	require.NotSame(t, eOld, eNew, "凭据替换 ⇒ 条目重建")
	require.Same(t, eNew, a.clients.Get(9), "当前条目已是新条目")

	// 请求 A 恢复执行：迟到写入只落在死条目自己的桶里。
	stale := a.clients.Client(eOld, clientKey{}, true, func() *codexsdk.HTTPClient {
		return codexsdk.NewHTTPClient(eOld.auth)
	})
	require.NotNil(t, stale, "在飞句柄仍可完成本次工作（不把在飞请求打断）")
	require.Nil(t, testCachedClient(a, eNew, clientKey{}),
		"死句柄的写入不得进活桶（否则新凭据的请求会被旧 auth 的 client 送出去）")

	built := false
	fresh := a.clients.Client(eNew, clientKey{}, true, func() *codexsdk.HTTPClient {
		built = true
		return codexsdk.NewHTTPClient(eNew.auth)
	})
	require.True(t, built, "替换后首个取用必须重建（活桶里不得留旧 auth 的 client）")
	require.Same(t, fresh, testCachedClient(a, eNew, clientKey{}))
	require.NotSame(t, stale, fresh, "旧 auth 的 client 不可能被后续请求命中")
}

// TestClientTableRemovedAccountCannotRecreateBucket 账号移除后，旧句柄即使继续执行也
// 不得重建出**可复用**的桶；账号重新加回（同 accountID）不得命中上一轮的桶（B3）。
//
// 交错：请求已取得条目 → 账号被移除（池注册表差分 → RemoveAccount）→ 请求恢复执行。
// 桶挂在条目上 ⇒ 移除后表里没有条目，迟到写入无处可"注册"，也就不存在"账号级可复用
// 桶"这种跨越账号生命周期的资源。
func TestClientTableRemovedAccountCannotRecreateBucket(t *testing.T) {
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{})
	cred := &domain.AccountCredential{AccountID: 9, PATKey: "pat-removed"}
	keyWS := clientKey{transport: domain.CodexResponsesTransportWS.Key()}

	e, err := a.entryFor(cred)
	require.NoError(t, err)
	a.RemoveAccount(cred.AccountID)

	stale := a.clients.Client(e, keyWS, true, func() *codexsdk.HTTPClient {
		return codexsdk.NewHTTPClient(e.auth)
	})
	require.NotNil(t, stale, "在飞句柄仍可完成本次工作")
	require.Nil(t, a.clients.Get(cred.AccountID),
		"移除后不得留下当前条目（桶随条目 ⇒ 不存在账号级可复用桶）")

	// 账号被重新加回：全新条目 + 全新桶。
	revived, err := a.entryFor(cred)
	require.NoError(t, err)
	require.NotSame(t, e, revived, "重新加回 = 新条目")
	require.Empty(t, testEntryBuckets(a, revived), "旧句柄预留的桶不得出现在新条目里")

	built := false
	fresh := a.clients.Client(revived, keyWS, true, func() *codexsdk.HTTPClient {
		built = true
		return codexsdk.NewHTTPClient(revived.auth)
	})
	require.True(t, built, "重新加回后的首个取用必须重建（不得命中旧桶）")
	require.NotSame(t, stale, fresh)
}

// TestUsageLaneKeepsActiveProxyBucket usage 快照面（无 Selection ⇒ 恒 Direct/HTTP）不得
// 摘掉账号在用的**代理**桶（N1 / oracle B2 的永久用例，momus N1 探针等价）。
//
// 活跃集由**账号当前配置**决定；usage 只是"仍被调用的组合"（§3.2）。让一次 usage 请求
// 重定义活跃集会把它 Close 掉并永久移出表——下一次代理请求就要重新握手，"跨窗口不重连"
// 当场失效。
func TestUsageLaneKeepsActiveProxyBucket(t *testing.T) {
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{})
	cred := &domain.AccountCredential{AccountID: 9, PATKey: "pat-usage-keep"}
	proxy := *a.DirectEgress()
	proxy.Spec.Scheme = "http"
	proxy.Spec.Host = "localhost"
	proxy.Spec.Port = 8000
	keyProxy := clientKey{proxy: egressDigest(&proxy), transport: domain.CodexResponsesTransportWS.Key()}

	_, proxyClient, err := a.clientFor(cred, &proxy, domain.CodexResponsesTransportWS)
	require.NoError(t, err)

	// usage 面（GetUsageSnapshot 的取 client 路径）：首次构造 Direct/HTTP 桶。
	_, usageClient, err := a.usageClientFor(cred)
	require.NoError(t, err)

	e := a.clients.Get(cred.AccountID)
	require.Same(t, proxyClient, testCachedClient(a, e, keyProxy),
		"usage 面调用不得摘掉在用的代理桶")
	require.Same(t, usageClient, testCachedClient(a, e, clientKey{}),
		"usage 的 Direct/HTTP 组合必须保留")

	// 代理请求继续命中同一 client（跨窗口不重连的前提）。
	_, again, err := a.clientFor(cred, &proxy, domain.CodexResponsesTransportWS)
	require.NoError(t, err)
	require.Same(t, proxyClient, again, "代理请求必须命中同一 client")
}

// TestReconcileKeepsTransportVariantsAndDropsOldEgress 出口 / transport 变更的剪枝口径：
// **只按 egress 维度**——同出口换 responses 传输是合法变体（旧桶不得误删），换出口规格
// （代理密码轮换）则该出口下的全部传输桶都必须摘除（历史摘要不得无界增长）。
func TestReconcileKeepsTransportVariantsAndDropsOldEgress(t *testing.T) {
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{})
	cred := &domain.AccountCredential{AccountID: 9, PATKey: "pat-egress"}
	egA := *a.DirectEgress()
	egA.Spec.Scheme = "http"
	egA.Spec.Host = "localhost"
	egA.Spec.Port = 8000
	egA.Spec.Password = "revision-a"
	egB := egA
	egB.Spec.Password = "revision-b"

	keyWS := clientKey{proxy: egressDigest(&egA), transport: domain.CodexResponsesTransportWS.Key()}
	keyHTTP := clientKey{proxy: egressDigest(&egA), transport: domain.CodexResponsesTransportDefault.Key()}

	_, wsA, err := a.clientFor(cred, &egA, domain.CodexResponsesTransportWS)
	require.NoError(t, err)
	_, httpA, err := a.clientFor(cred, &egA, domain.CodexResponsesTransportDefault)
	require.NoError(t, err)

	e := a.clients.Get(cred.AccountID)
	require.Len(t, testEntryBuckets(a, e), 2, "同出口 × 两种 responses 传输各一条")
	require.Same(t, wsA, testCachedClient(a, e, keyWS), "换传输不得误删同出口的旧桶")
	require.Same(t, httpA, testCachedClient(a, e, keyHTTP))

	_, againWS, err := a.clientFor(cred, &egA, domain.CodexResponsesTransportWS)
	require.NoError(t, err)
	require.Same(t, wsA, againWS, "ws 请求命中旧桶（不重拨）")

	// 出口规格变更（= 配置变更）⇒ 旧摘要下的桶（两种传输）全部摘除，条目本身不动。
	a.clients.Reconcile(map[int64]*egress.Client{cred.AccountID: &egB})
	e = a.clients.Get(cred.AccountID)
	require.NotNil(t, e, "回收 client 桶不得动条目（Auth/usage 仍在）")
	require.Empty(t, testEntryBuckets(a, e), "旧出口摘要下的桶（ws 与 http）都必须摘除")
}

// TestReconcileDropsAccountsOutsideActiveSet 配置面回收用**全量**账号集：账号不在集合里
// ⇒ 整账号摘除。这条覆盖「没有会话池的账号」（池注册表差分看不到它们，momus N7）。
func TestReconcileDropsAccountsOutsideActiveSet(t *testing.T) {
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{})
	cred := &domain.AccountCredential{AccountID: 42, PATKey: "pat-42"}
	_, _, err := a.clientFor(cred, a.DirectEgress(), domain.CodexResponsesTransportDefault)
	require.NoError(t, err)
	require.NotNil(t, a.clients.Get(cred.AccountID))

	// 集合里只有别的账号 ⇒ 42 整账号摘除。
	a.clients.Reconcile(map[int64]*egress.Client{7: nil})
	require.Nil(t, a.clients.Get(cred.AccountID), "不在账号集合里的条目必须回收")

	// 空集合（清空部署）⇒ 全部摘除；nil 表示"该能力未装配"，no-op。
	_, _, err = a.clientFor(cred, a.DirectEgress(), domain.CodexResponsesTransportDefault)
	require.NoError(t, err)
	a.clients.Reconcile(nil)
	require.NotNil(t, a.clients.Get(cred.AccountID), "nil = 未装配该能力 ⇒ 不得回收")
	a.clients.Reconcile(map[int64]*egress.Client{})
	require.Nil(t, a.clients.Get(cred.AccountID), "空集合 = 没有任何账号 ⇒ 全部回收")
}

// TestClientCacheFallbackBoundedWithoutConfig 未登记配置的账号（独立装配/单元用例）也
// 不得让表随规格版本无界增长：请求侧只在**首次**兜底登记出口，之后的历史摘要照摘——
// 表长恒 ≤ 首版 + 本次，与「20 个代理密码版本」这一形状无关。
//
// 生产装配下 Reconcile 已在 reload 面登记过，请求侧不会参与定义活跃集（见
// TestClientCacheBoundedByActiveEgressSet 的配置面版本）。
func TestClientCacheFallbackBoundedWithoutConfig(t *testing.T) {
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{})
	cred := &domain.AccountCredential{AccountID: 11, PATKey: "pat-fallback"}
	base := *a.DirectEgress()
	for i := range 20 {
		eg := base
		eg.Spec = egress.New("http", "localhost", 8000, "", fmt.Sprintf("revision-%d", i))
		_, _, err := a.clientFor(cred, &eg, domain.CodexResponsesTransportWS)
		require.NoError(t, err)
	}
	require.LessOrEqual(t, len(testEntryBuckets(a, a.clients.Get(cred.AccountID))), 2,
		"退回请求侧兜底时表长也必须是有界的（首版 + 本次）")
}
