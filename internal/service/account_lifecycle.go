// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/pkg/logx"
)

// RecoverProber recover→PROBING 健康写入面（*scheduler.RuntimeHealth 满足）：
// 恢复 CAS 成功后对新 revision 写通配 PROBING，探针环接管后续。nil = 未装配
// （测试/降级），recover 仍完成持久恢复（调度器周期同步兜底收敛）。
type RecoverProber interface {
	SetProbing(ctx context.Context, accountID int64, revision int64) error
}

// RecoverAccount 生命周期 fenced 恢复：CAS expectedRevision 清失效三字段
// （failed_at/last_error/failure_source）并 +1 → 新 revision 写 PROBING（best
// effort，失败仅 Warn——恢复已持久，探针环由同步周期兜底）→ 组级失效 + NOTIFY。
// revision 过期 → ErrConflict（409）；账号缺 id → ErrNotFound（404）。
func (s *Service) RecoverAccount(ctx context.Context, id, expectedRevision int64) (*domain.Account, error) {
	if expectedRevision < 1 {
		return nil, ErrInvalidInput
	}
	acct, err := s.store.GetAccount(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if err := mapRepoErr(s.store.RecoverAccountCAS(ctx, id, expectedRevision)); err != nil {
		return nil, err
	}
	if s.recoverProber != nil {
		// 健康记录按 K（identity_revision）隔离——EffectiveState 以 K 查询，
		// 故 PROBING 必须以 K 写入，否则该记录永不被命中。恢复**不是身份写入**：
		// K 不变，故 CAS 前取到的 K 仍然有效（无需重取）。
		if err := s.recoverProber.SetProbing(ctx, id, acct.IdentityRevision); err != nil && s.log != nil {
			s.log.Warn("recover probing health write failed", logx.Int64("account_id", id), logx.Error(err))
		}
	}
	s.invalidateAccountLifecycle(ctx, id)
	if s.log != nil {
		s.log.Info("account recovered", logx.Int64("account_id", id), logx.Int64("revision", expectedRevision+1))
	}
	return s.accountOrErr(ctx, id)
}

// SetAccountEnabled 管理面启用/禁用（CAS fencing +1）。Enable 不清失效字段
// （失效恢复唯一入口 = RecoverAccount）；stale → ErrConflict。
func (s *Service) SetAccountEnabled(ctx context.Context, id, expectedRevision int64, enabled bool) (*domain.Account, error) {
	if expectedRevision < 1 {
		return nil, ErrInvalidInput
	}
	if _, err := s.store.GetAccount(ctx, id); err != nil {
		return nil, mapRepoErr(err)
	}
	if err := mapRepoErr(s.store.SetAccountEnabledCAS(ctx, id, expectedRevision, enabled)); err != nil {
		return nil, err
	}
	s.invalidateAccountLifecycle(ctx, id)
	return s.accountOrErr(ctx, id)
}

// UpdateAccountCostMultiplier 采购成本倍率（basis points：10000 = 1.0x，
// 0 = 免费，上限 100000 = ×10 对齐组倍率天花板）CAS +1；越界 → ErrInvalidInput；
// stale → ErrConflict。
func (s *Service) UpdateAccountCostMultiplier(ctx context.Context, id, expectedRevision int64, multiplierBp int) (*domain.Account, error) {
	if expectedRevision < 1 || multiplierBp < 0 || multiplierBp > 100000 {
		return nil, ErrInvalidInput
	}
	if _, err := s.store.GetAccount(ctx, id); err != nil {
		return nil, mapRepoErr(err)
	}
	if err := mapRepoErr(s.store.UpdateAccountCostMultiplierCAS(ctx, id, expectedRevision, multiplierBp)); err != nil {
		return nil, err
	}
	s.invalidateAccountLifecycle(ctx, id)
	return s.accountOrErr(ctx, id)
}

// UpdateAccountCacheDomain 缓存域 CAS +1：domain = nil → 清空（回账号私有域）；
// 非 nil 必须过 validateCacheDomain（非法 → ErrInvalidInput）；stale → ErrConflict。
func (s *Service) UpdateAccountCacheDomain(ctx context.Context, id, expectedRevision int64, domain *string) (*domain.Account, error) {
	if expectedRevision < 1 {
		return nil, ErrInvalidInput
	}
	if domain != nil {
		if *domain == "" || validateCacheDomain(*domain) != nil {
			return nil, ErrInvalidInput
		}
	}
	if _, err := s.store.GetAccount(ctx, id); err != nil {
		return nil, mapRepoErr(err)
	}
	if err := mapRepoErr(s.store.UpdateAccountCacheDomainCAS(ctx, id, expectedRevision, domain)); err != nil {
		return nil, err
	}
	s.invalidateAccountLifecycle(ctx, id)
	return s.accountOrErr(ctx, id)
}

// invalidateAccountLifecycle 生命周期端点统一失效面：组级定向重载 + NOTIFY
// （旧组查询失败 → 空集 + Warn，30s 同步兜底——与 UpdateAccount 同纪律）。
func (s *Service) invalidateAccountLifecycle(ctx context.Context, id int64) {
	gids, err := s.store.GetAccountGroups(ctx, id)
	if err != nil && s.log != nil {
		s.log.Warn("account groups query failed", logx.Int64("account_id", id), logx.Error(err))
	}
	s.inv.Accounts(gids, false)
	s.publish(ctx, notify.Change{Groups: gids})
}

// accountOrErr 生命周期端点统一回显：CAS 后重读（revision 已 +1，响应携带
// 新代际供前端下一次 CAS）。
func (s *Service) accountOrErr(ctx context.Context, id int64) (*domain.Account, error) {
	a, err := s.store.GetAccount(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return a, nil
}
