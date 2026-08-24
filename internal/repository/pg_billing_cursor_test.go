// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// 计费游标消费者 PG 测试套件（三车道拓扑改写，spec-f2opt-settlement §〇-b/§三）：
// 端到端行为级——usage flusher 单写点（InsertBatch，billed=false 出生）落种子行，
// 三车道消费面（SettleBalanceBatch 余额车道 / SettleFefoBatch 临时车道 /
// MarkBilledBulk 零价扫尾 / UnbilledLag 度量 / AcquireBillingLock 会话锁）逐族
// 验收。legacy chunk 族（DeductGroupsAndMark/DeductOnlyAndMark）随 D8 退役——
// 语义等价断言迁移至结算语句族；EPQ 并发标记族为最高优先新族（协调者行锁屏障）。
//
// 基座约定同 pg_account_groups_test.go：TEST_DATABASE_URL 未设置 → t.Skip；
// newPGRepos 每测 DROP SCHEMA 重建；串行无 t.Parallel；testify 只 require。

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
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

// drainBillingCursor 三车道消费循环至游标清空（镜像 billing.flusher §〇-b 主路径：
// Balance 车道 → Temp 车道 → 零价 sweep，直至单轮零进展）。返回退出游标的行数
// 与其中 quarantined（幽灵/毒行隔离零扣费标记）行数。轮数上限 = 看门狗（有界
// 收敛，替代 sleep——病态不推进时快速失败而非拖垮套件）。
func drainBillingCursor(t *testing.T, repos *repository.Repository) (drained, quarantined int64) {
	t.Helper()
	ctx := context.Background()
	for round := 0; ; round++ {
		if round >= 200 {
			t.Fatalf("drainBillingCursor: 游标 200 轮未清空——消费推进卡死")
		}
		var n int64
		resB, err := repos.SettleBalanceBatch(ctx, 2000)
		require.NoError(t, err)
		n += resB.Marked
		quarantined += resB.Quarantined
		resF, err := repos.SettleFefoBatch(ctx, 2000)
		require.NoError(t, err)
		n += resF.Marked
		quarantined += resF.Quarantined
		rows := fetchAllUnbilled(t, repos)
		var zeroIDs []int64
		for _, r := range rows {
			if r.Cost <= 0 {
				zeroIDs = append(zeroIDs, r.ID)
			}
		}
		if len(zeroIDs) > 0 {
			require.NoError(t, repos.MarkBilledBulk(ctx, zeroIDs))
			n += int64(len(zeroIDs))
		}
		if n == 0 {
			return drained, quarantined
		}
		drained += n
	}
}

// —— 族 1：CrashRecovery ——

// TestPGBillingCursorCrashRecovery 崩溃恢复：部分用户先成功扣减（balance 车道
// 窗口 LIMIT=2 恰吃队头两行）→ 注入一次失败（已取消 ctx = 事务未开启即败，行
// 保持 unbilled 由游标天然重放）+ 已删用户行 quarantined 路径 → 重放收敛。断言：
// 成功组余额精确、quarantined 行 billed=true 零扣费、重放无重复（对账恒等式闭合）。
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
	require.Equal(t, 5, cursorUnbilledCount(t, repos))

	// 成功组：balance 车道窗口 LIMIT=2 按 id 升序恰吃 u1 两行（u1 先种）
	res, err := repos.SettleBalanceBatch(ctx, 2)
	require.NoError(t, err)
	require.Equal(t, int64(2), res.Marked)
	require.Len(t, res.Balances, 1)
	require.Equal(t, u1.ID, res.Balances[0].UserID)
	require.Equal(t, int64(400_000), res.Balances[0].Balance)
	require.Equal(t, 3, cursorUnbilledCount(t, repos), "仅 u2×2 + ghost×1 待处理")

	// 失败注入：已取消 ctx 上提交（崩溃瞬间语义）→ 报错且零标记
	failCtx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = repos.SettleBalanceBatch(failCtx, 10)
	require.Error(t, err, "已取消 ctx 上事务开启即败")
	require.Equal(t, 3, cursorUnbilledCount(t, repos), "失败注入零标记")

	// 重放：重启后三车道消费循环至清空——u2 恰扣一次、u1 不再扣、ghost 隔离零扣费
	drained, quarantinedN := drainBillingCursor(t, repos)
	require.Equal(t, int64(3), drained)
	require.Equal(t, int64(1), quarantinedN)
	require.Equal(t, int64(400_000), cursorBalance(t, repos, u1.ID), "u1 不被重放双扣")
	require.Equal(t, int64(400_000), cursorBalance(t, repos, u2.ID), "u2 重放恰扣一次")

	// 对账恒等式：Σbilled 行 cost == Σ|余额变动| + quarantine 和
	nBilled, sumBilled := cursorBilledStats(t, repos)
	require.Equal(t, 5, nBilled, "全部行退出游标")
	require.Equal(t, int64(1_150_000), sumBilled,
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
// （InsertBatch）产出的 billable 行出生恒 billed=false 直至消费；消费后
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
// 500 行）→ 三车道循环消费至清空 → 全量收敛 + UnbilledLag 出数（count 归零 /
// oldest 零值）。
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

// TestPGBillingCursorPoisonAdvance 幽灵推进：UserID=0（匿名 NULL user_id）与
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
	require.True(t, hasAnon, "匿名行以 userID=0 进 balance 车道 totals")

	drained, quarantinedN := drainBillingCursor(t, repos)
	require.Equal(t, int64(5), drained, "全批推进不卡死")
	require.Equal(t, int64(3), quarantinedN, "ghost1 + 匿名 + ghost2 三组隔离零扣费")

	// 正常行照扣精确；毒行全部 billed=true 零扣费退出游标
	require.Equal(t, int64(800_000), cursorBalance(t, repos, u.ID))
	require.Zero(t, cursorUnbilledCount(t, repos))
	for _, r := range rows {
		row := usageLogByID(t, repos, r.ID)
		require.True(t, row.Billed, "id=%d 全部标记", r.ID)
		require.False(t, row.Overdraft, "id=%d 隔离路径零透支", r.ID)
	}
}

// —— 族 5：AbortIncluded ——

// TestPGBillingCursorAbortIncluded error_type='abort' 行照常入账：cost 口径与
// none 一致（同额扣减、同样 billed 翻转、不透支）。
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
		row := usageLogByID(t, repos, r.ID)
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

	// 持锁者 A 在锁内完成 balance 车道结算窗口后释放
	res, err := repos.SettleBalanceBatch(ctx, 500)
	require.NoError(t, err)
	require.Equal(t, int64(2), res.Marked)
	require.Len(t, res.Balances, 1)
	require.Equal(t, u.ID, res.Balances[0].UserID)
	require.Equal(t, int64(800_000), res.Balances[0].Balance)
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

// guardNoXactAdvisoryLock 源码扫描：billing 游标消费面（repo 取批/载体/结算语句
// 及其 SQL 事实源四文件 + billing flusher）非注释行不得含 pg_advisory_xact_lock
// 字样；正向锚定会话级 pg_try_advisory_lock 仍在位。注释中的反模式引述不受限。
func guardNoXactAdvisoryLock(t *testing.T) {
	t.Helper()
	for _, path := range []string{
		"billing_cursor.go", "billing_repo.go", "billing_settle.go", "billing_settle_sql.go", "../billing/flusher.go",
	} {
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

	// 游标两面全空：取批 / lag 度量均不见吸收态行（吸收态 billed=true 行被
	// NOT billed 谓词排除于唯一取批查询）
	rows, err := repos.FetchUnbilledBatch(ctx, 100)
	require.NoError(t, err)
	require.Empty(t, rows, "出生吸收态不进游标")
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

// TestPGBillingCursorOverdraftWriteBack 临时额度过期场景：FEFO 无可用行 → 用户
// 非 temp-active 落 balance 车道重放走无条件透支路径 → overdraft 列回写 true +
// 余额负值精确；重放不二次透支。
func TestPGBillingCursorOverdraftWriteBack(t *testing.T) {
	repos := newPGRepos(t)
	ensureCursorPartitions(t, repos)
	ctx := context.Background()

	u := seedPGUser(t, repos, "odwb-u@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 10_000))
	expired := time.Now().Add(-time.Hour).Truncate(time.Second)
	tp := seedTempBalance(t, repos, u.ID, 999_999, &expired) // 已过期：非 temp-active

	seedCursorRows(t, repos, u.ID, 2, 20_000) // 合计 40000 > 余额 10000

	// 重放前置失败注入（同族 1 形态）：行保持 unbilled
	failCtx, cancel := context.WithCancel(ctx)
	cancel()
	rows := fetchAllUnbilled(t, repos)
	require.Len(t, rows, 2)
	_, err := repos.SettleBalanceBatch(failCtx, 10)
	require.Error(t, err)
	require.Equal(t, 2, cursorUnbilledCount(t, repos))

	// 重放：过期临时额度被跳过（非 temp-active 进 balance 车道）→ 条件扣不足 → 无条件透支
	drained, quarantinedN := drainBillingCursor(t, repos)
	require.Equal(t, int64(2), drained)
	require.Zero(t, quarantinedN)
	require.Equal(t, int64(-30_000), cursorBalance(t, repos, u.ID),
		"10000 − 40000 = −30000 精确负值")
	require.Equal(t, int64(999_999), tempBalanceAmount(t, repos, tp), "过期额度不动")

	// overdraft 列回写 true（B2）
	for _, r := range rows {
		row := usageLogByID(t, repos, r.ID)
		require.True(t, row.Billed)
		require.True(t, row.Overdraft, "id=%d 透支回写", r.ID)
	}

	// 再消费一轮：零额外透支（行已退出游标）
	drainedAgain, _ := drainBillingCursor(t, repos)
	require.Zero(t, drainedAgain)
	require.Equal(t, int64(-30_000), cursorBalance(t, repos, u.ID))
}

// —— 族 9：CostZeroFastMark ——

// TestPGBillingCursorCostZeroFastMark cost=0 行混批（三车道路由形态）：付费行走
// FEFO 车道（temp-active 用户）收敛、零价行由 sweep 纯标记（balances/temp 无变动，
// 零资金移动）。
func TestPGBillingCursorCostZeroFastMark(t *testing.T) {
	repos := newPGRepos(t)
	ensureCursorPartitions(t, repos)
	ctx := context.Background()

	u := seedPGUser(t, repos, "zc-u@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 1_000_000))
	tp := seedTempBalance(t, repos, u.ID, 80_000, nil) // 永久临时额度 → temp-active

	z1 := cursorLog(u.ID, 0)
	z2 := cursorLog(u.ID, 0)
	paid := cursorLog(u.ID, 120_000)
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{z1, paid, z2}))

	// 单取批面：零价行与付费行同批取出
	rows := fetchAllUnbilled(t, repos)
	require.Len(t, rows, 3, "零价行同批取出由消费侧路由")

	drained, quarantinedN := drainBillingCursor(t, repos)
	require.Equal(t, int64(3), drained)
	require.Zero(t, quarantinedN)

	// FEFO 车道只见付费行——临时额度扣 80000 + spill 40000 余额条件扣；
	// 零价 sweep 标记零资金语义
	require.Equal(t, int64(960_000), cursorBalance(t, repos, u.ID), "1000000 − 40000")
	require.Zero(t, tempBalanceAmount(t, repos, tp), "临时额度恰被付费行耗尽")

	for _, r := range rows {
		row := usageLogByID(t, repos, r.ID)
		require.True(t, row.Billed, "id=%d 标记收敛", r.ID)
		require.False(t, row.Overdraft, "id=%d overdraft 出生 false 保持", r.ID)
	}
	_, lagN, err := repos.UnbilledLag(ctx)
	require.NoError(t, err)
	require.Zero(t, lagN, "全游标清空")
}

// —— 族 10：RestartConvergence ——

// TestPGBillingCursorRestartConvergence 停机遗留收敛：实例 A（pgx 载体）部分
// 消费后"停机"→ 实例 B（ent 载体，模拟重启后进程）重新消费 → 全部收敛且零重复。
func TestPGBillingCursorRestartConvergence(t *testing.T) {
	reposA := newPGRepos(t)       // pgx 直连事务载体
	reposB := newPGReposNoPool(t) // nil pool → ent txDriver 载体（同 schema）
	ensureCursorPartitions(t, reposA)
	ctx := context.Background()

	u1 := seedPGUser(t, reposA, "restart-u1@example.com")
	u2 := seedPGUser(t, reposA, "restart-u2@example.com")
	require.NoError(t, reposA.UpdateUserBalance(ctx, u1.ID, 500_000))
	require.NoError(t, reposA.UpdateUserBalance(ctx, u2.ID, 900_000))

	seedCursorRows(t, reposA, u1.ID, 3, 100_000)
	seedCursorRows(t, reposA, u2.ID, 2, 250_000)

	// 停机前：实例 A 只消费队头一行（部分推进即中断）
	res, err := reposA.SettleBalanceBatch(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, int64(1), res.Marked)
	require.Len(t, res.Balances, 1)
	require.Equal(t, u1.ID, res.Balances[0].UserID)
	require.Equal(t, int64(400_000), res.Balances[0].Balance)
	require.Equal(t, 4, cursorUnbilledCount(t, reposA), "停机遗留 4 行 unbilled")

	// 重启后：实例 B（ent 载体）全量消费至收敛
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
// （已标记行静默跳过、不复活不重扣、不存在 id 静默、空批 no-op）。
func TestPGBillingCursorCostZeroFastMarkBulk(t *testing.T) {
	repos := newPGRepos(t)
	ensureCursorPartitions(t, repos)
	ctx := context.Background()

	u := seedPGUser(t, repos, "zcbulk-u@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 777_000))
	tp := seedTempBalance(t, repos, u.ID, 55_000, nil)

	seedCursorRows(t, repos, u.ID, 3, 0) // 三行 cost=0

	rows := fetchAllUnbilled(t, repos)
	require.Len(t, rows, 3, "零价行进取批（单取批面）")
	ids := ledgerIDsOf(rows)

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
		row := usageLogByID(t, repos, id)
		require.True(t, row.Billed, "id=%d 保持已标记", id)
		require.False(t, row.Overdraft, "id=%d overdraft 出生 false 保持", id)
		require.Zero(t, row.Cost)
	}
	_, lagN, err := repos.UnbilledLag(ctx)
	require.NoError(t, err)
	require.Zero(t, lagN)
}

// —— 族 12：EPQ 并发标记（最高优先新族，spec §三/oracle 必改 #1） ——

// TestPGSettleEPQConcurrentMark EPQ 并发标记族：协调者连接持用户行锁阻塞 A 结算
// 语句的 debited 步 → 中途抢标批内一行并提交 → 放锁后 A 的 marked CTE 经
// EvalPlanQual 重评该行新版本（AND NOT l.billed 谓词失败跳过）→ marked<batch
// 计数守卫触发 → 整事务回滚零移动 → 自动重放恰扣一次。屏障经 pg_stat_activity
// 状态同步（他方后端确实阻塞在行锁上），非时序假设。
func TestPGSettleEPQConcurrentMark(t *testing.T) {
	t.Run("balance lane mid-statement rival mark", func(t *testing.T) {
		repos := newPGRepos(t)
		ensureCursorPartitions(t, repos)
		ctx := context.Background()
		u := seedPGUser(t, repos, "epq-bal@example.com")
		require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 1_000_000))
		seedCursorRows(t, repos, u.ID, 3, 100_000)
		rows := fetchAllUnbilled(t, repos)
		require.Len(t, rows, 3)
		victim := rows[0].ID // 批队头（id 最小）

		coord := pgTestPool(t)
		tx, err := coord.Begin(ctx)
		require.NoError(t, err)
		defer tx.Rollback(ctx) // nolint:errcheck // Commit 后 ErrTxClosed 幂等；Fatal 路径防连接泄漏卡池 Close
		// 屏障①：持有用户行锁（updated_at 自赋值不触余额）——A 的 debited
		// UPDATE 将阻塞于此（协调者不触 usage_logs/temp，无锁环）
		_, err = tx.Exec(ctx, `UPDATE users SET updated_at = updated_at WHERE id = $1`, u.ID)
		require.NoError(t, err)

		type epqResult struct {
			res domain.SettlementSummary
			err error
		}
		ch := make(chan epqResult, 1)
		go func() {
			res, err := repos.SettleBalanceBatch(ctx, 10)
			ch <- epqResult{res: res, err: err}
		}()
		waitBlockedOnSettle(t, coord)

		// 中途断言：A 快照后未提交任何移动（回滚零移动的前置证据）
		var midBal int64
		require.NoError(t, tx.QueryRow(ctx, `SELECT balance FROM users WHERE id = $1`, u.ID).Scan(&midBal))
		require.Equal(t, int64(1_000_000), midBal)
		require.Equal(t, 3, cursorUnbilledCount(t, repos))

		// 对手消费者中途抢标队头行（纯标记不扣款——守卫判别面）并提交放锁
		_, err = tx.Exec(ctx, `UPDATE usage_logs SET billed = TRUE WHERE id = $1`, victim)
		require.NoError(t, err)
		require.NoError(t, tx.Commit(ctx))

		r := <-ch
		require.NoError(t, r.err, "并发标记守卫回滚后重放收敛")
		require.Equal(t, int64(2), r.res.Marked, "重放批收缩：被抢标行退出游标")
		require.Equal(t, int64(2), r.res.BatchRows)

		// 恰扣一次总额：victim 成本无人扣（对手纯标记、A 回滚弃扣）
		require.Equal(t, int64(800_000), cursorBalance(t, repos, u.ID))
		require.True(t, usageLogByID(t, repos, victim).Billed, "对手标记不被回滚复活")
		for _, row := range rows {
			got := usageLogByID(t, repos, row.ID)
			require.True(t, got.Billed)
			require.False(t, got.Overdraft)
		}
		require.Zero(t, cursorUnbilledCount(t, repos))
	})

	t.Run("pre-committed rival mark shrinks batch", func(t *testing.T) {
		repos := newPGRepos(t)
		ensureCursorPartitions(t, repos)
		ctx := context.Background()
		u1 := seedPGUser(t, repos, "epq-pre-u1@example.com")
		u2 := seedPGUser(t, repos, "epq-pre-u2@example.com")
		require.NoError(t, repos.UpdateUserBalance(ctx, u1.ID, 1_000_000))
		require.NoError(t, repos.UpdateUserBalance(ctx, u2.ID, 1_000_000))
		seedCursorRows(t, repos, u1.ID, 2, 100_000)
		seedCursorRows(t, repos, u2.ID, 2, 100_000)
		all := fetchAllUnbilled(t, repos)
		require.Len(t, all, 4)

		// 锁丢失窗口形态：对手已完整结算 u1 首行（扣款凭证 + 提交态标记）
		require.NoError(t, repos.MarkBilledBulk(ctx, []int64{all[0].ID}))
		require.NoError(t, repos.UpdateUserBalance(ctx, u1.ID, -100_000))

		// 本实例结算：提交态已标行在快照即退出取批——无守卫触发，余下三行恰扣一次
		res, err := repos.SettleBalanceBatch(ctx, 10)
		require.NoError(t, err)
		require.Equal(t, int64(3), res.Marked)
		byUID := map[int64]int64{}
		for _, p := range res.Balances {
			byUID[p.UserID] = p.Balance
		}
		require.Equal(t, int64(800_000), byUID[u1.ID], "u1 900000 − 100000（仅剩一行）")
		require.Equal(t, int64(800_000), byUID[u2.ID])
		// 守恒：Σ|Δ|（含对手已提交的 100000 凭证）== 对手凭证 + 本窗口结算成本
		require.Equal(t, int64(400_000), (1_000_000-byUID[u1.ID])+(1_000_000-byUID[u2.ID]),
			"守恒：Σ|Δ| == 对手凭证 100000 + 新结算成本 300000")
		require.Zero(t, cursorUnbilledCount(t, repos))
	})

	t.Run("fefo lane mid-statement rival mark", func(t *testing.T) {
		repos := newPGRepos(t)
		ensureCursorPartitions(t, repos)
		ctx := context.Background()
		u := seedPGUser(t, repos, "epq-fefo@example.com")
		require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 1_000_000))
		tp := seedTempBalance(t, repos, u.ID, 150_000, nil) // 永久 temp → temp-active
		seedCursorRows(t, repos, u.ID, 3, 100_000)          // delta 300000 → drawn 150000 + spill 150000
		rows := fetchAllUnbilled(t, repos)
		require.Len(t, rows, 3)
		victim := rows[0].ID

		coord := pgTestPool(t)
		tx, err := coord.Begin(ctx)
		require.NoError(t, err)
		defer tx.Rollback(ctx) // nolint:errcheck // Commit 后 ErrTxClosed 幂等；Fatal 路径防连接泄漏卡池 Close
		_, err = tx.Exec(ctx, `UPDATE users SET updated_at = updated_at WHERE id = $1`, u.ID)
		require.NoError(t, err)

		type epqResult struct {
			res domain.SettlementSummary
			err error
		}
		ch := make(chan epqResult, 1)
		go func() {
			res, err := repos.SettleFefoBatch(ctx, 10)
			ch <- epqResult{res: res, err: err}
		}()
		waitBlockedOnSettle(t, coord)

		// 中途断言：temp_drawn/debited/marked 全部未提交（整语句原子域）
		var midBal int64
		require.NoError(t, tx.QueryRow(ctx, `SELECT balance FROM users WHERE id = $1`, u.ID).Scan(&midBal))
		require.Equal(t, int64(1_000_000), midBal)
		require.Equal(t, int64(150_000), tempBalanceAmount(t, repos, tp), "temp 消耗随事务回滚前置证据")
		require.Equal(t, 3, cursorUnbilledCount(t, repos))

		_, err = tx.Exec(ctx, `UPDATE usage_logs SET billed = TRUE WHERE id = $1`, victim)
		require.NoError(t, err)
		require.NoError(t, tx.Commit(ctx))

		r := <-ch
		require.NoError(t, r.err, "fefo 车道并发标记守卫回滚后重放收敛")
		require.Equal(t, int64(2), r.res.Marked)
		require.Equal(t, int64(2), r.res.BatchRows)

		// 重放恰扣一次：2 行 delta 200000 → drawn 150000 + spill 50000 条件扣
		require.Equal(t, int64(950_000), cursorBalance(t, repos, u.ID))
		require.Zero(t, tempBalanceAmount(t, repos, tp), "temp 恰消耗一次（回滚不残留）")
		for _, row := range rows {
			got := usageLogByID(t, repos, row.ID)
			require.True(t, got.Billed)
			require.False(t, got.Overdraft)
		}
		require.Zero(t, cursorUnbilledCount(t, repos))
	})
}

// waitBlockedOnSettle 屏障等待（状态同步非竞态掩蔽）：轮询 pg_stat_activity 直到
// 存在他方后端阻塞在行锁上且语句为三车道结算 CTE（'WITH batch AS' 特征前缀——
// pg_stat_activity.query 截断于 1024 字节，语句尾部的 'FROM ghosts' 聚合段不可
// 见，锚点必须落在截断窗内）。有界看门狗替代无限等待；轮询间隔仅作用于外部进
// 程状态观测，不掩盖任何进程内竞态。
func waitBlockedOnSettle(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var n int
		err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid()
			  AND wait_event_type = 'Lock' AND query LIKE '%WITH batch AS%'`).Scan(&n)
		require.NoError(t, err)
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("EPQ 屏障超时：结算语句未按预期阻塞在 debited 行锁")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// —— 族 13：毒行梯子三态（spec §三；梯子随迁仓库侧 settleBatch） ——

// TestPGSettleLadderThreeStates 梯子三态：瞬态恢复（锁争用自愈，无隔离）/
// K 失败隔离越行（确定性毒行）/ 空游标退出。
func TestPGSettleLadderThreeStates(t *testing.T) {
	t.Run("transient recovery: lock contention self-heals", func(t *testing.T) {
		repos := newPGRepos(t)
		ensureCursorPartitions(t, repos)
		ctx := context.Background()
		u := seedPGUser(t, repos, "ladder-t@example.com")
		require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 1_000_000))
		seedCursorRows(t, repos, u.ID, 2, 100_000)

		coord := pgTestPool(t)
		tx, err := coord.Begin(ctx)
		require.NoError(t, err)
		defer tx.Rollback(ctx) // nolint:errcheck // 瞬态放锁路径本就 Rollback；Fatal 路径防连接泄漏卡池 Close
		_, err = tx.Exec(ctx, `UPDATE users SET updated_at = updated_at WHERE id = $1`, u.ID)
		require.NoError(t, err)

		type epqResult struct {
			res domain.SettlementSummary
			err error
		}
		ch := make(chan epqResult, 1)
		go func() {
			res, err := repos.SettleBalanceBatch(ctx, 10)
			ch <- epqResult{res: res, err: err}
		}()
		waitBlockedOnSettle(t, coord) // 纯瞬态争用：确认阻塞后即放锁不抢标
		require.NoError(t, tx.Rollback(ctx))

		r := <-ch
		require.NoError(t, r.err, "瞬态失败自愈——同一调用内完成")
		require.Equal(t, int64(2), r.res.Marked)
		require.Equal(t, int64(0), r.res.Quarantined, "瞬态不计隔离")
		require.Len(t, r.res.Balances, 1)
		require.Equal(t, int64(800_000), r.res.Balances[0].Balance)
		require.Equal(t, int64(800_000), cursorBalance(t, repos, u.ID))
		require.Zero(t, cursorUnbilledCount(t, repos))
	})

	t.Run("K-failure isolation then advance", func(t *testing.T) {
		repos := newPGRepos(t)
		ensureCursorPartitions(t, repos)
		ctx := context.Background()
		u := seedPGUser(t, repos, "ladder-k@example.com")
		// 确定性毒行配方：余额贴近 int64 下界——forced 补刀 balance−delta 数值
		// 下溢 → bigint 赋值 22003 报错，语句每次尝试恒败；MarkBilledBulk 只触
		// usage_logs 不受毒影响 → 梯子耗尽后探针定位队头写销越行。
		require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, math.MinInt64+1))
		seedCursorRows(t, repos, u.ID, 2, 1000)
		all := fetchAllUnbilled(t, repos)
		require.Len(t, all, 2)
		head, follower := all[0].ID, all[1].ID // 批序 = id 升序

		res1, err := repos.SettleBalanceBatch(ctx, 10)
		require.NoError(t, err, "梯子内部隔离成功——调用方零错误")
		require.Equal(t, int64(1), res1.Marked, "队头行写销越行")
		require.Equal(t, int64(1), res1.Quarantined, "隔离行计 quarantined")
		isolated := usageLogByID(t, repos, head)
		require.True(t, isolated.Billed, "毒队头被终极隔离标记（退出游标）")
		require.False(t, isolated.Overdraft, "隔离路径 od 出生 false 保持")
		remaining := fetchAllUnbilled(t, repos)
		require.Len(t, remaining, 1, "仅毒队头被隔离，次行保持 unbilled")
		require.Equal(t, follower, remaining[0].ID)
		require.False(t, usageLogByID(t, repos, follower).Billed)
		require.Equal(t, int64(math.MinInt64+1), cursorBalance(t, repos, u.ID),
			"毒行从未被扣费（写销该行计费）")

		// 越行推进：第二行成为新队头 → 同样隔离
		res2, err := repos.SettleBalanceBatch(ctx, 10)
		require.NoError(t, err)
		require.Equal(t, int64(1), res2.Marked)
		require.Equal(t, int64(1), res2.Quarantined)
		require.True(t, usageLogByID(t, repos, follower).Billed)
		require.Zero(t, cursorUnbilledCount(t, repos), "游标永不卡死")

		// 第三调用：空游标零进展退出（nil 错误零计数）
		res3, err := repos.SettleBalanceBatch(ctx, 10)
		require.NoError(t, err)
		require.Zero(t, res3.Marked)
		_, lagN, err := repos.UnbilledLag(ctx)
		require.NoError(t, err)
		require.Zero(t, lagN)
	})

	t.Run("empty-cursor exit", func(t *testing.T) {
		repos := newPGRepos(t)
		ensureCursorPartitions(t, repos)
		ctx := context.Background()

		resB, err := repos.SettleBalanceBatch(ctx, 10)
		require.NoError(t, err)
		require.Zero(t, resB.BatchRows)
		require.Zero(t, resB.Marked)
		resF, err := repos.SettleFefoBatch(ctx, 10)
		require.NoError(t, err)
		require.Zero(t, resF.BatchRows)
		require.Zero(t, resF.Marked)
		_, lagN, err := repos.UnbilledLag(ctx)
		require.NoError(t, err)
		require.Zero(t, lagN)
	})
}

// ledgerHeadID 已删除（K 失败族队头定位走 fetchAllUnbilled 差集，无需独立查询面）。

// —— 族 14：SyncCommitOffSmoke（F2-opt D4 形态保持） ——

// TestPGBillingChunkSyncCommitOffSmoke sync_commit 会话级让渡冒烟（D4）：SET
// LOCAL synchronous_commit TO off 事务作用域生效（tx 内 current_setting=off）、
// 连接归还即失效（tx 外回落库默认 on——零泄漏面）；结算事务含 SET LOCAL 首
// 语句正常提交（SET 失败 = 回滚重放的安全缺省由既有回滚族覆盖）。
func TestPGBillingChunkSyncCommitOffSmoke(t *testing.T) {
	repos := newPGRepos(t)
	ensureCursorPartitions(t, repos)
	ctx := context.Background()

	u := seedPGUser(t, repos, "synccommit@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 100_000))
	seedCursorRows(t, repos, u.ID, 1, 10_000)

	// 事务作用域断言：独立连接手工复现结算事务首语句形态
	conn, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)
	defer conn.Close(ctx)
	var setting string
	tx, err := conn.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `SET LOCAL synchronous_commit TO off`)
	require.NoError(t, err)
	require.NoError(t, tx.QueryRow(ctx, `SELECT current_setting('synchronous_commit')`).Scan(&setting))
	require.Equal(t, "off", setting, "事务作用域内让渡生效")
	require.NoError(t, tx.Rollback(ctx))
	require.NoError(t, conn.QueryRow(ctx, `SELECT current_setting('synchronous_commit')`).Scan(&setting))
	require.NotEqual(t, "off", setting, "连接归还即失效（tx 外回落库默认 on——零泄漏面）")

	// 行为级冒烟：结算事务含 SET LOCAL 首语句正常提交
	rows := fetchAllUnbilled(t, repos)
	require.Len(t, rows, 1)
	res, err := repos.SettleBalanceBatch(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), res.Marked)
	require.Zero(t, cursorUnbilledCount(t, repos), "结算事务正常提交（行退出游标）")
	require.Equal(t, int64(90_000), cursorBalance(t, repos, u.ID))
}
