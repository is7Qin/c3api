// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// newSnapshotSvc 构造带 settings 快照的 Service（New 初始化 = 真实装配路径；
// 其余既有测试直构 Service，快照为零值不影响非设置路径）。
func newSnapshotSvc(fs *fakeStore) *Service {
	return New(fs, nil, NopInvalidator{}, nil, nil, nil, nil)
}

// TestSettingsSnapshotInitAndReload 快照：New 后可用（注册表默认值）→
// UpdateSetting 后立即生效（重载）→ 未知 key 仍 400（类型化校验不回归）。
func TestSettingsSnapshotInitAndReload(t *testing.T) {
	fs := newFakeStore()
	svc := newSnapshotSvc(fs)
	ctx := context.Background()

	// New 初始化快照：默认值可直接读（快照 map 由 GetAllSettings 构建）
	require.Equal(t, "true", svc.settingValue("signup_enabled"))
	require.Equal(t, "0", svc.settingValue("default_user_max_concurrency"))
	require.Equal(t, "30", svc.settingValue("default_user_temp_balance_ttl_days"))

	// UpdateSetting 成功后重载：快照立即反映 DB 覆盖值
	_, err := svc.UpdateSetting(ctx, "default_user_balance", "500")
	require.NoError(t, err)
	require.Equal(t, "500", svc.settingValue("default_user_balance"))
	// DB 直写绕过 UpdateSetting → 快照不更新（读路径只认快照）
	_, err = fs.SetSetting(ctx, "default_user_balance", domain.SettingTypeNumber, "999")
	require.NoError(t, err)
	require.Equal(t, "500", svc.settingValue("default_user_balance"), "快照不随 DB 旁路写入变化")

	// 未知 key → 400（注册表校验，New 快照不含未知项）
	_, err = svc.UpdateSetting(ctx, "unknown_key", "1")
	require.ErrorIs(t, err, ErrInvalidInput)

	// number 类型化校验：非数字 → 400
	_, err = svc.UpdateSetting(ctx, "default_user_balance", "abc")
	require.ErrorIs(t, err, ErrInvalidInput)
}

// TestRegisterUserAppliesDefaults 注册应用默认值：
// 默认 0/0 → 用户 0/0 且不插临时额度行；设置 100/500/1000/30 → 用户
// 100/500 + temp 行（amount=1000, expires≈now+30d, note="signup bonus"）；
// temp_balance=0 → 不插行；temp 插行失败 → 注册仍成功（评审 M-2）。
func TestRegisterUserAppliesDefaults(t *testing.T) {
	ctx := context.Background()

	// 默认 0/0：用户零值，不插行
	fs := newFakeStore()
	svc := newSnapshotSvc(fs)
	u, err := svc.RegisterUser(ctx, "d0@example.com", "s3cret-pass")
	require.NoError(t, err)
	require.Zero(t, u.MaxConcurrency, "默认 max_concurrency=0 不套值")
	require.Zero(t, u.Balance, "默认 balance=0 不套值")
	require.Empty(t, fs.tempBalances, "temp_balance=0 不插行")

	// 设置 100/500/1000/30 → 注册套默认 + 赠品行
	fs = newFakeStore()
	svc = newSnapshotSvc(fs)
	for _, kv := range []struct{ key, val string }{
		{"default_user_max_concurrency", "100"},
		{"default_user_balance", "500"},
		{"default_user_temp_balance", "1000"},
		{"default_user_temp_balance_ttl_days", "30"},
	} {
		_, err := svc.UpdateSetting(ctx, kv.key, kv.val)
		require.NoError(t, err)
	}
	u, err = svc.RegisterUser(ctx, "bonus@example.com", "s3cret-pass")
	require.NoError(t, err)
	require.Equal(t, 100, u.MaxConcurrency)
	require.Equal(t, int64(500), u.Balance)
	require.Len(t, fs.tempBalances, 1)
	tb := fs.tempBalances[0]
	require.Equal(t, u.ID, tb.UserID)
	require.Equal(t, int64(1000), tb.Amount)
	require.NotNil(t, tb.ExpiresAt)
	require.True(t, tb.ExpiresAt.After(time.Now().AddDate(0, 0, 29)), "expires ≈ now+30d（早于下限）")
	require.True(t, tb.ExpiresAt.Before(time.Now().AddDate(0, 0, 31)), "expires ≈ now+30d（晚于上限）")
	require.NotNil(t, tb.Note)
	require.Equal(t, "signup bonus", *tb.Note)

	// temp_balance=0 → 不插行（其余默认保留）
	fs = newFakeStore()
	svc = newSnapshotSvc(fs)
	_, err = svc.UpdateSetting(ctx, "default_user_max_concurrency", "8")
	require.NoError(t, err)
	u, err = svc.RegisterUser(ctx, "notemp@example.com", "s3cret-pass")
	require.NoError(t, err)
	require.Equal(t, 8, u.MaxConcurrency)
	require.Empty(t, fs.tempBalances, "temp_balance=0 不插行")

	// 评审 M-2：temp 插行失败 → 注册仍成功（防客户端重试 → 409 email 死锁）
	fs = newFakeStore()
	fs.tempBalanceErr = errTempBalanceInjected
	svc = newSnapshotSvc(fs)
	_, err = svc.UpdateSetting(ctx, "default_user_temp_balance", "1000")
	require.NoError(t, err)
	u, err = svc.RegisterUser(ctx, "m2@example.com", "s3cret-pass")
	require.NoError(t, err, "插行失败不阻断注册")
	require.True(t, u.ID > 0)
}

// errTempBalanceInjected 评审 M-2 注入错误（模拟赠品插行失败）。
var errTempBalanceInjected = errors.New("injected: temp balance insert failed")

// TestRegisterUserBootstrapFirstAdmin 首个注册用户 bootstrap（方案 A，spec
// 2026-08-15）：空表注册 → role=platform_admin；非空表 → 恒为普通 user；
// CountUsers 错误传播（注册失败返回该错误）。
func TestRegisterUserBootstrapFirstAdmin(t *testing.T) {
	ctx := context.Background()

	// 空表：首个注册 = platform_admin
	fs := newFakeStore()
	svc := newSnapshotSvc(fs)
	u, err := svc.RegisterUser(ctx, "first@example.com", "s3cret-pass")
	require.NoError(t, err)
	require.Equal(t, domain.RolePlatformAdmin, u.Role, "空表首个注册 = platform_admin")

	// 非空表：后续注册恒为普通 user（无需额外机制）
	u, err = svc.RegisterUser(ctx, "second@example.com", "s3cret-pass")
	require.NoError(t, err)
	require.Equal(t, domain.RoleUser, u.Role, "非空表注册恒为普通 user")

	// CountUsers 错误传播：注册失败返回该错误（不落到 CreateUser）
	fs = newFakeStore()
	fs.countUsersErr = errCountUsersInjected
	svc = newSnapshotSvc(fs)
	_, err = svc.RegisterUser(ctx, "err@example.com", "s3cret-pass")
	require.ErrorIs(t, err, errCountUsersInjected)
}

// errCountUsersInjected CountUsers 注入错误（bootstrap 错误传播测试）。
var errCountUsersInjected = errors.New("injected: count users failed")

// TestCreateUserAdminNoDefaults 管理面 CreateUser 不套默认（用户拍板）：
// 显式 0 → 用户 0（注册默认值 100/500 不影响管理面）。
func TestCreateUserAdminNoDefaults(t *testing.T) {
	fs := newFakeStore()
	svc := newSnapshotSvc(fs)
	ctx := context.Background()

	_, err := svc.UpdateSetting(ctx, "default_user_max_concurrency", "100")
	require.NoError(t, err)
	_, err = svc.UpdateSetting(ctx, "default_user_balance", "500")
	require.NoError(t, err)

	u, err := svc.CreateUser(ctx, "admin@example.com", "s3cret-pass",
		domain.RoleUser, domain.UserStatusActive, 0, 0)
	require.NoError(t, err)
	require.Zero(t, u.MaxConcurrency, "管理面显式 0 不套默认")
	require.Zero(t, u.Balance, "管理面显式 0 不套默认")
	require.Empty(t, fs.tempBalances, "管理面创建不送赠品")
}
