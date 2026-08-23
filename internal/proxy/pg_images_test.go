// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
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

// 真实 PG 集成（与 pg_responses_special_test.go 同款约定）：
//
//	TEST_DATABASE_URL=postgres://postgres:c3api@127.0.0.1:15432/c3api_test \
//	  go test ./internal/proxy/ -run TestPGImages -v
//
// 未设置 TEST_DATABASE_URL → t.Skip。独立 schema（proxy_images_test）：与同包
// 其它 PG 测试并行跑不互踩。断言面：路由集成（spec §7）——api_key /
// responses-special 两类型 × generations/edits 两端点直连、multipart 不撞
// json.Valid 硬门、402 生死、纯 image 价模型不被 chat 预检误杀。

const imagesPGTestSchema = "proxy_images_test"

// pgImagesUpstream 直连 mock 上游：捕获请求面（路径/鉴权/Content-Type/body），
// 返回标准 ImageResponse。
type pgImagesUpstream struct {
	mu   sync.Mutex
	path string
	auth string
	body []byte
	ct   string
}

func fakePGImagesUpstream(t *testing.T) (*httptest.Server, *pgImagesUpstream) {
	t.Helper()
	u := &pgImagesUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.path, u.auth, u.body, u.ct = r.URL.Path, r.Header.Get("Authorization"), b, r.Header.Get("Content-Type")
		u.mu.Unlock()
		if !strings.HasPrefix(r.URL.Path, "/v1/images/") {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-upstream" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"created": 1, "data": []any{map[string]any{"b64_json": "QUJD"}}})
	}))
	t.Cleanup(srv.Close)
	return srv, u
}

// setupImagesPG 打开真实 PG（独立 schema）+ 建模板/组/账号，返回调度器、
// 两组的组 ID 与上游捕获（模板 BaseURL 指向该上游——断言须读同一捕获）。
// 两组独立（选号确定性：每组单账号，类型不互抢）。
func setupImagesPG(t *testing.T) (*scheduler.Scheduler, int64, int64, *pgImagesUpstream) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + imagesPGTestSchema
	} else {
		dsn += "?search_path=" + imagesPGTestSchema
	}
	ctx := context.Background()
	pool, err := repository.OpenPG(ctx, dsn, 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+imagesPGTestSchema+` CASCADE; CREATE SCHEMA `+imagesPGTestSchema+`;`)
	require.NoError(t, err)
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), true)
	require.NoError(t, err)

	upSrv, up := fakePGImagesUpstream(t)

	tplAPI, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "t-images-api", BaseURL: upSrv.URL,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIImages},
		Models:           []string{"gpt-image-1"},
	})
	require.NoError(t, err)
	tplSpecial, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "t-images-special", BaseURL: upSrv.URL,
		CredentialType:   credential.TypeResponsesSpecial,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIImages},
		Models:           []string{"gpt-image-1"},
	})
	require.NoError(t, err, "responses-special 模板声明 openai-images 格式必须可落库（Task B 类型-格式约束扩展）")

	gAPI, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "g-images-api", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	gSpecial, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "g-images-special", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	for _, g := range []struct {
		groupID int64
		tplID   int64
		name    string
	}{
		{gAPI.ID, tplAPI.ID, "a-images-api"},
		{gSpecial.ID, tplSpecial.ID, "a-images-special"},
	} {
		acc, err := repos.Accounts.CreateAccount(ctx, &domain.Account{
			Name: g.name, TemplateID: g.tplID, UpstreamKey: "sk-upstream",
			Weight: 1, MaxConcurrency: 8,
		})
		require.NoError(t, err)
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{g.groupID}))
	}

	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	sched := scheduler.New(scheduler.Config{
		DefaultMaxConcurrency: 4, SyncInterval: time.Hour,
	}, repos.Groups, re, nil)
	require.NoError(t, sched.InvalidateAllSync())
	return sched, gAPI.ID, gSpecial.ID, up
}

// newPGImagesTestProxy 以外部注入的调度器构造测试代理（PG 快照链路；bill
// 可注入计费钩子——缺价预检断言用）。auth key ck-<groupID> → 组。
func newPGImagesTestProxy(t *testing.T, sched *scheduler.Scheduler, groupID int64, bill *BillingHooks) *Proxy {
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
		keyForGroup(groupID): activeKey(1, 1, groupID),
	}}, noopUserLoader{}, nil)
	require.NoError(t, auth.Reload(context.Background()))
	hc := &http.Client{Transport: http.DefaultTransport}
	clients := aiclient.NewFactory(hc, aiclient.Config{
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
	})
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, FlushInterval: time.Hour}, noopErrLogStore{}, nil)
	return New(cfg, sched, credential.New(), rec, clients, auth, nil, bill, errlogW)
}

func keyForGroup(groupID int64) string { return "ck-" + strconv.FormatInt(groupID, 10) }

// TestPGImagesTwoTypesTwoEndpoints api_key / responses-special 两类型 ×
// generations/edits 两端点直连（真实 PG 快照链路：模板 credential_type +
// 账号 upstream_key 经调度器 Selection 下发 → credentialFor 取 key → 上游
// 收到 Bearer 账号 key）。
func TestPGImagesTwoTypesTwoEndpoints(t *testing.T) {
	sched, gAPI, gSpecial, up := setupImagesPG(t)

	for _, tc := range []struct {
		name    string
		groupID int64
		path    string
		handler func(p *Proxy) func(w http.ResponseWriter, r *http.Request)
	}{
		{"api_key generations", gAPI, "/v1/images/generations", func(p *Proxy) func(w http.ResponseWriter, r *http.Request) { return p.HandleImagesGenerations }},
		{"api_key edits", gAPI, "/v1/images/edits", func(p *Proxy) func(w http.ResponseWriter, r *http.Request) { return p.HandleImagesEdits }},
		{"responses-special generations", gSpecial, "/v1/images/generations", func(p *Proxy) func(w http.ResponseWriter, r *http.Request) { return p.HandleImagesGenerations }},
		{"responses-special edits", gSpecial, "/v1/images/edits", func(p *Proxy) func(w http.ResponseWriter, r *http.Request) { return p.HandleImagesEdits }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPGImagesTestProxy(t, sched, tc.groupID, nil)
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{"model":"gpt-image-1","prompt":"cat"}`))
			req.Header.Set("Authorization", "Bearer "+keyForGroup(tc.groupID))
			rec := httptest.NewRecorder()
			tc.handler(p)(rec, req)
			require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
			up.mu.Lock()
			defer up.mu.Unlock()
			require.Equal(t, tc.path, up.path, "上游子路径")
			require.Equal(t, "Bearer sk-upstream", up.auth, "账号 upstream_key 直连鉴权")
		})
	}
}

// TestPGImagesMultipartHardGateAnd402 multipart 不撞 json.Valid 硬门（真实 PG
// 链路）+ 402 生死（image_price 空快照 → 402，上游不收请求）+ 纯 image 价
// 模型不被 chat 预检误杀（chat 价表空 + image 价有行 → 200）。
func TestPGImagesMultipartHardGateAnd402(t *testing.T) {
	sched, gAPI, _, up := setupImagesPG(t)

	// multipart：body 非 JSON（含图片文件字节）→ 200 + body 原样透传
	p := newPGImagesTestProxy(t, sched, gAPI, nil)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	require.NoError(t, mw.WriteField("model", "gpt-image-1"))
	fw, err := mw.CreateFormFile("image", "p.png")
	require.NoError(t, err)
	_, err = fw.Write([]byte("png-bytes-not-json"))
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	body := buf.Bytes()
	ct := mw.FormDataContentType()

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+keyForGroup(gAPI))
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	p.HandleImagesEdits(rec, req)
	require.Equal(t, 200, rec.Code, "multipart 不得撞 json.Valid 硬门：body=%s", rec.Body.String())
	up.mu.Lock()
	require.Equal(t, ct, up.ct, "multipart Content-Type（含 boundary）透传")
	require.Equal(t, body, up.body, "multipart body 字节原样透传")
	up.mu.Unlock()

	// 402 生死：计费启用 + image_price 快照无行 → 402（预检零 DB 快照读，
	// 上游不接触——路由链路与预检解耦，此处断言状态码语义）
	p402 := newPGImagesTestProxy(t, sched, gAPI, &BillingHooks{
		Resolver: &fakeImagePriceLookup{entries: map[string]*domain.PriceEntry{}},
		Balances:    billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{}}, nil),
	})
	req = httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-1","prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer "+keyForGroup(gAPI))
	rec = httptest.NewRecorder()
	p402.HandleImagesGenerations(rec, req)
	require.Equal(t, http.StatusPaymentRequired, rec.Code, "无 image_price 行 → 402：body=%s", rec.Body.String())

	// 纯 image 价模型不被 chat 预检误杀：chat 价表空（Prices 空 map）+ image
	// 价有行 → 200（修复前 GetPrice 先行 402 误杀）
	pOK := newPGImagesTestProxy(t, sched, gAPI, &BillingHooks{
		Resolver: &fakeImagePriceLookup{entries: map[string]*domain.PriceEntry{"gpt-image-1": perImagePriceRow("gpt-image-1")}},
		Balances:    billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{}}, nil),
	})
	req = httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-1","prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer "+keyForGroup(gAPI))
	rec = httptest.NewRecorder()
	pOK.HandleImagesGenerations(rec, req)
	require.Equal(t, 200, rec.Code, "纯 image 价模型不得被 chat 价预检误杀：body=%s", rec.Body.String())
}
