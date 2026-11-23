// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// TestDigestCacheServesStaleDigestForInPlaceTemplateMutation 钉住"同指针 + 就地改
// 模板"这个**已知窗口**的真实行为：缓存摘要陈旧 ⇒ planKey 与指针内容分叉。
//
// 为什么必须有一条用例：template_share_alias_guard_test.go 证明的是"构造期定型"
// （已发布叶子的键不随共享对象的写而变），它**刻意传 nil cache**——恰好绕开了
// 活缓存这个最危险的组合。摘要缓存把"模板内容"从每次构造一次降价成每个作用域
// 一次，于是作用域内的第二次构造会命中**旧内容的字节**：
//
//	leaf1 := newSnapshotStatic(a, tp, …, cache)   // 摘要 = D({"m-1"})
//	tp.Models = []string{"m-1","m-2"}             // 就地改共享对象
//	leaf2 := newSnapshotStatic(a, tp, …, cache)   // 命中 ⇒ 仍是 D({"m-1"})
//	direct := newSnapshotStatic(a, tp, …, nil)    // 直接算 ⇒ D({"m-1","m-2"})
//
// 这是**共享对象的已知边界，不是本单元引入的缺陷，也不是本轮会修的窗口**：
//
//   - 生产没有就地改模板的路径。唯一的模板字段写是
//     service.validateTemplate（internal/service/service.go:907，字段写在 :911/:957），
//     作用对象是 handler 从请求体构造的 DTO，从不与快照/载入作用域共享。
//   - immutability 门禁（internal/repository/template_immutability_gate_test.go）
//     只认四种**选择子形状**（X.tpl.<f> / X.tpl = / X.Template.<f> / X.Template =），
//     而"把 *domain.Template 作函数参数、在函数里写它的字段"被它归为"构造、合法"
//     ——它自己的注入式负例就是这么写的。也就是说门禁守的是**形状**（谁经由视图
//     写），不是"模板不可变"这条语义。
//
// 所以这条缝由「门禁的形状责任 + code review」兜住，**不是完整证明**。本用例的
// 职责是把窗口的行为写成可执行事实：将来若改成"构造期把模板内容拷进叶/共享只读
// 摘要对象"，下面的 NotEqual 会变成 Equal，那正是窗口被真正关掉的信号。
func TestDigestCacheServesStaleDigestForInPlaceTemplateMutation(t *testing.T) {
	tp := &domain.Template{
		ID:             71,
		BaseURL:        "https://u/v1",
		CredentialType: credential.TypeAPIKey,
		Models:         []string{"m-1"},
	}
	account := digestFixtureAccount(tp)
	eg := directEgress()
	cache := newTemplateDigestCache()

	leaf1 := newSnapshotStatic(account, tp, nil, eg, cache)
	require.Equal(t, digestStrings([]string{"m-1"}), leaf1.key.modelsDigest)
	require.Equal(t, 1, cache.misses, "首枚构造未命中")
	require.Zero(t, cache.hits)

	// 就地改共享模板：同指针、新内容。生产无此路径（见上面注释），但门禁把它
	// 判为"构造、合法"，所以它必须在这里被显式钉住。
	tp.Models = []string{"m-1", "m-2"}
	leaf2 := newSnapshotStatic(account, tp, nil, eg, cache)
	direct := newSnapshotStatic(account, tp, nil, eg, nil)

	require.Equal(t, 1, cache.misses, "同指针必命中，不新增表项")
	require.Equal(t, 1, cache.hits, "第二次构造命中——这就是窗口本身")

	require.Equal(t, leaf1.key.modelsDigest, leaf2.key.modelsDigest,
		"命中重放的是**旧内容**的字节：同一作用域内改模板 ⇒ 摘要陈旧")
	require.Equal(t, digestStrings([]string{"m-1", "m-2"}), direct.key.modelsDigest,
		"直接计算（nil cache）读到的是当前内容")
	require.NotEqual(t, leaf2.key.modelsDigest, direct.key.modelsDigest,
		"陈旧摘要 ⇒ 与直接计算分叉")
	require.NotEqual(t, planKeyOf(leaf2), planKeyOf(direct),
		"整枚 planKey 与指针内容分叉：旧叶被复用，而消费者按新内容理解它")
	require.NotEqual(t, planKeyOf(leaf1), planKeyOf(direct))

	// 分叉只发生在**同一张表**里：换新作用域（下一次 reload/批次的形状）就恢复
	// 一致——这正是"表寿命 = 作用域"这条契约唯一能给的保证。
	fresh := newSnapshotStatic(account, tp, nil, eg, newTemplateDigestCache())
	require.Equal(t, planKeyOf(direct), planKeyOf(fresh),
		"新作用域按当前内容重算 ⇒ 与直接计算一致（陈旧是作用域内的事）")
	require.Equal(t, digestStrings([]string{"m-1", "m-2"}), fresh.key.modelsDigest)
	require.Equal(t, 1, cache.hits, "新作用域用自己的表，不影响旧表的计数")
}
