// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"os"
	"strings"
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
	"github.com/is7qin/c3api/internal/sdkbridge"
)

// 真实 PG 集成：fatal 标记全链路（T5 §2——OnAuthFatal → 统一回调 →
// failed_at + last_error + StatusDisabled 持久化 → 重启快照重载仍摘除 →
// 管理面恢复 status→active 双清 + 恢复调度）。
//
//	TEST_DATABASE_URL=postgres://postgres:c3api@127.0.0.1:15432/c3api_test_t5 \
//	  go test ./internal/proxy/ -run TestCodexFatalChainPG -v
//
// 独立 schema 与既有 PG 测试隔离（proxy_codex_ws_test 等）；依赖真实 refresh
// mock（CODEX_REFRESH_TOKEN_URL_OVERRIDE）——本地可编程面，真实凭据语义
//（落库 account_ext → 快照 → AccountCredential 派生直供适配层）。

const codexFatalPGTestSchema = "proxy_codex_fatal_test"

func TestCodexFatalChainPG(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + codexFatalPGTestSchema
	} else {
		dsn += "?search_path=" + codexFatalPGTestSchema
	}
	ctx := context.Background()
	pool, err := repository.OpenPG(ctx, dsn, 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+codexFatalPGTestSchema+` CASCADE; CREATE SCHEMA `+codexFatalPGTestSchema+`;`)
	require.NoError(t, err)
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), true)
	require.NoError(t, err)
	require.NoError(t, repos.EnsureUsageLogPartitioned(ctx, time.Now()))
	require.NoError(t, repos.EnsureErrLogPartitioned(ctx, time.Now()))

	// 落库：codex-oauth 模板 + 组 + 账号 + account_ext（oauth 凭据 + 身份四元组）
	tpl, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "codex-tpl", BaseURL: "https://u/v1",
		CredentialType:   credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS},
		Models:           []string{"gpt-4o"},
	})
	require.NoError(t, err)
	g, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "g", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	acc, err := repos.Accounts.CreateAccount(ctx, &domain.Account{
		Name: "codex-acc", TemplateID: tpl.ID, UpstreamKey: "sk-x", Weight: 100, MaxConcurrency: 4,
	})
	require.NoError(t, err)
	require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{g.ID}))
	const iid = "11111111-2222-3333-4444-555555555555"
	_, err = repos.AccountExts.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
		CodexIdentity: &domain.CodexIdentity{
			InstallationID: iid, SessionID: "s", ThreadID: "t", WindowID: "t:0",
		},
		CodexOAuthToken: strPtrPG("at-1"), CodexOAuthRefreshToken: strPtrPG("rt-1"),
	})
	require.NoError(t, err)

	// 可编程上游：images 端点 401 非判死（触发 SDK 自动轮转）+ refresh 端点
	// 判死（401 invalid_grant → RefreshOAuthError fatal）
	up, _ := newCodexImageUpstream(t, codexUpStep{status: 401, body: `{"error":{"code":"token_expired"}}`})
	codexRefreshMock(t, 401, `{"error":"invalid_grant"}`)

	// 真实失效链：适配层（统一回调）→ T1 HandleFailure（SetAccountFailed 直写
	// PG + FailAccount 快照摘除 + 经 writebackLoop 落库 status=disabled）
	re := rule.New(rule.Config{}, repos.Rules, nil)
	require.NoError(t, re.Reload(ctx))
	sched := scheduler.New(scheduler.Config{DefaultMaxConcurrency: 4, SyncInterval: time.Hour}, repos.Groups, re, nil)
	require.NoError(t, sched.InvalidateAllSync())
	sctx, scancel := context.WithCancel(ctx)
	require.NoError(t, sched.Start(sctx))
	t.Cleanup(scancel)
	failure := sdkbridge.NewFailureHandler(sdkbridge.FailureDeps{Store: repos.Accounts, Failer: sched, Log: nil})
	adapter := sdkbridge.NewCodex(failure)

	// 触发：fatal（refresh 判死）→ 统一回调全链路
	ext, err := repos.AccountExts.GetAccountExt(ctx, acc.ID)
	require.NoError(t, err)
	cred := domain.CredentialFromExt(ext)
	cred.BaseURL = up.URL + "/images/generations"
	_, err = adapter.GenerateImage(ctx, &cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "refresh 被拒绝", "RefreshOAuthError 透传")

	// ① 失效字段落库（HandleFailure 直写同步）：failed_at + last_error 留痕
	got, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.NotNil(t, got.FailedAt, "failed_at 落库")
	require.NotNil(t, got.LastError, "last_error 留痕（失效原因摘要）")
	require.Contains(t, *got.LastError, "invalid_grant")
	require.LessOrEqual(t, len(*got.LastError), domain.ErrMsgMaxLen, "域内截断 500 生效")

	// ② status=disabled 持久化（FailAccount → writebackLoop 落库——异步，等待）
	require.Eventually(t, func() bool {
		row, err := repos.Accounts.GetAccount(ctx, acc.ID)
		return err == nil && row.Status == domain.StatusDisabled
	}, 3*time.Second, 20*time.Millisecond, "调度摘除必须持久化（重启快照重载后仍摘除）")

	// ③ 重启等价：快照全量重建 → 仍摘除（pickFrom 跳 disabled）
	require.NoError(t, sched.InvalidateAllSync())
	ri, ok := sched.Runtime(acc.ID)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status, "重启快照重载后仍摘除")
	_, err = sched.Select(g.ID, domain.FormatOpenAIResponsesWS, "gpt-4o")
	require.ErrorIs(t, err, scheduler.ErrNoAvailable, "失效账号不可调度")

	// ④ 失效恢复（管理面）：UpdateAccount status→active → failed_at + last_error
	// 双清（P3-4 恢复断言）+ 调度恢复 active 重服务
	cur, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	cur.Status = domain.StatusActive
	_, err = repos.Accounts.UpdateAccount(ctx, cur, nil)
	require.NoError(t, err)
	got2, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Nil(t, got2.FailedAt, "failed_at 双清")
	require.Nil(t, got2.LastError, "last_error 双清")
	require.Equal(t, domain.StatusActive, got2.Status)
	require.NoError(t, sched.InvalidateAllSync(), "管理面恢复 → 组级重载恢复调度")
	sel, err := sched.Select(g.ID, domain.FormatOpenAIResponsesWS, "gpt-4o")
	require.NoError(t, err, "恢复调度：账号重新可被选中")
	require.Equal(t, acc.ID, sel.AccountID)
	ri2, ok := sched.Runtime(acc.ID)
	require.True(t, ok)
	require.Equal(t, domain.StatusActive, ri2.Status, "快照恢复 active")
}
