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

// CreateAccount 按值逐字写入 enabled：domain.Account.Enabled 是普通 bool，
// 仓库层无法区分"未提供"与"显式 false"，故零值即落库为禁用，不补默认。
// 直接构造 domain.Account 的仓库层调用方必须显式给出 Enabled（可用账号
// 写 true）；创建默认（enabled=true、倍率 ×1、默认并发、代际 C=1/K=1）
// 由 service.CreateAccount 收口统一施加。
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
		b := client.Account.Create().
			SetName(a.Name).SetTemplateID(a.TemplateID).
			SetNillableBaseURL(a.BaseURL).
			SetUpstreamKey(a.UpstreamKey).
			SetMaxConcurrency(a.MaxConcurrency).
			SetEnabled(a.Enabled)
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
		// 身份纪元（K）按存在性写：缺省（0）= 不写 → 落 DB 默认 1（与
		// LifecycleRevision 同款语义；§3.2 的创建默认由 service 收口显式给 1）。
		if a.IdentityRevision != 0 {
			b = b.SetIdentityRevision(a.IdentityRevision)
		}
		// Explicit enabled 落库：创建收口恒给出显式值（默认 true、显式 false
		// 生效），零值不再静默转默认。
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

func (r *AccountRepo) GetAccountWithTemplate(ctx context.Context, id int64) (*domain.Account, error) {
	row, err := r.client.Account.Query().Where(account.IDEQ(id)).WithTemplate().Only(ctx)
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
	if q.TemplateID > 0 {
		pred = pred.Where(account.TemplateIDEQ(q.TemplateID))
	}
	if q.Enabled != nil {
		pred = pred.Where(account.EnabledEQ(*q.Enabled))
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

// SetAccountFailed 幂等写账号失效（SDK 接入——统一失效回调处理链第一步）：
// failed_at + last_error（失效原因文本，复用既有 last_error——用户裁决
// 2026-08-13：两原因字段并存会漂移；失效后账号摘除不再被调度 → caller.go
// 的普通失败写点不会覆盖失效原因，复用安全）首写生效——failed_at 已置（首次
// 上报）→ 0 行不覆盖（保持首次失效时刻与原因；重复上报不重复写；恢复由
// 管理面清 failed_at + last_error）。**空 reason 不清旧值**（评审：
// 空原因上报不触碰既有 last_error——保持"最近错误"审计语义）。failed_at
// 即摘除的持久化事实：调度器快照装载经 runtimeStatusFor 置运行时 disabled；
// 管理面手动禁用 = enabled=false（语义分离，两者可并存）。
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

// ErrStaleIdentityRevision 失效路径 CAS 失效：期望 identity_revision（K）
// 已过期。失效事件携带它被计算时所见的 K；K 只由管理面身份写入推进，
// 故 stale 在此意味着"身份已授权变更，旧判决作废"——与 C 前置条件的
// ErrStaleRevision（"客户端令牌过期"）语义不同，不得混用（errors.Is
// 分支靠它区分 K 陈旧与 C 陈旧）。
var ErrStaleIdentityRevision = fmt.Errorf("%w: stale identity_revision", ErrConflict)

// FailAccountCAS 生命周期 fenced 失效：guard K（expectedIdentityRevision）
// 并原子 +1 C，设置 failed_at/last_error/failure_source。0 行命中 → K
// stale（ErrStaleIdentityRevision）。
//
// 非对称更新（必须 pin 住）：guard 的是 K，被推进的是 C。C 必须用相对自增
// AddLifecycleRevision(1)：guard 不再钉住 C，SetLifecycleRevision(
// expectedRevision+1) 会把 expected（一个 K 值）写进 C——C 同时是客户端
// CAS 令牌与重编译水位，回退/错写会同时破坏两者。
//
// K 本身在此不推进：失效是运行时观测写入，不是管理面身份写入（§2.1）；
// 身份未变 ⇒ 在途工件的 (I,K) 身份延续，C+1 只提供"被改过"的水位信号。
func (r *AccountRepo) FailAccountCAS(ctx context.Context, id int64, expectedIdentityRevision int64, source string, failedAt time.Time, reason string) error {
	u := r.client.Account.Update().
		Where(account.IDEQ(id), account.IdentityRevisionEQ(expectedIdentityRevision)).
		AddLifecycleRevision(1).
		SetFailedAt(failedAt).
		SetFailureSource(source)
	if reason != "" {
		u = u.SetLastError(reason)
	}
	n, err := u.Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: id=%d expected identity revision %d stale", ErrStaleIdentityRevision, id, expectedIdentityRevision)
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
