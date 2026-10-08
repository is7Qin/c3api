// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// supplier_cas_pg_test.go 补 A11（五态 CAS 并发竞争）与 A21③④（申请幂等键并发/
// 唯一冲突回滚条件扣）的**真实 PG 并发**验收（spec 2026-10-09 §7）。
// 跑法：TEST_DATABASE_URL=... go test -count=1 -p 1 ./internal/repository/ -run PG -v

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// runConcurrent 用公共起始闸并发执行 n 次 fn（最大限度制造竞争）；收集每次错误。
// 不并发调用 require/t.FailNow（failNow 只能从测试 goroutine 调用）。
func runConcurrent(n int, fn func(i int) error) []error {
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = fn(i)
		}(i)
	}
	close(start)
	wg.Wait()
	return errs
}

func countNil(errs []error) int {
	n := 0
	for _, e := range errs {
		if e == nil {
			n++
		}
	}
	return n
}

// TestPGSupplierAdminCASConcurrency A11①②③⑤：并发 approve 恰一次成功；并发 claim
// 恰一人认领；并发 reject 恰退还一次（不重复退款）。
func TestPGSupplierAdminCASConcurrency(t *testing.T) {
	repos := newPGReposShared(t)
	pool := pgSharedPool(t)
	ctx := context.Background()
	sr := repos.SupplierRepo(supplierRepoCfg())

	s := seedSupplierUser(t, repos, "sup-cas@example.com")
	admin := seedPGUserRole(t, repos, "admin-cas@example.com", domain.RolePlatformAdmin)
	seedSupplierBalance(t, pool, s.ID, 10000)
	aActor := domain.FundsActor{UserID: admin.ID, TokenVersion: admin.TokenVersion}

	// 建单（pending）。
	st, err := sr.AdminApplySettlement(ctx, domain.ApplySettlementRequest{
		OperatorUID: admin.ID, SupplierUID: s.ID, Kind: domain.SettlementAdminRequest,
		AmountMillis: 6000, RequestKey: "cas-approve",
	}, aActor)
	require.NoError(t, err)

	// A11①：并发 approve（同 revision）⇒ 恰好一次成功（其余 revision 冲突）。
	errs := runConcurrent(8, func(int) error {
		_, e := sr.ApproveSettlement(ctx, st.ID, st.Revision, aActor)
		return e
	})
	require.Equal(t, 1, countNil(errs), "并发 approve 恰好一次成功（CAS）")
	require.Equal(t, "approved", pgText(t, pool, `SELECT status FROM supplier_settlements WHERE id=$1`, st.ID))

	// A11③⑤：并发 claim（approved→paying）⇒ 恰好一人认领。
	rev := pgInt(t, pool, `SELECT revision FROM supplier_settlements WHERE id=$1`, st.ID)
	errs = runConcurrent(8, func(int) error {
		_, e := sr.ClaimSettlement(ctx, st.ID, rev, st.AmountMillis, testPayeeSnapshot("cas-acc"), testRiskEvidence(rev), aActor)
		return e
	})
	require.Equal(t, 1, countNil(errs), "并发 claim 恰好一人认领")
	require.Equal(t, "paying", pgText(t, pool, `SELECT status FROM supplier_settlements WHERE id=$1`, st.ID))

	// A11②：并发 reject（approved 态）⇒ 恰好退还一次（available 只回补一次）。
	st2, err := sr.AdminApplySettlement(ctx, domain.ApplySettlementRequest{
		OperatorUID: admin.ID, SupplierUID: s.ID, Kind: domain.SettlementAdminRequest,
		AmountMillis: 3000, RequestKey: "cas-reject",
	}, aActor)
	require.NoError(t, err)
	_, err = sr.ApproveSettlement(ctx, st2.ID, st2.Revision, aActor)
	require.NoError(t, err)
	avBefore := pgInt(t, pool, `SELECT available FROM supplier_balances WHERE supplier_user_id=$1`, s.ID)
	rev2 := pgInt(t, pool, `SELECT revision FROM supplier_settlements WHERE id=$1`, st2.ID)
	errs = runConcurrent(8, func(int) error {
		_, e := sr.RejectSettlement(ctx, st2.ID, rev2, nil, aActor)
		return e
	})
	require.Equal(t, 1, countNil(errs), "并发 reject 恰好一次成功")
	require.Equal(t, "rejected", pgText(t, pool, `SELECT status FROM supplier_settlements WHERE id=$1`, st2.ID))
	avAfter := pgInt(t, pool, `SELECT available FROM supplier_balances WHERE supplier_user_id=$1`, s.ID)
	require.Equal(t, avBefore+3000, avAfter, "并发 reject 恰好退还一次（不重复退款）")
}

// TestPGSupplierApplyIdempotencyConcurrency A21③④：并发同 request_key ⇒ 唯一致
// 条件扣在冲突时整事务回滚，恰一单、available 只扣一次；「提交成功但响应丢失」
// 后重试命中同一张单（幂等 no-op）。
func TestPGSupplierApplyIdempotencyConcurrency(t *testing.T) {
	repos := newPGReposShared(t)
	pool := pgSharedPool(t)
	ctx := context.Background()
	sr := repos.SupplierRepo(supplierRepoCfg())

	s := seedSupplierUser(t, repos, "sup-apply-c@example.com")
	seedSupplierBalance(t, pool, s.ID, 10000)
	actor := domain.FundsActor{UserID: s.ID, TokenVersion: s.TokenVersion}
	req := domain.ApplySettlementRequest{
		OperatorUID: s.ID, SupplierUID: s.ID, Kind: domain.SettlementSupplierRequest,
		AmountMillis: 3000, RequestKey: "conc-key",
	}

	const n = 8
	results := make([]*domain.SupplierSettlement, n)
	var okCount atomic.Int64
	errs := runConcurrent(n, func(i int) error {
		st, e := sr.ApplySettlement(ctx, req, actor)
		if e == nil {
			results[i] = st
			okCount.Add(1)
		}
		return e
	})
	require.GreaterOrEqual(t, countNil(errs), 1, "并发同 key 至少一次成功")
	require.Equal(t, int64(1), pgInt(t, pool,
		`SELECT COUNT(*) FROM supplier_settlements WHERE requested_operator=$1 AND request_key=$2`, s.ID, "conc-key"),
		"并发同 key 必须恰一单")
	require.Equal(t, int64(7000), pgInt(t, pool,
		`SELECT available FROM supplier_balances WHERE supplier_user_id=$1`, s.ID),
		"唯一冲突回滚条件扣：available 只扣一次")
	// 所有成功返回指向同一张单（并发重试命中同一单，不产生第二张）。
	var id int64
	for _, r := range results {
		if r == nil {
			continue
		}
		if id == 0 {
			id = r.ID
		}
		require.Equal(t, id, r.ID, "并发的成功返回必须是同一张单")
	}

	// 提交成功但响应丢失后重试 ⇒ 返回原单、available 不再扣。
	again, err := sr.ApplySettlement(ctx, req, actor)
	require.NoError(t, err)
	require.Equal(t, id, again.ID, "响应丢失重试命中原单（幂等 no-op）")
	require.Equal(t, int64(7000), pgInt(t, pool,
		`SELECT available FROM supplier_balances WHERE supplier_user_id=$1`, s.ID))
}
