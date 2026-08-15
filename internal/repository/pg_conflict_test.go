// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// TestUniqueConflictPG 真实 PG 唯一约束冲突 → repository.ErrConflict（含冲突值
// 详情；此前裸透传 PG 错误，service/handler 原样透传 → 500 而非 409）。
// 覆盖：template/group 创建 name 唯一、单/批量改名撞已有 name、key 创建
// key_raw 唯一（防御——随机明文理论不撞，映射保证一致性）。
func TestUniqueConflictPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	t.Run("template create name conflict", func(t *testing.T) {
		_, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
			Name: "tpl-dup", BaseURL: "https://u1",
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		})
		require.NoError(t, err)
		_, err = repos.Templates.CreateTemplate(ctx, &domain.Template{
			Name: "tpl-dup", BaseURL: "https://u2",
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		})
		require.ErrorIs(t, err, repository.ErrConflict, "重复 name → ErrConflict（409 语义）")
		require.Contains(t, err.Error(), `name="tpl-dup"`, "冲突详情含 name")
	})

	t.Run("group create name conflict", func(t *testing.T) {
		_, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "grp-dup", Visibility: domain.GroupVisibilityPublic})
		require.NoError(t, err)
		_, err = repos.Groups.CreateGroup(ctx, &domain.Group{Name: "grp-dup", Visibility: domain.GroupVisibilityPrivate})
		require.ErrorIs(t, err, repository.ErrConflict, "重复 name → ErrConflict（409 语义）")
		require.Contains(t, err.Error(), `name="grp-dup"`, "冲突详情含 name")
	})

	t.Run("template rename conflict", func(t *testing.T) {
		t1, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
			Name: "ren-1", BaseURL: "https://u1",
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		})
		require.NoError(t, err)
		t2, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
			Name: "ren-2", BaseURL: "https://u2",
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		})
		require.NoError(t, err)
		t2.Name = t1.Name
		_, err = repos.Templates.UpdateTemplate(ctx, t2)
		require.ErrorIs(t, err, repository.ErrConflict, "改名撞已有 name → ErrConflict")
		require.Contains(t, err.Error(), `name="ren-1"`)
	})

	t.Run("batch rename conflict", func(t *testing.T) {
		t1, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
			Name: "bat-1", BaseURL: "https://u1",
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		})
		require.NoError(t, err)
		t2, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
			Name: "bat-2", BaseURL: "https://u2",
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		})
		require.NoError(t, err)
		dup := t1.Name
		err = repos.Templates.UpdateTemplatesBatch(ctx, []int64{t2.ID}, repository.TemplatePatch{Name: &dup})
		require.ErrorIs(t, err, repository.ErrConflict, "批量改名撞已有 name → ErrConflict")
		require.Contains(t, err.Error(), `name="bat-1"`)
	})

	t.Run("key raw conflict", func(t *testing.T) {
		u := seedPGUser(t, repos, "keys-dup@example.com")
		g := seedPGGroup(t, repos, "keys-dup-g")
		k1, err := repos.Keys.CreateKey(ctx, &domain.Key{
			UserID: u.ID, GroupID: g.ID, Name: "k1",
			KeyRaw: "gk-dup",
			Status: domain.KeyStatusActive,
		})
		require.NoError(t, err)
		_, err = repos.Keys.CreateKey(ctx, &domain.Key{
			UserID: u.ID, GroupID: g.ID, Name: "k2",
			KeyRaw: k1.KeyRaw,
			Status: domain.KeyStatusActive,
		})
		require.ErrorIs(t, err, repository.ErrConflict, "key_raw 唯一 → ErrConflict（防御）")
		require.Contains(t, err.Error(), "key_raw", "冲突详情标识冲突列")
	})

	t.Run("user create email conflict", func(t *testing.T) {
		_, err := repos.Users.CreateUser(ctx, &domain.User{
			Email: "dup@example.com", PasswordHash: "h1",
			Role: domain.RoleUser, Status: domain.UserStatusActive,
		})
		require.NoError(t, err)
		_, err = repos.Users.CreateUser(ctx, &domain.User{
			Email: "dup@example.com", PasswordHash: "h2",
			Role: domain.RoleUser, Status: domain.UserStatusActive,
		})
		require.ErrorIs(t, err, repository.ErrConflict, "重复 email → ErrConflict（409 语义）")
		require.Contains(t, err.Error(), `email="dup@example.com"`, "冲突详情含 email")
	})

	t.Run("user create email conflict concurrent", func(t *testing.T) {
		// 两 goroutine 同 email 并发插入（模拟注册双过 pre-check 后一者撞
		// 23505）：恰一个成功、一个 ErrConflict——绝不裸透传 PG 错误（500）。
		email := "race@example.com"
		results := make([]error, 2)
		var wg sync.WaitGroup
		for i := range results {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, err := repos.Users.CreateUser(ctx, &domain.User{
					Email: email, PasswordHash: "h",
					Role: domain.RoleUser, Status: domain.UserStatusActive,
				})
				results[i] = err
			}(i)
		}
		wg.Wait()
		ok := 0
		for _, err := range results {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, repository.ErrConflict):
				// 并发落败方 → 409 语义
			default:
				t.Fatalf("unexpected error type: %v", err)
			}
		}
		require.Equal(t, 1, ok, "恰一个成功（另一并发者 409）")
	})
}
