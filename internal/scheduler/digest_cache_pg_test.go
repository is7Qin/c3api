// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"context"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"
	"github.com/is7qin/c3api/pkg/egress"

	// Registers pgtest's repository-dependent hooks.
	_ "github.com/is7qin/c3api/internal/testsupport/pgtest/pgrepo"
)

// TestPGLoadGroupAccountsFeedsDigestCache 是 spec §4.3 的端到端半边：**真实
// LoadGroupAccounts** 的返回值喂给快照构造后，
//
//	① 同一个 *domain.Template 指针被 ≥2 个账号共享（第一步的行为，缓存的前提）；
//	② 摘要缓存真的命中（hits ≥ 1，misses = 不同模板数）。
//
// 只在 scheduler 层手搓"两账号共享一枚模板"的夹具**替代不了**这条：那证明的是
// 夹具，不是载入器（momus 的 cheat_nor1 正是靠这个缺口全绿）。
func TestPGLoadGroupAccountsFeedsDigestCache(t *testing.T) {
	dsn := pgtest.Clone(t)
	ctx := context.Background()
	pool := pgtest.OpenPool(t, dsn)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), false)
	require.NoError(t, err)

	tpl, err := repos.CreateTemplate(ctx, &domain.Template{
		Name: "t-digest-cache", BaseURL: "http://upstream.example.com",
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		Models:           []string{"m-1", "m-2"},
		ModelMapping:     domain.ModelMapping{},
	})
	require.NoError(t, err)
	g, err := repos.CreateGroup(ctx, &domain.Group{
		Name: "g-digest-cache", Visibility: domain.GroupVisibilityPublic, PriceMultiplier: 10000,
	})
	require.NoError(t, err)
	for _, name := range []string{"digest-cache-a1", "digest-cache-a2"} {
		acc, cerr := repos.CreateAccount(ctx, &domain.Account{
			Name: name, TemplateID: tpl.ID, UpstreamKey: "sk-upstream",
			MaxConcurrency: 4, Enabled: true,
		})
		require.NoError(t, cerr)
		require.NoError(t, repos.SetAccountGroups(ctx, acc.ID, []int64{g.ID}))
	}

	accs, err := repos.LoadGroupAccounts(ctx, g.ID)
	require.NoError(t, err)
	require.Len(t, accs, 2)
	require.NotNil(t, accs[0].Template)
	require.NotNil(t, accs[1].Template)
	require.Same(t, accs[0].Template, accs[1].Template,
		"真实载入器必须让同 template_id 的账号共享同一枚 *domain.Template（缓存的键就是它）")

	// 本用例是**单组载入的半边**：直接调 buildSnapshots 喂一组账号，每账号恰好
	// 构造 1 次（buildSnapshots 的 infoMap 循环），不经过 invalidateOneIntoLocked
	// 的第二次 groupIDs 重派生 ⇒ 构造次数 = 账号数 A、不同模板对象数 = T = 1，
	// 精确不变量是 **hits = 构造次数 − 不同模板对象数 = A − T**。
	// （生产单组失效的公式是 2A − T，见 TestPGBatchInvalidationHitsDigestCacheAcrossGroups。）
	cache := newTemplateDigestCache()
	_, byID := buildSnapshots(map[int64][]*domain.Account{g.ID: accs}, nil, egress.NewPool(nil), cache)
	const distinctTemplates = 1
	constructions := len(accs) // 本场景每账号构造一次
	require.GreaterOrEqual(t, cache.hits, 1, "真实载入器 → 快照必须至少命中一次")
	require.Equal(t, distinctTemplates, cache.misses, "misses = 不同模板对象数（T）")
	require.Equal(t, constructions-distinctTemplates, cache.hits,
		"hits = 构造次数 − 不同模板对象数：单组载入 A 次构造、T=1 ⇒ A − T")
	require.Same(t, byID[accs[0].ID].static.Load().tpl, byID[accs[1].ID].static.Load().tpl,
		"快照里的叶必须仍指向同一枚模板对象")
	// 命中拿到的摘要必须来自这枚真实模板对象（不是零值、不是别人的）：用独立的
	// 旧算法 oracle 整枚比对，5 项摘要逐字节覆盖。
	require.NotEmpty(t, accs[0].Template.Models, "precondition: 模板行带模型集")
	require.Equal(t, digestStrings(accs[0].Template.Models),
		byID[accs[0].ID].static.Load().key.modelsDigest)
	requirePlanKeyMatchesFrozen(t, planKeyOf(byID[accs[0].ID].static.Load()), *accs[0], accs[0].Template, []int64{g.ID})
	requirePlanKeyMatchesFrozen(t, planKeyOf(byID[accs[1].ID].static.Load()), *accs[1], accs[1].Template, []int64{g.ID})
}

// pgDigestFixture 是一次真库夹具：**一枚**模板 + 两个组 + 每组两个账号。跨组共享
// 的对象是模板行（摘要缓存的单位），账号彼此不共享——这正是"每组各建一套模板
// 指针"退化的观测面。
type pgDigestFixture struct {
	repos  *repository.Repository
	g1     *domain.Group
	g2     *domain.Group
	g1Accs []*domain.Account
	g2Accs []*domain.Account
}

func (f *pgDigestFixture) accounts() []*domain.Account {
	out := make([]*domain.Account, 0, len(f.g1Accs)+len(f.g2Accs))
	out = append(out, f.g1Accs...)
	out = append(out, f.g2Accs...)
	return out
}

// scheduler 用**真 repository** 作载入器（生产接线同款：cmd/server 传 repos.Groups）。
func (f *pgDigestFixture) scheduler(t *testing.T) *Scheduler {
	t.Helper()
	return New(testCfg(), f.repos.Groups, newTestRuleEngine(t), nil, nil, nil, nil)
}

func newPGDigestFixture(t *testing.T) *pgDigestFixture {
	t.Helper()
	dsn := pgtest.Clone(t)
	ctx := context.Background()
	pool := pgtest.OpenPool(t, dsn)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), false)
	require.NoError(t, err)

	tpl, err := repos.CreateTemplate(ctx, &domain.Template{
		Name: "tpg-cross-group", BaseURL: "http://upstream.example.com",
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		Models:           []string{"m-1", "m-2"},
		ModelMapping:     domain.ModelMapping{},
	})
	require.NoError(t, err)
	f := &pgDigestFixture{repos: repos}
	for _, name := range []string{"gpg-cross-1", "gpg-cross-2"} {
		g, gerr := repos.CreateGroup(ctx, &domain.Group{
			Name: name, Visibility: domain.GroupVisibilityPublic, PriceMultiplier: 10000,
		})
		require.NoError(t, gerr)
		if f.g1 == nil {
			f.g1 = g
		} else {
			f.g2 = g
		}
	}
	seed := func(g *domain.Group, names ...string) []*domain.Account {
		out := make([]*domain.Account, 0, len(names))
		for _, name := range names {
			acc, cerr := repos.CreateAccount(ctx, &domain.Account{
				Name: name, TemplateID: tpl.ID, UpstreamKey: "sk-upstream",
				MaxConcurrency: 4, Enabled: true,
			})
			require.NoError(t, cerr)
			require.NoError(t, repos.SetAccountGroups(ctx, acc.ID, []int64{g.ID}))
			out = append(out, acc)
		}
		return out
	}
	f.g1Accs = seed(f.g1, "gpg-a1", "gpg-a2")
	f.g2Accs = seed(f.g2, "gpg-a3", "gpg-a4")
	return f
}

// TestPGBatchInvalidationHitsDigestCacheAcrossGroups 是 U22 §2.1 在**生产链路**上
// 的闭合断言（真 repository 作载入器，不是预共享指针的假 loader）：
//
// 一个 ≥2 组的失效批次里，摘要缓存必须做到 misses = 批次内不同模板数（1），其余
// 构造全部命中——包括第二个组的构造（跨组命中）。载入若退回未 scoped 变体，两个组
// 各拿一枚模板对象：misses 变 2、hits 少 1（每组第一次构造由命中变未命中）。
//
// 构造次数是 2×账号数：invalidateOneIntoLocked 对每个新账号构造两次静态叶
// （buildSnapshots 一次、groupIDs 重派生一次），两处都必须吃同一张批次表。
func TestPGBatchInvalidationHitsDigestCacheAcrossGroups(t *testing.T) {
	f := newPGDigestFixture(t)
	s := f.scheduler(t)
	require.Implements(t, (*ScopedGroupLoader)(nil), f.repos.Groups,
		"生产载入器必须提供 scoped 组载入能力（否则批次内模板对象无法跨组复用）")

	rec := installDigestCacheRecorder(t)
	rec.reset()
	s.InvalidateGroups([]int64{f.g2.ID, f.g1.ID}) // 乱序传入：批次内部按 id 排序

	require.Len(t, rec.caches, 1, "一个批次恰好一张摘要表（不是一组一张）")
	cache := rec.caches[0]
	accounts := len(f.accounts())
	const distinctTemplates = 1
	// 冷批次的构造次数是 **2A**，不是 A：每个账号被构造两次——
	//   ① invalidate_batch.go:187 的 buildSnapshots 一次；
	//   ② 同函数末尾（:282）为新叶重派生 groupIDs 再一次。
	// 两处必须吃同一张批次表，否则 ② 会对同一枚模板再未命中一次。本夹具没有
	// 账号被移出组，跨代保留分支（:225）不触发，故精确公式是 **hits = 2A − T**。
	const constructionsPerAccount = 2
	require.Equal(t, distinctTemplates, cache.misses,
		"misses = 批次内不同模板对象数：真 repo 的批次作用域必须让两个组拿到同一枚模板指针")
	require.Equal(t, constructionsPerAccount*accounts-distinctTemplates, cache.hits,
		"hits = 构造次数 − 不同模板对象数 = 2A − T：跨组的 buildSnapshots 与 groupIDs 重派生必须全部命中（未 scoped 时 misses=2、hits 少 1）")

	sv := s.publisher.pending
	require.NotNil(t, sv, "成功批次必须 stage 恰好一个冻结根")
	require.Same(t, sv.byID[f.g1Accs[0].ID].static.Load().tpl, sv.byID[f.g2Accs[0].ID].static.Load().tpl,
		"跨组的叶必须共享同一枚模板对象——摘要缓存跨组命中的唯一来源")
	for _, acc := range f.accounts() {
		av := sv.byID[acc.ID].static.Load()
		requirePlanKeyMatchesFrozen(t, planKeyOf(av), av.acc, av.tpl, av.groupIDs,
			"账号 %d：命中路径的键必须与独立 oracle 相等（缓存不得改变任何事实；凭据摘要维度由 1125 组参照测试覆盖）", acc.ID)
	}
}

// TestPGReloadDigestCacheSpansGroups 是真库上的全量半边：一次 reload 恰好一张表、
// misses = 不同模板数、hits = 账号数 − 不同模板数。装载器若退化成"每个账号各转
// 一份模板"（或 cache 不在 reload 作用域内），misses 立刻变成账号数。
func TestPGReloadDigestCacheSpansGroups(t *testing.T) {
	f := newPGDigestFixture(t)
	s := f.scheduler(t)

	rec := installDigestCacheRecorder(t)
	rec.reset()
	require.NoError(t, s.reload(context.Background()))

	require.Len(t, rec.caches, 1, "一次全量重载恰好一张作用域表")
	cache := rec.caches[0]
	accounts := len(f.accounts())
	// reload 是**一次扫描**：每账号恰好 1 次构造（buildSnapshots 的 infoMap 循环），
	// 没有 invalidateOneIntoLocked 的第二次重派生 ⇒ 精确公式是 **hits = A − T**
	// （冷批次的 2A − T 不适用于这里，两个场景的公式不能互相代入）。
	const distinctTemplates = 1
	require.Equal(t, distinctTemplates, cache.misses,
		"misses = 不同模板对象数（真库全量装在一次扫描里只转一枚模板）")
	require.Equal(t, accounts-distinctTemplates, cache.hits,
		"hits = 构造次数 − 不同模板对象数 = A − T（一次扫描，每账号构造一次）")

	sv := s.publisher.pending
	require.NotNil(t, sv)
	require.Same(t, sv.byID[f.g1Accs[0].ID].static.Load().tpl, sv.byID[f.g2Accs[0].ID].static.Load().tpl,
		"全量装同样必须让跨组的同模板账号共享同一枚指针")
}

// TestPGWarmBatchDigestCacheMissesExceedTemplateCount 钉住 **warm 批次**的真实
// 计数语义：`misses` 不是模板行数，**不要**硬凑 `misses = T`。
//
// 夹具（真 repository 作载入器，1 枚模板）：
//
//   - 预热：a1 同时在 g1、g2（"移出组但仍属其它组"的前提），reload 建出**上一代**
//     的模板对象；两个组的叶都指向它。
//   - 服务端改动：a1 移出 g1（仍留 g2）⇒ g1 变成 {a2}。
//   - InvalidateGroups([g1])：批次载入给的是**本代**新对象，但 buildSnapshots 拿
//     它与上一代叶做 staticKeyOf（值比较，指针不入键）⇒ 判等 ⇒ 整叶复用，
//     本代对象被丢弃。此后两处直构点用的是**上一代**叶的 `ost.tpl`/`nst.tpl`。
//
// 于是三次构造的归属是：本代对象未命中（构造了却因复用被丢）、上一代对象在
// "保留剥离"（:225）与"groupIDs 重派生"（:282）各命中/未命中一次 —— 见下面逐项
// 断言。结论：**misses 由"本批触到的不同模板对象数"决定，不是由模板行数决定**。
//
// 这是**已知语义，不是 bug**：
//
//   - 正确性不受影响：每个对象都按**自己的内容**派生，同内容的不同对象得到同一组
//     字节；本轮没有任何陈旧摘要（陈旧需要一枚**内容被就地改过**的对象，那由
//     template_digest_cache_staleness_test.go 单独钉住）。
//   - 代价是"本代对象派生一次、整叶复用后再用上一代对象派生一次"，即每次跨代
//     复用白付一次摘要。属**待优化**（可把旧叶已定型的 5 项摘要接续过来、只改
//     groupIDs），但收益未测量，不在 U22 范围内；真做了这一条会变绿得不同——
//     那正是"已知语义被改掉"的信号。
func TestPGWarmBatchDigestCacheMissesExceedTemplateCount(t *testing.T) {
	f := newPGDigestFixture(t)
	s := f.scheduler(t)
	ctx := context.Background()

	// 预热：a1 跨 g1、g2。
	crossGroup := f.g1Accs[0]
	require.NoError(t, f.repos.SetAccountGroups(ctx, crossGroup.ID, []int64{f.g1.ID, f.g2.ID}))
	require.NoError(t, s.reload(ctx))
	warm := s.publisher.pending
	require.NotNil(t, warm, "预热 reload 必须 stage 一个根")
	prevTpl := warm.byID[crossGroup.ID].static.Load().tpl
	require.NotNil(t, prevTpl, "预热叶持有上一代模板对象")
	require.Same(t, prevTpl, warm.byID[f.g2Accs[0].ID].static.Load().tpl,
		"同一模板在上一代里只有一枚对象（跨组共享）")

	// 服务端改动：a1 移出 g1，仍留 g2 ⇒ 只有 g1 需要失效。
	require.NoError(t, f.repos.SetAccountGroups(ctx, crossGroup.ID, []int64{f.g2.ID}))

	rec := installDigestCacheRecorder(t)
	rec.reset()
	s.InvalidateGroups([]int64{f.g1.ID})
	require.Len(t, rec.caches, 1, "一个批次仍然恰好一张表")
	cache := rec.caches[0]

	const (
		distinctTemplates = 1 // 模板**行**数
		constructions     = 3 // 本代对象 + 保留剥离 + groupIDs 重派生
	)
	require.Equal(t, 2, cache.misses,
		"misses = 本批触到的不同模板**对象**数：本代对象 + 上一代对象（跨代叶复用）")
	require.Greater(t, cache.misses, distinctTemplates,
		"warm 批次：misses > 模板行数 —— 跨代对象是两个键；这条钉住真实语义，不硬凑 misses = T")
	require.Equal(t, constructions-cache.misses, cache.hits,
		"hits = 构造次数 − 不同模板对象数（命中数由对象身份决定，不由模板行数决定）")
	require.Len(t, cache.entries, 2, "表里留下两枚对象：上一代的 + 本代的（后者随整叶复用被丢弃）")

	sv := s.publisher.pending
	require.NotNil(t, sv)
	// 保留剥离分支接续上一代的指针；a2 的叶因 staticKey 判等被整叶复用，同样是
	// 上一代对象 —— 这就是"本代对象白付一次派生"的全部原因。
	require.Same(t, prevTpl, sv.byID[crossGroup.ID].static.Load().tpl,
		"保留剥离分支（:225）用的是上一代叶的 ost.tpl")
	require.Same(t, prevTpl, sv.byID[f.g1Accs[1].ID].static.Load().tpl,
		"a2 的静态事实未变 ⇒ 整叶复用，叶仍指向上一代对象")

	// 计数偏离不改变任何事实：每枚叶的键仍等于按它**自己**的输入独立重算的结果。
	for _, acc := range f.accounts() {
		static := sv.byID[acc.ID].static.Load()
		requirePlanKeyMatchesFrozen(t, planKeyOf(static), static.acc, static.tpl, static.groupIDs,
			"账号 %d：warm 批次的键必须与独立 oracle 相等（多出的一次未命中不改变事实；凭据摘要维度由 1125 组参照测试覆盖）", acc.ID)
	}
}
