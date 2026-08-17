// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/tempbalance"
	"github.com/is7qin/c3api/internal/ent/usagelog"
	"github.com/is7qin/c3api/internal/ent/user"
)

// deductTimeout 计费扣费事务 per-query 超时（F-P2-4 降级形态）：会话级
// statement_timeout=10s 与 admin 面 ScanStats 大窗口聚合实测冲突（720 万行/30
// 天 → 57014，见 f1-impl-report.md 副作用核实）→ 按 spec 授权降级为计费路径
// per-query 超时：扣费事务整体 10s 上限（执行时长 + 锁等待双有界；锁等待另有
// 池级 lock_timeout=5s 会话 GUC 先行兜底——SELECT 不取行锁不受其影响）。
// 超时 → 事务取消回滚 → 调用方（billing.Flusher）失败回灌不丢不重。
const deductTimeout = 10 * time.Second

// usageLogBatchSize 计费日志单批插入行数上限（#37 P2）：ent CreateBulk 参数 =
// 列数 × 行数，超 PG 65535 参数上限即失败（"extended protocol limited to
// 65535 parameters"，19 列 × ~3448 行——压测实证扣费停滞根因）。
// 热点修复 A（2026-08-11，测量数据见 pg_deduct_bench_test.go）：500 → 2000
// 行/批（26 列最坏界 × 2000 = 52,000 参数 < 65,535；生产 19 列 38,000）——
// 单事务 10k 行往返 20 → 5 次（往返实测 0.3ms/次，本地净省 ~4.5ms/事务 ~2%，
// 负载/远端 DB 上按延迟放大）；服务器侧逐行插入耗时持平（~6.6µs/行）。
// 统一计费模型重构（spec 2026-08-13）删 6 加 2 后最坏界 28 列 × 2000 =
// 56,000 参数 < 65,535（仍安全，常量不动；生产 28 列 56,000）。S-E
// （2026-08-17）加 client_ip 后最坏界 29 列 × 2000 = 58,000 参数 < 65,535
// （仍安全，常量不动；生产 29 列 58,000）。
// 仅 ent CreateBulk 回落路径（pool == nil）使用；pgx COPY 路径无参数上限，
// 整事务一次 COPY 不涉及本常量。
const usageLogBatchSize = 2000

// BillingRepo 扣费落库：FEFO 临时额度优先 + 条件扣费（允许透支）+ 同事务批量
// 计费日志。全毫分直接扣减（1 USD = 100,000 毫分，零换算零取整误差）。
//
// 双路径（热点修复 A 扩，2026-08-11 用户裁决）：
//   - pool != nil（生产 NewWithPG 装配）→ deductAndLogCopy：pgx 直连事务 +
//     COPY 落明细（替代 ent CreateBulk 的客户端逐行编码/服务器 RETURNING
//     物化主成分——单事务 10k 行实测 230-254ms → 见 pg_deduct_bench_test.go）
//   - pool == nil（mock/无池装配、WithTx 事务内）→ deductAndLogEnt：既有
//     ent 事务路径回落
//
// 两路径共用 deductCore（FEFO/余额逻辑单一实现，防漂移——等价性测试兜底），
// 仅事务载体与明细落库面不同（deductTx 双适配）。
type BillingRepo struct {
	client *ent.Client
	// driver 为 raw SQL（FEFO/余额条件更新）用：与 txDriver 组合保证 raw SQL
	// 与 ent 构建器同事务连接（WithTx 同构，评审 I-1）。
	driver dialect.Driver
	// pool 为 pgx 连接池（NewWithPG 注入；New 构造的仓库为 nil）：非 nil →
	// DeductAndLog 走 pgx 直连 + COPY 路径；nil → ent 事务路径回落。WithTx
	// 的 tx 版仓库恒传 nil（事务内不挂池，见 repository.go WithTx）。
	pool *pgxpool.Pool
}

// DeductAndLog 批量扣费 + 计费日志落库（单事务，全成或全败）：
//
// ① FEFO 临时额度（评审 I-4 用户裁决）：未过期 temp_balances 按 expires_at
//
//	升序逐行扣（最早到期先扣、永久 NULL 最后——PG ASC 默认 NULLS LAST），
//	行级条件更新 amount >= take 防并发透支（多实例并发扣同一用户时 SELECT
//	读到的存量可能已被他事务扣减：条件不满足则跳过该行，剩余转扣余额，
//	temp 恒不为负）
//
// ② 剩余扣 users.balance：条件更新 balance >= remain（防并发负余额）；0 行 →
//
//	无条件扣（允许透支）；再 0 行 = user 不存在 → 跳过扣减仍插日志
//	（usagelog 无 FK）
//
// ③ 事务内 SELECT balance 回读（行锁串行无竞态）返回 balanceAfter
// ④ logs 逐个 Overdraft=overdrafted 同事务落明细（pgx COPY / ent CreateBulk）
//
// 任一步失败整体回滚。cost == 0 → 只插日志（不扣款；balanceAfter = 当前余额
// 原值，overdrafted=false）。
//
// F-P2-4：整个扣费事务受 deductTimeout per-query 超时约束（ctx 截止——pgx
// deadline watcher 取消在途语句 → 事务回滚；覆盖执行时长与锁等待双面，见
// deductTimeout 注释）。
func (r *BillingRepo) DeductAndLog(ctx context.Context, userID, cost int64, logs []*domain.UsageLog) (overdrafted bool, balanceAfter int64, err error) {
	ctx, cancel := context.WithTimeout(ctx, deductTimeout)
	defer cancel()
	if r.pool != nil {
		return r.deductAndLogCopy(ctx, userID, cost, logs)
	}
	return r.deductAndLogEnt(ctx, userID, cost, logs)
}

// deductAndLogEnt 既有 ent 事务路径（pool == nil 回落；与 COPY 路径共用
// deductCore）。WithTx 事务内经 txDriver 的嵌套 Tx（返回自身）语义不变。
func (r *BillingRepo) deductAndLogEnt(ctx context.Context, userID, cost int64, logs []*domain.UsageLog) (bool, int64, error) {
	tx, err := r.driver.Tx(ctx)
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback() // nolint:errcheck // Commit 成功后 Rollback 返回 ErrTxDone，忽略
	drv := &txDriver{tx: tx, drv: r.driver}
	exe := &entDeductTx{drv: drv, client: ent.NewClient(ent.Driver(drv))}
	od, bal, err := r.deductCore(ctx, exe, userID, cost, logs)
	if err != nil {
		return false, 0, err
	}
	if err := tx.Commit(); err != nil {
		return false, 0, err
	}
	return od, bal, nil
}

// deductAndLogCopy pgx 直连路径（热点修复 A 扩）：单连接 BEGIN → deductCore
// （pgx 执行面）→ COPY 明细 → COMMIT。同事务原子；任一失败整体回滚——错误
// 语义与 ent 路径一致（flusher 失败回灌/重试链不变）。
func (r *BillingRepo) deductAndLogCopy(ctx context.Context, userID, cost int64, logs []*domain.UsageLog) (bool, int64, error) {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return false, 0, err
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback(ctx) // nolint:errcheck // Commit 成功后返回 ErrTxClosed，忽略
	od, bal, err := r.deductCore(ctx, &pgxDeductTx{tx: tx}, userID, cost, logs)
	if err != nil {
		return false, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, 0, err
	}
	return od, bal, nil
}

// —— deductCore：扣费事务核心（两路径单一实现，防漂移） ——

// deductTx 扣费事务执行面（ent txDriver / pgx.Tx 双适配）：FEFO/余额逻辑在
// deductCore 单一实现，事务载体差异收敛到本接口。
type deductTx interface {
	// ExecAffected 执行一条 SQL 返回受影响行数（条件更新判定）。
	ExecAffected(ctx context.Context, query string, args []any) (int64, error)
	// QueryRows 执行查询返回行扫描器（FEFO SELECT）。
	QueryRows(ctx context.Context, query string, args []any) (rowScanner, error)
	// QueryRowScan 单行查询扫描（余额回读）；0 行 → 错误（与 ent Only 语义
	// 一致——cost==0 且用户缺失同样报错回滚）。
	QueryRowScan(ctx context.Context, query string, args []any, dest ...any) error
	// InsertLogs 同事务落计费明细（ent CreateBulk / pgx COPY）。
	InsertLogs(ctx context.Context, logs []*domain.UsageLog, overdrafted bool) error
}

// rowScanner 行扫描面（entsql.Rows 与 pgx.Rows 的公共子集；两者 Close 签名
// 不同——entsql.Close() error vs pgx.Close()，故不含 Close）。deductCore 读取
// 至 EOF（rows.Err() 确认）即释放连接：FEFO 行集在事务连接上，tx 生命周期内
// 无池归还压力，EOF 后连接立即可复用（database/sql 与 pgx 同语义）。
type rowScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

// fefoSelectSQL FEFO 临时额度查询（与 ent 生成的语义等价：未过期 + 正余额，
// expires_at 升序——PG ASC 默认 NULLS LAST，永久额度最后）。两路径共用同一
// SQL 事实源。
const fefoSelectSQL = `SELECT id, amount FROM temp_balances
WHERE user_id = $1 AND amount > 0 AND (expires_at IS NULL OR expires_at > $2)
ORDER BY expires_at`

// balanceSelectSQL 余额回读（事务内行锁串行无竞态）。
const balanceSelectSQL = `SELECT balance FROM users WHERE id = $1`

// deductCore 事务内扣费核心：FEFO 逐行条件扣 → 余额条件扣（允许透支/用户缺失
// 兜底）→ 余额回读 → 明细落库。任一步失败整体回滚（由调用方事务语义保证）。
func (r *BillingRepo) deductCore(ctx context.Context, exe deductTx, userID, cost int64, logs []*domain.UsageLog) (bool, int64, error) {
	remain := cost
	if cost > 0 {
		rows, err := exe.QueryRows(ctx, fefoSelectSQL, []any{userID, time.Now()})
		if err != nil {
			return false, 0, err
		}
		type tempRow struct{ id, amount int64 }
		var tempRows []tempRow
		for rows.Next() {
			var t tempRow
			if err := rows.Scan(&t.id, &t.amount); err != nil {
				return false, 0, err
			}
			tempRows = append(tempRows, t)
		}
		if err := rows.Err(); err != nil {
			return false, 0, err
		}
		for _, tb := range tempRows {
			if remain <= 0 {
				break
			}
			take := tb.amount
			if take > remain {
				take = remain
			}
			q, args := tempBalanceDeductQuery(tb.id, take)
			n, err := exe.ExecAffected(ctx, q, args)
			if err != nil {
				return false, 0, err
			}
			if n > 0 {
				remain -= take
			}
		}
	}
	overdrafted := false
	userExists := true
	if remain > 0 {
		q, args := balanceDeductQuery(userID, remain)
		n, err := exe.ExecAffected(ctx, q, args)
		if err != nil {
			return false, 0, err
		}
		if n == 0 {
			// 余额不足 → 无条件扣（允许透支）；再 0 行 = 用户不存在
			q, args = balanceDeductForceQuery(userID, remain)
			n, err = exe.ExecAffected(ctx, q, args)
			if err != nil {
				return false, 0, err
			}
			if n == 0 {
				userExists = false
			} else {
				overdrafted = true
			}
		}
	}
	balanceAfter := int64(0)
	if userExists {
		if err := exe.QueryRowScan(ctx, balanceSelectSQL, []any{userID}, &balanceAfter); err != nil {
			return false, 0, err
		}
	}
	if len(logs) > 0 {
		if err := exe.InsertLogs(ctx, logs, overdrafted); err != nil {
			return false, 0, err
		}
	}
	return overdrafted, balanceAfter, nil
}

// tempBalanceDeductQuery 临时额度行级条件扣（amount >= take 防并发透支）。
func tempBalanceDeductQuery(id, take int64) (string, []any) {
	u := entsql.Update(tempbalance.Table).
		Set(tempbalance.FieldAmount, entsql.ExprFunc(func(b *entsql.Builder) {
			b.Ident(tempbalance.FieldAmount).WriteString(" - ").Arg(take)
		})).
		Where(entsql.And(
			entsql.EQ(tempbalance.FieldID, id),
			entsql.GTE(tempbalance.FieldAmount, take),
		))
	u.SetDialect(dialect.Postgres)
	return u.Query()
}

// balanceDeductQuery 余额条件扣（balance >= remain 防并发负余额）。
func balanceDeductQuery(userID, remain int64) (string, []any) {
	u := entsql.Update(user.Table).
		Set(user.FieldBalance, entsql.ExprFunc(func(b *entsql.Builder) {
			b.Ident(user.FieldBalance).WriteString(" - ").Arg(remain)
		})).
		Where(entsql.And(
			entsql.EQ(user.FieldID, userID),
			entsql.GTE(user.FieldBalance, remain),
		))
	u.SetDialect(dialect.Postgres)
	return u.Query()
}

// balanceDeductForceQuery 无条件扣（允许透支；用户缺失时 0 行）。
func balanceDeductForceQuery(userID, remain int64) (string, []any) {
	u := entsql.Update(user.Table).
		Set(user.FieldBalance, entsql.ExprFunc(func(b *entsql.Builder) {
			b.Ident(user.FieldBalance).WriteString(" - ").Arg(remain)
		})).
		Where(entsql.EQ(user.FieldID, userID))
	u.SetDialect(dialect.Postgres)
	return u.Query()
}

// —— ent txDriver 适配（回落路径） ——

// entDeductTx ent 事务路径执行面（txDriver + tx client）。
type entDeductTx struct {
	drv    dialect.Driver
	client *ent.Client
}

func (d *entDeductTx) ExecAffected(ctx context.Context, query string, args []any) (int64, error) {
	var res sql.Result
	if err := d.drv.Exec(ctx, query, args, &res); err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (d *entDeductTx) QueryRows(ctx context.Context, query string, args []any) (rowScanner, error) {
	rows := &entsql.Rows{}
	if err := d.drv.Query(ctx, query, args, rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func (d *entDeductTx) QueryRowScan(ctx context.Context, query string, args []any, dest ...any) error {
	rows := &entsql.Rows{}
	if err := d.drv.Query(ctx, query, args, rows); err != nil {
		return err
	}
	// 具体类型直闭（接口无 Close——见 rowScanner 注释）；余额回读单行后必须
	// 释放行集，否则事务连接保持 busy。
	defer rows.Close() // nolint:errcheck
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		// 0 行 ≈ ent Only 的 NotFoundError：余额回读目标行不存在 → 整体回滚。
		return errors.New("billing: user not found")
	}
	return rows.Scan(dest...)
}

// InsertLogs 既有 ent CreateBulk 分片插入（#37 P2 参数上限分片；热点修复 A：
// 2000 行/批）。与 COPY 路径语义等价（等价性测试兜底）。
func (d *entDeductTx) InsertLogs(ctx context.Context, logs []*domain.UsageLog, overdrafted bool) error {
	for start := 0; start < len(logs); start += usageLogBatchSize {
		end := min(start+usageLogBatchSize, len(logs))
		builders := make([]*ent.UsageLogCreate, 0, end-start)
		for _, l := range logs[start:end] {
			l.Overdraft = overdrafted
			builders = append(builders, buildUsageLogCreate(d.client, l))
		}
		if _, err := d.client.UsageLog.CreateBulk(builders...).Save(ctx); err != nil {
			return err
		}
	}
	return nil
}

// —— pgx 直连适配（COPY 路径） ——

// pgxDeductTx pgx 事务执行面。
type pgxDeductTx struct {
	tx pgx.Tx
}

func (x *pgxDeductTx) ExecAffected(ctx context.Context, query string, args []any) (int64, error) {
	tag, err := x.tx.Exec(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (x *pgxDeductTx) QueryRows(ctx context.Context, query string, args []any) (rowScanner, error) {
	return x.tx.Query(ctx, query, args...)
}

func (x *pgxDeductTx) QueryRowScan(ctx context.Context, query string, args []any, dest ...any) error {
	return x.tx.QueryRow(ctx, query, args...).Scan(dest...)
}

// usageLogCopyColumns COPY 列清单 = buildUsageLogCreate 设置的列集合（29 列
// 全列显式列出——未设置的可选列传 NULL，与 ent 省略列（→NULL）等价；列序
// 与 usage_logs 分区表列定义一致，5 索引兼容）。COPY 无 65535 参数上限，
// 整事务一次 COPY（无分片）。统一计费模型（spec 2026-08-13）：原图片 6 列
// （image tokens/count + 3 价格快照）已删，加 call_count/price_per_call_millis。
// S-E（2026-08-17）：加 client_ip（紧随 request_id，与分区表列定义一致）。
var usageLogCopyColumns = []string{
	usagelog.FieldRequestID, usagelog.FieldClientIP, usagelog.FieldGroupID,
	usagelog.FieldAccountID, usagelog.FieldTemplateID, usagelog.FieldUserID,
	usagelog.FieldKeyID, usagelog.FieldModel, usagelog.FieldMappedModel,
	usagelog.FieldFormat, usagelog.FieldErrorType, usagelog.FieldLatencyMs,
	usagelog.FieldTtftMs, usagelog.FieldInputTokens, usagelog.FieldPriceInputMillis,
	usagelog.FieldOutputTokens, usagelog.FieldPriceOutputMillis, usagelog.FieldTotalTokens,
	usagelog.FieldCacheReadTokens, usagelog.FieldPriceCacheReadMillis,
	usagelog.FieldCacheCreationTokens, usagelog.FieldPriceCacheCreationMillis,
	usagelog.FieldCallCount, usagelog.FieldPricePerCallMillis, usagelog.FieldCost,
	usagelog.FieldBillingTier, usagelog.FieldAboveHit, usagelog.FieldOverdraft,
	usagelog.FieldCreatedAt,
}

// usageLogRowValues 单行 COPY 值（与 buildUsageLogCreate 的 Set 条件一一对应：
// 可选列 >0/非空/非 nil 才赋值，否则 NULL；call_count 恒落（NOT NULL DEFAULT 0）；
// client_ip 非空才赋值，否则 NULL）。
func usageLogRowValues(l *domain.UsageLog) []any {
	var groupID, accountID, templateID, userID, keyID, mappedModel, billingTier, clientIP any
	var ttft, priceIn, priceOut, priceCR, priceCC, pricePerCall any
	if l.ClientIP != "" {
		clientIP = l.ClientIP
	}
	if l.GroupID > 0 {
		groupID = l.GroupID
	}
	if l.AccountID > 0 {
		accountID = l.AccountID
	}
	if l.TemplateID > 0 {
		templateID = l.TemplateID
	}
	if l.UserID > 0 {
		userID = l.UserID
	}
	if l.KeyID > 0 {
		keyID = l.KeyID
	}
	if l.MappedModel != "" {
		mappedModel = l.MappedModel
	}
	if l.BillingTier != "" {
		billingTier = l.BillingTier
	}
	if l.TTFTMS != nil {
		ttft = *l.TTFTMS
	}
	if l.PriceInputMillis != nil {
		priceIn = *l.PriceInputMillis
	}
	if l.PriceOutputMillis != nil {
		priceOut = *l.PriceOutputMillis
	}
	if l.PriceCacheReadMillis != nil {
		priceCR = *l.PriceCacheReadMillis
	}
	if l.PriceCacheCreationMillis != nil {
		priceCC = *l.PriceCacheCreationMillis
	}
	if l.PricePerCallMillis != nil {
		pricePerCall = *l.PricePerCallMillis
	}
	return []any{
		l.RequestID, clientIP, groupID, accountID, templateID, userID, keyID,
		l.Model, mappedModel, string(l.Format), string(l.ErrorType), l.LatencyMS, ttft,
		l.InputTokens, priceIn, l.OutputTokens, priceOut, l.TotalTokens,
		l.CacheReadTokens, priceCR, l.CacheCreationTokens, priceCC,
		l.CallCount, pricePerCall,
		l.Cost, billingTier, l.AboveHit, l.Overdraft, l.CreatedAt,
	}
}

// InsertLogs COPY 落明细（热点修复 A 扩，实测选优）：同环境 ≥5 轮中位数
// 10k 行/事务：ent CreateBulk 222ms / raw multi-row INSERT（2000 行/批分片）
// 181ms / COPY 75ms——COPY 胜出（服务器批量装载路径 + 无参数上限一次完成 +
// 无 RETURNING 物化）。分区表按 created_at 逐行路由（等价性测试验证）。
// 失败 → 外层事务回滚（COPY 中途失败同样整体回滚——语义与 CreateBulk 分片
// 一致）。格式枚举校验前置：分区表 format 列为 varchar（无 DB 层 enum 约束），
// ent 路径由 CreateBulk 客户端 FormatValidator 拒绝非法值——COPY 路径必须
// 等价复刻（错误文本不同：ent 包装为 ValidationError（含字段前缀），COPY
// 返回裸错误；可观察行为一致——均在任何插入前拒绝、均致事务回滚）。
func (x *pgxDeductTx) InsertLogs(ctx context.Context, logs []*domain.UsageLog, overdrafted bool) error {
	if len(logs) == 0 {
		return nil
	}
	rows := make([][]any, 0, len(logs))
	for _, l := range logs {
		if err := usagelog.FormatValidator(usagelog.Format(l.Format)); err != nil {
			return err
		}
		l.Overdraft = overdrafted
		rows = append(rows, usageLogRowValues(l))
	}
	_, err := x.tx.CopyFrom(ctx, pgx.Identifier{usagelog.Table}, usageLogCopyColumns, pgx.CopyFromRows(rows))
	return err
}
