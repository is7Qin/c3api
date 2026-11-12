// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// order_repo.go 支付订单持久化（spec 支付网关 §4-1/§4-4）。
//
// 两条面：
//   - 非事务面（Store）：建单、读单/列表、失败记录、关单（单语句原子）、人工处置标记。
//   - 事务面（TxStore）：LockOrderByOutTradeNo（FOR UPDATE 行锁）+ MarkOrderPaid
//     （条件迁移，0 行 → ErrConflict）。**条件更新是纵深防御，不是主判据**（F37）：
//     主判据是行锁 + 状态 switch；WHERE status='pending' 只兜绕过锁的异常路径。

import (
	"context"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqlgraph"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/order"
)

// orderListLimitMax 管理面订单列表 limit 上限（§7-3：默认 20、上限 100）。
const orderListLimitMax = 100

// OrderQuery 订单列表筛选（§7-3 管理面 / §7-1 用户面）：
// 零值 = 不过滤；Limit <=0 → 20，>100 → 100；Offset <0 → 0。
type OrderQuery struct {
	UserID      int64 // 0 = 全部；用户面由 service 强制 = 本人
	Status      *domain.OrderStatus
	OutTradeNo  string
	CreatedFrom *time.Time // created_at >= (nil = 不限)
	CreatedTo   *time.Time // created_at <= (nil = 不限)
	Limit       int
	Offset      int
}

// OrderRepo 支付订单持久化。
type OrderRepo struct {
	client *ent.Client
	// driver 为 raw SQL（行锁 SELECT ... FOR UPDATE）用：ent 生成器无 FOR UPDATE
	// 入口（与 ownership.go/ext_repo.go 同款先例）；WithTx 内为事务驱动。
	driver dialect.Driver
}

// orderAccountDefault 已随 v1 的 `account` 列一起作废（v2 的账户快照是
// `account_key`，由选路结果提供，**没有**"默认上游"这回事）。

// CreateOrder 插入订单（pending）。out_trade_no / redemption_code_id 唯一冲突 →
// ErrConflict（service 层换单号重试 ≤3；码列在建单时为 NULL，不参与冲突）。
//
// `account_key` 是 v2 的账户快照（NOT NULL，**无默认值**）：建单必须由选路结果
// 提供——空值说明调用方没走 ResolveChannel，直接 ErrInvalidInput（不静默落任何
// 默认账户："有人用了别的上游的钱"必然导致对账错位）。
func (r *OrderRepo) CreateOrder(ctx context.Context, o *domain.Order) (*domain.Order, error) {
	if o.AccountKey == "" {
		return nil, fmt.Errorf("%w: order %q requires account_key snapshot", ErrInvalidInput, o.OutTradeNo)
	}
	b := r.client.Order.Create().
		SetOutTradeNo(o.OutTradeNo).
		SetUserID(o.UserID).
		SetProvider(string(o.Provider)).
		SetAccountKey(o.AccountKey).
		SetStatus(order.Status(o.Status)).
		SetCreditMilli(o.CreditMilli).
		SetPayMinor(o.PayMinor).
		SetCurrency(o.Currency).
		SetRateMicro(o.RateMicro).
		SetPayType(o.PayType).
		SetExpiresAt(o.ExpiresAt).
		SetNillableTradeNo(o.TradeNo).
		SetNillableRedemptionCodeID(o.RedemptionCodeID).
		SetNillableRawNotify(o.RawNotify).
		SetNillableFailReason(o.FailReason).
		SetNillableLastNotifyAt(o.LastNotifyAt).
		SetNillableResolvedAt(o.ResolvedAt).
		SetNillablePaidAt(o.PaidAt).
		SetFailCount(o.FailCount)
	row, err := b.Save(ctx)
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: out_trade_no=%q", ErrConflict, o.OutTradeNo)
		}
		return nil, err
	}
	return toDomainOrder(row), nil
}

// GetOrderByID 按 id 取订单；缺失 → ErrNotFound（含 id）。
func (r *OrderRepo) GetOrderByID(ctx context.Context, id int64) (*domain.Order, error) {
	row, err := r.client.Order.Get(ctx, id)
	if err != nil {
		return nil, errMissingID(err, id)
	}
	return toDomainOrder(row), nil
}

// ListOrders 分页/筛选（user_id/status/out_trade_no/created 区间）；默认 id 倒序
// （新单在前，管理面与用户面同一顺序契约）。status 非法枚举 → ErrInvalidInput。
func (r *OrderRepo) ListOrders(ctx context.Context, q OrderQuery) ([]*domain.Order, int64, error) {
	pred := r.client.Order.Query()
	if q.UserID != 0 {
		pred = pred.Where(order.UserIDEQ(q.UserID))
	}
	if q.Status != nil {
		if !q.Status.Valid() {
			return nil, 0, fmt.Errorf("%w: status=%q", ErrInvalidInput, *q.Status)
		}
		pred = pred.Where(order.StatusEQ(order.Status(*q.Status)))
	}
	if q.OutTradeNo != "" {
		pred = pred.Where(order.OutTradeNoEQ(q.OutTradeNo))
	}
	if q.CreatedFrom != nil {
		pred = pred.Where(order.CreatedAtGTE(*q.CreatedFrom))
	}
	if q.CreatedTo != nil {
		pred = pred.Where(order.CreatedAtLTE(*q.CreatedTo))
	}
	total, err := pred.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.Limit > orderListLimitMax {
		q.Limit = orderListLimitMax
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	rows, err := pred.Order(ent.Desc(order.FieldID)).Offset(q.Offset).Limit(q.Limit).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*domain.Order, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainOrder(row))
	}
	return out, int64(total), nil
}

// MarkOrderNotifyFailure 记录一次通知失败（**非事务**，由调用方在 CompleteOrder
// 返回错误后调用）：
//
//	UPDATE orders
//	   SET fail_reason = COALESCE(fail_reason, $1),   -- 只在为空时写：**首因不被覆盖**
//	       fail_count   = fail_count + 1,             -- 计数/时间戳每次都刷新
//	       last_notify_at = now()
//	 WHERE out_trade_no = $2 AND status <> 'paid'
//
// WHERE **不含 status='pending'**（F36：否则 closed 分支永远写不进 → 已付款未入账
// 静默）；`status <> 'paid'` 排除已入账订单，避免污染。
// **验签/重复键/未知单号失败不得调用**（单号未验证，可被伪造签名污染任意订单）。
//
// 为什么 `fail_reason` 是 COALESCE 而不是赋值（§19-3 / P58）：强制关单先写
// `fail_reason='forced'`，之后网关的迟到重试会带着 `ErrOrderClosed` 再进来一次。
// 无条件赋值会把 `forced` 覆盖成 `closed`——事后查账只剩"关单后又来了个回调"，
// 看不到"为什么关单"。首因才是审计要的东西，而 `fail_count` / `last_notify_at`
// 仍然每次刷新（它们是"这单还在持续失败"的活性信号）。
//
// 为什么用 raw builder 而不是 ent 的 `SetFailReason`（与 user_repo 的原子增减同款
// 先例）：ent 生成代码的字段 setter 只有无条件赋值；条件写只能用 SQL 表达式，
// 而这里必须保持**单语句**（非事务面，读改写会重新引入竞态）。
func (r *OrderRepo) MarkOrderNotifyFailure(ctx context.Context, outTradeNo, reason string) error {
	u := entsql.Update(order.Table).
		Set(order.FieldFailReason, entsql.ExprFunc(func(b *entsql.Builder) {
			b.WriteString("COALESCE(").Ident(order.FieldFailReason).WriteString(", ").Arg(reason).WriteString(")")
		})).
		Add(order.FieldFailCount, 1).
		Set(order.FieldLastNotifyAt, time.Now()).
		Where(entsql.And(
			entsql.EQ(order.FieldOutTradeNo, outTradeNo),
			entsql.NEQ(order.FieldStatus, string(order.StatusPaid)),
		))
	_, err := execUpdate(ctx, r.driver, u)
	return err
}

// CloseOrder 关单（单语句原子，§4-2：close ∥ notify 互斥）：
// WHERE id=? AND status='pending'；0 行 → (false, nil)（service 映射 409）。
// failReason nil = 不动 fail_reason（正常关单）；强制关单传 forced。
func (r *OrderRepo) CloseOrder(ctx context.Context, id int64, failReason *string) (bool, error) {
	u := r.client.Order.Update().
		Where(order.IDEQ(id), order.StatusEQ(order.StatusPending)).
		SetStatus(order.StatusClosed)
	if failReason != nil {
		u = u.SetFailReason(*failReason)
	}
	n, err := u.Save(ctx)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ResolveOrder 人工处置标记（§7-3 / F48）：写 resolved_at 防同一订单被补两次。
// 条件 `resolved_at IS NULL`：已标记 → (false, nil)（管理面 409，B41）。
func (r *OrderRepo) ResolveOrder(ctx context.Context, id int64) (bool, error) {
	n, err := r.client.Order.Update().
		Where(order.IDEQ(id), order.ResolvedAtIsNil()).
		SetResolvedAt(time.Now()).
		Save(ctx)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// UpdateOrderRawNotify 只更新 raw_notify（sync 遇到非 paid 状态时"只写 raw_notify
// 与审计"，§7-3）：不迁移状态、不碰 fail_reason。
func (r *OrderRepo) UpdateOrderRawNotify(ctx context.Context, id int64, rawNotify *string) error {
	u := r.client.Order.UpdateOneID(id)
	if rawNotify != nil {
		u = u.SetRawNotify(*rawNotify)
	} else {
		u = u.ClearRawNotify()
	}
	if err := u.Exec(ctx); err != nil {
		return errMissingID(err, id)
	}
	return nil
}

// LockOrderByOutTradeNo 事务内按单号加行锁读整行（CompleteOrder 的唯一入口）：
// 先 `SELECT id ... FOR UPDATE`（ent 无 FOR UPDATE 入口），再在同一事务连接上
// 用 ent 读整行。缺行 → ErrNotFound（service 换成 domain.ErrOrderNotFound）。
//
// **必须在 WithTx 内调用**：锁随事务提交/回滚释放，非事务面调用等于没锁。
func (r *OrderRepo) LockOrderByOutTradeNo(ctx context.Context, outTradeNo string) (*domain.Order, error) {
	const q = `SELECT id FROM orders WHERE out_trade_no = $1 FOR UPDATE`
	rows := &entsql.Rows{}
	if err := r.driver.Query(ctx, q, []any{outTradeNo}, rows); err != nil {
		return nil, err
	}
	if !rows.Next() {
		err := rows.Err()
		rows.Close() // nolint:errcheck // 已判定无行，关闭错误不改变判决
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: out_trade_no=%q", ErrNotFound, outTradeNo)
	}
	var id int64
	scanErr := rows.Scan(&id)
	// 同一事务单连接：必须关掉游标才能发下一条查询。
	closeErr := rows.Close()
	if scanErr != nil {
		return nil, scanErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return r.GetOrderByID(ctx, id)
}

// MarkOrderPaid 置 paid 并绑定到账凭据（条件迁移）：
// WHERE id=? AND status='pending' 命中 0 行 → ErrConflict（重试/人工）。
// trade_no / raw_notify 随订单一起落库（审计与对账依据）。
func (r *OrderRepo) MarkOrderPaid(ctx context.Context, id, codeID int64, tradeNo string, rawNotify *string) error {
	u := r.client.Order.Update().
		Where(order.IDEQ(id), order.StatusEQ(order.StatusPending)).
		SetStatus(order.StatusPaid).
		SetRedemptionCodeID(codeID).
		SetTradeNo(tradeNo).
		SetPaidAt(time.Now())
	if rawNotify != nil {
		u = u.SetRawNotify(*rawNotify)
	}
	n, err := u.Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: order id=%d is not pending", ErrConflict, id)
	}
	return nil
}

func toDomainOrder(o *ent.Order) *domain.Order {
	return &domain.Order{
		ID: o.ID, OutTradeNo: o.OutTradeNo, UserID: o.UserID,
		Provider: domain.ProviderName(o.Provider), Status: domain.OrderStatus(o.Status),
		AccountKey:  o.AccountKey,
		CreditMilli: o.CreditMilli, PayMinor: o.PayMinor, Currency: o.Currency,
		RateMicro: o.RateMicro, PayType: o.PayType,
		TradeNo: o.TradeNo, RedemptionCodeID: o.RedemptionCodeID,
		RawNotify: o.RawNotify, FailReason: o.FailReason, FailCount: o.FailCount,
		LastNotifyAt: o.LastNotifyAt, ResolvedAt: o.ResolvedAt,
		CreatedAt: o.CreatedAt, PaidAt: o.PaidAt, ExpiresAt: o.ExpiresAt,
	}
}

// --- Repository 门面（service.OrderStore 的接线点） ---

func (r *Repository) CreateOrder(ctx context.Context, o *domain.Order) (*domain.Order, error) {
	return r.Orders.CreateOrder(ctx, o)
}

func (r *Repository) GetOrderByID(ctx context.Context, id int64) (*domain.Order, error) {
	return r.Orders.GetOrderByID(ctx, id)
}

func (r *Repository) ListOrders(ctx context.Context, q OrderQuery) ([]*domain.Order, int64, error) {
	return r.Orders.ListOrders(ctx, q)
}

func (r *Repository) MarkOrderNotifyFailure(ctx context.Context, outTradeNo, reason string) error {
	return r.Orders.MarkOrderNotifyFailure(ctx, outTradeNo, reason)
}

func (r *Repository) CloseOrder(ctx context.Context, id int64, failReason *string) (bool, error) {
	return r.Orders.CloseOrder(ctx, id, failReason)
}

func (r *Repository) ResolveOrder(ctx context.Context, id int64) (bool, error) {
	return r.Orders.ResolveOrder(ctx, id)
}

func (r *Repository) UpdateOrderRawNotify(ctx context.Context, id int64, rawNotify *string) error {
	return r.Orders.UpdateOrderRawNotify(ctx, id, rawNotify)
}

// --- 事务面（repository.TxStore：完成订单的唯一入账路径） ---

func (r *Repository) LockOrderByOutTradeNo(ctx context.Context, outTradeNo string) (*domain.Order, error) {
	return r.Orders.LockOrderByOutTradeNo(ctx, outTradeNo)
}

func (r *Repository) MarkOrderPaid(ctx context.Context, id, codeID int64, tradeNo string, rawNotify *string) error {
	return r.Orders.MarkOrderPaid(ctx, id, codeID, tradeNo, rawNotify)
}
