// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// supplier_view_reload_test.go 验证账号归属/启用写面在成功后触发财务视图本地
// Reload（spec 2026-10-09 §4.6.3 发布屏障）——名称类补丁不触发（不换代）。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// fakeSupplierViewReloader 计数 Reload 调用（模拟 *supplier.ViewLoader）。
type fakeSupplierViewReloader struct{ calls int }

func (f *fakeSupplierViewReloader) Reload(ctx context.Context) bool {
	f.calls++
	return true
}

// TestPatchAccountOwnershipTriggersSupplierViewReload 转属/启用经唯一写点后须触发
// 财务视图换代；纯名称补丁不触发。
func TestPatchAccountOwnershipTriggersSupplierViewReload(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	acc := seedLifecycleAccount(t, fs)
	reload := &fakeSupplierViewReloader{}
	svc := &Service{store: fs, inv: &invRecorder{}, supplierViewReload: reload}

	name := "renamed"
	_, err := svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{Name: &name}, nil)
	require.NoError(t, err)
	require.Zero(t, reload.calls, "名称补丁不触发视图换代")

	owner := int64(42)
	_, err = svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{SupplierUserID: &owner}, nil)
	require.NoError(t, err)
	require.Equal(t, 1, reload.calls, "归属转属须触发财务视图换代")

	off := false
	_, err = svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{Enabled: &off}, nil)
	require.NoError(t, err)
	require.Equal(t, 2, reload.calls, "启用/禁用须触发财务视图换代")
}

// TestCreateAccountOwnedTriggersSupplierViewReload 带归属创建须触发视图换代
// （否则调度快照已含、财务视图未含，准入门一直拒绝该账号入选）；平台自有不触发。
func TestCreateAccountOwnedTriggersSupplierViewReload(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	svc := &Service{store: fs, inv: &invRecorder{}}
	_, err := svc.CreateTemplate(ctx, &domain.Template{Name: "t", BaseURL: "https://u", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}})
	require.NoError(t, err)
	reload := &fakeSupplierViewReloader{}
	svc.supplierViewReload = reload

	_, err = svc.CreateAccount(ctx, repository.AccountPatch{Name: strPtr("ownless"), TemplateID: int64Ptr(1), UpstreamKey: strPtr("k")})
	require.NoError(t, err)
	require.Zero(t, reload.calls, "平台自有创建不触发视图换代")

	owner := int64(7)
	_, err = svc.CreateAccount(ctx, repository.AccountPatch{Name: strPtr("owned"), TemplateID: int64Ptr(1), UpstreamKey: strPtr("k2"), SupplierUserID: &owner})
	require.NoError(t, err)
	require.Equal(t, 1, reload.calls, "带归属创建须触发视图换代")
}
