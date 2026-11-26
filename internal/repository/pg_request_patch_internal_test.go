// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// request_patch 的 repository 门禁（spec 2026-11-26-custom-request-patch §4.2/§9）：
//
//   - TestTemplateExtPostBaselineShape：结构约束——PostBaselineColumns 的每一列必须
//     真的在 ent schema 里，且不得被 PrimaryKey / Indexes / ForeignKeys 引用。只删
//     Columns 而索引/FK 仍引用它时，atlas 会生成引用缺失列的 DDL（gen 直接失败或
//     产出坏 0001），这条门在纯内存里当场红。
//   - TestAutomigrateShapeIncludesRequestPatch：runtime 侧——NewWithPG(migrate=true)
//     建出的库必须含 template_exts.request_patch（列排除只属 baseline 生成，不是
//     runtime schema 关注点），且 demo/seed 路径的 UpsertTemplateExt 能跑通。
//
// 为什么 UpsertTemplateExt 能证明列存在：ent client 的 SELECT 展开 schema 的全部列，
// 库缺 request_patch 时这里就是 42703——正是 RequiredVersion 3→4 要拦在启动期的
// 那个失败（decide → ModeReadonly）。本文件用内包 package repository：pgtest 只能
// 用 CloneEmpty（pgrepo → repository 会成 import cycle，口径同
// pg_billing_probe_internal_test.go）。

import (
	"context"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/schema"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	entmigrate "github.com/is7qin/c3api/internal/ent/migrate"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"
)

// TestTemplateExtPostBaselineShape 钉住 PostBaselineColumns 的结构约束（不连 PG）。
func TestTemplateExtPostBaselineShape(t *testing.T) {
	tables := make(map[string]*schema.Table, len(entmigrate.Tables))
	for _, table := range entmigrate.Tables {
		tables[table.Name] = table
	}
	declared := PostBaselineColumns()
	require.NotEmpty(t, declared, "空的 PostBaselineColumns 会让这条结构约束变成空话")

	for _, column := range declared {
		table := tables[column.Table]
		require.NotNil(t, table, "PostBaselineColumns 指向 ent 里不存在的表 %q", column.Table)
		require.True(t, tableHasColumn(table, column.Column),
			"PostBaselineColumns 声明的 %s.%s 不在 ent schema 里（拼错不会报错 = 0001 仍会带上该列）",
			column.Table, column.Column)

		// 列不得被 PK/索引/FK 引用：atlas 的列来自 Columns，索引/FK 来自各自的
		// Columns 字段——只删列会让索引 DDL 引用缺失列。
		for _, pk := range table.PrimaryKey {
			require.NotEqual(t, column.Column, pk.Name,
				"%s.%s 被 PrimaryKey 引用：不允许作为 post-baseline 列", column.Table, column.Column)
		}
		for _, index := range table.Indexes {
			for _, indexed := range index.Columns {
				require.NotEqual(t, column.Column, indexed.Name,
					"%s.%s 被索引 %q 引用：不允许作为 post-baseline 列", column.Table, column.Column, index.Name)
			}
		}
		for _, fk := range table.ForeignKeys {
			for _, ref := range fk.Columns {
				require.NotEqual(t, column.Column, ref.Name,
					"%s.%s 被外键 %q 引用：不允许作为 post-baseline 列", column.Table, column.Column, fk.Symbol)
			}
		}
	}
}

// tableHasColumn 判断 ent 表定义里是否有该列（列名不带引号）。
func tableHasColumn(table *schema.Table, name string) bool {
	for _, column := range table.Columns {
		if column.Name == name {
			return true
		}
	}
	return false
}

// TestAutomigrateShapeIncludesRequestPatch 用真实 PG 验证 runtime automigrate 的列面：
// NewWithPG(migrate=true) 建出的库含 request_patch，UpsertTemplateExt 往返可用。
func TestAutomigrateShapeIncludesRequestPatch(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.CloneEmpty(t)
	pool := pgtest.OpenPool(t, dsn)
	db := stdlib.OpenDBFromPool(pool)
	// LIFO：先关 *sql.DB 再关池（见 pgtest.OpenPool 的清理顺序注释）。
	t.Cleanup(func() { _ = db.Close() })

	repos, err := NewWithPG(ctx, entsql.OpenDB(dialect.Postgres, db), true, pool)
	require.NoError(t, err)

	var dataType, nullable string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT data_type, is_nullable FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'template_exts' AND column_name = 'request_patch'`).
		Scan(&dataType, &nullable))
	require.Equal(t, "jsonb", dataType,
		"runtime automigrate 必须建全列：request_patch 是 ent schema 列，只是不进冻结的 0001")
	require.Equal(t, "YES", nullable, "NULL = 未配置，列必须可空")

	// demo/seed 路径：UpsertTemplateExt 可跑通（ent SELECT 展开全部 schema 列，缺列即 42703）。
	tpl, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name:             "request-patch-automigrate",
		BaseURL:          "https://upstream.example.com/v1",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		ModelMapping:     domain.ModelMapping{},
	})
	require.NoError(t, err)

	allowed := func(*domain.TemplateExt) (bool, error) { return false, nil }
	saved, down, err := repos.TemplateExts.UpsertTemplateExt(ctx, &domain.TemplateExt{
		TemplateID:     tpl.ID,
		CredentialType: credential.TypeCustom,
		UpstreamPaths:  map[string]string{"openai_chat": "/v1/chat/completions"},
	}, allowed)
	require.NoError(t, err)
	require.False(t, down)
	require.Equal(t, tpl.ID, saved.TemplateID)

	got, err := repos.TemplateExts.GetTemplateExt(ctx, tpl.ID)
	require.NoError(t, err)
	require.Equal(t, credential.TypeCustom, got.CredentialType)
	require.Equal(t, map[string]string{"openai_chat": "/v1/chat/completions"}, got.UpstreamPaths)
}
