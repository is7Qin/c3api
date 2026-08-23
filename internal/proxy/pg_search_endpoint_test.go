// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
)

// 真实 PG e2e（search 端点 happy path——"真实凭据" = 凭据材料真实落库
// account_ext，经 LoadGroupsAccounts 快照 → Selection.Ext → AccountCredential
// 派生直供适配层；上游为本地 mock——真实上游不可控）：
//
//	TEST_DATABASE_URL=postgres://postgres:c3api@127.0.0.1:15432/c3api_test \
//	  go test ./internal/proxy/ -run TestSearchEndpointBillingPG -v
//
// 独立 schema（c3api_test_search）避共享库并发踩踏。断言面：SDK Search 透传
// （opaque 响应原样）+ 2xx 按次计费落库（format=openai-search + call_count=1
// + price_per_call_millis=1000 + cost=1000——applyBilling search 分支）+ cred
// 传递（Bearer pat-pg-s 直供适配层）。

const searchPGTestSchema = "c3api_test_search"

func TestSearchEndpointBillingPG(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + searchPGTestSchema
	} else {
		dsn += "?search_path=" + searchPGTestSchema
	}
	ctx := context.Background()
	pool, err := repository.OpenPG(ctx, dsn, 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+searchPGTestSchema+` CASCADE; CREATE SCHEMA `+searchPGTestSchema+`;`)
	require.NoError(t, err)
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), true)
	require.NoError(t, err)
	require.NoError(t, repos.EnsureUsageLogPartitioned(ctx, time.Now()))
	require.NoError(t, repos.EnsureErrLogPartitioned(ctx, time.Now()))

	// 本地 mock 上游（/v1/alpha/search——SDK 派生端点）
	up, upc := newCodexSearchUpstream(t, codexSearchStep{status: 200, body: searchRespRaw})
	defer up.Close()

	// 落库数据：codex-pat 模板 + 组 + 账号 + account_ext（PAT）
	tpl, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "codex-tpl", BaseURL: up.URL,
		CredentialType:   credential.TypeCodexPAT,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
		Models:           []string{"gpt-4o"},
	})
	require.NoError(t, err)
	g, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "g", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	acc, err := repos.Accounts.CreateAccount(ctx, &domain.Account{
		Name: "codex-acc", TemplateID: tpl.ID, Weight: 100, MaxConcurrency: 4,
	})
	require.NoError(t, err)
	require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{g.ID}))
	_, err = repos.AccountExts.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: acc.ID, CredentialType: credential.TypeCodexPAT, CodexPATKey: strPtrPG("pat-pg-s"),
	})
	require.NoError(t, err)

	// 调度器接真实 loader（快照含 Ext eager-load；请求期零 DB）
	re := rule.New(rule.Config{}, repos.Rules, nil)
	sched := scheduler.New(scheduler.Config{DefaultMaxConcurrency: 4, SyncInterval: time.Hour}, repos.Groups, re, nil)
	require.NoError(t, sched.InvalidateAllSync())

	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{
		"ck-1": activeKey(1, 1, g.ID),
	}}, noopUserLoader{}, nil)
	require.NoError(t, auth.Reload(context.Background()))

	// 计费钩子：价格快照 + 按单元价快照 + 余额快照 + flusher（DeductAndLog → PG）
	bal := billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{1: 1_000_000}}, nil)
	require.NoError(t, bal.Reload(ctx), "余额快照加载")
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, noopLogStore{}, nil)
	f := billing.NewFlusher(billing.FlushConfig{
		FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour,
	}, repos.Billing, rec, bal, nil)
	t.Cleanup(func() { _ = f.Close(context.Background()) })
	hc := &http.Client{Transport: http.DefaultTransport}
	clients := aiclient.NewFactory(hc, aiclient.Config{
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
	})
	p := New(Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
		GroupKeyRPM:           0, UsageCapture: true, BillingCapture: true,
	}, sched, credential.New(), rec, clients, auth, nil, &BillingHooks{
		Resolver: &fakeFunctionPriceLookup{entries: map[string]*domain.PriceEntry{}},
		Balances:       bal,
		Flusher:        f,
	}, nil)
	p.SetCodex(sdkbridge.NewCodex(nil))
	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	// 一次完整 search 请求（真实凭据链路：SDK Search 透传）
	resp := postSearch(t, srv, searchReqBody, "")
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body=%s", string(b))
	require.Equal(t, searchRespRaw, string(b), "opaque 响应原样透传（真实凭据 e2e）")
	require.Equal(t, 1, upc.callsN())
	require.Equal(t, "/v1/alpha/search", upc.path(0), "SDK Search 派生打 /alpha/search 非 /responses")
	require.Equal(t, "Bearer pat-pg-s", upc.auth(0), "PAT 凭据落库 → 派生直供适配层")
	require.Equal(t, searchReqBody, string(upc.body(0)), "请求体原样送达上游")

	// flusher 排空（单事务 DeductAndLog 落库）后断言 usage_logs 行——search
	// 按次计费（format=openai-search + call_count=1 + price_per_call_millis
	// 1000 默认兜底 + cost=1000）
	require.NoError(t, f.Close(context.Background()))
	var (
		callCount, pricePerCall, cost int64
		format, et, model             string
	)
	err = db.QueryRowContext(ctx, `SELECT call_count, price_per_call_millis, cost, format, error_type, model
		FROM usage_logs WHERE format = 'openai-search' ORDER BY id DESC LIMIT 1`).
		Scan(&callCount, &pricePerCall, &cost, &format, &et, &model)
	require.NoError(t, err, "usage_logs 必须有 openai-search 计费行（COPY 落库）")
	require.Equal(t, int64(1), callCount, "call_count=1（按次计费）")
	require.Equal(t, int64(1000), pricePerCall, "price_per_call_millis = GetFunctionPrice 默认兜底 1000 毫分")
	require.Equal(t, int64(1000), cost, "cost = 按次价 × call_count（非 0 计费断言）")
	require.Equal(t, "openai-search", format)
	require.Equal(t, "none", et)
	require.Equal(t, "gpt-4o", model)
}
