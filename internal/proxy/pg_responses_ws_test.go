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
	"github.com/is7qin/c3api/internal/usage"
)

// 真实 PG 计费 e2e（与 repository/pricing 包同款约定）：
//
//	TEST_DATABASE_URL=postgres://postgres:c3api@localhost:15432/c3api_test \
//	  go test ./internal/proxy/ -run TestResponsesWSBillingPG -v
//
// 未设置 TEST_DATABASE_URL → t.Skip。每测试重建独立 schema（proxy_ws_test）：
// 与 repository 包的 PG 测试（DROP public schema）隔离——两包测试并发跑不互踩。

const wsPGTestSchema = "proxy_ws_test"

// TestResponsesWSBillingPG resp-ws 全链路计费落库：WS 请求 → usage 嗅探 →
// finish → applyBilling（价格快照 + 倍率）→ routeLog 单写点（F2，spec §一）→
// rec → InsertBatch 落库 → 断言 usage_logs 5 计数 + cost + 格式 + Billed 出生标记。
// 5 计数：input 3 / output 5 / total 8 / cache_read 1 / cache_creation 3；
// cost = 3×1e7 + 5×2e7 每 M 毫分 = 130 毫分（缓存分量无价不参与计费）。
func TestResponsesWSBillingPG(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + wsPGTestSchema
	} else {
		dsn += "?search_path=" + wsPGTestSchema
	}
	ctx := context.Background()
	pool, err := repository.OpenPG(ctx, dsn, 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+wsPGTestSchema+` CASCADE; CREATE SCHEMA `+wsPGTestSchema+`;`)
	require.NoError(t, err)
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), true)
	require.NoError(t, err)
	// usagelog 已从 ent migrate 排除——分区表基座由 bootstrap 独占建表（与
	// repository 包 PG 测试同款基座约定）；缺表则 rec 的 InsertBatch 落入
	// 42P01/挂死路径，必须先建。
	require.NoError(t, repos.EnsureUsageLogPartitioned(ctx, time.Now()))

	// 计费钩子：价格快照 + 余额快照（用户 1 余额充足）；单写点：billable 行经
	// rec → repos.Usages 直落 usage_logs（F2：无 flusher 分流）。
	bal := billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{1: 1_000_000}}, nil)
	require.NoError(t, bal.Reload(ctx), "余额快照加载")
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, repos.Usages, nil)
	t.Cleanup(func() { _ = rec.Close(context.Background()) })

	up := fakeResponsesWS(t, &fakeWSHooks{frameLimit: 1})
	defer up.Close()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS},
		Models:           []string{"gpt-4o"},
	}
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, noopLogStore{}, &BillingHooks{
		Prices:   &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}},
		Balances: bal,
	})
	p.cfg.BillingCapture = true // 余额预检生效 + Billed=false 出生待对账（spec §一）
	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer srv.Close()

	// 一次完整 WS 会话（上游消费 1 帧后正常关闭）
	c := dialResponsesWS(t, srv)
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	for i := 0; i < 4; i++ {
		_ = readResponsesWSFrame(t, c)
	}
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	// rec 排空（InsertBatch 落库）后断言 usage_logs 行。
	// usage_logs 瘦身（分表设计）：status_code 已移除（错误审计归 err_logs）——
	// 成功计费行 status 语义由 error_type=none 承载。
	require.NoError(t, rec.Close(ctx))
	var (
		it, ot, tt, cr, cc, cost int64
		format, et, model        string
		billed                   bool
	)
	err = db.QueryRowContext(ctx, `SELECT input_tokens, output_tokens, total_tokens,
		cache_read_tokens, cache_creation_tokens, cost, format, error_type, model, billed
		FROM usage_logs WHERE format = 'openai-responses-ws' ORDER BY id DESC LIMIT 1`).
		Scan(&it, &ot, &tt, &cr, &cc, &cost, &format, &et, &model, &billed)
	require.NoError(t, err, "usage_logs 必须有 resp-ws 计费行")
	require.Equal(t, int64(3), it, "input_tokens")
	require.Equal(t, int64(5), ot, "output_tokens")
	require.Equal(t, int64(8), tt, "total_tokens")
	require.Equal(t, int64(1), cr, "cache_read_tokens")
	require.Equal(t, int64(3), cc, "cache_creation_tokens")
	require.Equal(t, int64(130), cost, "3×1e7+5×2e7 每 M 毫分 = 130")
	require.Equal(t, "openai-responses-ws", format)
	require.Equal(t, "none", et)
	require.Equal(t, "gpt-4o", model)
	require.False(t, billed, "capture on + 有用户 → 出生待对账（spec §一）")
}

// TestResponsesWSBillingTierPG resp-ws service_tier 计费落库（真实 PG）：WS 首帧
// 带 service_tier=fast → usage_logs.billing_tier="fast" + cost 按 fast 倍率
// （260 ≠ auto 130）——同请求 HTTP 按档计费、WS 恒 auto 的金额错收修复钉死。
func TestResponsesWSBillingTierPG(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + wsPGTestSchema
	} else {
		dsn += "?search_path=" + wsPGTestSchema
	}
	ctx := context.Background()
	pool, err := repository.OpenPG(ctx, dsn, 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+wsPGTestSchema+` CASCADE; CREATE SCHEMA `+wsPGTestSchema+`;`)
	require.NoError(t, err)
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), true)
	require.NoError(t, err)
	require.NoError(t, repos.EnsureUsageLogPartitioned(ctx, time.Now()))

	bal := billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{1: 1_000_000}}, nil)
	require.NoError(t, bal.Reload(ctx), "余额快照加载")
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, repos.Usages, nil)
	t.Cleanup(func() { _ = rec.Close(context.Background()) })

	up := fakeResponsesWS(t, &fakeWSHooks{frameLimit: 1})
	defer up.Close()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS},
		Models:           []string{"gpt-4o"},
	}
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, noopLogStore{}, &BillingHooks{
		Prices:   &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}},
		Balances: bal,
	})
	p.cfg.BillingCapture = true // 余额预检生效 + Billed=false 出生待对账（spec §一）
	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer srv.Close()

	c := dialResponsesWS(t, srv)
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","service_tier":"fast","input":"hi"}`)))
	for i := 0; i < 4; i++ {
		_ = readResponsesWSFrame(t, c)
	}
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	require.NoError(t, rec.Close(ctx))
	var cost int64
	var billingTier string
	err = db.QueryRowContext(ctx, `SELECT cost, billing_tier
		FROM usage_logs WHERE format = 'openai-responses-ws' ORDER BY id DESC LIMIT 1`).
		Scan(&cost, &billingTier)
	require.NoError(t, err, "usage_logs 必须有 resp-ws fast 档计费行")
	require.Equal(t, "fast", billingTier, "BillingTier=fast 落库（WS 按档计费）")
	require.Equal(t, int64(260), cost, "fast ×2.0：130×2 = 260（≠ auto 130）")
}
