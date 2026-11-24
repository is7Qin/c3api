// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

// P5-3-B 的**语义**用例（与 egresspool_fake_test.go 的夹具替身分开）：删掉
// `egress.NewPool(nil)` 之后，"未接出口"必须是两条明确的路，而不是一个恒非 nil
// 的假池：
//
//   - 未绑定账号 ⇒ 直连是确定状态，默认传输照常工作（今天的行为，不变）；
//   - 绑定账号   ⇒ 装配违例 ErrEgressNotWired，装载期失败并保留旧快照，
//     **不 panic、不静默退化成直连**。
//
// 三条取客户端的路径都要钉住（全量 reload / 组级批 / 账号全集），因为漏掉任何一条
// 都会让"配了代理"的账号从本机 IP 出栈。

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/identitypool"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/pkg/egressapi"
)

// nilDirectPool 是"池在、但给不出任何 client"的坏装配（接口契约违约）。
// 它存在只为证明：nil 指针不会被发给数据面（`sel.Egress.HTTP()` 是裸调用）。
type nilDirectPool struct{}

func (nilDirectPool) Direct() *egressapi.Client                        { return nil }
func (nilDirectPool) For(egressapi.Spec) *egressapi.Client             { return nil }
func (nilDirectPool) Detach(map[[16]byte]struct{}) []*egressapi.Client { return nil }
func (nilDirectPool) Size() int                                        { return 0 }

// egressTestTpl 是本文件所有用例共用的模板（一个格式、一个模型）。
func egressTestTpl() *domain.Template {
	return tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})
}

// egressTestProxy 是一个已启用、规格可解析的代理条目。
func egressTestProxy() *domain.Proxy {
	return &domain.Proxy{ID: 7, Scheme: domain.ProxySchemeSOCKS5, Host: "10.1.2.3", Port: 1080,
		Username: "u", Password: "p", Enabled: true}
}

// TestEgressUnwiredDirectForUnboundAccount 钉住**今天的行为不许改**的那一半：
// 没有绑定出口的账号在 Config.Egress == nil 时照常工作——直连、默认传输。
// 形状与被删掉的 egress.NewPool(nil).Direct() 逐项相同（nil 底座下 codex 派生
// 原样返回底座，两个口味本来就是同一个 http.DefaultClient）。
func TestEgressUnwiredDirectForUnboundAccount(t *testing.T) {
	tp := egressTestTpl()
	s := newSchedWithCfg(t, testCfg(), newMemLoader(map[int64][]*domain.Account{10: {acc(1, tp, 4)}}))
	require.Nil(t, s.cfg.Egress, "fixture 前提：端口未注入")

	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err, "未绑定出口 + 未接池：未绑定账号必须照常被选中")
	defer sel.Handle.Release()

	require.NotNil(t, sel.Egress, "快照的 egress 字段恒非 nil（热路径不写 nil 判断）")
	require.Equal(t, egressapi.Spec{}, sel.Egress.Spec, "直连 = 零值 Spec")
	require.Same(t, http.DefaultClient, sel.Egress.HTTP(), "默认传输：直连出口就是底座")
	require.Same(t, http.DefaultClient, sel.Egress.NoCompression(),
		"nil 底座下 codex 口味不派生（与 egress.NewPool(nil) 同形）")
}

// TestEgressUnwiredBoundAccountFailsClosed 钉住本单最重要的一条：账号绑定了出口
// 而池未注入时，**任何**取客户端的路径都返回 ErrEgressNotWired。
func TestEgressUnwiredBoundAccountFailsClosed(t *testing.T) {
	tp := egressTestTpl()
	px := egressTestProxy()
	bound := func() *domain.Account { return proxyAcc(t, 1, tp, px) }

	t.Run("全量 reload", func(t *testing.T) {
		m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tp, 4)}})
		s := newSchedWithCfg(t, testCfg(), m) // 首载：未绑定 ⇒ 直连，成功
		before := s.view.Load()
		require.NotNil(t, before)

		m.set(10, []*domain.Account{bound()})
		err := s.reload(context.Background())
		require.ErrorIs(t, err, ErrEgressNotWired, "绑定账号 + 未接池 = 装配违例")
		require.Same(t, before, s.view.Load(), "装配违例必须保留旧快照（不发布半成品）")
		require.False(t, s.reloadRequired, "装配违例不是暂时故障，不置粘性重试")
	})

	t.Run("组级批次", func(t *testing.T) {
		m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tp, 4)}})
		s := newSchedWithCfg(t, testCfg(), m)
		before := s.view.Load()
		require.NotNil(t, before)

		m.set(10, []*domain.Account{bound()})
		s.InvalidateGroups([]int64{10})
		require.Nil(t, s.publisher.pending,
			"组级路径同样 fail closed：装配违例必须在 stage **之前**中止（不留下待发布根）")
		require.Same(t, before, s.view.Load(), "装配违例不换根（也不 panic）")
		require.False(t, s.reloadRequired, "装配违例不置粘性重试")
	})

	t.Run("解析器本身", func(t *testing.T) {
		eg, err := egressClientFor(nil, bound())
		require.ErrorIs(t, err, ErrEgressNotWired)
		require.Nil(t, eg, "fail closed 不返回 client——nil 会让数据面裸调 panic")
	})

	t.Run("坏装配：池在但给不出 client", func(t *testing.T) {
		re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil, nil, nil)
		require.NoError(t, re.Reload(context.Background()))
		cfg := testCfg()
		cfg.Egress = nilDirectPool{}
		s := New(cfg, newMemLoader(map[int64][]*domain.Account{10: {acc(1, tp, 4)}}), re, nil, nil, nil, nil)
		err := s.reload(context.Background())
		require.ErrorIs(t, err, ErrEgressNotWired,
			"接口契约违约（Direct 返回 nil）同样按装配违例处理，绝不把 nil 发给快照")
		require.Nil(t, s.view.Load(), "没有任何快照被发布")
	})
}

// TestEgressUnwiredBoundUngroupedAccountFailsClosed 单独覆盖**账号全集**那条路径：
// 未编组账号不进路由根，buildSnapshots 看不见它，只有 fullLocked 能发现装配违例。
func TestEgressUnwiredBoundUngroupedAccountFailsClosed(t *testing.T) {
	tp := egressTestTpl()
	loader := &universeLoader{
		memLoader: newMemLoader(map[int64][]*domain.Account{}),
		ungrouped: []*domain.Account{proxyAcc(t, 3, tp, egressTestProxy())},
	}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil, nil, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := New(testCfg(), loader, re, nil, nil, nil, nil)

	err := s.reload(context.Background())
	require.ErrorIs(t, err, ErrEgressNotWired,
		"未编组账号独占的出口同样是装配违例：它不在路由根里，只在账号全集里")
	require.Nil(t, s.view.Load())
}

// TestInvalidateGroupsFailClosedDropsCollectedRetirements 钉住批路径的**副作用**
// 纪律：组 10 的池收缩（K 4→2）在**折叠期**就把 2 条线程退役收集进本批 reclaim，
// 组 20 随后撞上装配违例——整批放弃时那 2 条必须一起丢掉。withPublisherLock 无条件
// 执行 reclaim，丢了这一步就会把**旧快照仍在引用**的线程退役掉（比不发布更糟）。
func TestInvalidateGroupsFailClosedDropsCollectedRetirements(t *testing.T) {
	codexTpl := tpl(1, domain.FormatOpenAIResponses, []string{"gpt-5"})
	codexTpl.CredentialType = credential.TypeCodexOAuth
	codexAcc := func(k int) *domain.Account {
		a := acc(1, codexTpl, k)
		a.Ext = &domain.AccountExt{CodexIdentity: &domain.CodexIdentity{InstallationID: "inst-1"}}
		return a
	}
	chatTpl := egressTestTpl()
	m := newMemLoader(map[int64][]*domain.Account{
		10: {codexAcc(4)},
		20: {acc(2, chatTpl, 4)},
	})
	retired := 0
	cfg := testCfg()
	cfg.RetireThread = func(int64, string) { retired++ }
	s := newSchedWithCfg(t, cfg, m)
	require.NotNil(t, s.view.Load())

	m.set(10, []*domain.Account{codexAcc(2)})                                // Resize ⇒ 收集 2 条退役
	m.set(20, []*domain.Account{proxyAcc(t, 2, chatTpl, egressTestProxy())}) // 装配违例
	s.InvalidateGroups([]int64{10, 20})

	require.Zero(t, retired, "整批放弃必须丢弃已收集的退役动作（旧快照仍在引用这些线程）")
	require.Nil(t, s.publisher.pending, "不得留下待发布根")
}

// TestFullLockedBoundButUnexpandedAccount 钉住 L8（绑定但展开缺失，spec §4.7）在
// **配置面**的处置：池在时跳过（不登记出口），池不在时仍报装配违例——跳过逻辑不得
// 把既有判据吃掉。
//
// 形状的真实来源：repository 的 expandAccountProxy 在代理被软删/缺失时保留 ProxyID、
// 把 Proxy 留成 nil。它不可路由（routableFor 恒 false），永远不会成为请求出口。
func TestFullLockedBoundButUnexpandedAccount(t *testing.T) {
	tp := egressTestTpl()
	stale := func() *domain.Account {
		a := acc(1, tp, 4)
		a.ProxyID = new(int64(9)) // 绑定在；Proxy 未展开（已软删/缺失）
		return a
	}

	t.Run("池在：routable=false，且账号不进 universe", func(t *testing.T) {
		pool := newFakeEgressPool()
		// routable 已有独立覆盖（TestSelectSkipsUnroutableProxy/绑定但代理缺失），
		// 这里就地再钉一次，让"跳过"的**前提**与结论在同一个用例里可读。
		leaf := newSnapshotStatic(*stale(), tp, nil, pool.Direct(), nil)
		require.False(t, leaf.routable, "L8 ⇒ 不可路由")

		var r accountRuntime
		cfg, err := r.fullLocked(nil, []*domain.Account{stale()}, pool, nil)
		require.NoError(t, err, "陈旧绑定不是装配错误：不许把整批配置打掉")
		_, ok := cfg.egress[1]
		require.False(t, ok, "不可路由账号不得以'直连出口'的形式进 universe（那是假事实）")
	})

	t.Run("池不在：仍是 ErrEgressNotWired", func(t *testing.T) {
		var r accountRuntime
		_, err := r.fullLocked(nil, []*domain.Account{stale()}, nil, nil)
		require.ErrorIs(t, err, ErrEgressNotWired, "跳过逻辑只对'池已注入'生效，既有判据不变")
	})

	t.Run("绑定且已展开：照常走 pool.For 并进 universe", func(t *testing.T) {
		pool := newFakeEgressPool()
		px := egressTestProxy()
		var r accountRuntime
		cfg, err := r.fullLocked(nil, []*domain.Account{proxyAcc(t, 1, tp, px)}, pool, nil)
		require.NoError(t, err)
		require.Same(t, pool.For(specOfDomain(px)), cfg.egress[1],
			"正常绑定必须命中池里同一 client（跳过逻辑不得误伤它）")
	})

	t.Run("无全集能力时（all==nil 回退分支）同一条规则", func(t *testing.T) {
		pool := newFakeEgressPool()
		leaf := newSnapshotStatic(*stale(), tp, nil, pool.Direct(), nil)
		byID := map[int64]*accountSnapshot{1: newAccountSnapshot(leaf, &accState{status: domain.StatusActive})}

		var r accountRuntime
		cfg, err := r.fullLocked(byID, nil, pool, nil)
		require.NoError(t, err)
		_, ok := cfg.egress[1]
		require.False(t, ok, "回退分支（byID 起 universe）不得在装载源能力不同时给出第二种口径")
	})
}

// TestEgressClientForAlwaysResolves 是解析器的**唯一**不变量：成功 ⇔ 非 nil。
// 任何"顺手返回 nil 当直连"的改动都会让这条红——这是快照字段恒非 nil 的来源。
func TestEgressClientForAlwaysResolves(t *testing.T) {
	tp := egressTestTpl()
	pool := newFakeEgressPool()
	for _, tc := range []struct {
		name string
		pool EgressPool
		acc  *domain.Account
	}{
		{"未接池 + 未绑定", nil, acc(1, tp, 4)},
		{"未接池 + 未绑定（nil 账号）", nil, nil},
		{"已接池 + 未绑定", pool, acc(1, tp, 4)},
		{"已接池 + 绑定", pool, proxyAcc(t, 1, tp, egressTestProxy())},
		{"已接池 + 绑定但规格未展开", pool, func() *domain.Account {
			a := acc(1, tp, 4)
			a.ProxyID = new(int64(9))
			return a
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eg, err := egressClientFor(tc.pool, tc.acc)
			require.NoError(t, err)
			require.NotNil(t, eg, "成功路径必须给出 client（热路径不做 nil 判断）")
		})
	}
}

// TestPoolReclaimDiscardDropsCollectedEffects 钉住 fail-closed 中止时的副作用纪律：
// withPublisherLock 无条件执行收集到的 reclaim，所以"整批放弃"必须先把已收集的动作
// 丢掉——否则会把**旧快照仍在引用**的线程退役掉。
func TestPoolReclaimDiscardDropsCollectedEffects(t *testing.T) {
	b := &poolReclaim{}
	b.Threads = []identitypool.ThreadRetire{{AccountID: 1, ThreadKey: "thread-1"}}
	b.egress = []*egressapi.Client{nil}
	b.config = &accountConfig{}
	b.discard()

	require.Empty(t, b.Threads)
	require.Nil(t, b.egress)
	require.Nil(t, b.config)

	retired := 0
	b.retire = func(int64, string) { retired++ }
	b.run()
	require.Zero(t, retired, "丢弃后 run 必须是 no-op")
}
