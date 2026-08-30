// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// 真实 PG 基座（newPGReposShared；TEST_DATABASE_URL 未设置 → Skip）：
// 兑换码 Task 1 全部测试 —— code 唯一冲突、批量生成、use 唯一约束、批量失效幂等、
// 条件递增并发防超卖（评审 I-2）、原子资源方法（评审 I-1）、WithTx 回滚（评审 I-1）。

func codeFor(tag string, typ domain.RedemptionType, maxUses int) *domain.RedemptionCode {
	return &domain.RedemptionCode{
		Code: fmt.Sprintf("AAAAAA-%06s", tag[:6]), Type: typ, Value: 100,
		MaxUses: maxUses, Status: domain.RedemptionStatusActive,
	}
}

// TestRedemptionCodesPG 批量生成 + 读取 + 列表（分页/筛选/sort 白名单）+
// code 唯一冲突映射 ErrConflict。
func TestRedemptionCodesPG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	// code 唯一冲突 → ErrConflict（重复生成同 code；批量插入整体失败，无残留）
	c1 := codeFor("BAL001", domain.RedemptionTypeBalance, 1)
	require.NoError(t, repos.CreateCodes(ctx, []*domain.RedemptionCode{c1}))
	dup := *c1
	err := repos.CreateCodes(ctx, []*domain.RedemptionCode{&dup})
	require.ErrorIs(t, err, repository.ErrConflict, "重复 code → ErrConflict（409 语义）")
	_, err = repos.GetByCode(ctx, c1.Code)
	require.NoError(t, err, "冲突插入不残留新行")

	// 批量生成 3 码（两类型 + 备注/到期/多人）
	remark := "运营活动"
	expires := time.Now().Add(24 * time.Hour)
	batch := []*domain.RedemptionCode{
		codeFor("BAL002", domain.RedemptionTypeBalance, 1),
		{Code: "AAAAAA-CON001", Type: domain.RedemptionTypeConcurrency, Value: 5,
			MaxUses: 3, Status: domain.RedemptionStatusActive, Remark: &remark},
		{Code: "AAAAAA-TMP001", Type: domain.RedemptionTypeTempBalance, Value: 500,
			MaxUses: 1, Status: domain.RedemptionStatusActive, ExpiresAt: &expires,
			ResourceExpiresAt: &expires, CreatedBy: 42},
	}
	require.NoError(t, repos.CreateCodes(ctx, batch))

	// GetByCode / GetCode 字段 roundtrip
	got, err := repos.GetByCode(ctx, "AAAAAA-CON001")
	require.NoError(t, err)
	require.Equal(t, domain.RedemptionTypeConcurrency, got.Type)
	require.Equal(t, int64(5), got.Value)
	require.Equal(t, 3, got.MaxUses)
	require.NotNil(t, got.Remark)
	require.Equal(t, "运营活动", *got.Remark)
	require.Nil(t, got.ExpiresAt, "concurrency 码未设 expires_at → nil")
	gotByID, err := repos.GetCode(ctx, got.ID)
	require.NoError(t, err)
	require.Equal(t, got.Code, gotByID.Code)
	got2, err := repos.GetByCode(ctx, "AAAAAA-TMP001")
	require.NoError(t, err)
	require.Equal(t, int64(42), got2.CreatedBy, "created_by roundtrip")
	require.NotNil(t, got2.ExpiresAt, "expires_at roundtrip")
	require.NotNil(t, got2.ResourceExpiresAt)
	require.Equal(t, domain.RedemptionStatusActive, got2.Status, "status 默认 active 落库")

	// 缺失 → ErrNotFound
	_, err = repos.GetByCode(ctx, "ZZZZZZ-ZZZZZZ")
	require.ErrorIs(t, err, repository.ErrNotFound)
	require.Contains(t, err.Error(), `code="ZZZZZZ-ZZZZZZ"`)
	_, err = repos.GetCode(ctx, 999999)
	require.ErrorIs(t, err, repository.ErrNotFound)

	// ListCodes：全部（默认分页 id desc）
	rows, total, err := repos.ListCodes(ctx, repository.ListQuery{Limit: 10}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, int64(4), total)
	require.Len(t, rows, 4)

	// 筛选 type=balance
	bt := domain.RedemptionTypeBalance
	rows, total, err = repos.ListCodes(ctx, repository.ListQuery{}, &bt, nil)
	require.NoError(t, err)
	require.Equal(t, int64(2), total, "balance 码 2 个")
	for _, c := range rows {
		require.Equal(t, domain.RedemptionTypeBalance, c.Type)
	}

	// 筛选 status=active + 分页 + sort 白名单（created_at asc）
	as := domain.RedemptionStatusActive
	rows, total, err = repos.ListCodes(ctx, repository.ListQuery{Limit: 2, Offset: 0, Sort: "created_at", Order: "asc"}, nil, &as)
	require.NoError(t, err)
	require.Equal(t, int64(4), total)
	require.Len(t, rows, 2)

	// 非法 sort → ErrInvalidSort
	_, _, err = repos.ListCodes(ctx, repository.ListQuery{Sort: "bogus"}, nil, nil)
	require.ErrorIs(t, err, repository.ErrInvalidSort)
}

// TestRedemptionUsePG 兑换记录：CreateUse 唯一约束冲突 → ErrConflict（同 user 重复）；
// GetUse / ListCodeUses（分页）。
func TestRedemptionUsePG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	code := codeFor("USE001", domain.RedemptionTypeBalance, 10)
	require.NoError(t, repos.CreateCodes(ctx, []*domain.RedemptionCode{code}))
	got, err := repos.GetByCode(ctx, code.Code)
	require.NoError(t, err)

	// 两用户兑换同一码
	u1 := seedPGUser(t, repos, "use1@example.com")
	u2 := seedPGUser(t, repos, "use2@example.com")
	require.NoError(t, repos.CreateUse(ctx, &domain.RedemptionUse{CodeID: got.ID, UserID: u1.ID, Value: 100}))
	require.NoError(t, repos.CreateUse(ctx, &domain.RedemptionUse{CodeID: got.ID, UserID: u2.ID, Value: 100}))

	// 同用户重复 → ErrConflict（DB 唯一兜底幂等）
	err = repos.CreateUse(ctx, &domain.RedemptionUse{CodeID: got.ID, UserID: u1.ID, Value: 100})
	require.ErrorIs(t, err, repository.ErrConflict, "UNIQUE(code_id, user_id) → ErrConflict（409 语义）")

	// GetUse 命中/未命中
	rec, err := repos.GetUse(ctx, got.ID, u1.ID)
	require.NoError(t, err)
	require.Equal(t, u1.ID, rec.UserID)
	require.Equal(t, int64(100), rec.Value)
	_, err = repos.GetUse(ctx, got.ID, 999999)
	require.ErrorIs(t, err, repository.ErrNotFound, "无兑换记录 → ErrNotFound")

	// ListCodeUses 审计分页
	rows, total, err := repos.ListCodeUses(ctx, got.ID, repository.ListQuery{Limit: 10, Sort: "user_id", Order: "asc"})
	require.NoError(t, err)
	require.Equal(t, int64(2), total)
	require.Len(t, rows, 2)
	require.Equal(t, u1.ID, rows[0].UserID, "sort user_id asc")
	_, _, err = repos.ListCodeUses(ctx, got.ID, repository.ListQuery{Sort: "bogus"})
	require.ErrorIs(t, err, repository.ErrInvalidSort)
}

// TestRedemptionUseRetentionPG F3-2 redemption_uses 90 天 TTL 有界批删（真实
// PG）：超窗行清理、窗口内行保留、批删有界（超大批注入 → 多轮收敛，断言每轮
// 上限 5000）。普通表无分区可 DROP——DELETE 批删路径（retention worker 周期
// 任务内调用）。
func TestRedemptionUseRetentionPG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	pool := pgSharedPool(t)

	// redemption_uses.code_id 有 FK → 先建码
	code := codeFor("F3R001", domain.RedemptionTypeBalance, 20000)
	require.NoError(t, repos.CreateCodes(ctx, []*domain.RedemptionCode{code}))
	got, err := repos.GetByCode(ctx, code.Code)
	require.NoError(t, err)

	// 超窗行 12000 条（created_at = now - 120 天；user_id 经 generate_series 递增，
	// 不撞 UNIQUE(code_id, user_id)）+ 窗口内行 3 条（now - 30 天，90 天内保留）
	pgExec(t, pool,
		`INSERT INTO redemption_uses (code_id, user_id, value, created_at)
		 SELECT $1, g, 100, now() - interval '120 days' FROM generate_series(1, 12000) g`, got.ID)
	pgExec(t, pool,
		`INSERT INTO redemption_uses (code_id, user_id, value, created_at) VALUES
		 ($1, 20001, 100, now() - interval '30 days'),
		 ($1, 20002, 100, now() - interval '30 days'),
		 ($1, 20003, 100, now() - interval '30 days')`, got.ID)

	cutoff := time.Now().UTC().AddDate(0, 0, -90)
	require.Equal(t, int64(12000), pgCount(t, pool, `SELECT COUNT(*) FROM redemption_uses WHERE created_at < $1`, cutoff), "预置超窗行")
	require.Equal(t, int64(3), pgCount(t, pool, `SELECT COUNT(*) FROM redemption_uses WHERE created_at >= $1`, cutoff), "预置窗口内行")

	// 多轮收敛 + 每轮上限：5000/5000/2000/0（有界批删防长事务持锁；上限与
	// repository.redemptionUsesDeleteBatchLimit 同步锚定）
	for _, want := range []int64{5000, 5000, 2000, 0} {
		n, err := repos.DeleteRedemptionUsesBefore(ctx, cutoff)
		require.NoError(t, err)
		require.LessOrEqual(t, int64(n), int64(5000), "每轮批删不得超过上限 5000")
		require.Equal(t, want, int64(n), "本轮删除行数（多轮收敛）")
	}
	require.Equal(t, int64(0), pgCount(t, pool, `SELECT COUNT(*) FROM redemption_uses WHERE created_at < $1`, cutoff), "超窗行全部清理")

	// 窗口内行保留（审计语义：90 天窗口内的兑换记录即审计证据）
	require.Equal(t, int64(3), pgCount(t, pool, `SELECT COUNT(*) FROM redemption_uses WHERE created_at >= $1`, cutoff), "窗口内行不受批删影响")
	rows, total, err := repos.ListCodeUses(ctx, got.ID, repository.ListQuery{Limit: 10, Sort: "user_id", Order: "asc"})
	require.NoError(t, err)
	require.Equal(t, int64(3), total, "ent 读路径一致：仅窗口内行可查")
	require.Equal(t, int64(20001), rows[0].UserID)
}

// TestDeactivateCodesPG 批量失效：单事务、返回受影响数、已 disabled no-op 幂等。
func TestDeactivateCodesPG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	codes := []*domain.RedemptionCode{codeFor("DEA001", domain.RedemptionTypeBalance, 1),
		codeFor("DEA002", domain.RedemptionTypeBalance, 1), codeFor("DEA003", domain.RedemptionTypeBalance, 1)}
	require.NoError(t, repos.CreateCodes(ctx, codes))

	// 空 ids → (0, nil)
	n, err := repos.DeactivateCodes(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, int64(0), n)

	// 失效前 2 个 → 受影响 2
	got1, _ := repos.GetByCode(ctx, codes[0].Code)
	got2, _ := repos.GetByCode(ctx, codes[1].Code)
	n, err = repos.DeactivateCodes(ctx, []int64{got1.ID, got2.ID})
	require.NoError(t, err)
	require.Equal(t, int64(2), n, "新失效数 = 2")
	for _, c := range []*domain.RedemptionCode{got1, got2} {
		row, err := repos.GetCode(ctx, c.ID)
		require.NoError(t, err)
		require.Equal(t, domain.RedemptionStatusDisabled, row.Status, "状态落库 disabled")
	}
	got3, _ := repos.GetByCode(ctx, codes[2].Code)
	require.Equal(t, domain.RedemptionStatusActive, got3.Status, "未失效码保持 active")

	// 再次失效同一批（含已 disabled 与 active）→ 只计新增 1，幂等 no-op
	n, err = repos.DeactivateCodes(ctx, []int64{got1.ID, got2.ID, got3.ID})
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "已 disabled no-op，仅第三个新失效")
	row, err := repos.GetCode(ctx, got3.ID)
	require.NoError(t, err)
	require.Equal(t, domain.RedemptionStatusDisabled, row.Status)

	// 全 disabled 后再失效 → 0
	n, err = repos.DeactivateCodes(ctx, []int64{got1.ID, got2.ID, got3.ID})
	require.NoError(t, err)
	require.Equal(t, int64(0), n, "全 no-op")
}

// TestIncrementUsedConcurrentPG 条件递增防超卖（评审 I-2）：max_uses=2 的码，
// 3 并发 IncrementUsed → 恰 2 个 true 1 个 false（DB 行锁 + WHERE 原子，不超卖）。
func TestIncrementUsedConcurrentPG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	code := codeFor("INC001", domain.RedemptionTypeBalance, 2)
	require.NoError(t, repos.CreateCodes(ctx, []*domain.RedemptionCode{code}))
	got, err := repos.GetByCode(ctx, code.Code)
	require.NoError(t, err)

	const n = 3
	type result struct {
		ok  bool
		err error
	}
	results := make(chan result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := repos.IncrementUsed(ctx, got.ID)
			results <- result{ok, err}
		}()
	}
	wg.Wait()
	close(results)

	trues, falses := 0, 0
	for res := range results {
		require.NoError(t, res.err)
		if res.ok {
			trues++
		} else {
			falses++
		}
	}
	require.Equal(t, 2, trues, "恰 2 次成功（max_uses=2）")
	require.Equal(t, 1, falses, "第 3 次 false = 已用尽")

	row, err := repos.GetCode(ctx, got.ID)
	require.NoError(t, err)
	require.Equal(t, 2, row.UsedCount, "used_count 精确落 2（无超卖）")
	// 用尽后再递增 → false
	ok, err := repos.IncrementUsed(ctx, got.ID)
	require.NoError(t, err)
	require.False(t, ok, "已用尽 → false")
}

// TestUserResourceUpdatePG 原子资源方法（评审 I-1）：UpdateUserBalance 并发增量不丢；
// UpdateUserMaxConcurrency 0 特判（0 → value；非 0 → 累加）。
func TestUserResourceUpdatePG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	u := seedPGUser(t, repos, "atomic@example.com")

	// 5 并发 +10 → balance 精确 50（服务端原子，无读改写丢失）
	const workers = 5
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- repos.UpdateUserBalance(ctx, u.ID, 10)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	got, err := repos.Users.GetUser(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, int64(50), got.Balance, "并发增量不丢")

	// 0 特判：新用户 max_concurrency=0 → 直接设 value（5）
	require.NoError(t, repos.UpdateUserMaxConcurrency(ctx, u.ID, 5))
	got, err = repos.Users.GetUser(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, 5, got.MaxConcurrency, "0=不限 → 设为 value")

	// 非 0 → 累加（5 + 3 = 8）
	require.NoError(t, repos.UpdateUserMaxConcurrency(ctx, u.ID, 3))
	got, err = repos.Users.GetUser(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, 8, got.MaxConcurrency, "非 0 → 累加")

	// 用户不存在 → ErrNotFound（0 行受影响，兑换编排整体回滚）
	err = repos.UpdateUserBalance(ctx, 999999, 10)
	require.ErrorIs(t, err, repository.ErrNotFound)
	err = repos.UpdateUserMaxConcurrency(ctx, 999999, 10)
	require.ErrorIs(t, err, repository.ErrNotFound)
}

// TestWithTxPG 事务形态（评审 I-1）：
// 1) 提交路径：tx 内 建码 + 原子更新 + use + 条件递增，全落库；
// 2) 回滚路径：fn 返回错误 → 全部无残留（含 raw SQL 原子更新，走 tx 连接）。
func TestWithTxPG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	t.Run("commit all ops", func(t *testing.T) {
		u := seedPGUser(t, repos, "tx-commit@example.com")
		code := codeFor("TX0001", domain.RedemptionTypeBalance, 1)
		err := repos.WithTx(ctx, func(tr repository.TxStore) error {
			if err := tr.CreateCodes(ctx, []*domain.RedemptionCode{code}); err != nil {
				return err
			}
			got, err := tr.GetByCode(ctx, code.Code)
			if err != nil {
				return err
			}
			if err := tr.UpdateUserBalance(ctx, u.ID, 100); err != nil {
				return err
			}
			if err := tr.CreateUse(ctx, &domain.RedemptionUse{CodeID: got.ID, UserID: u.ID, Value: 100}); err != nil {
				return err
			}
			ok, err := tr.IncrementUsed(ctx, got.ID)
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("expected increment success")
			}
			return nil
		})
		require.NoError(t, err)

		got, err := repos.GetByCode(ctx, code.Code)
		require.NoError(t, err, "tx 内建码已提交")
		require.Equal(t, 1, got.UsedCount, "tx 内条件递增已提交")
		rec, err := repos.GetUse(ctx, got.ID, u.ID)
		require.NoError(t, err)
		require.Equal(t, int64(100), rec.Value)
		user, err := repos.Users.GetUser(ctx, u.ID)
		require.NoError(t, err)
		require.Equal(t, int64(100), user.Balance, "tx 内原子更新已提交")
	})

	t.Run("rollback on fn error", func(t *testing.T) {
		u := seedPGUser(t, repos, "tx-rollback@example.com")
		code := codeFor("TX0002", domain.RedemptionTypeConcurrency, 1)
		err := repos.WithTx(ctx, func(tr repository.TxStore) error {
			if err := tr.CreateCodes(ctx, []*domain.RedemptionCode{code}); err != nil {
				return err
			}
			got, err := tr.GetByCode(ctx, code.Code)
			if err != nil {
				return err
			}
			if err := tr.UpdateUserBalance(ctx, u.ID, 100); err != nil {
				return err
			}
			if err := tr.UpdateUserMaxConcurrency(ctx, u.ID, 5); err != nil {
				return err
			}
			if err := tr.CreateUse(ctx, &domain.RedemptionUse{CodeID: got.ID, UserID: u.ID, Value: 100}); err != nil {
				return err
			}
			if _, err := tr.IncrementUsed(ctx, got.ID); err != nil {
				return err
			}
			return errors.New("boom: 任一步失败整体回滚")
		})
		require.Error(t, err)

		_, err = repos.GetByCode(ctx, code.Code)
		require.ErrorIs(t, err, repository.ErrNotFound, "tx 内建的码无残留")
		user, err := repos.Users.GetUser(ctx, u.ID)
		require.NoError(t, err)
		require.Zero(t, user.Balance, "raw SQL 原子更新走 tx 连接：余额已回滚")
		require.Zero(t, user.MaxConcurrency, "raw SQL 原子更新走 tx 连接：并发数已回滚")
	})
}
