// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"
)

// ---------------------------------------------------------------------------
// 真实 PostgreSQL 测试基座（测试基座真实 PG 纪律：repository/service
// 测试一律真实 PG，不 pgxmock）。启动方式同 repository 包：
//   TEST_DATABASE_URL=postgres://postgres:c3api@localhost:15432/c3api_test \
//     scripts/test.sh -run TestRegisterUserBootstrapFirstAdminPG -v
//
// 隔离粒度 = database：每个测试从已迁移模板克隆一个私有库（pgtest.Clone）。
// 未设置 TEST_DATABASE_URL → t.Skip。
// ---------------------------------------------------------------------------

// TestRegisterUserBootstrapFirstAdminPG 首个注册用户 bootstrap 真实 PG 验证
// （spec 2026-08-15）：空表注册 → platform_admin；第二个注册 → 普通 user。
// RegisterUser 只触达 users/settings 常规表（ent migrate 建表；分区表由
// migrateHookExcludesPartitioned 排除且注册路径不涉及，无需 bootstrap）。
func TestRegisterUserBootstrapFirstAdminPG(t *testing.T) {
	dsn := pgtest.Clone(t)
	ctx := context.Background()
	pool := pgtest.OpenPool(t, dsn)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	repos, err := repository.NewWithPG(t.Context(), entsql.OpenDB(dialect.Postgres, db), false, pool)
	require.NoError(t, err)

	svc := New(Deps{Store: repos, Scheduler: nil, Invalidate: NopInvalidator{}, Publisher: nil, RuleReload: nil, Auth: nil, Log: nil, EmailCodeStore: testEmailCodes})

	first, err := svc.RegisterUser(ctx, "first@example.com", "s3cret-pass")
	require.NoError(t, err)
	require.Equal(t, domain.RolePlatformAdmin, first.Role, "空表首个注册 = platform_admin")

	second, err := svc.RegisterUser(ctx, "second@example.com", "s3cret-pass")
	require.NoError(t, err)
	require.Equal(t, domain.RoleUser, second.Role, "非空表注册恒为普通 user")
}
