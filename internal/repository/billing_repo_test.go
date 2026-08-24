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
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/usagelog"
	"github.com/is7qin/c3api/internal/repository"
)

// 计费游标消费面直调测试（三车道拓扑，spec-f2opt-settlement §三改写）：usage_logs
// 明细由 usage flusher 单写落库（InsertBatch，billed=false 出生），本文件只测消费
// 侧结算语句——FetchUnbilledBatch 取批过滤 / SettleBalanceBatch（余额-only 用户，
// 条件扣→透支补刀→标记一体）/ SettleFefoBatch（temp-active 用户，集合化 FEFO +
// spill 补差）/ MarkBilledBulk 幂等纯标记 / UnbilledLag 度量。legacy 逐组扣减面
// （pg_deduct_* 单组事务族）已随 D8 整体退役——FEFO/透支/条件扣语义改写为语句级
// 等价断言。

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

// costLogFor 指定 cost 的计费种子行（结算语句按行 cost 扣减——显式成本对齐
// 断言口径；legacy 逐组扣减面的显式 cost 实参语义由此承接）。
func costLogFor(userID int64, requestID string, cost int64) *domain.UsageLog {
	l := logFor(userID, requestID)
	l.Cost = cost
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
// （计费消费面的唯一入账形态）。
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
// ('none','abort') 两谓词（单取批面：零价行同批取出由消费侧路由）+ ORDER BY id
// + LIMIT；LedgerRow 瘦身投影字段逐项断言。
func TestPGFetchUnbilledBatchFilters(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	a := logFor(1, "f-a") // none + cost>0 → 取
	b := logFor(1, "f-b") // abort + cost>0 → 取
	b.ErrorType = domain.ErrAbort
	c := logFor(1, "f-c") // cost=0 → 取（单取批面）
	c.Cost = 0
	d := logFor(1, "f-d") // born-absorbed（billed=true）→ 不取
	d.Billed = true
	seedUnbilled(t, repos, a)
	seedUnbilled(t, repos, b)
	seedUnbilled(t, repos, c)
	seedUnbilled(t, repos, d)

	rows := fetchAllUnbilled(t, repos)
	require.Len(t, rows, 3, "none/abort 且未标记行进取批——零价行同批取出")
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

// usageLogByID 按 id 回读 ent 行（billed/overdraft 列值断言面）。
func usageLogByID(t *testing.T, repos *repository.Repository, id int64) *ent.UsageLog {
	t.Helper()
	row, err := repos.Client.UsageLog.Get(context.Background(), id)
	require.NoError(t, err)
	return row
}

// TestSettleFefoOrder FEFO 扣临时额度（D7 集合化语义）：最早到期先扣、永久最后、
// 已过期不参与；临时额度充足时余额不被触碰（spill=0 → 无资金移动、无余额对）；
// 同语句 billed 翻转。
func TestSettleFefoOrder(t *testing.T) {
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

	seedUnbilled(t, repos, costLogFor(u.ID, "r1", 40000))
	rows := fetchAllUnbilled(t, repos)
	require.Len(t, rows, 1)

	res, err := repos.SettleFefoBatch(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), res.BatchRows)
	require.Equal(t, int64(1), res.Marked, "扣减与标记同语句原子")
	require.Equal(t, int64(0), res.DebitedUsers, "临时额度充足不触余额")
	require.Equal(t, int64(0), res.ForcedUsers)
	require.Equal(t, int64(0), res.Quarantined)
	require.Empty(t, res.Balances, "spill=0 无资金移动——无定向余额对")

	require.Equal(t, int64(0), tempBalanceAmount(t, repos, t1), "最早到期先扣完")
	require.Equal(t, int64(40000), tempBalanceAmount(t, repos, t2), "次到期边界部分扣 10000")
	require.Equal(t, int64(70000), tempBalanceAmount(t, repos, tp), "永久额度不动")
	require.Equal(t, int64(90000), tempBalanceAmount(t, repos, te), "已过期不参与扣减")

	got, err := repos.GetUser(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, int64(100000), got.Balance, "余额未被触碰")

	row := usageLogByID(t, repos, rows[0].ID)
	require.True(t, row.Billed)
	require.False(t, row.Overdraft, "无透支")

	_, lagN, err := repos.UnbilledLag(ctx)
	require.NoError(t, err)
	require.Zero(t, lagN, "行退出游标")
}

// TestSettleFefoPermanentLast FEFO 全档耗尽后扣到永久额度（NULLS LAST 排序语义）。
func TestSettleFefoPermanentLast(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "perm@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 100000))

	exp1 := time.Now().Add(time.Hour).Truncate(time.Second)
	t1 := seedTempBalance(t, repos, u.ID, 30000, &exp1)
	tp := seedTempBalance(t, repos, u.ID, 70000, nil)

	seedUnbilled(t, repos, costLogFor(u.ID, "r1", 100000))

	res, err := repos.SettleFefoBatch(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), res.Marked)
	require.Empty(t, res.Balances, "临时额度恰好覆盖，余额不动")
	require.Equal(t, int64(0), tempBalanceAmount(t, repos, t1))
	require.Equal(t, int64(0), tempBalanceAmount(t, repos, tp), "永久额度最后扣 70000")
}

// TestSettleFefoPartialLastRowAndSpill 覆盖点边界两态（D7 部分覆盖语义）：
// 场景 A——覆盖点落在末行中间（部分扣，剩余保留）；场景 B——临时额度不足，
// spill 差额进余额条件扣（守恒精确：Σdrawn + Δbalance == cost）。
func TestSettleFefoPartialLastRowAndSpill(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	// 场景 A：120000 成本 vs 30000+50000+70000 池——覆盖点在永久行内（部分扣 40000）
	uA := seedPGUser(t, repos, "partial-a@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, uA.ID, 100000))
	a1 := seedTempBalance(t, repos, uA.ID, 30000, ptrTime(time.Now().Add(time.Hour)))
	a2 := seedTempBalance(t, repos, uA.ID, 50000, ptrTime(time.Now().Add(24*time.Hour)))
	ap := seedTempBalance(t, repos, uA.ID, 70000, nil)
	logA := logFor(uA.ID, "pa")
	logA.Cost = 120000
	seedUnbilled(t, repos, logA)

	resA, err := repos.SettleFefoBatch(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), resA.Marked)
	require.Empty(t, resA.Balances, "场景 A 全额被 temp 覆盖")
	require.Equal(t, int64(0), tempBalanceAmount(t, repos, a1))
	require.Equal(t, int64(0), tempBalanceAmount(t, repos, a2))
	require.Equal(t, int64(30000), tempBalanceAmount(t, repos, ap),
		"覆盖点部分扣：70000 只消耗 40000")

	// 场景 B：180000 成本 vs 150000 池——全池耗尽 + spill 30000 进余额条件扣
	uB := seedPGUser(t, repos, "partial-b@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, uB.ID, 100000))
	b1 := seedTempBalance(t, repos, uB.ID, 30000, ptrTime(time.Now().Add(time.Hour)))
	b2 := seedTempBalance(t, repos, uB.ID, 50000, ptrTime(time.Now().Add(24*time.Hour)))
	bp := seedTempBalance(t, repos, uB.ID, 70000, nil)
	logB := logFor(uB.ID, "pb")
	logB.Cost = 180000
	seedUnbilled(t, repos, logB)

	resB, err := repos.SettleFefoBatch(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), resB.Marked)
	require.Equal(t, int64(1), resB.DebitedUsers, "spill 走余额条件扣")
	require.Equal(t, int64(0), resB.ForcedUsers)
	require.Len(t, resB.Balances, 1)
	require.Equal(t, uB.ID, resB.Balances[0].UserID)
	require.Equal(t, int64(70000), resB.Balances[0].Balance,
		"守恒精确：drawn 150000 + Δ30000 == cost 180000")
	require.Equal(t, int64(0), tempBalanceAmount(t, repos, b1))
	require.Equal(t, int64(0), tempBalanceAmount(t, repos, b2))
	require.Equal(t, int64(0), tempBalanceAmount(t, repos, bp))

	got, err := repos.GetUser(ctx, uB.ID)
	require.NoError(t, err)
	require.Equal(t, int64(70000), got.Balance)
}

// TestSettleBalanceConditionalSuccess 无临时额度：余额充足走条件扣成功（不透支）；
// 车道互斥微断言——同一用户先经 SettleFefoBatch 零命中（非 temp-active 被谓词
// 排除），再经 SettleBalanceBatch 结算。
func TestSettleBalanceConditionalSuccess(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "cond@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 100000))

	seedUnbilled(t, repos, costLogFor(u.ID, "r1", 40000))
	rows := fetchAllUnbilled(t, repos)
	require.Len(t, rows, 1)

	fefoRes, err := repos.SettleFefoBatch(ctx, 10)
	require.NoError(t, err)
	require.Zero(t, fefoRes.BatchRows, "非 temp-active 用户被 fefo 车道谓词排除")
	require.Zero(t, fefoRes.Marked)

	res, err := repos.SettleBalanceBatch(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), res.BatchRows)
	require.Equal(t, int64(1), res.Marked)
	require.Equal(t, int64(1), res.DebitedUsers, "条件扣命中")
	require.Equal(t, int64(0), res.ForcedUsers, "不透支")
	require.Len(t, res.Balances, 1)
	require.Equal(t, u.ID, res.Balances[0].UserID)
	require.Equal(t, int64(60000), res.Balances[0].Balance, "(uid,balance_after) 定向对")

	got, err := repos.GetUser(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, int64(60000), got.Balance)
}

// TestSettleBalanceOverdraft 余额不足 → 无条件扣允许透支（负余额），overdraft
// 回写行内（B2）。
func TestSettleBalanceOverdraft(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "od@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 10000))

	seedUnbilled(t, repos, costLogFor(u.ID, "r1", 40000))
	rows := fetchAllUnbilled(t, repos)

	res, err := repos.SettleBalanceBatch(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), res.ForcedUsers, "透支补刀命中")
	require.Equal(t, int64(0), res.DebitedUsers)
	require.Len(t, res.Balances, 1)
	require.Equal(t, int64(-30000), res.Balances[0].Balance, "透支后负余额")

	out, err := repos.QueryUsages(context.Background(), repository.UsageQuery{UserID: u.ID, Limit: 10})
	require.NoError(t, err)
	require.True(t, out[0].Overdraft, "overdraft 回写 usage_logs 行内")
	require.True(t, usageLogByID(t, repos, rows[0].ID).Billed)
}

// TestSettleBalanceMixedOverdraft 混合透支批（替代退役 chunk 族）：od=true/false
// 用户同批一笔语句——逐用户 Δ余额精确、overdraft 列按用户整组对齐、守恒闭合。
func TestSettleBalanceMixedOverdraft(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	rich := seedPGUser(t, repos, "mixed-rich@example.com") // 余额充足：条件扣
	poor := seedPGUser(t, repos, "mixed-poor@example.com") // 余额不足：透支
	require.NoError(t, repos.UpdateUserBalance(ctx, rich.ID, 500_000))
	require.NoError(t, repos.UpdateUserBalance(ctx, poor.ID, 30_000))

	seedUnbilled(t, repos, costLogFor(rich.ID, "mr-1", 100_000))
	seedUnbilled(t, repos, costLogFor(rich.ID, "mr-2", 100_000))
	seedUnbilled(t, repos, costLogFor(poor.ID, "mp-1", 40_000))
	seedUnbilled(t, repos, costLogFor(poor.ID, "mp-2", 40_000))
	all := fetchAllUnbilled(t, repos)
	require.Len(t, all, 4)

	res, err := repos.SettleBalanceBatch(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, int64(4), res.BatchRows)
	require.Equal(t, int64(4), res.Marked, "整批一笔语句全标记")
	require.Equal(t, int64(1), res.DebitedUsers)
	require.Equal(t, int64(1), res.ForcedUsers)
	require.Len(t, res.Balances, 2)
	byUID := map[int64]int64{}
	for _, p := range res.Balances {
		byUID[p.UserID] = p.Balance
	}
	require.Equal(t, int64(300_000), byUID[rich.ID], "rich 500000−200000 条件扣")
	require.Equal(t, int64(-50_000), byUID[poor.ID], "poor 30000−80000 透支")

	// 守恒：Σ|Δ| == Σcost（280000 = 200000 + 80000）
	require.Equal(t, int64(280_000),
		(500_000-byUID[rich.ID])+(byUID[poor.ID]-30_000)*-1)

	// overdraft 列逐用户整组对齐
	for _, r := range all {
		row := usageLogByID(t, repos, r.ID)
		require.True(t, row.Billed)
		if r.UserID == rich.ID {
			require.False(t, row.Overdraft, "rich 行 od=false")
		} else {
			require.True(t, row.Overdraft, "poor 行 od=true 整组回写")
		}
	}
	require.Zero(t, cursorUnbilledCount(t, repos))
}

// TestSettleBalanceGhostQuarantined 用户不存在 → 跳过扣减仍标记全部行、
// Quarantined 行数返回（不变量 #1 尾语义——毒用户不卡游标）。
func TestSettleBalanceGhostQuarantined(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	seedUnbilled(t, repos, logFor(999999, "ghost"))
	seedUnbilled(t, repos, logFor(999999, "ghost2"))
	rows := fetchAllUnbilled(t, repos)
	require.Len(t, rows, 2)

	res, err := repos.SettleBalanceBatch(ctx, 10)
	require.NoError(t, err, "用户缺失不报错（跳过扣减仍标记）")
	require.Equal(t, int64(2), res.Quarantined, "幽灵行计数")
	require.Equal(t, int64(2), res.Marked)
	require.Empty(t, res.Balances, "幽灵用户无余额对")
	require.Equal(t, int64(0), res.DebitedUsers)
	require.Equal(t, int64(0), res.ForcedUsers)

	for _, r := range rows {
		row := usageLogByID(t, repos, r.ID)
		require.True(t, row.Billed, "全部 ids 标记退出游标")
		require.False(t, row.Overdraft, "隔离路径 od 出生 false 保持")
	}
	_, lagN, err := repos.UnbilledLag(ctx)
	require.NoError(t, err)
	require.Zero(t, lagN)
}

// TestSettleBalanceConcurrentSameUser 并发抢同一用户：行锁串行 + 并发标记守卫
// ——恰一人整批成交（另一人空批或守卫回滚重放至空批），终态守恒精确
// （100000 − 2×60000 = −20000），两行都标记恰一次（多实例安全语义）。
func TestSettleBalanceConcurrentSameUser(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "conc@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 100000))

	seedUnbilled(t, repos, costLogFor(u.ID, "c0", 60000))
	seedUnbilled(t, repos, costLogFor(u.ID, "c1", 60000))
	all := fetchAllUnbilled(t, repos)
	require.Len(t, all, 2)

	type result struct {
		res domain.SettlementSummary
		err error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := repos.SettleBalanceBatch(ctx, 10)
			results <- result{res: res, err: err}
		}()
	}
	wg.Wait()
	close(results)
	for r := range results {
		require.NoError(t, r.err, "并发标记守卫回滚后重放收敛——终态无错误")
	}
	got, err := repos.GetUser(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, int64(-20000), got.Balance, "100000 - 2×60000 恰扣一次")
	require.Zero(t, cursorUnbilledCount(t, repos), "两行各自原子标记退出游标")
	for _, r := range all {
		require.True(t, usageLogByID(t, repos, r.ID).Billed)
	}
}

// TestPGZeroCostFastPath 零价快速路径：cost=0 行进取批（单取批面）→
// MarkBilledBulk 幂等纯标记——不触碰余额/temp（不走结算语句），零资金移动。
func TestPGZeroCostFastPath(t *testing.T) {
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
	require.Len(t, fetched, 2, "cost=0 行进取批（单取批面）")
	ids := ledgerIDsOf(fetched)

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

// ledgerIDsOf LedgerRow 批 → id 序列（纯标记面实参）。
func ledgerIDsOf(rows []domain.LedgerRow) []int64 {
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
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
	require.NoError(t, repos.MarkBilledBulk(ctx, ledgerIDsOf(rows)))
	oldest, n, err = repos.UnbilledLag(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	require.True(t, oldest.IsZero(), "清空后 oldest 归零")
}
