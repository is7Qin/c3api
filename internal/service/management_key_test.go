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
)

// fakeMgmtRegistrar 记录管理 key 快照增量（Upsert/Delete）的测试假件。
type fakeMgmtRegistrar struct {
	mu     sync.Mutex
	upsRaw []string
	ups    []domain.ManagementKeyMeta
	del    []string
}

func (r *fakeMgmtRegistrar) UpsertManagementKey(raw string, meta domain.ManagementKeyMeta) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upsRaw = append(r.upsRaw, raw)
	r.ups = append(r.ups, meta)
}

func (r *fakeMgmtRegistrar) DeleteManagementKey(raw string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.del = append(r.del, raw)
}

// Upsert/Delete 补全 AuthRegistrar（客户端 key 面 no-op：本 fake 只记录管理 key
// 快照增量）。
func (r *fakeMgmtRegistrar) Upsert(string, domain.KeyMeta) {}
func (r *fakeMgmtRegistrar) Delete(string)                 {}

func (r *fakeMgmtRegistrar) lastMeta() *domain.ManagementKeyMeta {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ups) == 0 {
		return nil
	}
	m := r.ups[len(r.ups)-1]
	return &m
}

func newMgmtSvc(t *testing.T) (*Service, *fakeStore, *pubRecorder, *fakeMgmtRegistrar) {
	t.Helper()
	store := newFakeStore()
	pub := &pubRecorder{}
	reg := &fakeMgmtRegistrar{}
	svc := New(Deps{Store: store, Scheduler: nil, Invalidate: NopInvalidator{}, Publisher: pub, RuleReload: nil, Auth: reg, Log: nil, EmailCodeStore: testEmailCodes})
	return svc, store, pub, reg
}

// TestCreateManagementKey 创建管理 key：明文 mk- 前缀入库、本实例快照 Upsert
// （active）+ NOTIFY（ManagementKeys:true）。
func TestCreateManagementKey(t *testing.T) {
	svc, _, pub, reg := newMgmtSvc(t)
	k, err := svc.CreateManagementKey(context.Background(), 42, "ci-key")
	require.NoError(t, err)
	require.Equal(t, int64(42), k.UserID)
	require.Equal(t, "mk-", k.KeyRaw[:3])
	require.Equal(t, domain.ManagementKeyStatusActive, k.Status)

	meta := reg.lastMeta()
	require.NotNil(t, meta)
	require.Equal(t, int64(42), meta.UserID)
	require.Equal(t, domain.ManagementKeyStatusActive, meta.Status)
	require.Equal(t, pub.total(), 1)
	require.True(t, pub.last().ManagementKeys)
}

// TestListAndUpdateManagementKey 列表软删过滤；禁用即时快照 Upsert + NOTIFY。
func TestListAndUpdateManagementKey(t *testing.T) {
	svc, _, pub, reg := newMgmtSvc(t)
	ctx := context.Background()
	k, err := svc.CreateManagementKey(ctx, 42, "k1")
	require.NoError(t, err)

	rows, err := svc.ListManagementKeys(ctx, 42)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	// owner 隔离：他人列空。
	other, err := svc.ListManagementKeys(ctx, 99)
	require.NoError(t, err)
	require.Empty(t, other)

	disabled := domain.ManagementKeyStatusDisabled
	upd, err := svc.UpdateManagementKey(ctx, 42, k.ID, nil, &disabled)
	require.NoError(t, err)
	require.Equal(t, domain.ManagementKeyStatusDisabled, upd.Status)
	require.Equal(t, domain.ManagementKeyStatusDisabled, reg.lastMeta().Status, "禁用即时快照 Upsert")
	require.True(t, pub.last().ManagementKeys)

	// owner 隔离：越域更新 → ErrNotFound。
	_, err = svc.UpdateManagementKey(ctx, 99, k.ID, nil, &disabled)
	require.ErrorIs(t, err, ErrNotFound)
}

// TestDeleteManagementKey 删除：软删过滤 + 快照 Delete + NOTIFY；越域 404。
func TestDeleteManagementKey(t *testing.T) {
	svc, _, pub, reg := newMgmtSvc(t)
	ctx := context.Background()
	k, err := svc.CreateManagementKey(ctx, 42, "k1")
	require.NoError(t, err)

	// 越域删除 → ErrNotFound（不得移除快照）。
	require.ErrorIs(t, svc.DeleteManagementKey(ctx, 99, k.ID), ErrNotFound)

	require.NoError(t, svc.DeleteManagementKey(ctx, 42, k.ID))
	require.Contains(t, reg.del, k.KeyRaw)
	rows, err := svc.ListManagementKeys(ctx, 42)
	require.NoError(t, err)
	require.Empty(t, rows, "软删后列表过滤")
	require.True(t, pub.last().ManagementKeys)
}

// TestCreateManagementKeyInvalidInput 空 name / 非法 owner → 400（ErrInvalidInput）。
func TestCreateManagementKeyInvalidInput(t *testing.T) {
	svc, _, _, _ := newMgmtSvc(t)
	ctx := context.Background()
	_, err := svc.CreateManagementKey(ctx, 42, "")
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.CreateManagementKey(ctx, 0, "n")
	require.ErrorIs(t, err, ErrInvalidInput)
}
