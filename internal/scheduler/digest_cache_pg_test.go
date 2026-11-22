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

	cache := newTemplateDigestCache()
	_, byID := buildSnapshots(map[int64][]*domain.Account{g.ID: accs}, nil, egress.NewPool(nil), cache)
	require.GreaterOrEqual(t, cache.hits, 1, "真实载入器 → 快照必须至少命中一次")
	require.Equal(t, 1, cache.misses, "misses = 不同模板数")
	require.Equal(t, len(accs)-1, cache.hits, "hits = 账号数 − 模板数")
	require.Same(t, byID[accs[0].ID].static.Load().tpl, byID[accs[1].ID].static.Load().tpl,
		"快照里的叶必须仍指向同一枚模板对象")
	// 命中拿到的摘要必须来自这枚真实模板对象（不是零值、不是别人的）：用独立的
	// 旧算法 oracle 整枚比对，5 项摘要逐字节覆盖。
	require.NotEmpty(t, accs[0].Template.Models, "precondition: 模板行带模型集")
	require.Equal(t, digestStrings(accs[0].Template.Models),
		byID[accs[0].ID].static.Load().key.modelsDigest)
	require.Equal(t, frozenPlanKey(*accs[0], accs[0].Template, []int64{g.ID}),
		planKeyOf(byID[accs[0].ID].static.Load()))
	require.Equal(t, frozenPlanKey(*accs[1], accs[1].Template, []int64{g.ID}),
		planKeyOf(byID[accs[1].ID].static.Load()))
}
