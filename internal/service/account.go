// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
)

// CreateAccount 账号创建：与 PATCH 共用同一三态字段模型（repository.AccountPatch），
// 创建侧额外要求 Name 与 TemplateID 必须显式提供。写时默认：Enabled=true、
// UpstreamCostMultiplierBp=10000（×1）、CacheDomain/BaseURL=nil、无分组、
// MaxConcurrency=注入的默认并发、LifecycleRevision=IdentityRevision=1。
// 未提供的字段落默认；base_url/cache_domain 的 &"" 即 NULL（与默认一致）；
// 显式 enabled:false 生效。
func (s *Service) CreateAccount(ctx context.Context, p repository.AccountPatch) (*domain.Account, error) {
	if p.Name == nil || *p.Name == "" {
		return nil, ErrInvalidInput
	}
	if p.TemplateID == nil || *p.TemplateID <= 0 {
		return nil, ErrInvalidInput
	}
	if err := validateAccountPatch(p); err != nil {
		return nil, err
	}
	tpl, err := s.store.GetTemplate(ctx, *p.TemplateID)
	if err != nil {
		return nil, mapRepoErr(err) // 模板缺 id → 404
	}
	// upstream_key 必填性按模板类型：codex-oauth/codex-pat 凭据走 account_ext
	// （创建后经 /accounts/{id}/ext 配置），可空；其余类型静态透传必填。
	key := ""
	if p.UpstreamKey != nil {
		key = *p.UpstreamKey
	}
	if key == "" && tpl.CredentialType != credential.TypeCodexOAuth && tpl.CredentialType != credential.TypeCodexPAT {
		return nil, ErrInvalidInput
	}
	if isCodexCredentialType(tpl.CredentialType) && p.BaseURL != nil && *p.BaseURL != "" {
		return nil, ErrInvalidInput
	}
	if p.GroupIDs != nil {
		if err := s.checkGroupsExist(ctx, *p.GroupIDs); err != nil {
			return nil, err // 组缺 id → 404
		}
	}
	a := &domain.Account{
		Name:                     *p.Name,
		TemplateID:               *p.TemplateID,
		UpstreamKey:              key,
		MaxConcurrency:           s.defaultMaxConcurrency,
		Enabled:                  true,
		UpstreamCostMultiplierBp: 10000,
		LifecycleRevision:        1,
		IdentityRevision:         1,
	}
	if p.BaseURL != nil && *p.BaseURL != "" {
		v := *p.BaseURL
		a.BaseURL = &v
	}
	if p.CacheDomain != nil && *p.CacheDomain != "" {
		v := *p.CacheDomain
		a.CacheDomain = &v
	}
	if p.MaxConcurrency != nil {
		a.MaxConcurrency = *p.MaxConcurrency
	}
	if p.Enabled != nil {
		a.Enabled = *p.Enabled
	}
	if p.UpstreamCostMultiplierBp != nil {
		a.UpstreamCostMultiplierBp = *p.UpstreamCostMultiplierBp
	}
	created, err := s.store.CreateAccount(ctx, a)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if p.GroupIDs != nil {
		// 创建才有 id；替换语义（含空数组 = 清空，对新建账号即无分组）。
		if err := mapRepoErr(s.store.SetAccountGroups(ctx, created.ID, *p.GroupIDs)); err != nil {
			return nil, err
		}
	}
	// O2 组级定向：新账号进其分组快照（无分组账号不入任何快照 → 空集 no-op）。
	s.inv.Accounts(groupsOfPatch(p), false)
	s.publish(ctx, notify.Change{Groups: groupsOfPatch(p)})
	return created, nil
}

func (s *Service) GetAccount(ctx context.Context, id int64) (*domain.Account, error) {
	a, err := s.store.GetAccount(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return a, nil
}

func (s *Service) ListAccounts(ctx context.Context, q repository.ListQuery) ([]*domain.Account, int64, error) {
	if err := validateListQuery(q, listSortFields["accounts"]); err != nil {
		return nil, 0, err
	}
	return s.store.ListAccounts(ctx, q)
}

// PatchAccount 账号配置的**唯一写点**：三态补丁一次落库，无条件推进配置代际 C
// （客户端 CAS 令牌）。身份字段（模板/base_url/upstream_key）按值变更时另推进
// 身份代际 K（由 repository 层判定并落库）。事务提交后统一失效：组级定向重载
// + clients 失效（身份字段变更）+ NOTIFY——失效只在提交后执行，回滚路径不留
// 半套失效。
//
// 三态语义由 repository.AccountPatch 承载：nil = 不变；base_url/cache_domain
// 空串 = 清空（落 NULL）；有值 = 落值；group_ids nil = 不变、非 nil（含空数组）
// = 替换。
//
// ifMatch 非 nil 时为前置条件（对当前 lifecycle_revision）：陈旧 →
// ErrPreconditionFailed（412）；nil = 不做前置条件检查。
//
// 失效字段（failed_at/last_error/failure_source）不在写面内：失效恢复的唯一入口
// 是 RecoverAccount。
func (s *Service) PatchAccount(ctx context.Context, id int64, p repository.AccountPatch, ifMatch *int64) (*domain.Account, error) {
	if err := validateAccountPatch(p); err != nil {
		return nil, err
	}
	cur, err := s.store.GetAccount(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err) // 缺 id → 404
	}
	if ifMatch != nil && *ifMatch != cur.LifecycleRevision {
		return nil, ErrPreconditionFailed
	}
	// upstream_key 必填性按**合并后**的模板类型判定（模板可随本补丁一起改）。
	tplID := cur.TemplateID
	if p.TemplateID != nil {
		tplID = *p.TemplateID
	}
	tpl, err := s.store.GetTemplate(ctx, tplID)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	key := cur.UpstreamKey
	if p.UpstreamKey != nil {
		key = *p.UpstreamKey
	}
	if key == "" && !isCodexCredentialType(tpl.CredentialType) {
		return nil, ErrInvalidInput
	}
	if p.GroupIDs != nil {
		if err := s.checkGroupsExist(ctx, *p.GroupIDs); err != nil {
			return nil, err // 组缺 id → 404
		}
	}
	// 失效判据来自 repository 回显的**真实变更集**（身份类字段按值变更），
	// 不在本层重写字段清单。
	oldGroups, gErr := s.store.GetAccountGroups(ctx, id)
	results, err := s.store.UpdateAccountsBatch(ctx, []int64{id}, p)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	gids := oldGroups
	if p.GroupIDs != nil {
		gids = append(gids, (*p.GroupIDs)...)
	}
	if gErr != nil && s.log != nil {
		s.log.Warn("account groups query failed", logx.Int64("account_id", id), logx.Error(gErr))
	}
	identityChanged := identityChangedIn(results)
	s.inv.Accounts(gids, identityChanged)
	s.publish(ctx, notify.Change{Groups: gids, Clients: identityChanged})
	return s.accountOrErr(ctx, id)
}

// identityChangedIn 汇总一次写入中是否有账号真的换了路由目标身份（身份类字段
// 按值变更）。判据只来自 repository 回显的变更集，本层不重写字段清单。
func identityChangedIn(results []repository.AccountWriteResult) bool {
	for _, res := range results {
		if res.ChangedFields.IdentityChanged() {
			return true
		}
	}
	return false
}

// GetAccountGroups 账号的分组 id 列表（编辑回显）。账号缺 id → 404。
func (s *Service) GetAccountGroups(ctx context.Context, id int64) ([]int64, error) {
	if _, err := s.store.GetAccount(ctx, id); err != nil {
		return nil, mapRepoErr(err)
	}
	return s.store.GetAccountGroups(ctx, id)
}

// checkGroupsExist 校验分组全部存在（缺失 → service.ErrNotFound 含 id）。
func (s *Service) checkGroupsExist(ctx context.Context, ids []int64) error {
	for _, id := range ids {
		if _, err := s.store.GetGroup(ctx, id); err != nil {
			return mapRepoErr(err)
		}
	}
	return nil
}

func (s *Service) DeleteAccount(ctx context.Context, id int64) error {
	// O2：删除前查旧组（删除后快照须移除该账号）。
	gids, err := s.store.GetAccountGroups(ctx, id)
	if err != nil && s.log != nil {
		s.log.Warn("account groups query failed", logx.Int64("account_id", id), logx.Error(err))
	}
	if err := mapRepoErr(s.store.DeleteAccount(ctx, id)); err != nil {
		return err // 404 缺 id（与批量语义对齐）
	}
	s.inv.Accounts(gids, false)
	s.publish(ctx, notify.Change{Groups: gids})
	return nil
}

func (s *Service) DeleteAccountsBatch(ctx context.Context, ids []int64) error {
	if err := validateIDs(ids); err != nil {
		return err
	}
	// O2：删除前逐个查旧组（组级定向并集）。
	var gids []int64
	for _, id := range ids {
		gs, err := s.store.GetAccountGroups(ctx, id)
		if err != nil {
			if s.log != nil {
				s.log.Warn("account groups query failed", logx.Int64("account_id", id), logx.Error(err))
			}
			continue
		}
		gids = append(gids, gs...)
	}
	if err := mapRepoErr(s.store.DeleteAccountsBatch(ctx, ids)); err != nil {
		return err
	}
	s.inv.Accounts(gids, false)
	s.publish(ctx, notify.Change{Groups: gids})
	return nil
}

// UpdateAccountsBatch 批量账号配置写入：单事务全成或全败，返回每账号的新配置
// 代际（客户端下一次读-改-写令牌）与真实变更集。
func (s *Service) UpdateAccountsBatch(ctx context.Context, ids []int64, p repository.AccountPatch) ([]repository.AccountWriteResult, error) {
	if err := validateIDs(ids); err != nil {
		return nil, err
	}
	if err := validateAccountPatch(p); err != nil {
		return nil, err
	}
	// O2：变更前逐个查旧组 + 替换目标组并集（upstream_key 批量变更 →
	// clients 失效）。
	var gids []int64
	for _, id := range ids {
		gs, err := s.store.GetAccountGroups(ctx, id)
		if err != nil {
			if s.log != nil {
				s.log.Warn("account groups query failed", logx.Int64("account_id", id), logx.Error(err))
			}
			continue
		}
		gids = append(gids, gs...)
	}
	if p.GroupIDs != nil {
		gids = append(gids, (*p.GroupIDs)...)
	}
	results, err := s.store.UpdateAccountsBatch(ctx, ids, p)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	// clients 失效判据来自 repository 回显的**真实变更集**：只有身份类字段真的
	// 变了才失效（幂等重写不再白白断连）。
	identityChanged := identityChangedIn(results)
	s.inv.Accounts(gids, identityChanged)
	s.publish(ctx, notify.Change{Groups: gids, Clients: identityChanged})
	return results, nil
}

// groupsOfPatch 补丁携带的分组 id 列表（nil = 未提供 → 空集）。
func groupsOfPatch(p repository.AccountPatch) []int64 {
	if p.GroupIDs == nil {
		return nil
	}
	return *p.GroupIDs
}

// AccountView 是账号的管理端视图（含调度器运行时信息）。运行时并发/EWMA
// 指标与请求路径同源（快照原子读）；账号生命周期（enabled/failed_at）与失效
// 恢复走 fenced 端点，不在视图内重复。
type AccountView struct {
	*domain.Account
	Concurrency int64   `json:"concurrency"`
	ErrRate     float64 `json:"err_rate"`
	ErrCount    int     `json:"err_count"`
}

// ListAccountViews 账号管理端视图（含调度器运行时信息）。handler 列表入口，
// 与 ListAccounts 一致做 sort/order 校验（非法 → ErrInvalidInput → 400）。
func (s *Service) ListAccountViews(ctx context.Context, q repository.ListQuery) ([]*AccountView, int64, error) {
	if err := validateListQuery(q, listSortFields["accounts"]); err != nil {
		return nil, 0, err
	}
	accs, total, err := s.store.ListAccounts(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*AccountView, 0, len(accs))
	for _, a := range accs {
		v := &AccountView{Account: a}
		if s.sched != nil {
			if ri, ok := s.sched.Runtime(a.ID); ok {
				v.Concurrency, v.ErrRate, v.ErrCount = ri.Concurrency, ri.ErrRate, ri.ErrCount
			}
		}
		out = append(out, v)
	}
	return out, total, nil
}
