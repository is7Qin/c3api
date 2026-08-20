// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
)

// SetGroupAssignments 替换语义设置组的授予用户（PUT /api/admin/groups/{id}/
// assignments）：完整列表 = 授予结果（未列出即撤销，空数组 = 清空）。
// 幂等（Grant/Revoke 本身幂等）；用户/组必须存在且组未软删（缺失/软删 → 404，
// F3）。整个替换循环包 WithTx（S3-F2）：Grant/SetMultiplier/Revoke/组内读同一
// 事务，中途失败整体回滚——不再出现混合授予态。
// mults 可选：user_id → 该用户在该组的专属价格倍率（万分数，T3.5 修正：按
// 组——用户在不同组可有不同倍率；nil 值 = 清除为未设置 → 回退组倍率；0 =
// 免费）。mults 的 key 必须 ⊆ userIDs（未列出的用户不改动既有倍率；未知用户
// → 400）。返回生效的 user_ids 列表 + 该组各授予用户的 post-state 倍率
// （user_ids 全量，nil = 未设置；response 回显用）。
// 授予/倍率变更影响计费倍率快照 → Multipliers() 定向刷新（assignment 倍率
// 变更不依赖全量 Reload）。
func (s *Service) SetGroupAssignments(ctx context.Context, groupID int64, userIDs []int64, mults map[int64]*int) ([]int64, map[int64]*int, error) {
	if _, err := s.getGroupLive(ctx, groupID); err != nil {
		return nil, nil, err
	}
	if len(userIDs) > 100 {
		return nil, nil, ErrInvalidInput
	}
	want := make(map[int64]bool, len(userIDs))
	for _, uid := range userIDs {
		if uid <= 0 || want[uid] {
			return nil, nil, ErrInvalidInput // 非法 id / 重复
		}
		want[uid] = true
		if _, err := s.store.GetUser(ctx, uid); err != nil {
			return nil, nil, mapRepoErr(err)
		}
	}
	for uid, m := range mults {
		if !want[uid] {
			return nil, nil, ErrInvalidInput // multipliers key 必须 ∈ user_ids
		}
		if m != nil && (*m < 0 || *m > 100000) {
			return nil, nil, ErrInvalidInput // 万分数 0~100000（API 边界正常值 0~10 换算）
		}
	}
	var post map[int64]*int
	err := s.store.WithTx(ctx, func(tx repository.TxStore) error {
		cur, err := tx.ListAssignmentsByGroup(ctx, groupID)
		if err != nil {
			return err
		}
		post, err = s.applyGroupAssignments(ctx, tx, groupID, userIDs, mults, cur)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	s.inv.Multipliers()
	s.publish(ctx, notify.Change{Multipliers: true})
	if s.log != nil {
		s.log.Info("group assignments set", logx.Int64("group_id", groupID), logx.Int64("count", int64(len(userIDs))))
	}
	return userIDs, post, nil
}

// applyGroupAssignments 组维度替换核心（SetGroupAssignments / SetUserGroups
// 共用；调用方负责存在性/合法性校验与 Multipliers() 刷新；变更经 tx 面——
// S3-F2 整体回滚的前提）：cur = 组当前授予行，want = 目标授予集合（已校验
// 去重/存在/≤100），mults = 组内专属倍率更新（key ⊆ want，万分数已校验；
// nil = 清除为未设置）。幂等 Grant 新增 / SetAssignmentMultiplier 更新 /
// Revoke 撤销；返回 post-state 倍率（want 全量，未在 mults 的用户沿用旧值，
// nil = 未设置）。RevokeGroup 撤销授予不影响既有 key 使用权（语义待裁决：
// B 方案 = 撤销时停用该组该用户 key）——当前方案 A，零行为变化。
func (s *Service) applyGroupAssignments(ctx context.Context, tx repository.TxStore, groupID int64, want []int64, mults map[int64]*int, cur []*domain.GroupAssignment) (map[int64]*int, error) {
	have := make(map[int64]bool, len(cur))
	oldMult := make(map[int64]*int, len(cur))
	for _, a := range cur {
		have[a.UserID] = true
		oldMult[a.UserID] = a.PriceMultiplier
	}
	wantSet := make(map[int64]bool, len(want))
	for _, uid := range want {
		wantSet[uid] = true
		if !have[uid] {
			if err := tx.GrantGroup(ctx, groupID, uid); err != nil {
				return nil, err
			}
		}
	}
	for uid, m := range mults {
		if err := tx.SetAssignmentMultiplier(ctx, groupID, uid, m); err != nil {
			return nil, err
		}
	}
	for _, a := range cur {
		if !wantSet[a.UserID] {
			if err := tx.RevokeGroup(ctx, groupID, a.UserID); err != nil {
				return nil, err
			}
		}
	}
	post := make(map[int64]*int, len(want))
	for _, uid := range want {
		if m, ok := mults[uid]; ok {
			post[uid] = m
			continue
		}
		post[uid] = oldMult[uid]
	}
	return post, nil
}

// ListGroupsForUser 用户可选组列表（public 全部 + 已授予 private；/api/user/groups
// 只读，key 创建时选组）。
func (s *Service) ListGroupsForUser(ctx context.Context, userID int64) ([]*domain.Group, error) {
	return s.store.ListGroupsForUser(ctx, userID)
}
