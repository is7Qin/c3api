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

	"github.com/is7qin/c3api/internal/credential"
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

	_, err = svc.CreateAccount(ctx, repository.AccountPatch{Name: new("ownless"), TemplateID: new(int64(1)), UpstreamKey: new("k")})
	require.NoError(t, err)
	require.Zero(t, reload.calls, "平台自有创建不触发视图换代")

	owner := int64(7)
	_, err = svc.CreateAccount(ctx, repository.AccountPatch{Name: new("owned"), TemplateID: new(int64(1)), UpstreamKey: new("k2"), SupplierUserID: &owner})
	require.NoError(t, err)
	require.Equal(t, 1, reload.calls, "带归属创建须触发视图换代")
}

// TestImportCodexOAuthSupplierScopeTriggersSupplierViewReload 批量导入入口与设备码
// 登录共用写核心：供应商作用域的导入必须在批末触发财务视图换代（屏障在核心内，
// 入口不得各自记得调用）。
func TestImportCodexOAuthSupplierScopeTriggersSupplierViewReload(t *testing.T) {
	fs := newFakeStore()
	fs.tpls[1] = &domain.Template{ID: 1, Name: "t", CredentialType: credential.TypeCodexOAuth}
	reload := &fakeSupplierViewReloader{}
	svc := New(Deps{Store: fs, Invalidate: &invRecorder{}, EmailCodeStore: testEmailCodes, SupplierViewReload: reload})
	tplID := int64(1)

	supplierCtx := domain.WithAccountScope(context.Background(), domain.SupplierAccountScope(42))
	_, err := svc.ImportCodexOAuthAccounts(supplierCtx, []domain.CodexOAuthImportItem{{
		CodexEmail: "s@example.com", CodexAccountID: "acc-s",
		CodexOAuthToken: "at", CodexOAuthRefreshToken: "rt",
	}}, &tplID, nil, domain.CodexImportConfig{})
	require.NoError(t, err)
	require.Equal(t, 1, reload.calls, "供应商面导入须触发视图换代")

	_, err = svc.ImportCodexOAuthAccounts(context.Background(), []domain.CodexOAuthImportItem{{
		CodexEmail: "p@example.com", CodexAccountID: "acc-p",
		CodexOAuthToken: "at", CodexOAuthRefreshToken: "rt",
	}}, &tplID, nil, domain.CodexImportConfig{})
	require.NoError(t, err)
	require.Equal(t, 1, reload.calls, "管理面导入不触发供应商视图换代")
}
