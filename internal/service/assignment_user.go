// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"slices"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
)

// 用户维度分组操作（GET/PUT /api/admin/users/{id}/groups）+ 组授予读取
// （GET /api/admin/groups/{id}/assignments）：与组维度 SetGroupAssignments 对称，
// 复用 applyGroupAssignments 组维度替换核心。

// collectAssignmentIDs 授予行 → id 列表 + 专属倍率 map（mults 只含有专属倍率
// 的行：nil/缺省 = 未设置 → 用组倍率，响应省略）。idOf 决定维度：组视角取
// UserID、用户视角取 GroupID。
func collectAssignmentIDs(rows []*domain.GroupAssignment, idOf func(a *domain.GroupAssignment) int64) ([]int64, map[int64]*int) {
	ids := make([]int64, 0, len(rows))
	var mults map[int64]*int
	for _, a := range rows {
		id := idOf(a)
		ids = append(ids, id)
		if a.PriceMultiplier != nil {
			if mults == nil {
				mults = make(map[int64]*int, len(rows))
			}
			mults[id] = a.PriceMultiplier
		}
	}
	return ids, mults
}

// GetGroupAssignments 读取组当前授予用户与专属倍率（GET /api/admin/groups/{id}/
// assignments；组缺失 → 404）。mults 只含该组有专属倍率的用户（nil/缺省 =
// 未设置 → 用组倍率）。
func (s *Service) GetGroupAssignments(ctx context.Context, groupID int64) ([]int64, map[int64]*int, error) {
	if _, err := s.store.GetGroup(ctx, groupID); err != nil {
		return nil, nil, mapRepoErr(err)
	}
	rows, err := s.store.ListAssignmentsByGroup(ctx, groupID)
	if err != nil {
		return nil, nil, err
	}
	ids, mults := collectAssignmentIDs(rows, func(a *domain.GroupAssignment) int64 { return a.UserID })
	return ids, mults, nil
}

// GetUserGroups 读取用户被授予的组与各专属倍率（GET /api/admin/users/{id}/groups；
// 用户缺失 → 404）。mults 只含该用户有专属倍率的组。
func (s *Service) GetUserGroups(ctx context.Context, userID int64) ([]int64, map[int64]*int, error) {
	if _, err := s.store.GetUser(ctx, userID); err != nil {
		return nil, nil, mapRepoErr(err)
	}
	rows, err := s.store.ListAssignmentsByUser(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	ids, mults := collectAssignmentIDs(rows, func(a *domain.GroupAssignment) int64 { return a.GroupID })
	return ids, mults, nil
}

// SetUserGroups 替换语义设置用户的授予组（PUT /api/admin/users/{id}/groups）：
// group_ids = 完整授予组列表（未列出即撤销，空数组 = 清空）。multipliers 仅对
// group_ids 中的组生效（key 必须 ∈ group_ids，否则 400；null = 清除为未设置 →
// 回退组倍率；未列出的组 = 撤销，谈不上倍率）。校验：用户存在（404）、组存在
// 且未软删（404，F3 逐组同校验）、非法/重复 id 与越界倍率（400）；组数上限与
// SetGroupAssignments 对齐 ≤100。整个替换循环（含逐组读）包 WithTx（S3-F2）：
// 逐组读与写同一事务，中途失败整体回滚——不再出现混合授予态。实现按组复用组
// 维度替换核心：对每个目标组读现成员 → 现成员 ∪ {userID} 作为新授予集合
// （SetAssignmentMultiplier 只传该用户，其他成员不传 = 沿用现倍率，互不影响）；
// 不在 group_ids 的当前授予组逐个 RevokeGroup（组本身不动，组内其他用户保留）。
// 返回生效的 group_ids + 各授予组 post-state 倍率。
func (s *Service) SetUserGroups(ctx context.Context, userID int64, groupIDs []int64, mults map[int64]*int) ([]int64, map[int64]*int, error) {
	if _, err := s.store.GetUser(ctx, userID); err != nil {
		return nil, nil, mapRepoErr(err)
	}
	if len(groupIDs) > 100 {
		return nil, nil, ErrInvalidInput
	}
	want := make(map[int64]bool, len(groupIDs))
	for _, gid := range groupIDs {
		if gid <= 0 || want[gid] {
			return nil, nil, ErrInvalidInput // 非法 id / 重复
		}
		want[gid] = true
		if _, err := s.getGroupLive(ctx, gid); err != nil {
			return nil, nil, err
		}
	}
	for gid, m := range mults {
		if !want[gid] {
			return nil, nil, ErrInvalidInput // multipliers key 必须 ∈ group_ids
		}
		if m != nil && (*m < 0 || *m > 100000) {
			return nil, nil, ErrInvalidInput // 万分数 0~100000（API 边界正常值 0~10 换算）
		}
	}
	var post map[int64]*int
	err := s.store.WithTx(ctx, func(tx repository.TxStore) error {
		cur, err := tx.ListAssignmentsByUser(ctx, userID)
		if err != nil {
			return err
		}
		oldMult := make(map[int64]*int, len(cur))
		for _, a := range cur {
			oldMult[a.GroupID] = a.PriceMultiplier
		}
		// 每个目标组做组维度替换：现成员 ∪ {userID}；该用户倍率按 mults 更新
		// （组内仅该用户有专属倍率，其他成员不传 = 沿用现倍率）
		for gid := range want {
			members, err := tx.ListAssignmentsByGroup(ctx, gid)
			if err != nil {
				return err
			}
			union := make([]int64, 0, len(members)+1)
			for _, a := range members {
				union = append(union, a.UserID)
			}
			if !slices.Contains(union, userID) {
				union = append(union, userID)
			}
			var m map[int64]*int
			if v, ok := mults[gid]; ok {
				m = map[int64]*int{userID: v}
			}
			if _, err := s.applyGroupAssignments(ctx, tx, gid, union, m, members); err != nil {
				return err
			}
		}
		// 撤销不在 group_ids 的当前授予组（撤销行 = 该用户在该组的授予删除）
		for _, a := range cur {
			if !want[a.GroupID] {
				if err := tx.RevokeGroup(ctx, a.GroupID, userID); err != nil {
					return err
				}
			}
		}
		// post-state 倍率：group_ids 全量（未在 mults 的组沿用旧值；新授予未设 → nil）
		post = make(map[int64]*int, len(groupIDs))
		for _, gid := range groupIDs {
			if m, ok := mults[gid]; ok {
				post[gid] = m
				continue
			}
			post[gid] = oldMult[gid]
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	s.inv.Multipliers()
	s.publish(ctx, notify.Change{Multipliers: true}) // 倍率/授予变更跨实例传播（评审 M-1：组维度写路径已有，用户维度写补齐）
	if s.log != nil {
		s.log.Info("user groups set", logx.Int64("user_id", userID), logx.Int64("count", int64(len(groupIDs))))
	}
	return groupIDs, post, nil
}
