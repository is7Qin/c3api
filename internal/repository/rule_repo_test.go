// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v5"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// when/then JSON 的期望字节（json.Marshal 对 map 按键排序输出，与 ent 落库一致）。
const (
	ruleWhenJSON = `{"count_429_ge":5,"http_status":429,"kind":"http_status","ratio_429_ge":0.5,"window_seconds":60}`
	ruleThenJSON = `{"cooldown":"30s","status":"429","weight":80}`
)

func ruleRow() *pgxmock.Rows {
	return pgxmock.NewRows([]string{"id", "name", "enabled", "priority", "when", "then",
		"updated_at", "deleted_at", "created_at"}).
		AddRow(int64(1), "r1", true, int(10), []byte(ruleWhenJSON), []byte(ruleThenJSON),
			time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Time{}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
}

func ruleCRUDWhen() domain.RuleWhen {
	kind, hs, ws, c429 := "http_status", 429, 60, 5
	ratio := 0.5
	return domain.RuleWhen{
		Kind: &kind, HTTPStatus: &hs, WindowSeconds: &ws, Count429GE: &c429, Ratio429GE: &ratio,
	}
}

func ruleCRUDThen() domain.RuleThen {
	st, cd, wt := domain.Status429, "30s", 80
	return domain.RuleThen{Status: &st, Cooldown: &cd, Weight: &wt}
}

func TestRuleCRUD(t *testing.T) {
	tr := newRepos(t)

	// Create -> INSERT ... RETURNING id（when/then 序列化为 JSON 字节）
	tr.pool.ExpectQuery(q(`INSERT INTO "rules"`)).
		WithArgs("r1", true, int(10), json.RawMessage(ruleWhenJSON), json.RawMessage(ruleThenJSON),
			pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(1)))

	// List 全部（nil 过滤）-> WHERE deleted_at IS NULL + ORDER BY priority ASC
	//（软删除：已删规则不加载——SQL 层面断言过滤谓词）
	tr.pool.ExpectQuery(`(?i)FROM "rules".*"rules"\."deleted_at" IS NULL.*ORDER BY "rules"\."priority" ASC`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "enabled", "priority", "when", "then",
			"updated_at", "deleted_at", "created_at"}).
			AddRow(int64(1), "r1", true, int(10), []byte(ruleWhenJSON), []byte(ruleThenJSON),
				time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Time{}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)).
			AddRow(int64(2), "r2", true, int(20), []byte(`{}`), []byte(`{}`),
				time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Time{}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))

	// List enabled=true -> WHERE deleted_at IS NULL AND "enabled"（PG bool 列直接作条件，无参数）
	tr.pool.ExpectQuery(`(?i)FROM "rules".*"rules"\."deleted_at" IS NULL.*"rules"\."enabled" ORDER BY "rules"\."priority" ASC`).
		WillReturnRows(ruleRow())

	// Update -> Tx: UPDATE（name/enabled/priority/when/then/updated_at）+ re-SELECT + Commit
	tr.pool.ExpectBegin()
	tr.pool.ExpectExec(q(`UPDATE "rules" SET`)).
		WithArgs("r1-renamed", false, int(20), json.RawMessage(ruleWhenJSON), json.RawMessage(ruleThenJSON),
			pgxmock.AnyArg(), int64(1)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	tr.pool.ExpectQuery(q(`FROM "rules" WHERE`)).
		WithArgs(int64(1)).
		WillReturnRows(ruleRow())
	tr.pool.ExpectCommit()

	// Delete（软删：UPDATE deleted_at/updated_at）
	tr.pool.ExpectExec(q(`UPDATE "rules" SET`)).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), int64(1)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	// Delete 不存在 -> ErrNotFound（含缺失 id）
	tr.pool.ExpectExec(q(`UPDATE "rules" SET`)).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), int64(999)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	// Count
	tr.pool.ExpectQuery(q(`SELECT COUNT("rules"."id") FROM "rules"`)).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(3)))

	r := domain.Rule{
		Name: "r1", Enabled: true, Priority: 10,
		When: ruleCRUDWhen(), Then: ruleCRUDThen(),
	}
	id, err := tr.repos.Rules.CreateRule(ctx(), r)
	require.NoError(t, err)
	require.Equal(t, int64(1), id)

	rows, err := tr.repos.Rules.ListRules(ctx(), nil)
	require.NoError(t, err)
	require.Len(t, rows, 2, "nil 过滤 = 全部")
	require.Equal(t, "r1", rows[0].Name, "mock 行序即返回序（SQL ORDER BY 已由正则断言）")
	require.Equal(t, 429, *rows[0].When.HTTPStatus, "when JSON roundtrip")
	require.Equal(t, 5, *rows[0].When.Count429GE)
	require.Equal(t, 0.5, *rows[0].When.Ratio429GE)
	require.Equal(t, domain.Status429, *rows[0].Then.Status, "then JSON roundtrip")
	require.Equal(t, "30s", *rows[0].Then.Cooldown)
	require.Equal(t, 80, *rows[0].Then.Weight)
	require.Nil(t, rows[1].When.Kind, "空 {} → 全 nil 指针")
	require.Nil(t, rows[1].Then.Status)

	enabled := true
	rows, err = tr.repos.Rules.ListRules(ctx(), &enabled)
	require.NoError(t, err)
	require.Len(t, rows, 1, "enabled 过滤生效")
	require.Equal(t, "r1", rows[0].Name)

	r.ID, r.Name, r.Enabled, r.Priority = id, "r1-renamed", false, 20
	require.NoError(t, tr.repos.Rules.UpdateRule(ctx(), r))

	require.NoError(t, tr.repos.Rules.DeleteRule(ctx(), id))
	err = tr.repos.Rules.DeleteRule(ctx(), 999)
	require.ErrorIs(t, err, repository.ErrNotFound)
	require.Contains(t, err.Error(), "id=999 missing")

	n, err := tr.repos.Rules.CountRules(ctx())
	require.NoError(t, err)
	require.Equal(t, int64(3), n)

	tr.expectDone(t)
}

// TestDeleteRulesBatch 批量软删除规则（ent.Tx 事务，全成或全败）。
func TestDeleteRulesBatch(t *testing.T) {
	// 成功：存在性通过 → 逐个软删 UPDATE → Commit
	t.Run("all exist commit", func(t *testing.T) {
		tr := newRepos(t)
		tr.pool.ExpectBegin()
		tr.pool.ExpectQuery(q(`SELECT "rules"."id" FROM "rules" WHERE`)).
			WithArgs(int64(1), int64(2)).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(1)).AddRow(int64(2)))
		tr.pool.ExpectExec(q(`UPDATE "rules" SET`)).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), int64(1)).
			WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		tr.pool.ExpectExec(q(`UPDATE "rules" SET`)).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), int64(2)).
			WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		tr.pool.ExpectCommit()

		require.NoError(t, tr.repos.Rules.DeleteRulesBatch(ctx(), []int64{1, 2}))
		tr.expectDone(t)
	})

	// 缺失 id → ErrNotFound（含缺失 id），且无任何 UPDATE 执行
	t.Run("missing id returns ErrNotFound without UPDATE", func(t *testing.T) {
		tr := newRepos(t)
		tr.pool.ExpectBegin()
		// 存在性检查：SELECT id WHERE id IN (1,2,3) 只返回 2 行（id=3 缺失）
		tr.pool.ExpectQuery(q(`SELECT "rules"."id" FROM "rules" WHERE`)).
			WithArgs(int64(1), int64(2), int64(3)).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(1)).AddRow(int64(2)))
		tr.pool.ExpectRollback()

		err := tr.repos.Rules.DeleteRulesBatch(ctx(), []int64{1, 2, 3})
		require.ErrorIs(t, err, repository.ErrNotFound)
		require.Contains(t, err.Error(), "id=3 missing")
		tr.expectDone(t)
	})

	// 中途 DB 错误 → 整体回滚（无 Commit）
	t.Run("midway db error rolls back without commit", func(t *testing.T) {
		tr := newRepos(t)
		tr.pool.ExpectBegin()
		tr.pool.ExpectQuery(q(`SELECT "rules"."id" FROM "rules" WHERE`)).
			WithArgs(int64(1), int64(2)).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(1)).AddRow(int64(2)))
		tr.pool.ExpectExec(q(`UPDATE "rules" SET`)).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), int64(1)).
			WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		tr.pool.ExpectExec(q(`UPDATE "rules" SET`)).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), int64(2)).
			WillReturnError(errors.New("midway db error"))
		tr.pool.ExpectRollback()

		err := tr.repos.Rules.DeleteRulesBatch(ctx(), []int64{1, 2})
		require.Error(t, err)
		require.NotErrorIs(t, err, repository.ErrNotFound, "DB 错误不应伪装成 not found")
		tr.expectDone(t)
	})
}

// TestRuleStoreContract RuleStore 接口与 Repos.Rules 装配断言。
func TestRuleStoreContract(t *testing.T) {
	tr := newRepos(t)
	tr.pool.ExpectQuery(q(`SELECT COUNT("rules"."id") FROM "rules"`)).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(0)))
	n, err := tr.repos.Rules.CountRules(ctx())
	require.NoError(t, err)
	require.Equal(t, int64(0), n)
	tr.expectDone(t)
}
