// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

// 真实 PG 端到端（spec 2026-11-18 retry-observability §6 的 S2 验收 6/4）：
// 让 **完整写路径**（failover 循环 → attempt_logs 写点 → usage.AttemptLogWorker →
// repository.AttemptLogRepo → PG，以及同一条路上的 err_logs）真的落库。
//
// 纪律：**禁止手搓行**——所有断言读到的行都是 HandleChat 跑出来的。否则「列可空」
// 「repo 映射」这些只在真实写入时才会暴露的错都能被夹具掩盖。
//
//	先导出 PG 测试 DSN（见 pgtest 包文档）
//	  go test ./internal/proxy/ -run TestAttemptLogsWritePathPG -v

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
)

// TestAttemptLogsWritePathPG 2 次失败（5xx）耗尽 + 一次本地 401 拒绝：
//   - attempt_logs：恰好 2 行，ordinal ∈ {1,2}，两行不同账号；
//   - err_logs：耗尽行带 ordinal（= 最后一次尝试 2）；401 拒绝行 ordinal IS NULL。
func TestAttemptLogsWritePathPG(t *testing.T) {
	dsn := pgtest.Clone(t)
	ctx := context.Background()
	pool := pgtest.OpenPool(t, dsn)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), false)
	require.NoError(t, err)

	now := time.Now()
	// 迁移只建父表；分区由「唯一入口」补齐（空库直接插会整体失败——S2a 已锚）。
	require.NoError(t, repos.EnsureAllTablePartitions(ctx, now, now.AddDate(0, 0, 1)))

	// 上游恒 500：两次尝试 ⇒ 耗尽。
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	t.Cleanup(up.Close)

	// 两个同格式账号落在同一组：publishTestRoutes 会按模型发布 Primary=[1,2] 的决策。
	g, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "g", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	for i := int64(1); i <= 2; i++ {
		suffix := fmt.Sprintf("%d", i)
		tpl, terr := repos.Templates.CreateTemplate(ctx, &domain.Template{
			Name: "t" + suffix, BaseURL: up.URL,
			CredentialType:   credential.TypeAPIKey,
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
			Models:           []string{"gpt-4o"},
			ModelMapping:     domain.ModelMapping{},
		})
		require.NoError(t, terr)
		acc, aerr := repos.Accounts.CreateAccount(ctx, &domain.Account{
			Name: "a" + suffix, TemplateID: tpl.ID, UpstreamKey: "sk-upstream",
			MaxConcurrency: 4, Enabled: true,
		})
		require.NoError(t, aerr)
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{g.ID}))
	}

	re := rule.New(rule.Config{}, repos.Rules, nil, nil, nil)
	require.NoError(t, re.Reload(ctx))
	sched := scheduler.New(scheduler.Config{SyncInterval: time.Hour}, repos.Groups, re, nil, nil, nil, nil)
	require.NoError(t, sched.InvalidateAllSync())
	publishTestRoutes(t, sched)

	rec := usage.New(usage.UsageConfig{BatchSize: 100, FlushInterval: time.Hour, QuotaFlushInterval: time.Hour}, repos.Usages, nil)
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, FlushInterval: time.Hour}, repos.ErrLogs, nil)
	attemptW, err := usage.NewAttemptLogWorker(usage.AttemptLogConfig{}, repos.AttemptLogs, nil)
	require.NoError(t, err)

	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{"ck-1": activeKey(1, 1, g.ID)}},
		noopUserLoader{}, nil, nil, true)
	require.NoError(t, auth.Reload(ctx))
	px := New(Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second,
		UsageCapture: true,
	}, sched, credential.New(), rec,
		aiclient.NewFactory(&http.Client{Transport: http.DefaultTransport},
			aiclient.Config{UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second}),
		auth, nil, nil, errlogW, Deps{})
	px.SetAttemptLogs(attemptW)

	// ① 两次 5xx ⇒ 耗尽（这一跳既写 attempt_logs 两行，也写 err_logs 的耗尽行）。
	require.Equal(t, http.StatusBadGateway, chatOnce(px, "ck-1").Code)
	// ② 本地 401 拒绝（没有 attempt 语义）。
	require.Equal(t, http.StatusUnauthorized, chatOnce(px, "ck-invalid").Code)

	// 排空三条通道（全部是真实 worker 的 Close 排空，不做 sleep）。
	require.NoError(t, attemptW.Close(ctx))
	require.NoError(t, errlogW.Close(ctx))
	require.NoError(t, rec.Close(ctx))

	// attempt_logs：真实写入的两行。
	rows, err := pool.Query(ctx, `SELECT attempt_ordinal, account_id, status_code, outcome, error_type, commit_state, latency_ms
		FROM attempt_logs ORDER BY attempt_ordinal`)
	require.NoError(t, err)
	defer rows.Close()
	type attemptRow struct {
		ordinal    int16
		accountID  int64
		statusCode int
		outcome    string
		errorType  string
		commit     string
		latencyMS  int64
	}
	var got []attemptRow
	for rows.Next() {
		var r attemptRow
		require.NoError(t, rows.Scan(&r.ordinal, &r.accountID, &r.statusCode, &r.outcome, &r.errorType, &r.commit, &r.latencyMS))
		got = append(got, r)
	}
	require.NoError(t, rows.Err())
	require.Len(t, got, 2, "2 次失败 dispatch ⇒ attempt_logs 恰好 2 行")
	require.Equal(t, []int16{1, 2}, []int16{got[0].ordinal, got[1].ordinal}, "ordinal 来自 scheduler.Attempt")
	require.NotEqual(t, got[0].accountID, got[1].accountID, "failover 换号 ⇒ 两行不同账号")
	for _, r := range got {
		require.Equal(t, http.StatusInternalServerError, r.statusCode)
		require.Equal(t, "5xx", r.outcome)
		require.Equal(t, string(domain.Err5xx), r.errorType)
		require.Equal(t, "upstream_responded", r.commit)
		require.GreaterOrEqual(t, r.latencyMS, int64(0))
	}

	// err_logs 的 ordinal：耗尽行 = 最后一次尝试（2）；401 拒绝行必须 NULL。
	var exhausted, rejected int
	var exhaustedOrdinal int16
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*), COALESCE(min(attempt_ordinal), 0)
		FROM err_logs WHERE status_code = 500`).Scan(&exhausted, &exhaustedOrdinal))
	require.Equal(t, 1, exhausted, "耗尽只落一行终态错误（5xx 跳本身不写 err_logs）")
	require.EqualValues(t, 2, exhaustedOrdinal, "耗尽行必须带最后一次尝试的 ordinal")
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM err_logs
		WHERE status_code = 401 AND attempt_ordinal IS NULL`).Scan(&rejected))
	require.Equal(t, 1, rejected, "本地拒绝行 attempt_ordinal 必须 NULL（没有 attempt 语义）")

	// 反向守卫：拒绝行不得在 attempt_logs 里留下任何痕迹。
	var attemptRowsFor401 int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM attempt_logs WHERE status_code = 401`).Scan(&attemptRowsFor401))
	require.Zero(t, attemptRowsFor401, "本地拒绝不产生尝试行")
}
