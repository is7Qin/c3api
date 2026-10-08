// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/cryptox"
	"github.com/is7qin/c3api/pkg/logx"
)

// CreateManagementKey 自助签发管理 key（/api/{user,user/supplier,admin}/management-keys
// POST；spec 2026-10-09）：cryptox 生成明文 mk- → 落库 → 本实例 Auth 快照增量
// Upsert（创建即时可用）→ publish NOTIFY（跨实例经 dispatcher → KindManagementKeys
// → Auth.Reload 收敛）。owner = 调用方身份（user/supplier 面 ClaimsFrom.UserID、
// admin 面 UserIDFromContext）。
func (s *Service) CreateManagementKey(ctx context.Context, ownerID int64, name string) (*domain.ManagementKey, error) {
	if ownerID <= 0 || name == "" {
		return nil, ErrInvalidInput
	}
	raw := cryptox.NewManagementKey()
	created, err := s.store.CreateManagementKey(ctx, &domain.ManagementKey{
		UserID: ownerID, Name: name, KeyRaw: raw, Status: domain.ManagementKeyStatusActive,
	})
	if err != nil {
		return nil, mapRepoErr(err) // key_raw 唯一冲突 → ErrConflict（409）
	}
	s.registerManagementKey(created)
	s.publish(ctx, notify.Change{ManagementKeys: true})
	if s.log != nil {
		s.log.Info("management key created", logx.Int64("id", created.ID), logx.Int64("user_id", ownerID))
	}
	return created, nil
}

// ListManagementKeys owner 自身的管理 key 列表（软删过滤；含明文，与客户端 key
// 一致——自托管权衡）。
func (s *Service) ListManagementKeys(ctx context.Context, ownerID int64) ([]*domain.ManagementKey, error) {
	if ownerID <= 0 {
		return nil, ErrInvalidInput
	}
	return s.store.ListManagementKeysByUser(ctx, ownerID)
}

// UpdateManagementKey 更新自身管理 key（name/status；nil = 不改）。status ∈
// {active,disabled}：disabled 软禁用（快照即时 401，可再启用）。owner-only（越域/
// 缺失 → 404）。写后本实例快照 Upsert（禁用即时失效）+ publish NOTIFY。
func (s *Service) UpdateManagementKey(ctx context.Context, ownerID, id int64, name *string, status *domain.ManagementKeyStatus) (*domain.ManagementKey, error) {
	if ownerID <= 0 || id <= 0 {
		return nil, ErrInvalidInput
	}
	if name != nil && *name == "" {
		return nil, ErrInvalidInput
	}
	if status != nil && !status.Valid() {
		return nil, ErrInvalidInput
	}
	updated, err := s.store.UpdateManagementKey(ctx, &repository.ManagementKeyPatch{
		UserID: ownerID, ID: id, Name: name, Status: status,
	})
	if err != nil {
		return nil, mapRepoErr(err)
	}
	s.registerManagementKey(updated)
	s.publish(ctx, notify.Change{ManagementKeys: true})
	return updated, nil
}

// DeleteManagementKey 软删除自身管理 key（owner-only；越域/缺失/已删 → 404）。
// 写后本实例快照 Delete（即时 401）+ publish NOTIFY。
func (s *Service) DeleteManagementKey(ctx context.Context, ownerID, id int64) error {
	if ownerID <= 0 || id <= 0 {
		return ErrInvalidInput
	}
	raw, err := s.store.DeleteManagementKey(ctx, ownerID, id)
	if err != nil {
		return mapRepoErr(err)
	}
	if s.mgmtKeys != nil {
		s.mgmtKeys.DeleteManagementKey(raw)
	}
	s.publish(ctx, notify.Change{ManagementKeys: true})
	return nil
}

// registerManagementKey 把管理 key 写入本实例 Auth 快照（纯内存，不可失败）：
// 禁用/启用经同一 Upsert 覆盖（AuthenticateManagement 按 Status==active 门控，
// disabled 即时 401）。nil 装配（测试降级）⇒ no-op；已软删的行（deleted_at 非空）
// 一律不入快照——防御性保证 A4（已删 key 不可复活为可用凭据）。
func (s *Service) registerManagementKey(k *domain.ManagementKey) {
	if s.mgmtKeys == nil || k == nil || k.DeletedAt != nil {
		return
	}
	s.mgmtKeys.UpsertManagementKey(k.KeyRaw, domain.ManagementKeyMeta{
		ID: k.ID, UserID: k.UserID, Status: k.Status,
	})
}
