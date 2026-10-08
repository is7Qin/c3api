// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
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

// ErrInvalidInput 表示写入会破坏跨实体业务不变量。
var ErrInvalidInput = errors.New("repository: invalid input")

// --- 批量更新字段子集（nil 字段 = 不更新） ---

type TemplatePatch struct {
	Name             *string
	BaseURL          *string
	SupportedFormats *[]domain.RequestFormat
	Models           *[]string
	FormatModels     *map[domain.RequestFormat][]string
	ModelMapping     *domain.ModelMapping
}

type AccountPatch struct {
	Name        *string
	TemplateID  *int64
	UpstreamKey *string
	// BaseURL 批量三态（定死）：nil = 不变；&"" = 清空（落 NULL = 继承
	// 模板）；&非空 = 落值。
	BaseURL        *string
	MaxConcurrency *int
	// GroupIDs nil = 不变；非 nil = 替换账号全部分组（含空数组 = 清空）。
	GroupIDs                 *[]int64
	Enabled                  *bool
	UpstreamCostMultiplierBp *int
	CacheDomain              *string // nil=不变, &""=清空, &val=落值
	// SupplierUserID 归属（spec §2.5 管理面「把账号分配给供应商」入口）：nil =
	// 不变；&0 = 清空（回平台自有，落 NULL）；&uid(>0) = 分配给该用户（值域校验：
	// 目标须为供应商面可达用户，且与禁用路径锁同一 users 行）。
	SupplierUserID *int64
}

type GroupPatch struct {
	Name       *string
	Visibility *domain.GroupVisibility
}

// AccountWriteResult 单账号一次写入的结果：新配置代际 C（客户端 CAS 令牌）与本次
// **真实变更**的字段集。ChangedFields 是调用方推导失效计划的唯一输入
// （IdentityChanged 由 domain 的声明表派生），调用方不得再自行比较补丁字段。
type AccountWriteResult struct {
	AccountID         int64
	LifecycleRevision int64
	ChangedFields     domain.FieldSet
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
	return withWriteTx(ctx, r.driver, func(client *ent.Client, driver dialect.Driver) error {
		if err := lockTemplateWrites(ctx, driver, ids); err != nil {
			return err
		}
		templates, err := loadTemplates(ctx, client, ids)
		if err != nil {
			return err
		}
		if p.BaseURL != nil && *p.BaseURL != "" {
			for _, id := range ids {
				if templates[id].CredentialType.IsCodex() {
					return fmt.Errorf("%w: codex template base_url must be empty", ErrInvalidInput)
				}
			}
		}
		for _, id := range ids {
			u := client.Template.UpdateOneID(id)
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
				if sqlgraph.IsUniqueConstraintError(err) && p.Name != nil {
					return fmt.Errorf("%w: name=%q", ErrConflict, *p.Name)
				}
				return errMissingID(err, id)
			}
		}
		return nil
	})
}

// maxOwnershipLockRelocks 归属锁集合重启上限（N1）：每轮重启至少把一个新的最终
// owner 纳入锁集合（单调增长 ⇒ 收敛），故正常 1–2 轮即定；上限防病态抖动
// （持续转属）下的活锁。
const maxOwnershipLockRelocks = 8

// errOwnershipLockSetStale 内部哨兵（N1）：锁内读到的最终 owner 不在预锁 users
// 集合内——需整体重启，在锁 accounts 前按统一升序把该 owner 也纳入预锁，避免
// 「已持 accounts 后补锁 users」的 accounts→users 反序死锁面。
var errOwnershipLockSetStale = errors.New("repository: ownership lock set stale")

// lockSetHas 报告 uid 是否已在锁集合内（N1 重启判据；集合小、线性即可）。
func lockSetHas(set []int64, uid int64) bool {
	for _, v := range set {
		if v == uid {
			return true
		}
	}
	return false
}

// UpdateAccountsBatch 批量更新账号配置（公共入口，无测试钩子）。
func (r *AccountRepo) UpdateAccountsBatch(ctx context.Context, ids []int64, p AccountPatch) ([]AccountWriteResult, error) {
	return r.updateAccountsBatch(ctx, ids, p, nil)
}

// updateAccountsBatch 批量更新账号（公共入口的逻辑）。afterOwnerRead 是**私有测试
// 钩子**（prod 恒 nil）：在「读当前归属」之后、「锁 users/accounts」之前触发——供
// N1 交错测试在两读之间以独立连接提交一次转属（无 sleep 的确定性屏障）。hook 由调用
// 方私有持有，不再是包级可变全局；内层公共调用（传入 nil）自然不继承 hook。
func (r *AccountRepo) updateAccountsBatch(ctx context.Context, ids []int64, p AccountPatch, afterOwnerRead func()) ([]AccountWriteResult, error) {
	// 归属锁集合协议（N1，§2.5/§5.7）：users 锁集合必须在锁 accounts **之前**
	// 确定并锁死（固定锁序 users → accounts，与「禁用/转属」同序）。无锁读得到的
	// 当前 owner 可能陈旧（并发转属）——若在已持 accounts 后才发现最终 owner 不在
	// 集合内并补锁 users，即 accounts→users 反序，与「禁用/转属持 users 后等
	// accounts」构成 ABBA 死锁面。故这里：预锁 users（当前 owner ∪ 目标 owner ∪
	// 历史发现的最终 owner）→ 锁 accounts → 锁内复核；一旦发现集合外的最终 owner
	// 就**整体重启**（回滚释放已持锁），下轮把它并入预锁集合（单调增长 ⇒ 收敛），
	// 绝不在持 accounts 后补锁 users。
	extraOwners := make(map[int64]struct{})
	for attempt := 0; ; attempt++ {
		var results []AccountWriteResult
		err := withWriteTx(ctx, r.driver, func(client *ent.Client, driver dialect.Driver) error {
			currentOwners, err := loadAccountOwners(ctx, client, ids)
			if err != nil {
				return err
			}
			if afterOwnerRead != nil {
				afterOwnerRead()
			}
			lockSet := make([]int64, 0, len(currentOwners)+1+len(extraOwners))
			lockSet = append(lockSet, currentOwners...)
			if p.SupplierUserID != nil {
				lockSet = append(lockSet, *p.SupplierUserID)
			}
			for uid := range extraOwners {
				lockSet = append(lockSet, uid)
			}
			if err := lockOwnershipUsers(ctx, driver, lockSet); err != nil {
				return err
			}
			locked, err := lockAccountsForUpdate(ctx, driver, ids)
			if err != nil {
				return err
			}
			// 锁内回读的真实 owner 才是权威判据。最终 owner 不在预锁集合 ⇒ 不得在
			// 持 accounts 后补锁 users；登记该 owner 并整体重启（下轮按统一升序预锁）。
			for _, row := range locked {
				finalOwner := row.SupplierUserID
				if p.SupplierUserID != nil {
					finalOwner = *p.SupplierUserID
				}
				if finalOwner > 0 && !lockSetHas(lockSet, finalOwner) {
					extraOwners[finalOwner] = struct{}{}
					return errOwnershipLockSetStale
				}
			}
			templateIDs := make([]int64, 0, len(locked)+1)
			for _, row := range locked {
				templateIDs = append(templateIDs, row.TemplateID)
			}
			if p.TemplateID != nil {
				templateIDs = append(templateIDs, *p.TemplateID)
			}
			if err := lockTemplateWrites(ctx, driver, templateIDs); err != nil {
				return err
			}
			templates, err := loadTemplates(ctx, client, templateIDs)
			if err != nil {
				return err
			}
			for _, row := range locked {
				templateID := row.TemplateID
				if p.TemplateID != nil {
					templateID = *p.TemplateID
				}
				baseURL := row.BaseURL
				if p.BaseURL != nil {
					baseURL = p.BaseURL
				}
				if err := validateCodexAccountBaseURL(templates[templateID], baseURL); err != nil {
					return err
				}
			}
			if p.GroupIDs != nil && len(*p.GroupIDs) > 0 {
				if err := checkGroupExist(ctx, client.Group.Query, *p.GroupIDs); err != nil {
					return err
				}
			}
			// 归属最终态复核（§2.5/A13⑤）：**每个账号**按补丁合并后的最终归属判定
			// ——不是只在补丁出现 supplier_user_id 时才查。普通 `{enabled:true}` 撞上
			// 已被禁用/降权的归属供应商同样被拒（否则即「重新启用 disabled 供应商的
			// 账号」，spec I1 反例）。users 行已在上方锁定，禁用路径无法在本事务期间
			// 提交 ⇒ 无 TOCTOU。
			for _, row := range locked {
				finalOwner := row.SupplierUserID
				if p.SupplierUserID != nil {
					finalOwner = *p.SupplierUserID
				}
				if finalOwner > 0 {
					if err := validateOwnershipTarget(ctx, driver, finalOwner); err != nil {
						return err
					}
				}
			}
			pre := make(map[int64]AccountFieldValues, len(locked))
			for _, row := range locked {
				pre[row.ID] = row
			}
			for _, id := range sortedUniqueIDs(ids) {
				// C（配置代际）**无条件**推进：一次配置变更就是一个新代际，读-改-写的
				// 客户端据此必然重读。K（身份代际）**按值**推进：仅当身份类字段真的变了
				// 才推进——幂等重写不推进。二者分开是因为在途判定（失效判决 / latch /
				// 健康记录 / continuation）围栏在 (I,K) 上：身份变了旧判定必须作废，而
				// 普通配置变更不得白白作废它们。用**相对递增**而非先读后写：行已在本
				// 事务内 lockAccountsForUpdate 锁定，故 pre 里的旧值即为判据且无
				// lost-update 窗口。Save 回显新行 → 新 C/K 直接作为响应回显。
				row := pre[id]
				result := AccountWriteResult{AccountID: id, ChangedFields: ChangedFields(p, row)}
				u := client.Account.UpdateOneID(id).AddLifecycleRevision(1)
				if result.ChangedFields.IdentityChanged() {
					u = u.AddIdentityRevision(1)
				}
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
					if *p.BaseURL == "" {
						u = u.ClearBaseURL()
					} else {
						u = u.SetBaseURL(*p.BaseURL)
					}
				}
				if p.MaxConcurrency != nil {
					u = u.SetMaxConcurrency(*p.MaxConcurrency)
				}
				if p.GroupIDs != nil {
					u = u.ClearGroups().AddGroupIDs(*p.GroupIDs...)
				}
				if p.Enabled != nil {
					u = u.SetEnabled(*p.Enabled)
				}
				if p.UpstreamCostMultiplierBp != nil {
					u = u.SetUpstreamCostMultiplierBp(*p.UpstreamCostMultiplierBp)
				}
				if p.CacheDomain != nil {
					if *p.CacheDomain == "" {
						u = u.ClearCacheDomain()
					} else {
						u = u.SetCacheDomain(*p.CacheDomain)
					}
				}
				// 归属（§2.5）：&0 = 清空（回平台自有，落 NULL）；&uid>0 = 分配。
				if p.SupplierUserID != nil {
					if *p.SupplierUserID == 0 {
						u = u.ClearSupplierUserID()
					} else {
						u = u.SetSupplierUserID(*p.SupplierUserID)
					}
				}
				updated, err := u.Save(ctx)
				if err != nil {
					return errMissingID(err, id)
				}
				result.LifecycleRevision = updated.LifecycleRevision
				results = append(results, result)
			}
			return nil
		})
		if errors.Is(err, errOwnershipLockSetStale) {
			if attempt >= maxOwnershipLockRelocks {
				return nil, fmt.Errorf("%w: ownership changed repeatedly during batch update; retry", ErrConflict)
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		return results, nil
	}
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

// loadAccountOwners 读出 ids 的当前归属（0 = 平台自有），用于「锁哪些 users 行」
// 这一**锁集合**决策——不是写入判据（写入判据来自 FOR UPDATE 后回读的
// AccountFieldValues）。无锁读：即使并发转属改了这一集合，最坏是多锁/少锁一个
// users 行，而真正写入仍由锁内复核串行化保证正确。
func loadAccountOwners(ctx context.Context, client *ent.Client, ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := client.Account.Query().
		Where(account.IDIn(sortedUniqueIDs(ids)...)).
		Select(account.FieldSupplierUserID).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(rows))
	for _, row := range rows {
		if row.SupplierUserID != nil {
			out = append(out, *row.SupplierUserID)
		}
	}
	return out, nil
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
		// 作用域 AND 进存在性检查（§2.5）：越域 id 视为缺失 ⇒ 整事务失败（404）。
		return q().Where(account.IDIn(chunk...)).Where(accountOwnerPred(domain.AccountScopeFrom(ctx))...).IDs(ctx)
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
// 空集直接返回 nil。错误带 (chunk i/n, N ids) 上下文。
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
