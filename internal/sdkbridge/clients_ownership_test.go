// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

// 传输资源归属与配置可见性的边界回归（§3.2 凭据替换 / 账号移除 / 出口·transport
// 变更）。都是**单线程确定性交错**：不依赖并发概率，交错顺序就是生产里两条请求的
// 真实先后。
//
// 配置可见性只有一个写入口 ApplyBatch（执行控制器算好的资源指令）；请求路径
// （Client/Entry）只取/建，不剪枝、
// 不登记。

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/egress"
)

// TestClientTableStaleEntryCannotSeedLiveBucket 凭据替换窗口内的旧条目句柄不得把 client
// 写进**活桶**（永久用例）。
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
	stale := a.clients.Client(eOld, clientKey{}, func() *codexsdk.HTTPClient {
		return codexsdk.NewHTTPClient(eOld.auth)
	})
	require.NotNil(t, stale, "在飞句柄仍可完成本次工作（不把在飞请求打断）")
	require.Nil(t, testCachedClient(a, eNew, clientKey{}),
		"死句柄的写入不得进活桶（否则新凭据的请求会被旧 auth 的 client 送出去）")

	built := false
	fresh := a.clients.Client(eNew, clientKey{}, func() *codexsdk.HTTPClient {
		built = true
		return codexsdk.NewHTTPClient(eNew.auth)
	})
	require.True(t, built, "替换后首个取用必须重建（活桶里不得留旧 auth 的 client）")
	require.Same(t, fresh, testCachedClient(a, eNew, clientKey{}))
	require.NotSame(t, stale, fresh, "旧 auth 的 client 不可能被后续请求命中")
}

// TestClientTableRemovedAccountCannotRecreateBucket 账号移除后，旧句柄即使继续执行也
// 不得重建出**可复用**的桶；账号重新加回（同 accountID）不得命中上一轮的桶。
//
// 交错：请求已取得条目 → 账号从账号全集消失（控制面按差分下发 Delete 指令）→ 请求恢复。
// 桶挂在条目上 ⇒ 移除后表里没有条目，迟到写入无处可"注册"，也就不存在"账号级可复用
// 桶"这种跨越账号生命周期的资源。
func TestClientTableRemovedAccountCannotRecreateBucket(t *testing.T) {
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{})
	cred := &domain.AccountCredential{AccountID: 9, PATKey: "pat-removed"}
	keyWS := clientKey{transport: domain.CodexResponsesTransportWS.Key()}

	e, err := a.entryFor(cred)
	require.NoError(t, err)
	applyUniverse(t, a.clients, 1, map[int64]*egress.Client{})

	stale := a.clients.Client(e, keyWS, func() *codexsdk.HTTPClient {
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
	fresh := a.clients.Client(revived, keyWS, func() *codexsdk.HTTPClient {
		built = true
		return codexsdk.NewHTTPClient(revived.auth)
	})
	require.True(t, built, "重新加回后的首个取用必须重建（不得命中旧桶）")
	require.NotSame(t, stale, fresh)
}

// TestUsageLaneKeepsRegisteredProxyBucket usage 快照面（无 Selection ⇒ 恒 Direct/HTTP）
// 不得摘掉账号配置里在用的**代理**桶。
//
// 可见性由**配置批次的资源指令**决定；usage 只是"仍被调用的组合"。让一次 usage 请求重定义可见
// 性会把它 Close 掉并永久移出表——下一次代理请求就要重新握手，"跨窗口不重连"当场
// 失效。
func TestUsageLaneKeepsRegisteredProxyBucket(t *testing.T) {
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{})
	cred := &domain.AccountCredential{AccountID: 9, PATKey: "pat-usage-keep"}
	proxy := *a.DirectEgress()
	proxy.Spec.Scheme = "http"
	proxy.Spec.Host = "localhost"
	proxy.Spec.Port = 8000
	keyProxy := clientKey{proxy: egressDigest(&proxy), transport: domain.CodexResponsesTransportWS.Key()}

	// 配置面登记当前出口（reload 面）后再发请求。
	applyUniverse(t, a.clients, 1, map[int64]*egress.Client{cred.AccountID: &proxy})
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

	// 再一批同出口配置：直连桶是 usage 的合法组合，同样不得被摘。
	applyUniverse(t, a.clients, 2, map[int64]*egress.Client{cred.AccountID: &proxy})
	require.Same(t, usageClient, testCachedClient(a, e, clientKey{}))

	// 代理请求继续命中同一 client（跨窗口不重连的前提）。
	_, again, err := a.clientFor(cred, &proxy, domain.CodexResponsesTransportWS)
	require.NoError(t, err)
	require.Same(t, proxyClient, again, "代理请求必须命中同一 client")
}

// TestApplyBatchKeepsTransportVariantsAndDropsOldEgress 出口 / transport 变更的剪枝
// 口径：**只按 egress 维度**——同出口换 responses 传输是合法变体（旧桶不得误删），
// 换出口规格（代理密码轮换）则该出口下的全部传输桶都必须摘除（历史摘要不得无界
// 增长）。
func TestApplyBatchKeepsTransportVariantsAndDropsOldEgress(t *testing.T) {
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

	applyUniverse(t, a.clients, 1, map[int64]*egress.Client{cred.AccountID: &egA})
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
	applyUniverse(t, a.clients, 2, map[int64]*egress.Client{cred.AccountID: &egB})
	e = a.clients.Get(cred.AccountID)
	require.NotNil(t, e, "回收 client 桶不得动条目（Auth/usage 仍在）")
	require.Empty(t, testEntryBuckets(a, e), "旧出口摘要下的桶（ws 与 http）都必须摘除")
}

// TestApplyBatchDropsAccountsOutsideUniverse 账号移除 = 控制器按**账号全集差分**点名
// ResourceDelete ⇒ 整账号摘除。这条覆盖「没有会话池的账号」（池注册表差分看不到它们）。
func TestApplyBatchDropsAccountsOutsideUniverse(t *testing.T) {
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{})
	cred := &domain.AccountCredential{AccountID: 42, PATKey: "pat-42"}
	_, _, err := a.clientFor(cred, a.DirectEgress(), domain.CodexResponsesTransportDefault)
	require.NoError(t, err)
	require.NotNil(t, a.clients.Get(cred.AccountID))

	// 新批次的全集里只有别的账号 ⇒ 控制器点名 Delete(42) ⇒ 整账号摘除。
	applyUniverse(t, a.clients, 1, map[int64]*egress.Client{7: nil})
	require.Nil(t, a.clients.Get(cred.AccountID), "不在账号全集里的条目必须回收")

	// 空集合（清空部署）⇒ 全部摘除；nil 表示"该能力未装配"，no-op。
	_, _, err = a.clientFor(cred, a.DirectEgress(), domain.CodexResponsesTransportDefault)
	require.NoError(t, err)
	// 零值批次 = 该能力未装配（没有指令）⇒ 不得回收任何资源。
	a.clients.ApplyBatch(domain.ResourceBatch{})
	require.NotNil(t, a.clients.Get(cred.AccountID), "nil = 未装配该能力 ⇒ 不得回收")
	applyUniverse(t, a.clients, 3, map[int64]*egress.Client{})
	require.Nil(t, a.clients.Get(cred.AccountID), "空集合 = 没有任何账号 ⇒ 全部回收")
}

// TestRequestPathNeverPrunesBuckets 请求路径**不回收**资源（M3 的判据）：同账号连续
// 20 个出口版本，请求只累积各自的桶（每个版本都合法，只是不再是配置出口）；下一次
// 配置批次按当前出口摘除历史摘要——表长因此由配置约束，而不是由单次调用猜测。
func TestRequestPathNeverPrunesBuckets(t *testing.T) {
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{})
	cred := &domain.AccountCredential{AccountID: 11, PATKey: "pat-fallback"}
	base := *a.DirectEgress()
	var last *egress.Client
	for i := range 20 {
		eg := base
		eg.Spec = egress.New("http", "localhost", 8000, "", fmt.Sprintf("revision-%d", i))
		_, _, err := a.clientFor(cred, &eg, domain.CodexResponsesTransportWS)
		require.NoError(t, err)
		last = &eg
	}
	require.Len(t, testEntryBuckets(a, a.clients.Get(cred.AccountID)), 20,
		"请求路径不得以任何理由剪枝（配置不可由单个请求定义）")

	applyUniverse(t, a.clients, 1, map[int64]*egress.Client{cred.AccountID: last})
	kept := testEntryBuckets(a, a.clients.Get(cred.AccountID))
	require.Len(t, kept, 1, "配置批次按当前出口回收历史摘要")
	require.Equal(t, egressDigest(last), kept[0].proxy)
}

// TestApplyBatchRejectsStaleBatch 资源指令的应用有单调代号：旧批次可能在新批次**之后**
// 才应用（锁外执行），不得把可见性回退到旧配置，也不得删掉新配置里才出现的账号。
// 覆盖出口变更 + 账号增删（A/B 形状）。
func TestApplyBatchRejectsStaleBatch(t *testing.T) {
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{})
	base := *a.DirectEgress()
	egA := base
	egA.Spec = egress.New("http", "localhost", 8000, "", "")
	egB := base
	egB.Spec = egress.New("http", "localhost", 8001, "", "")
	cred1 := &domain.AccountCredential{AccountID: 1, PATKey: "pat-1"}
	cred2 := &domain.AccountCredential{AccountID: 2, PATKey: "pat-2"}
	cred3 := &domain.AccountCredential{AccountID: 3, PATKey: "pat-3"}
	keyB := clientKey{proxy: egressDigest(&egB), transport: domain.CodexResponsesTransportDefault.Key()}

	// 旧配置的资源：账号 1 在出口 A、账号 3 在直连。
	applyUniverse(t, a.clients, 1, map[int64]*egress.Client{1: &egA, 3: nil})
	_, oldA, err := a.clientFor(cred1, &egA, domain.CodexResponsesTransportDefault)
	require.NoError(t, err)
	_, _, err = a.clientFor(cred3, a.DirectEgress(), domain.CodexResponsesTransportDefault)
	require.NoError(t, err)

	// 新批次（代号 2）：账号 1 改到出口 B、账号 3 删除、账号 2 新增。
	applyUniverse(t, a.clients, 2, map[int64]*egress.Client{1: &egB, 2: nil})
	require.Nil(t, a.clients.Get(3), "新批次删除的账号整账号摘除")
	e1 := a.clients.Get(1)
	require.NotNil(t, e1)
	require.Empty(t, testEntryBuckets(a, e1), "出口变更 ⇒ 旧摘要桶摘除")
	_, newB, err := a.clientFor(cred1, &egB, domain.CodexResponsesTransportDefault)
	require.NoError(t, err)
	require.NotSame(t, oldA, newB)
	_, _, err = a.clientFor(cred2, a.DirectEgress(), domain.CodexResponsesTransportDefault)
	require.NoError(t, err)
	require.NotNil(t, a.clients.Get(2), "新批次新增的账号有条目")

	// 旧批次（代号 1）迟到：整体 no-op。
	applyUniverse(t, a.clients, 1, map[int64]*egress.Client{1: &egA, 3: nil})
	require.NotNil(t, a.clients.Get(2), "旧批次不得删掉新配置才出现的账号")
	require.Nil(t, a.clients.Get(3), "旧批次不得让已删除账号复活")
	require.Same(t, newB, testCachedClient(a, a.clients.Get(1), keyB),
		"旧批次不得把配置回退到出口 A（B 桶必须仍在）")

	// 更新的批次仍被接受（守卫只拒旧号）。
	applyUniverse(t, a.clients, 3, map[int64]*egress.Client{1: &egA})
	require.Empty(t, testEntryBuckets(a, a.clients.Get(1)), "代号 3 = 更新的配置：B 桶摘除")
}

// TestApplyBatchGatesRemovalsWithBatchSequence 账号删除与可见性共享**同一个**批次准入：
// 旧批次（按当时的集合列出了 Delete）迟到时不得删掉新配置重建出来的条目。判据是
// "过期批次整体 no-op"，不是"删除本身幂等"。
func TestApplyBatchGatesRemovalsWithBatchSequence(t *testing.T) {
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{})
	cred := &domain.AccountCredential{AccountID: 31, PATKey: "pat-31"}
	_, _, err := a.clientFor(cred, a.DirectEgress(), domain.CodexResponsesTransportDefault)
	require.NoError(t, err)
	old := a.clients.Get(cred.AccountID)
	require.NotNil(t, old)

	// 批次 2：账号仍在全集，资源保留。
	applyUniverse(t, a.clients, 2, map[int64]*egress.Client{cred.AccountID: nil})
	require.Same(t, old, a.clients.Get(cred.AccountID))

	// 旧批次（代号 1）迟到，全集里没有这个账号：整个批次 no-op。
	applyUniverse(t, a.clients, 1, map[int64]*egress.Client{})
	require.Same(t, old, a.clients.Get(cred.AccountID),
		"旧批次的账号缺失必须被同一个 gen 准入拒掉（不得删掉新配置的条目）")

	// 只带 nil 的调用同样受准入约束（nil 不是"跳过判定"的旁路）。
	// 零值批次同样受"无指令即不动"约束（nil 不是"跳过判定"的旁路）。
	a.clients.ApplyBatch(domain.ResourceBatch{})
	require.Same(t, old, a.clients.Get(cred.AccountID))

	// 更新的批次仍然可以删除（守卫只拒旧号）。
	applyUniverse(t, a.clients, 3, map[int64]*egress.Client{})
	require.Nil(t, a.clients.Get(cred.AccountID), "更新的批次必须能删除")
}

// TestUsageLaneDoesNotWriteConfig usage 面的永久判据：账号尚无配置登记时，usage 面
// **不得**用它的直连出口定义可见性——否则随后代理面换传输时就会把在用的代理桶摘掉并
// 永久 Close（"跨窗口不重连"失效）。
func TestUsageLaneDoesNotWriteConfig(t *testing.T) {
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{})
	cred := &domain.AccountCredential{AccountID: 21, PATKey: "pat-usage"}
	proxy := *a.DirectEgress()
	proxy.Spec = egress.New("http", "localhost", 8002, "", "")

	_, _, err := a.usageClientFor(cred) // usage 先到：不写配置
	require.NoError(t, err)
	_, ws1, err := a.clientFor(cred, &proxy, domain.CodexResponsesTransportWS)
	require.NoError(t, err)
	_, _, err = a.clientFor(cred, &proxy, domain.CodexResponsesTransportDefault)
	require.NoError(t, err)
	_, ws2, err := a.clientFor(cred, &proxy, domain.CodexResponsesTransportWS)
	require.NoError(t, err)
	require.Same(t, ws1, ws2, "usage 不得定义可见性：换传输后代理 ws 桶必须仍在（命中同一 client）")
}
