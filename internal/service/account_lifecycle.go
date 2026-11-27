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

// RecoverLatchReleaser recover→latch 显式释放面（*latch.LatchStore 满足）：
// 恢复 CAS 成功后清除该账号的失效锁存，否则该账号永久不可选。nil = 未装配
// （跳过释放）。
type RecoverLatchReleaser interface {
	Clear(accountID int64)
}

// RecoverHealthClearer recover→健康记录显式清除面（*scheduler.RuntimeHealth
// 满足）：恢复 CAS 成功后清除该账号的全部健康记录，否则旧代际的 per-route
// OPEN 记录仍压住该账号。nil = 未装配（跳过清除）。
//
// 这是恢复的**唯一**健康面动作，且必须是纯清除：清空即「无记录」，而
// EffectiveState 把无记录视作 READY，账号因此立即可选。**不要在这里写任何
// 新记录**——尤其不要写 PROBING：它是唯一需要探针推进才能离开的状态，写它就
// 等于把「恢复是否生效」绑死在一个可选组件上。
type RecoverHealthClearer interface {
	ClearAccount(ctx context.Context, accountID int64) (int64, error)
}

// RecoverAccount 生命周期 fenced 恢复：CAS expectedRevision 清失效三字段
// （failed_at/last_error/failure_source）并 +1 → 显式释放 latch → 显式清除该
// 账号全部健康记录 → 组级失效 + NOTIFY。revision 过期 → ErrConflict（409）；
// 账号缺 id → ErrNotFound（404）。
//
// **返回成功即意味着账号可选**：清空健康记录后 EffectiveState 判为 READY，
// 选号门（attempt_plan_reservation 的 StateReady 等式）不再拒绝它。恢复链路
// 不写任何新健康记录，因此不依赖探针环是否在跑——「可选组件不得是命令生效的
// 必要条件」是这条链路的硬约束。
func (s *Service) RecoverAccount(ctx context.Context, id, expectedRevision int64) (*domain.Account, error) {
	if expectedRevision < 1 {
		return nil, ErrInvalidInput
	}
	// 没有任何前置读：CAS 自身区分「账号不存在」（ErrNotFound）与「代际过期」
	// （ErrConflict），预读只会制造一个用不上的窗口（读到的账号状态在 CAS 前
	// 就可能过期）。恢复也不推 K，因此不需要事先拿到 K。
	if err := mapRepoErr(s.store.RecoverAccountCAS(ctx, id, expectedRevision)); err != nil {
		return nil, err
	}
	if s.recoverLatch != nil {
		s.recoverLatch.Clear(id)
	}
	if s.recoverHealthClear != nil {
		if _, err := s.recoverHealthClear.ClearAccount(ctx, id); err != nil {
			s.log.Warn("recover health clear failed", logx.Int64("account_id", id), logx.Error(err))
		}
	}
	s.invalidateAccountLifecycle(ctx, id)
	s.log.Info("account recovered", logx.Int64("account_id", id), logx.Int64("revision", expectedRevision+1))
	return s.accountOrErr(ctx, id)
}

// invalidateAccountLifecycle 生命周期端点统一失效面：组级定向重载 + NOTIFY
// （旧组查询失败 → 空集 + Warn，30s 同步兜底——与账号写面同纪律）。
func (s *Service) invalidateAccountLifecycle(ctx context.Context, id int64) {
	gids, err := s.store.GetAccountGroups(ctx, id)
	if err != nil {
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
