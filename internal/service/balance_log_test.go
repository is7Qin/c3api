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
	"github.com/is7qin/c3api/internal/repository"
)

// errBalanceLogInjected 注入事务内 CreateBalanceLog 失败（原子回滚断言，A6）。
var errBalanceLogInjected = errors.New("injected: balance log insert failed")

// TestBalanceLogSignupDefault A2：注册 default_user_balance>0 → 1 行 signup_default
// （amount=初值, balance_after=初值, operator=0）；=0 → 0 行。
func TestBalanceLogSignupDefault(t *testing.T) {
	ctx := context.Background()

	fs := newFakeStore()
	svc := newSnapshotSvc(fs)
	_, err := svc.UpdateSetting(ctx, "default_user_balance", "500")
	require.NoError(t, err)
	u, err := svc.RegisterUser(ctx, "s@example.com", "s3cret-pass")
	require.NoError(t, err)
	require.Len(t, fs.balanceLogs, 1)
	l := fs.balanceLogs[0]
	require.Equal(t, u.ID, l.UserID)
	require.Equal(t, int64(500), l.Amount)
	require.Equal(t, int64(500), l.BalanceAfter)
	require.Equal(t, domain.BalanceSourceSignupDefault, l.Source)
	require.Zero(t, l.OperatorID)
	require.Nil(t, l.Note)

	// default_user_balance=0 → 不写行。
	fs2 := newFakeStore()
	svc2 := newSnapshotSvc(fs2)
	_, err = svc2.RegisterUser(ctx, "z@example.com", "s3cret-pass")
	require.NoError(t, err)
	require.Empty(t, fs2.balanceLogs, "balance=0 非变动 → 不写行")
}

// TestBalanceLogAdminCreate A3：管理面建用户 balance>0 → 1 行 admin_create
// （operator=admin id）；=0 → 0 行。
func TestBalanceLogAdminCreate(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	svc := newSnapshotSvc(fs)

	u, err := svc.CreateUser(ctx, "a@example.com", "pw12345678", domain.RoleUser, domain.UserStatusActive, 0, 5000, 77)
	require.NoError(t, err)
	require.Len(t, fs.balanceLogs, 1)
	l := fs.balanceLogs[0]
	require.Equal(t, u.ID, l.UserID)
	require.Equal(t, int64(5000), l.Amount)
	require.Equal(t, int64(5000), l.BalanceAfter)
	require.Equal(t, domain.BalanceSourceAdminCreate, l.Source)
	require.Equal(t, int64(77), l.OperatorID)

	// balance=0 → 不写行。
	_, err = svc.CreateUser(ctx, "b@example.com", "pw12345678", domain.RoleUser, domain.UserStatusActive, 0, 0, 77)
	require.NoError(t, err)
	require.Len(t, fs.balanceLogs, 1, "balance=0 → 不写行")
}

// TestBalanceLogAdminAdjust A4：PUT 显式改余额且变化 → 1 行 admin_adjust
// （amount=Δ, balance_after=新值, operator=admin id）；新值==旧值 / 不带 balance /
// 只改 role → 0 行。
func TestBalanceLogAdminAdjust(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	svc := newSnapshotSvc(fs)
	u := seedUser(t, fs, "adj@example.com", 1000, 0)

	// 显式改余额且变化 → 1 行。
	bal, oldBal := int64(700), int64(1000)
	updated, err := svc.UpdateUser(ctx, &repository.UserPatch{ID: u.ID, Balance: &bal, OldBalance: &oldBal}, 88)
	require.NoError(t, err)
	require.Equal(t, int64(700), updated.Balance)
	require.Len(t, fs.balanceLogs, 1)
	l := fs.balanceLogs[0]
	require.Equal(t, u.ID, l.UserID)
	require.Equal(t, int64(-300), l.Amount, "delta = 新值 - 旧值")
	require.Equal(t, int64(700), l.BalanceAfter)
	require.Equal(t, domain.BalanceSourceAdminAdjust, l.Source)
	require.Equal(t, int64(88), l.OperatorID)

	// 新值==旧值 → 0 行。
	same, old2 := int64(700), int64(700)
	_, err = svc.UpdateUser(ctx, &repository.UserPatch{ID: u.ID, Balance: &same, OldBalance: &old2}, 88)
	require.NoError(t, err)
	require.Len(t, fs.balanceLogs, 1, "无变动不记")

	// 不带 balance（只改 role）→ 0 行。
	role := domain.RolePlatformAdmin
	_, err = svc.UpdateUser(ctx, &repository.UserPatch{ID: u.ID, Role: &role}, 88)
	require.NoError(t, err)
	require.Len(t, fs.balanceLogs, 1, "只改 role 不记")
}

// TestBalanceLogRedemption A5：兑换 balance 型成功 → 1 行 redemption
// （amount=value, balance_after=兑换后, operator=0, note=码文本）；
// 兑换 temp_balance/concurrency 型 → 0 行；兑换失败 → 0 行。
func TestBalanceLogRedemption(t *testing.T) {
	ctx := context.Background()
	svc, fs, _ := newRedemptionSvc()
	u := seedUser(t, fs, "r@example.com", 100, 0)

	// balance 型。
	cb := genOne(t, svc, GenerateRequest{Type: domain.RedemptionTypeBalance, Value: 300}, 0)
	_, err := svc.Redeem(ctx, cb.Code, u.ID)
	require.NoError(t, err)
	require.Len(t, fs.balanceLogs, 1)
	l := fs.balanceLogs[0]
	require.Equal(t, u.ID, l.UserID)
	require.Equal(t, int64(300), l.Amount)
	require.Equal(t, int64(400), l.BalanceAfter, "100 + 300")
	require.Equal(t, domain.BalanceSourceRedemption, l.Source)
	require.Zero(t, l.OperatorID)
	require.NotNil(t, l.Note)
	require.Equal(t, cb.Code, *l.Note, "note = 兑换码文本")

	// temp_balance 型 → 0 行。
	re := time.Now().Add(7 * 24 * time.Hour)
	ct := genOne(t, svc, GenerateRequest{Type: domain.RedemptionTypeTempBalance, Value: 500, ResourceExpiresAt: &re}, 0)
	_, err = svc.Redeem(ctx, ct.Code, u.ID)
	require.NoError(t, err)
	require.Len(t, fs.balanceLogs, 1, "temp_balance 不入本表")

	// concurrency 型 → 0 行。
	cc := genOne(t, svc, GenerateRequest{Type: domain.RedemptionTypeConcurrency, Value: 5}, 0)
	_, err = svc.Redeem(ctx, cc.Code, u.ID)
	require.NoError(t, err)
	require.Len(t, fs.balanceLogs, 1, "concurrency 不入本表")

	// 兑换失败（重复同码 → 409）→ 0 行。
	_, err = svc.Redeem(ctx, cb.Code, u.ID)
	require.ErrorIs(t, err, ErrConflict)
	require.Len(t, fs.balanceLogs, 1, "失败回滚 → 0 新行")
}

// TestBalanceLogAtomicRollback A6：注入 CreateBalanceLog 失败 → 余额变更一并回滚
// （注册/建用户/改余额/兑换四路径各验一次）。
func TestBalanceLogAtomicRollback(t *testing.T) {
	ctx := context.Background()

	t.Run("注册路径", func(t *testing.T) {
		fs := newFakeStore()
		svc := newSnapshotSvc(fs)
		_, err := svc.UpdateSetting(ctx, "default_user_balance", "500")
		require.NoError(t, err)
		fs.createBalanceLogErr = errBalanceLogInjected
		_, err = svc.RegisterUser(ctx, "rollback@example.com", "s3cret-pass")
		require.ErrorIs(t, err, errBalanceLogInjected)
		require.Empty(t, fs.users, "用户创建随事务回滚")
		require.Empty(t, fs.balanceLogs)
	})

	t.Run("建用户路径", func(t *testing.T) {
		fs := newFakeStore()
		svc := newSnapshotSvc(fs)
		fs.createBalanceLogErr = errBalanceLogInjected
		_, err := svc.CreateUser(ctx, "rollback2@example.com", "pw12345678", domain.RoleUser, domain.UserStatusActive, 0, 500, 1)
		require.ErrorIs(t, err, errBalanceLogInjected)
		require.Empty(t, fs.users)
		require.Empty(t, fs.balanceLogs)
	})

	t.Run("改余额路径", func(t *testing.T) {
		fs := newFakeStore()
		svc := newSnapshotSvc(fs)
		u := seedUser(t, fs, "rollback3@example.com", 1000, 0)
		fs.createBalanceLogErr = errBalanceLogInjected
		bal, oldBal := int64(700), int64(1000)
		_, err := svc.UpdateUser(ctx, &repository.UserPatch{ID: u.ID, Balance: &bal, OldBalance: &oldBal}, 1)
		require.ErrorIs(t, err, errBalanceLogInjected)
		cur, err := fs.GetUser(ctx, u.ID)
		require.NoError(t, err)
		require.Equal(t, int64(1000), cur.Balance, "余额变更随事务回滚")
		require.Empty(t, fs.balanceLogs)
	})

	t.Run("兑换路径", func(t *testing.T) {
		svc, fs, _ := newRedemptionSvc()
		u := seedUser(t, fs, "rollback4@example.com", 100, 0)
		cb := genOne(t, svc, GenerateRequest{Type: domain.RedemptionTypeBalance, Value: 300}, 0)
		fs.createBalanceLogErr = errBalanceLogInjected
		_, err := svc.Redeem(ctx, cb.Code, u.ID)
		require.ErrorIs(t, err, errBalanceLogInjected)
		cur, err := fs.GetUser(ctx, u.ID)
		require.NoError(t, err)
		require.Equal(t, int64(100), cur.Balance, "余额未变（整体回滚）")
		require.Empty(t, fs.balanceLogs)
	})
}

// TestBalanceLogConcurrencyRetry A7：CAS 冲突重试成功后恰好 1 行且 delta = 真实跃迁
// （不重复记、不丢记）。
func TestBalanceLogConcurrencyRetry(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	svc := newSnapshotSvc(fs)
	u := seedUser(t, fs, "conc@example.com", 1000, 0)

	// 模拟管理员 GET 快照后、PUT 前的并发扣费：1000 → 900。
	require.NoError(t, fs.UpdateUserBalance(ctx, u.ID, -100))

	// 带陈旧旧值 1000 PUT 500 → 首次 CAS 冲突 → 重读 900 → 重试成功。
	bal, stale := int64(500), int64(1000)
	updated, err := svc.UpdateUser(ctx, &repository.UserPatch{ID: u.ID, Balance: &bal, OldBalance: &stale}, 9)
	require.NoError(t, err)
	require.Equal(t, int64(500), updated.Balance)

	require.Len(t, fs.balanceLogs, 1, "重试成功恰好 1 行（不重复记）")
	l := fs.balanceLogs[0]
	require.Equal(t, int64(500-900), l.Amount, "delta = 真实跃迁（900 → 500）")
	require.Equal(t, int64(500), l.BalanceAfter)
	require.Equal(t, int64(9), l.OperatorID)
}
