// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"entgo.io/ent/dialect/sql/sqlgraph"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/rule"
)

// RuleStore 规则存储接口；service 层通过 Repos.Rules 使用。
type RuleStore interface {
	ListRules(ctx context.Context, enabled *bool) ([]domain.Rule, error) // nil = 全部；priority 升序
	CreateRule(ctx context.Context, r domain.Rule) (int64, error)
	UpdateRule(ctx context.Context, r domain.Rule) error
	DeleteRule(ctx context.Context, id int64) error
	DeleteRulesBatch(ctx context.Context, ids []int64) error
	CountRules(ctx context.Context) (int64, error)
}

type RuleRepo struct{ client *ent.Client }

var _ RuleStore = (*RuleRepo)(nil)

func (r *RuleRepo) ListRules(ctx context.Context, enabled *bool) ([]domain.Rule, error) {
	// 软删除：已删规则不加载（规则引擎 Reload 消费同路径——过滤随本方法生效）。
	pred := r.client.Rule.Query().Where(rule.DeletedAtIsNil())
	if enabled != nil {
		pred = pred.Where(rule.Enabled(*enabled))
	}
	rows, err := pred.Order(ent.Asc(rule.FieldPriority)).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Rule, 0, len(rows))
	for _, row := range rows {
		out = append(out, *toDomainRule(row))
	}
	return out, nil
}

func (r *RuleRepo) CreateRule(ctx context.Context, rl domain.Rule) (int64, error) {
	row, err := r.client.Rule.Create().
		SetName(rl.Name).SetEnabled(rl.Enabled).SetPriority(rl.Priority).
		SetWhen(whenToMap(rl.When)).SetThen(thenToMap(rl.Then)).
		Save(ctx)
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return 0, fmt.Errorf("%w: priority=%d or name=%q", ErrConflict, rl.Priority, rl.Name)
		}
		return 0, err
	}
	return row.ID, nil
}

func (r *RuleRepo) UpdateRule(ctx context.Context, rl domain.Rule) error {
	_, err := r.client.Rule.UpdateOneID(rl.ID).
		SetName(rl.Name).SetEnabled(rl.Enabled).SetPriority(rl.Priority).
		SetWhen(whenToMap(rl.When)).SetThen(thenToMap(rl.Then)).
		Save(ctx)
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return fmt.Errorf("%w: priority=%d or name=%q", ErrConflict, rl.Priority, rl.Name)
		}
		return err
	}
	return nil
}

// DeleteRule 软删除：deleted_at 置值（行保留留审计；规则引擎重载按
// deleted_at IS NULL 过滤 → 已删规则不加载，GET 单个仍可查已删项）。bulk
// Update（无 re-SELECT）单语句；0 行命中 = 缺 id → ErrNotFound（同 errMissingID 格式）。
func (r *RuleRepo) DeleteRule(ctx context.Context, id int64) error {
	n, err := r.client.Rule.Update().Where(rule.IDEQ(id)).SetDeletedAt(time.Now()).Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: id=%d missing", ErrNotFound, id)
	}
	return nil
}

// DeleteRulesBatch 批量软删除规则（事务，全成或全败；与 templates/accounts 同构）。
func (r *RuleRepo) DeleteRulesBatch(ctx context.Context, ids []int64) error {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck // Commit 成功后 Rollback 返回 ErrTxDone，忽略
	if err := checkRuleExist(ctx, tx.Rule.Query, ids); err != nil {
		return err
	}
	// 逐个软删 UPDATE（无 re-SELECT）；0 行命中 = check→update 竞态窗口缺 id。
	for _, id := range ids {
		n, err := tx.Rule.Update().Where(rule.IDEQ(id)).SetDeletedAt(time.Now()).Save(ctx)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: id=%d missing", ErrNotFound, id)
		}
	}
	return tx.Commit()
}

// checkRuleExist 事务内存在性检查：ids 必须全部存在，否则 ErrNotFound（含第一个缺失 id）。
// IN 按 inChunkSize 分片（与 batch 三检查同构，repo 层不依赖 service 的 ≤100 约束）。
func checkRuleExist(ctx context.Context, q func() *ent.RuleQuery, ids []int64) error {
	return checkIDsExist(ids, func(chunk []int64) ([]int64, error) {
		return q().Where(rule.IDIn(chunk...)).IDs(ctx)
	})
}

// CountRules 规则总数（seedRules 判定"是否全删"用）。不过滤软删 =
// 防复活语义锚定（F6）：全删后软删行仍被计数 → 不重新 seed；若改过滤，全删
// 即空表 → 下次启动重新 seed = 用户明确删除的规则复活。
func (r *RuleRepo) CountRules(ctx context.Context) (int64, error) {
	n, err := r.client.Rule.Query().Count(ctx)
	if err != nil {
		return 0, err
	}
	return int64(n), nil
}

func toDomainRule(row *ent.Rule) *domain.Rule {
	return &domain.Rule{
		ID: row.ID, Name: row.Name, Enabled: row.Enabled, Priority: row.Priority,
		When: whenFromMap(row.When), Then: thenFromMap(row.Then),
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, DeletedAt: row.DeletedAt,
	}
}

// whenToMap/thenToMap 领域 when/then → ent JSON 字段值；whenFromMap/thenFromMap 反向。
// 用 json 标签 round-trip（nil 指针字段自然省略、整数精度无损），避免逐字段手写互转。
func whenToMap(w domain.RuleWhen) map[string]any {
	b, _ := json.Marshal(w)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}

func whenFromMap(m map[string]any) domain.RuleWhen {
	b, _ := json.Marshal(m)
	var w domain.RuleWhen
	_ = json.Unmarshal(b, &w)
	return w
}

func thenToMap(t domain.RuleThen) map[string]any {
	b, _ := json.Marshal(t)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}

func thenFromMap(m map[string]any) domain.RuleThen {
	b, _ := json.Marshal(m)
	var t domain.RuleThen
	_ = json.Unmarshal(b, &t)
	return t
}
