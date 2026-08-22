// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// TestAccountBaseURLRoundTripPG 账号级 base_url 往返（C1）：设值落库/读回/
// 清空（nil 往返）；批量 patch 三态："" → NULL 清空、非空 → 落值、nil → 不变。
// 真实 PostgreSQL 基座（TEST_DATABASE_URL 未设 → t.Skip）。
func TestAccountBaseURLRoundTripPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "t-bu", BaseURL: "https://u/v1", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
	})
	require.NoError(t, err)
	acc, err := repos.Accounts.CreateAccount(ctx, &domain.Account{
		Name: "bu1", TemplateID: tpl.ID, UpstreamKey: "sk-bu1", Weight: 1, MaxConcurrency: 8,
	})
	require.NoError(t, err)

	// 创建未设 → nil（NULL = 继承模板）
	got, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Nil(t, got.BaseURL, "创建未设 base_url → NULL")

	// 单条更新设值 → 读回
	b := "https://acc.example.com"
	acc.BaseURL = &b
	updated, err := repos.Accounts.UpdateAccount(ctx, acc, nil)
	require.NoError(t, err)
	require.NotNil(t, updated.BaseURL)
	require.Equal(t, b, *updated.BaseURL)
	got, err = repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.NotNil(t, got.BaseURL)
	require.Equal(t, b, *got.BaseURL)

	// 清空：nil → NULL 往返（继承模板）
	acc.BaseURL = nil
	_, err = repos.Accounts.UpdateAccount(ctx, acc, nil)
	require.NoError(t, err)
	got, err = repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Nil(t, got.BaseURL, "nil → 落 NULL（继承模板）")

	// --- 批量三态（C1） ---
	acc2, err := repos.Accounts.CreateAccount(ctx, &domain.Account{
		Name: "bu2", TemplateID: tpl.ID, UpstreamKey: "sk-bu2", Weight: 1, MaxConcurrency: 8,
	})
	require.NoError(t, err)

	// 非空 → 落值
	b2 := "https://batch.example.com"
	require.NoError(t, repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc2.ID}, repository.AccountPatch{BaseURL: &b2}))
	got2, err := repos.Accounts.GetAccount(ctx, acc2.ID)
	require.NoError(t, err)
	require.NotNil(t, got2.BaseURL)
	require.Equal(t, b2, *got2.BaseURL)

	// "" → NULL 清空
	empty := ""
	require.NoError(t, repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc2.ID}, repository.AccountPatch{BaseURL: &empty}))
	got2, err = repos.Accounts.GetAccount(ctx, acc2.ID)
	require.NoError(t, err)
	require.Nil(t, got2.BaseURL, "批量空串 → 落 NULL（继承模板）")

	// nil → 不变（先设值，再 nil patch）
	b3 := "https://keep.example.com"
	require.NoError(t, repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc2.ID}, repository.AccountPatch{BaseURL: &b3}))
	require.NoError(t, repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc2.ID}, repository.AccountPatch{Name: &acc2.Name}))
	got2, err = repos.Accounts.GetAccount(ctx, acc2.ID)
	require.NoError(t, err)
	require.NotNil(t, got2.BaseURL)
	require.Equal(t, b3, *got2.BaseURL, "nil patch → base_url 不变")
}
