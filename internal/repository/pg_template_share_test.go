// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// U22 §3 不变量 3 的端到端半边（真库、两个生产载入入口各测一遍）：
//
//	同一作用域内 | 同 template_id 的两个账号 ⇒ 同一枚 *domain.Template
//	同一作用域内 | 不同 template_id          ⇒ 不同对象
//	跨 reload    | 两个作用域                 ⇒ 不是同一枚（作用域新对象，新指针）
//
// 计数器的断言把"每模板一次转换"变成可执行数字：misses = 不同 template_id 数，
// hits = 账号数 − 不同 template_id 数。映射层的同形状断言见
// mapping_template_share_internal_test.go。
func TestPGLoadSharesOneTemplateObjectPerTemplateID(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	tplA := seedPGTemplateNamed(t, repos, "tpl-share-a")
	tplB := seedPGTemplateNamed(t, repos, "tpl-share-b")
	g := seedPGGroup(t, repos, "grp-share")
	a1 := seedPGAccount(t, repos, tplA.ID, "share-a1")
	a2 := seedPGAccount(t, repos, tplA.ID, "share-a2")
	b1 := seedPGAccount(t, repos, tplB.ID, "share-b1")
	for _, id := range []int64{a1.ID, a2.ID, b1.ID} {
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, id, []int64{g.ID}))
	}

	// 每个 loader 拿一个**新作用域**，这样"跨 reload 不是同一枚"与本用例的计数
	// 断言同时成立：3 个账号、2 个模板 ⇒ misses=2、hits=1。
	loaders := []struct {
		name string
		load func(scope *repository.TemplateLoadScope) map[int64]*domain.Account
	}{
		{"LoadGroupsAccountsFull", func(scope *repository.TemplateLoadScope) map[int64]*domain.Account {
			byGroup, _, err := repos.Groups.LoadGroupsAccountsFullScoped(ctx, scope)
			require.NoError(t, err)
			out := make(map[int64]*domain.Account, len(byGroup[g.ID]))
			for _, a := range byGroup[g.ID] {
				out[a.ID] = a
			}
			return out
		}},
		{"LoadGroupAccounts", func(scope *repository.TemplateLoadScope) map[int64]*domain.Account {
			members, err := repos.Groups.LoadGroupAccountsScoped(ctx, g.ID, scope)
			require.NoError(t, err)
			out := make(map[int64]*domain.Account, len(members))
			for _, a := range members {
				out[a.ID] = a
			}
			return out
		}},
	}

	for _, l := range loaders {
		t.Run(l.name, func(t *testing.T) {
			firstScope := repository.NewTemplateLoadScope()
			first := l.load(firstScope)
			require.Len(t, first, 3)

			require.NotNil(t, first[a1.ID].Template)
			require.Same(t, first[a1.ID].Template, first[a2.ID].Template,
				"同一作用域内同 template_id 必须共享同一枚 *domain.Template")
			require.NotSame(t, first[a1.ID].Template, first[b1.ID].Template,
				"不同 template_id 必须是不同对象")
			require.Equal(t, tplA.ID, first[a1.ID].Template.ID)
			require.Equal(t, tplB.ID, first[b1.ID].Template.ID)
			require.Equal(t, tplA.BaseURL, first[a1.ID].Template.BaseURL,
				"共享对象的内容仍是从模板行转出来的那一份")
			require.Equal(t, tplA.ID, first[a1.ID].TemplateID, "账号侧的 template_id 不受共享影响")
			require.Equal(t, 2, firstScope.Misses(), "misses = 不同 template_id 数")
			require.Equal(t, 1, firstScope.Hits(), "hits = 账号数 - 不同 template_id 数")

			// 跨 reload：新作用域必须给新对象（同一 template_id 的账号之间仍然共享）。
			second := l.load(repository.NewTemplateLoadScope())
			require.Same(t, second[a1.ID].Template, second[a2.ID].Template)
			require.NotSame(t, first[a1.ID].Template, second[a1.ID].Template,
				"跨 reload 必须是新对象：作用域生命周期 = 一次构建/一个批次")
			require.NotSame(t, first[b1.ID].Template, second[b1.ID].Template)
		})
	}
}

// TestPGUnscopedLoadersKeepScopePerCall 钉住**既有调用点**（不传作用域）的契约：
// 一次调用内共享，两次调用之间是新对象。把作用域做成包级单例会让"跨调用"那半条红；
// 这条与上面两条互补——显式作用域测"批次共享"，nil 作用域测"默认不跨调用复用"。
func TestPGUnscopedLoadersKeepScopePerCall(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	tpl := seedPGTemplateNamed(t, repos, "tpl-unscoped")
	g := seedPGGroup(t, repos, "grp-unscoped")
	a1 := seedPGAccount(t, repos, tpl.ID, "unscoped-a1")
	a2 := seedPGAccount(t, repos, tpl.ID, "unscoped-a2")
	for _, id := range []int64{a1.ID, a2.ID} {
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, id, []int64{g.ID}))
	}

	byID := func(m map[int64][]*domain.Account) map[int64]*domain.Account {
		out := make(map[int64]*domain.Account, len(m[g.ID]))
		for _, a := range m[g.ID] {
			out[a.ID] = a
		}
		return out
	}
	full1, _, err := repos.Groups.LoadGroupsAccountsFull(ctx)
	require.NoError(t, err)
	full2, _, err := repos.Groups.LoadGroupsAccountsFull(ctx)
	require.NoError(t, err)
	first, second := byID(full1), byID(full2)
	require.Same(t, first[a1.ID].Template, first[a2.ID].Template, "调用内共享")
	require.NotSame(t, first[a1.ID].Template, second[a1.ID].Template,
		"两次调用之间必须是新对象（作用域不得是包级单例）")

	one1, err := repos.Groups.LoadGroupAccounts(ctx, g.ID)
	require.NoError(t, err)
	one2, err := repos.Groups.LoadGroupAccounts(ctx, g.ID)
	require.NoError(t, err)
	idx := func(list []*domain.Account) map[int64]*domain.Account {
		out := make(map[int64]*domain.Account, len(list))
		for _, a := range list {
			out[a.ID] = a
		}
		return out
	}
	g1, g2 := idx(one1), idx(one2)
	require.Same(t, g1[a1.ID].Template, g1[a2.ID].Template, "调用内共享")
	require.NotSame(t, g1[a1.ID].Template, g2[a1.ID].Template,
		"两次调用之间必须是新对象（作用域不得是包级单例）")
}

// TestPGBatchScopeSharesTemplateAcrossGroups 是"组级失效批次"那条必须的不变量：
// invalidate_batch 对每个受影响组各调一次单组载入（invalidate_batch.go:73-89），
// 同一模板若在多个组里出现，**一个批次作用域**必须让它们共享同一枚指针；逐组各建
// 作用域则拿到多枚。前者是第二步按指针键做摘要缓存的前提（否则只能做到"每组每
// 模板一次"）。
func TestPGBatchScopeSharesTemplateAcrossGroups(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	tpl := seedPGTemplateNamed(t, repos, "tpl-batch-share")
	g1 := seedPGGroup(t, repos, "grp-batch-1")
	g2 := seedPGGroup(t, repos, "grp-batch-2")
	a1 := seedPGAccount(t, repos, tpl.ID, "batch-a1")
	a2 := seedPGAccount(t, repos, tpl.ID, "batch-a2")
	a3 := seedPGAccount(t, repos, tpl.ID, "batch-a3")
	require.NoError(t, repos.Accounts.SetAccountGroups(ctx, a1.ID, []int64{g1.ID}))
	require.NoError(t, repos.Accounts.SetAccountGroups(ctx, a2.ID, []int64{g1.ID}))
	require.NoError(t, repos.Accounts.SetAccountGroups(ctx, a3.ID, []int64{g2.ID}))

	loadGroup := func(scope *repository.TemplateLoadScope, gid int64) map[int64]*domain.Account {
		members, err := repos.Groups.LoadGroupAccountsScoped(ctx, gid, scope)
		require.NoError(t, err)
		out := make(map[int64]*domain.Account, len(members))
		for _, a := range members {
			out[a.ID] = a
		}
		return out
	}

	// ① 一个批次作用域贯穿两个组 ⇒ 同模板只有一枚指针。
	batch := repository.NewTemplateLoadScope()
	fromG1 := loadGroup(batch, g1.ID)
	fromG2 := loadGroup(batch, g2.ID)
	require.Len(t, fromG1, 2)
	require.Len(t, fromG2, 1)
	require.Same(t, fromG1[a1.ID].Template, fromG1[a2.ID].Template, "同组内共享")
	require.Same(t, fromG1[a1.ID].Template, fromG2[a3.ID].Template,
		"同一批次作用域内跨组也必须共享同一枚指针")
	require.Equal(t, 1, batch.Misses(), "misses = 批次内不同 template_id 数")
	require.Equal(t, 2, batch.Hits(), "hits = 账号数 - 不同 template_id 数")

	// ② 对照组：逐组各建作用域（旧形状）⇒ 同模板拿到不同枚指针。这正是批次作用域
	// 要消除的退化，也是本用例的"可观测面"（防 ① 恒真）。
	perGroup := loadGroup(repository.NewTemplateLoadScope(), g1.ID)
	require.NotSame(t, fromG1[a1.ID].Template, perGroup[a1.ID].Template,
		"逐组各建作用域 = 每组一份模板对象（批次内不共享）")
}
