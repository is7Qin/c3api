// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/latch"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/rulepersist"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"
	"github.com/is7qin/c3api/pkg/logx"

	// Registers pgtest's repository-dependent hooks.
	_ "github.com/is7qin/c3api/internal/testsupport/pgtest/pgrepo"
)

// u39PGRepos 用真 PG 克隆建一套仓库 + 池（裸 SQL 造脏行需要池）。
func u39PGRepos(t *testing.T) (*repository.Repository, *pgxpool.Pool) {
	t.Helper()
	dsn := pgtest.Clone(t)
	pool := pgtest.OpenPool(t, dsn)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), false)
	require.NoError(t, err)
	return repos, pool
}

// u39SeedBPSTemplate 建 BPS 模板 + 一个组（装配层用例的最小拓扑）。
func u39SeedBPSTemplate(t *testing.T, repos *repository.Repository) (*domain.Template, *domain.Group) {
	t.Helper()
	ctx := context.Background()
	tpl, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "u39-sched-bps", BaseURL: "",
		CredentialType:   credential.TypeBPS,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
		Models:           []string{"gpt-5.6-sol"},
		ModelMapping:     domain.ModelMapping{},
	})
	require.NoError(t, err)
	g, err := repos.Groups.CreateGroup(ctx, &domain.Group{
		Name: "u39-sched-g", Visibility: domain.GroupVisibilityPublic, PriceMultiplier: 10000,
	})
	require.NoError(t, err)
	return tpl, g
}

// u39SeedAccount 直接落一行账号 + 组成员关系（BPS 账号不走通用 CreateAccount）。
func u39SeedAccount(t *testing.T, repos *repository.Repository, tplID, groupID int64, name string) int64 {
	t.Helper()
	ctx := context.Background()
	acc, err := repos.Client.Account.Create().
		SetName(name).SetTemplateID(tplID).SetUpstreamKey("").
		SetMaxConcurrency(4).SetEnabled(true).Save(ctx)
	require.NoError(t, err)
	require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{groupID}))
	return acc.ID
}

// u39SeedBPSRow 写一条合法 BPS ext 行。
func u39SeedBPSRow(t *testing.T, pool *pgxpool.Pool, accountID int64) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO account_exts
		(account_id, credential_type, codex_identity, codex_oauth_token,
		 codex_oauth_refresh_token, codex_oauth_expires_at, codex_pat_key,
		 codex_email, codex_account_id)
		VALUES ($1, 'bps', '{"bps":{"profile_id":"p-1"}}'::jsonb, 'at', 'rt', NULL, NULL, NULL, 'acct')`,
		accountID)
	require.NoError(t, err)
}

// u39ErrorLines 读日志文件里的 ERROR 行（每行一条 JSON 记录）。
func u39ErrorLines(t *testing.T, lg *logx.Logger, path string) []string {
	t.Helper()
	require.NoError(t, lg.Sync())
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	var out []string
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.Contains(ln, `"level":"error"`) {
			out = append(out, ln)
		}
	}
	return out
}

// TestReloadSkipsMismatchedAccountAndLogsError 是 J7.8 的装配层用例：脏行存在 ⇒
// 装载**成功**、该账号不在快照、其它账号照常可路由、计数 1、K 未被覆盖、记一条
// ERROR（带 account_id 与两侧类型）。
func TestReloadSkipsMismatchedAccountAndLogsError(t *testing.T) {
	repos, pool := u39PGRepos(t)
	tpl, g := u39SeedBPSTemplate(t, repos)
	ctx := context.Background()

	clean := u39SeedAccount(t, repos, tpl.ID, g.ID, "u39-sched-clean")
	u39SeedBPSRow(t, pool, clean)
	dirty := u39SeedAccount(t, repos, tpl.ID, g.ID, "u39-sched-dirty")
	u39SeedBPSRow(t, pool, dirty)
	// 裸 SQL 造脏（唯一允许的形状）：只改 credential_type 一列。
	_, err := pool.Exec(ctx, `UPDATE account_exts SET credential_type = $1 WHERE account_id = $2`,
		string(credential.TypeCodexPAT), dirty)
	require.NoError(t, err)
	// K 观测：干净账号的 K 必须进快照；脏账号的 K 不许被装载改写。
	_, err = pool.Exec(ctx, `UPDATE accounts SET identity_revision = 17 WHERE id = $1`, clean)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE accounts SET identity_revision = 41 WHERE id = $1`, dirty)
	require.NoError(t, err)

	lg, logPath := newWarnLogger(t)
	s := New(testCfg(), repos.Groups, newTestRuleEngine(t), nil, lg, nil, nil)
	require.NoError(t, s.reload(ctx), "脏行是账号级隔离：装载必须成功（不许拒发布）")

	static := s.view.Load().static
	require.NotNil(t, static)
	require.Contains(t, static.byID, clean, "其它账号照常进快照")
	require.NotContains(t, static.byID, dirty, "BPS 位错配的账号不在快照")
	require.Equal(t, int64(17), static.byID[clean].static.Load().acc.IdentityRevision)
	require.True(t, static.byID[clean].static.Load().routable, "其它账号照常可路由")

	lines := u39ErrorLines(t, lg, logPath)
	require.Len(t, lines, 1, "装载成功后按条记 ERROR（不刷屏）：%v", lines)
	require.Contains(t, lines[0], `"account_id":`+strconv.FormatInt(dirty, 10))
	require.Contains(t, lines[0], `"template_type":"bps"`)
	require.Contains(t, lines[0], `"ext_type":"codex-pat"`)

	require.Len(t, repos.Groups.LastAccountExtMismatches(), 1)
	require.Equal(t, dirty, repos.Groups.LastAccountExtMismatches()[0].AccountID)

	// 快照里没有它 ⇒ 调度面只剩干净账号；K 仍未被覆盖（库里仍是 41）。
	wireSources(s, nil, nil)
	s.compileOnce()
	sel, err := s.Select(g.ID, domain.FormatOpenAIResponses, "gpt-5.6-sol")
	require.NoError(t, err)
	require.Equal(t, clean, sel.AccountID, "其它账号照常可路由")
	after, err := repos.GetAccount(ctx, dirty)
	require.NoError(t, err)
	require.Equal(t, int64(41), after.IdentityRevision, "脏行的 K 未被覆盖")
}

// TestRulePersistLandsVerdictWhenExtProjectionFails 钉住 v4 修正 4 的后果面：
// 坏 installation_id 让 GetAccountWithTemplate 的投影失败（Ext = nil、**不报错**），
// rule_persist 的判决必须照常落库——错误面不许扩大。
func TestRulePersistLandsVerdictWhenExtProjectionFails(t *testing.T) {
	repos, pool := u39PGRepos(t)
	ctx := context.Background()
	tpl, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "u39-sched-codex", BaseURL: "",
		CredentialType:   credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
		Models:           []string{"gpt-5.6-sol"},
		ModelMapping:     domain.ModelMapping{},
	})
	require.NoError(t, err)
	g, err := repos.Groups.CreateGroup(ctx, &domain.Group{
		Name: "u39-sched-codex-g", Visibility: domain.GroupVisibilityPublic, PriceMultiplier: 10000,
	})
	require.NoError(t, err)

	acc := u39SeedAccount(t, repos, tpl.ID, g.ID, "u39-sched-bad-identity")
	// installation_id 是数字：jsonb 可解码，但 codex 身份投影会失败（坏 envelope）。
	_, err = pool.Exec(ctx, `INSERT INTO account_exts
		(account_id, credential_type, codex_identity, codex_oauth_token,
		 codex_oauth_refresh_token, codex_oauth_expires_at, codex_pat_key,
		 codex_email, codex_account_id)
		VALUES ($1, 'codex-oauth', '{"installation_id":123}'::jsonb, 'at', 'rt', NULL, NULL, NULL, NULL)`, acc)
	require.NoError(t, err)

	// 投影错误沿用旧 mapper 语义：Ext = nil、不报错。
	loaded, err := repos.GetAccountWithTemplate(ctx, acc)
	require.NoError(t, err)
	require.Nil(t, loaded.Ext, "坏安装标识只让投影失败，不是读错误")

	fp, err := CandidateFingerprint(loaded)
	require.NoError(t, err)
	persist := rulepersist.NewRulePersistFunc(repos, latch.NewLatchStore(), nil, nil)
	require.NoError(t, persist(ctx, rule.PersistItem{
		Event: rule.Event{
			AccountID:                acc,
			TemplateID:               tpl.ID,
			ErrorMessage:             "upstream said no",
			CandidateFingerprint:     fp,
			ExpectedIdentityRevision: loaded.IdentityRevision,
		},
		Then: domain.RuleThen{FailAccount: true},
	}))
	failed, err := repos.GetAccount(ctx, acc)
	require.NoError(t, err)
	require.NotNil(t, failed.FailedAt, "rule_persist 判决必须照常落库（投影错误不得挡住它）")
}
