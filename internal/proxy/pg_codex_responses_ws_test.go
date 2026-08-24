// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/coder/websocket"
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

// 真实 PG e2e（T4 happy path——"真实凭据"= 凭据材料真实落库 account_ext，
// 经 LoadGroupsAccounts 快照 → Selection.Ext → AccountCredential 派生直供适
// 配层；上游为本地 mock WS 面——真实上游不可控，P3-5 分工）：
//
//	TEST_DATABASE_URL=postgres://postgres:c3api@127.0.0.1:15432/c3api_test_t4 \
//	  go test ./internal/proxy/ -run TestCodexResponsesWSBillingPG -v
//
// 断言面：WS 握手 / 双向透传 / usage 嗅探计费与 aiclient 路径（
// TestResponsesWSBillingPG）**逐字节一致**——usage_logs 行 5 计数 + cost 130
// + format/error_type/model 全同。独立 schema 与 repository 包隔离。

const codexWSPGTestSchema = "proxy_codex_ws_test"

func strPtrPG(s string) *string { return &s }

func TestCodexResponsesWSBillingPG(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + codexWSPGTestSchema
	} else {
		dsn += "?search_path=" + codexWSPGTestSchema
	}
	ctx := context.Background()
	pool, err := repository.OpenPG(ctx, dsn, 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+codexWSPGTestSchema+` CASCADE; CREATE SCHEMA `+codexWSPGTestSchema+`;`)
	require.NoError(t, err)
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), true)
	require.NoError(t, err)
	require.NoError(t, repos.EnsureUsageLogPartitioned(ctx, time.Now()))
	require.NoError(t, repos.EnsureErrLogPartitioned(ctx, time.Now()))

	// 本地 mock 上游（SDK 拨号面；3 帧后 1000 关闭——事件流 + 回声）
	up, hooks := newCodexWSUpstream(t, []int{200}, 3)
	defer up.Close()

	// 落库数据：codex-pat 模板 + 组 + 账号 + account_ext（PAT + 身份四元组）
	tpl, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "codex-tpl", BaseURL: up.URL,
		CredentialType:   credential.TypeCodexPAT,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS},
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
	const iid = "11111111-2222-3333-4444-555555555555"
	sess, thread, win := "s-sess-1", "t-thread-1", "t-thread-1:0"
	_, err = repos.AccountExts.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: acc.ID, CredentialType: credential.TypeCodexPAT,
		CodexIdentity: &domain.CodexIdentity{
			InstallationID: iid, SessionID: sess, ThreadID: thread, WindowID: win,
		},
		CodexPATKey: strPtrPG("pat-pg-1"),
	})
	require.NoError(t, err)

	// 调度器接真实 loader（repos.Groups——LoadGroupsAccounts 快照含 Ext
	// eager-load；请求期零 DB）
	re := rule.New(rule.Config{}, repos.Rules, nil)
	sched := scheduler.New(scheduler.Config{DefaultMaxConcurrency: 4, SyncInterval: time.Hour}, repos.Groups, re, nil)
	require.NoError(t, sched.InvalidateAllSync())

	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{
		"ck-1": activeKey(1, 1, g.ID),
	}}, noopUserLoader{}, nil)
	require.NoError(t, auth.Reload(context.Background()))

	// 计费钩子：价格快照 + 余额快照；单写点：billable 行经 rec → repos.Usages
	// 直落 usage_logs（F2：无 flusher 分流）。
	bal := billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{1: 1_000_000}}, nil)
	require.NoError(t, bal.Reload(ctx), "余额快照加载")
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, repos.Usages, nil)
	t.Cleanup(func() { _ = rec.Close(context.Background()) })
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
	}, nil)
	p.SetCodex(sdkbridge.NewCodex(nil))
	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer srv.Close()

	// 一次完整 WS 会话（与 aiclient 路径 e2e 同帧：3 帧 → 事件流 + 回声 →
	// 1000 关闭；伪装四元组帧级注入断言）
	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	for _, f := range []string{
		`{"type":"response.create","model":"gpt-4o","input":"hi"}`,
		`{"type":"response.input_text.delta","delta":"typing"}`,
		`{"type":"custom.mid","n":42}`,
	} {
		require.NoError(t, c.Write(context.Background(), websocket.MessageText, []byte(f)))
	}
	for i := 0; i < 6; i++ {
		_ = readResponsesWSFrame(t, c)
	}
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	// 握手头：伪装四元组 = 落库 ext 身份（真实凭据链路）
	hooks.mu.Lock()
	require.Equal(t, 1, hooks.upgrades, "真实凭据单拨成功")
	require.Equal(t, "Bearer pat-pg-1", hooks.headers[0].Get("Authorization"), "PAT 直供适配层")
	require.Equal(t, sess, hooks.headers[0].Get("Session-Id"))
	require.Equal(t, thread, hooks.headers[0].Get("Thread-Id"))
	require.Equal(t, win, hooks.headers[0].Get("X-Codex-Window-Id"))
	hooks.mu.Unlock()

	// rec 排空（InsertBatch 落库）后断言 usage_logs 行——与
	// TestResponsesWSBillingPG（aiclient 路径）逐字节一致。
	require.NoError(t, rec.Close(ctx))
	var (
		it, ot, tt, cr, cc, cost int64
		format, et, model        string
	)
	err = db.QueryRowContext(ctx, `SELECT input_tokens, output_tokens, total_tokens,
		cache_read_tokens, cache_creation_tokens, cost, format, error_type, model
		FROM usage_logs WHERE format = 'openai-responses-ws' ORDER BY id DESC LIMIT 1`).
		Scan(&it, &ot, &tt, &cr, &cc, &cost, &format, &et, &model)
	require.NoError(t, err, "usage_logs 必须有 resp-ws 计费行")
	require.Equal(t, int64(3), it, "input_tokens（与 aiclient 路径逐字节一致）")
	require.Equal(t, int64(5), ot, "output_tokens")
	require.Equal(t, int64(8), tt, "total_tokens")
	require.Equal(t, int64(1), cr, "cache_read_tokens")
	require.Equal(t, int64(3), cc, "cache_creation_tokens")
	require.Equal(t, int64(130), cost, "3×1e7+5×2e7 每 M 毫分 = 130")
	require.Equal(t, "openai-responses-ws", format)
	require.Equal(t, "none", et)
	require.Equal(t, "gpt-4o", model)
}
