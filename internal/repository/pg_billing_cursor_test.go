// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// 计费游标消费者 PG 测试套件（F2 T4，spec-f2-ledger-cursor §四测试清单十一族）：
// 端到端行为级——usage flusher 单写点（InsertBatch，billed=false 出生）落种子行，
// 游标消费面（FetchUnbilledBatch → 按 userID 分组 → DeductOnlyAndMark 单事务
// FEFO 扣减 + billed 标记原子 / MarkBilledBulk 幂等纯标记 / UnbilledLag 度量 /
// AcquireBillingLock 会话锁）逐族验收。与 billing_repo_test.go（消费面直调单元）
// 互补：本文件每族走完整"落库→消费→对账收敛"链路。
//
// 基座约定同 pg_account_groups_test.go：TEST_DATABASE_URL 未设置 → t.Skip；
// newPGRepos 每测 DROP SCHEMA 重建；串行无 t.Parallel；testify 只 require。

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/usagelog"
	"github.com/is7qin/c3api/internal/repository"
)

// cursorSeedTime 种子行固定 created_at（惯例：time.Date 注入；分区路由安全由
// ensureCursorPartitions 保证——bootstrap 该日分区幂等补建）。
var cursorSeedTime = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

// cursorSeq request_id 全局唯一序号（(request_id, created_at) 幂等唯一索引防撞；
// 进程内串行测试共享递增即可）。
var cursorSeq atomic.Int64

// ensureCursorPartitions 固定种子日的 usage_logs 分区补建（幂等；newPGRepos 只
// bootstrap 今明两日，固定 2026-08-20 需显式确保——跨日插入无分区整体失败）。
func ensureCursorPartitions(t *testing.T, repos *repository.Repository) {
	t.Helper()
	require.NoError(t, repos.EnsureUsageLogPartitioned(context.Background(), cursorSeedTime))
}

// cursorLog 构造计费种子行（none + cost>0 可消费形态；created_at 固定日内错峰）。
func cursorLog(userID, cost int64) *domain.UsageLog {
	seq := cursorSeq.Add(1)
	return &domain.UsageLog{
		RequestID:    fmt.Sprintf("cur-%d-%d", userID, seq),
		UserID:       userID,
		Model:        "gpt-4o",
		Format:       domain.FormatOpenAIChat,
		ErrorType:    domain.ErrNone,
		LatencyMS:    10,
		InputTokens:  3,
		OutputTokens: 5,
		TotalTokens:  8,
		Cost:         cost,
		BillingTier:  "auto",
		CreatedAt:    cursorSeedTime.Add(time.Duration(seq%3600) * time.Second),
	}
}

// seedCursorRows usage flusher 单写点批量落库（InsertBatch 分块 ≤500 行/语句，
// 防 CreateBulk 参数上限）。落库行 id 由调用方经 FetchUnbilledBatch 取。
func seedCursorRows(t *testing.T, repos *repository.Repository, userID, n, cost int64) {
	t.Helper()
	const chunk = 500
	for done := int64(0); done < n; done += chunk {
		size := min(chunk, n-done)
		batch := make([]*domain.UsageLog, size)
		for i := range batch {
			batch[i] = cursorLog(userID, cost)
		}
		require.NoError(t, repos.Usages.InsertBatch(context.Background(), batch))
	}
}

// cursorUserGroup 同用户消费组（对齐 billing.groupLedgerRows 语义：同 user 恒
// 同组、一组一笔事务；保序确定性不依赖 map 迭代序）。
type cursorUserGroup struct {
	userID int64
	rows   []domain.LedgerRow
}

func cursorGroups(rows []domain.LedgerRow) []*cursorUserGroup {
	byUID := make(map[int64]*cursorUserGroup, 16)
	out := make([]*cursorUserGroup, 0, 16)
	for _, r := range rows {
		g, ok := byUID[r.UserID]
		if !ok {
			g = &cursorUserGroup{userID: r.UserID}
			byUID[r.UserID] = g
			out = append(out, g)
		}
		g.rows = append(g.rows, r)
	}
	return out
}

// cursorFindGroup 按 userID 取组（缺失 → nil）。
func cursorFindGroup(groups []*cursorUserGroup, uid int64) *cursorUserGroup {
	for _, g := range groups {
		if g.userID == uid {
			return g
		}
	}
	return nil
}

// cursorGroupSum 组内成本和（逐行累加——cost == Σ rows.Cost 不变量）与 id 序列
//（DeductOnlyAndMark 标记面实参）。
func cursorGroupSum(g *cursorUserGroup) (cost int64, ids []int64) {
	ids = make([]int64, 0, len(g.rows))
	for _, r := range g.rows {
		cost += r.Cost
		ids = append(ids, r.ID)
	}
	return cost, ids
}

// drainBillingCursor 消费循环至游标清空（镜像 billing.flusher consumeBatch 主
// 路径：取批 LIMIT 500 → 分组 → 逐组单事务 DeductOnlyAndMark）。返回退出游标
// 的行数与其中 quarantined（用户缺失零扣费标记）行数。轮数上限 = 看门狗
//（有界收敛，替代 sleep——病态不推进时快速失败而非拖垮套件）。
func drainBillingCursor(t *testing.T, repos *repository.Repository) (drained, quarantined int64) {
	t.Helper()
	ctx := context.Background()
	for round := 0; ; round++ {
		if round >= 200 {
			t.Fatalf("drainBillingCursor: 游标 200 轮未清空——消费推进卡死")
		}
		rows, err := repos.FetchUnbilledBatch(ctx, 500)
		require.NoError(t, err)
		if len(rows) == 0 {
			return drained, quarantined
		}
		for _, g := range cursorGroups(rows) {
			cost, ids := cursorGroupSum(g)
			_, _, q, err := repos.DeductOnlyAndMark(ctx, g.userID, cost, ids)
			require.NoError(t, err)
			drained += int64(len(g.rows))
			if q {
				quarantined += int64(len(g.rows))
			}
		}
	}
}

// cursorBalance 用户余额回读。
func cursorBalance(t *testing.T, repos *repository.Repository, uid int64) int64 {
	t.Helper()
	u, err := repos.GetUser(context.Background(), uid)
	require.NoError(t, err)
	return u.Balance
}

// cursorBilledStats billed=true 行集合的（行数, Σcost）——对账恒等式主断言面。
func cursorBilledStats(t *testing.T, repos *repository.Repository) (int, int64) {
	t.Helper()
	rows, err := repos.Client.UsageLog.Query().
		Where(usagelog.BilledEQ(true)).All(context.Background())
	require.NoError(t, err)
	var sum int64
	for _, r := range rows {
		sum += r.Cost
	}
	return len(rows), sum
}

// cursorUnbilledCount 未标记行数（ent 直查——QueryUsages 投影不含 billed）。
func cursorUnbilledCount(t *testing.T, repos *repository.Repository) int {
	t.Helper()
	n, err := repos.Client.UsageLog.Query().
		Where(usagelog.BilledEQ(false)).Count(context.Background())
	require.NoError(t, err)
	return n
}

// usageLogRow 按 id 回读 ent 行（billed/overdraft/cost 列值断言面）。
func cursorUsageLogRow(t *testing.T, repos *repository.Repository, id int64) *ent.UsageLog {
	t.Helper()
	row, err := repos.Client.UsageLog.Get(context.Background(), id)
	require.NoError(t, err)
	return row
}

// —— 族 1：CrashRecovery ——

// TestPGBillingCursorCrashRecovery 崩溃恢复：部分用户先成功扣减 → 注入一次失败
//（已取消 ctx = 事务未开启即败，行保持 unbilled 由游标天然重放）+ 已删用户行
// quarantined 路径 → 重放收敛。断言：成功组余额精确、quarantined 行 billed=true
// 零扣费、重放无重复（Σbilled 行 cost == Σ|余额变动| + quarantine 和）。
func TestPGBillingCursorCrashRecovery(t *testing.T) {
	repos := newPGRepos(t)
	ensureCursorPartitions(t, repos)
	ctx := context.Background()

	u1 := seedPGUser(t, repos, "crash-u1@example.com")
	u2 := seedPGUser(t, repos, "crash-u2@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u1.ID, 1_000_000))
	require.NoError(t, repos.UpdateUserBalance(ctx, u2.ID, 800_000))
	const ghostUID = int64(987654321) // 已删用户：从不建行

	seedCursorRows(t, repos, u1.ID, 2, 300_000)
	seedCursorRows(t, repos, u2.ID, 2, 200_000)
	seedCursorRows(t, repos, ghostUID, 1, 150_000)

	all := fetchAllUnbilled(t, repos)
	require.Len(t, all, 5)
	groups := cursorGroups(all)
	require.Len(t, groups, 3)
	u1Group := cursorFindGroup(groups, u1.ID)
	u2Group := cursorFindGroup(groups, u2.ID)
	ghostGroup := cursorFindGroup(groups, ghostUID)
	require.NotNil(t, u1Group)
	require.NotNil(t, u2Group)
	require.NotNil(t, ghostGroup)

	// 成功组：手工消费 u1 → 余额精确（1000000 − 600000）
	u1Cost, u1IDs := cursorGroupSum(u1Group)
	bal, od, q, err := repos.DeductOnlyAndMark(ctx, u1.ID, u1Cost, u1IDs)
	require.NoError(t, err)
	require.False(t, od)
	require.False(t, q)
	require.Equal(t, int64(400_000), bal)
	require.Equal(t, 3, cursorUnbilledCount(t, repos), "仅 u2×2 + ghost×1 待处理")

	// 失败注入：已取消 ctx 上重试 u2 组（崩溃瞬间语义）→ 报错且零标记
	failCtx, cancel := context.WithCancel(ctx)
	cancel()
	u2Cost, u2IDs := cursorGroupSum(u2Group)
	_, _, _, err = repos.DeductOnlyAndMark(failCtx, u2.ID, u2Cost, u2IDs)
	require.Error(t, err, "已取消 ctx 上事务开启即败")
	require.Equal(t, 3, cursorUnbilledCount(t, repos), "失败注入不标记任何行")

	// quarantined 路径：ghost 组跳过扣减仍标记
	ghostCost, ghostIDs := cursorGroupSum(ghostGroup)
	gbal, god, gq, err := repos.DeductOnlyAndMark(ctx, ghostUID, ghostCost, ghostIDs)
	require.NoError(t, err)
	require.True(t, gq, "用户缺失 → quarantined 出口")
	require.False(t, god)
	require.Zero(t, gbal)
	require.Equal(t, 2, cursorUnbilledCount(t, repos), "ghost 行已标记退出游标")

	// 重放：重启后消费循环至清空——u2 恰扣一次、u1 不再扣
	drained, quarantinedN := drainBillingCursor(t, repos)
	require.Equal(t, int64(2), drained)
	require.Zero(t, quarantinedN)
	require.Equal(t, int64(400_000), cursorBalance(t, repos, u1.ID), "u1 不被重放双扣")
	require.Equal(t, int64(400_000), cursorBalance(t, repos, u2.ID), "u2 重放恰扣一次")

	// 对账恒等式：Σbilled 行 cost == Σ|余额变动| + quarantine 和
	nBilled, sumBilled := cursorBilledStats(t, repos)
	require.Equal(t, 5, nBilled, "全部行退出游标")
	require.Equal(t, (1_000_000-400_000)+(800_000-400_000)+ghostCost, sumBilled,
		"Σbilled cost == 扣减凭证和 + 隔离零扣费行和")

	_, lagN, err := repos.UnbilledLag(ctx)
	require.NoError(t, err)
	require.Zero(t, lagN)

	// 再重放一轮：零副作用（幂等收敛终态）
	drainedAgain, _ := drainBillingCursor(t, repos)
	require.Zero(t, drainedAgain)
	require.Equal(t, int64(400_000), cursorBalance(t, repos, u2.ID))
}

// —— 族 2：SingleWriterNoConflict ——

// TestPGBillingCursorSingleWriterNoConflict 结构性断言：usage flusher 写入路径
//（InsertBatch）产出的 billable 行出生恒 billed=false 直至消费；消费后
// Σ(billed=true 行 cost) == Σ 扣减凭证（逐用户余额变动精确和）。
func TestPGBillingCursorSingleWriterNoConflict(t *testing.T) {
	repos := newPGRepos(t)
	ensureCursorPartitions(t, repos)
	ctx := context.Background()

	u1 := seedPGUser(t, repos, "sw-u1@example.com")
	u2 := seedPGUser(t, repos, "sw-u2@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u1.ID, 1_000_000))
	require.NoError(t, repos.UpdateUserBalance(ctx, u2.ID, 1_000_000))

	seedCursorRows(t, repos, u1.ID, 2, 130)
	seedCursorRows(t, repos, u2.ID, 1, 270)

	// 结构断言①：写入路径产出恒 billed=false（单写者不预标记）
	require.Equal(t, 3, cursorUnbilledCount(t, repos), "InsertBatch 出生行全部待消费")

	drained, quarantinedN := drainBillingCursor(t, repos)
	require.Equal(t, int64(3), drained)
	require.Zero(t, quarantinedN)

	// 结构断言②：Σ(billed=true 行 cost) == Σ 扣减凭证
	nBilled, sumBilled := cursorBilledStats(t, repos)
	require.Equal(t, 3, nBilled)
	require.Equal(t, int64(530), sumBilled)
	balU1 := cursorBalance(t, repos, u1.ID)
	balU2 := cursorBalance(t, repos, u2.ID)
	require.Equal(t, int64(999_740), balU1, "u1 凭证 2×130=260")
	require.Equal(t, int64(999_730), balU2, "u2 凭证 270")
	require.Equal(t, int64(2_000_000)-sumBilled, balU1+balU2,
		"Σbilled 行 cost == Σ 扣减凭证（全局对账闭合）")

	// 结构断言③：消费后新写入行仍出生 billed=false 并入游标（写者永不预标记）
	seedCursorRows(t, repos, u1.ID, 1, 111)
	require.Equal(t, 1, cursorUnbilledCount(t, repos))
	rows, err := repos.FetchUnbilledBatch(ctx, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, int64(111), rows[0].Cost)
}

// —— 族 3：BurstBacklog ——

// TestPGBillingCursorBurstBacklog 积压收敛：种 5000 行 unbilled（10 用户 ×
// 500 行）→ 循环消费至清空 → 全量收敛 + UnbilledLag 出数（count 归零 / oldest
// 零值）。
func TestPGBillingCursorBurstBacklog(t *testing.T) {
	repos := newPGRepos(t)
	ensureCursorPartitions(t, repos)
	ctx := context.Background()

	const users = 10
	const rowsPerUser = 500
	uids := make([]int64, users)
	expectedDeduct := make(map[int64]int64, users)
	for i := range uids {
		u := seedPGUser(t, repos, fmt.Sprintf("burst-%d@example.com", i))
		uids[i] = u.ID
		require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 100_000_000))
		rowCost := int64(i%5+1) * 100
		seedCursorRows(t, repos, u.ID, rowsPerUser, rowCost)
		expectedDeduct[u.ID] = rowCost * rowsPerUser
	}

	// lag 出数：积压可见（护栏数据源契约；oldest 落在固定种子日内）
	oldest, lagN, err := repos.UnbilledLag(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(users*rowsPerUser), lagN)
	require.False(t, oldest.IsZero())
	require.False(t, oldest.Before(cursorSeedTime), "oldest ≥ 种子日基点")
	require.Less(t, oldest.Sub(cursorSeedTime), time.Hour, "oldest 在种子错峰窗内")

	drained, quarantinedN := drainBillingCursor(t, repos)
	require.Equal(t, int64(users*rowsPerUser), drained)
	require.Zero(t, quarantinedN)

	// 全量收敛：逐用户余额精确 + 游标清空
	for _, uid := range uids {
		require.Equal(t, 100_000_000-expectedDeduct[uid], cursorBalance(t, repos, uid),
			fmt.Sprintf("user %d 余额精确", uid))
	}
	require.Zero(t, cursorUnbilledCount(t, repos))
	oldest, lagN, err = repos.UnbilledLag(ctx)
	require.NoError(t, err)
	require.Zero(t, lagN, "count 归零")
	require.True(t, oldest.IsZero(), "oldest 零值")
}

// —— 族 4：PoisonAdvance ——

// TestPGBillingCursorPoisonAdvance 毒行推进：UserID=0（匿名 NULL user_id）与
// 不存在用户行混批 → 消费推进不卡死、缺失用户行 billed=true 零扣费、正常行照扣。
func TestPGBillingCursorPoisonAdvance(t *testing.T) {
	repos := newPGRepos(t)
	ensureCursorPartitions(t, repos)
	ctx := context.Background()

	u := seedPGUser(t, repos, "poison-u@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 1_000_000))
	const ghost1 = int64(888888001)
	const ghost2 = int64(888888002)

	// 交错落库：游标序（id 升序）中毒行与正常行混合
	seedCursorRows(t, repos, u.ID, 1, 100_000)
	seedCursorRows(t, repos, ghost1, 1, 50_000)
	seedCursorRows(t, repos, u.ID, 1, 100_000)
	seedCursorRows(t, repos, 0, 1, 70_000) // UserID=0 → 列 NULL → COALESCE 归 0
	seedCursorRows(t, repos, ghost2, 1, 60_000)

	rows := fetchAllUnbilled(t, repos)
	require.Len(t, rows, 5, "UserID=0 行进取批（COALESCE(user_id,0)）——不因毒行缺批")
	hasAnon := false
	for _, r := range rows {
		if r.UserID == 0 {
			hasAnon = true
		}
	}
	require.True(t, hasAnon, "匿名行以 userID=0 进组")

	drained, quarantinedN := drainBillingCursor(t, repos)
	require.Equal(t, int64(5), drained, "全批推进不卡死")
	require.Equal(t, int64(3), quarantinedN, "ghost1 + 匿名 + ghost2 三组隔离零扣费")

	// 正常行照扣精确；毒行全部 billed=true 零扣费退出游标
	require.Equal(t, int64(800_000), cursorBalance(t, repos, u.ID))
	require.Zero(t, cursorUnbilledCount(t, repos))
	for _, r := range rows {
		row := cursorUsageLogRow(t, repos, r.ID)
		require.True(t, row.Billed, "id=%d 全部标记", r.ID)
		require.False(t, row.Overdraft, "id=%d 隔离路径零透支", r.ID)
	}
}

// —— 族 5：AbortIncluded ——

// TestPGBillingCursorAbortIncluded error_type='abort' 行照常入账：cost 口径与
// none 一致（同组同事务同额扣减、同样 billed 翻转、不透支）。
func TestPGBillingCursorAbortIncluded(t *testing.T) {
	repos := newPGRepos(t)
	ensureCursorPartitions(t, repos)
	ctx := context.Background()

	u := seedPGUser(t, repos, "abort-u@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 1_000_000))

	noneRow := cursorLog(u.ID, 200_000)
	abortRow := cursorLog(u.ID, 200_000)
	abortRow.ErrorType = domain.ErrAbort
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{noneRow, abortRow}))

	rows := fetchAllUnbilled(t, repos)
	require.Len(t, rows, 2, "none 与 abort 都进取批（error_type IN ('none','abort')）")

	drained, quarantinedN := drainBillingCursor(t, repos)
	require.Equal(t, int64(2), drained)
	require.Zero(t, quarantinedN)

	// abort 入账口径与 none 一致：同额扣减、同翻转、零透支
	require.Equal(t, int64(600_000), cursorBalance(t, repos, u.ID), "2×200000 同额入账")
	require.Zero(t, cursorUnbilledCount(t, repos))
	for _, r := range rows {
		row := cursorUsageLogRow(t, repos, r.ID)
		require.True(t, row.Billed)
		require.False(t, row.Overdraft)
		require.Equal(t, int64(200_000), row.Cost, "cost 口径不变")
	}
}

// —— 族 6：MultiInstanceLock ——

// TestPGBillingCursorMultiInstanceLock 多实例互斥（行为级 + 源码级守卫）：
// 会话级 advisory lock 下持锁者消费、另一方 ok=false 跳过本周期；释放后可再抢。
// 源码守卫：billing 消费面可执行代码不得出现 pg_advisory_xact_lock（Momus M1
// 双扣防线——每事务锁取批与标记间无互斥）。
func TestPGBillingCursorMultiInstanceLock(t *testing.T) {
	repos := newPGRepos(t)
	ensureCursorPartitions(t, repos)
	ctx := context.Background()

	u := seedPGUser(t, repos, "lock-u@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 1_000_000))
	seedCursorRows(t, repos, u.ID, 2, 100_000)

	// 实例 A 抢锁成功；实例 B 同刻抢锁 ok=false（会话级互斥）
	releaseA, okA, err := repos.AcquireBillingLock(ctx)
	require.NoError(t, err)
	require.True(t, okA)
	releaseB, okB, err := repos.AcquireBillingLock(ctx)
	require.NoError(t, err)
	require.False(t, okB, "持锁期间他实例抢锁失败")
	require.Nil(t, releaseB)

	// 实例 B（真实 flusher 消费周期，经 Close 排空循环驱动 consumeCycle）
	// 在持锁窗口内跳过：首周期 ok=false → n==0 即退——行不被消费、余额不动
	flusherB := billing.NewFlusher(
		billing.FlushConfig{FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour},
		repos, billing.NewBalances(repos, nil), nil)
	skipCtx, skipCancel := context.WithTimeout(ctx, 30*time.Second)
	defer skipCancel()
	require.NoError(t, flusherB.Close(skipCtx))
	require.Equal(t, 2, cursorUnbilledCount(t, repos), "锁他实例持有：本周期跳过不消费")
	require.Equal(t, int64(1_000_000), cursorBalance(t, repos, u.ID))

	// 持锁者 A 在锁内完成消费周期后释放
	rows, err := repos.FetchUnbilledBatch(ctx, 500)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	bal, od, q, err := repos.DeductOnlyAndMark(ctx, u.ID, 200_000, ledgerRowIDs(rows))
	require.NoError(t, err)
	require.False(t, od)
	require.False(t, q)
	require.Equal(t, int64(800_000), bal)
	releaseA()

	// 释放后实例 C 抢锁成功并消费：游标已空 → 无第二次扣减（多实例无双扣）
	flusherC := billing.NewFlusher(
		billing.FlushConfig{FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour},
		repos, billing.NewBalances(repos, nil), nil)
	drainCtx, drainCancel := context.WithTimeout(ctx, 30*time.Second)
	defer drainCancel()
	require.NoError(t, flusherC.Close(drainCtx))
	require.Equal(t, int64(800_000), cursorBalance(t, repos, u.ID), "游标空：零额外扣减")

	// flusher 周期结束即放锁：仓库级再抢成功
	releaseD, okD, err := repos.AcquireBillingLock(ctx)
	require.NoError(t, err)
	require.True(t, okD, "释放后可再抢")
	releaseD()

	// 源码级守卫：xact 锁反模式防回归
	guardNoXactAdvisoryLock(t)
}

// guardNoXactAdvisoryLock 源码扫描：billing 游标消费面（repo 取批/扣费两文件 +
// billing flusher）非注释行不得含 pg_advisory_xact_lock 字样；正向锚定会话级
// pg_try_advisory_lock 仍在位。注释中的反模式引述不受限。
func guardNoXactAdvisoryLock(t *testing.T) {
	t.Helper()
	for _, path := range []string{"billing_cursor.go", "billing_repo.go", "../billing/flusher.go"} {
		data, err := os.ReadFile(path)
		require.NoError(t, err, "源码守卫读文件失败（须在包目录内运行）: %s", path)
		for i, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			require.NotContains(t, trimmed, "pg_advisory_xact_lock",
				"%s:%d: 禁止每事务 advisory lock（会话级持锁整周期是双扣防线，Momus M1）", path, i+1)
		}
	}
	data, err := os.ReadFile("billing_cursor.go")
	require.NoError(t, err)
	require.Contains(t, string(data), "pg_try_advisory_lock", "会话级 try-lock 形态必须在位")
}

// —— 族 7：CaptureOffAbsorb ——

// TestPGBillingCursorCaptureOffAbsorb BillingCapture=false 出生吸收态模拟：
// InsertBatch 直接写 billed=true 行 → 游标查询返回空、消费周期零动作、余额零变动。
func TestPGBillingCursorCaptureOffAbsorb(t *testing.T) {
	repos := newPGRepos(t)
	ensureCursorPartitions(t, repos)
	ctx := context.Background()

	u := seedPGUser(t, repos, "absorb-u@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 500_000))

	// 关闭计费/匿名行的出生吸收态：写者直接盖 billed=true
	absorbed := cursorLog(u.ID, 300_000)
	absorbed.Billed = true
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{absorbed}))

	// 游标三面全空：取批 / 零价取数 / lag 度量均不见吸收态行
	rows, err := repos.FetchUnbilledBatch(ctx, 100)
	require.NoError(t, err)
	require.Empty(t, rows, "出生吸收态不进游标")
	zeros, err := repos.FetchZeroCostIDs(ctx, 100)
	require.NoError(t, err)
	require.Empty(t, zeros)
	_, lagN, err := repos.UnbilledLag(ctx)
	require.NoError(t, err)
	require.Zero(t, lagN)

	// 消费周期零动作：真实 flusher 排空循环跑完，余额零变动、overdraft 不动
	f := billing.NewFlusher(
		billing.FlushConfig{FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour},
		repos, billing.NewBalances(repos, nil), nil)
	drainCtx, drainCancel := context.WithTimeout(ctx, 30*time.Second)
	defer drainCancel()
	require.NoError(t, f.Close(drainCtx))
	require.Equal(t, int64(500_000), cursorBalance(t, repos, u.ID), "吸收态行零扣费")
	absorbedRows, err := repos.Client.UsageLog.Query().
		Where(usagelog.RequestIDEQ(absorbed.RequestID)).All(ctx)
	require.NoError(t, err)
	require.Len(t, absorbedRows, 1)
	require.True(t, absorbedRows[0].Billed)
	require.False(t, absorbedRows[0].Overdraft)

	// 对照组：billed=false 活行照常进游标（证明上方空游标源于吸收态而非取批失效）
	live := cursorLog(u.ID, 100_000)
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{live}))
	rows, err = repos.FetchUnbilledBatch(ctx, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1, "仅活行进取批")
	drained, _ := drainBillingCursor(t, repos)
	require.Equal(t, int64(1), drained)
	require.Equal(t, int64(400_000), cursorBalance(t, repos, u.ID))
}

// —— 族 8：OverdraftWriteBack ——

// TestPGBillingCursorOverdraftWriteBack 临时额度过期场景：FEFO 无可用行 → 重放
// 走无条件透支路径 → overdraft 列回写 true + 余额负值精确；重放不二次透支。
func TestPGBillingCursorOverdraftWriteBack(t *testing.T) {
	repos := newPGRepos(t)
	ensureCursorPartitions(t, repos)
	ctx := context.Background()

	u := seedPGUser(t, repos, "odwb-u@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 10_000))
	expired := time.Now().Add(-time.Hour).Truncate(time.Second)
	tp := seedTempBalance(t, repos, u.ID, 999_999, &expired) // 已过期：FEFO 不参与

	seedCursorRows(t, repos, u.ID, 2, 20_000) // 合计 40000 > 余额 10000

	// 重放前置失败注入（同族 1 形态）：行保持 unbilled
	failCtx, cancel := context.WithCancel(ctx)
	cancel()
	rows := fetchAllUnbilled(t, repos)
	require.Len(t, rows, 2)
	groups := cursorGroups(rows)
	require.Len(t, groups, 1)
	cost, ids := cursorGroupSum(groups[0])
	_, _, _, err := repos.DeductOnlyAndMark(failCtx, u.ID, cost, ids)
	require.Error(t, err)
	require.Equal(t, 2, cursorUnbilledCount(t, repos))

	// 重放：过期临时额度被跳过 → 条件扣不足 → 无条件透支
	drained, quarantinedN := drainBillingCursor(t, repos)
	require.Equal(t, int64(2), drained)
	require.Zero(t, quarantinedN)
	require.Equal(t, int64(-30_000), cursorBalance(t, repos, u.ID),
		"10000 − 40000 = −30000 精确负值")
	require.Equal(t, int64(999_999), tempBalanceAmount(t, repos, tp), "过期额度不动")

	// overdraft 列回写 true（B2）
	for _, r := range rows {
		row := cursorUsageLogRow(t, repos, r.ID)
		require.True(t, row.Billed)
		require.True(t, row.Overdraft, "id=%d 透支回写", r.ID)
	}

	// 再消费一轮：零额外透支（行已退出游标）
	drainedAgain, _ := drainBillingCursor(t, repos)
	require.Zero(t, drainedAgain)
	require.Equal(t, int64(-30_000), cursorBalance(t, repos, u.ID))
}

// —— 族 9：CostZeroFastMark ——

// TestPGBillingCursorCostZeroFastMark cost=0 行混批：不进取批（不经 FEFO 机器
// ——balances/temp 无变动）走 FetchZeroCostIDs + MarkBilledBulk 快速标记收敛。
func TestPGBillingCursorCostZeroFastMark(t *testing.T) {
	repos := newPGRepos(t)
	ensureCursorPartitions(t, repos)
	ctx := context.Background()

	u := seedPGUser(t, repos, "zc-u@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 1_000_000))
	tp := seedTempBalance(t, repos, u.ID, 80_000, nil) // 永久临时额度

	z1 := cursorLog(u.ID, 0)
	z2 := cursorLog(u.ID, 0)
	paid := cursorLog(u.ID, 120_000)
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{z1, paid, z2}))

	// cost=0 行不进主批（不进 FEFO）
	rows := fetchAllUnbilled(t, repos)
	require.Len(t, rows, 1, "仅 cost>0 行进取批")
	require.Equal(t, int64(120_000), rows[0].Cost)

	// 主批消费：FEFO 只见付费行——临时额度扣 80000 + 余额补 40000
	drained, quarantinedN := drainBillingCursor(t, repos)
	require.Equal(t, int64(1), drained)
	require.Zero(t, quarantinedN)
	require.Equal(t, int64(960_000), cursorBalance(t, repos, u.ID), "1000000 − 40000")
	require.Zero(t, tempBalanceAmount(t, repos, tp), "临时额度恰被付费行耗尽")

	// 零价快速标记：纯标记零资金语义（余额/临时额度不动）
	ids, err := repos.FetchZeroCostIDs(ctx, 100)
	require.NoError(t, err)
	require.Len(t, ids, 2)
	require.NoError(t, repos.MarkBilledBulk(ctx, ids))
	require.Equal(t, int64(960_000), cursorBalance(t, repos, u.ID), "快速标记不扣款")
	require.Zero(t, tempBalanceAmount(t, repos, tp))

	for _, zid := range ids {
		row := cursorUsageLogRow(t, repos, zid)
		require.True(t, row.Billed, "cost=0 行标记收敛")
		require.False(t, row.Overdraft, "出生 false 保持")
	}
	_, lagN, err := repos.UnbilledLag(ctx)
	require.NoError(t, err)
	require.Zero(t, lagN, "全游标清空")
}

// —— 族 10：RestartConvergence ——

// TestPGBillingCursorRestartConvergence 停机遗留收敛：实例 A（pgx 载体）部分
// 消费后"停机"→ 实例 B（ent 载体，模拟重启后进程）重新消费 → 全部收敛且零重复。
func TestPGBillingCursorRestartConvergence(t *testing.T) {
	reposA := newPGRepos(t)        // pgx 直连事务载体
	reposB := newPGReposNoPool(t)  // nil pool → ent txDriver 载体（同 schema）
	ensureCursorPartitions(t, reposA)
	ctx := context.Background()

	u1 := seedPGUser(t, reposA, "restart-u1@example.com")
	u2 := seedPGUser(t, reposA, "restart-u2@example.com")
	require.NoError(t, reposA.UpdateUserBalance(ctx, u1.ID, 500_000))
	require.NoError(t, reposA.UpdateUserBalance(ctx, u2.ID, 900_000))

	seedCursorRows(t, reposA, u1.ID, 3, 100_000)
	seedCursorRows(t, reposA, u2.ID, 2, 250_000)

	// 停机前：实例 A 只消费 u1 组的第一行（部分推进即中断）
	rows := fetchAllUnbilled(t, reposA)
	require.Len(t, rows, 5)
	u1Group := cursorFindGroup(cursorGroups(rows), u1.ID)
	require.NotNil(t, u1Group)
	first := u1Group.rows[0]
	bal, od, q, err := reposA.DeductOnlyAndMark(ctx, u1.ID, first.Cost, []int64{first.ID})
	require.NoError(t, err)
	require.False(t, od)
	require.False(t, q)
	require.Equal(t, int64(400_000), bal)
	require.Equal(t, 4, cursorUnbilledCount(t, reposA), "停机遗留 4 行 unbilled")

	// 重启后：实例 B 全量消费至收敛
	drained, quarantinedN := drainBillingCursor(t, reposB)
	require.Equal(t, int64(4), drained)
	require.Zero(t, quarantinedN)

	// 零重复对账：Σbilled cost == Σ|余额变动|（跨两实例扣减凭证闭合）
	nBilled, sumBilled := cursorBilledStats(t, reposB)
	require.Equal(t, 5, nBilled)
	require.Equal(t, int64(800_000), sumBilled,
		"u1 扣 300000 + u2 扣 500000")
	require.Equal(t, int64(200_000), cursorBalance(t, reposB, u1.ID))
	require.Equal(t, int64(400_000), cursorBalance(t, reposB, u2.ID))
	require.Zero(t, cursorUnbilledCount(t, reposB))
	_, lagN, err := reposB.UnbilledLag(ctx)
	require.NoError(t, err)
	require.Zero(t, lagN)
}

// —— 族 11：CostZeroFastMarkBulk ——

// TestPGBillingCursorCostZeroFastMarkBulk bulk 标记幂等性：重复调用零副作用
//（已标记行静默跳过、不复活不重扣、不存在 id 静默、空批 no-op）。
func TestPGBillingCursorCostZeroFastMarkBulk(t *testing.T) {
	repos := newPGRepos(t)
	ensureCursorPartitions(t, repos)
	ctx := context.Background()

	u := seedPGUser(t, repos, "zcbulk-u@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 777_000))
	tp := seedTempBalance(t, repos, u.ID, 55_000, nil)

	seedCursorRows(t, repos, u.ID, 3, 0) // 三行 cost=0

	ids, err := repos.FetchZeroCostIDs(ctx, 100)
	require.NoError(t, err)
	require.Len(t, ids, 3)

	// 重复调用 + 混入不存在 id + 空批：全部静默成功
	require.NoError(t, repos.MarkBilledBulk(ctx, ids))
	require.NoError(t, repos.MarkBilledBulk(ctx, ids), "幂等：重复标记零副作用")
	require.NoError(t, repos.MarkBilledBulk(ctx, append([]int64{424242424}, ids...)),
		"不存在 id 静默跳过")
	require.NoError(t, repos.MarkBilledBulk(ctx, nil), "空批 no-op")

	// 终态稳定：全标记、零资金语义、重复调用不复活
	require.Zero(t, cursorUnbilledCount(t, repos))
	require.Equal(t, int64(777_000), cursorBalance(t, repos, u.ID), "余额不动")
	require.Equal(t, int64(55_000), tempBalanceAmount(t, repos, tp), "临时额度不动")
	for _, id := range ids {
		row := cursorUsageLogRow(t, repos, id)
		require.True(t, row.Billed, "id=%d 保持已标记", id)
		require.False(t, row.Overdraft, "id=%d overdraft 出生 false 保持", id)
		require.Zero(t, row.Cost)
	}
	_, lagN, err := repos.UnbilledLag(ctx)
	require.NoError(t, err)
	require.Zero(t, lagN)
}
