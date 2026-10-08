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
	"github.com/is7qin/c3api/internal/ent/predicate"
)

type AccountRepo struct {
	client *ent.Client
	driver dialect.Driver
}

// accountOwnerPred 供应商面归属作用域谓词（spec 2026-10-09 §2.5）：作用域**显式入参**
// （调用方从 ctx 提取一次），管理面/用户面缺省（Set=false）⇒ 空谓词（全量，行为不变）。
// 必须 AND 进每一处账号 WHERE——禁止「先按 id 取行、再应用层比归属」（TOCTOU）。显式
// 入参使新增查询的作者无法忽略「这是带作用域的数据入口」，作用域仍进入 SQL 而非读出后过滤。
func accountOwnerPred(scope domain.AccountScope) []predicate.Account {
	if scope.Set {
		return []predicate.Account{account.SupplierUserID(scope.OwnerUID)}
	}
	return nil
}

// FindMissingOwnedAccountID 报告 ids 中**不被当前作用域拥有**的第一个 id（按输入顺序），
// 全部在作用域内 ⇒ (0, false)。作用域未注入（管理面全量）⇒ 恒 (0, false)——管理面
// 语义不因此收窄。查询把 owner 谓词 AND 进同一条 SQL（§2.5：禁止先取行再应用层比
// 归属），故「越域」与「不存在」不可区分，调用方一律按 404 处理（不泄漏存在性）。
//
// 批量聚合面（usage）用它做**整批前置校验**：任一越域 ⇒ 整批 404、不补零。
func (r *AccountRepo) FindMissingOwnedAccountID(ctx context.Context, ids []int64) (int64, bool, error) {
	scope := domain.AccountScopeFrom(ctx)
	if !scope.Set || len(ids) == 0 {
		return 0, false, nil
	}
	uniq := sortedUniqueIDs(ids)
	owned, err := r.client.Account.Query().
		Where(account.IDIn(uniq...)).
		Where(accountOwnerPred(domain.AccountScopeFrom(ctx))...).
		IDs(ctx)
	if err != nil {
		return 0, false, err
	}
	seen := make(map[int64]struct{}, len(owned))
	for _, id := range owned {
		seen[id] = struct{}{}
	}
	for _, id := range uniq {
		if _, ok := seen[id]; !ok {
			return id, true, nil
		}
	}
	return 0, false, nil
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
		// 归属写入（§2.5）：锁目标 users 行并校验「供应商面可达 + active」。
		if a.SupplierUserID > 0 {
			if err := validateOwnershipTarget(ctx, driver, a.SupplierUserID); err != nil {
				return err
			}
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
		// nil = 未显式提供 → 不写该列（落存储默认 ×1）；非 nil 精确落值——含 0（免费）。
		if a.UpstreamCostMultiplierBp != nil {
			b = b.SetUpstreamCostMultiplierBp(*a.UpstreamCostMultiplierBp)
		}
		if a.LifecycleRevision != 0 {
			b = b.SetLifecycleRevision(a.LifecycleRevision)
		}
		// 归属（供应商面）：服务端钉死 JWT 本人（>0）；0/缺省 = 不写该列（落 NULL =
		// 平台自有）。供应商面无法改写归属——字段不在 AccountConfigPatch 写面内。
		if a.SupplierUserID > 0 {
			b = b.SetSupplierUserID(a.SupplierUserID)
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
	// 作用域 AND 进 WHERE（供应商面越域 id ⇒ 0 行 ⇒ ErrNotFound/404，不泄漏存在性）。
	row, err := r.client.Account.Query().
		Where(account.IDEQ(id)).
		Where(accountOwnerPred(domain.AccountScopeFrom(ctx))...).
		Only(ctx)
	if err != nil {
		return nil, errMissingID(err, id)
	}
	return toDomainAccount(row), nil
}

func (r *AccountRepo) GetAccountWithTemplate(ctx context.Context, id int64) (*domain.Account, error) {
	row, err := r.client.Account.Query().
		Where(account.IDEQ(id)).
		Where(accountOwnerPred(domain.AccountScopeFrom(ctx))...).
		WithTemplate().Only(ctx)
	if err != nil {
		return nil, errMissingID(err, id)
	}
	return toDomainAccount(row), nil
}

func (r *AccountRepo) ListAccounts(ctx context.Context, q ListQuery) ([]*domain.Account, int64, error) {
	// 软删除：列表默认过滤已删（count 同谓词——pred 复用）；GET 单个不过滤。
	pred := r.client.Account.Query().Where(account.DeletedAtIsNil()).Where(accountOwnerPred(domain.AccountScopeFrom(ctx))...)
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
	// 作用域 AND 进 UPDATE 的 WHERE：越域 id ⇒ 0 行 ⇒ ErrNotFound/404。
	n, err := r.client.Account.Update().Where(account.IDEQ(id)).Where(accountOwnerPred(domain.AccountScopeFrom(ctx))...).SetDeletedAt(time.Now()).Save(ctx)
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
	// 供应商面（作用域已注入）：作用域 AND 进 UPDATE 的 WHERE（越域 id ⇒ 0 行 ⇒
	// ErrNotFound）；管理面（无作用域）保持既有 UpdateOneID 形态不变。
	if sc := domain.AccountScopeFrom(ctx); sc.Set {
		n, err := r.client.Account.Update().
			Where(account.IDEQ(accountID)).
			Where(account.SupplierUserID(sc.OwnerUID)).
			ClearGroups().
			AddGroupIDs(groupIDs...).
			Save(ctx)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: id=%d missing", ErrNotFound, accountID)
		}
		return nil
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
		Where(accountOwnerPred(domain.AccountScopeFrom(ctx))...).
		QueryGroups().
		IDs(ctx)
}

// SetAccountFailed 幂等写账号失效（SDK 接入——统一失效回调处理链第一步）：
// failed_at + last_error（失效原因文本，复用既有 last_error——用户裁决
// 2026-08-13：两原因字段并存会漂移；失效后账号摘除不再被调度 → caller.go
// 的普通失败写点不会覆盖失效原因，复用安全）首写生效——failed_at 已置（首次
// 上报）→ 0 行不覆盖（保持首次失效时刻与原因；重复上报不重复写；恢复由
// 管理面清 failed_at + last_error）。**空 reason 不清旧值**（
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
// 并原子 +1 C，设置 failed_at/last_error/failure_source。
//
// **幂等**：guard 另有 failed_at IS NULL——已失效账号重复上报不再推进 C、
// 不二次改写失败字段（返回 nil no-op）。失效是终态（恢复唯一入口
// /recover），重复上报（如失效账号在途请求又收 token revoked）不得再推代际。
//
// 非对称更新（必须 pin 住）：guard 的是 K，被推进的是 C。C 必须用相对自增
// AddLifecycleRevision(1)：guard 不再钉住 C，SetLifecycleRevision(
// expectedRevision+1) 会把 expected（一个 K 值）写进 C——C 同时是客户端
// CAS 令牌与重编译水位，回退/错写会同时破坏两者。
//
// K 本身在此不推进：失效是运行时观测写入，不是管理面身份写入（§2.1）；
// 身份未变 ⇒ 在途工件的 (I,K) 身份延续，C+1 只提供"被改过"的水位信号。
//
// 0 行命中回读区分（一次 PK 读，冷面）：K 仍当前且已失效 → 幂等 no-op（nil）；
// 其余（K 陈旧 / 缺行 / 回读故障——保守按 stale 处理）→ ErrStaleIdentityRevision
// （旧判决作废，语义不变）。
func (r *AccountRepo) FailAccountCAS(ctx context.Context, id int64, expectedIdentityRevision int64, source string, failedAt time.Time, reason string) error {
	u := r.client.Account.Update().
		Where(account.IDEQ(id), account.IdentityRevisionEQ(expectedIdentityRevision), account.FailedAtIsNil()).
		Where(accountOwnerPred(domain.AccountScopeFrom(ctx))...).
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
		// 幂等守卫命中（已失效且身份未变）→ 视为成功 no-op：不推 C、不改写。
		if fresh, gerr := r.GetAccount(ctx, id); gerr == nil && fresh != nil &&
			fresh.FailedAt != nil && fresh.IdentityRevision == expectedIdentityRevision {
			return nil
		}
		return fmt.Errorf("%w: id=%d expected identity revision %d stale", ErrStaleIdentityRevision, id, expectedIdentityRevision)
	}
	return nil
}

// RecoverAccountCAS 生命周期 fenced 恢复：CAS expectedRevision 并 +1，清除
// failed_at/last_error/failure_source。0 行 → stale 或未失效（no-op 视为 stale）。
func (r *AccountRepo) RecoverAccountCAS(ctx context.Context, id int64, expectedRevision int64) error {
	n, err := r.client.Account.Update().
		Where(account.IDEQ(id), account.LifecycleRevisionEQ(expectedRevision)).
		Where(accountOwnerPred(domain.AccountScopeFrom(ctx))...).
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
