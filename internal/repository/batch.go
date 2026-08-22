// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"entgo.io/ent/dialect/sql/sqlgraph"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/account"
	"github.com/is7qin/c3api/internal/ent/group"
	"github.com/is7qin/c3api/internal/ent/template"
)

// ErrNotFound 批量操作中存在性检查失败（缺失 id）。
var ErrNotFound = errors.New("repository: not found")

// ErrConflict 唯一约束冲突（规则 priority/name 等；service 映射 409）。
var ErrConflict = errors.New("repository: conflict")

// --- 批量更新字段子集（nil 字段 = 不更新） ---

type TemplatePatch struct {
	Name             *string
	BaseURL          *string
	SupportedFormats *[]domain.RequestFormat
	Models           *[]string
	FormatModels     *map[domain.RequestFormat][]string
	ModelMapping     *map[string]string
}

type AccountPatch struct {
	Name           *string
	TemplateID     *int64
	UpstreamKey    *string
	// BaseURL 批量三态（C1 定死）：nil = 不变；&"" = 清空（落 NULL = 继承
	// 模板）；&非空 = 落值。
	BaseURL        *string
	Status         *domain.AccountStatus
	Weight         *int
	MaxConcurrency *int
	// GroupIDs nil = 不变；非 nil = 替换账号全部分组（含空数组 = 清空）。
	GroupIDs *[]int64
	// CooldownUntil nil = 不变；非 nil = SetCooldownUntil（管理面永不 Clear）。
	CooldownUntil *time.Time
}

type GroupPatch struct {
	Name       *string
	Visibility *domain.GroupVisibility
}

// --- 批量删除（软删：deleted_at 置值；事务，全成或全败） ---

func (r *TemplateRepo) DeleteTemplatesBatch(ctx context.Context, ids []int64) error {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck // Commit 成功后 Rollback 返回 ErrTxDone，忽略
	if err := checkTemplateExist(ctx, tx.Template.Query, ids); err != nil {
		return err
	}
	// 逐个软删 UPDATE（无 re-SELECT）；0 行命中 = check→update 竞态窗口缺 id
	//（与 errMissingID 同格式，语义同 DeleteOne 时代的 NotFound 映射）。
	for _, id := range ids {
		n, err := tx.Template.Update().Where(template.IDEQ(id)).SetDeletedAt(time.Now()).Save(ctx)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: id=%d missing", ErrNotFound, id)
		}
	}
	return tx.Commit()
}

func (r *AccountRepo) DeleteAccountsBatch(ctx context.Context, ids []int64) error {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck
	if err := checkAccountExist(ctx, tx.Account.Query, ids); err != nil {
		return err
	}
	// 逐个软删 UPDATE（无 re-SELECT）；0 行命中 = check→update 竞态窗口缺 id。
	for _, id := range ids {
		n, err := tx.Account.Update().Where(account.IDEQ(id)).SetDeletedAt(time.Now()).Save(ctx)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: id=%d missing", ErrNotFound, id)
		}
	}
	return tx.Commit()
}

func (r *GroupRepo) DeleteGroupsBatch(ctx context.Context, ids []int64) error {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck
	if err := checkGroupExist(ctx, tx.Group.Query, ids); err != nil {
		return err
	}
	// 逐个软删 UPDATE（无 re-SELECT）；0 行命中 = check→update 竞态窗口缺 id。
	for _, id := range ids {
		n, err := tx.Group.Update().Where(group.IDEQ(id)).SetDeletedAt(time.Now()).Save(ctx)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: id=%d missing", ErrNotFound, id)
		}
	}
	return tx.Commit()
}

// --- 批量更新（事务，全成或全败；Set 链只按 patch 非 nil 字段） ---

func (r *TemplateRepo) UpdateTemplatesBatch(ctx context.Context, ids []int64, p TemplatePatch) error {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck
	if err := checkTemplateExist(ctx, tx.Template.Query, ids); err != nil {
		return err
	}
	for _, id := range ids {
		u := tx.Template.UpdateOneID(id)
		if p.Name != nil {
			u = u.SetName(*p.Name)
		}
		if p.BaseURL != nil {
			u = u.SetBaseURL(*p.BaseURL)
		}
		if p.SupportedFormats != nil {
			u = u.SetSupportedFormats(formatsToStrings(*p.SupportedFormats))
		}
		if p.Models != nil {
			u = u.SetModels(*p.Models)
		}
		if p.FormatModels != nil {
			u = u.SetFormatModels(formatModelsToStrings(*p.FormatModels))
		}
		if p.ModelMapping != nil {
			u = u.SetModelMapping(*p.ModelMapping)
		}
		if _, err := u.Save(ctx); err != nil {
			if sqlgraph.IsUniqueConstraintError(err) {
				return fmt.Errorf("%w: name=%q", ErrConflict, *p.Name)
			}
			return errMissingID(err, id)
		}
	}
	return tx.Commit()
}

func (r *AccountRepo) UpdateAccountsBatch(ctx context.Context, ids []int64, p AccountPatch) error {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck
	if err := checkAccountExist(ctx, tx.Account.Query, ids); err != nil {
		return err
	}
	// 组存在性校验：循环外一次完成（空数组跳过查询——[] = 清空，无依赖组）。
	if p.GroupIDs != nil && len(*p.GroupIDs) > 0 {
		if err := checkGroupExist(ctx, tx.Group.Query, *p.GroupIDs); err != nil {
			return err
		}
	}
	for _, id := range ids {
		u := tx.Account.UpdateOneID(id)
		if p.Name != nil {
			u = u.SetName(*p.Name)
		}
		if p.TemplateID != nil {
			u = u.SetTemplateID(*p.TemplateID)
		}
		if p.UpstreamKey != nil {
			u = u.SetUpstreamKey(*p.UpstreamKey)
		}
		if p.BaseURL != nil {
			// 批量三态（C1）："" = 清空（落 NULL = 继承模板）；非空 = 落值。
			if *p.BaseURL == "" {
				u = u.ClearBaseURL()
			} else {
				u = u.SetBaseURL(*p.BaseURL)
			}
		}
		if p.Status != nil {
			u = u.SetStatus(account.Status(*p.Status))
			if *p.Status == domain.StatusActive {
				// T5 失效恢复（管理面批量——status 枚举含 active）：status→
				// active 隐含清 failed_at + last_error（与 UpdateAccount 单
				// 路径同语义——恢复动作 = 状态切换）。
				u = u.ClearFailedAt().ClearLastError()
			}
		}
		if p.Weight != nil {
			u = u.SetWeight(*p.Weight)
		}
		if p.MaxConcurrency != nil {
			u = u.SetMaxConcurrency(*p.MaxConcurrency)
		}
		if p.GroupIDs != nil {
			// ent 无 SetGroups：ClearGroups + AddGroupIDs 实现整组替换
			// （AddGroupIDs 内部 map 去重，重复 id 安全）。
			u = u.ClearGroups().AddGroupIDs(*p.GroupIDs...)
		}
		if p.CooldownUntil != nil {
			u = u.SetCooldownUntil(*p.CooldownUntil)
		}
		if _, err := u.Save(ctx); err != nil {
			return errMissingID(err, id)
		}
	}
	return tx.Commit()
}

func (r *GroupRepo) UpdateGroupsBatch(ctx context.Context, ids []int64, p GroupPatch) error {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck
	if err := checkGroupExist(ctx, tx.Group.Query, ids); err != nil {
		return err
	}
	for _, id := range ids {
		u := tx.Group.UpdateOneID(id)
		if p.Name != nil {
			u = u.SetName(*p.Name)
		}
		if p.Visibility != nil {
			u = u.SetVisibility(group.Visibility(*p.Visibility))
		}
		if _, err := u.Save(ctx); err != nil {
			if sqlgraph.IsUniqueConstraintError(err) {
				return fmt.Errorf("%w: name=%q", ErrConflict, *p.Name)
			}
			return errMissingID(err, id)
		}
	}
	return tx.Commit()
}

// errMissingID 把 per-id 执行错误映射为 ErrNotFound 包装（与 diffMissing 同格式）。
// 覆盖 check→exec 竞态窗口：存在性检查通过后、DeleteOne/UpdateOne 执行前若并发
// 请求已删同 id，ent 对 0 行删除/更新返回 NotFoundError（ent.IsNotFound）。
func errMissingID(err error, id int64) error {
	if ent.IsNotFound(err) {
		return fmt.Errorf("%w: id=%d missing", ErrNotFound, id)
	}
	return err
}

// --- 事务内存在性检查：ids 必须全部存在，否则 ErrNotFound（含缺失 id） ---

// checkTemplateExist 查询实际存在的 ids 与输入比对，拼出第一个缺失 id。
// （ent v0.14 生成的查询无 Ints/Ints64，用等价的 IDs —— 内部即
// q.Select(FieldID).Scan(&ids)。）IN 按 inChunkSize 分片：ids 超 65,535 时
// 单条 `WHERE id IN (...)` 超 PG 参数上限（service 层已限 ≤100，repo 层
// 自保护不依赖调用方约束）。
func checkTemplateExist(ctx context.Context, q func() *ent.TemplateQuery, ids []int64) error {
	return checkIDsExist(ids, func(chunk []int64) ([]int64, error) {
		return q().Where(template.IDIn(chunk...)).IDs(ctx)
	})
}

func checkAccountExist(ctx context.Context, q func() *ent.AccountQuery, ids []int64) error {
	return checkIDsExist(ids, func(chunk []int64) ([]int64, error) {
		return q().Where(account.IDIn(chunk...)).IDs(ctx)
	})
}

func checkGroupExist(ctx context.Context, q func() *ent.GroupQuery, ids []int64) error {
	return checkIDsExist(ids, func(chunk []int64) ([]int64, error) {
		return q().Where(group.IDIn(chunk...)).IDs(ctx)
	})
}

// checkIDsExist 通用存在性检查：按块逐块查询（每块新建查询——ent Where 原地
// 追加谓词，复用同一查询会跨块累加 IN）后合并 existing，再 diffMissing。
// 合并只做集合并集（diffMissing 不依赖顺序）；空 ids → 零块 → diffMissing
// 空集直接返回 nil。错误带 (chunk i/n, N ids) 上下文（评审 I-2）。
func checkIDsExist(ids []int64, each func(chunk []int64) ([]int64, error)) error {
	chunks := chunkIDs(ids, inChunkSize)
	var existing []int64
	for i, chunk := range chunks {
		got, err := each(chunk)
		if err != nil {
			return fmt.Errorf("exists check (chunk %d/%d, %d ids): %w", i+1, len(chunks), len(chunk), err)
		}
		existing = append(existing, got...)
	}
	return diffMissing(existing, ids)
}

// diffMissing 返回 ids 中不在 existing 里的第一个缺失 id 错误。
func diffMissing(existing, ids []int64) error {
	seen := make(map[int64]struct{}, len(existing))
	for _, id := range existing {
		seen[id] = struct{}{}
	}
	for _, id := range ids {
		if _, ok := seen[id]; !ok {
			return fmt.Errorf("%w: id=%d missing", ErrNotFound, id)
		}
	}
	return nil
}

