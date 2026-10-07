// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// supplier_business_repo.go 供应商业务面持久化（spec 2026-10-09 §6.1/§6.2）：
//   - overview：余额 + 冻结桶聚合 + 生效 share_bp/freeze_hours；
//   - earnings：区间收益明细（走报表索引 ② (supplier_user_id, created_at)）；
//   - chunks：未解冻桶；
//   - settlements：我的结算单列表；
//   - apply：单事务条件扣 available + 期间链 + request_key 幂等（I3）。

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/supplier"
)

// SupplierOverview 供应商收益概览（§6.1）。
func (r *SupplierRepo) SupplierOverview(ctx context.Context, uid int64) (*domain.SupplierOverview, error) {
	if r.pool == nil {
		return nil, errSupplierNoPool
	}
	const q = `SELECT
  COALESCE(b.available, 0)::bigint,
  COALESCE(b.lifetime_credited, 0)::bigint,
  COALESCE(b.lifetime_paid, 0)::bigint,
  b.share_bp,
  b.freeze_hours,
  COALESCE(c.frozen_amount, 0)::bigint,
  c.latest_available_at,
  COALESCE(c.bucket_rows, 0)::bigint
FROM (SELECT 1) AS one
LEFT JOIN supplier_balances b ON b.supplier_user_id = $1
LEFT JOIN LATERAL (
    SELECT SUM(amount) AS frozen_amount, MAX(available_at) AS latest_available_at, COUNT(*) AS bucket_rows
    FROM supplier_frozen_chunks WHERE supplier_user_id = $1
) c ON TRUE`
	var (
		ov           domain.SupplierOverview
		shareBp      *int32
		freezeHours  *int32
		latest       *time.Time
		frozenAmount int64
		bucketRows   int64
		available    int64
		lifetimeCred int64
		lifetimePaid int64
	)
	if err := r.pool.QueryRow(ctx, q, uid).Scan(&available, &lifetimeCred, &lifetimePaid, &shareBp, &freezeHours, &frozenAmount, &latest, &bucketRows); err != nil {
		return nil, err
	}
	ov.Available = available
	ov.LifetimeCredited = lifetimeCred
	ov.LifetimePaid = lifetimePaid
	ov.FrozenAmount = frozenAmount
	ov.LatestAvailableAt = latest
	ov.BucketRows = bucketRows
	// 生效 share_bp：无行 / NULL ⇒ 默认；显式 0 保 0（§2.3）。
	if shareBp == nil {
		ov.ShareBp = r.cfg.ShareBpDefault
	} else {
		ov.ShareBp = int(*shareBp)
	}
	// 生效 freeze_hours：全局开关优先（§2.4）。
	var fh *int
	if freezeHours != nil {
		v := int(*freezeHours)
		fh = &v
	}
	ov.FreezeHours = supplier.EffectiveFreezeHours(r.cfg.FreezeEnabled, fh, r.cfg.FreezeHoursDefault)
	return &ov, nil
}

// SupplierChunks 未解冻桶（§6.1）。
func (r *SupplierRepo) SupplierChunks(ctx context.Context, uid int64, limit, offset int) ([]domain.SupplierChunk, int64, error) {
	if r.pool == nil {
		return nil, 0, errSupplierNoPool
	}
	limit, offset = normPage(limit, offset)
	var total int64
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*)::bigint FROM supplier_frozen_chunks WHERE supplier_user_id = $1`, uid).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.pool.Query(ctx, `SELECT available_at, amount FROM supplier_frozen_chunks WHERE supplier_user_id = $1 ORDER BY available_at LIMIT $2 OFFSET $3`, uid, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]domain.SupplierChunk, 0, limit)
	for rows.Next() {
		var c domain.SupplierChunk
		if err := rows.Scan(&c.AvailableAt, &c.Amount); err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

// SupplierEarnings 区间收益明细（走报表索引 ②；§6.1）。
func (r *SupplierRepo) SupplierEarnings(ctx context.Context, uid int64, limit, offset int) ([]domain.SupplierEarning, int64, error) {
	if r.pool == nil {
		return nil, 0, errSupplierNoPool
	}
	limit, offset = normPage(limit, offset)
	var total int64
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*)::bigint FROM usage_logs WHERE supplier_user_id = $1 AND supplier_earn_millis > 0`, uid).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.pool.Query(ctx, `SELECT created_at, cost, supplier_earn_millis FROM usage_logs WHERE supplier_user_id = $1 AND supplier_earn_millis > 0 ORDER BY created_at DESC LIMIT $2 OFFSET $3`, uid, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]domain.SupplierEarning, 0, limit)
	for rows.Next() {
		var e domain.SupplierEarning
		if err := rows.Scan(&e.CreatedAt, &e.Cost, &e.EarnMillis); err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

const supplierSettlementColumns = `id, supplier_user_id, kind, amount_millis, period_start, period_end, status, revision,
  request_key, requested_at, requested_operator, reviewed_at, reviewer_user_id, payout_operator_user_id,
  payout_started_at, external_ref, paid_at, paid_operator_user_id, payout_failure_reason, payout_failed_at, note, reject_reason`

type scanTarget interface{ Scan(dest ...any) error }

func scanSupplierSettlement(s scanTarget) (*domain.SupplierSettlement, error) {
	var x domain.SupplierSettlement
	var kind, status string
	err := s.Scan(&x.ID, &x.SupplierUserID, &kind, &x.AmountMillis, &x.PeriodStart, &x.PeriodEnd, &status, &x.Revision,
		&x.RequestKey, &x.RequestedAt, &x.RequestedOperator, &x.ReviewedAt, &x.ReviewerUserID, &x.PayoutOperatorUserID,
		&x.PayoutStartedAt, &x.ExternalRef, &x.PaidAt, &x.PaidOperatorUserID, &x.PayoutFailureReason, &x.PayoutFailedAt, &x.Note, &x.RejectReason)
	if err != nil {
		return nil, err
	}
	x.Kind = domain.SupplierSettlementKind(kind)
	x.Status = domain.SupplierSettlementStatus(status)
	return &x, nil
}

// ListSupplierSettlements 我的结算单列表（§6.1）。
func (r *SupplierRepo) ListSupplierSettlements(ctx context.Context, uid int64, limit, offset int) ([]*domain.SupplierSettlement, int64, error) {
	if r.pool == nil {
		return nil, 0, errSupplierNoPool
	}
	limit, offset = normPage(limit, offset)
	var total int64
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*)::bigint FROM supplier_settlements WHERE supplier_user_id = $1`, uid).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.pool.Query(ctx, `SELECT `+supplierSettlementColumns+` FROM supplier_settlements WHERE supplier_user_id = $1 ORDER BY id DESC LIMIT $2 OFFSET $3`, uid, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]*domain.SupplierSettlement, 0, limit)
	for rows.Next() {
		s, err := scanSupplierSettlement(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, s)
	}
	return out, total, rows.Err()
}

// ApplySettlement 申请结算单事务（§6.2；RC + clock_timestamp + request_key 幂等 I3）：
//
//	⓪ 扣款前按 key 预查（命中 ⇒ 校验参数 ⇒ 返回原单 / 409）；
//	① 条件扣（UPDATE ... WHERE available >= amount RETURNING 的 FEFO idiom）；
//	   0 行 ⇒ ①′ 用新 RC 语句重查 key；
//	② 独立语句读期间起点（必须在 ① 之后，不得合并）；
//	③ INSERT（status=pending, revision=1）；并发同 key 唯一冲突 ⇒ 回滚再读回原单。
func (r *SupplierRepo) ApplySettlement(ctx context.Context, req domain.ApplySettlementRequest) (*domain.SupplierSettlement, error) {
	if req.AmountMillis <= 0 || req.RequestKey == "" || req.OperatorUID <= 0 || req.SupplierUID <= 0 {
		return nil, ErrInvalidInput
	}
	if r.pool == nil {
		return nil, errSupplierNoPool
	}
	ctx, cancel := context.WithTimeout(ctx, supplierTxTimeout)
	defer cancel()
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) // nolint:errcheck

	// ⓪ 幂等预查（扣款之前）。
	if s, err := r.readSettlementByKey(ctx, tx, req.OperatorUID, req.RequestKey); err != nil {
		return nil, err
	} else if s != nil {
		if err := matchSettlement(s, req); err != nil {
			return nil, err
		}
		return s, tx.Commit(ctx)
	}

	// ① 条件扣（行锁获取点与串行化点）。
	tag, err := tx.Exec(ctx, `UPDATE supplier_balances SET available = available - $2, updated_at = now()
WHERE supplier_user_id = $1 AND available >= $2`, req.SupplierUID, req.AmountMillis)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		// ①′ 用新 RC 语句重查 key（提交成功但响应丢失后的重试会走到这里）。
		s, err := r.readSettlementByKey(ctx, tx, req.OperatorUID, req.RequestKey)
		if err != nil {
			return nil, err
		}
		if s != nil {
			if err := matchSettlement(s, req); err != nil {
				return nil, err
			}
			return s, tx.Commit(ctx)
		}
		// 余额不足 或 无余额行 ⇒ 400（金额校验）。
		return nil, ErrInvalidInput
	}

	// ② 独立语句读期间起点（① 之后；不得合并进 ①）。
	var periodStart time.Time
	if err := tx.QueryRow(ctx, `SELECT COALESCE(
    (SELECT MAX(period_end) FROM supplier_settlements WHERE supplier_user_id = $1),
    (SELECT created_at FROM supplier_balances WHERE supplier_user_id = $1),
    clock_timestamp())`, req.SupplierUID).Scan(&periodStart); err != nil {
		return nil, err
	}

	// ③ INSERT（period_end = GREATEST(start, clock_timestamp())）。
	row := tx.QueryRow(ctx, `INSERT INTO supplier_settlements
  (supplier_user_id, kind, amount_millis, period_start, period_end, status, revision, request_key, requested_at, requested_operator, note)
VALUES ($1, $2, $3, $4, GREATEST($4, clock_timestamp()), 'pending', 1, $5, clock_timestamp(), $6, $7)
RETURNING `+supplierSettlementColumns,
		req.SupplierUID, string(req.Kind), req.AmountMillis, periodStart, req.RequestKey, req.OperatorUID, req.Note)
	s, err := scanSupplierSettlement(row)
	if err != nil {
		if isUniqueViolation(err) {
			// 并发同 key 唯一冲突 ⇒ 回滚整事务，再按 key 读回原单。
			_ = tx.Rollback(ctx)
			orig, ferr := r.getSettlementByKey(ctx, req.OperatorUID, req.RequestKey)
			if ferr != nil {
				return nil, ferr
			}
			if orig == nil {
				return nil, err
			}
			if merr := matchSettlement(orig, req); merr != nil {
				return nil, merr
			}
			return orig, nil
		}
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// readSettlementByKey 事务内按 (operator, key) 读结算单（命中 ⇒ 返回，缺 ⇒ nil）。
func (r *SupplierRepo) readSettlementByKey(ctx context.Context, tx pgx.Tx, operator int64, key string) (*domain.SupplierSettlement, error) {
	row := tx.QueryRow(ctx, `SELECT `+supplierSettlementColumns+` FROM supplier_settlements WHERE requested_operator = $1 AND request_key = $2`, operator, key)
	s, err := scanSupplierSettlement(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return s, err
}

// getSettlementByKey 池上按 (operator, key) 读结算单（唯一冲突回滚后读回原单）。
func (r *SupplierRepo) getSettlementByKey(ctx context.Context, operator int64, key string) (*domain.SupplierSettlement, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+supplierSettlementColumns+` FROM supplier_settlements WHERE requested_operator = $1 AND request_key = $2`, operator, key)
	s, err := scanSupplierSettlement(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return s, err
}

// matchSettlement 同 key 关键参数校验：一致 ⇒ nil；不一致（金额/目标供应商/kind）⇒ ErrConflict（409）。
func matchSettlement(s *domain.SupplierSettlement, req domain.ApplySettlementRequest) error {
	if s.Kind != req.Kind || s.SupplierUserID != req.SupplierUID || s.AmountMillis != req.AmountMillis {
		return fmt.Errorf("%w: request_key reused with different parameters", ErrConflict)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func normPage(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
