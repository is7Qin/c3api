// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"fmt"
	"time"

	"entgo.io/ent/dialect/sql/sqlgraph"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/managementkey"
)

// ManagementKeyRepo 管理 API key（前缀 mk-，独立表，spec 2026-10-09）持久化。
type ManagementKeyRepo struct {
	client *ent.Client
}

// ManagementKeyPatch 管理 key 更新补丁（对齐 KeyPatch 范式）：显式字段 =
// 请求显式提供的字段；nil = 不改。UserID 为 owner 作用域（越域 → ErrNotFound）。
type ManagementKeyPatch struct {
	UserID int64 // owner（作用域）
	ID     int64
	Name   *string
	Status *domain.ManagementKeyStatus
}

// CreateManagementKey 创建管理 key（明文 key_raw 唯一——重复 → ErrConflict）。
func (r *ManagementKeyRepo) CreateManagementKey(ctx context.Context, k *domain.ManagementKey) (*domain.ManagementKey, error) {
	row, err := r.client.ManagementKey.Create().
		SetUserID(k.UserID).
		SetName(k.Name).
		SetKeyRaw(k.KeyRaw).
		SetStatus(managementkey.Status(k.Status)).
		Save(ctx)
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: key_raw=%q", ErrConflict, k.KeyRaw)
		}
		return nil, err
	}
	return toDomainManagementKey(row), nil
}

// ListManagementKeysByUser 列某 owner 的管理 key（软删过滤——已删不可见；
// 明文回显，与客户端 key 一致）。写入低频，id 升序。
func (r *ManagementKeyRepo) ListManagementKeysByUser(ctx context.Context, userID int64) ([]*domain.ManagementKey, error) {
	rows, err := r.client.ManagementKey.Query().
		Where(managementkey.UserIDEQ(userID), managementkey.DeletedAtIsNil()).
		Order(ent.Asc(managementkey.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.ManagementKey, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainManagementKey(row))
	}
	return out, nil
}

// UpdateManagementKey 按 patch 更新 name/status（owner 作用域：id + user_id 双
// 条件 + 未删过滤，越域/缺失/已删 → ErrNotFound）；仅 Set 非 nil 列。status=disabled
// 软禁用（可再启用）；已软删的行不可更新——避免 PUT 把已删 key 复活进鉴权快照。
// 返回更新后行。
func (r *ManagementKeyRepo) UpdateManagementKey(ctx context.Context, p *ManagementKeyPatch) (*domain.ManagementKey, error) {
	upd := r.client.ManagementKey.Update().
		Where(managementkey.IDEQ(p.ID), managementkey.UserIDEQ(p.UserID), managementkey.DeletedAtIsNil())
	if p.Name != nil {
		upd.SetName(*p.Name)
	}
	if p.Status != nil {
		upd.SetStatus(managementkey.Status(*p.Status))
	}
	n, err := upd.Save(ctx)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("%w: id=%d missing", ErrNotFound, p.ID)
	}
	row, err := r.client.ManagementKey.Query().
		Where(managementkey.IDEQ(p.ID), managementkey.DeletedAtIsNil()).
		Only(ctx)
	if err != nil {
		return nil, errMissingID(err, p.ID)
	}
	return toDomainManagementKey(row), nil
}

// DeleteManagementKey 软删除（owner 作用域：id + user_id 双条件 + 未删过滤；
// 越域/缺失/已删 → ErrNotFound）。返回被删明文（Auth 快照增量清理用）。行保留
// 留审计；鉴权快照按 deleted_at IS NULL 过滤 → 已删即时 401。
func (r *ManagementKeyRepo) DeleteManagementKey(ctx context.Context, userID, id int64) (string, error) {
	row, err := r.client.ManagementKey.Query().
		Where(managementkey.IDEQ(id), managementkey.UserIDEQ(userID), managementkey.DeletedAtIsNil()).
		Only(ctx)
	if err != nil {
		return "", errMissingID(err, id)
	}
	if _, err := r.client.ManagementKey.UpdateOneID(id).SetDeletedAt(time.Now()).Save(ctx); err != nil {
		return "", errMissingID(err, id)
	}
	return row.KeyRaw, nil
}

// LoadManagementKeys 构建 Auth 管理 key 鉴权快照：key_raw（明文）→
// ManagementKeyMeta（软删过滤 + 按 id 分块——对照 KeyRepo.LoadKeys，规避 PG
// 参数上限）。热路径数据源（reload 时一次查询；请求路径零 DB）。
func (r *ManagementKeyRepo) LoadManagementKeys(ctx context.Context) (map[string]domain.ManagementKeyMeta, error) {
	ids, err := r.client.ManagementKey.Query().
		Where(managementkey.DeletedAtIsNil()).
		Order(ent.Asc(managementkey.FieldID)).
		IDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("load management keys (scan ids): %w", err)
	}
	out := make(map[string]domain.ManagementKeyMeta, len(ids))
	chunks := chunkIDs(ids, inChunkSize)
	for i, chunk := range chunks {
		rows, err := r.client.ManagementKey.Query().
			Where(managementkey.IDIn(chunk...)).
			All(ctx)
		if err != nil {
			return nil, fmt.Errorf("load management keys (chunk %d/%d, %d ids): %w", i+1, len(chunks), len(chunk), err)
		}
		for _, row := range rows {
			out[row.KeyRaw] = domain.ManagementKeyMeta{
				ID:     row.ID,
				UserID: row.UserID,
				Status: domain.ManagementKeyStatus(row.Status),
			}
		}
	}
	return out, nil
}
