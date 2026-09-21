// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// TestAccountBaseURLInvalidation 账号级 base_url 变更 → clients 失效：
// PatchAccount 按值判定（M4——nil↔"" 同值不误报）并入既有 keyChanged（复用
// Accounts(gids, keyChanged) 参数面，零新增失效类型）；UpdateAccountsBatch
// 保守失效（提供 BaseURL 即失效，含 "" 清空态）。
func TestAccountBaseURLInvalidation(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	rec := &invRecorder{}
	svc := &Service{store: fs, inv: rec, log: nil}
	tpl, err := svc.CreateTemplate(ctx, &domain.Template{
		Name: "t", BaseURL: "https://t.example.com",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
	})
	require.NoError(t, err)
	acc, err := svc.CreateAccount(ctx, repository.AccountPatch{
		Name: strPtr("a1"), TemplateID: &tpl.ID, UpstreamKey: strPtr("sk-1"),
	})
	require.NoError(t, err)

	t.Run("PatchAccount base_url 变更 → keyChanged", func(t *testing.T) {
		b := "https://acc.example.com"
		_, err := svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{BaseURL: &b}, nil)
		require.NoError(t, err)
		require.True(t, rec.last().key, "账号级 base_url 变更 → clients 失效（keyChanged=true）")
	})

	t.Run("PatchAccount base_url 不变 → 不失效", func(t *testing.T) {
		b := "https://acc.example.com"
		_, err := svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{BaseURL: &b}, nil)
		require.NoError(t, err)
		require.False(t, rec.last().key, "base_url 不变 → keyChanged=false（按值判定不误报）")
	})

	t.Run("UpdateAccountsBatch 提供 BaseURL（非空与空串清空）→ keyChanged", func(t *testing.T) {
		b := "https://batch.example.com"
		_, err := svc.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{BaseURL: &b})
		require.NoError(t, err)
		require.True(t, rec.last().key, "批量非空 base_url → 保守失效")
		empty := ""
		_, err = svc.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{BaseURL: &empty})
		require.NoError(t, err)
		require.True(t, rec.last().key, "批量空串（清空态）→ 保守失效")
	})

	t.Run("UpdateAccountsBatch 未提供 BaseURL/UpstreamKey → 不失效", func(t *testing.T) {
		name := "a1"
		_, err := svc.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{Name: &name})
		require.NoError(t, err)
		require.False(t, rec.last().key, "未提供 base_url/upstream_key → keyChanged=false")
	})
}

// TestValidateAccountPatchBaseURL 补丁 base_url 校验：空串放行（repo 层清空编码，
// 合法）；非空时复用 validateBaseURL；nil = 不变不校验。
func TestValidateAccountPatchBaseURL(t *testing.T) {
	require.NoError(t, validateAccountPatch(repository.AccountPatch{BaseURL: strPtr("")}), "空串 = 清空编码，合法")
	require.NoError(t, validateAccountPatch(repository.AccountPatch{BaseURL: strPtr("https://api.example.com")}))
	require.ErrorIs(t, validateAccountPatch(repository.AccountPatch{BaseURL: strPtr("no-scheme")}), ErrInvalidInput)
	require.NoError(t, validateAccountPatch(repository.AccountPatch{Name: strPtr("x")}), "字段缺省 = 不变（补丁非空即可）")
}
