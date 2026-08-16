// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
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
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
)

// GET /v1/models 真实 PG 集成（与 pg_responses_special_test.go 同款约定）：
//
//	TEST_DATABASE_URL=postgres://postgres:c3api@127.0.0.1:15432/c3api_test \
//	  go test ./internal/proxy/ -run TestPGModels -v
//
// 未设置 TEST_DATABASE_URL → t.Skip。独立 schema（proxy_models_test）：与同包
// 其它 PG 测试并行跑均不互踩。

const modelsPGTestSchema = "proxy_models_test"

// modelsPGTestDB 打开独立 schema 的测试库并迁移建表。
func modelsPGTestDB(t *testing.T) *repository.Repository {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + modelsPGTestSchema
	} else {
		dsn += "?search_path=" + modelsPGTestSchema
	}
	ctx := context.Background()
	pool, err := repository.OpenPG(ctx, dsn, 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+modelsPGTestSchema+` CASCADE; CREATE SCHEMA `+modelsPGTestSchema+`;`)
	require.NoError(t, err)
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), true)
	require.NoError(t, err)
	return repos
}

// modelsFixture 种子产物（各测试用例共享）。
type modelsFixture struct {
	repos  *repository.Repository
	sched  *scheduler.Scheduler
	gid    int64 // g1：两模板跨格式账号
	gid2   int64 // g2：空组
	userID int64 // 余额 0 的种子用户（计费门禁误触发断言用）
}

// modelsPGFixture 种子：用户 + 组 g1（两模板跨格式）+ 空组 g2 + 三把 key
// （g1 / g2 / 幽灵组）。g1 可路由模型并集 = {gpt-4o, claude-3.5-sonnet,
// dall-e-3}——gpt-4o 跨模板（tplA/tplB）与跨格式（chat/responses/images）重复，
// 列表必须去重为 3 项。key → 组绑定走真实 PG keys 表（鉴权快照经 repos.Keys
// 加载——与生产同源）。
func modelsPGFixture(t *testing.T) modelsFixture {
	t.Helper()
	repos := modelsPGTestDB(t)
	ctx := context.Background()

	u, err := repos.Users.CreateUser(ctx, &domain.User{
		Email: "models-test@example.com", PasswordHash: "x",
		Role: domain.RoleUser, Status: domain.UserStatusActive,
		MaxConcurrency: 8, Balance: 0, // 余额 0：计费门禁若被错误触发 → 402
	})
	require.NoError(t, err)

	g1, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "g-models", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	g2, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "g-empty", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	// g3 = 幽灵组：key 正常绑定后软删除——FK 仍满足（行保留），但调度器快照
	// 按 deleted_at IS NULL 过滤 → 组不在快照 → /v1/models 404（组删除场景）。
	g3, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "g-ghost", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)

	tplA, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "t-models-a", BaseURL: "https://upstream-a.invalid",
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatOpenAIResponses},
		Models:           []string{"gpt-4o", "claude-3.5-sonnet"},
	})
	require.NoError(t, err)
	tplB, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "t-models-b", BaseURL: "https://upstream-b.invalid",
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatOpenAIImages},
		Models:           []string{"gpt-4o", "dall-e-3"},
	})
	require.NoError(t, err)
	for _, a := range []*domain.Account{
		{Name: "a-models-a", TemplateID: tplA.ID, UpstreamKey: "sk-1", Weight: 1, MaxConcurrency: 4},
		{Name: "a-models-b", TemplateID: tplB.ID, UpstreamKey: "sk-2", Weight: 1, MaxConcurrency: 4},
	} {
		acc, err := repos.Accounts.CreateAccount(ctx, a)
		require.NoError(t, err)
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{g1.ID}))
	}

	for _, k := range []*domain.Key{
		{UserID: u.ID, GroupID: g1.ID, Name: "k-models", KeyRaw: "ck-models", Status: domain.KeyStatusActive},
		{UserID: u.ID, GroupID: g2.ID, Name: "k-empty", KeyRaw: "ck-empty", Status: domain.KeyStatusActive},
		{UserID: u.ID, GroupID: g3.ID, Name: "k-ghost", KeyRaw: "ck-ghost", Status: domain.KeyStatusActive},
	} {
		_, err := repos.Keys.CreateKey(ctx, k)
		require.NoError(t, err)
	}
	// key 绑定后软删组 g3：鉴权快照仍含 k-ghost（行保留），调度器快照不含 g3。
	require.NoError(t, repos.Groups.DeleteGroup(ctx, g3.ID))

	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(ctx)) // 空表写种子（同 newTestProxyTplTimeoutRec）
	sched := scheduler.New(scheduler.Config{
		DefaultMaxConcurrency: 4, SyncInterval: time.Hour,
	}, repos.Groups, re, nil)
	require.NoError(t, sched.InvalidateAllSync())
	return modelsFixture{repos: repos, sched: sched, gid: g1.ID, gid2: g2.ID, userID: u.ID}
}

// newPGModelsTestProxy 以外部注入的调度器 + 鉴权快照构造测试代理（bill 非 nil
// = 计费启用——余额 0 也不得拦 /v1/models）。
func newPGModelsTestProxy(t *testing.T, sched *scheduler.Scheduler, auth *Auth, bill *BillingHooks) *Proxy {
	t.Helper()
	cfg := Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
		GroupKeyRPM:           0, UsageCapture: true,
		BillingCapture: bill != nil,
	}
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, noopLogStore{}, nil)
	hc := &http.Client{Transport: http.DefaultTransport}
	clients := aiclient.NewFactory(hc, aiclient.Config{
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
	})
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, FlushInterval: time.Hour}, noopErrLogStore{}, nil)
	return New(cfg, sched, credential.New(), rec, clients, auth, nil, bill, errlogW)
}

// modelsAuth 从真实 PG keys/users 构建并首刷鉴权快照（NewAuth 构造不再自载）。
func modelsAuth(t *testing.T, repos *repository.Repository) *Auth {
	t.Helper()
	a := NewAuth(repos.Keys, repos.Users, nil)
	require.NoError(t, a.Reload(context.Background()))
	return a
}

// modelsGET 经 AIRouter 发 GET /v1/models（覆盖路由注册面）。
func modelsGET(t *testing.T, router http.Handler, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// modelsResp OpenAI wire 解码容器。
type modelsResp struct {
	Object string `json:"object"`
	Data   []struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
}

// 跨格式去重 + 字典序稳定排序 + wire 形态（object/created/owned_by=组 ID 字符串）。
func TestPGModelsListDedupSorted(t *testing.T) {
	fx := modelsPGFixture(t)
	p := newPGModelsTestProxy(t, fx.sched, modelsAuth(t, fx.repos), nil)
	router := AIRouter(p)

	rec := modelsGET(t, router, "ck-models")
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var body modelsResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "list", body.Object, "OpenAI wire: object=list")
	require.Len(t, body.Data, 3, "gpt-4o 跨模板/跨格式必须去重为一项")
	ids := make([]string, 0, len(body.Data))
	for i, d := range body.Data {
		require.Equal(t, "model", d.Object, "data[%d].object 固定 model", i)
		require.Zero(t, d.Created, "data[%d].created = 0（无意义字段）", i)
		require.Equal(t, strconv.FormatInt(fx.gid, 10), d.OwnedBy, "data[%d].owned_by = 组 ID 字符串", i)
		ids = append(ids, d.ID)
	}
	require.Equal(t, []string{"claude-3.5-sonnet", "dall-e-3", "gpt-4o"}, ids, "按 id 字典序稳定排序")

	// 排序确定性：同快照两次请求响应逐字节一致。
	rec2 := modelsGET(t, router, "ck-models")
	require.Equal(t, http.StatusOK, rec2.Code)
	require.Equal(t, rec.Body.Bytes(), rec2.Body.Bytes(), "响应确定性：同快照两次请求逐字节一致")
}

// 空组 → 200 + 空 data 数组（不 404）。
func TestPGModelsEmptyGroup(t *testing.T) {
	fx := modelsPGFixture(t)
	p := newPGModelsTestProxy(t, fx.sched, modelsAuth(t, fx.repos), nil)

	rec := modelsGET(t, AIRouter(p), "ck-empty")
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"data":[]`, "空组必须返回空数组（非 null 非 404）")
	var body modelsResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "list", body.Object)
	require.Empty(t, body.Data)
}

// 未知组（鉴权已过但组失效）→ 404（对齐 Select 的 ErrGroupNotFound 语义）。
func TestPGModelsUnknownGroup(t *testing.T) {
	fx := modelsPGFixture(t)
	p := newPGModelsTestProxy(t, fx.sched, modelsAuth(t, fx.repos), nil)

	rec := modelsGET(t, AIRouter(p), "ck-ghost")
	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "group not found")
}

// 无鉴权 → 401（既有鉴权面）。
func TestPGModelsNoAuth(t *testing.T) {
	fx := modelsPGFixture(t)
	p := newPGModelsTestProxy(t, fx.sched, modelsAuth(t, fx.repos), nil)

	rec := modelsGET(t, AIRouter(p), "")
	require.Equal(t, http.StatusUnauthorized, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "invalid gateway key")
}

// 不走计费门禁：计费启用（BillingCapture + hooks）+ 用户余额 0（真实 PG 余额
// 快照，组倍率默认 10000 非 0）→ 仍 200——只读列表端点鉴权通过即放行，不得
// 误走 handleFormat 的余额预检（否则此场景必 402）。
func TestPGModelsBillingDisabledGate(t *testing.T) {
	fx := modelsPGFixture(t)
	balances := billing.NewBalances(fx.repos, nil)
	require.NoError(t, balances.Reload(context.Background()))
	bal, ok := balances.BalanceOf(fx.userID)
	require.True(t, ok, "种子用户必须进余额快照")
	require.Zero(t, bal, "余额 0：若误走余额预检（bal<0 且倍率非 0）必 402")
	bill := &BillingHooks{Balances: balances}
	p := newPGModelsTestProxy(t, fx.sched, modelsAuth(t, fx.repos), bill)

	rec := modelsGET(t, AIRouter(p), "ck-models")
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
}

// 组模板增删模型 → reload 后列表变化（对齐调度器重载语义）。
func TestPGModelsReloadReflectsChanges(t *testing.T) {
	fx := modelsPGFixture(t)
	p := newPGModelsTestProxy(t, fx.sched, modelsAuth(t, fx.repos), nil)
	router := AIRouter(p)
	ctx := context.Background()

	list := func() []string {
		rec := modelsGET(t, router, "ck-models")
		require.Equal(t, http.StatusOK, rec.Code)
		var body modelsResp
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		ids := make([]string, 0, len(body.Data))
		for _, d := range body.Data {
			ids = append(ids, d.ID)
		}
		return ids
	}
	require.Equal(t, []string{"claude-3.5-sonnet", "dall-e-3", "gpt-4o"}, list())

	// 增：新模板模型 gpt-5 + 组内账号 → reload 后出现
	tplC, err := fx.repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "t-models-c", BaseURL: "https://upstream-c.invalid",
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		Models:           []string{"gpt-5"},
	})
	require.NoError(t, err)
	accC, err := fx.repos.Accounts.CreateAccount(ctx, &domain.Account{
		Name: "a-models-c", TemplateID: tplC.ID, UpstreamKey: "sk-3",
		Weight: 1, MaxConcurrency: 4,
	})
	require.NoError(t, err)
	require.NoError(t, fx.repos.Accounts.SetAccountGroups(ctx, accC.ID, []int64{fx.gid}))
	require.NoError(t, fx.sched.InvalidateAllSync())
	require.Equal(t, []string{"claude-3.5-sonnet", "dall-e-3", "gpt-4o", "gpt-5"}, list())

	// 删：账号摘除组归属 → reload 后模型消失
	require.NoError(t, fx.repos.Accounts.SetAccountGroups(ctx, accC.ID, []int64{}))
	require.NoError(t, fx.sched.InvalidateAllSync())
	require.Equal(t, []string{"claude-3.5-sonnet", "dall-e-3", "gpt-4o"}, list())
}
