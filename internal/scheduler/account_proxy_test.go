// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/egress"
)

// testEgressPool 是 fixture 的出口池（绝大多数用例不关心出口，直连即可）。
var testEgressPool = egress.NewPool(nil)

// directEgress 返回直连出口 client——测试 fixture 的默认出口。生产路径一律由
// buildSnapshots 从 Config.Egress 解析，不存在这个入口。
func directEgress() *egress.Client { return testEgressPool.Direct() }

// proxySched 构造注入了出口池的调度器（组 10，已武装编译道）。
func proxySched(t *testing.T, pool *egress.Pool, accs ...*domain.Account) *Scheduler {
	t.Helper()
	cfg := testCfg()
	cfg.Egress = pool
	return newSchedWithCfg(t, cfg, newMemLoader(map[int64][]*domain.Account{10: accs}))
}

// proxyAcc 造一个绑定到 px 的账号（ProxyID 已落、Proxy 已展开——装载器的产物）。
func proxyAcc(t *testing.T, id int64, tpl *domain.Template, px *domain.Proxy) *domain.Account {
	t.Helper()
	a := acc(id, tpl, 4)
	proxyID := px.ID
	a.ProxyID = &proxyID
	a.Proxy = px
	return a
}

// TestSelectEgressAlwaysResolved 钉住装配面（spec §9.2「装配」行）：
// `Selection.Egress` **恒非 nil**，未绑定 = `Pool.Direct()` 的**同一个指针**，
// 绑定 = 该规格在池里的 client（摘要相等即同一条线）。
func TestSelectEgressAlwaysResolved(t *testing.T) {
	tplA := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})

	t.Run("未绑定 ⇒ Direct 同指针", func(t *testing.T) {
		pool := egress.NewPool(nil)
		s := proxySched(t, pool, acc(1, tplA, 4))
		sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
		require.NoError(t, err)
		defer sel.Release()
		require.NotNil(t, sel.Egress, "Egress 恒非 nil（热路径不写 nil 判断）")
		require.Same(t, pool.Direct(), sel.Egress, "未绑定必须复用直连单例（同指针）")
		require.Equal(t, egress.Spec{}, sel.Egress.Spec, "直连 = 零值 Spec")
	})

	t.Run("绑定 ⇒ 池内该规格的 client", func(t *testing.T) {
		pool := egress.NewPool(nil)
		px := &domain.Proxy{ID: 7, Scheme: domain.ProxySchemeSOCKS5, Host: "10.1.2.3", Port: 1080,
			Username: "u", Password: "p", Enabled: true}
		s := proxySched(t, pool, proxyAcc(t, 1, tplA, px))
		sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
		require.NoError(t, err)
		defer sel.Release()
		require.NotNil(t, sel.Egress)
		require.NotSame(t, pool.Direct(), sel.Egress)
		require.Equal(t, egress.New("socks5", "10.1.2.3", 1080, "u", "p").Key(), sel.Egress.Spec.Key(),
			"绑定的出口摘要必须与装载展开的规格一致")
		require.Same(t, pool.For(egress.New("socks5", "10.1.2.3", 1080, "u", "p")), sel.Egress,
			"同一规格必须命中池里同一个 client（连接池跨快照保留）")
		require.Equal(t, 1, pool.Size())
	})
}

// TestSelectSkipsUnroutableProxy 钉住「决策一位」（spec §4.7/§9.2「决策一位」行）：
// 代理禁用或绑定缺失 ⇒ Routable=false ⇒ 不入选。两条不可用路径都不在热路径
// 分原因——原因只用于日志/回显。
func TestSelectSkipsUnroutableProxy(t *testing.T) {
	tplA := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})

	t.Run("绑定但代理禁用", func(t *testing.T) {
		pool := egress.NewPool(nil)
		px := &domain.Proxy{ID: 7, Scheme: domain.ProxySchemeSOCKS5, Host: "10.1.2.3", Port: 1080, Enabled: false}
		s := proxySched(t, pool, proxyAcc(t, 1, tplA, px))
		_, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
		require.Error(t, err, "禁用的代理 ⇒ 账号不可派发")
		// 决策位定型正确（快照层面直读，避免只凭 Select 的报错类型推断）。
		av := s.view.Load().static.byID[1].static.Load()
		require.False(t, av.routable)
		require.Equal(t, int64(7), *av.acc.ProxyID, "绑定关系仍在（只是不可用）")
	})

	t.Run("绑定但代理缺失", func(t *testing.T) {
		pool := egress.NewPool(nil)
		a := acc(1, tplA, 4)
		proxyID := int64(9)
		a.ProxyID = &proxyID // 绑定了，但 Proxy 未展开（已软删/不存在）
		s := proxySched(t, pool, a)
		_, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
		require.Error(t, err, "绑定缺失 ⇒ fail-closed（spec L8）")
		av := s.view.Load().static.byID[1].static.Load()
		require.False(t, av.routable)
		require.Nil(t, av.acc.Proxy)
		require.Same(t, pool.Direct(), av.egress, "不可用也要有出口指针（恒非 nil，指向直连）")
	})

	t.Run("禁用账号 + 直连同样不入选", func(t *testing.T) {
		pool := egress.NewPool(nil)
		a := acc(1, tplA, 4)
		a.Enabled = false
		s := proxySched(t, pool, a)
		_, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
		require.Error(t, err)
	})
}

// TestProxyRoutesToUsableProxy 正向对照：绑定可用的代理时账号**必须**入选，
// 否则上面那些"不入选"的断言可能只是因为整个路由没编译出来。
func TestProxyRoutesToUsableProxy(t *testing.T) {
	pool := egress.NewPool(nil)
	tplA := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})
	px := &domain.Proxy{ID: 7, Scheme: domain.ProxySchemeHTTP, Host: "10.1.2.3", Port: 3128, Enabled: true}
	s := proxySched(t, pool, proxyAcc(t, 1, tplA, px))
	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	defer sel.Release()
	require.Equal(t, int64(1), sel.AccountID)
	require.True(t, s.view.Load().static.byID[1].static.Load().routable)
}

// TestProxyPlanKeySensitivity 钉住键的两个出口相关性：
//
//	① 改绑定（换线/解绑）⇒ planKey 必变（proxyDigest，在途计划作废重编译）；
//	② 代理 enabled 翻转（**不碰任何账号字段**）⇒ planKey 必变（routable 位）
//	   —— 这是 K 与 proxyDigest 之外的**第三个**变化源：规格摘要一模一样，
//	   只有"能不能用"变了。
func TestProxyPlanKeySensitivity(t *testing.T) {
	tplA := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})
	pxA := &domain.Proxy{ID: 1, Scheme: domain.ProxySchemeSOCKS5, Host: "10.0.0.1", Port: 1080, Enabled: true}
	pxB := &domain.Proxy{ID: 2, Scheme: domain.ProxySchemeSOCKS5, Host: "10.0.0.2", Port: 1080, Enabled: true}

	keyOf := func(a *domain.Account) planKey {
		return planKeyOf(newSnapshotStatic(*a, tplA, nil, directEgress()))
	}

	boundA := keyOf(proxyAcc(t, 1, tplA, pxA))
	require.NotEqual(t, keyOf(acc(1, tplA, 4)), boundA, "① 绑定 vs 直连：摘要不同 ⇒ 键必变")
	require.NotEqual(t, boundA, keyOf(proxyAcc(t, 1, tplA, pxB)), "① 换代理：摘要不同 ⇒ 键必变")

	// ② 同一份规格、只翻 enabled：摘要不变，routable 位必须让键变。
	pxADisabled := *pxA
	pxADisabled.Enabled = false
	disabledKey := keyOf(proxyAcc(t, 1, tplA, &pxADisabled))
	require.NotEqual(t, boundA, disabledKey, "② 代理 enabled 翻转必须换计划（routable 位）")
	require.Equal(t, boundA.proxyDigest, disabledKey.proxyDigest, "规格摘要本身没变——变的只有可用性")
	require.True(t, boundA.routable)
	require.False(t, disabledKey.routable)

	// ③ 反向：不碰代理、不碰账号的两次派生必须键相等（否则上面的敏感性是噪声）。
	require.Equal(t, boundA, keyOf(proxyAcc(t, 1, tplA, &domain.Proxy{
		ID: 1, Scheme: domain.ProxySchemeSOCKS5, Host: "10.0.0.1", Port: 1080, Enabled: true})))

	// ④ 改绑定后**在途计划失效**（spec §4.7）：同一账号从 A 换到 B，键不等 ⇒
	// 预留谓词 fact.planKey != c.PlanKey ⇒ 计划被重编译。
	s := proxySched(t, egress.NewPool(nil), proxyAcc(t, 1, tplA, pxA))
	require.NoError(t, s.InvalidateAllSync()) // 同内容重载 ⇒ 键判等
	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	sel.Release()
}

// TestReloadSkipsSnapshotWhenEgressUnwired 钉住装配违例（spec §4.7「装配缺失」）：
// 有账号绑定代理而池未注入 ⇒ 装载期 error，且**保留旧快照**（不发布半成品、
// 更不静默按直连出栈）。
func TestReloadSkipsSnapshotWhenEgressUnwired(t *testing.T) {
	tplA := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})
	plain := acc(1, tplA, 4)
	m := newMemLoader(map[int64][]*domain.Account{10: {plain}})
	// testCfg() 不注入 Egress ⇒ egressWired=false。
	s := newSchedWithCfg(t, testCfg(), m)
	require.False(t, s.egressWired, "fixture 前提：池未注入")
	before := s.view.Load()
	require.NotNil(t, before)

	// 把数据源换成"绑定代理"的账号：装配没跟上来 ⇒ reload 必须报装配违例。
	px := &domain.Proxy{ID: 3, Scheme: domain.ProxySchemeSOCKS5, Host: "10.0.0.3", Port: 1080, Enabled: true}
	m.set(10, []*domain.Account{proxyAcc(t, 1, tplA, px)})
	err := s.reload(context.Background())
	require.ErrorIs(t, err, ErrEgressNotWired)
	require.Same(t, before, s.view.Load(), "装配违例必须保留旧快照（不发布半成品）")
	require.False(t, s.reloadRequired, "装配违例不是暂时故障，不置粘性重试")

	// 注入池后同一份数据源即可装载（违例消失）。测试内直接补装配：等价于
	// main 在 New 时就把池传进 Config。
	s.cfg.Egress = egress.NewPool(nil)
	s.egressWired = true
	require.NoError(t, s.reload(context.Background()))
	require.Equal(t, 1, s.cfg.Egress.Size(), "绑定可用的账号要让池里出现该规格")
}

// TestReloadPrunesUnreferencedEgress 钉住 spec §8 失效矩阵的回收动作：改绑/解绑
// 后的全量重载必须把不再被引用的规格 client 关闭并移除（池不随历史增长）。
func TestReloadPrunesUnreferencedEgress(t *testing.T) {
	tplA := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})
	pxA := &domain.Proxy{ID: 1, Scheme: domain.ProxySchemeSOCKS5, Host: "10.0.0.1", Port: 1080, Enabled: true}
	pxB := &domain.Proxy{ID: 2, Scheme: domain.ProxySchemeHTTP, Host: "10.0.0.2", Port: 3128, Enabled: true}
	pool := egress.NewPool(nil)
	m := newMemLoader(map[int64][]*domain.Account{10: {proxyAcc(t, 1, tplA, pxA)}})
	cfg := testCfg()
	cfg.Egress = pool
	s := newSchedWithCfg(t, cfg, m)
	require.Equal(t, 1, pool.Size(), "绑定 A ⇒ 池里一条规格")

	// 换绑 A → B：旧规格（A）不再被引用 ⇒ 关闭并移除。
	// reload 只 stage；发布要等编译道（compileOnce）——断言读的是发布后的视图。
	m.set(10, []*domain.Account{proxyAcc(t, 1, tplA, pxB)})
	require.NoError(t, s.reload(context.Background()))
	s.compileOnce()
	require.Equal(t, 1, pool.Size())
	require.Same(t, pool.For(specOfDomain(pxB)), s.view.Load().static.byID[1].static.Load().egress,
		"换绑后叶子必须指向 B 的 client")
	require.NotSame(t, pool.For(specOfDomain(pxA)), s.view.Load().static.byID[1].static.Load().egress)

	// 解绑回直连：B 也回收，注册表清空（直连不占条目）。
	m.set(10, []*domain.Account{acc(1, tplA, 4)})
	require.NoError(t, s.reload(context.Background()))
	s.compileOnce()
	require.Equal(t, 0, pool.Size(), "解绑后不再有活跃规格 ⇒ 池清空")
	require.Same(t, pool.Direct(), s.view.Load().static.byID[1].static.Load().egress)
}

// TestEgressHotPathIsFieldRead 把 G5「零开销」的可读部分钉在测试里：Selection 的
// 出口只来自快照字段的一次读——同一份池、两次选号拿到**同一个指针**（没有 per-call
// For/建 client），且 `Selection.Egress.HTTP` 是池里那个 client 的 HTTP。
func TestEgressHotPathIsFieldRead(t *testing.T) {
	tplA := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})
	px := &domain.Proxy{ID: 1, Scheme: domain.ProxySchemeHTTPS, Host: "10.0.0.1", Port: 8443, Enabled: true}
	pool := egress.NewPool(nil)
	s := proxySched(t, pool, proxyAcc(t, 1, tplA, px))
	sizeBefore := pool.Size()

	for range 5 {
		sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
		require.NoError(t, err)
		require.Same(t, pool.For(specOfDomain(px)), sel.Egress)
		require.NotNil(t, sel.Egress.HTTP)
		sel.Release()
	}
	require.Equal(t, sizeBefore, pool.Size(), "选号不得建新 client（热路径零池操作）")
}

// memLoaderSet 是 memLoader 的测试辅助：替换某组账号（不新增导出面）。
func (m *memLoader) set(gid int64, accs []*domain.Account) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byGroup[gid] = accs
}
