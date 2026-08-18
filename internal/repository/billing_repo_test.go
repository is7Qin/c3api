// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent/usagelog"
	"github.com/is7qin/c3api/internal/repository"
)

// logFor 构造测试计费日志（DeductAndLog 插入断言用）。
func logFor(userID int64, requestID string) *domain.UsageLog {
	return &domain.UsageLog{
		RequestID: requestID, UserID: userID, Model: "gpt-4o",
		Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone,
		LatencyMS: 10, InputTokens: 3, OutputTokens: 5, TotalTokens: 8,
		Cost: 130, BillingTier: "auto",
		CreatedAt: time.Now(),
	}
}

// fullLogFor 填满全部可选列的计费日志（#37 P2 回归锚）：21 列 × 4000 行 =
// 84,000 参数，单批 CreateBulk 必超 PG 65535 上限（logFor 仅 16 列 × 4000 =
// 64,000 不触发；28 列可写列最坏界 65535/28 ≈ 2340 行即超限——压测实证生产
// 批次 19 列 × ~3448 行即超限报错；统一计费模型重构删 6 加 2 后 28 列——
// 2000 行/批仍安全，见 usageLogBatchSize 注释）。
// 功能调用分量（统一计费模型 spec 2026-08-13）也全填：CallCount/PricePerCall
// Millis（毫分/单元——原图片 6 列已删，image token 并入 in/out）——双路径
// 等价测试（TestPGDeductCopyPathEquivalent）逐字段对比即覆盖新列。
func fullLogFor(userID int64, requestID string) *domain.UsageLog {
	l := logFor(userID, requestID)
	l.GroupID = 1
	l.AccountID = 2
	l.TemplateID = 3
	l.KeyID = 4
	l.MappedModel = "gpt-4o-mapped"
	l.CacheReadTokens = 1
	l.CacheCreationTokens = 2
	l.CallCount = 2
	l.PricePerCallMillis = int64Ptr(5_400) // 毫分/单元（例外单位——per-call 不走 /1e6）
	// RawCost（spec 2026-08-18）：乘倍率前原始成本——有值即两路径（COPY/ent
	// CreateBulk）都必须落库（TestPGDeductCopyPathEquivalent 逐字段对比）。
	l.RawCost = 7_700
	// S-E（2026-08-17）：client_ip 有值——双路径（COPY/ent CreateBulk）等价性
	// 测试逐字段对比，有值即两路径都必须落库（TestPGDeductCopyPathEquivalent）。
	l.ClientIP = "9.9.9.9"
	msg := "err:" + requestID
	l.ErrorMessage = &msg
	return l
}

// seedTempBalance 直插临时额度行（返回行 id，断言扣减用）。
func seedTempBalance(t *testing.T, repos *repository.Repository, userID, amount int64, expiresAt *time.Time) int64 {
	t.Helper()
	row, err := repos.Client.TempBalance.Create().
		SetUserID(userID).SetAmount(amount).SetNillableExpiresAt(expiresAt).
		Save(context.Background())
	require.NoError(t, err)
	return row.ID
}

func tempBalanceAmount(t *testing.T, repos *repository.Repository, id int64) int64 {
	t.Helper()
	row, err := repos.Client.TempBalance.Get(context.Background(), id)
	require.NoError(t, err)
	return row.Amount
}

// countLogs 统计用户日志数（契约去 Total 后查询面不再返回 count——
// 测试直接走 ent 客户端计数，不引入生产 Count 路径）。
func countLogs(t *testing.T, repos *repository.Repository, userID int64) int64 {
	t.Helper()
	n, err := repos.Client.UsageLog.Query().Where(usagelog.UserIDEQ(userID)).Count(context.Background())
	require.NoError(t, err)
	return int64(n)
}

// TestPGDeductFEFOOrder FEFO 扣临时额度：最早到期先扣、永久最后、已过期不参与；
// 临时额度充足时余额不被触碰。
func TestPGDeductFEFOOrder(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "fefo@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 100000))

	exp1 := time.Now().Add(time.Hour).Truncate(time.Second)
	exp2 := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	expired := time.Now().Add(-time.Hour).Truncate(time.Second)
	t1 := seedTempBalance(t, repos, u.ID, 30000, &exp1)    // 最早到期
	t2 := seedTempBalance(t, repos, u.ID, 50000, &exp2)    // 次到期
	tp := seedTempBalance(t, repos, u.ID, 70000, nil)      // 永久最后
	te := seedTempBalance(t, repos, u.ID, 90000, &expired) // 已过期不参与

	od, bal, err := repos.DeductAndLog(ctx, u.ID, 40000, []*domain.UsageLog{logFor(u.ID, "r1")})
	require.NoError(t, err)
	require.False(t, od, "临时额度充足不透支")
	require.Equal(t, int64(100000), bal, "余额未被触碰")
	require.Equal(t, int64(0), tempBalanceAmount(t, repos, t1), "最早到期先扣完")
	require.Equal(t, int64(40000), tempBalanceAmount(t, repos, t2), "次到期补足剩余 10000")
	require.Equal(t, int64(70000), tempBalanceAmount(t, repos, tp), "永久额度不动")
	require.Equal(t, int64(90000), tempBalanceAmount(t, repos, te), "已过期不参与扣减")
	require.Equal(t, int64(1), countLogs(t, repos, u.ID))
}

// TestPGDeductPermanentLast FEFO 全档耗尽后扣到永久额度（永久最后）。
func TestPGDeductPermanentLast(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "perm@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 100000))

	exp1 := time.Now().Add(time.Hour).Truncate(time.Second)
	t1 := seedTempBalance(t, repos, u.ID, 30000, &exp1)
	tp := seedTempBalance(t, repos, u.ID, 70000, nil)

	od, bal, err := repos.DeductAndLog(ctx, u.ID, 100000, []*domain.UsageLog{logFor(u.ID, "r1")})
	require.NoError(t, err)
	require.False(t, od)
	require.Equal(t, int64(100000), bal, "临时额度 100000 恰好覆盖，余额不动")
	require.Equal(t, int64(0), tempBalanceAmount(t, repos, t1))
	require.Equal(t, int64(0), tempBalanceAmount(t, repos, tp), "永久额度最后扣 70000")
}

// TestPGDeductTempPartialAndBalance 临时额度部分扣 + 余额补足。
func TestPGDeductTempPartialAndBalance(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "partial@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 100000))

	tp := seedTempBalance(t, repos, u.ID, 150000, nil) // 150000 毫分临时额度

	od, bal, err := repos.DeductAndLog(ctx, u.ID, 200000, []*domain.UsageLog{logFor(u.ID, "r1")})
	require.NoError(t, err)
	require.False(t, od, "余额充足不透支")
	require.Equal(t, int64(50000), bal, "临时扣 150000 + 余额补 50000")
	require.Equal(t, int64(0), tempBalanceAmount(t, repos, tp))
}

// TestPGDeductConditionalSuccess 无临时额度：余额充足走条件扣成功（不透支）。
func TestPGDeductConditionalSuccess(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "cond@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 100000))

	od, bal, err := repos.DeductAndLog(ctx, u.ID, 40000, []*domain.UsageLog{logFor(u.ID, "r1")})
	require.NoError(t, err)
	require.False(t, od)
	require.Equal(t, int64(60000), bal)
}

// TestPGDeductOverdraft 余额不足 → 无条件扣允许透支（负余额），日志标记 Overdraft。
func TestPGDeductOverdraft(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "od@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 10000))

	od, bal, err := repos.DeductAndLog(ctx, u.ID, 40000, []*domain.UsageLog{logFor(u.ID, "r1")})
	require.NoError(t, err)
	require.True(t, od, "允许透支")
	require.Equal(t, int64(-30000), bal, "透支后负余额")
	require.Equal(t, int64(1), countLogs(t, repos, u.ID))
	rows, err := repos.Usages.QueryUsages(context.Background(), repository.UsageQuery{UserID: u.ID, Limit: 10})
	require.NoError(t, err)
	require.True(t, rows[0].Overdraft, "日志 Overdraft 标记")
}

// TestPGDeductConcurrentSameUser 并发抢同一用户：行锁串行——一个条件扣成功，
// 一个透支（100000 - 120000 = -20000），两笔日志都在（多实例安全语义）。
func TestPGDeductConcurrentSameUser(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "conc@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 100000))

	type result struct {
		od  bool
		err error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			od, _, err := repos.DeductAndLog(ctx, u.ID, 60000, []*domain.UsageLog{logFor(u.ID, fmt.Sprintf("c%d", n))})
			results <- result{od: od, err: err}
		}(i)
	}
	wg.Wait()
	close(results)
	odCount := 0
	for r := range results {
		require.NoError(t, r.err)
		if r.od {
			odCount++
		}
	}
	require.Equal(t, 1, odCount, "并发抢同一用户：一成一透")
	got, err := repos.GetUser(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, int64(-20000), got.Balance, "100000 - 2×60000")
	require.Equal(t, int64(2), countLogs(t, repos, u.ID))
}

// TestPGDeductRollbackOnFailure 同事务回滚：日志插入失败（非法 format 枚举）→
// 扣费与日志全部回滚（余额/temp/日志均不变）。
func TestPGDeductRollbackOnFailure(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "rollback@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 50000))
	tp := seedTempBalance(t, repos, u.ID, 30000, nil)

	bad := logFor(u.ID, "bad")
	bad.Format = domain.RequestFormat("bogus") // 非法枚举 → 插入报错
	_, _, err := repos.DeductAndLog(ctx, u.ID, 40000, []*domain.UsageLog{bad})
	require.Error(t, err, "日志插入失败必须整体回滚")
	got, err := repos.GetUser(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, int64(50000), got.Balance, "余额回滚")
	require.Equal(t, int64(30000), tempBalanceAmount(t, repos, tp), "临时额度回滚")
	require.Zero(t, countLogs(t, repos, u.ID), "日志回滚（0 行）")
}

// TestPGDeductZeroCostOnlyLogs cost=0 → 只插日志：不扣款，overdrafted=false，
// balanceAfter = 当前余额原值。
func TestPGDeductZeroCostOnlyLogs(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "zerocost@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 50000))
	tp := seedTempBalance(t, repos, u.ID, 30000, nil)

	od, bal, err := repos.DeductAndLog(ctx, u.ID, 0, []*domain.UsageLog{
		logFor(u.ID, "z1"), logFor(u.ID, "z2"),
	})
	require.NoError(t, err)
	require.False(t, od)
	require.Equal(t, int64(50000), bal, "balanceAfter = 当前余额原值")
	require.Equal(t, int64(30000), tempBalanceAmount(t, repos, tp), "临时额度不动")
	require.Equal(t, int64(2), countLogs(t, repos, u.ID))
	rows, err := repos.Usages.QueryUsages(context.Background(), repository.UsageQuery{UserID: u.ID, Limit: 10})
	require.NoError(t, err)
	require.False(t, rows[0].Overdraft, "cost=0 恒不透支")
}

// TestPGDeductUserMissing 用户不存在 → 跳过扣减仍插日志（usagelog 无 FK）；
// balanceAfter 0、不透支、无错误。
func TestPGDeductUserMissing(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	od, bal, err := repos.DeductAndLog(ctx, 999999, 50000, []*domain.UsageLog{logFor(999999, "ghost")})
	require.NoError(t, err, "用户不存在不报错（跳过扣减仍插日志）")
	require.False(t, od)
	require.Zero(t, bal, "balanceAfter 0")
	require.Equal(t, int64(1), countLogs(t, repos, 999999), "日志照常插入")
}

// --- #37 P2：CreateBulk 参数上限分片（PG 65535 参数） ---

// TestPGDeductLargeBatchSuccess 单 user 4000 行全列日志（21 列 × 4000 =
// 84,000 参数）扣费成功：修复前单批 CreateBulk 超 PG 65535 参数上限 →
// "extended protocol limited to 65535 parameters" → 扣费停滞 pending 积压
// （压测实证）；分片 2000 行/批同事务逐片插入后全量成功，扣费精确。
func TestPGDeductLargeBatchSuccess(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "big@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 1_000_000))

	logs := make([]*domain.UsageLog, 0, 4000)
	for i := 0; i < 4000; i++ {
		logs = append(logs, fullLogFor(u.ID, fmt.Sprintf("big-%d", i)))
	}
	od, bal, err := repos.DeductAndLog(ctx, u.ID, 520_000, logs)
	require.NoError(t, err)
	require.False(t, od)
	require.Equal(t, int64(480_000), bal, "扣减 520,000（4000 × 130 毫分）")
	require.Equal(t, int64(4000), countLogs(t, repos, u.ID), "4000 行日志全量落库")
}

// TestPGDeductLargeBatchRollback 分片跨片回滚：毒丸行（非法 format 枚举）置于
// 第 8 片（index 3500，前 7 片已插入）→ 任一失败整体回滚——余额/临时额度/日志
// 全部不变（分片不改变"全成或全败"事务语义）。
func TestPGDeductLargeBatchRollback(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "bigroll@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 50000))
	tp := seedTempBalance(t, repos, u.ID, 30000, nil)

	logs := make([]*domain.UsageLog, 0, 4000)
	for i := 0; i < 4000; i++ {
		l := fullLogFor(u.ID, fmt.Sprintf("bigr-%d", i))
		if i == 3500 { // 第 8 片（3500..3999）内的毒丸行：前 7 片已插入后本片失败
			l.Format = domain.RequestFormat("bogus") // 非法枚举 → 插入报错
		}
		logs = append(logs, l)
	}
	_, _, err := repos.DeductAndLog(ctx, u.ID, 40000, logs)
	require.Error(t, err, "任一片失败必须整体回滚")
	got, err := repos.GetUser(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, int64(50000), got.Balance, "余额回滚")
	require.Equal(t, int64(30000), tempBalanceAmount(t, repos, tp), "临时额度回滚")
	require.Zero(t, countLogs(t, repos, u.ID), "日志全量回滚（0 行）")
}

// TestPGDeductLargeBatchZeroCost cost=0 大批量只插日志（4000 行）：不扣款、
// 不透支、balanceAfter 原值——只插日志路径同样分片。
func TestPGDeductLargeBatchZeroCost(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "bigzero@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 50000))

	logs := make([]*domain.UsageLog, 0, 4000)
	for i := 0; i < 4000; i++ {
		logs = append(logs, fullLogFor(u.ID, fmt.Sprintf("bigz-%d", i)))
	}
	od, bal, err := repos.DeductAndLog(ctx, u.ID, 0, logs)
	require.NoError(t, err)
	require.False(t, od)
	require.Equal(t, int64(50000), bal, "cost=0 不扣款")
	require.Equal(t, int64(4000), countLogs(t, repos, u.ID), "4000 行日志全量落库")
}
