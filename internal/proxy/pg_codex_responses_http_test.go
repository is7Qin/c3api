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
	"github.com/tidwall/gjson"

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

// 真实 PG e2e（T6 happy path——"真实凭据"= 凭据材料真实落库 account_ext，经
// LoadGroupsAccounts 快照 → Selection.Ext → AccountCredential 派生直供适配层；
// 上游为本地 mock SSE 面——真实上游不可控）：
//
//	TEST_DATABASE_URL=postgres://postgres:c3api@127.0.0.1:15432/c3api_test \
//	  go test ./internal/proxy/ -run TestCodexResponsesHTTPBillingPG -v
//
// 独立 schema（c3api_test_t6）避共享库并发踩踏。断言面：SDK 合成体透传 +
// usage 顶层五计数计费落库（cost 500 毫分）+ cred 传递（Bearer pat-pg-1 直供
// 适配层）。

const codexHTTPPGTestSchema = "c3api_test_t6"

func TestCodexResponsesHTTPBillingPG(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + codexHTTPPGTestSchema
	} else {
		dsn += "?search_path=" + codexHTTPPGTestSchema
	}
	ctx := context.Background()
	pool, err := repository.OpenPG(ctx, dsn, 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+codexHTTPPGTestSchema+` CASCADE; CREATE SCHEMA `+codexHTTPPGTestSchema+`;`)
	require.NoError(t, err)
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), true)
	require.NoError(t, err)
	require.NoError(t, repos.EnsureUsageLogPartitioned(ctx, time.Now()))
	require.NoError(t, repos.EnsureErrLogPartitioned(ctx, time.Now()))

	// 本地 mock 上游（SDK HTTP 面——/v1/responses SSE 流）
	up, upc := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{t6RespCreated, t6RespItemEv, t6RespDone}})
	defer up.Close()

	// 落库数据：codex-pat 模板 + 组 + 账号 + account_ext（PAT 凭据 + 伪装身份
	// 四元组持久化——META-2 断言面：HTTP 面注入 client_metadata）
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
		AccountID: acc.ID, CredentialType: credential.TypeCodexPAT, CodexPATKey: strPtrPG("pat-pg-1"),
		CodexIdentity: &domain.CodexIdentity{
			InstallationID: "inst-pg-1",
			SessionID:      "sess-pg-1",
			ThreadID:       "thread-pg-1",
			WindowID:       "thread-pg-1:0",
		},
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

	// 计费钩子：价格快照 + 余额快照 + flusher（DeductAndLog → PG）
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
		Resolver: &fakePriceLookup{entries: map[string]*domain.PriceEntry{"gpt-4o": proxyPricingEntry()}, variants: map[string][]*domain.PriceVariant{"gpt-4o": proxyPricingVariants()}},
		Balances: bal,
		Flusher:  f,
	}, nil)
	p.SetCodex(sdkbridge.NewCodex(nil))
	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	// 一次完整非流式 resp 请求（真实凭据链路）
	resp := postResponses(t, srv, `{"model":"gpt-4o","input":"hi"}`)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	want := `{"id":"resp_t6","object":"response","status":"completed","output":[` + t6RespItem + `],"usage":` + t6RespUsage + `}`
	require.Equal(t, want, string(b), "合成体透传（真实凭据 e2e）")
	require.Equal(t, "Bearer pat-pg-1", upc.auth(0), "PAT 凭据落库 → 派生直供适配层")
	require.Equal(t, "gpt-4o", gjson.GetBytes(upc.body(0), "model").String(), "未映射 → 模型不改写")
	if !gjson.GetBytes(upc.body(0), "stream").Bool() {
		t.Fatalf("非流式 wire 必须 stream:true（SDK 注入）, body = %s", upc.body(0))
	}

	// META-2：伪装身份注入——上游收到 client_metadata（account_ext 持久化值 →
	// 快照 → codexIdentityFromExt → SDK 注入）：恒 4 key + turn_id UUIDv7
	//（spec 2026-08-15 验收面）
	cm := gjson.GetBytes(upc.body(0), "client_metadata")
	require.Equal(t, "inst-pg-1", cm.Get("x-codex-installation-id").String(), "installation_id（ext 持久化）注入")
	require.Equal(t, "sess-pg-1", cm.Get("session_id").String(), "session_id（ext 持久化）注入")
	require.Equal(t, "thread-pg-1", cm.Get("thread_id").String(), "thread_id（ext 持久化）注入")
	require.Equal(t, "thread-pg-1:0", cm.Get("x-codex-window-id").String(), "window_id（ext 持久化）注入")
	require.True(t, isUUIDv7(cm.Get("turn_id").String()), "turn_id UUIDv7 格式（SDK 自动生成）")

	// flusher 排空（单事务 DeductAndLog 落库）后断言 usage_logs 行——顶层 usage
	// 五计数 + cost（10×1e7+20×2e7 = 500 毫分；cache 价 nil → cache 分量 0 成本）
	require.NoError(t, f.Close(context.Background()))
	var (
		it, ot, tt, cr, cc, cost int64
		format, et, model        string
	)
	err = db.QueryRowContext(ctx, `SELECT input_tokens, output_tokens, total_tokens,
		cache_read_tokens, cache_creation_tokens, cost, format, error_type, model
		FROM usage_logs WHERE format = 'openai-responses' ORDER BY id DESC LIMIT 1`).
		Scan(&it, &ot, &tt, &cr, &cc, &cost, &format, &et, &model)
	require.NoError(t, err, "usage_logs 必须有 resp 计费行")
	require.Equal(t, int64(10), it, "input_tokens（顶层 usage 提取）")
	require.Equal(t, int64(20), ot, "output_tokens")
	require.Equal(t, int64(30), tt, "total_tokens")
	require.Equal(t, int64(2), cr, "cache_read_tokens")
	require.Equal(t, int64(4), cc, "cache_creation_tokens")
	require.Equal(t, int64(500), cost, "10×1e7+20×2e7 每 M 毫分 = 500")
	require.Equal(t, "openai-responses", format)
	require.Equal(t, "none", et)
	require.Equal(t, "gpt-4o", model)
}
