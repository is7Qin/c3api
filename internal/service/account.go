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

func (s *Service) CreateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	if err := validateAccount(a); err != nil {
		return nil, err
	}
	tpl, err := s.store.GetTemplate(ctx, a.TemplateID)
	if err != nil {
		return nil, mapRepoErr(err) // 模板缺 id → 404
	}
	// upstream_key 必填性按模板类型：codex-oauth/codex-pat 凭据走 account_ext
	// （创建后经 /accounts/{id}/ext 配置），可空；其余类型静态透传必填。
	if a.UpstreamKey == "" && tpl.CredentialType != credential.TypeCodexOAuth && tpl.CredentialType != credential.TypeCodexPAT {
		return nil, ErrInvalidInput
	}
	if isCodexCredentialType(tpl.CredentialType) && a.BaseURL != nil && *a.BaseURL != "" {
		return nil, ErrInvalidInput
	}
	if a.GroupIDs != nil {
		if err := s.checkGroupsExist(ctx, *a.GroupIDs); err != nil {
			return nil, err // 组缺 id → 404
		}
	}
	created, err := s.store.CreateAccount(ctx, a)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if a.GroupIDs != nil {
		// 创建才有 id；替换语义（含空数组 = 清空，对新建账号即无分组）。
		if err := mapRepoErr(s.store.SetAccountGroups(ctx, created.ID, *a.GroupIDs)); err != nil {
			return nil, err
		}
	}
	// O2 组级定向：新账号进其分组快照（无分组账号不入任何快照 → 空集 no-op）。
	s.inv.Accounts(groupsOf(a), false)
	s.publish(ctx, notify.Change{Groups: groupsOf(a)})
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

func (s *Service) UpdateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	if err := validateAccount(a); err != nil {
		return nil, err
	}
	tpl, err := s.store.GetTemplate(ctx, a.TemplateID)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if a.UpstreamKey == "" && !isCodexCredentialType(tpl.CredentialType) {
		return nil, ErrInvalidInput
	}
	if isCodexCredentialType(tpl.CredentialType) && a.BaseURL != nil && *a.BaseURL != "" {
		return nil, ErrInvalidInput
	}
	if a.GroupIDs != nil {
		if err := s.checkGroupsExist(ctx, *a.GroupIDs); err != nil {
			return nil, err // 组缺 id → 404
		}
	}
	// O2 组级定向：变更前取旧组（账号移组 A→B 时 A、B 两组快照都要重载——
	// 旧组移除账号、新组加入账号）+ 旧 upstream_key/base_url 比较（变更 →
	// clients 失效）。查询失败 → 空集 + Warn（调度器 ≤30s 同步兜底）。
	oldGroups, gErr := s.store.GetAccountGroups(ctx, a.ID)
	keyChanged := false
	var curForCAS *domain.Account
	if cur, err := s.store.GetAccount(ctx, a.ID); err == nil {
		curForCAS = cur
		// 生命周期独占字段回填（fenced 端点所有权）：PUT 的 handler 转换面不携带
		// enabled/采购倍率/缓存域/revision（零值），repo 全字段 Set 会把零值直接
		// 落库 = 静默 clobber（禁用账号、倍率归 0、清缓存域）。这些字段只能经
		// CAS 端点变更，PUT 一律以当前值覆盖入参零值。失效字段
		// （failed_at/last_error/failure_source）同样不在 PUT 写面——恢复唯一
		// 入口 POST /accounts/{id}/recover（fenced）。
		a.Enabled = cur.Enabled
		a.UpstreamCostMultiplierBp = cur.UpstreamCostMultiplierBp
		a.CacheDomain = cur.CacheDomain
		a.LifecycleRevision = cur.LifecycleRevision
		keyChanged = cur.UpstreamKey != a.UpstreamKey
		// baseURLChanged 并入 keyChanged（C2——BaseURL 构建时固化在 client 缓存
		// 键内，非流式路径新值不生效直到失效）：按值判定（M4——nil↔"" 同值，
		// DB 经 create 归一/批量空串落 NULL 无 "" 形态，误报仅多余失效无害；
		// 不引入 helper）。复用既有 Accounts(gids, keyChanged) 参数面，
		// 零新增失效类型/调用点。
		curB, newB := "", ""
		if cur.BaseURL != nil {
			curB = *cur.BaseURL
		}
		if a.BaseURL != nil {
			newB = *a.BaseURL
		}
		keyChanged = keyChanged || curB != newB
	}
	var updated *domain.Account
	if keyChanged {
		expected := int64(1)
		if curForCAS != nil {
			expected = curForCAS.LifecycleRevision
		} else if fetched, ferr := s.store.GetAccount(ctx, a.ID); ferr == nil {
			expected = fetched.LifecycleRevision
		}
		updated, err = s.store.UpdateAccountCAS(ctx, a, expected)
		if err != nil {
			return nil, mapRepoErr(err)
		}
	} else {
		updated, err = s.store.UpdateAccount(ctx, a)
		if err != nil {
			return nil, err
		}
	}
	if a.GroupIDs != nil {
		// nil = 不变；非 nil = 替换（含空数组 = 清空）。
		if err := mapRepoErr(s.store.SetAccountGroups(ctx, a.ID, *a.GroupIDs)); err != nil {
			return nil, err
		}
	}
	gids := oldGroups
	if a.GroupIDs != nil {
		gids = append(gids, (*a.GroupIDs)...)
	}
	if gErr != nil && s.log != nil {
		s.log.Warn("account groups query failed", logx.Int64("account_id", a.ID), logx.Error(gErr))
	}
	s.inv.Accounts(gids, keyChanged)
	s.publish(ctx, notify.Change{Groups: gids, Clients: keyChanged}) // upstream_key/base_url 变更 → clients 失效
	return updated, nil
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

func (s *Service) UpdateAccountsBatch(ctx context.Context, ids []int64, p repository.AccountPatch) error {
	if err := validateIDs(ids); err != nil {
		return err
	}
	if err := validateAccountPatch(p); err != nil {
		return err
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
	if err := mapRepoErr(s.store.UpdateAccountsBatch(ctx, ids, p)); err != nil {
		return err
	}
	// 评审 I-3：nil = 未提供；空串 = 清除 upstream_key（同为变更语义）。
	// 批量路径不做逐账号旧值比较（需 N 次 GetAccount），只要提供了
	// UpstreamKey 就保守标记 clients 失效——clients 失效成本远低于旧 key
	// 滞留风险（宁可多失效一次）。BaseURL 同此保守失效（C2——含 "" 清空态，
	// 复用既有 Accounts(gids, keyChanged) 调用面）。
	s.inv.Accounts(gids, p.UpstreamKey != nil || p.BaseURL != nil)
	s.publish(ctx, notify.Change{Groups: gids, Clients: p.UpstreamKey != nil || p.BaseURL != nil})
	return nil
}

// groupsOf 账号分组 id 列表（nil = 无分组）。
func groupsOf(a *domain.Account) []int64 {
	if a.GroupIDs == nil {
		return nil
	}
	return *a.GroupIDs
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
