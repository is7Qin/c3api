// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// 本文件是 U22 第二步（per-reload 模板摘要缓存）的硬不变量。判据分四层：
//
//  1. **逐字节相等**：缓存路径与直接计算路径的 5 个 [32]byte 全等，并用
//     frozenPlanKey（独立的旧算法 oracle）交叉验证——不是"计划相等"，是值相等；
//  2. **键是对象不是 ID**：同 ID 不同内容的两枚模板在**同一张**缓存里不得串味；
//  3. **不跨作用域**：模板内容改了，新作用域的摘要必须变（表提到包级会红）；
//  4. **必须命中**：misses == 不同模板数、hits == 构造次数 − 不同模板数。
//     没有第 4 层，"收下缓存从不读"（cheat_noop）能让前三层全绿。

// tplDigestsOf 从叶上取回 5 项摘要（[32]byte 值拷贝，供逐字节比较）。
func tplDigestsOf(av *snapshotStatic) tplDigests {
	return tplDigests{
		modelsDigest:     av.key.modelsDigest,
		formatModelsKey:  av.key.formatModelsKey,
		supportedFormats: av.key.supportedFormats,
		modelMappingKey:  av.key.modelMappingKey,
		upstreamPathsKey: av.key.upstreamPathsKey,
	}
}

// digestFixtureAccount 是摘要用例的账号（账号侧事实固定，唯一变量是模板内容）。
func digestFixtureAccount(tp *domain.Template) domain.Account {
	return domain.Account{
		ID: 1, TemplateID: tp.ID, Template: tp, UpstreamKey: "k",
		Enabled: true, MaxConcurrency: 1, IdentityRevision: 1,
	}
}

// randomDigestTemplate 生成一枚形状随机的模板：nil/空 表壳、空/重复/未排序的
// 模型与格式列表、多格式 FormatModels、两种 ModelMapping 模式、非空上游路径表。
// 随机源是固定种子的 PCG，用例可复现；map 的**插入顺序**同样被随机化。
func randomDigestTemplate(rng *rand.Rand, id int64) *domain.Template {
	tp := &domain.Template{
		ID:             id,
		BaseURL:        "https://u/v1",
		CredentialType: credential.TypeAPIKey,
	}
	formats := slices.Clone(domain.RoutableFormats())
	rng.Shuffle(len(formats), func(i, j int) { formats[i], formats[j] = formats[j], formats[i] })

	// Models：0..5 项，取值只有 4 个 → 天然出现重复值；nil 与空切片各占一半。
	if n := rng.IntN(6); n > 0 {
		tp.Models = make([]string, 0, n)
		for i := 0; i < n; i++ {
			tp.Models = append(tp.Models, fmt.Sprintf("m-%d", rng.IntN(4)))
		}
	} else if rng.IntN(2) == 0 {
		tp.Models = []string{}
	}

	// SupportedFormats：随机子集（可能为空），顺序随机。
	if n := rng.IntN(len(formats) + 1); n > 0 {
		tp.SupportedFormats = slices.Clone(formats[:n])
	} else if rng.IntN(2) == 0 {
		tp.SupportedFormats = []domain.RequestFormat{}
	}

	// FormatModels：随机格式 → 随机值列表（值列表同样未排序、可空）。
	if rng.IntN(2) == 0 {
		tp.FormatModels = make(map[domain.RequestFormat][]string, len(formats))
		for i := 0; i < rng.IntN(4); i++ {
			f := formats[rng.IntN(len(formats))]
			vals := make([]string, 0, 3)
			for j := 0; j < rng.IntN(4); j++ {
				vals = append(vals, fmt.Sprintf("fm-%d", rng.IntN(3)))
			}
			tp.FormatModels[f] = vals
		}
	}

	// ModelMapping：随机别名，两种模式都出现。
	if rng.IntN(2) == 0 {
		tp.ModelMapping = make(domain.ModelMapping, 3)
		for i := 0; i < rng.IntN(4); i++ {
			key := fmt.Sprintf("alias-%d", rng.IntN(3))
			mode := domain.ModelMappingModeExplicit
			if rng.IntN(2) == 0 {
				mode = domain.ModelMappingModeImplicit
			}
			tp.ModelMapping[key] = domain.ModelMappingEntry{
				MappedModel: fmt.Sprintf("up-%d", rng.IntN(3)),
				Mode:        mode,
			}
		}
	}

	// TemplateUpstream：随机路径键值（键只有 3 个 → 覆盖与覆盖写都可能发生）。
	if rng.IntN(2) == 0 {
		tp.Upstream = &domain.TemplateUpstream{Paths: make(map[string]string, 3)}
		for i := 0; i < rng.IntN(4); i++ {
			tp.Upstream.Paths[fmt.Sprintf("path-%d", rng.IntN(3))] = fmt.Sprintf("v%d/x", rng.IntN(3))
		}
	}
	return tp
}

// TestTemplateDigestCacheMatchesDirectDerivation 是不变量 1：随机模板（覆盖
// nil/空表壳/重复值/未排序列表/多格式映射/两种映射模式/非空上游路径）上，
// 缓存路径与直接计算路径的 5 项摘要**逐字节**相等，且整枚 planKey 与独立
// oracle（frozenPlanKey）相等；第二遍全命中时必须重放同一批字节。
func TestTemplateDigestCacheMatchesDirectDerivation(t *testing.T) {
	rng := rand.New(rand.NewPCG(0x22, 0xc0ffee))
	tpls := []*domain.Template{
		// 5 项全 nil。
		{ID: 1, CredentialType: credential.TypeAPIKey},
		// 空表壳（非 nil）：与 nil 必须同摘要（空集摘要的定义）。
		{
			ID: 1, CredentialType: credential.TypeAPIKey,
			Models: []string{}, SupportedFormats: []domain.RequestFormat{},
			FormatModels: map[domain.RequestFormat][]string{},
			ModelMapping: domain.ModelMapping{}, Upstream: &domain.TemplateUpstream{Paths: map[string]string{}},
		},
		// 同 ID、不同内容：以 templateID 为键的缓存会在这里串味（变异 1）。
		{
			ID: 7, CredentialType: credential.TypeAPIKey,
			Models:           []string{"m-2", "m-1"},
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
			Upstream:         &domain.TemplateUpstream{Paths: map[string]string{"openai-chat": "v1/a"}},
		},
		{
			ID: 7, CredentialType: credential.TypeAPIKey,
			Models:           []string{"m-9"},
			SupportedFormats: []domain.RequestFormat{domain.FormatAnthropic},
			Upstream:         &domain.TemplateUpstream{Paths: map[string]string{"openai-chat": "v1/a"}},
		},
	}
	for i := 0; i < 24; i++ {
		tpls = append(tpls, randomDigestTemplate(rng, int64(100+i)))
	}
	gids := []int64{3, 1, 1} // 含重复值：组摘要同样不许被缓存影响

	cache := newTemplateDigestCache()
	want := make([]tplDigests, len(tpls))
	for i, tp := range tpls {
		acc := digestFixtureAccount(tp)
		cached := newSnapshotStatic(acc, tp, gids, directEgress(), cache)
		direct := newSnapshotStatic(acc, tp, gids, directEgress(), nil)

		got, plain := tplDigestsOf(cached), tplDigestsOf(direct)
		require.Equal(t, plain.modelsDigest, got.modelsDigest, "case %d: modelsDigest 必须逐字节相等", i)
		require.Equal(t, plain.formatModelsKey, got.formatModelsKey, "case %d: formatModelsKey 必须逐字节相等", i)
		require.Equal(t, plain.supportedFormats, got.supportedFormats, "case %d: supportedFormats 必须逐字节相等", i)
		require.Equal(t, plain.modelMappingKey, got.modelMappingKey, "case %d: modelMappingKey 必须逐字节相等", i)
		require.Equal(t, plain.upstreamPathsKey, got.upstreamPathsKey, "case %d: upstreamPathsKey 必须逐字节相等（漏一项即红）", i)
		require.Equal(t, planKeyOf(direct), planKeyOf(cached), "case %d: 整枚 planKey 必须相等", i)
		// 独立的旧算法 oracle：缓存路径不得只在"自己和自己比"时成立（凭据摘要维度
		// 由 1125 组参照测试覆盖，不在这里逐值对照）。
		requirePlanKeyMatchesFrozen(t, planKeyOf(cached), acc, tp, gids, "case %d: 与独立 oracle 必须相等", i)
		want[i] = got
	}
	require.Equal(t, len(tpls), cache.misses, "misses = 不同模板对象数")
	require.Zero(t, cache.hits, "首遍不得有命中")

	// 第二遍：同一批对象 ⇒ 全命中，且命中值逐字节等于首遍算出的那一份。
	for i, tp := range tpls {
		cached := newSnapshotStatic(digestFixtureAccount(tp), tp, gids, directEgress(), cache)
		require.Equal(t, want[i], tplDigestsOf(cached), "case %d: 命中必须重放同一份字节", i)
	}
	require.Equal(t, len(tpls), cache.misses, "命中不得新增表项")
	require.Equal(t, len(tpls), cache.hits, "第二遍：每个模板对象恰好命中一次")
}

// TestTemplateDigestCacheKeysByObjectNotByID 是变异 1 的正面证据：两枚 **ID 相同、
// 内容不同** 的模板共享一张缓存时，第二枚必须重算（miss）并与直接计算相等；
// 若缓存键写成 templateID，第二枚会命中第一枚的条目 ⇒ 摘要串味。
func TestTemplateDigestCacheKeysByObjectNotByID(t *testing.T) {
	a := &domain.Template{
		ID: 7, CredentialType: credential.TypeAPIKey,
		Models:   []string{"m-1"},
		Upstream: &domain.TemplateUpstream{Paths: map[string]string{"openai-chat": "v1/a"}},
	}
	b := &domain.Template{
		ID: 7, CredentialType: credential.TypeAPIKey,
		Models:   []string{"m-2", "m-3"},
		Upstream: &domain.TemplateUpstream{Paths: map[string]string{"openai-chat": "v2/b"}},
	}

	cache := newTemplateDigestCache()
	avA := newSnapshotStatic(digestFixtureAccount(a), a, nil, directEgress(), cache)
	avB := newSnapshotStatic(digestFixtureAccount(b), b, nil, directEgress(), cache)

	require.Equal(t, 2, cache.misses, "同 ID 不同内容 = 两个模板对象 = 两次未命中")
	require.Zero(t, cache.hits)
	require.Equal(t, tplDigestsOf(newSnapshotStatic(digestFixtureAccount(a), a, nil, directEgress(), nil)),
		tplDigestsOf(avA))
	require.Equal(t, tplDigestsOf(newSnapshotStatic(digestFixtureAccount(b), b, nil, directEgress(), nil)),
		tplDigestsOf(avB), "第二枚必须按自己的内容派生（ID 键会把它换成第一枚的字节）")
	require.NotEqual(t, avA.key.modelsDigest, avB.key.modelsDigest)
	require.NotEqual(t, avA.key.upstreamPathsKey, avB.key.upstreamPathsKey)

	// 再构造第一枚 ⇒ 命中，且拿回的仍是它自己的那份。
	again := newSnapshotStatic(digestFixtureAccount(a), a, nil, directEgress(), cache)
	require.Equal(t, 1, cache.hits)
	require.Equal(t, tplDigestsOf(avA), tplDigestsOf(again))
}

// TestTemplateDigestIgnoresMapInsertionOrder 钉住"摘要与 map 迭代序无关"：同内容、
// 不同插入顺序、不同指针的两枚模板必须得到同一组字节；缓存不得引入顺序依赖。
func TestTemplateDigestIgnoresMapInsertionOrder(t *testing.T) {
	build := func(reverse bool) *domain.Template {
		tp := &domain.Template{
			ID: 3, CredentialType: credential.TypeAPIKey,
			Models:           []string{"m-b", "m-a", "m-a"},
			SupportedFormats: []domain.RequestFormat{domain.FormatAnthropic, domain.FormatOpenAIChat},
			FormatModels:     map[domain.RequestFormat][]string{},
			ModelMapping:     domain.ModelMapping{},
			Upstream:         &domain.TemplateUpstream{Paths: map[string]string{}},
		}
		formats := []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatAnthropic}
		aliases := []string{"alias-a", "alias-b"}
		paths := []string{"path-a", "path-b"}
		if reverse {
			slices.Reverse(formats)
			slices.Reverse(aliases)
			slices.Reverse(paths)
		}
		for _, f := range formats {
			tp.FormatModels[f] = []string{"m-b", "m-a"}
		}
		for _, alias := range aliases {
			tp.ModelMapping[alias] = domain.ModelMappingEntry{MappedModel: alias + "-up", Mode: domain.ModelMappingModeExplicit}
		}
		for _, p := range paths {
			tp.Upstream.Paths[p] = p + "/v1"
		}
		return tp
	}
	a, b := build(false), build(true)
	require.NotSame(t, a, b)
	ca := newSnapshotStatic(digestFixtureAccount(a), a, nil, directEgress(), newTemplateDigestCache())
	cb := newSnapshotStatic(digestFixtureAccount(b), b, nil, directEgress(), newTemplateDigestCache())
	require.Equal(t, tplDigestsOf(ca), tplDigestsOf(cb), "同内容不同插入顺序必须同摘要")
	require.Equal(t, planKeyOf(ca), planKeyOf(cb))
}

// TestTemplateDigestCacheDoesNotLeakAcrossScopes 是不变量 2：模板内容就地改动后，
// **新作用域**必须重新派生（摘要是模板内容的函数）。把表提到包级（或跨 reload
// 复用同一张）会命中旧条目 ⇒ 新叶仍带旧摘要 ⇒ 本用例红。
func TestTemplateDigestCacheDoesNotLeakAcrossScopes(t *testing.T) {
	tp := &domain.Template{
		ID: 11, CredentialType: credential.TypeAPIKey,
		Models:           []string{"m-1"},
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		Upstream:         &domain.TemplateUpstream{Paths: map[string]string{"openai-chat": "v1/a"}},
	}
	first := newTemplateDigestCache()
	before := newSnapshotStatic(digestFixtureAccount(tp), tp, nil, directEgress(), first)

	// 模板内容改动（就地：同一枚指针），下一次作用域必须重算。
	tp.Models = []string{"m-1", "m-2"}
	tp.Upstream.Paths["openai-chat"] = "v2/b"
	second := newTemplateDigestCache()
	after := newSnapshotStatic(digestFixtureAccount(tp), tp, nil, directEgress(), second)

	require.Equal(t, digestStrings([]string{"m-1", "m-2"}), after.key.modelsDigest,
		"模板改了摘要必须变（跨作用域复用会返回旧字节）")
	require.Equal(t, digestTemplateUpstream(&domain.TemplateUpstream{Paths: map[string]string{"openai-chat": "v2/b"}}), after.key.upstreamPathsKey)
	require.NotEqual(t, before.key.modelsDigest, after.key.modelsDigest,
		"模板改了摘要必须变（表提到包级会返回旧字节）")
	require.NotEqual(t, before.key.upstreamPathsKey, after.key.upstreamPathsKey)
	require.NotEqual(t, planKeyOf(before), planKeyOf(after))
	require.Equal(t, 1, second.misses, "新作用域必须未命中（命中 = 跨作用域复用）")
	require.Zero(t, second.hits)
}

// TestBuildSnapshotsDigestCacheCountersFollowProductionShape 是 spec §4.3 的命中
// 断言：生产形状夹具（每模板 M 个账号、指针自然共享）下，misses 恰好等于模板数、
// hits 恰好等于"构造次数 − 模板数"，且缓存产出的整枚 planKey 与逐次计算逐字段相等。
func TestBuildSnapshotsDigestCacheCountersFollowProductionShape(t *testing.T) {
	const accounts, templates = 23, 5
	accs := routeBenchProductionAccounts(accounts, templates)
	require.Len(t, accs, accounts)

	cache := newTemplateDigestCache()
	byGroup := map[int64][]*domain.Account{10: accs}
	groups, byID, err := buildSnapshots(byGroup, nil, testEgressPool, cache)
	require.NoError(t, err)

	require.Len(t, byID, accounts)
	require.Len(t, groups[10].accounts, accounts)
	require.Equal(t, templates, cache.misses, "misses = 不同模板对象数（T）")
	require.Equal(t, accounts-templates, cache.hits, "hits = 账号数 − 模板数（A − T）")
	require.Equal(t, templates, len(cache.entries))

	// 缓存路径与逐次计算路径的叶子必须同键（缓存不得改变任何事实）。
	_, plainByID, err := buildSnapshots(byGroup, nil, testEgressPool, nil)
	require.NoError(t, err)
	for id, cached := range byID {
		require.Equal(t, plainByID[id].static.Load().key, cached.static.Load().key,
			"account %d: 缓存路径与逐次计算的 planKey 必须相等", id)
	}
}

// digestCacheRecorder 抓住作用域内部建的那张摘要表（唯一的批次作用域观测面）。
type digestCacheRecorder struct {
	caches []*templateDigestCache
}

// installDigestCacheRecorder 替换构造函数并把每张新表记下来；清理时还原。
func installDigestCacheRecorder(t *testing.T) *digestCacheRecorder {
	t.Helper()
	orig := newTemplateDigestCache
	rec := &digestCacheRecorder{}
	newTemplateDigestCache = func() *templateDigestCache {
		c := orig()
		rec.caches = append(rec.caches, c)
		return c
	}
	t.Cleanup(func() { newTemplateDigestCache = orig })
	return rec
}

func (r *digestCacheRecorder) reset() { r.caches = nil }

// TestReloadBuildsOneDigestCachePerReload 钉住全量重载的作用域：一次 reload 恰好
// 建一张表（贯穿 buildSnapshots），计数为 misses=T / hits=A−T；第二次 reload 必须
// 是**另一张**表，且模板改动后的摘要真的变（跨 reload 不泄漏）。
func TestReloadBuildsOneDigestCachePerReload(t *testing.T) {
	tp := &domain.Template{
		ID: 21, BaseURL: "https://u/v1", CredentialType: credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		Models:           []string{"m-1"},
	}
	a1, a2, a3 := acc(1, tp, 4), acc(2, tp, 4), acc(3, tp, 4)
	ldr := newMemLoader(map[int64][]*domain.Account{10: {a1, a2, a3}})
	s := New(testCfg(), ldr, newTestRuleEngine(t), nil, nil, nil, nil)

	rec := installDigestCacheRecorder(t)
	require.NoError(t, s.reload(context.Background()))
	require.Len(t, rec.caches, 1, "一次全量重载恰好一张作用域表")
	first := rec.caches[0]
	require.Equal(t, 1, first.misses, "misses = 模板数")
	require.Equal(t, 2, first.hits, "hits = 账号数 − 模板数")
	require.Equal(t, digestStrings([]string{"m-1"}), s.publisher.pending.byID[1].static.Load().key.modelsDigest)

	// 模板内容就地改动：下一次 reload 是新作用域 ⇒ 必须重算。
	tp.Models = []string{"m-1", "m-2"}
	rec.reset()
	require.NoError(t, s.reload(context.Background()))
	require.Len(t, rec.caches, 1)
	second := rec.caches[0]
	require.Equal(t, digestStrings([]string{"m-1", "m-2"}),
		s.publisher.pending.byID[1].static.Load().key.modelsDigest,
		"模板改了摘要必须变（本轮 reload 的新叶必须带新摘要；跨 reload 复用同一张表会返回旧字节）")
	require.NotSame(t, first, second, "跨 reload 必须是新表（包级表会让上面一行变陈旧）")
	require.Equal(t, 1, second.misses, "新作用域必须未命中")
	require.Equal(t, 2, second.hits)
}

// TestBatchInvalidationDigestCacheSpansWholeBatch 钉住批次作用域贯穿**两个组**：
// 一个批次恰好建一张表（也恰好问一次 NewBatch），并同时喂给 buildSnapshots、
// :188（保留剥离分支）与 :245（新叶重派生分支）。两处直构点任一漏传缓存，hits 都会
// 少一截；把批次状态挪进 for 循环，`newBatches` 会从 1 变 2。
//
// 为什么必须是 ≥2 组：只失效 1 个组时 "rec.caches 恰好 1 张" 在"1 组 1 表"下**恒成立**，
// 名字承诺的"整批"没有任何断言承担（此前正是如此）。
//
// 夹具：g1={a1,a2}、g2={a1,a2}（同模板共享一枚指针），批次把 g1 改成 {a1,a3} 后
// 失效 {1,2}：
//   - 步 1 buildSnapshots(g1) 构造 a1,a3 → 1 miss + 1 hit；
//   - a2 从 g1 移除但仍在 g2 ⇒ :188 保留剥离 → +1 hit（用旧叶的模板指针）；
//   - :245 对 a1,a3 重派生 groupIDs → +2 hit；
//   - 步 2 buildSnapshots(g2) 构造 a1,a2 → +2 hit（同一张表，模板已见过）；
//   - :245 对 a1,a2 重派生 groupIDs → +2 hit。
//
// 合计 misses=1、hits=8（= 9 次构造 − 1 枚模板）。groupIDs 的顺序是**最后一次触达
// 在前**：a1 在步 2 被重派生为 [2,1]（gid 先、其它组按旧序），a2 只剩 [2]。
func TestBatchInvalidationDigestCacheSpansWholeBatch(t *testing.T) {
	tp := tpl(31, domain.FormatOpenAIChat, []string{"m"})
	a1, a2, a3 := acc(1, tp, 4), acc(2, tp, 4), acc(3, tp, 4)
	ldr := &countingBatchLoader{t1Loader: newT1Loader(map[int64][]*domain.Account{
		1: {a1, a2},
		2: {a1, a2},
	})}
	s := New(testCfg(), ldr, newTestRuleEngine(t), nil, nil, nil, nil)
	require.NoError(t, s.reload(context.Background()))

	rec := installDigestCacheRecorder(t)
	rec.reset()
	ldr.byGroup[1] = []*domain.Account{a1, a3}
	s.InvalidateGroups([]int64{1, 2})

	require.Equal(t, 1, ldr.newBatches(),
		"一个批次只问一次 NewBatch：把 newBatchState 挪进 for 循环会变成每组一次（本断言是它的机械防线）")
	require.Len(t, rec.caches, 1, "一个批次恰好一张作用域表（不是一组一张）")
	batch := rec.caches[0]
	require.Equal(t, 1, batch.misses, "misses = 批次内不同模板对象数")
	require.Equal(t, 8, batch.hits,
		"hits = 9 次构造 − 1 枚模板：两个组的 buildSnapshots(2+2) + 保留剥离(1) + 两次重派生(2+2) 都必须吃同一张表")

	sv := s.publisher.pending
	require.NotNil(t, sv)
	// 命中路径不得改变任何事实：三枚叶的缓存键仍等于独立 oracle（凭据摘要维度由
	// 1125 组参照测试覆盖）。
	requirePlanKeyMatchesFrozen(t, planKeyOf(sv.byID[1].static.Load()), *a1, tp, []int64{2, 1},
		"a1 在批次里被两个组各触达一次：最后一次重派生把 gid=2 放在首位")
	requirePlanKeyMatchesFrozen(t, planKeyOf(sv.byID[2].static.Load()), *a2, tp, []int64{2},
		"保留剥离分支（:188）的键必须按自己的 groupIDs 重派生")
	requirePlanKeyMatchesFrozen(t, planKeyOf(sv.byID[3].static.Load()), *a3, tp, []int64{1})
}

// TestDigestCacheHitSkipsDerivation 把"命中 = 不再执行派生"变成分配数字：首次
// 构造要付 5 项摘要的账（本夹具 209 allocs），命中构造只剩与模板无关的 5 项。
// 这条与计数断言互补：计数证明"读了缓存"，分配证明"省下了活"。
func TestDigestCacheHitSkipsDerivation(t *testing.T) {
	models := make([]string, 0, 99)
	for i := 0; i < 99; i++ {
		models = append(models, fmt.Sprintf("m-%03d", i))
	}
	tp := &domain.Template{
		ID: 41, BaseURL: "https://u/v1", CredentialType: credential.TypeAPIKey,
		SupportedFormats: slices.Clone(domain.RoutableFormats()),
		Models:           models,
		FormatModels:     map[domain.RequestFormat][]string{domain.FormatAnthropic: slices.Clone(models)},
	}
	a := digestFixtureAccount(tp)
	eg := directEgress()

	miss := testing.AllocsPerRun(1, func() {
		digestCacheBenchSink = newSnapshotStatic(a, tp, nil, eg, newTemplateDigestCache())
	})
	warm := newTemplateDigestCache()
	digestCacheBenchSink = newSnapshotStatic(a, tp, nil, eg, warm) // 先付一次未命中
	hit := testing.AllocsPerRun(20, func() {
		digestCacheBenchSink = newSnapshotStatic(a, tp, nil, eg, warm)
	})
	t.Logf("allocs/构造：未命中 %.0f、命中 %.0f（差 %.0f）", miss, hit, miss-hit)
	require.GreaterOrEqual(t, miss, float64(200), "未命中必须真的算 5 项摘要（本夹具 ≈209 allocs）")
	require.LessOrEqual(t, hit, float64(6), "命中只该剩与模板无关的 5 项分配")
	require.GreaterOrEqual(t, miss-hit, float64(200), "命中必须省下整个摘要派生")
}

// digestCacheBenchSink 防死代码消除。
var digestCacheBenchSink *snapshotStatic
