// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"fmt"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/group"
	"github.com/is7qin/c3api/internal/ent/groupassignment"
)

// GroupAssignmentRepo private 组授予（用户 ↔ 组多对多）。
type GroupAssignmentRepo struct{ client *ent.Client }

// Grant 授予；联合唯一 (group_id, user_id) 兜底幂等（重复授予不报错）。
func (r *GroupAssignmentRepo) Grant(ctx context.Context, groupID, userID int64) error {
	_, err := r.client.GroupAssignment.Create().
		SetGroupID(groupID).
		SetUserID(userID).
		OnConflictColumns("group_id", "user_id").
		Ignore().
		ID(ctx)
	return err
}

// Revoke 撤销（不存在时幂等成功）。
func (r *GroupAssignmentRepo) Revoke(ctx context.Context, groupID, userID int64) error {
	_, err := r.client.GroupAssignment.Delete().
		Where(groupassignment.GroupIDEQ(groupID), groupassignment.UserIDEQ(userID)).
		Exec(ctx)
	return err
}

// SetMultiplier 设置/清除该用户在该组的专属价格倍率（T3.5 修正：按组——用户
// 在不同组可有不同倍率）：m = nil → 清除为未设置（ClearPriceMultiplier，回退
// 组倍率）；非 nil → 写万分数（0 = 免费）。授予行必须已存在（service 先 Grant
// 再 SetMultiplier）；缺失 → ErrNotFound。
func (r *GroupAssignmentRepo) SetMultiplier(ctx context.Context, groupID, userID int64, m *int) error {
	q := r.client.GroupAssignment.Update().
		Where(groupassignment.GroupIDEQ(groupID), groupassignment.UserIDEQ(userID))
	if m == nil {
		q = q.ClearPriceMultiplier()
	} else {
		q = q.SetPriceMultiplier(*m)
	}
	n, err := q.Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: group_id=%d user_id=%d", ErrNotFound, groupID, userID)
	}
	return nil
}

func (r *GroupAssignmentRepo) ListByUser(ctx context.Context, userID int64) ([]*domain.GroupAssignment, error) {
	rows, err := r.client.GroupAssignment.Query().
		Where(groupassignment.UserIDEQ(userID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.GroupAssignment, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainGroupAssignment(row))
	}
	return out, nil
}

func (r *GroupAssignmentRepo) ListByGroup(ctx context.Context, groupID int64) ([]*domain.GroupAssignment, error) {
	rows, err := r.client.GroupAssignment.Query().
		Where(groupassignment.GroupIDEQ(groupID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.GroupAssignment, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainGroupAssignment(row))
	}
	return out, nil
}

// ListGroupsForUser 用户可选组：public 全部 + 已授予的 private（/api/user/groups
// 只读列表；软删除：已删组不进可选列表）。
func (r *GroupAssignmentRepo) ListGroupsForUser(ctx context.Context, userID int64) ([]*domain.Group, error) {
	rows, err := r.client.Group.Query().
		Where(group.DeletedAtIsNil(), group.Or(
			group.VisibilityEQ(group.VisibilityPublic),
			group.HasAssignmentsWith(groupassignment.UserIDEQ(userID)),
		)).
		Order(ent.Asc(group.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.Group, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainGroup(row))
	}
	return out, nil
}
