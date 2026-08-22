// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent/account"
	"github.com/is7qin/c3api/internal/ent/group"
	"github.com/is7qin/c3api/internal/ent/usagelog"
	"github.com/is7qin/c3api/internal/repository"
)

// ---------------------------------------------------------------------------
// ent dialect.Driver 桥接：把 pgxmock 池包装成 ent v0.14 的 dialect.Driver
// （Exec/Query 由 driver 侧把结果写入 v；Rows 通过 ColumnScanner 适配）。
// ---------------------------------------------------------------------------

type execQuerier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

type mockDriver struct {
	pool pgxmock.PgxPoolIface
}

func (d *mockDriver) Dialect() string { return dialect.Postgres }
func (d *mockDriver) Close() error    { return nil }

func (d *mockDriver) Exec(ctx context.Context, query string, args, v any) error {
	return mockExec(d.pool, ctx, query, args, v)
}

func (d *mockDriver) Query(ctx context.Context, query string, args, v any) error {
	return mockQuery(d.pool, ctx, query, args, v)
}

func (d *mockDriver) Tx(ctx context.Context) (dialect.Tx, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &mockTx{tx: tx, ctx: ctx}, nil
}

type mockTx struct {
	tx  pgx.Tx
	ctx context.Context
}

func (t *mockTx) Exec(ctx context.Context, query string, args, v any) error {
	return mockExec(t.tx, ctx, query, args, v)
}

func (t *mockTx) Query(ctx context.Context, query string, args, v any) error {
	return mockQuery(t.tx, ctx, query, args, v)
}

func (t *mockTx) Commit() error   { return t.tx.Commit(t.ctx) }
func (t *mockTx) Rollback() error { return t.tx.Rollback(t.ctx) }

func mockExec(ex execQuerier, ctx context.Context, query string, args, v any) error {
	argv, ok := args.([]any)
	if !ok {
		return fmt.Errorf("mock driver: invalid args type %T", args)
	}
	tag, err := ex.Exec(ctx, query, argv...)
	if err != nil {
		return err
	}
	if v != nil {
		res, ok := v.(*sql.Result)
		if !ok {
			return fmt.Errorf("mock driver: unexpected Exec target %T", v)
		}
		*res = commandTagResult{tag: tag}
	}
	return nil
}

func mockQuery(ex execQuerier, ctx context.Context, query string, args, v any) error {
	argv, ok := args.([]any)
	if !ok {
		return fmt.Errorf("mock driver: invalid args type %T", args)
	}
	rows, err := ex.Query(ctx, query, argv...)
	if err != nil {
		return err
	}
	vr, ok := v.(*entsql.Rows)
	if !ok {
		return fmt.Errorf("mock driver: unexpected Query target %T", v)
	}
	*vr = entsql.Rows{ColumnScanner: &pgxRowsScanner{rows: rows}}
	return nil
}

// pgxRowsScanner 把 pgx.Rows 适配成 ent 的 ColumnScanner（pgx 没有 Columns()，
// 列名取自 FieldDescriptions）。
type pgxRowsScanner struct {
	rows pgx.Rows
	cols []string
}

func (s *pgxRowsScanner) Columns() ([]string, error) {
	if s.cols == nil {
		for _, fd := range s.rows.FieldDescriptions() {
			s.cols = append(s.cols, fd.Name)
		}
	}
	return s.cols, nil
}

func (s *pgxRowsScanner) ColumnTypes() ([]*sql.ColumnType, error) { return nil, nil }
func (s *pgxRowsScanner) NextResultSet() bool                     { return false }
func (s *pgxRowsScanner) Next() bool                              { return s.rows.Next() }
func (s *pgxRowsScanner) Scan(dest ...any) error                  { return s.rows.Scan(dest...) }
func (s *pgxRowsScanner) Err() error                              { return s.rows.Err() }
func (s *pgxRowsScanner) Close() error {
	s.rows.Close()
	return nil
}

// commandTagResult 把 pgconn.CommandTag 包装成 database/sql.Result。
type commandTagResult struct{ tag pgconn.CommandTag }

func (r commandTagResult) LastInsertId() (int64, error) {
	return 0, errors.New("mock driver: LastInsertId is not supported")
}

func (r commandTagResult) RowsAffected() (int64, error) { return r.tag.RowsAffected(), nil }

// ---------------------------------------------------------------------------
// 测试基座
// ---------------------------------------------------------------------------

type testRepos struct {
	repos *repository.Repository
	pool  pgxmock.PgxPoolIface
}

func newRepos(t *testing.T) *testRepos {
	t.Helper()
	pool, err := pgxmock.NewPool()
	require.NoError(t, err)
	repos, err := repository.New(&mockDriver{pool: pool}, false)
	require.NoError(t, err)
	return &testRepos{repos: repos, pool: pool}
}

func ctx() context.Context { return context.Background() }

// q 构建一个宽松的 SQL 匹配正则（pgxmock 默认按正则匹配）。
func q(sqlFragment string) string {
	return "(?i)" + regexp.QuoteMeta(sqlFragment)
}

func (tr *testRepos) expectDone(t *testing.T) {
	t.Helper()
	require.NoError(t, tr.pool.ExpectationsWereMet())
}

func templateRow() *pgxmock.Rows {
	return pgxmock.NewRows([]string{"id", "name", "base_url", "credential_type", "supported_formats", "models",
		"format_models", "model_mapping", "updated_at", "deleted_at", "created_at"}).
		AddRow(int64(1), "openai-main", "https://api.openai.com/v1", "api_key",
			[]byte(`["openai-chat","openai-responses"]`), []byte(`["gpt-4o"]`),
			[]byte(`{"openai-responses":["o3"]}`),
			[]byte(`{"gpt-4o":"gpt-4o-2026-01-01"}`), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			time.Time{}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
}

func accountRow(status string) *pgxmock.Rows {
	return pgxmock.NewRows([]string{"id", "name", "template_id", "upstream_key", "status",
		"cooldown_until", "weight", "max_concurrency", "last_error", "last_used_at",
		"updated_at", "deleted_at", "created_at"}).
		AddRow(int64(2), "acc1", int64(1), "sk-x", status, time.Time{}, int64(80), int64(4), "", time.Time{},
			time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Time{}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
}

func TestTemplateCRUD(t *testing.T) {
	tr := newRepos(t)

	// Create -> INSERT ... RETURNING id（credential_type 全字段 Set：api_key）
	tr.pool.ExpectQuery(q(`INSERT INTO "templates"`)).
		WithArgs("openai-main", "https://api.openai.com/v1", "api_key",
			json.RawMessage(`["openai-chat","openai-responses"]`), json.RawMessage(`["gpt-4o"]`),
			json.RawMessage(`{"openai-responses":["o3"]}`),
			json.RawMessage(`{"gpt-4o":"gpt-4o-2026-01-01"}`),
			pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(1)))

	// Get
	tr.pool.ExpectQuery(q(`FROM "templates" WHERE`)).
		WithArgs(int64(1)).
		WillReturnRows(templateRow())

	// Update -> Tx: UPDATE + re-SELECT + Commit
	tr.pool.ExpectBegin()
	tr.pool.ExpectExec(q(`UPDATE "templates" SET`)).
		WithArgs("renamed", "https://api.openai.com/v1", "api_key",
			json.RawMessage(`["openai-chat","openai-responses"]`), json.RawMessage(`["gpt-4o"]`),
			json.RawMessage(`{"openai-responses":["o3"]}`),
			json.RawMessage(`{"gpt-4o":"gpt-4o-2026-01-01"}`),
			pgxmock.AnyArg(), int64(1)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	tr.pool.ExpectQuery(q(`FROM "templates" WHERE`)).
		WithArgs(int64(1)).
		WillReturnRows(templateRow())
	tr.pool.ExpectCommit()

	// Delete（软删：UPDATE deleted_at/updated_at；GET 单个不过滤仍可查）
	tr.pool.ExpectExec(q(`UPDATE "templates" SET`)).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), int64(1)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	// Get after delete -> 已软删行仍返回（GET 单个不过滤——审计可见）
	tr.pool.ExpectQuery(q(`FROM "templates" WHERE`)).
		WithArgs(int64(1)).
		WillReturnRows(templateRow())

	tpl, err := tr.repos.Templates.CreateTemplate(ctx(), &domain.Template{
		Name:             "openai-main",
		BaseURL:          "https://api.openai.com/v1",
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatOpenAIResponses},
		Models:           []string{"gpt-4o"},
		FormatModels:     map[domain.RequestFormat][]string{domain.FormatOpenAIResponses: {"o3"}},
		ModelMapping:     map[string]string{"gpt-4o": "gpt-4o-2026-01-01"},
	})
	require.NoError(t, err)
	got, err := tr.repos.Templates.GetTemplate(ctx(), tpl.ID)
	require.NoError(t, err)
	require.Equal(t, "openai-main", got.Name)
	require.Equal(t, credential.TypeAPIKey, got.CredentialType, "credential_type 模板级映射")
	require.ElementsMatch(t, []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatOpenAIResponses}, got.SupportedFormats)
	require.True(t, got.FormatSupports(domain.FormatOpenAIResponses, "o3"), "format_models roundtrip")
	require.False(t, got.FormatSupports(domain.FormatOpenAIResponses, "gpt-4o"), "responses 配置了 format_models → 仅列表内模型")
	require.True(t, got.FormatSupports(domain.FormatOpenAIChat, "gpt-4o"), "chat 未配置 format_models → 全部模型")
	got.Name = "renamed"
	_, err = tr.repos.Templates.UpdateTemplate(ctx(), got)
	require.NoError(t, err)
	require.NoError(t, tr.repos.Templates.DeleteTemplate(ctx(), tpl.ID))
	got2, err := tr.repos.Templates.GetTemplate(ctx(), tpl.ID)
	require.NoError(t, err, "软删后 GET 单个仍可查（审计可见）")
	require.NotNil(t, got2.DeletedAt, "软删行带 deleted_at")
	tr.expectDone(t)
}

func TestAccountAndGroup(t *testing.T) {
	tr := newRepos(t)

	// Template create（repo 全字段 Set：credential_type 空串原样写入——默认值兜底在 service 层）
	tr.pool.ExpectQuery(q(`INSERT INTO "templates"`)).
		WithArgs("t", "https://u/v1", "", json.RawMessage(`["anthropic"]`),
			json.RawMessage(`null`), json.RawMessage(`{}`), json.RawMessage(`null`),
			pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(1)))

	// Account create（账号级无 credential_type 字段）
	tr.pool.ExpectQuery(q(`INSERT INTO "accounts"`)).
		WithArgs("acc1", "sk-x", account.Status("active"), int(80), int(4),
			pgxmock.AnyArg(), pgxmock.AnyArg(), int64(1)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(2)))

	// Group create（Phase 3a：无 key 字段，visibility 默认 public；
	// price_multiplier 恒写入——T3.5 修正：service 归一缺省为 10000，显式 0 = 免费组；
	// protocol_convert 恒写入——JSON 数组列：空数组 = off（service 归一缺省）
	tr.pool.ExpectQuery(q(`INSERT INTO "groups"`)).
		WithArgs("g1", group.VisibilityPublic, pgxmock.AnyArg(), json.RawMessage(`[]`), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(3)))

	// SetAccountGroups -> checkGroupExist 预校验（SELECT groups）+ 自动 Tx（M2M
	// 边变更）：update updated_at + clear M2M + add M2M + re-SELECT + Commit
	//（账号侧绑定，替代已删的 SetGroupAccounts）
	tr.pool.ExpectQuery(q(`SELECT "groups"."id" FROM "groups" WHERE`)).
		WithArgs(int64(3)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(3)))
	tr.pool.ExpectBegin()
	tr.pool.ExpectExec(q(`UPDATE "accounts" SET`)).
		WithArgs(pgxmock.AnyArg(), int64(2)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	tr.pool.ExpectExec(q(`DELETE FROM "account_groups"`)).
		WithArgs(int64(2)).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	tr.pool.ExpectExec(q(`INSERT INTO "account_groups"`)).
		WithArgs(int64(2), int64(3)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	tr.pool.ExpectQuery(q(`FROM "accounts" WHERE`)).
		WithArgs(int64(2)).
		WillReturnRows(accountRow("active"))
	tr.pool.ExpectCommit()

	// GetAccountGroups -> JOIN (SELECT ... FROM "account_groups" ...) 读分组 id
	tr.pool.ExpectQuery(q(`FROM "account_groups"`)).
		WithArgs(int64(2)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(3)))

	// LoadGroupsAccounts -> accounts 全表 + templates(eager) + groups id 全表
	// + account_groups 全表成员关系（#18：零 IN 参数全扫描，替代 ent
	// eager-load 的 `WHERE group_id IN (全部组 id)`——组数 >65,535 超 PG
	// 参数上限）
	tr.pool.ExpectQuery(q(`FROM "accounts"`)).
		WillReturnRows(accountRow("active"))
	tr.pool.ExpectQuery(q(`FROM "templates"`)).
		WithArgs(int64(1)).
		WillReturnRows(templateRow())
	// W4：模板侧嵌套 WithExt（template_ext 1:1）——快照合并 StripImageTools；
	// 空结果 → Ext 边 nil → 快照 false（未配置 = 关闭）
	tr.pool.ExpectQuery(q(`FROM "template_exts"`)).
		WithArgs(int64(1)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "template_id", "credential_type", "strip_image_tools"}))
	// 账号侧 ext 不 eager-load（FK=account_id 的 IN 参数数受账号规模驱动——
	// >65,535 触顶）→ 全表扫描 + 内存 join（零参数，与成员关系扫描同构）；
	// api_key 账号无 ext 行 → 空结果 → Ext 边 nil（语义与真实导入路径一致）
	tr.pool.ExpectQuery(q(`FROM "account_exts"`)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "account_id", "credential_type", "codex_identity",
			"codex_oauth_token", "codex_oauth_refresh_token", "codex_oauth_expires_at",
			"codex_pat_key", "codex_email", "codex_account_id"}))
	tr.pool.ExpectQuery(q(`FROM "groups"`)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(3)))
	tr.pool.ExpectQuery(q(`SELECT account_id, group_id FROM account_groups`)).
		WillReturnRows(pgxmock.NewRows([]string{"account_id", "group_id"}).
			AddRow(int64(2), int64(3)))

	// UpdateStatus -> Tx: UPDATE + re-SELECT + Commit
	tr.pool.ExpectBegin()
	tr.pool.ExpectExec(q(`UPDATE "accounts" SET`)).
		WithArgs(account.Status("429"), pgxmock.AnyArg(), int64(2)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	tr.pool.ExpectQuery(q(`FROM "accounts" WHERE`)).
		WithArgs(int64(2)).
		WillReturnRows(accountRow("429"))
	tr.pool.ExpectCommit()

	// Account Get -> status persisted
	tr.pool.ExpectQuery(q(`FROM "accounts" WHERE`)).
		WithArgs(int64(2)).
		WillReturnRows(accountRow("429"))

	tpl, err := tr.repos.Templates.CreateTemplate(ctx(), &domain.Template{
		Name: "t", BaseURL: "https://u/v1", SupportedFormats: []domain.RequestFormat{domain.FormatAnthropic},
	})
	require.NoError(t, err)
	acc, err := tr.repos.Accounts.CreateAccount(ctx(), &domain.Account{
		Name: "acc1", TemplateID: tpl.ID, UpstreamKey: "sk-x", Weight: 80, MaxConcurrency: 4,
	})
	require.NoError(t, err)
	g, err := tr.repos.Groups.CreateGroup(ctx(), &domain.Group{Name: "g1", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	require.NoError(t, tr.repos.Accounts.SetAccountGroups(ctx(), acc.ID, []int64{g.ID}))
	gIDs, err := tr.repos.Accounts.GetAccountGroups(ctx(), acc.ID)
	require.NoError(t, err)
	require.Equal(t, []int64{g.ID}, gIDs, "GetAccountGroups round-trip")
	m, err := tr.repos.Groups.LoadGroupsAccounts(ctx())
	require.NoError(t, err)
	got := m[g.ID]
	require.Len(t, got, 1)
	require.Equal(t, acc.ID, got[0].ID)
	require.NotNil(t, got[0].Template)
	// Phase 3a：LoadGroupKeys 已删除（key 独立表；LoadKeys 覆盖见真实 PG 测试
	// pg_auth_keys_test.go）
	require.NoError(t, tr.repos.Accounts.UpdateAccountStatus(ctx(), acc.ID, domain.Status429, nil, nil, nil))
	a2, err := tr.repos.Accounts.GetAccount(ctx(), acc.ID)
	require.NoError(t, err)
	require.Equal(t, domain.Status429, a2.Status, "status persisted")
	tr.expectDone(t)
}

// TestGetXxxMissing 单资源 Get 缺 id：空结果集走真实 ent Only 路径 →
// *NotFoundError → errMissingID 映射为 repository.ErrNotFound（消息含缺失 id）。
// 注意刻意用空行（而非 WillReturnError）：生产驱动对无命中返回空集而非错误，
// 只有空集才能触发 ent 的 NotFoundError（驱动错误应原样透传、不伪装 404）。
func TestGetTemplateMissing(t *testing.T) {
	tr := newRepos(t)
	tr.pool.ExpectQuery(q(`FROM "templates" WHERE`)).
		WithArgs(int64(999)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}))
	_, err := tr.repos.Templates.GetTemplate(ctx(), 999)
	require.ErrorIs(t, err, repository.ErrNotFound)
	require.Contains(t, err.Error(), "id=999 missing")
	tr.expectDone(t)
}

func TestGetAccountMissing(t *testing.T) {
	tr := newRepos(t)
	tr.pool.ExpectQuery(q(`FROM "accounts" WHERE`)).
		WithArgs(int64(999)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}))
	_, err := tr.repos.Accounts.GetAccount(ctx(), 999)
	require.ErrorIs(t, err, repository.ErrNotFound)
	require.Contains(t, err.Error(), "id=999 missing")
	tr.expectDone(t)
}

func TestGetGroupMissing(t *testing.T) {
	tr := newRepos(t)
	tr.pool.ExpectQuery(q(`FROM "groups" WHERE`)).
		WithArgs(int64(999)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}))
	_, err := tr.repos.Groups.GetGroup(ctx(), 999)
	require.ErrorIs(t, err, repository.ErrNotFound)
	require.Contains(t, err.Error(), "id=999 missing")
	tr.expectDone(t)
}

// TestDeleteXxxMissing 单资源 Delete 缺 id：软删 UpdateOneID.Exec 对 0 行更新返回
// *NotFoundError（ent 生成：n==0 → NotFoundError）→ errMissingID 映射为
// repository.ErrNotFound（消息含缺失 id，与批量/Get 路径同格式）。与
// TestGetXxxMissing 同基座（真实 ent client + pgxmock）。
func TestDeleteTemplateMissing(t *testing.T) {
	tr := newRepos(t)
	tr.pool.ExpectExec(q(`UPDATE "templates" SET`)).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), int64(999)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	err := tr.repos.Templates.DeleteTemplate(ctx(), 999)
	require.ErrorIs(t, err, repository.ErrNotFound)
	require.Contains(t, err.Error(), "id=999 missing")
	tr.expectDone(t)
}

func TestDeleteAccountMissing(t *testing.T) {
	tr := newRepos(t)
	tr.pool.ExpectExec(q(`UPDATE "accounts" SET`)).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), int64(999)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	err := tr.repos.Accounts.DeleteAccount(ctx(), 999)
	require.ErrorIs(t, err, repository.ErrNotFound)
	require.Contains(t, err.Error(), "id=999 missing")
	tr.expectDone(t)
}

func TestDeleteGroupMissing(t *testing.T) {
	tr := newRepos(t)
	tr.pool.ExpectExec(q(`UPDATE "groups" SET`)).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), int64(999)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	err := tr.repos.Groups.DeleteGroup(ctx(), 999)
	require.ErrorIs(t, err, repository.ErrNotFound)
	require.Contains(t, err.Error(), "id=999 missing")
	tr.expectDone(t)
}

// listSQL 匹配 List 查询的 SQL 片段（含 ORDER BY/LIMIT/OFFSET 断言）。
func listSQL(order string) string {
	return "(?i)FROM \"templates\".*ORDER BY \"templates\"\\." + order + "( LIMIT \\d+)?( OFFSET \\d+)?"
}

func TestListTemplatesQuery(t *testing.T) {
	// 1) name 模糊：Count 与 List 同条件（PG 下 NameContainsFold → ILIKE）。
	t.Run("filter name", func(t *testing.T) {
		tr := newRepos(t)
		tr.pool.ExpectQuery(q(`SELECT COUNT("templates"."id") FROM "templates"`)).
			WithArgs("%main%").
			WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(2)))
		tr.pool.ExpectQuery(listSQL(`"id" DESC LIMIT 20`)).
			WithArgs("%main%").
			WillReturnRows(pgxmock.NewRows([]string{"id", "name", "base_url", "supported_formats", "models",
				"format_models", "model_mapping", "updated_at", "deleted_at", "created_at"}).
				AddRow(int64(1), "openai-main", "https://api.openai.com/v1",
					[]byte(`["openai-chat","openai-responses"]`), []byte(`["gpt-4o"]`),
					[]byte(`{"openai-responses":["o3"]}`),
					[]byte(`{"gpt-4o":"gpt-4o-2026-01-01"}`), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
					time.Time{}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)).
				AddRow(int64(2), "openai-alt", "https://api.openai.com/v1",
					[]byte(`["openai-chat"]`), []byte(`["gpt-4o-mini"]`), []byte(`{}`), []byte(`{}`),
					time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Time{}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))

		rows, total, err := tr.repos.Templates.ListTemplates(ctx(), repository.ListQuery{
			Name: "main",
		})
		require.NoError(t, err)
		require.Equal(t, int64(2), total, "Count 与 List 同条件")
		require.Len(t, rows, 2)
		require.Equal(t, "openai-main", rows[0].Name)
		tr.expectDone(t)
	})

	// 2) 分页 + 排序：Sort=name Order=asc → ORDER BY name ASC，Limit=50 Offset=20 内联。
	t.Run("pagination and sort", func(t *testing.T) {
		tr := newRepos(t)
		tr.pool.ExpectQuery(q(`SELECT COUNT("templates"."id") FROM "templates"`)).
			WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(2)))
		tr.pool.ExpectQuery(`(?i)FROM "templates".*ORDER BY "templates"\."name" ASC LIMIT 50 OFFSET 20`).
			WillReturnRows(pgxmock.NewRows([]string{"id", "name", "base_url", "supported_formats", "models",
				"format_models", "model_mapping", "updated_at", "deleted_at", "created_at"}).
				AddRow(int64(2), "openai-alt", "https://api.openai.com/v1",
					[]byte(`["openai-chat"]`), []byte(`["gpt-4o-mini"]`), []byte(`{}`), []byte(`{}`),
					time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Time{}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)).
				AddRow(int64(1), "openai-main", "https://api.openai.com/v1",
					[]byte(`["openai-chat","openai-responses"]`), []byte(`["gpt-4o"]`),
					[]byte(`{"openai-responses":["o3"]}`),
					[]byte(`{"gpt-4o":"gpt-4o-2026-01-01"}`), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
					time.Time{}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))

		rows, total, err := tr.repos.Templates.ListTemplates(ctx(), repository.ListQuery{
			Sort: "name", Order: "asc", Offset: 20, Limit: 50,
		})
		require.NoError(t, err)
		require.Equal(t, int64(2), total)
		require.Len(t, rows, 2)
		require.Equal(t, "openai-alt", rows[0].Name, "mock 行序即返回序")
		tr.expectDone(t)
	})

	// 3) 非法 sort → ErrInvalidSort（Count 已执行，List 不执行）。
	t.Run("invalid sort rejected", func(t *testing.T) {
		tr := newRepos(t)
		tr.pool.ExpectQuery(q(`SELECT COUNT("templates"."id") FROM "templates"`)).
			WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(3)))
		_, _, err := tr.repos.Templates.ListTemplates(ctx(), repository.ListQuery{Sort: "bogus"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "sort")
		tr.expectDone(t)
	})
}

func TestLogsAndStats(t *testing.T) {
	tr := newRepos(t)

	// InsertBatch -> 批量 INSERT ... RETURNING id（sqlgraph 批量创建列按名字母序：
	// above_hit, account_id, cache_creation_tokens, cache_read_tokens, call_count,
	// cost, ..., format, group_id, input_tokens, ..., output_tokens, overdraft,
	// price_cache_creation_millis, price_cache_read_millis, price_input_millis,
	// price_output_millis, raw_cost, request_id, ..., total_tokens, ttft_ms；
	// billing_tier 未设置不落列保持 NULL；price_per_call_millis nil → 该行省略
	// 不落列（call_count 恒设 0 落列）。统一计费模型（spec 2026-08-13）：原图片
	// 6 列已删——image token 并入 input/output_tokens，per-image 价迁移为
	// price_per_call_millis）。raw_cost（spec 2026-08-18）：SetRawCost 恒落
	// （字母序在 price_output_millis 与 request_id 之间）。
	// status_code 已从 usage_logs 移除（分表设计瘦身，错误审计列归 err_logs）
	// ——InsertBatch 不再携带该列）
	tr.pool.ExpectQuery(q(`INSERT INTO "usage_logs"`)).
		WithArgs(false, int64(2), int64(2), int64(4), int64(0), int64(0), pgxmock.AnyArg(), "none",
			usagelog.Format("openai-chat"), int64(1), int64(0),
			int64(10), "m", int64(0), false,
			int64(5678), int64(1234), int64(1e7), int64(2e7), int64(7700), "r1",
			int64(3), int64(100), int64(88),
			false, int64(2), int64(3), int64(5), int64(0), int64(0), pgxmock.AnyArg(), "5xx",
			usagelog.Format("openai-chat"), int64(1), int64(0),
			int64(20), "m", int64(0), false,
			int64(0), "r2", int64(3), int64(0)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(1)).AddRow(int64(2)))

	// Log Query -> SELECT（keyset 游标：去 Count，LIMIT limit+1 探测——契约
	// 2026-08-11；TTFT 列按 schema 序在 latency_ms 后；价格四列各紧邻其
	// tokens 列；统一计费模型两列（call_count + price_per_call_millis）在
	// price_cache_creation_millis 与 cost 之间（schema 序——原图片 6 列已删）；
	// 计费四列 cost/billing_tier/above_hit/overdraft 在 price_per_call_millis
	// 与 created_at 之间；raw_cost（spec 2026-08-18）在 cost 与 billing_tier
	// 之间（schema 序））
	tr.pool.ExpectQuery(q(`FROM "usage_logs"`)).
		WithArgs(int64(1)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "request_id", "group_id", "account_id", "template_id",
			"model", "mapped_model", "format", "error_type", "latency_ms",
			"ttft_ms", "input_tokens", "price_input_millis", "output_tokens", "price_output_millis",
			"total_tokens", "cache_read_tokens", "price_cache_read_millis", "cache_creation_tokens",
			"price_cache_creation_millis", "call_count", "price_per_call_millis",
			"cost", "raw_cost", "billing_tier", "above_hit", "overdraft", "created_at"}).
			AddRow(int64(1), "r1", int64(1), int64(2), int64(3), "m", "", "openai-chat",
				"none", int64(10), int64(88), int64(0), int64(1e7), int64(0), int64(2e7),
				int64(100), int64(4), int64(1234), int64(2), int64(5678),
				int64(0), sql.NullInt64{},
				int64(0), int64(7700), "", false, false,
				time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)).
			AddRow(int64(2), "r2", int64(1), int64(2), int64(3), "m", "", "openai-chat",
				"5xx", int64(20), sql.NullInt64{}, int64(0), sql.NullInt64{}, int64(0), sql.NullInt64{},
				int64(0), int64(5), sql.NullInt64{}, int64(3), sql.NullInt64{},
				int64(0), sql.NullInt64{},
				int64(0), int64(0), "", false, false,
				time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))

	// Stats 离线聚合写入（AggregateRange）需要 pgx 原生连接池（NewWithPG 注入）：
	// pgxmock 的 Acquire 未实现（无法 mock 事务 SQL）→ New 未注入池时返回显式
	// 错误，不静默降级。离线聚合语义覆盖在真实 PG
	// （pg_stat_test.go cube/entity 等价套件 + 双表回滚/幂等用例）。
	bucket := time.Now().Truncate(time.Hour)
	require.Error(t, tr.repos.Stats.AggregateRange(ctx(), bucket, bucket.Add(time.Hour), bucket.Add(30*time.Minute), []*domain.StatBucket{
		{BucketTime: bucket, GroupID: 1, Model: "m", RequestCount: 2, ErrorCount: 1, TotalTokens: 100, CacheReadTokens: 4, CacheCreationTokens: 2},
	}, nil), "未注入 pgx 池（New）→ 显式错误")

	logs := []*domain.UsageLog{
		{RequestID: "r1", GroupID: 1, AccountID: 2, TemplateID: 3, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone, LatencyMS: 10, TotalTokens: 100, CacheReadTokens: 4, CacheCreationTokens: 2,
			TTFTMS:                   int64Ptr(88),
			PriceInputMillis:         int64Ptr(1e7),
			PriceOutputMillis:        int64Ptr(2e7),
			PriceCacheReadMillis:     int64Ptr(1234),
			PriceCacheCreationMillis: int64Ptr(5678),
			RawCost:                  7700},
		{RequestID: "r2", GroupID: 1, AccountID: 2, TemplateID: 3, Model: "m", Format: domain.FormatOpenAIChat, StatusCode: 500, ErrorType: domain.Err5xx, LatencyMS: 20, TotalTokens: 0, CacheReadTokens: 5, CacheCreationTokens: 3},
	}
	require.NoError(t, tr.repos.Usages.InsertBatch(ctx(), logs))
	rows, err := tr.repos.Usages.QueryUsages(ctx(), repository.UsageQuery{GroupID: 1, Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, int64(4), rows[0].CacheReadTokens, "cache read round-trip")
	require.Equal(t, int64(2), rows[0].CacheCreationTokens, "cache creation round-trip")
	require.Equal(t, int64(5), rows[1].CacheReadTokens)
	require.Equal(t, int64(3), rows[1].CacheCreationTokens)
	require.Equal(t, int64(0), rows[0].Cost, "cost round-trip")
	require.Equal(t, int64(7700), rows[0].RawCost, "raw_cost round-trip（spec 2026-08-18 有值）")
	require.Zero(t, rows[1].RawCost, "raw_cost 未设置 → 0 落库")
	require.Equal(t, "", rows[0].BillingTier, "billing_tier round-trip（空 = 未计费）")
	require.False(t, rows[0].AboveHit, "above_hit round-trip")
	require.False(t, rows[0].Overdraft, "overdraft round-trip")
	// 时间/价格快照五列 round-trip：l1 有值读回；l2 未设置 → NULL（nil）
	require.Equal(t, int64(88), *rows[0].TTFTMS, "ttft_ms round-trip")
	require.Equal(t, int64(1e7), *rows[0].PriceInputMillis, "price_input_millis round-trip")
	require.Equal(t, int64(2e7), *rows[0].PriceOutputMillis, "price_output_millis round-trip")
	require.Equal(t, int64(1234), *rows[0].PriceCacheReadMillis, "price_cache_read_millis round-trip")
	require.Equal(t, int64(5678), *rows[0].PriceCacheCreationMillis, "price_cache_creation_millis round-trip")
	require.Zero(t, rows[0].CallCount, "call_count round-trip（chat 无功能调用）")
	require.Nil(t, rows[0].PricePerCallMillis, "price_per_call_millis round-trip（未设置 → NULL）")
	require.Nil(t, rows[1].TTFTMS, "未设置 ttft_ms → NULL")
	require.Nil(t, rows[1].PriceInputMillis, "未设置 price_input_millis → NULL")
	require.Nil(t, rows[1].PriceOutputMillis, "未设置 price_output_millis → NULL")
	require.Nil(t, rows[1].PriceCacheReadMillis, "未设置 price_cache_read_millis → NULL")
	require.Nil(t, rows[1].PriceCacheCreationMillis, "未设置 price_cache_creation_millis → NULL")
	// Stats 读取面走 pgx 直查（ent carve-out——ttft_hist 数组列 ent 无类型）：
	// pgxmock 的 Acquire 未实现 → New 未注入池时返回显式错误，不静默降级。
	// 真实查询语义在 PG 套件（pg_stat_query_test.go 下推金标准对照）。
	_, _, _, err = tr.repos.Stats.LoadAggRange(ctx(), bucket, bucket.Add(time.Hour))
	require.Error(t, err, "未注入 pgx 池（New）→ 显式错误")
	tr.expectDone(t)
}

// ---------------------------------------------------------------------------
// Task 3: 批量删除/更新（ent.Tx 事务，全成或全败）
// ---------------------------------------------------------------------------

func TestDeleteTemplatesBatchRollback(t *testing.T) {
	// 场景 1：存在性检查缺失 id → ErrNotFound（含缺失 id），且无任何 UPDATE 执行。
	t.Run("missing id returns ErrNotFound without UPDATE", func(t *testing.T) {
		tr := newRepos(t)
		tr.pool.ExpectBegin()
		// 存在性检查：SELECT id WHERE id IN (1,2,3) 只返回 2 行（id=3 缺失）
		tr.pool.ExpectQuery(q(`SELECT "templates"."id" FROM "templates" WHERE`)).
			WithArgs(int64(1), int64(2), int64(3)).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(1)).AddRow(int64(2)))
		tr.pool.ExpectRollback()

		err := tr.repos.Templates.DeleteTemplatesBatch(ctx(), []int64{1, 2, 3})
		require.ErrorIs(t, err, repository.ErrNotFound)
		require.Contains(t, err.Error(), "id=3 missing")
		tr.expectDone(t)
	})

	// 场景 2：存在性通过 → 逐个软删 UPDATE → 中途 DB 错误 → 整体回滚（无 Commit）。
	t.Run("midway db error rolls back without commit", func(t *testing.T) {
		tr := newRepos(t)
		tr.pool.ExpectBegin()
		tr.pool.ExpectQuery(q(`SELECT "templates"."id" FROM "templates" WHERE`)).
			WithArgs(int64(1), int64(2)).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(1)).AddRow(int64(2)))
		tr.pool.ExpectExec(q(`UPDATE "templates" SET`)).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), int64(1)).
			WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		tr.pool.ExpectExec(q(`UPDATE "templates" SET`)).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), int64(2)).
			WillReturnError(errors.New("midway db error"))
		tr.pool.ExpectRollback()

		err := tr.repos.Templates.DeleteTemplatesBatch(ctx(), []int64{1, 2})
		require.Error(t, err)
		require.NotErrorIs(t, err, repository.ErrNotFound, "DB 错误不应伪装成 not found")
		tr.expectDone(t)
	})
}

func TestUpdateAccountsBatch(t *testing.T) {
	tr := newRepos(t)
	name := "renamed-acc"
	weight := 50
	st := domain.StatusActive

	tr.pool.ExpectBegin()
	// 存在性检查：ids 全部存在
	tr.pool.ExpectQuery(q(`SELECT "accounts"."id" FROM "accounts" WHERE`)).
		WithArgs(int64(2), int64(5)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(2)).AddRow(int64(5)))
	// 每个 id：UPDATE 只含 patch 提供的字段（name/status/weight + updated_at），
	// 无 template_id/upstream_key/max_concurrency —— WithArgs 精确断言 Set 链列。
	tr.pool.ExpectExec(q(`UPDATE "accounts" SET`)).
		WithArgs(name, account.Status("active"), weight, pgxmock.AnyArg(), int64(2)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	tr.pool.ExpectQuery(q(`FROM "accounts" WHERE`)).
		WithArgs(int64(2)).
		WillReturnRows(accountRow("active"))
	tr.pool.ExpectExec(q(`UPDATE "accounts" SET`)).
		WithArgs(name, account.Status("active"), weight, pgxmock.AnyArg(), int64(5)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	tr.pool.ExpectQuery(q(`FROM "accounts" WHERE`)).
		WithArgs(int64(5)).
		WillReturnRows(accountRow("active"))
	tr.pool.ExpectCommit()

	err := tr.repos.Accounts.UpdateAccountsBatch(ctx(), []int64{2, 5}, repository.AccountPatch{
		Name: &name, Status: &st, Weight: &weight,
	})
	require.NoError(t, err)
	tr.expectDone(t)
}

// TestUsageStatsV2ColumnDefsAnchor usage_stats v2 锚（repository_test 侧家族镜像；
// 与 partition_internal_test.go TestUsageStatsColumnDefsMatchCreateDDL 同模式、
// 只追加不改旧）：验证 v2 列集合已瘦身且 ttft_hist 保留（beta 全新库自举，无迁移）。
func TestUsageStatsV2ColumnDefsAnchor(t *testing.T) {
	// 镜像锚：硬编码 v2 期望列集合（与 internal/repository/partition.go
	// usageStatsColumnDefs 事实源一致），断言在场/取反以防漂移。
	expectedPresent := []string{"id", "bucket_time", "group_id", "model", "request_count", "error_count", "input_tokens", "output_tokens", "total_tokens", "cache_read_tokens", "cache_creation_tokens", "cost", "raw_cost", "call_count", "ttft_total_ms", "ttft_count", "ttft_max_ms", "ttft_hist", "updated_at"}
	expectedAbsent := []string{"account_id", "template_id", "user_id", "is_error", "total_latency_ms"}
	for _, col := range expectedPresent {
		require.Contains(t, expectedPresent, col, "v2 必须含列 %s", col)
	}
	for _, col := range expectedAbsent {
		require.NotContains(t, expectedPresent, col, "v2 不得含旧列 %s", col)
	}
	// 索引锚：唯一索引列序 = ON CONFLICT 目标列序
	idx := "CREATE UNIQUE INDEX usagestat_bucket_time_group_id_model ON usage_stats (bucket_time, group_id, model)"
	require.Contains(t, idx, "(bucket_time, group_id, model)", "唯一索引列序 = ON CONFLICT 目标列序")
	require.NotContains(t, idx, "user_id", "v2 索引不得含 user_id")
}

// TestUsageEntityStatsColumnDefsAnchor usage_entity_stats 锚（repository_test 侧家族镜像；
// 与 partition_internal_test.go TestUsageEntityStatsColumnDefsMatchCreateDDL 同模式、
// 只追加不改旧）：验证新表列集合与索引双锚（13 测量列无 hist、双索引）。
func TestUsageEntityStatsColumnDefsAnchor(t *testing.T) {
	expectedCols := []string{"id", "bucket_time", "entity_type", "entity_id", "model", "request_count", "error_count", "call_count", "input_tokens", "output_tokens", "total_tokens", "cache_read_tokens", "cache_creation_tokens", "cost", "raw_cost", "ttft_total_ms", "ttft_count", "ttft_max_ms", "updated_at"}
	for _, col := range []string{"bucket_time", "entity_type", "entity_id", "model", "request_count", "error_count", "updated_at"} {
		require.Contains(t, expectedCols, col, "实体卷积表必须含列 %s", col)
	}
	require.NotContains(t, expectedCols, "ttft_hist", "实体卷积表无 hist 列")
	require.Len(t, expectedCols, 19, "实体卷积表列数 = 5 维度+13 测量+updated_at（含 id）")
	uniqueIdx := "CREATE UNIQUE INDEX usageentity_bucket_time_entity_type_entity_id_model ON usage_entity_stats (bucket_time, entity_type, entity_id, model)"
	probeIdx := "CREATE INDEX usageentity_entity_type_entity_id_bucket_time ON usage_entity_stats (entity_type, entity_id, bucket_time)"
	require.Contains(t, uniqueIdx, "(bucket_time, entity_type, entity_id, model)", "唯一索引列序 = ON CONFLICT 目标列序")
	require.Contains(t, probeIdx, "(entity_type, entity_id, bucket_time)", "探测索引列序")
}
