// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/balancelog"
)

// BalanceLogRepo 余额变动记录持久化（管理面只读列表 + 与余额变更同事务的写）。
type BalanceLogRepo struct {
	client *ent.Client
}

// CreateBalanceLog 写入一条余额变动记录。WithTx 事务内经 tx client 插入，随整体
// 提交/回滚；user_id 为普通列（无 FK 边），不依赖用户行存在。普通 client 亦可用。
func (r *BalanceLogRepo) CreateBalanceLog(ctx context.Context, l *domain.BalanceLog) error {
	_, err := r.client.BalanceLog.Create().
		SetUserID(l.UserID).
		SetAmount(l.Amount).
		SetBalanceAfter(l.BalanceAfter).
		SetSource(balancelog.Source(l.Source)).
		SetOperatorID(l.OperatorID).
		SetNillableNote(l.Note).
		Save(ctx)
	return err
}

// ListUserBalanceLogs 某用户的余额变动记录（管理面 /api/admin/users/{id}/balance-logs）：
// WHERE user_id = $1 + Count（total 与行集同条件）+ sort 白名单（非法 → ErrInvalidSort）
// + order + 分页（Limit<=0→20、Offset<0→0；缺省 ORDER BY id DESC 走 (user_id, id) 索引）。
func (r *BalanceLogRepo) ListUserBalanceLogs(ctx context.Context, userID int64, q ListQuery) ([]*domain.BalanceLog, int64, error) {
	pred := r.client.BalanceLog.Query().Where(balancelog.UserIDEQ(userID))
	total, err := pred.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	order, err := q.sortOrder(balanceLogSortFields)
	if err != nil {
		return nil, 0, err
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	rows, err := pred.Order(order).Offset(q.Offset).Limit(q.Limit).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*domain.BalanceLog, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainBalanceLog(row))
	}
	return out, int64(total), nil
}

// toDomainBalanceLog ent 行 → 领域对象（只读查询面）。
func toDomainBalanceLog(row *ent.BalanceLog) *domain.BalanceLog {
	return &domain.BalanceLog{
		ID:           row.ID,
		UserID:       row.UserID,
		Amount:       row.Amount,
		BalanceAfter: row.BalanceAfter,
		Source:       domain.BalanceLogSource(row.Source),
		OperatorID:   row.OperatorID,
		Note:         row.Note,
		CreatedAt:    row.CreatedAt,
	}
}
