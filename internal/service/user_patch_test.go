// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// TestServiceUpdateUserPatchValidation patch 形态校验只作用于显式字段：
// 只改 balance 的 PUT（Role/Status 零值）不误拒；显式非法值照旧
// 400；未提供字段不触碰 DB。
func TestServiceUpdateUserPatchValidation(t *testing.T) {
	fs := newFakeStore()
	rec := &invRecorder{}
	svc := &Service{store: fs, inv: rec, log: nil}
	u, err := svc.CreateUser(t.Context(), "pv@example.com", "pw12345678",
		domain.RoleUser, domain.UserStatusActive, 8, 1000, 0)
	require.NoError(t, err)

	// 只改 balance（Role/Status 零值 → 旧全字段校验会误拒的面）
	bal, oldBal := int64(700), int64(1000)
	updated, err := svc.UpdateUser(t.Context(), &repository.UserPatch{
		ID: u.ID, Balance: &bal, OldBalance: &oldBal,
	}, 0)
	require.NoError(t, err, "只改 balance 的 PUT 不误拒")
	require.Equal(t, int64(700), updated.Balance)
	require.Equal(t, domain.RoleUser, updated.Role, "role 未被触碰")
	require.Equal(t, domain.UserStatusActive, updated.Status, "status 未被触碰")
	require.Equal(t, 8, updated.MaxConcurrency, "并发未被触碰")

	// 显式非法值照旧拒绝
	neg := int64(-1)
	_, err = svc.UpdateUser(t.Context(), &repository.UserPatch{
		ID: u.ID, Balance: &neg, OldBalance: &bal,
	}, 0)
	require.ErrorIs(t, err, ErrInvalidInput, "显式负 balance → 400")
	badRole := domain.Role("bogus")
	_, err = svc.UpdateUser(t.Context(), &repository.UserPatch{ID: u.ID, Role: &badRole}, 0)
	require.ErrorIs(t, err, ErrInvalidInput, "显式非法 role → 400")
	badStatus := domain.UserStatus("bogus")
	_, err = svc.UpdateUser(t.Context(), &repository.UserPatch{ID: u.ID, Status: &badStatus}, 0)
	require.ErrorIs(t, err, ErrInvalidInput, "显式非法 status → 400")
	negMC := -1
	_, err = svc.UpdateUser(t.Context(), &repository.UserPatch{ID: u.ID, MaxConcurrency: &negMC}, 0)
	require.ErrorIs(t, err, ErrInvalidInput, "显式负并发 → 400")

	// 非法值校验失败不 invalidate（创建 + 成功更新各触发一次，非法值零触发）
	require.Equal(t, 2, rec.countKind("users"), "创建 + 成功更新各一次；非法值不触发")
}

// TestServiceUpdateUserConflictRetry 条件写 0 行（期间有扣费）→ 重读当前值
// 刷新旧值条件重试（new 保持管理员显式意图）；成功后 invalidate。
func TestServiceUpdateUserConflictRetry(t *testing.T) {
	fs := newFakeStore()
	rec := &invRecorder{}
	svc := &Service{store: fs, inv: rec, log: nil}
	u, err := svc.CreateUser(t.Context(), "retry@example.com", "pw12345678",
		domain.RoleUser, domain.UserStatusActive, 8, 1000, 0)
	require.NoError(t, err)

	// 模拟 GET 后、条件写前的扣费：余额 1000 → 900
	require.NoError(t, fs.UpdateUserBalance(t.Context(), u.ID, -100))

	// 陈旧快照（1000）条件写 → 首试 ErrConflict → 重读（900）→ 重试成功
	bal, stale := int64(500), int64(1000)
	updated, err := svc.UpdateUser(t.Context(), &repository.UserPatch{
		ID: u.ID, Balance: &bal, OldBalance: &stale,
	}, 0)
	require.NoError(t, err, "冲突 → 重读 → 重试成功")
	require.Equal(t, int64(500), updated.Balance, "new 保持管理员显式意图")
	require.Equal(t, 2, rec.countKind("users"), "重试成功仅 invalidate 一次")
}

// TestServiceUpdateUserConflictRetryExhausted 条件写持续 0 行（期间持续有扣费）
// → 重读重试 ≤3 次 → 超限 ErrConflict（409），失败不 invalidate。
// 事务内 UpdateUser 恒 ErrConflict 由 fakeStore.updateUserTxErr 注入。
func TestServiceUpdateUserConflictRetryExhausted(t *testing.T) {
	fs := newFakeStore()
	fs.updateUserTxErr = fmt.Errorf("%w: id balance changed", repository.ErrConflict)
	rec := &invRecorder{}
	svc := &Service{store: fs, inv: rec, log: nil}
	u, err := svc.CreateUser(t.Context(), "exhaust@example.com", "pw12345678",
		domain.RoleUser, domain.UserStatusActive, 8, 1000, 0)
	require.NoError(t, err)

	bal, oldBal := int64(500), int64(1000)
	_, err = svc.UpdateUser(t.Context(), &repository.UserPatch{
		ID: u.ID, Balance: &bal, OldBalance: &oldBal,
	}, 0)
	require.ErrorIs(t, err, ErrConflict, "重试超限 → 409")
	require.Equal(t, 1, rec.countKind("users"), "失败不 invalidate")
}
