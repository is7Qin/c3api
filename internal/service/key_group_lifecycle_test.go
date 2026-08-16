// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// TestSoftDeletedKeyNotResurrectable F2 红绿：软删 key 全路 404（fake 已建模
// 软删——GetKey 仍返回行且 DeletedAt 置值，删除前 ownedKey 放行则测试失败）；
// 删除后 Auth 快照增量剔除（keys.deleted 含明文，不可鉴权）、列表过滤、不再
// 注册（不复活）。
func TestSoftDeletedKeyNotResurrectable(t *testing.T) {
	svc, fs, keys := newTask4Svc()
	ctx := context.Background()

	u, err := fs.CreateUser(ctx, &domain.User{Email: "sd-k@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	g, err := fs.CreateGroup(ctx, &domain.Group{Name: "sd-k-g", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	k, err := svc.CreateKey(ctx, u.ID, "k", g.ID, 0, 0)
	require.NoError(t, err)

	require.NoError(t, svc.DeleteKey(ctx, u.ID, k.ID))
	require.Equal(t, []string{k.KeyRaw}, keys.deleted, "删除后 Auth 快照增量剔除（已删 key 不可鉴权）")

	// 全路 404：GET/PUT/Rotate/Delete（对已删 key 幂等 404 可接受）
	_, err = svc.GetKey(ctx, u.ID, k.ID)
	require.ErrorIs(t, err, ErrNotFound)
	st := domain.KeyStatusDisabled
	_, err = svc.UpdateKey(ctx, u.ID, k.ID, nil, &st, nil, nil)
	require.ErrorIs(t, err, ErrNotFound, "已删 key PUT → 404（修复前可复活）")
	_, err = svc.RotateKey(ctx, u.ID, k.ID)
	require.ErrorIs(t, err, ErrNotFound, "已删 key rotate → 404（修复前可复活）")
	err = svc.DeleteKey(ctx, u.ID, k.ID)
	require.ErrorIs(t, err, ErrNotFound)

	// 列表过滤 + 不复活（仅创建时注册一次）
	rows, total, err := svc.ListKeys(ctx, u.ID, repository.ListQuery{})
	require.NoError(t, err)
	require.Zero(t, total)
	require.Empty(t, rows)
	require.Len(t, keys.upserted, 1, "删除后无任何增量注册（不复活）")
}

// TestSoftDeletedGroupUnusable F3 红绿：软删组三调用点 404（建 key/授
// assignment/SetUserGroups 逐组）；管理面 GET 详情与 GetGroupAssignments 仍
// 200（R2 收窄——repo GET 单个不过滤的既有语义不动）。
func TestSoftDeletedGroupUnusable(t *testing.T) {
	svc, fs, _ := newTask4Svc()
	ctx := context.Background()

	u, err := fs.CreateUser(ctx, &domain.User{Email: "sd-g@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	g, err := fs.CreateGroup(ctx, &domain.Group{Name: "sd-g", Visibility: domain.GroupVisibilityPrivate})
	require.NoError(t, err)
	require.NoError(t, fs.GrantGroup(ctx, g.ID, u.ID))
	require.NoError(t, svc.DeleteGroup(ctx, g.ID))

	// 软删组下建 key → 404（修复前成功 → 悬空孤儿 key）
	_, err = svc.CreateKey(ctx, u.ID, "k", g.ID, 0, 0)
	require.ErrorIs(t, err, ErrNotFound)
	// 软删组授 assignment → 404
	_, _, err = svc.SetGroupAssignments(ctx, g.ID, []int64{u.ID}, nil)
	require.ErrorIs(t, err, ErrNotFound)
	// SetUserGroups 逐组校验同路 → 404
	_, _, err = svc.SetUserGroups(ctx, u.ID, []int64{g.ID}, nil)
	require.ErrorIs(t, err, ErrNotFound)

	// 管理面 GET 详情仍 200（GET 单个可查已删项，deleted_at 置值）
	got, err := svc.GetGroup(ctx, g.ID)
	require.NoError(t, err)
	require.NotNil(t, got.DeletedAt, "软删后 deleted_at 置值")
	// GetGroupAssignments 仍 200（R2：读面不动——授予行照常可读）
	ids, _, err := svc.GetGroupAssignments(ctx, g.ID)
	require.NoError(t, err)
	require.Equal(t, []int64{u.ID}, ids, "软删组授予行照常可读（读面语义不变）")

	// 列表过滤软删组
	rows, total, err := svc.ListGroups(ctx, repository.ListQuery{Limit: 10})
	require.NoError(t, err)
	require.Zero(t, total)
	require.Empty(t, rows)
}

// TestDeleteGroupWithAccountsConflict F1 单删红绿：含账号组删除 → 409 +
// "group has accounts"；组未被删（deleted_at 仍 nil）、组内 key 未被删（校验
// 在删 key 前）。
func TestDeleteGroupWithAccountsConflict(t *testing.T) {
	svc, fs, keys := newTask4Svc()
	ctx := context.Background()

	u, err := fs.CreateUser(ctx, &domain.User{Email: "f1@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	tpl, err := fs.CreateTemplate(ctx, &domain.Template{
		Name: "f1-t", BaseURL: "https://t.example.com",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
	})
	require.NoError(t, err)
	emptyG, err := svc.CreateGroup(ctx, "f1-empty", domain.GroupVisibilityPublic, nil, nil)
	require.NoError(t, err)
	g, err := svc.CreateGroup(ctx, "f1-acc", domain.GroupVisibilityPublic, nil, nil)
	require.NoError(t, err)
	k, err := svc.CreateKey(ctx, u.ID, "k", g.ID, 0, 0)
	require.NoError(t, err)
	gids := []int64{g.ID}
	_, err = svc.CreateAccount(ctx, &domain.Account{
		Name: "f1-a", TemplateID: tpl.ID, UpstreamKey: "sk-1", GroupIDs: &gids,
	})
	require.NoError(t, err)

	// 空组可删（回归：无账号校验不误伤）
	require.NoError(t, svc.DeleteGroup(ctx, emptyG.ID))

	// 含账号组 → 409 + 文案（修复前静默成功——账号被调度快照过滤后静默脱离路由）
	err = svc.DeleteGroup(ctx, g.ID)
	require.ErrorIs(t, err, ErrConflict)
	require.Contains(t, err.Error(), "group has accounts")
	got, err := svc.GetGroup(ctx, g.ID)
	require.NoError(t, err)
	require.Nil(t, got.DeletedAt, "409 拒绝时组未被删")
	gotK, err := svc.GetKey(ctx, u.ID, k.ID)
	require.NoError(t, err)
	require.Nil(t, gotK.DeletedAt, "校验在删 key 前——组内 key 未被软删")
	require.Empty(t, keys.deleted, "409 拒绝时 Auth 快照零清理")
}

// TestDeleteGroupsBatchPreScanConflict F1 批删 R1 红绿：多组中含账号组 →
// 整批拒绝（预扫描先全量校验后删 key——组存 key 亡的中间态不发生）：无 key
// 被删、组全部未被删、Auth 快照零清理。
func TestDeleteGroupsBatchPreScanConflict(t *testing.T) {
	svc, fs, keys := newTask4Svc()
	ctx := context.Background()

	u, err := fs.CreateUser(ctx, &domain.User{Email: "f1b@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	tpl, err := fs.CreateTemplate(ctx, &domain.Template{
		Name: "f1b-t", BaseURL: "https://t.example.com",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
	})
	require.NoError(t, err)
	g1, err := svc.CreateGroup(ctx, "f1b-1", domain.GroupVisibilityPublic, nil, nil)
	require.NoError(t, err)
	g2, err := svc.CreateGroup(ctx, "f1b-2", domain.GroupVisibilityPublic, nil, nil)
	require.NoError(t, err)
	k, err := svc.CreateKey(ctx, u.ID, "k", g1.ID, 0, 0)
	require.NoError(t, err)
	gids := []int64{g2.ID}
	_, err = svc.CreateAccount(ctx, &domain.Account{
		Name: "f1b-a", TemplateID: tpl.ID, UpstreamKey: "sk-1", GroupIDs: &gids,
	})
	require.NoError(t, err)

	// 整批拒绝（g2 含账号；g1 在预扫描后本会被删 key——必须零发生）
	err = svc.DeleteGroupsBatch(ctx, []int64{g1.ID, g2.ID})
	require.ErrorIs(t, err, ErrConflict)
	require.Contains(t, err.Error(), "group has accounts")

	gotK, err := svc.GetKey(ctx, u.ID, k.ID)
	require.NoError(t, err)
	require.Nil(t, gotK.DeletedAt, "整批拒绝：g1 的 key 未被删（预扫描先于删 key）")
	for _, id := range []int64{g1.ID, g2.ID} {
		got, err := svc.GetGroup(ctx, id)
		require.NoError(t, err)
		require.Nil(t, got.DeletedAt, "整批拒绝：组 %d 未被删", id)
	}
	require.Empty(t, keys.deleted, "整批拒绝：Auth 快照零清理")
}

// TestUpdateKeyPatchSingleField S3-F1：单字段 PUT 只改该字段（patch 化——其余
// 字段保持原值，不再全列写回）。
func TestUpdateKeyPatchSingleField(t *testing.T) {
	svc, fs, _ := newTask4Svc()
	ctx := context.Background()

	u, err := fs.CreateUser(ctx, &domain.User{Email: "patch1@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	g, err := fs.CreateGroup(ctx, &domain.Group{Name: "patch1-g", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	k, err := svc.CreateKey(ctx, u.ID, "k", g.ID, 3, 1000)
	require.NoError(t, err)

	q := int64(2000)
	updated, err := svc.UpdateKey(ctx, u.ID, k.ID, nil, nil, nil, &q)
	require.NoError(t, err)
	require.Equal(t, int64(2000), updated.Quota, "quota 被改")
	require.Equal(t, "k", updated.Name, "name 未被改动")
	require.Equal(t, 3, updated.MaxConcurrency, "并发上限未被改动")
	require.Equal(t, domain.KeyStatusActive, updated.Status, "status 未被改动")

	// 全 nil = 无变更：零写库直接返回当前行
	noop, err := svc.UpdateKey(ctx, u.ID, k.ID, nil, nil, nil, nil)
	require.NoError(t, err)
	require.Equal(t, int64(2000), noop.Quota)
}

// TestUpdateKeyPatchConcurrent S3-F1 -race：并发两个 PUT 改不同字段 → 各自
// 生效（patch 化消除 lost-update——修复前全行快照写回，后写者覆盖先写者）。
func TestUpdateKeyPatchConcurrent(t *testing.T) {
	svc, fs, _ := newTask4Svc()
	ctx := context.Background()

	u, err := fs.CreateUser(ctx, &domain.User{Email: "patchc@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	g, err := fs.CreateGroup(ctx, &domain.Group{Name: "patchc-g", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	k, err := svc.CreateKey(ctx, u.ID, "k", g.ID, 0, 0)
	require.NoError(t, err)

	name := "renamed"
	st := domain.KeyStatusDisabled
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := svc.UpdateKey(ctx, u.ID, k.ID, &name, nil, nil, nil)
		require.NoError(t, err)
	}()
	go func() {
		defer wg.Done()
		_, err := svc.UpdateKey(ctx, u.ID, k.ID, nil, &st, nil, nil)
		require.NoError(t, err)
	}()
	wg.Wait()

	got, err := svc.GetKey(ctx, u.ID, k.ID)
	require.NoError(t, err)
	require.Equal(t, "renamed", got.Name, "name PUT 生效")
	require.Equal(t, domain.KeyStatusDisabled, got.Status, "status PUT 生效（互不覆盖）")
	require.Equal(t, 0, got.MaxConcurrency, "未改字段保持原值")
	require.Equal(t, int64(0), got.Quota, "未改字段保持原值")
}

// TestSetGroupAssignmentsRollback S3-F2：替换中途注入失败（RevokeGroup）→
// 整体回滚——已完成的 Grant 一并撤销，主视图回到替换前（fake 事务暂存语义）。
func TestSetGroupAssignmentsRollback(t *testing.T) {
	fs := newFakeStore()
	svc := &Service{store: fs, inv: &invRecorder{}, log: nil}
	ctx := context.Background()

	u1, err := fs.CreateUser(ctx, &domain.User{Email: "rb1@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	u2, err := fs.CreateUser(ctx, &domain.User{Email: "rb2@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	g, err := fs.CreateGroup(ctx, &domain.Group{Name: "rb-g", Visibility: domain.GroupVisibilityPrivate})
	require.NoError(t, err)
	_, _, err = svc.SetGroupAssignments(ctx, g.ID, []int64{u1.ID}, nil)
	require.NoError(t, err)

	// 注入 RevokeGroup 失败：替换 [u1] → [u2] 在撤销步中止
	fs.revokeGroupErr = fmt.Errorf("injected revoke failure")
	_, _, err = svc.SetGroupAssignments(ctx, g.ID, []int64{u2.ID}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "injected revoke failure")

	got, err := fs.ListAssignmentsByGroup(ctx, g.ID)
	require.NoError(t, err)
	require.Len(t, got, 1, "整体回滚：授予行数与替换前一致")
	require.Equal(t, u1.ID, got[0].UserID, "回滚：u1 授予保留、u2 授予未落（Grant 一并回滚）")
}
