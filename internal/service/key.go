// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"fmt"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/cryptox"
	"github.com/is7qin/c3api/pkg/logx"
)

// ErrGroupNotEligible private 组未授予（key 创建组可选性校验；包装
// ErrInvalidInput → 400）。
var ErrGroupNotEligible = fmt.Errorf("%w: group is private and not granted to user", ErrInvalidInput)

// CreateKey 用户自建 key（/user/keys POST）：
// 组可选性校验（public 或已授予 private）→ 用户门禁字段写库前预取（B1-1：
// GetUser 前置——写后注册退化为纯内存 Upsert 不可失败）→ cryptox 生成明文
// → 落库 → Auth 增量纯内存 Upsert。明文长期可查看/复制（列表/详情回显）。
func (s *Service) CreateKey(ctx context.Context, userID int64, name string, groupID int64, maxConcurrency int, quota int64) (*domain.Key, error) {
	if name == "" || groupID <= 0 || maxConcurrency < 0 || quota < 0 {
		return nil, ErrInvalidInput
	}
	if err := s.checkGroupEligible(ctx, userID, groupID); err != nil {
		return nil, err
	}
	// B1-1：用户门禁字段写库前预取（checkGroupEligible 本就在写前做 DB 读，
	// 前置 GetUser 零成本）——写后 upsertKeyMetaInMemory 不可失败，失败窗口
	// 整体消失（新 raw 永不蒸发）
	var user *domain.User
	if s.keys != nil {
		u, err := s.store.GetUser(ctx, userID)
		if err != nil {
			return nil, mapRepoErr(err)
		}
		user = u
	}
	raw := cryptox.NewGroupKey()
	created, err := s.store.CreateKey(ctx, &domain.Key{
		UserID: userID, GroupID: groupID, Name: name,
		KeyRaw: raw,
		Status: domain.KeyStatusActive, MaxConcurrency: maxConcurrency,
		Quota: quota, QuotaUsed: 0,
	})
	if err != nil {
		return nil, mapRepoErr(err) // key_raw 唯一冲突 → ErrConflict（409）
	}
	s.upsertKeyMetaInMemory(created, user) // 写后注册纯内存（不可失败）
	// key 创建是 #14 多实例缺口（不进 invalidate）：其余实例鉴权快照需全量
	// Reload 覆盖（v1 不做增量定向）。
	s.publish(ctx, notify.Change{Keys: true})
	if s.log != nil {
		s.log.Info("key created", logx.Int64("id", created.ID), logx.Int64("user_id", userID), logx.String("name", name))
	}
	return created, nil
}

// checkGroupEligible 组可选性：组必须存在（缺失 → 404）；private 组须有
// 授予记录（未授予 → 400，防越权使用专属容量池）。
func (s *Service) checkGroupEligible(ctx context.Context, userID, groupID int64) error {
	g, err := s.store.GetGroup(ctx, groupID)
	if err != nil {
		return mapRepoErr(err)
	}
	if g.Visibility == domain.GroupVisibilityPublic {
		return nil
	}
	assignments, err := s.store.ListAssignmentsByUser(ctx, userID)
	if err != nil {
		return err
	}
	for _, a := range assignments {
		if a.GroupID == groupID {
			return nil
		}
	}
	return ErrGroupNotEligible
}

// ListAdminKeys 管理端全量 key 列表（/admin/keys，spec 2026-08-16）：全量
// 视角（不限归属用户）+ name/user_id/group_id 筛选 + sort 白名单
// id/name/created_at。脱敏在 handler 转换面（AdminKey 无 key 明文字段——
// 用户裁决，明文绝不下发管理端）。
func (s *Service) ListAdminKeys(ctx context.Context, q repository.ListQuery) ([]*domain.Key, int64, error) {
	if err := validateListQuery(q, listSortFields["admin_keys"]); err != nil {
		return nil, 0, err
	}
	return s.store.ListKeys(ctx, q)
}

// GetKey 用户取自己的 key 详情（/user/keys/{id} GET）。
func (s *Service) GetKey(ctx context.Context, userID, keyID int64) (*domain.Key, error) {
	return s.ownedKey(ctx, userID, keyID)
}

// ListKeys 用户自己的 key 列表（/user/keys GET）。
func (s *Service) ListKeys(ctx context.Context, userID int64, q repository.ListQuery) ([]*domain.Key, int64, error) {
	if err := validateListQuery(q, listSortFields["keys"]); err != nil {
		return nil, 0, err
	}
	return s.store.ListKeysByUser(ctx, userID, q)
}

// UpdateKey 更新自己的 key（name/status/max_concurrency/quota；nil 字段不变）。
// 变更后 Auth 增量 Upsert（禁用/额度调整即时生效——评审 I-2 的 key 级路径）。
func (s *Service) UpdateKey(ctx context.Context, userID, keyID int64, name *string, status *domain.KeyStatus, maxConcurrency *int, quota *int64) (*domain.Key, error) {
	cur, err := s.ownedKey(ctx, userID, keyID)
	if err != nil {
		return nil, err
	}
	if name != nil {
		if *name == "" {
			return nil, ErrInvalidInput
		}
		cur.Name = *name
	}
	if status != nil {
		if !status.Valid() {
			return nil, ErrInvalidInput
		}
		cur.Status = *status
	}
	if maxConcurrency != nil {
		if *maxConcurrency < 0 {
			return nil, ErrInvalidInput
		}
		cur.MaxConcurrency = *maxConcurrency
	}
	if quota != nil {
		if *quota < 0 {
			return nil, ErrInvalidInput
		}
		cur.Quota = *quota
	}
	updated, err := s.store.UpdateKey(ctx, cur)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if err := s.upsertKeyMeta(ctx, updated); err != nil {
		return nil, err
	}
	s.publish(ctx, notify.Change{Keys: true}) // 改额度/状态 → 全实例 auth 快照全量 Reload
	return updated, nil
}

// RotateKey 轮换自己的 key（/user/keys/{id}/rotate）：新明文落库；旧明文
// 增量移除（立即失效）、新明文增量注册。用户门禁字段写库前预取（B1-1：
// GetUser 前置——Delete 后只剩不可失败的内存 Upsert，失败窗口整体消失——
// DB 已轮换只留新明文时新 raw 永不蒸发）。
func (s *Service) RotateKey(ctx context.Context, userID, keyID int64) (*domain.Key, error) {
	cur, err := s.ownedKey(ctx, userID, keyID)
	if err != nil {
		return nil, err
	}
	// B1-1：GetUser 写库前预取（失败 → 轮换零发生，旧 key 原样可用）
	var user *domain.User
	if s.keys != nil {
		u, err := s.store.GetUser(ctx, userID)
		if err != nil {
			return nil, mapRepoErr(err)
		}
		user = u
	}
	raw := cryptox.NewGroupKey()
	updated, err := s.store.RotateKey(ctx, keyID, raw)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if s.keys != nil {
		s.keys.Delete(cur.KeyRaw)
		s.upsertKeyMetaInMemory(updated, user) // 写后注册纯内存（不可失败）
	}
	s.publish(ctx, notify.Change{Keys: true}) // 轮换 = 旧明文失效 + 新明文注册 → 全量覆盖
	if s.log != nil {
		s.log.Info("key rotated", logx.Int64("id", keyID), logx.Int64("user_id", userID))
	}
	return updated, nil
}

// DeleteKey 删除自己的 key（/user/keys/{id} DELETE；Auth 增量移除——旧明文
// 立即失效）。
func (s *Service) DeleteKey(ctx context.Context, userID, keyID int64) error {
	cur, err := s.ownedKey(ctx, userID, keyID)
	if err != nil {
		return err
	}
	if err := s.store.DeleteKey(ctx, keyID); err != nil {
		return mapRepoErr(err)
	}
	if s.keys != nil {
		s.keys.Delete(cur.KeyRaw)
	}
	s.publish(ctx, notify.Change{Keys: true}) // 删除 → 全实例 auth 快照全量 Reload（旧明文立即失效）
	return nil
}

// ownedKey 取 key 并校验归属：非本人 key 一律按不存在处理（404，防越权探测
// 他人 key 存在性）。
func (s *Service) ownedKey(ctx context.Context, userID, keyID int64) (*domain.Key, error) {
	k, err := s.store.GetKey(ctx, keyID)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if k.UserID != userID {
		return nil, ErrNotFound
	}
	return k, nil
}

// upsertKeyMeta 构造 KeyMeta 并增量注册到 Auth 鉴权快照（UpdateKey 用——P3
// 路径：GetUser 失败 → 错误返回，快照靠全量 Reload 兜底 ≤60s 自愈）。
func (s *Service) upsertKeyMeta(ctx context.Context, k *domain.Key) error {
	if s.keys == nil {
		return nil
	}
	u, err := s.store.GetUser(ctx, k.UserID)
	if err != nil {
		return mapRepoErr(err)
	}
	s.upsertKeyMetaInMemory(k, u)
	return nil
}

// upsertKeyMetaInMemory 纯内存增量注册（不可失败）：CreateKey/RotateKey 的
// 用户门禁字段已写库前预取（B1-1）——调用方保证 s.keys != nil 时 u 非 nil。
func (s *Service) upsertKeyMetaInMemory(k *domain.Key, u *domain.User) {
	if s.keys == nil {
		return
	}
	meta := domain.KeyMeta{
		KeyID: k.ID, UserID: k.UserID, GroupID: k.GroupID,
		KeyStatus: k.Status, KeyMaxConc: k.MaxConcurrency,
		HasQuota: k.HasQuota(), Quota: k.Quota, QuotaUsed: k.QuotaUsed,
		UserStatus: u.Status, UserMaxConc: u.MaxConcurrency,
	}
	s.keys.Upsert(k.KeyRaw, meta)
}
