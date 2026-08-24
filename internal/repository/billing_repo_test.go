// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent/usagelog"
	"github.com/is7qin/c3api/internal/repository"
)

// 计费游标消费面直调测试（F2 ledger-cursor，spec 2026-08-23 §四）：usage_logs
// 明细由 usage flusher 单写落库（InsertBatch，billed=false 出生），本文件只测
// 消费侧——FetchUnbilledBatch 取批过滤 / DeductOnlyAndMark FEFO 扣减 + billed
// 标记原子 / MarkBilledBulk 幂等纯标记 / UnbilledLag 度量。

// logFor 构造测试计费日志（usage flusher InsertBatch 种子行；多文件共用）。
func logFor(userID int64, requestID string) *domain.UsageLog {
	return &domain.UsageLog{
		RequestID: requestID, UserID: userID, Model: "gpt-4o",
		Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone,
		LatencyMS: 10, InputTokens: 3, OutputTokens: 5, TotalTokens: 8,
		CallCount: 1, Cost: 130, BillingTier: "auto",
		CreatedAt: time.Now(),
	}
}

// fullLogFor 填满全部可选列的计费日志（列集合锚定回归锚：COPY 列清单/
// buildUsageLogCreate/分区表列定义三面同步——任一漏列本 fixture 即红）。
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
	l.RawCost = 7_700
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

// seedUnbilled usage flusher 单写点种子：InsertBatch 落库 billed=false 出生行
// （F2 后计费消费面的唯一入账形态）。
func seedUnbilled(t *testing.T, repos *repository.Repository, l *domain.UsageLog) {
	t.Helper()
	require.NoError(t, repos.Usages.InsertBatch(context.Background(), []*domain.UsageLog{l}))
}

// fetchAllUnbilled 取全量未批批（测试 schema 恒小，limit 放大即可）。
func fetchAllUnbilled(t *testing.T, repos *repository.Repository) []domain.LedgerRow {
	t.Helper()
	rows, err := repos.FetchUnbilledBatch(context.Background(), 10000)
	require.NoError(t, err)
	return rows
}

// TestPGFetchUnbilledBatchFilters 取批过滤语义：NOT billed + error_type IN
// ('none','abort') 两谓词（F2-opt D1：cost > 0 谓词删除——零价行同批取出由消
// 费侧内存路由）+ ORDER BY id + LIMIT；LedgerRow 瘦身投影字段逐项断言。
func TestPGFetchUnbilledBatchFilters(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	a := logFor(1, "f-a") // none + cost>0 → 取
	b := logFor(1, "f-b") // abort + cost>0 → 取
	b.ErrorType = domain.ErrAbort
	c := logFor(1, "f-c") // cost=0 → 取（D1 单取批面）
	c.Cost = 0
	d := logFor(1, "f-d") // born-absorbed（billed=true）→ 不取
	d.Billed = true
	seedUnbilled(t, repos, a)
	seedUnbilled(t, repos, b)
	seedUnbilled(t, repos, c)
	seedUnbilled(t, repos, d)

	rows := fetchAllUnbilled(t, repos)
	require.Len(t, rows, 3, "none/abort 且未标记行进取批——零价行不再被谓词排除")
	require.Less(t, rows[0].ID, rows[1].ID, "ORDER BY id 单调推进游标")
	first := rows[0]
	require.Equal(t, int64(1), first.UserID)
	require.Equal(t, int64(130), first.Cost)
	require.Equal(t, "gpt-4o", first.Model)
	require.Equal(t, "auto", first.BillingTier)
	require.Equal(t, int64(1), first.CallCount)
	require.Equal(t, "openai-chat", first.Format)

	limited, err := repos.FetchUnbilledBatch(ctx, 1)
	require.NoError(t, err)
	require.Len(t, limited, 1, "LIMIT 生效")
	require.Equal(t, rows[0].ID, limited[0].ID, "取批按 id 升序截断")

	require.Equal(t, c.RequestID, requestIDOf(t, repos, rows[2].ID),
		"cost=0 行同批取出（零价路由归消费侧内存判断）")
}

// requestIDOf 按 id 反查 request_id（零价取数半归属断言辅助）。
func requestIDOf(t *testing.T, repos *repository.Repository, id int64) string {
	t.Helper()
	row, err := repos.Client.UsageLog.Get(context.Background(), id)
	require.NoError(t, err)
	return row.RequestID
}

// TestPGDeductOnlyAndMarkFEFOOrder FEFO 扣临时额度：最早到期先扣、永久最后、
// 已过期不参与；临时额度充足时余额不被触碰；同事务 billed 翻转。
func TestPGDeductOnlyAndMarkFEFOOrder(t *testing.T) {
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

	l := logFor(u.ID, "r1")
	seedUnbilled(t, repos, l)
	rows := fetchAllUnbilled(t, repos)
	require.Len(t, rows, 1)

	bal, od, quarantined, err := repos.DeductOnlyAndMark(ctx, u.ID, 40000, ledgerRowIDs(rows))
	require.NoError(t, err)
	require.False(t, od, "临时额度充足不透支")
	require.False(t, quarantined)
	require.Equal(t, int64(100000), bal, "余额未被触碰")
	require.Equal(t, int64(0), tempBalanceAmount(t, repos, t1), "最早到期先扣完")
	require.Equal(t, int64(40000), tempBalanceAmount(t, repos, t2), "次到期补足剩余 10000")
	require.Equal(t, int64(70000), tempBalanceAmount(t, repos, tp), "永久额度不动")
	require.Equal(t, int64(90000), tempBalanceAmount(t, repos, te), "已过期不参与扣减")

	_, lagN, err := repos.UnbilledLag(ctx)
	require.NoError(t, err)
	require.Zero(t, lagN, "扣减与标记同事务原子——行退出游标")
}

// ledgerRowIDs LedgerRow 批 → id 实参（DeductOnlyAndMark 标记面）。
func ledgerRowIDs(rows []domain.LedgerRow) []int64 {
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}

// TestPGDeductOnlyAndMarkPermanentLast FEFO 全档耗尽后扣到永久额度（永久最后）。
func TestPGDeductOnlyAndMarkPermanentLast(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "perm@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 100000))

	exp1 := time.Now().Add(time.Hour).Truncate(time.Second)
	t1 := seedTempBalance(t, repos, u.ID, 30000, &exp1)
	tp := seedTempBalance(t, repos, u.ID, 70000, nil)

	seedUnbilled(t, repos, logFor(u.ID, "r1"))
	rows := fetchAllUnbilled(t, repos)

	bal, od, _, err := repos.DeductOnlyAndMark(ctx, u.ID, 100000, ledgerRowIDs(rows))
	require.NoError(t, err)
	require.False(t, od)
	require.Equal(t, int64(100000), bal, "临时额度 100000 恰好覆盖，余额不动")
	require.Equal(t, int64(0), tempBalanceAmount(t, repos, t1))
	require.Equal(t, int64(0), tempBalanceAmount(t, repos, tp), "永久额度最后扣 70000")
}

// TestPGDeductOnlyAndMarkTempPartialAndBalance 临时额度部分扣 + 余额补足。
func TestPGDeductOnlyAndMarkTempPartialAndBalance(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "partial@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 100000))
	tp := seedTempBalance(t, repos, u.ID, 150000, nil) // 150000 毫分临时额度

	seedUnbilled(t, repos, logFor(u.ID, "r1"))
	rows := fetchAllUnbilled(t, repos)

	bal, od, _, err := repos.DeductOnlyAndMark(ctx, u.ID, 200000, ledgerRowIDs(rows))
	require.NoError(t, err)
	require.False(t, od, "余额充足不透支")
	require.Equal(t, int64(50000), bal, "临时扣 150000 + 余额补 50000")
	require.Equal(t, int64(0), tempBalanceAmount(t, repos, tp))
}

// TestPGDeductOnlyAndMarkConditionalSuccess 无临时额度：余额充足走条件扣成功
// （不透支）。
func TestPGDeductOnlyAndMarkConditionalSuccess(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "cond@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 100000))

	seedUnbilled(t, repos, logFor(u.ID, "r1"))
	rows := fetchAllUnbilled(t, repos)

	bal, od, _, err := repos.DeductOnlyAndMark(ctx, u.ID, 40000, ledgerRowIDs(rows))
	require.NoError(t, err)
	require.False(t, od)
	require.Equal(t, int64(60000), bal)
}

// TestPGDeductOnlyAndMarkOverdraft 余额不足 → 无条件扣允许透支（负余额），
// overdraft 回写行内（B2）。
func TestPGDeductOnlyAndMarkOverdraft(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "od@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 10000))

	seedUnbilled(t, repos, logFor(u.ID, "r1"))
	rows := fetchAllUnbilled(t, repos)

	bal, od, _, err := repos.DeductOnlyAndMark(ctx, u.ID, 40000, ledgerRowIDs(rows))
	require.NoError(t, err)
	require.True(t, od, "允许透支")
	require.Equal(t, int64(-30000), bal, "透支后负余额")

	out, err := repos.QueryUsages(context.Background(), repository.UsageQuery{UserID: u.ID, Limit: 10})
	require.NoError(t, err)
	require.True(t, out[0].Overdraft, "overdraft 回写 usage_logs 行内")
}

// TestPGDeductOnlyAndMarkConcurrentSameUser 并发抢同一用户：行锁串行——一个
// 条件扣成功，一个透支（100000 - 120000 = -20000），两组 ids 都被标记（多实例
// 安全语义）。
func TestPGDeductOnlyAndMarkConcurrentSameUser(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "conc@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 100000))

	seedUnbilled(t, repos, logFor(u.ID, "c0"))
	seedUnbilled(t, repos, logFor(u.ID, "c1"))
	all := fetchAllUnbilled(t, repos)
	require.Len(t, all, 2)

	type result struct {
		od          bool
		quarantined bool
		err         error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, od, q, err := repos.DeductOnlyAndMark(ctx, u.ID, 60000, []int64{all[n].ID})
			results <- result{od: od, quarantined: q, err: err} // balanceAfter 并发序不定，不断言
		}(i)
	}
	wg.Wait()
	close(results)
	odCount := 0
	for r := range results {
		require.NoError(t, r.err)
		require.False(t, r.quarantined)
		if r.od {
			odCount++
		}
	}
	require.Equal(t, 1, odCount, "并发抢同一用户：一成一透")
	got, err := repos.GetUser(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, int64(-20000), got.Balance, "100000 - 2×60000")
	_, lagN, err := repos.UnbilledLag(ctx)
	require.NoError(t, err)
	require.Zero(t, lagN, "两笔事务各自原子标记")
}

// TestPGDeductOnlyAndMarkUserMissingQuarantined 用户不存在 → 跳过扣减仍标记
// 全部 ids、quarantined=true 返回（不变量 #1 尾语义——毒用户不卡游标）。
func TestPGDeductOnlyAndMarkUserMissingQuarantined(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	ghost := logFor(999999, "ghost")
	seedUnbilled(t, repos, ghost)
	seedUnbilled(t, repos, logFor(999999, "ghost2"))
	rows := fetchAllUnbilled(t, repos)
	require.Len(t, rows, 2)

	bal, od, quarantined, err := repos.DeductOnlyAndMark(ctx, 999999, 50000, ledgerRowIDs(rows))
	require.NoError(t, err, "用户缺失不报错（跳过扣减仍标记）")
	require.False(t, od)
	require.True(t, quarantined, "quarantined 出口")
	require.Zero(t, bal, "balanceAfter 0")
	_, lagN, err := repos.UnbilledLag(ctx)
	require.NoError(t, err)
	require.Zero(t, lagN, "全部 ids 标记退出游标")
}

// TestPGZeroCostFastMarkPath 零价快速路径（m4 CostZeroFastMark，F2-opt D1 后
// 形态）：cost=0 行进取批（单取批面）→ MarkBilledBulk 幂等纯标记——不触碰余额/
// temp（不走 FEFO 机器），零资金移动。
func TestPGZeroCostFastMarkPath(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "zerocost@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 50000))
	tp := seedTempBalance(t, repos, u.ID, 30000, nil)

	z1 := logFor(u.ID, "z1")
	z1.Cost = 0
	z2 := logFor(u.ID, "z2")
	z2.Cost = 0
	seedUnbilled(t, repos, z1)
	seedUnbilled(t, repos, z2)

	fetched := fetchAllUnbilled(t, repos)
	require.Len(t, fetched, 2, "cost=0 行进取批（D1 单取批面）")
	ids := ledgerRowIDs(fetched)

	require.NoError(t, repos.MarkBilledBulk(ctx, ids))
	require.NoError(t, repos.MarkBilledBulk(ctx, ids), "幂等：重复标记静默跳过")
	got, err := repos.GetUser(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, int64(50000), got.Balance, "纯标记不扣款")
	require.Equal(t, int64(30000), tempBalanceAmount(t, repos, tp), "临时额度不动")
	_, lagN, err := repos.UnbilledLag(ctx)
	require.NoError(t, err)
	require.Zero(t, lagN, "快速标记退出游标")
}

// TestPGUnbilledLag 游标积压度量：空游标零值；种子后 count + oldest 对齐；
// 全部标记后归零（lag 护栏数据源契约）。
func TestPGUnbilledLag(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	oldest, n, err := repos.UnbilledLag(ctx)
	require.NoError(t, err)
	require.Zero(t, n, "空游标")
	require.True(t, oldest.IsZero(), "空游标 oldest 零值")

	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	newer := time.Now().Truncate(time.Second)
	r1 := logFor(1, "lag-old")
	r1.CreatedAt = old
	r2 := logFor(1, "lag-new")
	r2.CreatedAt = newer
	seedUnbilled(t, repos, r1)
	seedUnbilled(t, repos, r2)

	oldest, n, err = repos.UnbilledLag(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), n)
	require.Equal(t, old.UTC(), oldest.UTC(), "最老 unbilled 行 created_at")

	rows := fetchAllUnbilled(t, repos)
	require.NoError(t, repos.MarkBilledBulk(ctx, ledgerRowIDs(rows)))
	oldest, n, err = repos.UnbilledLag(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	require.True(t, oldest.IsZero(), "清空后 oldest 归零")
}
