// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// TestCreateTemplateNameConflict 重复 name 创建模板 → service.ErrConflict（409 语义），
// 错误消息含冲突详情（与真实 repo 的 repository.ErrConflict 包装同构）；改名撞
// 已有 name（更新路径）同样映射。
func TestCreateTemplateNameConflict(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}
	ctx := context.Background()

	_, err := svc.CreateTemplate(ctx, &domain.Template{
		Name: "dup", BaseURL: "https://a.example.com",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
	})
	require.NoError(t, err)

	_, err = svc.CreateTemplate(ctx, &domain.Template{
		Name: "dup", BaseURL: "https://b.example.com",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
	})
	require.ErrorIs(t, err, ErrConflict, "重复 name → ErrConflict（409）")
	require.Contains(t, err.Error(), `name="dup"`, "409 消息含冲突详情")

	other, err := svc.CreateTemplate(ctx, &domain.Template{
		Name: "other", BaseURL: "https://c.example.com",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
	})
	require.NoError(t, err)
	other.Name = "dup"
	_, err = svc.UpdateTemplate(ctx, other)
	require.ErrorIs(t, err, ErrConflict, "改名撞已有 name → ErrConflict（409）")
	require.Contains(t, err.Error(), `name="dup"`)
}

// TestCreateGroupNameConflict 重复 name 创建分组 → service.ErrConflict（409 语义），
// 错误消息含冲突详情。
func TestCreateGroupNameConflict(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}, log: nil}
	ctx := context.Background()

	_, err := svc.CreateGroup(ctx, "dup-g", domain.GroupVisibilityPublic, nil, nil)
	require.NoError(t, err)

	_, err = svc.CreateGroup(ctx, "dup-g", domain.GroupVisibilityPrivate, nil, nil)
	require.ErrorIs(t, err, ErrConflict, "重复 name → ErrConflict（409）")
	require.Contains(t, err.Error(), `name="dup-g"`, "409 消息含冲突详情")

	g, err := svc.CreateGroup(ctx, "other-g", domain.GroupVisibilityPublic, nil, nil)
	require.NoError(t, err)
	g.Name = "dup-g"
	_, err = svc.UpdateGroup(ctx, g)
	require.ErrorIs(t, err, ErrConflict, "改名撞已有 name → ErrConflict（409）")
	require.Contains(t, err.Error(), `name="dup-g"`)
}

// TestMapRepoErrConflict mapRepoErr 的 ErrConflict 分支：repository.ErrConflict →
// service.ErrConflict（保留冲突详情，handler 409 响应带详情）；非冲突错误原样透传。
func TestMapRepoErrConflict(t *testing.T) {
	err := fmt.Errorf("%w: name=%q", repository.ErrConflict, "x")
	mapped := mapRepoErr(err)
	require.ErrorIs(t, mapped, ErrConflict, "repository.ErrConflict → service.ErrConflict")
	require.Contains(t, mapped.Error(), `name="x"`, "409 消息保留冲突详情")

	raw := errors.New("boom")
	require.Same(t, raw, mapRepoErr(raw), "非映射错误原样透传")
}
