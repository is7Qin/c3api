// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

// GET /ops/workers 真实 PG 端到端（spec 2026-08-11 验收：各 worker 指标与真实
// 状态一致断言——pending 增长、落盘计数、retention DROP 分区数、注册表 Status
// 同步；typed struct 断言；admin 鉴权）。
//
//	TEST_DATABASE_URL=postgres://postgres:c3api@localhost:5432/c3api_test_ops \
//	  go test ./cmd/server/ -run TestOpsWorkersPG -v
//
// 独立测试库 c3api_test_ops（本任务专用，避开并行 worktree 竞争）；另用独立
// schema（ops_test）与同库其它 schema 隔离。未设置 TEST_DATABASE_URL → t.Skip。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler"
	"github.com/is7qin/c3api/internal/proxy"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/server"
	"github.com/is7qin/c3api/internal/service"
	"github.com/is7qin/c3api/internal/snapshot"
	"github.com/is7qin/c3api/internal/usage"
)

// opsTestSchema 本测试专用 schema（同一数据库内隔离命名空间）。
const opsTestSchema = "ops_test"

func newOpsPGRepos(t *testing.T) (*repository.Repository, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + opsTestSchema
	} else {
		dsn += "?search_path=" + opsTestSchema
	}
	ctx := context.Background()
	pool, err := repository.OpenPG(ctx, dsn, 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+opsTestSchema+` CASCADE; CREATE SCHEMA `+opsTestSchema+`;`)
	require.NoError(t, err)
	repos, err := repository.NewWithPG(ctx, entsql.OpenDB(dialect.Postgres, db), true, pool)
	require.NoError(t, err)
	return repos, pool
}

// TestOpsWorkersPG 端到端：真实 PG 上装配代表性 worker（usage/errlog/
// retention/rule-engine/scheduler）+ 快照注册表，经 /ops/workers 断言各指标
// 与真实状态一致。
func TestOpsWorkersPG(t *testing.T) {
	repos, pool := newOpsPGRepos(t)
	ctx := context.Background()

	// 三表分区 bootstrap（插入/retention 前置；与 main 装配序一致）。
	now := time.Now()
	require.NoError(t, repos.EnsureUsageLogPartitioned(ctx, now))
	require.NoError(t, repos.EnsureErrLogPartitioned(ctx, now))
	require.NoError(t, repos.EnsureUsageStatsPartitioned(ctx, now))

	// --- 种子数据（注册表 ReloadAll 首刷全部可见） ---
	u, err := repos.CreateUser(ctx, &domain.User{
		Email: "ops@example.com", PasswordHash: "bcrypt-hash",
		Role: domain.RoleUser, Status: domain.UserStatusActive, MaxConcurrency: 8,
		Balance: 123_456,
	})
	require.NoError(t, err)
	g, err := repos.Groups.CreateGroup(ctx, &domain.Group{
		Name: "ops-g", Visibility: domain.GroupVisibilityPublic, PriceMultiplier: 10000,
	})
	require.NoError(t, err)
	tpl, err := repos.CreateTemplate(ctx, &domain.Template{
		Name: "ops-t", BaseURL: "http://upstream.example.com", CredentialType: credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		Models:           []string{"gpt-4o"},
	})
	require.NoError(t, err)
	acc, err := repos.CreateAccount(ctx, &domain.Account{
		Name: "ops-acc", TemplateID: tpl.ID, UpstreamKey: "sk-upstream",
		Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4,
	})
	require.NoError(t, err)
	require.NoError(t, repos.SetAccountGroups(ctx, acc.ID, []int64{g.ID}))
	_, err = repos.CreateKey(ctx, &domain.Key{
		UserID: u.ID, GroupID: g.ID, Name: "k-ops",
		KeyRaw: "ck-ops-1",
		Status: domain.KeyStatusActive, MaxConcurrency: 8, Quota: 1_000_000,
	})
	require.NoError(t, err)
	_, err = repos.UpsertManual(ctx, &repository.PricingManual{
		Model: "gpt-4o", PromptPricePerMillion: 250_000, CompletionPricePerMillion: 1_000_000,
	})
	require.NoError(t, err)

	// --- 构造链（与 main 装配序一致：模块构造零 reload——单一入口） ---
	ruleEngine := rule.New(rule.Config{}, repos.Rules, nil)
	sched := scheduler.New(scheduler.Config{
		DefaultMaxConcurrency: 4, SyncInterval: time.Hour,
	}, repos.Groups, ruleEngine, nil)
	auth := proxy.NewAuth(repos.Keys, repos.Users, nil)
	balances := billing.NewBalances(repos, nil)
	svc := service.New(repos, sched, service.NopInvalidator{}, nil, ruleEngine, auth, nil)
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour, QuotaFlushInterval: time.Hour,
	}, repos.Usages, nil)
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{
		QueueSize: 100, ExemptQueueSize: 100, BatchSize: 10, FlushInterval: 20 * time.Millisecond,
	}, repos.ErrLogs, nil)
	statsAgg := usage.NewStatsAgg(usage.StatsAggConfig{Interval: 20 * time.Millisecond, Lag: 50 * time.Millisecond}, repos.Stats, nil)
	retention := usage.NewRetention(usage.RetentionConfig{
		LogRetentionDays: 1, ErrLogRetentionDays: 0, StatsRetentionDays: 0, TickerInterval: time.Hour,
	}, repos, nil)

	reg := snapshot.New()
	for _, s := range []snapshot.Snapshot{
		authSnapshot{auth}, schedSnapshot{sched}, ruleSnapshot{ruleEngine},
		pricingSnapshot{svc}, balanceSnapshot{balances},
	} {
		require.NoError(t, reg.Register(s))
	}
	require.Empty(t, reg.ReloadAll(ctx), "五路快照首刷全部成功")

	// --- 1) usage pending 增长：不 Start（无 flush），Record 5 条 → 指标 5 ---
	base := time.Now().UTC().Truncate(time.Hour)
	for i := 0; i < 5; i++ {
		rec.Record(&domain.UsageLog{
			RequestID: fmt.Sprintf("ops-%d", i), UserID: u.ID, Model: "gpt-4o",
			Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone,
			TotalTokens: 10, Cost: 1, CreatedAt: base,
		})
	}
	require.Equal(t, int64(5), rec.Stats().(usage.RecorderStats).PendingLogs, "pending 与 Record 累计一致")

	// --- 2) errlog 落盘计数 = 真实行数：Start + 投递 2 条 → 落盘 2 ---
	require.NoError(t, errlogW.Start(ctx))
	errlogW.EnqueueRejected(&domain.UsageLog{
		RequestID: "ops-rej", UserID: u.ID, Model: "gpt-4o", Format: domain.FormatOpenAIChat,
		StatusCode: 429, ErrorType: domain.Err429, CreatedAt: time.Now(),
	})
	errlogW.EnqueueError(&domain.UsageLog{
		RequestID: "ops-err", UserID: u.ID, Model: "gpt-4o", Format: domain.FormatOpenAIChat,
		StatusCode: 502, ErrorType: domain.Err5xx, CreatedAt: time.Now(),
	})
	require.Eventually(t, func() bool { return errlogW.Inserted() == 2 },
		3*time.Second, 10*time.Millisecond, "errlog 周期 flush 落盘")

	// --- 3) retention：预建 2 天前分区 → 启动巡检 DROP（真实 DROP 1 个；
	// DropTablePartitionsBefore 边界为 d.Before(cut)，cutoff=now-1day → 当日
	// 分区保留、2 天前分区删除） ---
	stale := now.AddDate(0, 0, -2)
	require.NoError(t, repos.EnsureUsageLogPartitions(ctx, stale, stale))
	require.NoError(t, retention.Start(ctx))
	require.Eventually(t, func() bool {
		return retention.Stats().(usage.RetentionWorkerStats).LastPatrolUnixMs > 0
	}, 3*time.Second, 10*time.Millisecond, "启动即巡检")
	rst := retention.Stats().(usage.RetentionWorkerStats)
	require.Equal(t, int64(1), rst.LastDroppedLogPartitions, "DROP 分区数与真实一致")

	// --- 4) /api/admin/ops/workers 端点：typed struct 断言 + 指标与真实状态一致 ---
	// （用户裁决并入管理面：路由由契约 chi-server 生成，走 /admin 组鉴权）
	opsWorkers := []handler.StatsProvider{ruleEngine, sched, rec, errlogW, retention, statsAgg}
	ah := handler.New(nil, handler.OpsOptions{
		Workers:   opsWorkers,
		Snapshots: func() []handler.SnapshotState { return snapshotStates(reg.Status()) },
	})
	srv := server.NewServer(server.Options{
		AdminToken:   "tok",
		AdminHandler: ah.Router(),
	})

	// 非 admin → 401。
	req := httptest.NewRequest(http.MethodGet, "/api/admin/ops/workers", nil)
	recw := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recw, req)
	require.Equal(t, http.StatusUnauthorized, recw.Code, "非 admin 401")

	// stats-agg：启动即聚合（首轮初始化 watermark + 空窗口聚合）。观测与真实
	// 一致性比对需确定性：GET 前 cancel 停摆 + 表值冻结（20ms 周期不停则 GET
	// 快照与读表间可能推进一轮），再 GET → 响应值 == 表值。
	saggCtx, cancelStatsAgg := context.WithCancel(context.Background())
	t.Cleanup(cancelStatsAgg)
	require.NoError(t, statsAgg.Start(saggCtx))
	require.Eventually(t, func() bool {
		return statsAgg.Stats().(usage.StatsAggWorkerStats).LastDurationMs > 0
	}, 3*time.Second, 10*time.Millisecond, "首轮聚合轮完成（含耗时观测）")
	cancelStatsAgg()
	require.Eventually(t, func() bool {
		wm1, err1 := repos.Stats.LoadStatsAggWatermark(ctx)
		time.Sleep(50 * time.Millisecond)
		wm2, err2 := repos.Stats.LoadStatsAggWatermark(ctx)
		return err1 == nil && err2 == nil && wm1.Equal(wm2)
	}, 3*time.Second, 20*time.Millisecond, "worker 已停摆（watermark 冻结）")
	wm, err := repos.Stats.LoadStatsAggWatermark(ctx)
	require.NoError(t, err)
	require.False(t, wm.IsZero(), "watermark 已初始化（首轮聚合推进）")

	// admin → 200 + typed struct 解码断言。
	req = httptest.NewRequest(http.MethodGet, "/api/admin/ops/workers", nil)
	req.Header.Set("Authorization", "Bearer tok")
	recw = httptest.NewRecorder()
	srv.Handler().ServeHTTP(recw, req)
	require.Equal(t, http.StatusOK, recw.Code)
	var resp handler.WorkersResponse
	require.NoError(t, json.Unmarshal(recw.Body.Bytes(), &resp))
	require.Len(t, resp.Workers, 6)

	got := map[string]map[string]any{}
	for _, w := range resp.Workers {
		got[w.Name] = w.Stats.(map[string]any)
	}
	require.Equal(t, float64(5), got["usage"]["pending_logs"], "usage pending 与真实一致")
	require.Equal(t, float64(2), got["errlog"]["inserted"], "errlog 落盘计数与真实一致")
	require.NotZero(t, got["retention"]["last_patrol_unix_ms"], "retention 巡检时刻已记")
	require.Equal(t, float64(1), got["retention"]["last_dropped_log_partitions"], "DROP 分区数")
	require.GreaterOrEqual(t, got["scheduler"]["writeback_cap"].(float64), float64(4096))
	require.NotZero(t, got["rule-engine"]["queue_cap"].(float64))
	// stats-agg 观测（spec 2026-08-14 §6）：watermark 与 stats_agg_watermark
	// 表真实值一致（停摆冻结后 GET 的快照值 == 读表值——见上文前置比对）；
	// 上轮耗时已观测（首轮聚合完成必有耗时）。
	require.Equal(t, float64(wm.UnixMilli()), got["stats-agg"]["watermark_unix_ms"], "watermark 观测 = stats_agg_watermark 表真实值")
	require.Positive(t, got["stats-agg"]["last_duration_ms"].(float64), "上轮耗时已观测")

	// 快照区 Status 同步：5 条、全部已首刷、无错误。
	require.Len(t, resp.Snapshots, 5)
	for _, s := range resp.Snapshots {
		require.False(t, s.LastReload.IsZero(), "%s 已首刷", s.Name)
		require.Empty(t, s.LastError, "%s 首刷无错误", s.Name)
	}
	require.False(t, resp.GeneratedAt.IsZero())

	// --- 5) 指标回检真实 DB：err_logs 恰 2 行；retention DROP 后分区消失；
	// rec.Close 排空 → usage_logs 恰 5 行（pending 归零 = 真实落库） ---
	countRows := func(table string) int {
		var n int
		require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n))
		return n
	}
	require.Equal(t, 2, countRows("err_logs"), "inserted 指标 = err_logs 真实行数")
	// 已 DROP 的分区表不存在（pg_tables 计数 = 0——DROP 计数与真实一致）。
	var stalePartitions int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT count(*) FROM pg_tables WHERE schemaname = current_schema() AND tablename = $1",
		"usage_logs_"+stale.UTC().Format("20060102")).Scan(&stalePartitions))
	require.Zero(t, stalePartitions, "DROP 后过期分区已不存在（DROP 计数 = 真实）")

	require.NoError(t, rec.Close(ctx), "rec.Close 真实排空 pending")
	require.Zero(t, rec.Stats().(usage.RecorderStats).PendingLogs, "排空后 pending 归零")
	require.Equal(t, 5, countRows("usage_logs"), "usage_logs 真实行数 = 5（pending → 落库）")
}
