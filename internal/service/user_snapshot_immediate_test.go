// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// fakeAuthUpsert 记录 UpsertUser 调用的测试假件（本地立即可见路径钉）。
type fakeAuthUpsert struct {
	mu    sync.Mutex
	users map[int64]domain.UserSnapshot
	// 同时满足 KeyRegistrar：keys 相关 no-op 记录（CreateKey 路径依赖）。
	keys map[string]domain.KeyMeta
}

func newFakeAuthUpsert() *fakeAuthUpsert {
	return &fakeAuthUpsert{
		users: make(map[int64]domain.UserSnapshot),
		keys:  make(map[string]domain.KeyMeta),
	}
}

func (f *fakeAuthUpsert) Upsert(hash string, meta domain.KeyMeta) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[hash] = meta
}

func (f *fakeAuthUpsert) Delete(hash string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.keys, hash)
}

func (f *fakeAuthUpsert) UpsertUser(userID int64, snap domain.UserSnapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[userID] = snap
}

func (f *fakeAuthUpsert) snapshot(userID int64) (domain.UserSnapshot, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.users[userID]
	return s, ok
}

// TestCreateUser_ImmediateUserSnapshot 本地立即可见：admin CreateUser 后 RequireJWT
// 所需的 UserSnapshot 已插入本地 auth 快照，不等 200ms 去抖窗口。
// 回归：tools/e2e 场景2 的 401（create-key unauthorized）即因此窗口丢失。
func TestCreateUser_ImmediateUserSnapshot(t *testing.T) {
	fs := newFakeStore()
	auth := newFakeAuthUpsert()
	rec := &invRecorder{}
	svc := &Service{store: fs, inv: rec, keys: auth, log: nil}

	created, err := svc.CreateUser(context.Background(), "snap@example.com", "pw12345678",
		domain.RoleUser, domain.UserStatusActive, 8, 1000)
	require.NoError(t, err)

	snap, ok := auth.snapshot(created.ID)
	require.True(t, ok, "本地 auth 快照必须立即可见（不等去抖）")
	require.Equal(t, domain.UserStatusActive, snap.Status)
	require.Equal(t, domain.RoleUser, snap.Role)
	// 去抖仍需触发（远端实例靠 NOTIFY 全量覆盖）
	require.Equal(t, 1, rec.countKind("users"))
}

// TestRegisterUser_ImmediateUserSnapshot 公开注册同路径的立即可见（首个用户 bootstrap=admin）。
func TestRegisterUser_ImmediateUserSnapshot(t *testing.T) {
	fs := newFakeStore()
	auth := newFakeAuthUpsert()
	rec := &invRecorder{}
	svc := New(fs, nil, rec, nil, nil, auth, nil)
	// 注册前 settings 默认 signup_enabled=true（fakeStore 未设 → DefaultSetting 回退）

	u, err := svc.RegisterUser(context.Background(), "reg@example.com", "pw12345678")
	require.NoError(t, err)

	snap, ok := auth.snapshot(u.ID)
	require.True(t, ok, "RegisterUser 后本地快照立即可见")
	require.Equal(t, domain.UserStatusActive, snap.Status)
	require.Equal(t, u.Role, snap.Role)
	require.Equal(t, 1, rec.countKind("users"))
}

// TestUpdateUser_ImmediateUserSnapshot 状态/角色变更同样立即可见（禁用立即拒绝）。
func TestUpdateUser_ImmediateUserSnapshot(t *testing.T) {
	fs := newFakeStore()
	auth := newFakeAuthUpsert()
	rec := &invRecorder{}
	svc := &Service{store: fs, inv: rec, keys: auth, log: nil}

	u, err := svc.CreateUser(context.Background(), "up@example.com", "pw12345678",
		domain.RoleUser, domain.UserStatusActive, 8, 1000)
	require.NoError(t, err)

	// 变更 status 为 disabled（附带旧值条件）
	st := domain.UserStatusDisabled
	updated, err := svc.UpdateUser(context.Background(), &repository.UserPatch{
		ID: u.ID, Status: &st,
	})
	require.NoError(t, err)
	require.Equal(t, domain.UserStatusDisabled, updated.Status)

	snap, ok := auth.snapshot(u.ID)
	require.True(t, ok)
	require.Equal(t, domain.UserStatusDisabled, snap.Status, "禁用后本地快照立即为 disabled，RequireJWT fail-closed")
	require.Equal(t, 2, rec.countKind("users"))
}

// TestCreateUser_NoAuthRegistrar_NoPanic keys 未装配（nil）时仅去抖，不 panic。
func TestCreateUser_NoAuthRegistrar_NoPanic(t *testing.T) {
	fs := newFakeStore()
	svc := &Service{store: fs, inv: &invRecorder{}, keys: nil, log: nil}
	_, err := svc.CreateUser(context.Background(), "noop@example.com", "pw12345678",
		domain.RoleUser, domain.UserStatusActive, 8, 1000)
	require.NoError(t, err)
}
