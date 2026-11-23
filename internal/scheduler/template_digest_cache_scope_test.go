// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// 本文件钉住 templateDigestCache 的**所有权**——static_key.go 类型注释的第一句
// 「一次 reload / 一个失效批次作用域内的模板摘要表」，也就是"表由作用域拥有"。
//
// 为什么需要它：template_digest_cache_test.go 的四层判据证明的是"表里没有别的
// 作用域的东西"（跨作用域必须重算）。它们证明不了"表就是这一枚"：把所有权退化成
//
//	var digestEntriesPool = map[*domain.Template]tplDigests{}
//	func newTemplateDigestCache() *templateDigestCache {
//	    clear(digestEntriesPool)                   // 每次"新表"清空共享 map
//	    return &templateDigestCache{entries: digestEntriesPool}
//	}
//
// 之后，逐字节相等 / 键是对象 / 跨作用域不泄漏 / misses=T 与 hits=A−T / 批次
// 用例 / 真库用例**全绿**——单线程可观测面上，它与真实现不可区分（clear 起到了
// "新表"的作用）。能区分的只有两件事，就是下面两条用例：
//
//  1. **身份**：两个作用域的 entries 不是同一枚 map（写一张，另一张看不见）；
//  2. **并发**：两台 Scheduler 并发 reload 时不得有共享可写状态（-race）。
//
// 第 2 条是这条契约真正的牙齿：reloadLocked 与 invalidateGroupsLocked 各持自己的
// publisher.mu，「表由作用域拥有」正是让两台调度器互不相干的**唯一**理由；进程级
// 唯一表把它换成"单调度器看不出、多调度器静默串味 + DATA RACE"。

// TestDigestCacheScopesOwnDistinctEntries 是所有权判据 1：两个作用域拿到的
// entries **不是同一枚 map**。
//
// 断言用"写一张、查另一张"，而不是比较 map 的内部指针：前者直接就是契约的含义
// （"这张表只有这个作用域看得见"），且不依赖 reflect 对 map 的支持细节
// （reflect.Value.Pointer 对 map 的可用性在 Go 版本间变过）。
//
// 两种作用域各测一遍：构造函数的两枚返回值，以及两次真实 reload 各建的那张
// （生产作用域的形状——同时也把"一次 reload 一张"的建表计数一并钉住）。
func TestDigestCacheScopesOwnDistinctEntries(t *testing.T) {
	tp := &domain.Template{ID: 61, CredentialType: credential.TypeAPIKey, Models: []string{"m-1"}}
	// 探针表项：内容取自真实摘要函数，但键是一枚伪造模板——只用来观测
	// "写进这张表的东西是否出现在另一张表里"。
	probeKey := &domain.Template{ID: 62, CredentialType: credential.TypeAPIKey}
	probe := tplDigests{modelsDigest: digestStrings([]string{"scope-probe"})}

	// ① 构造函数：两次调用必须是两张独立的表。
	first, second := newTemplateDigestCache(), newTemplateDigestCache()
	require.NotSame(t, first, second, "每次建表必须是新壳体")
	require.NotNil(t, first.entries, "表壳必须带一张非 nil 的 entries")
	first.entries[probeKey] = probe
	_, leaked := second.entries[probeKey]
	require.False(t, leaked,
		"写第一张表不得让第二张看见 ⇒ 两枚 entries 必须是不同的 map（包级共享 + clear 会在这里红）")
	require.Zero(t, second.misses, "探测写只碰 entries，不得改变另一张表的计数")
	require.Zero(t, second.hits)

	// ② 生产作用域：两次 reload 各一张表，同样不得共享 entries。
	a1 := acc(1, tp, 4)
	s := New(testCfg(), newMemLoader(map[int64][]*domain.Account{10: {a1}}),
		newTestRuleEngine(t), nil, nil, nil, nil)
	rec := installDigestCacheRecorder(t)

	require.NoError(t, s.reload(context.Background()))
	require.Len(t, rec.caches, 1, "一次 reload 恰好一张作用域表（不是每账号/每组一张）")
	previous := rec.caches[0]
	require.Len(t, previous.entries, 1, "本次 reload 只该有这一枚模板对象")
	require.Equal(t, digestStrings([]string{"m-1"}), previous.entries[tp].modelsDigest)

	rec.reset()
	require.NoError(t, s.reload(context.Background()))
	require.Len(t, rec.caches, 1, "下一次 reload 同样恰好一张表")
	current := rec.caches[0]
	require.NotSame(t, previous, current, "跨 reload 必须是新壳体")
	require.Len(t, current.entries, 1)

	// reload 已结束，事后写没有任何并发；若两枚 entries 是同一张 map，
	// 探针会出现在 current 里。
	previous.entries[probeKey] = probe
	_, leaked = current.entries[probeKey]
	require.False(t, leaked,
		"两次 reload 的 entries 不得是同一枚 map：否则下一次 reload 的命中会读到上一次的表项")
}

// newDigestConcurrentScheduler 造一台**自成一体**的调度器：3 组 × 3 账号 × 2 枚
// 模板。载入器每次返回同一批指针（内存行），因此本夹具考察的是**表的所有权**，
// 不是指针换代：两台调度器之间没有任何共享的可写对象（各自的 Config.Egress 池、
// 规则引擎、publisher 锁都是 New 里现造的）。
func newDigestConcurrentScheduler(t *testing.T) *Scheduler {
	t.Helper()
	templates := []*domain.Template{
		tpl(501, domain.FormatOpenAIChat, []string{"m-a"}),
		tpl(502, domain.FormatAnthropic, []string{"m-b"}),
	}
	byGroup := make(map[int64][]*domain.Account, 3)
	for gid := int64(1); gid <= 3; gid++ {
		accounts := make([]*domain.Account, 0, 3)
		for i := 0; i < 3; i++ {
			accounts = append(accounts, acc(gid*10+int64(i), templates[i%len(templates)], 4))
		}
		byGroup[gid] = accounts
	}
	return New(testCfg(), newMemLoader(byGroup), newTestRuleEngine(t), nil, nil, nil, nil)
}

// TestDigestCacheScopesStayIsolatedAcrossConcurrentReloads 是所有权判据 2：
// 两台 Scheduler 各自并发 reload（其中一台还并发跑批次失效）时，`-race` 必须干净。
//
// 进程级共享 entries + 每次建表 clear() 在这里立刻现形：三枚 goroutine 同时对
// **同一张 map** 做 clear/mapassign，race detector 报 DATA RACE（本机实测 8 条；
// 若某台机器上时序不重叠而没报，判据 1 仍然确定性变红——见上一条用例）。
//
// 不使用 time.Sleep：三枚 goroutine 用 close(start) 屏障同时放行；结束由
// sync.WaitGroup 收口（wg.Wait 也建立了读取发布根的 happens-before）。
func TestDigestCacheScopesStayIsolatedAcrossConcurrentReloads(t *testing.T) {
	first, second := newDigestConcurrentScheduler(t), newDigestConcurrentScheduler(t)
	const (
		rounds    = 200 // 每枚 goroutine 的作用域数（与 momus 的 M3 探针同量级）
		wantLeafs = 9   // 3 组 × 3 账号，账号 ID 唯一
	)

	start := make(chan struct{})
	var (
		wg        sync.WaitGroup
		firstErr  error
		secondErr error
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < rounds; i++ {
			if err := first.reload(context.Background()); err != nil {
				firstErr = err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < rounds; i++ {
			if err := second.reload(context.Background()); err != nil {
				secondErr = err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		// 批次作用域与全量 reload 抢同一台调度器的 publisher.mu（串行），
		// 但两张表分别属于各自的临界区——第二次 reload/第二台调度器才是
		// "跨作用域"的那一面。
		for i := 0; i < rounds; i++ {
			first.InvalidateGroups([]int64{3, 1})
		}
	}()
	close(start)
	wg.Wait()

	require.NoError(t, firstErr, "并发 reload 不得失败")
	require.NoError(t, secondErr, "并发 reload 不得失败")
	// 并发不得损坏各自的根（表串味会让命中读到别人的表项，最终在叶内容上显形）。
	require.NotNil(t, first.publisher.pending, "第一台必须留下一个 staged 根")
	require.Len(t, first.publisher.pending.byID, wantLeafs, "第一台不得丢账号")
	require.NotNil(t, second.publisher.pending, "第二台必须留下一个 staged 根")
	require.Len(t, second.publisher.pending.byID, wantLeafs, "第二台不得丢账号")
}
