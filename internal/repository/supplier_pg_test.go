// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// supplier_pg_test.go 用**真实 PostgreSQL** 执行供应商记账链/解冻链/结算链/管理面
// 五态/视图装载/liability/PATCH/分区退休屏障的真实 SQL（spec 2026-10-09 §5/§6）。
// 跑法：TEST_DATABASE_URL=... go test -count=1 -p 1 ./internal/repository/ -run PG -v

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/supplier"
)

// ---- helpers ----

func supplierRepoCfg() repository.SupplierRepoConfig {
	return repository.SupplierRepoConfig{
		GranularitySeconds:   7200,
		FreezeEnabled:        true,
		FreezeHoursDefault:   24,
		ShareBpDefault:       1000,
		BillingEnabled:       true,
		PayoutMaxBacklogRows: 10000,
		PayoutMaxBacklogAge:  10 * time.Minute,
		PayoutMaxObserveAge:  3 * time.Minute,
		RiskReviewMaxAge:     24 * time.Hour,
	}
}

func seedPGUserRole(t *testing.T, repos *repository.Repository, email string, role domain.Role) *domain.User {
	t.Helper()
	u, err := repos.CreateUser(context.Background(), &domain.User{
		Email: email, PasswordHash: "hash-" + email, Role: role, Status: domain.UserStatusActive,
	})
	require.NoError(t, err)
	return u
}

func seedSupplierUser(t *testing.T, repos *repository.Repository, email string) *domain.User {
	return seedPGUserRole(t, repos, email, domain.RoleSupplier)
}

func seedSupplierBalance(t *testing.T, pool *pgxpool.Pool, uid, available int64) {
	t.Helper()
	pgExec(t, pool, `INSERT INTO supplier_balances (supplier_user_id, available, lifetime_credited, lifetime_paid, created_at, updated_at)
		VALUES ($1,$2,$2,0,now(),now())`, uid, available)
}

// testPayeeSnapshot 结构化收款目标快照（§6.5 C4）。
func testPayeeSnapshot(account string) domain.SupplierPayeeSnapshot {
	return domain.SupplierPayeeSnapshot{PayeeName: "Acme Supplier Ltd", Account: account, Unit: "millis"}
}

// testConfirmNotPaid 结构化「确定未支付」核验（具名确认人 + 结论 + 证据 + 旧执行停止）。
func testConfirmNotPaid(reason string) domain.SupplierPayoutFailureConfirmation {
	return domain.SupplierPayoutFailureConfirmation{
		Reason:              reason,
		Evidence:            "bank-query-ref-" + reason,
		ConfirmedNotPaid:    true,
		OldExecutionStopped: true,
	}
}

// seedSupplierUsage 插一条使用行（supplier 三列；billed=true 避免污染 claim 探针）。
func seedSupplierUsage(t *testing.T, pool *pgxpool.Pool, uid, earn, cost int64, credited bool, createdAt time.Time) {
	t.Helper()
	pgExec(t, pool, `INSERT INTO usage_logs
		(request_id, model, format, error_type, cost, raw_cost, created_at, supplier_user_id, supplier_earn_millis, supplier_credited, billed)
		VALUES ($1,'m','openai-chat','none',$2,$2,$3,$4,$5,$6,true)`,
		fmt.Sprintf("sreq-%d-%d", uid, createdAt.UnixNano()), cost, createdAt, uid, earn, credited)
}

func pgInt(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int64 {
	t.Helper()
	var v int64
	require.NoError(t, pool.QueryRow(context.Background(), q, args...).Scan(&v))
	return v
}

// ---- A7/A25/§5.2 记账链 ----

func TestPGSupplierCreditChain(t *testing.T) {
	repos := newPGReposShared(t)
	pool := pgSharedPool(t)
	ctx := context.Background()
	sr := repos.SupplierRepo(supplierRepoCfg())

	s := seedSupplierUser(t, repos, "sup-credit@example.com")
	now := time.Now().UTC()
	seedSupplierUsage(t, pool, s.ID, 700, 1000, false, now.Add(-time.Minute))
	seedSupplierUsage(t, pool, s.ID, 300, 400, false, now.Add(-30*time.Second))

	batch, err := sr.FetchCreditBatch(ctx, 100)
	require.NoError(t, err)
	require.Len(t, batch, 2, "取批走消费索引①（NOT credited AND earn>0）")

	freeze, err := sr.FreezeHoursByUID(ctx, []int64{s.ID})
	require.NoError(t, err)
	require.Equal(t, 24, freeze[s.ID], "无行 ⇒ 继承全局默认冻结小时")

	require.NoError(t, sr.ApplyCreditTx(ctx, batch, freeze))

	// 余额：全部冻结（freeze 24h > 0）⇒ available 0，lifetime_credited = Σearn。
	require.Equal(t, int64(1000), pgInt(t, pool, `SELECT lifetime_credited FROM supplier_balances WHERE supplier_user_id=$1`, s.ID))
	require.Equal(t, int64(0), pgInt(t, pool, `SELECT available FROM supplier_balances WHERE supplier_user_id=$1`, s.ID))
	// 桶：同一 uid 聚合到同一 available_at ⇒ 1 行 amount=1000。
	require.Equal(t, int64(1), pgInt(t, pool, `SELECT COUNT(*) FROM supplier_frozen_chunks WHERE supplier_user_id=$1`, s.ID))
	require.Equal(t, int64(1000), pgInt(t, pool, `SELECT COALESCE(SUM(amount),0) FROM supplier_frozen_chunks WHERE supplier_user_id=$1`, s.ID))
	// 标记：两行 credited=true。
	require.Equal(t, int64(2), pgInt(t, pool, `SELECT COUNT(*) FROM usage_logs WHERE supplier_user_id=$1 AND supplier_credited`, s.ID))
	// 封账：源日 open 行 row_count=2、earned=1000、gross_cost=1400。
	require.Equal(t, int64(2), pgInt(t, pool, `SELECT row_count FROM supplier_reconciliation WHERE supplier_user_id=$1`, s.ID))
	require.Equal(t, int64(1000), pgInt(t, pool, `SELECT earned FROM supplier_reconciliation WHERE supplier_user_id=$1`, s.ID))
	require.Equal(t, int64(1400), pgInt(t, pool, `SELECT gross_cost FROM supplier_reconciliation WHERE supplier_user_id=$1`, s.ID))
	require.Equal(t, "open", pgText(t, pool, `SELECT state FROM supplier_reconciliation WHERE supplier_user_id=$1`, s.ID))

	// I3：backlog 收敛为 0。
	rows, sum, err := sr.CreditLagFull(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), rows)
	require.Equal(t, int64(0), sum)

	// 幂等重放：同批再记账 ⇒ 标记计数守卫失败 ⇒ 回滚 ⇒ 不重复记账。
	err = sr.ApplyCreditTx(ctx, batch, freeze)
	require.Error(t, err, "重放同批必须被标记计数守卫拒绝")
	require.Equal(t, int64(1000), pgInt(t, pool, `SELECT lifetime_credited FROM supplier_balances WHERE supplier_user_id=$1`, s.ID))
	require.Equal(t, int64(1000), pgInt(t, pool, `SELECT COALESCE(SUM(amount),0) FROM supplier_frozen_chunks WHERE supplier_user_id=$1`, s.ID))
}

func TestPGSupplierCreditConcurrentOnce(t *testing.T) {
	repos := newPGReposShared(t)
	pool := pgSharedPool(t)
	ctx := context.Background()
	sr := repos.SupplierRepo(supplierRepoCfg())

	s := seedSupplierUser(t, repos, "sup-conc@example.com")
	now := time.Now().UTC()
	seedSupplierUsage(t, pool, s.ID, 500, 800, false, now.Add(-time.Minute))
	batch, err := sr.FetchCreditBatch(ctx, 10)
	require.NoError(t, err)
	freeze, err := sr.FreezeHoursByUID(ctx, []int64{s.ID})
	require.NoError(t, err)

	// 两实例并发消费同一批：至多一次成功（余额覆盖/标记计数守卫）；另一实例
	// 或守卫失败或撞锁（40P01/55P03）——终态恒为「只记账一次」。
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = sr.ApplyCreditTx(ctx, batch, freeze)
		}(i)
	}
	wg.Wait()
	success := 0
	for _, e := range errs {
		if e == nil {
			success++
		}
	}
	require.Equal(t, 1, success, "并发同批至多一次成功")
	require.Equal(t, int64(500), pgInt(t, pool, `SELECT lifetime_credited FROM supplier_balances WHERE supplier_user_id=$1`, s.ID))
	require.Equal(t, int64(500), pgInt(t, pool, `SELECT COALESCE(SUM(amount),0) FROM supplier_frozen_chunks WHERE supplier_user_id=$1`, s.ID))
}

func TestPGSupplierCreditZeroFreezeDirectAvailable(t *testing.T) {
	repos := newPGReposShared(t)
	pool := pgSharedPool(t)
	ctx := context.Background()
	sr := repos.SupplierRepo(supplierRepoCfg())

	s := seedSupplierUser(t, repos, "sup-nofreeze@example.com")
	// 逐供应商 freeze_hours = 0 ⇒ 直接入 available、不写 chunks（§2.4 短路）。
	seedSupplierBalance(t, pool, s.ID, 0)
	pgExec(t, pool, `UPDATE supplier_balances SET freeze_hours = 0, available = 0 WHERE supplier_user_id=$1`, s.ID)
	now := time.Now().UTC()
	seedSupplierUsage(t, pool, s.ID, 4242, 5000, false, now.Add(-time.Minute))

	batch, err := sr.FetchCreditBatch(ctx, 10)
	require.NoError(t, err)
	freeze, err := sr.FreezeHoursByUID(ctx, []int64{s.ID})
	require.NoError(t, err)
	require.Equal(t, 0, freeze[s.ID])
	require.NoError(t, sr.ApplyCreditTx(ctx, batch, freeze))

	require.Equal(t, int64(4242), pgInt(t, pool, `SELECT available FROM supplier_balances WHERE supplier_user_id=$1`, s.ID))
	require.Equal(t, int64(0), pgInt(t, pool, `SELECT COUNT(*) FROM supplier_frozen_chunks WHERE supplier_user_id=$1`, s.ID), "freeze=0 不写 chunks")
}

// ---- A5/A6/A8 解冻链 ----

func TestPGSupplierThaw(t *testing.T) {
	repos := newPGReposShared(t)
	pool := pgSharedPool(t)
	ctx := context.Background()
	sr := repos.SupplierRepo(supplierRepoCfg())

	s := seedSupplierUser(t, repos, "sup-thaw@example.com")
	seedSupplierBalance(t, pool, s.ID, 0)

	// 到期桶 + 未来桶。
	pgExec(t, pool, `INSERT INTO supplier_frozen_chunks (supplier_user_id, available_at, amount) VALUES ($1, now() - interval '1 hour', 500)`, s.ID)
	pgExec(t, pool, `INSERT INTO supplier_frozen_chunks (supplier_user_id, available_at, amount) VALUES ($1, now() + interval '10 hours', 700)`, s.ID)

	n, err := sr.ThawDueChunks(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, 1, n, "仅到期桶被解冻")
	require.Equal(t, int64(500), pgInt(t, pool, `SELECT available FROM supplier_balances WHERE supplier_user_id=$1`, s.ID))
	require.Equal(t, int64(1), pgInt(t, pool, `SELECT COUNT(*) FROM supplier_frozen_chunks WHERE supplier_user_id=$1`, s.ID), "未来桶保留")

	// freeze_enabled=false 存量释放：忽略时间谓词（未来桶也释放）。
	rel, err := sr.ReleaseFrozenNoWait(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, 1, rel)
	require.Equal(t, int64(1200), pgInt(t, pool, `SELECT available FROM supplier_balances WHERE supplier_user_id=$1`, s.ID))
}

func TestPGSupplierThawCoverageGuard(t *testing.T) {
	repos := newPGReposShared(t)
	pool := pgSharedPool(t)
	ctx := context.Background()
	sr := repos.SupplierRepo(supplierRepoCfg())

	// 桶存在但余额行缺失 ⇒ 覆盖守卫触发、回滚、chunks 未丢（§5.3）。
	pgExec(t, pool, `INSERT INTO supplier_frozen_chunks (supplier_user_id, available_at, amount) VALUES (424242, now() - interval '1 hour', 100)`)
	_, err := sr.ThawDueChunks(ctx, 100)
	require.Error(t, err, "余额行缺失 ⇒ 覆盖守卫失败")
	require.Equal(t, int64(1), pgInt(t, pool, `SELECT COUNT(*) FROM supplier_frozen_chunks WHERE supplier_user_id=424242`), "chunks 未被删（回滚）")
}

// ---- A9/A10/A21/I5 申请结算 ----

func TestPGSupplierSettlementApplyIdempotency(t *testing.T) {
	repos := newPGReposShared(t)
	pool := pgSharedPool(t)
	ctx := context.Background()
	sr := repos.SupplierRepo(supplierRepoCfg())

	s := seedSupplierUser(t, repos, "sup-apply@example.com")
	seedSupplierBalance(t, pool, s.ID, 10000)
	actor := domain.FundsActor{UserID: s.ID, TokenVersion: s.TokenVersion}

	req := domain.ApplySettlementRequest{
		OperatorUID: s.ID, SupplierUID: s.ID, Kind: domain.SettlementSupplierRequest,
		AmountMillis: 8000, RequestKey: "k1",
	}
	s1, err := sr.ApplySettlement(ctx, req, actor)
	require.NoError(t, err)
	require.Equal(t, domain.SettlementPending, s1.Status)
	require.Equal(t, int64(2000), pgInt(t, pool, `SELECT available FROM supplier_balances WHERE supplier_user_id=$1`, s.ID))

	// 同 key 同参数重试 ⇒ 返回原单，available 只扣一次。
	s2, err := sr.ApplySettlement(ctx, req, actor)
	require.NoError(t, err)
	require.Equal(t, s1.ID, s2.ID)
	require.Equal(t, int64(2000), pgInt(t, pool, `SELECT available FROM supplier_balances WHERE supplier_user_id=$1`, s.ID))

	// 同 key 不同金额 ⇒ 409（ErrConflict）。
	reqBad := req
	reqBad.AmountMillis = 5000
	_, err = sr.ApplySettlement(ctx, reqBad, actor)
	require.ErrorIs(t, err, repository.ErrConflict)

	// I5：陈旧 token_version ⇒ 拒绝。
	badActor := actor
	badActor.TokenVersion = 99
	_, err = sr.ApplySettlement(ctx, domain.ApplySettlementRequest{
		OperatorUID: s.ID, SupplierUID: s.ID, Kind: domain.SettlementSupplierRequest,
		AmountMillis: 1, RequestKey: "kX",
	}, badActor)
	require.ErrorIs(t, err, repository.ErrFundsForbidden, "I5：token_version 不符 ⇒ 403")

	// I5：操作者角色不可达供应商面（role=user）⇒ 拒绝。
	plain := seedPGUser(t, repos, "plain-apply@example.com")
	_, err = sr.ApplySettlement(ctx, domain.ApplySettlementRequest{
		OperatorUID: plain.ID, SupplierUID: plain.ID, Kind: domain.SettlementSupplierRequest,
		AmountMillis: 1, RequestKey: "kY",
	}, domain.FundsActor{UserID: plain.ID, TokenVersion: plain.TokenVersion})
	require.ErrorIs(t, err, repository.ErrFundsForbidden, "I5：非供应商面可达角色 ⇒ 403")

	// I5：静态 admin token（无 uid，UserID=0）⇒ 拒绝（operator mismatch）。
	_, err = sr.ApplySettlement(ctx, req, domain.FundsActor{UserID: 0, TokenVersion: 0})
	require.ErrorIs(t, err, repository.ErrFundsForbidden)

	// 金额 ≤ 0 ⇒ ErrInvalidInput。
	_, err = sr.ApplySettlement(ctx, domain.ApplySettlementRequest{
		OperatorUID: s.ID, SupplierUID: s.ID, Kind: domain.SettlementSupplierRequest,
		AmountMillis: 0, RequestKey: "kZ",
	}, actor)
	require.ErrorIs(t, err, repository.ErrInvalidInput)

	// 余额不足 ⇒ ErrInvalidInput（400）。
	_, err = sr.ApplySettlement(ctx, domain.ApplySettlementRequest{
		OperatorUID: s.ID, SupplierUID: s.ID, Kind: domain.SettlementSupplierRequest,
		AmountMillis: 999999, RequestKey: "kBig",
	}, actor)
	require.ErrorIs(t, err, repository.ErrInvalidInput)
}

func TestPGSupplierSettlementPeriodChain(t *testing.T) {
	repos := newPGReposShared(t)
	pool := pgSharedPool(t)
	ctx := context.Background()
	sr := repos.SupplierRepo(supplierRepoCfg())

	// 供应商 + 平台管理员。
	s := seedSupplierUser(t, repos, "sup-period@example.com")
	admin := seedPGUserRole(t, repos, "admin-period@example.com", domain.RolePlatformAdmin)
	seedSupplierBalance(t, pool, s.ID, 30000)
	sActor := domain.FundsActor{UserID: s.ID, TokenVersion: s.TokenVersion}
	aActor := domain.FundsActor{UserID: admin.ID, TokenVersion: admin.TokenVersion}

	// 单 1（自申请）。
	st1, err := sr.ApplySettlement(ctx, domain.ApplySettlementRequest{
		OperatorUID: s.ID, SupplierUID: s.ID, Kind: domain.SettlementSupplierRequest, AmountMillis: 1000, RequestKey: "p1",
	}, sActor)
	require.NoError(t, err)

	// 单 2（自申请）：period_start == 单 1 period_end（链无缝）。
	st2, err := sr.ApplySettlement(ctx, domain.ApplySettlementRequest{
		OperatorUID: s.ID, SupplierUID: s.ID, Kind: domain.SettlementSupplierRequest, AmountMillis: 1000, RequestKey: "p2",
	}, sActor)
	require.NoError(t, err)
	require.True(t, st2.PeriodStart.Equal(st1.PeriodEnd), "期间链：start == 上一单 end")

	// 驳回单 1（管理员）：退还 available；期间链不分 status。
	_, err = sr.RejectSettlement(ctx, st1.ID, st1.Revision, nil, aActor)
	require.NoError(t, err)

	// 单 3（自申请）：period_start == 单 2 period_end（驳回单也占期间）。
	st3, err := sr.ApplySettlement(ctx, domain.ApplySettlementRequest{
		OperatorUID: s.ID, SupplierUID: s.ID, Kind: domain.SettlementSupplierRequest, AmountMillis: 1000, RequestKey: "p3",
	}, sActor)
	require.NoError(t, err)
	require.True(t, st3.PeriodStart.Equal(st2.PeriodEnd), "期间链含驳回单")
	// DB CHECK period_end >= period_start。
	require.False(t, st3.PeriodEnd.Before(st3.PeriodStart))
}

// TestPGRejectRefundRequiresBalanceRow A12①（⑥ 退还影响行数守卫）：余额行缺失时
// reject 必须在**退款 UPDATE 0 行**处报错并整事务回滚——状态 CAS 一并撤回，单保持
// pending（而不是被打成 rejected 却让在途负债从 I2 消失、available 不回）。
func TestPGRejectRefundRequiresBalanceRow(t *testing.T) {
	repos := newPGReposShared(t)
	pool := pgSharedPool(t)
	ctx := context.Background()
	sr := repos.SupplierRepo(supplierRepoCfg())

	s := seedSupplierUser(t, repos, "sup-reject-refund@example.com")
	admin := seedPGUserRole(t, repos, "admin-reject-refund@example.com", domain.RolePlatformAdmin)
	seedSupplierBalance(t, pool, s.ID, 10000)
	sActor := domain.FundsActor{UserID: s.ID, TokenVersion: s.TokenVersion}
	aActor := domain.FundsActor{UserID: admin.ID, TokenVersion: admin.TokenVersion}

	st, err := sr.ApplySettlement(ctx, domain.ApplySettlementRequest{
		OperatorUID: s.ID, SupplierUID: s.ID, Kind: domain.SettlementSupplierRequest, AmountMillis: 5000, RequestKey: "rr1",
	}, sActor)
	require.NoError(t, err)
	require.Equal(t, domain.SettlementPending, st.Status)

	// 余额行被人为删除（模拟余额行缺失）：退还 UPDATE 将命中 0 行。
	pgExec(t, pool, `DELETE FROM supplier_balances WHERE supplier_user_id = $1`, s.ID)

	_, err = sr.RejectSettlement(ctx, st.ID, st.Revision, nil, aActor)
	require.ErrorIs(t, err, repository.ErrInvalidInput,
		"余额行缺失时退还必须报错（0 行守卫），而非静默提交")

	// 整事务回滚：单仍为 pending（状态 CAS 被撤回）。
	var status string
	require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM supplier_settlements WHERE id=$1`, st.ID).Scan(&status))
	require.Equal(t, string(domain.SettlementPending), status, "退款 0 行 ⇒ 状态迁移一并回滚（A12①）")
}

// ---- A11/A12/A22/A24 管理面五态 + 认领 + 风控门 ----

func TestPGSupplierAdminStateMachine(t *testing.T) {
	repos := newPGReposShared(t)
	pool := pgSharedPool(t)
	ctx := context.Background()
	sr := repos.SupplierRepo(supplierRepoCfg())

	s := seedSupplierUser(t, repos, "sup-admin@example.com")
	admin := seedPGUserRole(t, repos, "admin-sm@example.com", domain.RolePlatformAdmin)
	seedSupplierBalance(t, pool, s.ID, 10000)
	aActor := domain.FundsActor{UserID: admin.ID, TokenVersion: admin.TokenVersion}

	// 代申请（admin_request）：目标须有余额行。
	st, err := sr.AdminApplySettlement(ctx, domain.ApplySettlementRequest{
		OperatorUID: admin.ID, SupplierUID: s.ID, Kind: domain.SettlementAdminRequest, AmountMillis: 6000, RequestKey: "ar1",
	}, aActor)
	require.NoError(t, err)
	require.Equal(t, domain.SettlementAdminRequest, st.Kind)
	require.Equal(t, int64(4000), pgInt(t, pool, `SELECT available FROM supplier_balances WHERE supplier_user_id=$1`, s.ID))

	// 代申请目标不存在 ⇒ 404。
	_, err = sr.AdminApplySettlement(ctx, domain.ApplySettlementRequest{
		OperatorUID: admin.ID, SupplierUID: 987654, Kind: domain.SettlementAdminRequest, AmountMillis: 1, RequestKey: "ar2",
	}, aActor)
	require.ErrorIs(t, err, repository.ErrNotFound)

	// approve：pending → approved。
	st, err = sr.ApproveSettlement(ctx, st.ID, st.Revision, aActor)
	require.NoError(t, err)
	require.Equal(t, domain.SettlementApproved, st.Status)
	// 陈旧 revision ⇒ ErrStaleRevision。
	_, err = sr.ApproveSettlement(ctx, st.ID, st.Revision, aActor)
	require.ErrorIs(t, err, repository.ErrStaleRevision)

	// claim：approved → paying + 风控门 + risk_review + payment_key + payee 固定。
	st, err = sr.ClaimSettlement(ctx, st.ID, st.Revision, st.AmountMillis, testPayeeSnapshot("payee-acc-1"), "bank evidence ref", aActor)
	require.NoError(t, err)
	require.Equal(t, domain.SettlementPaying, st.Status)
	key1 := pgText(t, pool, `SELECT payment_key FROM supplier_settlements WHERE id=$1`, st.ID)
	require.NotEmpty(t, key1)
	// claim 响应/回读含固定 payment_key + 收款快照 + risk_review（§6.5 C4：执行人可核验）。
	require.NotNil(t, st.PaymentKey)
	require.Equal(t, key1, *st.PaymentKey)
	require.NotNil(t, st.PayeeSnapshot)
	require.Contains(t, *st.PayeeSnapshot, "payee-acc-1")
	require.NotNil(t, st.RiskReview)
	// risk_review 严格反序列化 + 校验。
	rr := pgText(t, pool, `SELECT risk_review FROM supplier_settlements WHERE id=$1`, st.ID)
	require.NoError(t, supplier.ParseAndValidateRiskReview(rr, admin.ID, st.Revision-1, time.Now().UTC(), 24*time.Hour))

	// claim 金额 != 单据金额 ⇒ 400（在置 paying 之前）。
	_, err = sr.ClaimSettlement(ctx, st.ID, st.Revision, st.AmountMillis+1, testPayeeSnapshot("payee-acc-1"), "ev", aActor)
	require.ErrorIs(t, err, repository.ErrInvalidInput, "claim 金额不符 ⇒ 拒绝")

	// paying 不可 reject。
	_, err = sr.RejectSettlement(ctx, st.ID, st.Revision, nil, aActor)
	require.ErrorIs(t, err, repository.ErrStaleRevision, "paying 不在 reject 目标集")

	// confirm-failed：paying → approved（结构化「确定未支付」+ 具名确认人）。
	st, err = sr.ConfirmFailedSettlement(ctx, st.ID, st.Revision, testConfirmNotPaid("bank confirmed not paid"), aActor)
	require.NoError(t, err)
	require.Equal(t, domain.SettlementApproved, st.Status)
	require.NotNil(t, st.ReviewerUserID)
	require.Equal(t, admin.ID, *st.ReviewerUserID, "具名确认人落 reviewer_user_id")

	// 再 claim：payment_key 复用、payee 复用（不换键）。
	sid := st.ID
	st, err = sr.ClaimSettlement(ctx, sid, st.Revision, st.AmountMillis, testPayeeSnapshot("payee-acc-1"), "bank evidence ref 2", aActor)
	require.NoError(t, err)
	require.Equal(t, key1, pgText(t, pool, `SELECT payment_key FROM supplier_settlements WHERE id=$1`, sid), "恢复后不换 payment_key")

	// 收款目标不一致 ⇒ 拒绝（先于 CAS 的收款目标固定校验）。
	require.NoError(t, func() error {
		_, e := sr.ConfirmFailedSettlement(ctx, sid, st.Revision, testConfirmNotPaid("again"), aActor)
		return e
	}())
	_, err = sr.ClaimSettlement(ctx, sid, pgInt(t, pool, `SELECT revision FROM supplier_settlements WHERE id=$1`, sid), st.AmountMillis, testPayeeSnapshot("DIFFERENT-payee"), "ev", aActor)
	require.ErrorIs(t, err, repository.ErrInvalidInput, "收款信息不符 ⇒ 拒绝")

	// paid：重新读取 revision 后 claim + paid（lifetime_paid += amount；available 不变）。
	rev := pgInt(t, pool, `SELECT revision FROM supplier_settlements WHERE id=$1`, sid)
	st, err = sr.ClaimSettlement(ctx, sid, rev, st.AmountMillis, testPayeeSnapshot("payee-acc-1"), "ev", aActor)
	require.NoError(t, err)
	require.Equal(t, domain.SettlementPaying, st.Status)
	// paid：external_ref 空 ⇒ 400（必须非空核验）。
	_, err = sr.PaidSettlement(ctx, sid, st.Revision, "  ", aActor)
	require.ErrorIs(t, err, repository.ErrInvalidInput, "external_ref 空 ⇒ 拒绝")
	st, err = sr.PaidSettlement(ctx, sid, st.Revision, "ext-ref-1", aActor)
	require.NoError(t, err)
	require.Equal(t, domain.SettlementPaid, st.Status)
	require.NotNil(t, st.ExternalRef)
	require.Equal(t, "ext-ref-1", *st.ExternalRef)
	require.Equal(t, int64(6000), pgInt(t, pool, `SELECT lifetime_paid FROM supplier_balances WHERE supplier_user_id=$1`, s.ID))
	require.Equal(t, int64(4000), pgInt(t, pool, `SELECT available FROM supplier_balances WHERE supplier_user_id=$1`, s.ID), "paid 不改 available")

	// I5：非 platform_admin 操作者 ⇒ 403。
	sActor := domain.FundsActor{UserID: s.ID, TokenVersion: s.TokenVersion}
	_, err = sr.ApproveSettlement(ctx, sid, pgInt(t, pool, `SELECT revision FROM supplier_settlements WHERE id=$1`, sid), sActor)
	require.ErrorIs(t, err, repository.ErrFundsForbidden)
}

// TestPGSupplierConfirmFailedStructured 结构化「确定未支付」核验：缺结论/证据/旧执行
// 停止任一 ⇒ 失败闭合（保留 paying）；合法结构化 ⇒ 回 approved 且具名确认人落库。
func TestPGSupplierConfirmFailedStructured(t *testing.T) {
	repos := newPGReposShared(t)
	pool := pgSharedPool(t)
	ctx := context.Background()
	sr := repos.SupplierRepo(supplierRepoCfg())

	s := seedSupplierUser(t, repos, "sup-cf@example.com")
	admin := seedPGUserRole(t, repos, "admin-cf@example.com", domain.RolePlatformAdmin)
	seedSupplierBalance(t, pool, s.ID, 10000)
	aActor := domain.FundsActor{UserID: admin.ID, TokenVersion: admin.TokenVersion}

	st, err := sr.ApplySettlement(ctx, domain.ApplySettlementRequest{
		OperatorUID: s.ID, SupplierUID: s.ID, Kind: domain.SettlementSupplierRequest, AmountMillis: 1000, RequestKey: "cf1",
	}, domain.FundsActor{UserID: s.ID, TokenVersion: s.TokenVersion})
	require.NoError(t, err)
	st, err = sr.ApproveSettlement(ctx, st.ID, st.Revision, aActor)
	require.NoError(t, err)
	st, err = sr.ClaimSettlement(ctx, st.ID, st.Revision, st.AmountMillis, testPayeeSnapshot("acc-1"), "ev", aActor)
	require.NoError(t, err)

	// 未确认未支付 + 缺证据 + 旧执行未停止 ⇒ 各失败闭合，保持 paying。
	missingConclusion := domain.SupplierPayoutFailureConfirmation{Reason: "r", Evidence: "e", ConfirmedNotPaid: false, OldExecutionStopped: true}
	_, err = sr.ConfirmFailedSettlement(ctx, st.ID, st.Revision, missingConclusion, aActor)
	require.ErrorIs(t, err, repository.ErrInvalidInput, "未确认未支付 ⇒ 拒绝")
	missingEvidence := domain.SupplierPayoutFailureConfirmation{Reason: "r", Evidence: "  ", ConfirmedNotPaid: true, OldExecutionStopped: true}
	_, err = sr.ConfirmFailedSettlement(ctx, st.ID, st.Revision, missingEvidence, aActor)
	require.ErrorIs(t, err, repository.ErrInvalidInput, "缺证据 ⇒ 拒绝")
	notStopped := domain.SupplierPayoutFailureConfirmation{Reason: "r", Evidence: "e", ConfirmedNotPaid: true, OldExecutionStopped: false}
	_, err = sr.ConfirmFailedSettlement(ctx, st.ID, st.Revision, notStopped, aActor)
	require.ErrorIs(t, err, repository.ErrInvalidInput, "旧执行未停止 ⇒ 拒绝")
	require.Equal(t, "paying", pgText(t, pool, `SELECT status FROM supplier_settlements WHERE id=$1`, st.ID), "失败闭合保留 paying")

	// 合法结构化 ⇒ 回 approved，具名确认人落 reviewer_user_id。
	st, err = sr.ConfirmFailedSettlement(ctx, st.ID, st.Revision, testConfirmNotPaid("confirmed"), aActor)
	require.NoError(t, err)
	require.Equal(t, domain.SettlementApproved, st.Status)
	require.NotNil(t, st.ReviewerUserID)
	require.Equal(t, admin.ID, *st.ReviewerUserID)
}

func TestPGSupplierPayoutGate(t *testing.T) {
	repos := newPGReposShared(t)
	pool := pgSharedPool(t)
	ctx := context.Background()

	// billing disabled ⇒ claim 失败闭合。
	cfg := supplierRepoCfg()
	cfg.BillingEnabled = false
	sr := repos.SupplierRepo(cfg)
	s := seedSupplierUser(t, repos, "sup-gate@example.com")
	admin := seedPGUserRole(t, repos, "admin-gate@example.com", domain.RolePlatformAdmin)
	seedSupplierBalance(t, pool, s.ID, 5000)
	aActor := domain.FundsActor{UserID: admin.ID, TokenVersion: admin.TokenVersion}

	st, err := sr.ApplySettlement(ctx, domain.ApplySettlementRequest{
		OperatorUID: s.ID, SupplierUID: s.ID, Kind: domain.SettlementSupplierRequest, AmountMillis: 1000, RequestKey: "g1",
	}, domain.FundsActor{UserID: s.ID, TokenVersion: s.TokenVersion})
	require.NoError(t, err)
	st, err = sr.ApproveSettlement(ctx, st.ID, st.Revision, aActor)
	require.NoError(t, err)
	_, err = sr.ClaimSettlement(ctx, st.ID, st.Revision, st.AmountMillis, testPayeeSnapshot("payee"), "ev", aActor)
	require.ErrorIs(t, err, supplier.ErrPayoutGate, "billing 关闭 ⇒ 拒绝放行")
	// 仍为 approved（未置 paying）。
	require.Equal(t, "approved", pgText(t, pool, `SELECT status FROM supplier_settlements WHERE id=$1`, st.ID))

	// backlog 超阈值 ⇒ 拒绝：插一条未扣费正收益行（NOT billed）。
	sr2 := repos.SupplierRepo(supplierRepoCfg())
	pgExec(t, pool, `INSERT INTO usage_logs (request_id, model, format, error_type, cost, created_at, billed)
		VALUES ('gate-backlog','m','openai-chat','none',100, now() - interval '1 hour', false)`)
	_, err = sr2.ClaimSettlement(ctx, st.ID, st.Revision, st.AmountMillis, testPayeeSnapshot("payee"), "ev", aActor)
	require.ErrorIs(t, err, supplier.ErrPayoutGate, "队头滞留超阈值 ⇒ 拒绝")

	// 清理 backlog 后放行。
	pgExec(t, pool, `UPDATE usage_logs SET billed = true WHERE request_id = 'gate-backlog'`)
	st, err = sr2.ClaimSettlement(ctx, st.ID, st.Revision, st.AmountMillis, testPayeeSnapshot("payee"), "ev", aActor)
	require.NoError(t, err)
	require.Equal(t, domain.SettlementPaying, st.Status)
}

// ---- A19/A3 视图装载 + 归属回显 ----

func TestPGSupplierViewAndAccountOwner(t *testing.T) {
	repos := newPGReposShared(t)
	pool := pgSharedPool(t)
	ctx := context.Background()
	sr := repos.SupplierRepo(supplierRepoCfg())

	tpl := seedPGTemplate(t, repos)
	s := seedSupplierUser(t, repos, "sup-view@example.com")

	// 归属账号：CreateAccount 写 supplier_user_id（值域校验：目标可达且 active）。
	acc, err := repos.Accounts.CreateAccount(ctx, &domain.Account{
		Name: "owned-acc", TemplateID: tpl.ID, UpstreamKey: "k", MaxConcurrency: 8, Enabled: true,
		SupplierUserID: s.ID,
	})
	require.NoError(t, err)
	// 归属回显（AccountView 源数据）：GetAccount 映射 SupplierUserID。
	got, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, s.ID, got.SupplierUserID, "AccountView 归属列回填")

	// 视图装载：owner 命中；无 balances 行 ⇒ share 默认。
	owner, share, err := sr.LoadSupplierView(ctx)
	require.NoError(t, err)
	require.Equal(t, s.ID, owner[acc.ID])
	require.Equal(t, 1000, share[s.ID], "无行 ⇒ 默认 share_bp")

	// 显式 share_bp NULL ⇒ 默认；显式 0 ⇒ 0（不得依赖 map 零值）。
	seedSupplierBalance(t, pool, s.ID, 0)
	pgExec(t, pool, `UPDATE supplier_balances SET share_bp = NULL WHERE supplier_user_id=$1`, s.ID)
	_, share, err = sr.LoadSupplierView(ctx)
	require.NoError(t, err)
	require.Equal(t, 1000, share[s.ID], "显式 NULL ⇒ 默认")
	pgExec(t, pool, `UPDATE supplier_balances SET share_bp = 0 WHERE supplier_user_id=$1`, s.ID)
	_, share, err = sr.LoadSupplierView(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, share[s.ID], "显式 0 保 0")
}

// ---- A23/I6 liability + PATCH + 启动 override ----

func TestPGSupplierLiabilityAndPatch(t *testing.T) {
	repos := newPGReposShared(t)
	pool := pgSharedPool(t)
	ctx := context.Background()
	sr := repos.SupplierRepo(supplierRepoCfg())

	// 全零 ⇒ 可安全停用。
	liab, err := sr.SupplierLiability(ctx)
	require.NoError(t, err)
	require.True(t, liab.Zero())

	s := seedSupplierUser(t, repos, "sup-liab@example.com")
	seedSupplierBalance(t, pool, s.ID, 5000)
	pgExec(t, pool, `INSERT INTO supplier_frozen_chunks (supplier_user_id, available_at, amount) VALUES ($1, now() + interval '1 hour', 300)`, s.ID)
	seedSupplierUsage(t, pool, s.ID, 200, 300, false, time.Now().UTC().Add(-time.Minute))
	pgExec(t, pool, `INSERT INTO supplier_settlements (supplier_user_id, kind, amount_millis, period_start, period_end, status, revision, request_key, requested_at, requested_operator)
		VALUES ($1,'supplier_request',100,now(),now(),'pending',1,'liab1',now(),$1)`, s.ID)

	liab, err = sr.SupplierLiability(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), liab.ChunksRows)
	require.Equal(t, int64(1), liab.UncreditedRows)
	require.Equal(t, int64(5000), liab.Available)
	require.Equal(t, int64(1), liab.InFlightSettlements)
	require.False(t, liab.Zero())

	// PATCH：set + clear（回继承）。
	shareBp := 2500
	out, err := sr.PatchSupplierBalance(ctx, s.ID, &shareBp, false, nil, false)
	require.NoError(t, err)
	require.NotNil(t, out.ShareBp)
	require.Equal(t, 2500, *out.ShareBp)
	out, err = sr.PatchSupplierBalance(ctx, s.ID, nil, true, nil, false)
	require.NoError(t, err)
	require.Nil(t, out.ShareBp, "清空 ⇒ 回继承")

	// 启动 override 校验数据源。
	overrides, err := sr.ListSupplierOverrides(ctx)
	require.NoError(t, err)
	require.Len(t, overrides, 1)
	require.Equal(t, s.ID, overrides[0].UID)
}

// ---- A20 分区退休屏障 ----

func TestPGSupplierRetireBarrier(t *testing.T) {
	repos := newPGReposShared(t)
	pool := pgSharedPool(t)
	ctx := context.Background()

	// 建一个远端过去分区 + 未确认债权行（直接写子分区，避免落到他测建的分区）。
	day := time.Now().UTC().AddDate(0, 0, -300).Truncate(24 * time.Hour)
	partName := "usage_logs_" + day.Format("20060102")
	require.NoError(t, repos.Partitions.EnsureUsageLogPartitions(ctx, day, day))
	pgExec(t, pool, `INSERT INTO `+partName+`
		(request_id, model, format, error_type, cost, created_at, supplier_user_id, supplier_earn_millis, supplier_credited, billed)
		VALUES ('retire-unc','m','openai-chat','none',10,$1,7,5,false,true)`, day.Add(time.Hour))

	res, err := repos.Partitions.RetireUsageLogPartitions(ctx, day.AddDate(0, 0, 2))
	require.NoError(t, err)
	// 未确认债权分区必须进入 blocked 且仍存在（不得 DROP）。
	var blocked *domain.BlockedPartition
	for i := range res.Blocked {
		if res.Blocked[i].Name == partName {
			blocked = &res.Blocked[i]
		}
	}
	require.NotNil(t, blocked, "未确认债权分区必须进入 blocked")
	require.GreaterOrEqual(t, blocked.UncreditedEarnRows, int64(1))
	require.True(t, pgBool(t, pool, `SELECT to_regclass($1) IS NOT NULL`, partName), "被挡分区不得 DROP")

	// 确认债权但缺源日汇总行 ⇒ 仍禁止 DROP（§3.10：封账证据缺失不得清债务）。
	pgExec(t, pool, `UPDATE `+partName+` SET supplier_credited = true WHERE request_id = 'retire-unc'`)
	res, err = repos.Partitions.RetireUsageLogPartitions(ctx, day.AddDate(0, 0, 2))
	require.NoError(t, err)
	blocked = nil
	for i := range res.Blocked {
		if res.Blocked[i].Name == partName {
			blocked = &res.Blocked[i]
		}
	}
	require.NotNil(t, blocked, "缺源日汇总行必须进入 blocked")
	require.GreaterOrEqual(t, blocked.ReconMismatch, int64(1))
	require.True(t, pgBool(t, pool, `SELECT to_regclass($1) IS NOT NULL`, partName), "缺汇总行分区不得 DROP")

	// 补齐一致的源日汇总行 ⇒ 退休成功并原子封账。
	pgExec(t, pool, `INSERT INTO supplier_reconciliation (supplier_user_id, source_day, gross_cost, earned, row_count, state)
		VALUES (7, $1::date, 10, 5, 1, 'open')`, day.Format("2006-01-02"))
	res, err = repos.Partitions.RetireUsageLogPartitions(ctx, day.AddDate(0, 0, 2))
	require.NoError(t, err)
	require.False(t, pgBool(t, pool, `SELECT to_regclass($1) IS NOT NULL`, partName), "封账一致后 DROP 成功")
	require.Equal(t, "closed", pgText(t, pool, `SELECT state FROM supplier_reconciliation WHERE supplier_user_id = 7 AND source_day = $1::date`, day.Format("2006-01-02")), "封账状态须为 closed")
	require.True(t, pgBool(t, pool, `SELECT closed_revision IS NOT NULL AND closed_at IS NOT NULL FROM supplier_reconciliation WHERE supplier_user_id = 7 AND source_day = $1::date`, day.Format("2006-01-02")))
}

// TestPGSupplierRetireReconciliation 源日封账矩阵（§3.10/§3.11/A20④⑤⑥⑦⑧）：
// 缺汇总行 / 多汇总行 / 数值偏差 / earn=0 排除 / 晚到 INSERT ⇒ 不 DROP；
// 一致 ⇒ DROP + 原子封账。
func TestPGSupplierRetireReconciliation(t *testing.T) {
	repos := newPGReposShared(t)
	pool := pgSharedPool(t)
	ctx := context.Background()

	day := time.Now().UTC().AddDate(0, 0, -301).Truncate(24 * time.Hour)
	partName := "usage_logs_" + day.Format("20060102")
	require.NoError(t, repos.Partitions.EnsureUsageLogPartitions(ctx, day, day))
	dayStr := day.Format("2006-01-02")

	// earn>0 源行（uid=11，UTC 日=day）与一条 earn=0 排除行（cost>0）。
	pgExec(t, pool, `INSERT INTO `+partName+`
		(request_id, model, format, error_type, cost, created_at, supplier_user_id, supplier_earn_millis, supplier_credited, billed)
		VALUES ('rec-a','m','openai-chat','none',100,$1,11,20,true,true),
		       ('rec-zero','m','openai-chat','none',50,$1,11,0,true,true)`, day.Add(2*time.Hour))

	retire := func() domain.UsageLogRetireResult {
		t.Helper()
		r, err := repos.Partitions.RetireUsageLogPartitions(ctx, day.AddDate(0, 0, 2))
		require.NoError(t, err)
		return r
	}
	blockedFor := func(r domain.UsageLogRetireResult) *domain.BlockedPartition {
		for i := range r.Blocked {
			if r.Blocked[i].Name == partName {
				return &r.Blocked[i]
			}
		}
		return nil
	}

	// ① 缺汇总行 ⇒ blocked（mismatch），分区仍在。
	require.NotNil(t, blockedFor(retire()), "缺汇总行必须 blocked")
	require.True(t, pgBool(t, pool, `SELECT to_regclass($1) IS NOT NULL`, partName))

	// ② 多汇总行（同 source_day 但非源内 uid）⇒ blocked，且不改该多出行。
	pgExec(t, pool, `INSERT INTO supplier_reconciliation (supplier_user_id, source_day, gross_cost, earned, row_count, state)
		VALUES (99, $1::date, 1, 1, 1, 'open')`, dayStr)
	b := blockedFor(retire())
	require.NotNil(t, b, "多汇总行必须 blocked")
	require.GreaterOrEqual(t, b.ReconMismatch, int64(1))
	require.Equal(t, "open", pgText(t, pool, `SELECT state FROM supplier_reconciliation WHERE supplier_user_id = 99`), "多出行不得被误封账")
	require.True(t, pgBool(t, pool, `SELECT to_regclass($1) IS NOT NULL`, partName))

	// ③ 数值偏差（earned 少 1）⇒ blocked。
	pgExec(t, pool, `DELETE FROM supplier_reconciliation WHERE supplier_user_id = 99`)
	pgExec(t, pool, `INSERT INTO supplier_reconciliation (supplier_user_id, source_day, gross_cost, earned, row_count, state)
		VALUES (11, $1::date, 100, 19, 1, 'open')`, dayStr)
	b = blockedFor(retire())
	require.NotNil(t, b, "数值偏差必须 blocked")
	require.GreaterOrEqual(t, b.ReconMismatch, int64(1))

	// ④ 晚到 INSERT（credit 未及记账的 earn>0 行）与已一致汇总并存 ⇒ 未确认屏障挡。
	pgExec(t, pool, `DELETE FROM supplier_reconciliation WHERE supplier_user_id = 11`)
	pgExec(t, pool, `INSERT INTO supplier_reconciliation (supplier_user_id, source_day, gross_cost, earned, row_count, state)
		VALUES (11, $1::date, 100, 20, 1, 'open')`, dayStr)
	pgExec(t, pool, `INSERT INTO `+partName+`
		(request_id, model, format, error_type, cost, created_at, supplier_user_id, supplier_earn_millis, supplier_credited, billed)
		VALUES ('rec-late','m','openai-chat','none',100,$1,11,20,false,true)`, day.Add(3*time.Hour))
	b = blockedFor(retire())
	require.NotNil(t, b, "晚到未记账行必须 blocked")
	require.GreaterOrEqual(t, b.UncreditedEarnRows, int64(1))
	require.True(t, pgBool(t, pool, `SELECT to_regclass($1) IS NOT NULL`, partName))

	// ⑤ 收尾：补正汇总（含晚到行）⇒ 一致 DROP + 源日键原子封账（earn=0 行不入 src）。
	pgExec(t, pool, `DELETE FROM supplier_reconciliation WHERE supplier_user_id = 11`)
	pgExec(t, pool, `UPDATE `+partName+` SET supplier_credited = true WHERE request_id = 'rec-late'`)
	// 晚到行同 uid 同源日 ⇒ 同一 (uid, day) 键，聚合 = 200/40/2。
	pgExec(t, pool, `INSERT INTO supplier_reconciliation (supplier_user_id, source_day, gross_cost, earned, row_count, state)
		VALUES (11, $1::date, 200, 40, 2, 'open')`, dayStr)
	r := retire()
	require.Nil(t, blockedFor(r), "一致后必须 DROP：%+v", r.Blocked)
	require.False(t, pgBool(t, pool, `SELECT to_regclass($1) IS NOT NULL`, partName), "一致后 DROP 成功")
	require.Equal(t, int64(1), pgInt(t, pool, `SELECT count(*) FROM supplier_reconciliation WHERE state='closed' AND closed_revision IS NOT NULL AND closed_at IS NOT NULL`))
}

// TestPGSupplierRetireReconciliationCrossDay 跨 UTC 日作用域（A20⑤）：同 uid 另一
// 源日（另一分区）的汇总行存在时，退役本日分区不得被误判为「多汇总行」，且只封本日。
func TestPGSupplierRetireReconciliationCrossDay(t *testing.T) {
	repos := newPGReposShared(t)
	pool := pgSharedPool(t)
	ctx := context.Background()

	day := time.Now().UTC().AddDate(0, 0, -302).Truncate(24 * time.Hour)
	next := day.AddDate(0, 0, 1)
	partName := "usage_logs_" + day.Format("20060102")
	require.NoError(t, repos.Partitions.EnsureUsageLogPartitions(ctx, day, day))
	// 本日分区源行（uid=21）。
	pgExec(t, pool, `INSERT INTO `+partName+`
		(request_id, model, format, error_type, cost, created_at, supplier_user_id, supplier_earn_millis, supplier_credited, billed)
		VALUES ('xd-a','m','openai-chat','none',30,$1,21,7,true,true)`, day.Add(time.Hour))
	// 另一源日汇总（uid=21, day+1）与本日汇总同时存在。
	pgExec(t, pool, `INSERT INTO supplier_reconciliation (supplier_user_id, source_day, gross_cost, earned, row_count, state)
		VALUES (21, $1::date, 999, 999, 9, 'open'), (21, $2::date, 30, 7, 1, 'open')`,
		next.Format("2006-01-02"), day.Format("2006-01-02"))

	res, err := repos.Partitions.RetireUsageLogPartitions(ctx, day.AddDate(0, 0, 3))
	require.NoError(t, err)
	require.Empty(t, res.Blocked, "另一源日汇总不得误判为本日多汇总行")
	require.False(t, pgBool(t, pool, `SELECT to_regclass($1) IS NOT NULL`, partName))
	// 本日已封账，另一源日保持 open。
	require.Equal(t, "closed", pgText(t, pool, `SELECT state FROM supplier_reconciliation WHERE supplier_user_id = 21 AND source_day = $1::date`, day.Format("2006-01-02")))
	require.Equal(t, "open", pgText(t, pool, `SELECT state FROM supplier_reconciliation WHERE supplier_user_id = 21 AND source_day = $1::date`, next.Format("2006-01-02")))
}

// ---- M2 DB CHECK ----

func TestPGSupplierDBConstraints(t *testing.T) {
	repos := newPGReposShared(t)
	pool := pgSharedPool(t)
	_ = repos

	// share_bp 越界 ⇒ 拒绝。
	_, err := pool.Exec(context.Background(), `INSERT INTO supplier_balances (supplier_user_id, available, lifetime_credited, lifetime_paid, share_bp, created_at, updated_at)
		VALUES (1,0,0,0,10001,now(),now())`)
	require.Error(t, err, "share_bp > 10000 违约")
	// freeze_hours 负 ⇒ 拒绝。
	_, err = pool.Exec(context.Background(), `INSERT INTO supplier_balances (supplier_user_id, available, lifetime_credited, lifetime_paid, freeze_hours, created_at, updated_at)
		VALUES (2,0,0,0,-1,now(),now())`)
	require.Error(t, err, "freeze_hours < 0 违约")
	// available 负 ⇒ 拒绝。
	_, err = pool.Exec(context.Background(), `INSERT INTO supplier_balances (supplier_user_id, available, lifetime_credited, lifetime_paid, created_at, updated_at)
		VALUES (3,-1,0,0,now(),now())`)
	require.Error(t, err, "available < 0 违约")
	// chunk amount <= 0 ⇒ 拒绝。
	_, err = pool.Exec(context.Background(), `INSERT INTO supplier_frozen_chunks (supplier_user_id, available_at, amount) VALUES (1, now(), 0)`)
	require.Error(t, err, "amount <= 0 违约")
}

func pgText(t *testing.T, pool *pgxpool.Pool, q string, args ...any) string {
	t.Helper()
	var v string
	require.NoError(t, pool.QueryRow(context.Background(), q, args...).Scan(&v))
	return v
}

func pgBool(t *testing.T, pool *pgxpool.Pool, q string, args ...any) bool {
	t.Helper()
	var v bool
	require.NoError(t, pool.QueryRow(context.Background(), q, args...).Scan(&v))
	return v
}
