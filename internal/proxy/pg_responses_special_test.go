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

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
)

// 真实 PG 集成（与 pg_responses_ws_test.go 同款约定）：
//
//	TEST_DATABASE_URL=postgres://postgres:c3api@127.0.0.1:15432/c3api_test \
//	  go test ./internal/proxy/ -run TestPGResponsesSpecialCredential -v
//
// 未设置 TEST_DATABASE_URL → t.Skip。独立 schema（proxy_rsp_special_test）：
// 与同包 pg_responses_ws_test 的 proxy_ws_test、repository 包的 public schema
// 测试并行跑均不互踩。

const rspSpecialPGTestSchema = "proxy_rsp_special_test"

// TestPGResponsesSpecialCredential responses-special 模板（主列
// credential_type）+ 账号 upstream_key → 真实 PG 快照 → Selection →
// credentialFor 取 key 成功（P4 502 消灭断言：修复前 TypeResponsesSpecial 未
// 注册 → For 兜底（现 unsupportedProvider，旧 apiKeyProvider）→ Credential
// 类型不匹配 ErrUnsupported → 真实流量 502 unsupported credential type；本断
// 言确定性红）。HTTP 补充：完整 resp-ws 会话正常闭环（上游校验 Authorization:
// Bearer <账号 key>）。
func TestPGResponsesSpecialCredential(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + rspSpecialPGTestSchema
	} else {
		dsn += "?search_path=" + rspSpecialPGTestSchema
	}
	ctx := context.Background()
	pool, err := repository.OpenPG(ctx, dsn, 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+rspSpecialPGTestSchema+` CASCADE; CREATE SCHEMA `+rspSpecialPGTestSchema+`;`)
	require.NoError(t, err)
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), true)
	require.NoError(t, err)

	up := fakeResponsesWS(t, &fakeWSHooks{frameLimit: 1})
	defer up.Close()
	tpl, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "t-rsp-special", BaseURL: up.URL,
		CredentialType:   credential.TypeResponsesSpecial,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS},
		Models:           []string{"gpt-4o"},
	})
	require.NoError(t, err, "模板主列 credential_type=responses-special 落库")
	g, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "g-rsp", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	acc, err := repos.Accounts.CreateAccount(ctx, &domain.Account{
		Name: "a-rsp", TemplateID: tpl.ID, UpstreamKey: "sk-upstream",
		Weight: 1, MaxConcurrency: 8,
	})
	require.NoError(t, err)
	require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{g.ID}))

	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background())) // 空表写种子（同 newTestProxyTplTimeoutRec）
	sched := scheduler.New(scheduler.Config{
		DefaultMaxConcurrency: 4, SyncInterval: time.Hour,
	}, repos.Groups, re, nil)
	require.NoError(t, sched.InvalidateAllSync())

	sel, err := sched.Select(g.ID, domain.FormatOpenAIResponsesWS, "gpt-4o")
	require.NoError(t, err, "responses-special 模板选号")
	require.Equal(t, credential.TypeResponsesSpecial, sel.CredentialType, "模板主列 credential_type 随快照下发")
	require.Equal(t, "sk-upstream", sel.UpstreamKey, "账号 upstream_key 随 Selection 携带")
	defer sched.Release(sel.AccountID)

	p := newPGTestProxy(t, sched, g.ID)

	// P4 502 消灭断言（确定性）：credentialFor 必须取到账号 key。
	cred, err := p.credentialFor(ctx, sel)
	require.NoError(t, err, "responses-special 凭据取用必须成功（修复前：未注册 → 502 unsupported credential type）")
	require.Equal(t, "sk-upstream", cred)
	require.Equal(t, credential.TypeResponsesSpecial, p.creds.For(credential.TypeResponsesSpecial).Type(),
		"注册表必须含 responses-special provider")

	// HTTP 补充：完整 resp-ws 会话正常闭环（fake 上游校验 Bearer 头=账号 key；
	// 凭据失败 → 错误事件帧 + 非正常关闭，本断言红）。
	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer srv.Close()
	c := dialResponsesWS(t, srv)
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	for i := 0; i < 4; i++ {
		_ = readResponsesWSFrame(t, c)
	}
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)
}

// newPGTestProxy 以外部注入的调度器构造测试代理（PG 快照链路）：auth key
// ck-1 → groupID；计费全关；用量 no-op（本测试只断言凭据路径）。
func newPGTestProxy(t *testing.T, sched *scheduler.Scheduler, groupID int64) *Proxy {
	t.Helper()
	cfg := Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
		GroupKeyRPM:           0, UsageCapture: true,
	}
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, noopLogStore{}, nil)
	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{
		"ck-1": activeKey(1, 1, groupID),
	}}, noopUserLoader{}, nil)
	require.NoError(t, auth.Reload(context.Background())) // 构造不再自载——测试显式首刷
	hc := &http.Client{Transport: http.DefaultTransport}
	clients := aiclient.NewFactory(hc, aiclient.Config{
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
	})
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, FlushInterval: time.Hour}, noopErrLogStore{}, nil)
	return New(cfg, sched, credential.New(), rec, clients, auth, nil, nil, errlogW)
}
