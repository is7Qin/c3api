// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"os"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// ---------------------------------------------------------------------------
// 真实 PostgreSQL 测试基座（评审 B1 + 测试基座真实 PG 纪律：repository/service
// 测试一律真实 PG，不 pgxmock）。启动方式同 repository 包：
//   TEST_DATABASE_URL=postgres://postgres:c3api@localhost:15432/c3api_test \
//     go test ./internal/service/ -run TestRegisterUserBootstrapFirstAdminPG -v
//
// 独立 schema（serviceBootstrapPGTestSchema，同 handler 包惯例）——repository
// 包测试独占 public schema，本文件不得 DROP public，否则与跨包并行测试互踩
// （全量 go test ./... 下各包并行进程）。未设置 TEST_DATABASE_URL → t.Skip。
// ---------------------------------------------------------------------------

// serviceBootstrapPGTestSchema 本文件 PG 测试专用 schema。
const serviceBootstrapPGTestSchema = "service_bootstrap_test"

// TestRegisterUserBootstrapFirstAdminPG 首个注册用户 bootstrap 真实 PG 验证
// （spec 2026-08-15）：空表注册 → platform_admin；第二个注册 → 普通 user。
// RegisterUser 只触达 users/settings 常规表（ent migrate 建表；分区表由
// migrateHookExcludesPartitioned 排除且注册路径不涉及，无需 bootstrap）。
func TestRegisterUserBootstrapFirstAdminPG(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + serviceBootstrapPGTestSchema
	} else {
		dsn += "?search_path=" + serviceBootstrapPGTestSchema
	}
	ctx := context.Background()
	pool, err := repository.OpenPG(ctx, dsn, 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	// 每测试重建独立 schema（AutoMigrate 幂等；DROP 保证表间无残留）
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+serviceBootstrapPGTestSchema+` CASCADE; CREATE SCHEMA `+serviceBootstrapPGTestSchema+`;`)
	require.NoError(t, err)
	repos, err := repository.NewWithPG(t.Context(), entsql.OpenDB(dialect.Postgres, db), true, pool)
	require.NoError(t, err)

	svc := New(repos, nil, NopInvalidator{}, nil, nil, nil, nil)

	first, err := svc.RegisterUser(ctx, "first@example.com", "s3cret-pass")
	require.NoError(t, err)
	require.Equal(t, domain.RolePlatformAdmin, first.Role, "空表首个注册 = platform_admin")

	second, err := svc.RegisterUser(ctx, "second@example.com", "s3cret-pass")
	require.NoError(t, err)
	require.Equal(t, domain.RoleUser, second.Role, "非空表注册恒为普通 user")
}
