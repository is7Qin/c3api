// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// supplier_admin_repo.go 管理面结算审批（spec 2026-10-09 §6.3/§6.5）：五态 CAS
// （pending→approved→paying→paid / rejected；claim=approved→paying /
// confirm-failed=paying→approved）+ reject 退还 available + paid 增 lifetime_paid +
// 代申请（admin_request 复用同一通道）+ 余额列表/PATCH。
//
// 所有迁移走条件 UPDATE CAS + revision 递增。**所有资金写命令**（§6.5 I5）：
//   - 具名 JWT 操作者（domain.FundsActor）；写事务内锁操作者 users 行并复核
//     status=active / role=platform_admin / token_version == 签名请求 version；
//   - 锁序固定 **操作者 users → 目标 supplier_balances → supplier_settlements**。
//
// claim 在事务内判定付款风控门（§6.5 C1：扣费链健康探针 + 风险核对记录），
// 缺一项即失败闭合（不置 paying）。paid 只确认已核验的付款事实，**不重做**风控门。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/supplier"
)

// --- 停用 liability 校验（I6/A23，§5.5）---

// SupplierLiability 停用前必须为零的 outstanding liability 分项（§5.5）：
// liability = available + Σ chunks.amount + Σ settlements(pending|approved|paying)
// ——**必须含 available**（否则债权人无法自行提现 = 债权不可达）。另暴露 usage
// backlog 行数（未记账债权）用于拒绝启动时的分项打印。
type SupplierLiability struct {
	ChunksRows          int64
	UncreditedRows      int64
	Available           int64
	InFlightSettlements int64
}

// Zero 报告全部 liability 分项为零（可安全停用）。
func (l SupplierLiability) Zero() bool {
	return l.ChunksRows == 0 && l.UncreditedRows == 0 && l.Available == 0 && l.InFlightSettlements == 0
}

// SupplierLiability 单语句汇总停用检查所需分项（§5.5 enabled=false 启动校验）。
func (r *SupplierRepo) SupplierLiability(ctx context.Context) (SupplierLiability, error) {
	if r.pool == nil {
		return SupplierLiability{}, errSupplierNoPool
	}
	const q = `SELECT
  (SELECT COUNT(*) FROM supplier_frozen_chunks),
  (SELECT COUNT(*) FROM usage_logs WHERE NOT supplier_credited AND supplier_earn_millis > 0),
  (SELECT COALESCE(SUM(available), 0) FROM supplier_balances),
  (SELECT COUNT(*) FROM supplier_settlements WHERE status IN ('pending','approved','paying'))`
	var l SupplierLiability
	if err := r.pool.QueryRow(ctx, q).Scan(&l.ChunksRows, &l.UncreditedRows, &l.Available, &l.InFlightSettlements); err != nil {
		return l, err
	}
	return l, nil
}

// ListSupplierSettlementsAdmin 管理面结算单列表（§6.3；status/kind 可空过滤）。
func (r *SupplierRepo) ListSupplierSettlementsAdmin(ctx context.Context, filter domain.SupplierSettlementFilter, limit, offset int) ([]*domain.SupplierSettlement, int64, error) {
	if r.pool == nil {
		return nil, 0, errSupplierNoPool
	}
	limit, offset = normPage(limit, offset)
	where := "TRUE"
	args := []any{}
	if filter.Status != nil {
		args = append(args, string(*filter.Status))
		where += fmt.Sprintf(" AND status = $%d", len(args))
	}
	if filter.Kind != nil {
		args = append(args, string(*filter.Kind))
		where += fmt.Sprintf(" AND kind = $%d", len(args))
	}
	var total int64
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*)::bigint FROM supplier_settlements WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, limit, offset)
	rows, err := r.pool.Query(ctx, `SELECT `+supplierSettlementColumns+` FROM supplier_settlements WHERE `+where+
		fmt.Sprintf(" ORDER BY requested_at DESC, id DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args)), args...)
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

// ListSupplierBalances 管理面余额列表（§6.3；含最晚 available_at / bucket_rows）。
func (r *SupplierRepo) ListSupplierBalances(ctx context.Context, limit, offset int) ([]domain.SupplierBalance, int64, error) {
	if r.pool == nil {
		return nil, 0, errSupplierNoPool
	}
	limit, offset = normPage(limit, offset)
	var total int64
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*)::bigint FROM supplier_balances`).Scan(&total); err != nil {
		return nil, 0, err
	}
	const q = `SELECT b.supplier_user_id, b.available, b.lifetime_credited, b.lifetime_paid, b.share_bp, b.freeze_hours,
       c.latest_available_at, COALESCE(c.bucket_rows, 0)::bigint
FROM supplier_balances b
LEFT JOIN LATERAL (
    SELECT MAX(available_at) AS latest_available_at, COUNT(*) AS bucket_rows
    FROM supplier_frozen_chunks WHERE supplier_user_id = b.supplier_user_id
) c ON TRUE
ORDER BY b.supplier_user_id LIMIT $1 OFFSET $2`
	rows, err := r.pool.Query(ctx, q, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]domain.SupplierBalance, 0, limit)
	for rows.Next() {
		var b domain.SupplierBalance
		var shareBp, freezeHours *int32
		var latest *time.Time
		if err := rows.Scan(&b.SupplierUserID, &b.Available, &b.LifetimeCredited, &b.LifetimePaid, &shareBp, &freezeHours, &latest, &b.BucketRows); err != nil {
			return nil, 0, err
		}
		if shareBp != nil {
			v := int(*shareBp)
			b.ShareBp = &v
		}
		if freezeHours != nil {
			v := int(*freezeHours)
			b.FreezeHours = &v
		}
		b.LatestAvailableAt = latest
		out = append(out, b)
	}
	return out, total, rows.Err()
}

// GetSupplierBalance 单供应商余额（PATCH 回显）。
func (r *SupplierRepo) GetSupplierBalance(ctx context.Context, uid int64) (*domain.SupplierBalance, error) {
	if r.pool == nil {
		return nil, errSupplierNoPool
	}
	const q = `SELECT b.supplier_user_id, b.available, b.lifetime_credited, b.lifetime_paid, b.share_bp, b.freeze_hours,
       c.latest_available_at, COALESCE(c.bucket_rows, 0)::bigint
FROM supplier_balances b
LEFT JOIN LATERAL (
    SELECT MAX(available_at) AS latest_available_at, COUNT(*) AS bucket_rows
    FROM supplier_frozen_chunks WHERE supplier_user_id = b.supplier_user_id
) c ON TRUE
WHERE b.supplier_user_id = $1`
	var b domain.SupplierBalance
	var shareBp, freezeHours *int32
	var latest *time.Time
	err := r.pool.QueryRow(ctx, q, uid).Scan(&b.SupplierUserID, &b.Available, &b.LifetimeCredited, &b.LifetimePaid, &shareBp, &freezeHours, &latest, &b.BucketRows)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: supplier_user_id=%d missing", ErrNotFound, uid)
	}
	if err != nil {
		return nil, err
	}
	if shareBp != nil {
		v := int(*shareBp)
		b.ShareBp = &v
	}
	if freezeHours != nil {
		v := int(*freezeHours)
		b.FreezeHours = &v
	}
	b.LatestAvailableAt = latest
	return &b, nil
}

// PatchSupplierBalance PATCH 逐供应商配置（§6.7/§3.4：**仅** share_bp / freeze_hours，
// 不接受金额）。nil 指针 = 不变；Specified + nil 值 = 清空（回继承，落 NULL）。
func (r *SupplierRepo) PatchSupplierBalance(ctx context.Context, uid int64, shareBp *int, clearShareBp bool, freezeHours *int, clearFreezeHours bool) (*domain.SupplierBalance, error) {
	if r.pool == nil {
		return nil, errSupplierNoPool
	}
	sets := []string{}
	args := []any{uid}
	if shareBp != nil {
		args = append(args, *shareBp)
		sets = append(sets, fmt.Sprintf("share_bp = $%d", len(args)))
	} else if clearShareBp {
		sets = append(sets, "share_bp = NULL")
	}
	if freezeHours != nil {
		args = append(args, *freezeHours)
		sets = append(sets, fmt.Sprintf("freeze_hours = $%d", len(args)))
	} else if clearFreezeHours {
		sets = append(sets, "freeze_hours = NULL")
	}
	if len(sets) == 0 {
		return r.GetSupplierBalance(ctx, uid)
	}
	sets = append(sets, "updated_at = now()")
	tag, err := r.pool.Exec(ctx, `UPDATE supplier_balances SET `+strings.Join(sets, ", ")+` WHERE supplier_user_id = $1`, args...)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, fmt.Errorf("%w: supplier_user_id=%d missing", ErrNotFound, uid)
	}
	return r.GetSupplierBalance(ctx, uid)
}

// --- 五态 CAS 迁移 ---

// lockFundsOperator 在写事务内锁操作者 users 行并复核（I5）：status=active、
// role ∈ roles（缺省 = platform_admin；`supplier_request` 传供应商面可达集
// `SupplierSurfaceRoles()`）、token_version == 签名请求 version。任一不符 ⇒
// ErrFundsForbidden（service 归类 403）。缺行 ⇒ 同样拒绝。锁序固定：操作者
// users → 目标 supplier_balances → supplier_settlements（§6.5）。
func lockFundsOperator(ctx context.Context, tx pgx.Tx, actor domain.FundsActor, roles ...domain.Role) error {
	if len(roles) == 0 {
		roles = []domain.Role{domain.RolePlatformAdmin}
	}
	var role, status string
	var tv int64
	err := tx.QueryRow(ctx, `SELECT role, status, token_version FROM users WHERE id = $1 FOR UPDATE`, actor.UserID).Scan(&role, &status, &tv)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: operator %d not found", ErrFundsForbidden, actor.UserID)
	}
	if err != nil {
		return err
	}
	allowed := false
	for _, r := range roles {
		if role == string(r) {
			allowed = true
			break
		}
	}
	if !allowed || status != string(domain.UserStatusActive) {
		return fmt.Errorf("%w: operator %d role=%s status=%s", ErrFundsForbidden, actor.UserID, role, status)
	}
	if tv != actor.TokenVersion {
		return fmt.Errorf("%w: operator %d token_version mismatch", ErrFundsForbidden, actor.UserID)
	}
	return nil
}

// ErrFundsForbidden 资金操作者复核失败（service 归类 403）。
var ErrFundsForbidden = errors.New("funds operator recheck failed")

// withFundsTx 资金写事务骨架：单连接 + 显式 READ COMMITTED + 锁操作者 users 行。
func (r *SupplierRepo) withFundsTx(ctx context.Context, actor domain.FundsActor, fn func(tx pgx.Tx) error) error {
	if r.pool == nil {
		return errSupplierNoPool
	}
	ctx, cancel := context.WithTimeout(ctx, supplierTxTimeout)
	defer cancel()
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // nolint:errcheck
	if err := lockFundsOperator(ctx, tx, actor); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// readSettlementForUpdate 读取目标结算单（no-lock 取 supplier_user_id，供后续按
// 锁序预锁 balances 行；status/revision/amount 由条件 UPDATE 复核）。
func readSettlementHead(ctx context.Context, tx pgx.Tx, id int64) (supplierUID, amount, revision int64, status string, err error) {
	err = tx.QueryRow(ctx, `SELECT supplier_user_id, amount_millis, revision, status FROM supplier_settlements WHERE id = $1`, id).
		Scan(&supplierUID, &amount, &revision, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		err = fmt.Errorf("%w: settlement id=%d missing", ErrNotFound, id)
	}
	return
}

// lockSupplierBalanceRow 按锁序预锁目标供应商余额行（存在性由调用方语义决定）。
func lockSupplierBalanceRow(ctx context.Context, tx pgx.Tx, uid int64) (bool, error) {
	var got int64
	err := tx.QueryRow(ctx, `SELECT supplier_user_id FROM supplier_balances WHERE supplier_user_id = $1 FOR UPDATE`, uid).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ApproveSettlement pending → approved（§6.3 CAS）。
func (r *SupplierRepo) ApproveSettlement(ctx context.Context, id, expectedRevision int64, actor domain.FundsActor) (*domain.SupplierSettlement, error) {
	var out *domain.SupplierSettlement
	err := r.withFundsTx(ctx, actor, func(tx pgx.Tx) error {
		supplierUID, _, _, _, err := readSettlementHead(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, err := lockSupplierBalanceRow(ctx, tx, supplierUID); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `UPDATE supplier_settlements
SET status = 'approved', revision = revision + 1, reviewed_at = clock_timestamp(), reviewer_user_id = $3
WHERE id = $1 AND status = 'pending' AND revision = $2
RETURNING `+supplierSettlementColumns, id, expectedRevision, actor.UserID)
		s, err := scanSupplierSettlement(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: settlement id=%d expected revision %d stale", ErrStaleRevision, id, expectedRevision)
		}
		if err != nil {
			return err
		}
		out = s
		return nil
	})
	return out, err
}

// RejectSettlement pending|approved → rejected + **退还 available**（同事务原子；§6.3）。
// paying 不在目标集（已认领=已开始外部副作用，§6.5）。
func (r *SupplierRepo) RejectSettlement(ctx context.Context, id, expectedRevision int64, reason *string, actor domain.FundsActor) (*domain.SupplierSettlement, error) {
	var out *domain.SupplierSettlement
	err := r.withFundsTx(ctx, actor, func(tx pgx.Tx) error {
		supplierUID, amount, _, _, err := readSettlementHead(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, err := lockSupplierBalanceRow(ctx, tx, supplierUID); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `UPDATE supplier_settlements
SET status = 'rejected', revision = revision + 1, reviewed_at = clock_timestamp(), reviewer_user_id = $3, reject_reason = $4
WHERE id = $1 AND status IN ('pending','approved') AND revision = $2
RETURNING `+supplierSettlementColumns, id, expectedRevision, actor.UserID, reason)
		s, err := scanSupplierSettlement(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: settlement id=%d expected revision %d stale or not rejectable", ErrStaleRevision, id, expectedRevision)
		}
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE supplier_balances SET available = available + $2, updated_at = now() WHERE supplier_user_id = $1`, supplierUID, amount)
		if err != nil {
			return err
		}
		// 退还必须命中余额行：0 行 ⇒ 余额行缺失（`lockSupplierBalanceRow` 缺行返回
		// (false,nil)），若就此提交，则单已被打成 rejected 而在途负债静默消失、
		// available 不回（§6.3：状态迁移与退还同事务，0 行须整事务回滚）。
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: supplier_balances uid=%d missing while rejecting settlement", ErrInvalidInput, supplierUID)
		}
		out = s
		return nil
	})
	return out, err
}

// encodePayeeSnapshot 严格校验收款目标快照（结构化：收款人/账号/单位均非空）并
// 序列化为规范 JSON（§6.5 C4）：不得把自由文本当收款目标。
func encodePayeeSnapshot(p domain.SupplierPayeeSnapshot) (string, error) {
	name := strings.TrimSpace(p.PayeeName)
	account := strings.TrimSpace(p.Account)
	unit := strings.TrimSpace(p.Unit)
	if name == "" || account == "" || unit == "" {
		return "", fmt.Errorf("%w: payee_snapshot requires payee_name/account/unit", ErrInvalidInput)
	}
	b, err := json.Marshal(struct {
		PayeeName string `json:"payee_name"`
		Account   string `json:"account"`
		Unit      string `json:"unit"`
	}{PayeeName: name, Account: account, Unit: unit})
	if err != nil {
		return "", fmt.Errorf("%w: encode payee_snapshot: %v", ErrInvalidInput, err)
	}
	return string(b), nil
}

// encodePayoutFailureConfirmation 校验结构化「确定未支付」核验并序列化为 JSON
// （§6.5 C4）：具名确认人（落 reviewer_user_id）+ 确定未支付结论 + 证据 + 旧执行
// 已停止确认，任一缺失/false ⇒ 失败闭合（保留 paying）。
func encodePayoutFailureConfirmation(in domain.SupplierPayoutFailureConfirmation) (string, error) {
	reason := strings.TrimSpace(in.Reason)
	evidence := strings.TrimSpace(in.Evidence)
	if reason == "" || evidence == "" || !in.ConfirmedNotPaid || !in.OldExecutionStopped {
		return "", fmt.Errorf("%w: confirm-failed requires reason, evidence and confirmed-not-paid/old-execution-stopped", ErrInvalidInput)
	}
	b, err := json.Marshal(struct {
		Reason              string `json:"reason"`
		Evidence            string `json:"evidence"`
		ConfirmedNotPaid    bool   `json:"confirmed_not_paid"`
		OldExecutionStopped bool   `json:"old_execution_stopped"`
	}{Reason: reason, Evidence: evidence, ConfirmedNotPaid: true, OldExecutionStopped: true})
	if err != nil {
		return "", fmt.Errorf("%w: encode payout failure confirmation: %v", ErrInvalidInput, err)
	}
	return string(b), nil
}

// ClaimSettlement approved → paying（认领 CAS；§6.5 C3）：**金额必须 == 单据
// amount_millis**；结构化收款快照固定（首次认领固定、重试复用、不一致 ⇒ 拒绝）；
// 生成/复用全局唯一 payment_key；判定付款风控门；原子持久化 risk_review。
func (r *SupplierRepo) ClaimSettlement(ctx context.Context, id, expectedRevision, amountMillis int64, payee domain.SupplierPayeeSnapshot, riskEvidence string, actor domain.FundsActor) (*domain.SupplierSettlement, error) {
	if amountMillis <= 0 {
		return nil, fmt.Errorf("%w: amount_millis must be > 0", ErrInvalidInput)
	}
	if strings.TrimSpace(riskEvidence) == "" {
		return nil, fmt.Errorf("%w: risk evidence is required", ErrInvalidInput)
	}
	payeeSnapshot, err := encodePayeeSnapshot(payee)
	if err != nil {
		return nil, err
	}
	var out *domain.SupplierSettlement
	err = r.withFundsTx(ctx, actor, func(tx pgx.Tx) error {
		supplierUID, amount, _, _, err := readSettlementHead(ctx, tx, id)
		if err != nil {
			return err
		}
		// claim 金额 == 单据金额（§6.5 C2）：不符 ⇒ 400，不得凭错金额认领。
		if amountMillis != amount {
			return fmt.Errorf("%w: claim amount %d != settlement amount %d", ErrInvalidInput, amountMillis, amount)
		}
		if _, err := lockSupplierBalanceRow(ctx, tx, supplierUID); err != nil {
			return err
		}
		// payment_key / payee_snapshot 永久固定（§6.5 C2/I1）：首次认领落定，后续
		// 认领/恢复复用同键同收款目标；收款目标不一致 ⇒ 拒绝（不得静默更换）。
		var existingKey, existingPayee *string
		if err := tx.QueryRow(ctx,
			`SELECT payment_key, payee_snapshot FROM supplier_settlements WHERE id = $1`, id).
			Scan(&existingKey, &existingPayee); err != nil {
			return err
		}
		paymentKey := uuid.NewString()
		if existingKey != nil && *existingKey != "" {
			paymentKey = *existingKey
		}
		if existingPayee != nil && *existingPayee != "" && *existingPayee != payeeSnapshot {
			return fmt.Errorf("%w: payee_snapshot mismatch on re-claim", ErrInvalidInput)
		}
		// 付款风控门（锁等待后以新取 clock_timestamp() 复核年龄）。
		probe, err := r.billingProbe(ctx, tx)
		if err != nil {
			return err
		}
		var dbNow time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dbNow); err != nil {
			return err
		}
		cfg := supplier.PayoutGateConfig{
			BillingEnabled: r.cfg.BillingEnabled,
			MaxBacklogRows: r.cfg.PayoutMaxBacklogRows,
			MaxBacklogAge:  r.cfg.PayoutMaxBacklogAge,
			MaxObserveAge:  r.cfg.PayoutMaxObserveAge,
		}
		if err := supplier.EvaluatePayoutGate(cfg, probe, dbNow); err != nil {
			return err
		}
		riskReview, err := buildRiskReview(actor, riskEvidence, expectedRevision, dbNow, r.cfg.RiskReviewMaxAge)
		if err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `UPDATE supplier_settlements
SET status = 'paying', revision = revision + 1, payout_operator_user_id = $3,
    payout_started_at = clock_timestamp(), payment_key = $4, payee_snapshot = $5, risk_review = $6
WHERE id = $1 AND status = 'approved' AND revision = $2
RETURNING `+supplierSettlementColumns, id, expectedRevision, actor.UserID, paymentKey, payeeSnapshot, riskReview)
		s, err := scanSupplierSettlement(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: settlement id=%d expected revision %d stale or not claimable", ErrStaleRevision, id, expectedRevision)
		}
		if err != nil {
			return err
		}
		out = s
		return nil
	})
	return out, err
}

// ConfirmFailedSettlement paying → approved（**仅接受结构化「确定未支付」**的核验
// 结果；网络失败不算；具名确认人落 reviewer_user_id，留证 payout_failure_reason +
// payout_failed_at）。§6.5。未知/无法确认 ⇒ 失败闭合（保持 paying）。
func (r *SupplierRepo) ConfirmFailedSettlement(ctx context.Context, id, expectedRevision int64, in domain.SupplierPayoutFailureConfirmation, actor domain.FundsActor) (*domain.SupplierSettlement, error) {
	record, err := encodePayoutFailureConfirmation(in)
	if err != nil {
		return nil, err
	}
	var out *domain.SupplierSettlement
	err = r.withFundsTx(ctx, actor, func(tx pgx.Tx) error {
		supplierUID, _, _, _, err := readSettlementHead(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, err := lockSupplierBalanceRow(ctx, tx, supplierUID); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `UPDATE supplier_settlements
SET status = 'approved', revision = revision + 1, payout_failure_reason = $3, payout_failed_at = clock_timestamp(), reviewer_user_id = $4
WHERE id = $1 AND status = 'paying' AND revision = $2
RETURNING `+supplierSettlementColumns, id, expectedRevision, record, actor.UserID)
		s, err := scanSupplierSettlement(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: settlement id=%d expected revision %d stale or not paying", ErrStaleRevision, id, expectedRevision)
		}
		if err != nil {
			return err
		}
		out = s
		return nil
	})
	return out, err
}

// PaidSettlement paying → paid（**仅** paying；§6.3）：**external_ref 必须非空**
// （未核验不得确认）；available 不变（申请已扣），lifetime_paid += amount，记
// paid_operator_user_id / paid_at / external_ref。不重做风控门（已核验付款事实）。
func (r *SupplierRepo) PaidSettlement(ctx context.Context, id, expectedRevision int64, externalRef string, actor domain.FundsActor) (*domain.SupplierSettlement, error) {
	ref := strings.TrimSpace(externalRef)
	if ref == "" {
		return nil, fmt.Errorf("%w: external_ref is required to confirm paid", ErrInvalidInput)
	}
	var out *domain.SupplierSettlement
	err := r.withFundsTx(ctx, actor, func(tx pgx.Tx) error {
		supplierUID, amount, _, _, err := readSettlementHead(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, err := lockSupplierBalanceRow(ctx, tx, supplierUID); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `UPDATE supplier_settlements
SET status = 'paid', revision = revision + 1, paid_at = clock_timestamp(), paid_operator_user_id = $3, external_ref = $4
WHERE id = $1 AND status = 'paying' AND revision = $2
RETURNING `+supplierSettlementColumns, id, expectedRevision, actor.UserID, ref)
		s, err := scanSupplierSettlement(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: settlement id=%d expected revision %d stale or not paying", ErrStaleRevision, id, expectedRevision)
		}
		if err != nil {
			return err
		}
		// lifetime_paid 累加（available 不变）。
		tag, err := tx.Exec(ctx, `UPDATE supplier_balances SET lifetime_paid = lifetime_paid + $2, updated_at = now() WHERE supplier_user_id = $1`, supplierUID, amount)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: supplier_balances uid=%d missing while settling paid", ErrInvalidInput, supplierUID)
		}
		out = s
		return nil
	})
	return out, err
}

// AdminApplySettlement 代申请（§6.3 admin_request）：与 supplier_request 共用同一
// 条件扣/行锁/期间规则/五态流程，**仅 requested_operator = 管理员 uid**（§2.7-4）。
// 目标须已有 supplier_balances 行（否则 404）——代申请不给无余额的新供应商创单。
// 写事务内复核操作者（I5）。
func (r *SupplierRepo) AdminApplySettlement(ctx context.Context, req domain.ApplySettlementRequest, actor domain.FundsActor) (*domain.SupplierSettlement, error) {
	if req.AmountMillis <= 0 || req.RequestKey == "" || req.SupplierUID <= 0 {
		return nil, fmt.Errorf("%w: amount_millis must be > 0 and request_key required", ErrInvalidInput)
	}
	if actor.UserID != req.OperatorUID {
		return nil, fmt.Errorf("%w: operator mismatch", ErrFundsForbidden)
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
	if err := lockFundsOperator(ctx, tx, actor); err != nil {
		return nil, err
	}
	// 目标余额行存在性（404）+ 预锁（锁序：操作者 users → 目标 balances 行）。
	exists, err := lockSupplierBalanceRow(ctx, tx, req.SupplierUID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%w: supplier_user_id=%d has no balance row", ErrNotFound, req.SupplierUID)
	}
	s, err := applySettlementTx(ctx, tx, req)
	if err != nil {
		var kc errSettlementKeyConflict
		if errors.As(err, &kc) {
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

// billingProbe 扣费链健康探针（§6.5 新专用探针，单语句快照）：backlog 行数 +
// 队头 created_at + 采样时刻（statement_timestamp()，非长聚合完成时）。
func (r *SupplierRepo) billingProbe(ctx context.Context, tx pgx.Tx) (supplier.BillingProbe, error) {
	var p supplier.BillingProbe
	const q = `SELECT count(*)::bigint, MIN(created_at), statement_timestamp()
FROM usage_logs WHERE NOT billed AND error_type IN ('none','abort') AND cost > 0`
	if err := tx.QueryRow(ctx, q).Scan(&p.BacklogRows, &p.OldestCreatedAt, &p.ObservedAt); err != nil {
		return p, fmt.Errorf("billing health probe: %w", err)
	}
	return p, nil
}

// buildRiskReview 结构化 risk_review（§6.5 严格 JSON）：服务端派生 operator_id
// （取资金 Actor）/ decided_at（DB 时刻）/ scope（固定 platform_credit_risk）/
// expires_at（decided_at + max_age），绑定 approved_revision；evidence 由客户端
// 提供（非空 reference）。**落库前对构建出的记录做严格反序列化 + 校验
// （round-trip）**——任何非良构记录失败闭合（A22 拒绝矩阵）。返回持久化文本。
func buildRiskReview(actor domain.FundsActor, evidence string, expectedRevision int64, decidedAt time.Time, maxAge time.Duration) (string, error) {
	rr, err := supplier.BuildRiskReview(actor.UserID, evidence, expectedRevision, decidedAt, maxAge)
	if err != nil {
		return "", err
	}
	raw, err := supplier.MarshalRiskReview(rr)
	if err != nil {
		return "", err
	}
	if err := supplier.ParseAndValidateRiskReview(raw, actor.UserID, expectedRevision, decidedAt, maxAge); err != nil {
		return "", err
	}
	return raw, nil
}
