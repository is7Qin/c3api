// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// ListUserBalanceLogs 某用户的余额变动记录（/api/admin/users/{id}/balance-logs；
// platform_admin 专属）。sort/order 白名单校验（非法 → ErrInvalidInput 400），
// 分页 limit/offset 缺省归一在 repo。
func (s *Service) ListUserBalanceLogs(ctx context.Context, userID int64, q repository.ListQuery) ([]*domain.BalanceLog, int64, error) {
	if err := validateListQuery(q, listSortFields["balance_logs"]); err != nil {
		return nil, 0, err
	}
	return s.store.ListUserBalanceLogs(ctx, userID, q)
}

// writeBalanceLog 在事务内写入一条余额变动记录（收敛 §4 各路径的构造，
// 减少重复）。调用方保证 amount/balanceAfter 与同事务内 users.balance 变更一致。
func writeBalanceLog(ctx context.Context, tx repository.TxStore, userID, amount, balanceAfter int64, source domain.BalanceLogSource, operatorID int64, note *string) error {
	return tx.CreateBalanceLog(ctx, &domain.BalanceLog{
		UserID:       userID,
		Amount:       amount,
		BalanceAfter: balanceAfter,
		Source:       source,
		OperatorID:   operatorID,
		Note:         note,
	})
}
