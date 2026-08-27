// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"fmt"
	"time"

	"entgo.io/ent/dialect"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/account"
)

type AccountRepo struct {
	client *ent.Client
	driver dialect.Driver
}

func (r *AccountRepo) CreateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	var row *ent.Account
	err := withWriteTx(ctx, r.driver, func(client *ent.Client, driver dialect.Driver) error {
		if err := lockTemplateWrites(ctx, driver, []int64{a.TemplateID}); err != nil {
			return err
		}
		templates, err := loadTemplates(ctx, client, []int64{a.TemplateID})
		if err != nil {
			return err
		}
		if err := validateCodexAccountBaseURL(templates[a.TemplateID], a.BaseURL); err != nil {
			return err
		}
		status := a.Status
		if status == "" {
			status = domain.StatusActive
		}
		b := client.Account.Create().
			SetName(a.Name).SetTemplateID(a.TemplateID).
			SetNillableBaseURL(a.BaseURL).
			SetUpstreamKey(a.UpstreamKey).
			SetStatus(account.Status(status)).
			SetWeight(a.Weight).SetMaxConcurrency(a.MaxConcurrency)
		if a.FailureSource != nil {
			b = b.SetFailureSource(*a.FailureSource)
		}
		if a.CacheDomain != nil {
			b = b.SetCacheDomain(*a.CacheDomain)
		}
		if a.UpstreamCostMultiplierBp != 0 {
			b = b.SetUpstreamCostMultiplierBp(a.UpstreamCostMultiplierBp)
		}
		if a.LifecycleRevision != 0 {
			b = b.SetLifecycleRevision(a.LifecycleRevision)
		}
		if !a.Enabled && (a.LifecycleRevision != 0 || a.UpstreamCostMultiplierBp != 0 || a.FailureSource != nil || a.CacheDomain != nil) {
			b = b.SetEnabled(false)
		}
		row, err = b.Save(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return toDomainAccount(row), nil
}

func (r *AccountRepo) GetAccount(ctx context.Context, id int64) (*domain.Account, error) {
	row, err := r.client.Account.Get(ctx, id)
	if err != nil {
		return nil, errMissingID(err, id)
	}
	return toDomainAccount(row), nil
}

func (r *AccountRepo) ListAccounts(ctx context.Context, q ListQuery) ([]*domain.Account, int64, error) {
	// 软删除：列表默认过滤已删（count 同谓词——pred 复用）；GET 单个不过滤。
	pred := r.client.Account.Query().Where(account.DeletedAtIsNil())
	if q.Name != "" {
		pred = pred.Where(account.NameContainsFold(q.Name))
	}
	if len(q.StatusList) > 0 {
		sts, err := toAccountStatusList(q.StatusList)
		if err != nil {
			return nil, 0, err
		}
		pred = pred.Where(account.StatusIn(sts...))
	}
	if q.TemplateID > 0 {
		pred = pred.Where(account.TemplateIDEQ(q.TemplateID))
	}
	total, err := pred.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	order, err := q.sortOrder(accountSortFields)
	if err != nil {
		return nil, 0, err
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	rows, err := pred.WithTemplate().Order(order).Offset(q.Offset).Limit(q.Limit).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*domain.Account, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainAccount(row))
	}
	return out, int64(total), nil
}

// toAccountStatusList 校验并转换多值 status 筛选。ent 生成的枚举没有 Valid() 方法
// （只有 StatusValidator），对照枚举常量校验；非法值返回 error（handler 已校验，repo 兜底）。
func toAccountStatusList(list []string) ([]account.Status, error) {
	out := make([]account.Status, 0, len(list))
	for _, s := range list {
		st := account.Status(s)
		switch st {
		case account.StatusActive, account.StatusUnhealthy, account.Status429, account.StatusDisabled:
		default:
			return nil, fmt.Errorf("invalid account status %q", s)
		}
		out = append(out, st)
	}
	return out, nil
}

func (r *AccountRepo) UpdateAccount(ctx context.Context, a *domain.Account, cooldownUntil *time.Time) (*domain.Account, error) {
	var row *ent.Account
	err := withWriteTx(ctx, r.driver, func(client *ent.Client, driver dialect.Driver) error {
		locked, err := lockAccountsForUpdate(ctx, driver, []int64{a.ID})
		if err != nil {
			return err
		}
		templateIDs := []int64{locked[0].templateID, a.TemplateID}
		if err := lockTemplateWrites(ctx, driver, templateIDs); err != nil {
			return err
		}
		templates, err := loadTemplates(ctx, client, templateIDs)
		if err != nil {
			return err
		}
		if err := validateCodexAccountBaseURL(templates[a.TemplateID], a.BaseURL); err != nil {
			return err
		}
		u := client.Account.UpdateOneID(a.ID).
			SetName(a.Name).SetTemplateID(a.TemplateID).
			SetUpstreamKey(a.UpstreamKey).
			SetWeight(a.Weight).SetMaxConcurrency(a.MaxConcurrency).
			SetStatus(account.Status(a.Status)).
			SetEnabled(a.Enabled).
			SetUpstreamCostMultiplierBp(a.UpstreamCostMultiplierBp)
			// 账号级 base_url 全量替换语义（对齐其余字段的「全字段 Set」现状——PUT 是
			// 全量替换：nil = 继承模板 → ClearBaseURL 清空既有覆盖；非空 → SetBaseURL。
			// SetNillableBaseURL(nil) 是 no-op，无法表达「清空」，故显式分写）。
		if a.BaseURL != nil {
			u = u.SetBaseURL(*a.BaseURL)
		} else {
			u = u.ClearBaseURL()
		}
		if a.Status == domain.StatusActive {
			// T5 失效恢复（管理面，P2-2 定死方案 b）：status→active 隐含清
			// failed_at + last_error（恢复动作 = 状态切换，不做 openapi 字段扩展；
			// 清字段语义对齐既有 ClearLastError——active ⇒ 未失效不变量）。
			u = u.ClearFailedAt().ClearLastError()
		}
		if a.CacheDomain != nil {
			u = u.SetCacheDomain(*a.CacheDomain)
		} else {
			u = u.ClearCacheDomain()
		}
		if cooldownUntil != nil {
			u = u.SetCooldownUntil(*cooldownUntil)
		}
		row, err = u.Save(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return toDomainAccount(row), nil
}

// DeleteAccount 软删除：deleted_at 置值（行保留留审计；调度器快照按
// deleted_at IS NULL 过滤，GET 单个仍可查已删项）。bulk Update（无 re-SELECT）
// 单语句；0 行命中 = 缺 id → ErrNotFound（与 errMissingID 同格式）。
func (r *AccountRepo) DeleteAccount(ctx context.Context, id int64) error {
	n, err := r.client.Account.Update().Where(account.IDEQ(id)).SetDeletedAt(time.Now()).Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: id=%d missing", ErrNotFound, id)
	}
	return nil
}

// SetAccountGroups 替换账号的全部分组（替换语义：给定集合 = 账号全部分组；
// 空数组 = 清空）。组 id 先做存在性校验（缺失 → ErrNotFound 含 id）；
// 账号缺 id → ErrNotFound（errMissingID）。
func (r *AccountRepo) SetAccountGroups(ctx context.Context, accountID int64, groupIDs []int64) error {
	if len(groupIDs) > 0 {
		if err := checkGroupExist(ctx, r.client.Group.Query, groupIDs); err != nil {
			return err
		}
	}
	_, err := r.client.Account.UpdateOneID(accountID).
		ClearGroups().
		AddGroupIDs(groupIDs...).
		Save(ctx)
	return errMissingID(err, accountID)
}

// GetAccountGroups 读取账号的全部分组 id（编辑回显专用端点数据源；
// 不 eager-load，GetAccount/ListAccounts 读路径不加 groups edge）。
// 账号是否存在由调用方（service.GetAccountGroups 先 GetAccount）负责——
// 本方法对不存在账号返回空集而非错误。
func (r *AccountRepo) GetAccountGroups(ctx context.Context, accountID int64) ([]int64, error) {
	return r.client.Account.Query().
		Where(account.ID(accountID)).
		QueryGroups().
		IDs(ctx)
}

// UpdateAccountStatus 满足 scheduler.Loader：状态/冷却/错误信息回写；weight 非 nil
// 时一并更新（规则引擎权重动作，nil = 不动 weight）。status=active 时扩展清
// failed_at（T5 失效恢复——"active ⇒ 未失效"不变量：ClearLastError 既有语义
// 同处扩展 failed_at 一并清除；调度器在账号失效后快照为 disabled，规则
// apply/MarkResult 防复活守卫拦截 active 回写，此处清除不会误伤在途失效）。
func (r *AccountRepo) UpdateAccountStatus(ctx context.Context, id int64, status domain.AccountStatus, cooldownUntil *time.Time, lastError *string, weight *int) error {
	u := r.client.Account.UpdateOneID(id).SetStatus(account.Status(status))
	if cooldownUntil != nil {
		u = u.SetCooldownUntil(*cooldownUntil)
	} else {
		u = u.ClearCooldownUntil()
	}
	if lastError != nil {
		u = u.SetLastError(*lastError)
	} else {
		u = u.ClearLastError()
	}
	if status == domain.StatusActive {
		u = u.ClearFailedAt()
	}
	if weight != nil {
		u = u.SetWeight(*weight)
	}
	_, err := u.Save(ctx)
	return err
}

// SetAccountFailed 幂等写账号失效（SDK 接入 T1——统一失效回调处理链第一步）：
// failed_at + last_error（失效原因文本，复用既有 last_error——用户裁决
// 2026-08-13：两原因字段并存会漂移；失效后账号摘除不再被调度 → caller.go
// 的普通失败写点不会覆盖失效原因，复用安全）首写生效——failed_at 已置（首次
// 上报）→ 0 行不覆盖（保持首次失效时刻与原因；重复上报不重复写；T5 恢复由
// 管理面清 failed_at + last_error）。**空 reason 不清旧值**（P3-2 评审：
// 空原因上报不触碰既有 last_error——保持"最近错误"审计语义；调度回写携带的
// 快照旧值与 DB 一致，不互相覆盖）。不触碰 status（与 disabled 语义分离：
// disabled = 管理面手动禁用；调度摘除走 scheduler.FailAccount 经 loader 落库）。
// 账号不存在 → 0 行不报错（审计性写入，无对象可写；调度摘除亦会因快照外账号 no-op）。
func (r *AccountRepo) SetAccountFailed(ctx context.Context, id int64, failedAt time.Time, reason string) error {
	u := r.client.Account.Update().
		Where(account.IDEQ(id), account.FailedAtIsNil()).
		SetFailedAt(failedAt)
	if reason != "" {
		u = u.SetLastError(reason)
	}
	_, err := u.Save(ctx)
	return err
}

// ErrStaleRevision CAS 失效：期望 revision 已过期。
var ErrStaleRevision = fmt.Errorf("%w: stale lifecycle_revision", ErrConflict)

// FailAccountCAS 生命周期 fenced 失效：CAS expectedRevision 并原子 +1，设置
// failed_at/last_error/failure_source。0 行命中 → stale（ErrStaleRevision）。
func (r *AccountRepo) FailAccountCAS(ctx context.Context, id int64, expectedRevision int64, source string, failedAt time.Time, reason string) error {
	u := r.client.Account.Update().
		Where(account.IDEQ(id), account.LifecycleRevisionEQ(expectedRevision)).
		SetFailedAt(failedAt).
		SetFailureSource(source).
		SetLifecycleRevision(expectedRevision + 1)
	if reason != "" {
		u = u.SetLastError(reason)
	}
	n, err := u.Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: id=%d expected revision %d stale", ErrStaleRevision, id, expectedRevision)
	}
	return nil
}

// RecoverAccountCAS 生命周期 fenced 恢复：CAS expectedRevision 并 +1，清除
// failed_at/last_error/failure_source。0 行 → stale 或未失效（no-op 视为 stale）。
func (r *AccountRepo) RecoverAccountCAS(ctx context.Context, id int64, expectedRevision int64) error {
	n, err := r.client.Account.Update().
		Where(account.IDEQ(id), account.LifecycleRevisionEQ(expectedRevision)).
		SetLifecycleRevision(expectedRevision + 1).
		ClearFailedAt().ClearLastError().ClearFailureSource().
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: id=%d expected revision %d stale", ErrStaleRevision, id, expectedRevision)
	}
	return nil
}

// SetAccountEnabledCAS 切换 enabled 且 CAS fencing +1。Enable 不清 failure（Task1 不变量）。
func (r *AccountRepo) SetAccountEnabledCAS(ctx context.Context, id int64, expectedRevision int64, enabled bool) error {
	n, err := r.client.Account.Update().
		Where(account.IDEQ(id), account.LifecycleRevisionEQ(expectedRevision)).
		SetEnabled(enabled).
		SetLifecycleRevision(expectedRevision + 1).
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: id=%d expected revision %d stale", ErrStaleRevision, id, expectedRevision)
	}
	return nil
}

// ReplaceAccountCredentialCAS 管理员替换凭据/密钥/base_url 且 CAS +1。SDK 内部刷新不调此方法。
func (r *AccountRepo) ReplaceAccountCredentialCAS(ctx context.Context, id int64, expectedRevision int64, newKey string, newBaseURL *string) error {
	u := r.client.Account.Update().
		Where(account.IDEQ(id), account.LifecycleRevisionEQ(expectedRevision)).
		SetUpstreamKey(newKey).
		SetLifecycleRevision(expectedRevision + 1)
	if newBaseURL != nil {
		u = u.SetBaseURL(*newBaseURL)
	} else {
		u = u.ClearBaseURL()
	}
	n, err := u.Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: id=%d expected revision %d stale", ErrStaleRevision, id, expectedRevision)
	}
	return nil
}

// UpdateAccountCostMultiplierCAS 管理员更新采购倍率且 CAS +1。
func (r *AccountRepo) UpdateAccountCostMultiplierCAS(ctx context.Context, id int64, expectedRevision int64, multiplier int) error {
	n, err := r.client.Account.Update().
		Where(account.IDEQ(id), account.LifecycleRevisionEQ(expectedRevision)).
		SetUpstreamCostMultiplierBp(multiplier).
		SetLifecycleRevision(expectedRevision + 1).
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: id=%d expected revision %d stale", ErrStaleRevision, id, expectedRevision)
	}
	return nil
}

// UpdateAccountCacheDomainCAS 管理员更新缓存域且 CAS +1。
func (r *AccountRepo) UpdateAccountCacheDomainCAS(ctx context.Context, id int64, expectedRevision int64, domain *string) error {
	u := r.client.Account.Update().
		Where(account.IDEQ(id), account.LifecycleRevisionEQ(expectedRevision)).
		SetLifecycleRevision(expectedRevision + 1)
	if domain != nil {
		u = u.SetCacheDomain(*domain)
	} else {
		u = u.ClearCacheDomain()
	}
	n, err := u.Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: id=%d expected revision %d stale", ErrStaleRevision, id, expectedRevision)
	}
	return nil
}

// UpdateAccountCAS 管理员全量更新（fenced）：CAS expectedRevision 并原子 +1，更新全部可变字段。
// 用于 PUT credential/baseURL 替换及恢复场景，保证单条原子条件更新。
func (r *AccountRepo) UpdateAccountCAS(ctx context.Context, a *domain.Account, expectedRevision int64, cooldownUntil *time.Time) (*domain.Account, error) {
	u := r.client.Account.Update().
		Where(account.IDEQ(a.ID), account.LifecycleRevisionEQ(expectedRevision)).
		SetName(a.Name).SetTemplateID(a.TemplateID).
		SetUpstreamKey(a.UpstreamKey).
		SetWeight(a.Weight).SetMaxConcurrency(a.MaxConcurrency).
		SetStatus(account.Status(a.Status)).
		SetEnabled(a.Enabled).
		SetUpstreamCostMultiplierBp(a.UpstreamCostMultiplierBp).
		SetLifecycleRevision(expectedRevision + 1)
	if a.BaseURL != nil {
		u = u.SetBaseURL(*a.BaseURL)
	} else {
		u = u.ClearBaseURL()
	}
	if a.CacheDomain != nil {
		u = u.SetCacheDomain(*a.CacheDomain)
	} else {
		u = u.ClearCacheDomain()
	}
	if a.Status == domain.StatusActive {
		u = u.ClearFailedAt().ClearLastError().ClearFailureSource()
	}
	if cooldownUntil != nil {
		u = u.SetCooldownUntil(*cooldownUntil)
	} else {
		u = u.ClearCooldownUntil()
	}
	n, err := u.Save(ctx)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("%w: id=%d expected revision %d stale", ErrStaleRevision, a.ID, expectedRevision)
	}
	return r.GetAccount(ctx, a.ID)
}
