// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// supplier_repo.go 供应商记账链 / 解冻链的真实持久面（spec 2026-10-09 §5.1/§5.2/
// §5.3/§5.4/§5.7）：
//
//   - 取批（消费索引①，双谓词 `NOT supplier_credited AND supplier_earn_millis > 0`）
//   - 记账单事务：阶段 A chunks → B 按 uid 升序预锁 balances → C 建行+累加 →
//     封账 upsert → D 标记 usage_logs；两条守卫（余额覆盖 + 标记计数）+ 金额守恒。
//   - 解冻：四步独立顺序（DELETE RETURNING → 按 uid 升序预锁 → UPDATE + 覆盖守卫）。
//   - lag：头行高频（走索引①取头行）+ 全量降频（COUNT/SUM）。
//
// 事务载体：pgx 直连单连接显式事务（形态参考 settlePGX）；pool 未注入 ⇒ 显式
// 错误（记账链写路径不可缺）。所有 raw SQL 与 ent 建表同库。

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/supplier"
)

// supplierTxTimeout 单个记账/解冻事务预算（per-batch 有界 ctx，§5.7）。
const supplierTxTimeout = 10 * time.Second

// SupplierRepoConfig 供应商持久面配置（记账链读取的生效默认；§2.4/§2.3/§6.4）。
type SupplierRepoConfig struct {
	// GranularitySeconds 解冻粒度 g（整秒；桶时刻 ceil 网格）。
	GranularitySeconds int64
	// FreezeEnabled 全局冻结开关（false ⇒ 生效冻结小时恒 0，全部直接入 available）。
	FreezeEnabled bool
	// FreezeHoursDefault 全局默认冻结小时（row.freeze_hours IS NULL 时继承）。
	FreezeHoursDefault int
	// ShareBpDefault 全局默认分成率（supplier_balances 无行 / share_bp IS NULL 时继承）。
	ShareBpDefault int
	// 付款风控门（§6.5）：claim 事务内判定，缺一项即失败闭合。
	BillingEnabled       bool
	PayoutMaxBacklogRows int
	PayoutMaxBacklogAge  time.Duration
	PayoutMaxObserveAge  time.Duration
	RiskReviewMaxAge     time.Duration
}

// SupplierRepo 供应商记账链 / 解冻链持久面（实现 supplier.CreditStore；另暴露
// 视图装载 / lag / 启动校验 / 会话锁）。
type SupplierRepo struct {
	client *ent.Client
	pool   *pgxpool.Pool
	cfg    SupplierRepoConfig
}

var errSupplierNoPool = errors.New("supplier repo: pgx pool not configured (repository.NewWithPG); cannot run supplier credit/thaw transaction")

// 编译期断言：*SupplierRepo 实现供应商消费面接口。
var (
	_ supplier.CreditStore = (*SupplierRepo)(nil)
	_ supplier.ThawStore   = (*SupplierRepo)(nil)
	_ supplier.ViewStore   = (*SupplierRepo)(nil)
)

// ---------------------------------------------------------------------------
// 视图装载（§4.3：accounts 归属 + supplier_balances 分成率，同一一致性视图）
// ---------------------------------------------------------------------------

// LoadSupplierView 装载供应商视图（单条 UNION ALL 语句 = 一致性快照）：
//   - owner：account_id → supplier_user_id（仅 supplier_user_id IS NOT NULL
//     且未软删的账号）；
//   - share：supplier_user_id → 生效 share_bp（**已注入默认**：无行 / 显式 NULL
//     ⇒ config.share_bp_default；显式 0 保 0）。
//
// 全部成功才返回；失败由调用方保留旧视图（fail-safe，§4.3）。
func (r *SupplierRepo) LoadSupplierView(ctx context.Context) (map[int64]int64, map[int64]int, error) {
	if r.pool == nil {
		return nil, nil, errSupplierNoPool
	}
	const q = `SELECT kind, a, b FROM (
    SELECT 0::int AS kind, id AS a, supplier_user_id AS b
      FROM accounts WHERE supplier_user_id IS NOT NULL AND deleted_at IS NULL
    UNION ALL
    SELECT 1::int AS kind, supplier_user_id AS a, COALESCE(share_bp, -1)::bigint AS b
      FROM supplier_balances
) t`
	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	owner := map[int64]int64{}
	rawShare := map[int64]*int{}
	for rows.Next() {
		var kind int
		var a, b int64
		if err := rows.Scan(&kind, &a, &b); err != nil {
			return nil, nil, err
		}
		switch kind {
		case 0:
			owner[a] = b
		case 1:
			if b < 0 {
				rawShare[a] = nil // 显式 NULL ⇒ 继承
			} else {
				bp := int(b)
				rawShare[a] = &bp
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	share := make(map[int64]int, len(rawShare)+len(owner))
	for uid, bp := range rawShare {
		if bp == nil {
			share[uid] = r.cfg.ShareBpDefault
		} else {
			share[uid] = *bp
		}
	}
	// 归属存在但无 balances 行 ⇒ 默认（§2.3「无行 ⇒ 默认」；不得依赖 map 零值）。
	for _, uid := range owner {
		if _, ok := share[uid]; !ok {
			share[uid] = r.cfg.ShareBpDefault
		}
	}
	return owner, share, nil
}

// ListSupplierOverrides 遍历 supplier_balances 的逐供应商 override（启动校验 §5.5/
// A16⑦：按全局 g + 其 override 重算上界，越界即拒绝启动并列出违规 uid）。
func (r *SupplierRepo) ListSupplierOverrides(ctx context.Context) ([]SupplierOverride, error) {
	if r.pool == nil {
		return nil, errSupplierNoPool
	}
	rows, err := r.pool.Query(ctx, `SELECT supplier_user_id, freeze_hours, share_bp FROM supplier_balances`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SupplierOverride
	for rows.Next() {
		var o SupplierOverride
		var fh, sb *int32
		if err := rows.Scan(&o.UID, &fh, &sb); err != nil {
			return nil, err
		}
		if fh != nil {
			v := int(*fh)
			o.FreezeHours = &v
		}
		if sb != nil {
			v := int(*sb)
			o.ShareBp = &v
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// SupplierOverride 单供应商 override 投影（启动校验用）。
type SupplierOverride struct {
	UID         int64
	FreezeHours *int
	ShareBp     *int
}

// ---------------------------------------------------------------------------
// 记账链（supplier.CreditStore）
// ---------------------------------------------------------------------------

// FetchCreditBatch 取批（消费索引①；`ORDER BY id LIMIT`，与索引①同谓词）。
func (r *SupplierRepo) FetchCreditBatch(ctx context.Context, limit int) ([]supplier.BatchRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	if r.pool == nil {
		return nil, errSupplierNoPool
	}
	const q = `SELECT id, supplier_user_id, cost, supplier_earn_millis, created_at
FROM usage_logs
WHERE NOT supplier_credited AND supplier_earn_millis > 0
ORDER BY id LIMIT $1`
	rows, err := r.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]supplier.BatchRow, 0, limit)
	for rows.Next() {
		var b supplier.BatchRow
		if err := rows.Scan(&b.ID, &b.UID, &b.Cost, &b.Earn, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// FreezeHoursByUID 各 uid 生效冻结小时（全局开关优先 + NULL 注入默认，§2.4）。
// 无行 / NULL ⇒ 全局默认（开关关则 0）；显式 0 保 0。
func (r *SupplierRepo) FreezeHoursByUID(ctx context.Context, uids []int64) (map[int64]int, error) {
	out := make(map[int64]int, len(uids))
	if len(uids) == 0 {
		return out, nil
	}
	if r.pool == nil {
		return nil, errSupplierNoPool
	}
	rows, err := r.pool.Query(ctx, `SELECT supplier_user_id, freeze_hours FROM supplier_balances WHERE supplier_user_id = ANY($1)`, uids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	override := map[int64]*int{}
	for rows.Next() {
		var uid int64
		var fh *int32
		if err := rows.Scan(&uid, &fh); err != nil {
			return nil, err
		}
		if fh != nil {
			v := int(*fh)
			override[uid] = &v
		} else {
			override[uid] = nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, uid := range uids {
		ov, ok := override[uid]
		if !ok {
			ov = nil
		}
		out[uid] = supplier.EffectiveFreezeHours(r.cfg.FreezeEnabled, ov, r.cfg.FreezeHoursDefault)
	}
	return out, nil
}

// ApplyCreditTx 记账单事务（§5.2，阶段 A→D + 两条守卫 + 金额守恒 + 封账累计）：
//
//	A chunks upsert（仅生效冻结小时 > 0 的 uid；available_at 由 DB clock_timestamp
//	  算定，M1）；
//	B 按 uid 升序预锁已存在的 balances 行（显式锁序）；
//	C 建行+累加合一（uid 排序 upsert）RETURNING uid（余额覆盖守卫）；
//	封账按源日 upsert 累加 gross_cost/earned/row_count（仅 state='open'；§3.10）；
//	D UPDATE usage_logs SET supplier_credited=TRUE（标记计数守卫 == 批大小）。
//
// 任一步失败整事务回滚（含守卫不齐）；错误原样上抛（40P01/55P03 由 worker 分类重试）。
func (r *SupplierRepo) ApplyCreditTx(ctx context.Context, batch []supplier.BatchRow, freezeByUID map[int64]int) error {
	if len(batch) == 0 {
		return nil
	}
	if r.pool == nil {
		return errSupplierNoPool
	}
	totals, recon, err := supplier.AggregateBatch(batch, freezeByUID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, supplierTxTimeout)
	defer cancel()
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // nolint:errcheck // Commit 成功后返回 ErrTxClosed，忽略
	if err := r.applyCredit(ctx, tx, batch, totals, recon, freezeByUID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *SupplierRepo) applyCredit(ctx context.Context, tx pgx.Tx, batch []supplier.BatchRow, totals []supplier.UIDCredit, recon []supplier.DayRecon, freezeByUID map[int64]int) error {
	g := r.cfg.GranularitySeconds
	if g <= 0 {
		g = 3600
	}
	// --- 阶段 A：chunks（仅 to_frozen > 0；hours = 该 uid 生效冻结小时） ---
	var cUIDs, cAmts []int64
	var cHours []int32
	for _, t := range totals {
		if t.ToFrozen <= 0 {
			continue
		}
		h := freezeByUID[t.UID]
		if h <= 0 {
			// AggregateBatch 仅在 freezeByUID>0 时把 earn 分流进 ToFrozen；此处
			// 若不一致（调用方传错）⇒ 失败闭合，绝不静默丢桶。
			return fmt.Errorf("supplier credit: uid %d has frozen amount but non-positive freeze hours", t.UID)
		}
		cUIDs = append(cUIDs, t.UID)
		cAmts = append(cAmts, t.ToFrozen)
		cHours = append(cHours, int32(h))
	}
	if len(cUIDs) > 0 {
		const chunkSQL = `INSERT INTO supplier_frozen_chunks (supplier_user_id, available_at, amount)
SELECT u.uid,
       to_timestamp((((floor(extract(epoch from clock_timestamp()))::bigint + $4 - 1) / $4) * $4) + u.hours::bigint * 3600),
       u.amount
FROM unnest($1::bigint[], $2::bigint[], $3::int[]) AS u(uid, amount, hours)
ON CONFLICT (supplier_user_id, available_at)
DO UPDATE SET amount = supplier_frozen_chunks.amount + EXCLUDED.amount`
		if _, err := tx.Exec(ctx, chunkSQL, cUIDs, cAmts, cHours, g); err != nil {
			return fmt.Errorf("supplier credit stage A (chunks): %w", err)
		}
	}

	// --- 阶段 B：按 uid 升序预锁 balances（显式锁序） ---
	bUIDs := make([]int64, 0, len(totals))
	for _, t := range totals {
		bUIDs = append(bUIDs, t.UID)
	}
	if _, err := tx.Exec(ctx,
		`SELECT supplier_user_id FROM supplier_balances WHERE supplier_user_id = ANY($1) ORDER BY supplier_user_id FOR UPDATE`,
		bUIDs); err != nil {
		return fmt.Errorf("supplier credit stage B (prelock): %w", err)
	}

	// --- 阶段 C：建行+累加（uid 排序 upsert） ---
	cAvail := make([]int64, 0, len(totals))
	cCredited := make([]int64, 0, len(totals))
	for _, t := range totals {
		cAvail = append(cAvail, t.ToAvailable)
		cCredited = append(cCredited, t.Earn)
	}
	const balanceSQL = `INSERT INTO supplier_balances (supplier_user_id, available, lifetime_credited, created_at, updated_at)
SELECT u.uid, u.avail, u.credited, now(), now()
FROM unnest($1::bigint[], $2::bigint[], $3::bigint[]) AS u(uid, avail, credited)
ON CONFLICT (supplier_user_id) DO UPDATE
SET available = supplier_balances.available + EXCLUDED.available,
    lifetime_credited = supplier_balances.lifetime_credited + EXCLUDED.lifetime_credited,
    updated_at = now()
RETURNING supplier_user_id`
	returned, err := scanUIDs(ctx, tx, balanceSQL, bUIDs, cAvail, cCredited)
	if err != nil {
		return fmt.Errorf("supplier credit stage C (balances): %w", err)
	}
	// 余额覆盖守卫：RETURNING uid 集 ⊇ 待记账 uid 集，缺任一 ⇒ 回滚
	// （marked==batch 只验标记数，不足以证明余额真的增加，§5.2）。
	got := make(map[int64]struct{}, len(returned))
	for _, uid := range returned {
		got[uid] = struct{}{}
	}
	for _, uid := range bUIDs {
		if _, ok := got[uid]; !ok {
			return fmt.Errorf("supplier credit: balance coverage guard failed: uid %d missing from RETURNING", uid)
		}
	}

	// --- 封账累计（§3.10；仅 state='open' 可累加） ---
	if len(recon) > 0 {
		rUIDs := make([]int64, 0, len(recon))
		rDays := make([]string, 0, len(recon))
		rGross := make([]int64, 0, len(recon))
		rEarn := make([]int64, 0, len(recon))
		rRows := make([]int64, 0, len(recon))
		for _, d := range recon {
			rUIDs = append(rUIDs, d.UID)
			rDays = append(rDays, d.SourceDay.UTC().Format("2006-01-02"))
			rGross = append(rGross, d.GrossCost)
			rEarn = append(rEarn, d.Earn)
			rRows = append(rRows, d.Rows)
		}
		const reconSQL = `INSERT INTO supplier_reconciliation (supplier_user_id, source_day, gross_cost, earned, row_count, state)
SELECT u.uid, u.day::date, u.gross, u.earned, u.rows, 'open'
FROM unnest($1::bigint[], $2::text[], $3::bigint[], $4::bigint[], $5::bigint[]) AS u(uid, day, gross, earned, rows)
ON CONFLICT (supplier_user_id, source_day) DO UPDATE
SET gross_cost = supplier_reconciliation.gross_cost + EXCLUDED.gross_cost,
    earned = supplier_reconciliation.earned + EXCLUDED.earned,
    row_count = supplier_reconciliation.row_count + EXCLUDED.row_count
WHERE supplier_reconciliation.state = 'open'
RETURNING supplier_user_id`
		affected, err := scanUIDs(ctx, tx, reconSQL, rUIDs, rDays, rGross, rEarn, rRows)
		if err != nil {
			return fmt.Errorf("supplier credit reconciliation upsert: %w", err)
		}
		// 已封账源日不得再累加（§A20⑧）：conflict 命中 closed 行时 WHERE 失败 ⇒
		// 无 RETURNING 行 ⇒ 计数不齐 ⇒ 整事务回滚（失败闭合）。
		if len(affected) != len(recon) {
			return fmt.Errorf("supplier credit: reconciliation guard failed: affected %d/%d (closed or missing source day)", len(affected), len(recon))
		}
	}

	// --- 阶段 D：标记 usage_logs（标记计数守卫 == 批大小） ---
	ids := make([]int64, 0, len(batch))
	for _, b := range batch {
		ids = append(ids, b.ID)
	}
	tag, err := tx.Exec(ctx,
		`UPDATE usage_logs SET supplier_credited = TRUE WHERE id = ANY($1) AND NOT supplier_credited`, ids)
	if err != nil {
		return fmt.Errorf("supplier credit stage D (mark): %w", err)
	}
	if int(tag.RowsAffected()) != len(batch) {
		return fmt.Errorf("supplier credit: mark guard failed: marked %d/%d", tag.RowsAffected(), len(batch))
	}
	return nil
}

// scanUIDs 执行带 RETURNING supplier_user_id 的语句并收集 uid。
func scanUIDs(ctx context.Context, tx pgx.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var uid int64
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		out = append(out, uid)
	}
	return out, rows.Err()
}

// ReleaseFrozenNoWait freeze_enabled=false 的存量释放（§5.4）：忽略 available_at，
// 保留 LIMIT（每周期一批）——最终收敛语义；纳入 credit.Close（不得因 backlog==0
// 提前退出）。返回本批释放的桶行数。
func (r *SupplierRepo) ReleaseFrozenNoWait(ctx context.Context, limit int) (int, error) {
	return r.thawChunks(ctx, limit, false)
}

// ThawDueChunks 解冻到期桶（§5.3）：available_at <= now() 的桶整体到期、整体解冻。
func (r *SupplierRepo) ThawDueChunks(ctx context.Context, limit int) (int, error) {
	return r.thawChunks(ctx, limit, true)
}

// ThawBucketStats 冻结桶物理观测（spec §7.2/G7）：单条聚合查询，零缓存。bucket_lag
// = 最老未解冻桶 available_at 距 DB now 的秒数（全部未来 / 空 = 0，GREATEST 夹负）；
// overdue = available_at <= now() 的桶。DAL 直读，供 thaw worker 每轮刷新 ops 面。
func (r *SupplierRepo) ThawBucketStats(ctx context.Context) (supplier.ThawBucketSnapshot, error) {
	if r.pool == nil {
		return supplier.ThawBucketSnapshot{}, errSupplierNoPool
	}
	const q = `SELECT
    COUNT(*)::bigint,
    GREATEST(0, floor(extract(epoch from clock_timestamp() - MIN(available_at))))::bigint,
    COUNT(*) FILTER (WHERE available_at <= now())::bigint,
    COALESCE(SUM(amount) FILTER (WHERE available_at <= now()), 0)::bigint
FROM supplier_frozen_chunks`
	var st supplier.ThawBucketSnapshot
	if err := r.pool.QueryRow(ctx, q).Scan(
		&st.BucketRows, &st.BucketLagSeconds, &st.OverdueRows, &st.OverdueAmount,
	); err != nil {
		return supplier.ThawBucketSnapshot{}, err
	}
	return st, nil
}

// thawChunks 解冻链四步（单连接契约；§5.3）：① DELETE RETURNING → ② 按 uid 升序
// 预锁 balances → ③ balances 变更 → ④ 覆盖守卫（deleted_uids ⊄ updated_uids ⇒
// ROLLBACK）。dueOnly=true 仅取到期桶，否则忽略时间谓词（freeze_enabled=false
// 存量释放）。返回删除的桶行数。
func (r *SupplierRepo) thawChunks(ctx context.Context, limit int, dueOnly bool) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	if r.pool == nil {
		return 0, errSupplierNoPool
	}
	ctx, cancel := context.WithTimeout(ctx, supplierTxTimeout)
	defer cancel()
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) // nolint:errcheck // Commit 后 ErrTxClosed，忽略

	// ① 取批 + 删除 + 返回（一条语句，原子；消灭读-写间隙）。
	delSQL := `DELETE FROM supplier_frozen_chunks
WHERE (supplier_user_id, available_at) IN (
      SELECT supplier_user_id, available_at FROM supplier_frozen_chunks
      WHERE %s
      ORDER BY available_at LIMIT $1
      FOR UPDATE SKIP LOCKED)
RETURNING supplier_user_id, amount`
	pred := "TRUE"
	if dueOnly {
		pred = "available_at <= now()"
	}
	delSQL = fmt.Sprintf(delSQL, pred)
	rows, err := tx.Query(ctx, delSQL, limit)
	if err != nil {
		return 0, err
	}
	deltaByUID := map[int64]int64{}
	var order []int64
	deleted := 0
	for rows.Next() {
		var uid, amt int64
		if err := rows.Scan(&uid, &amt); err != nil {
			rows.Close()
			return 0, err
		}
		// 前置断言（§5.3 M1）：uid 正向、`amount > 0`（DB CHECK 之外的防御），
		// 聚合 delta 无溢出；任一不满足 ⇒ 失败闭合（UPDATE 前，chunks 未提交删除）。
		if uid <= 0 || amt < 0 {
			rows.Close()
			return 0, fmt.Errorf("supplier thaw: invalid chunk uid=%d amount=%d", uid, amt)
		}
		if _, ok := deltaByUID[uid]; !ok {
			order = append(order, uid)
		}
		sum, err := supplier.AddChecked(deltaByUID[uid], amt)
		if err != nil {
			rows.Close()
			return 0, fmt.Errorf("supplier thaw: %w", err)
		}
		deltaByUID[uid] = sum
		deleted++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	if deleted == 0 {
		return 0, tx.Commit(ctx)
	}

	// ② 按 uid 升序预锁 balances（★ 独立语句，锁顺序真实发生；§5.3/§5.7）。
	slices.Sort(order)
	lockRows, err := tx.Query(ctx,
		`SELECT supplier_user_id FROM supplier_balances WHERE supplier_user_id = ANY($1) ORDER BY supplier_user_id FOR UPDATE`,
		order)
	if err != nil {
		return 0, err
	}
	lockRows.Close()
	if err := lockRows.Err(); err != nil {
		return 0, err
	}

	// ③ balances 变更（两参数等长 unnest，避免双 SRF 位置配对陷阱）。
	// 数组边界前置断言（A8④ M1）：uids/deltas 等长且与 order 一致；delta 非负。
	uids := make([]int64, 0, len(order))
	deltas := make([]int64, 0, len(order))
	for _, uid := range order {
		d := deltaByUID[uid]
		if d < 0 {
			return 0, fmt.Errorf("supplier thaw: negative delta %d for uid %d", d, uid)
		}
		uids = append(uids, uid)
		deltas = append(deltas, d)
	}
	if len(uids) != len(deltas) || len(uids) != len(order) {
		return 0, fmt.Errorf("supplier thaw: array length mismatch uids=%d deltas=%d order=%d", len(uids), len(deltas), len(order))
	}
	updated, err := scanUIDs(ctx, tx,
		`UPDATE supplier_balances b
SET available = b.available + v.delta, updated_at = now()
FROM (SELECT * FROM unnest($1::bigint[], $2::bigint[]) AS t(uid, delta)) v
WHERE b.supplier_user_id = v.uid
RETURNING b.supplier_user_id`,
		uids, deltas)
	if err != nil {
		return 0, err
	}
	// ④ 覆盖守卫：deleted_uids ⊄ updated_uids ⇒ ROLLBACK（chunks 未丢）。
	got := make(map[int64]struct{}, len(updated))
	for _, uid := range updated {
		got[uid] = struct{}{}
	}
	for _, uid := range order {
		if _, ok := got[uid]; !ok {
			return 0, fmt.Errorf("supplier thaw: balance coverage guard failed: uid %d missing", uid)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return deleted, nil
}

// CreditLagHead 头行快速 lag（走索引①取最小 id 行的 created_at，低成本；高频）。
// 返回头行 created_at 距 DB now 的秒数（无待记账行 ⇒ 0）。
func (r *SupplierRepo) CreditLagHead(ctx context.Context) (int64, error) {
	if r.pool == nil {
		return 0, errSupplierNoPool
	}
	const q = `SELECT COALESCE((
    SELECT floor(extract(epoch from clock_timestamp() - u.created_at))::bigint
    FROM usage_logs u
    WHERE NOT u.supplier_credited AND u.supplier_earn_millis > 0
    ORDER BY u.id LIMIT 1), 0)`
	var lag int64
	if err := r.pool.QueryRow(ctx, q).Scan(&lag); err != nil {
		return 0, err
	}
	return lag, nil
}

// CreditLagFull 完整 backlog COUNT + SUM（O(backlog) 成本；降频，§3.9 I3）。
func (r *SupplierRepo) CreditLagFull(ctx context.Context) (int64, int64, error) {
	if r.pool == nil {
		return 0, 0, errSupplierNoPool
	}
	const q = `SELECT COUNT(*)::bigint, COALESCE(SUM(supplier_earn_millis), 0)::bigint
FROM usage_logs WHERE NOT supplier_credited AND supplier_earn_millis > 0`
	var rows, sum int64
	if err := r.pool.QueryRow(ctx, q).Scan(&rows, &sum); err != nil {
		return 0, 0, err
	}
	return rows, sum, nil
}

// AcquireSupplierLock 抢占记账链会话级 advisory lock（I4：单 leader 取批，减少
// 多实例撞同批；形态对齐 AcquireBillingLock——专用连接持有到 release）。抢锁失败
// ⇒ ok=false（本周期跳过，其他实例在消费）。
func (r *SupplierRepo) AcquireSupplierLock(ctx context.Context) (release func(), ok bool, err error) {
	if r.pool == nil {
		return nil, false, errSupplierNoPool
	}
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, supplierCreditLockKey).Scan(&acquired); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}
	return func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, supplierCreditLockKey)
		conn.Release()
	}, true, nil
}

// supplierCreditLockKey 记账链会话锁键（与 billing/stats 不同命名空间）。
const supplierCreditLockKey int64 = 0x7375707063726564 // "suppcred" ASCII 前缀（避免与既有键碰撞的常数）
