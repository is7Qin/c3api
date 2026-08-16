// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
)

// fakeFailureStore 失效落库替身（T1 FailureStore 面——SetAccountFailed 记录
// 上报；Failer 用真实调度器——断言失效标记/摘除联动）。
type fakeFailureStore struct {
	mu        sync.Mutex
	calls     int
	accountID int64
	reason    string
}

func (f *fakeFailureStore) SetAccountFailed(ctx context.Context, accountID int64, _ time.Time, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.accountID = accountID
	f.reason = reason
	return nil
}

func (f *fakeFailureStore) snapshot() (int, int64, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.accountID, f.reason
}

// recorderCalls 失效上报次数（fakeFailureStore 断言辅助）。
func recorderCalls(f *fakeFailureStore) int {
	calls, _, _ := f.snapshot()
	return calls
}

// codexRefreshMock refresh 端点 mock（env override 构造期读取——SDK 测试同款
// t.Setenv("CODEX_REFRESH_TOKEN_URL_OVERRIDE")）。
func codexRefreshMock(t *testing.T, status int, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CODEX_REFRESH_TOKEN_URL_OVERRIDE", srv.URL)
}

// codexImageUpstream images 端点 mock：记录鉴权头/路径/body，响应序列按序
// 弹出（耗尽重复最后一步）。
type codexImageUpstream struct {
	mu     sync.Mutex
	auths  []string
	paths  []string
	bodies [][]byte
	steps  []codexUpStep
	last   codexUpStep
}

type codexUpStep struct {
	status int
	body   string
}

func newCodexImageUpstream(t *testing.T, steps ...codexUpStep) (*httptest.Server, *codexImageUpstream) {
	t.Helper()
	c := &codexImageUpstream{last: codexUpStep{status: 500, body: `{}`}}
	if len(steps) > 0 {
		c.steps = steps
		c.last = steps[len(steps)-1]
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.auths = append(c.auths, r.Header.Get("Authorization"))
		c.paths = append(c.paths, r.URL.Path)
		c.bodies = append(c.bodies, b)
		step := c.last
		if len(c.steps) > 0 {
			step = c.steps[0]
			c.steps = c.steps[1:]
			c.last = step
		}
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(step.status)
		_, _ = w.Write([]byte(step.body))
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func (c *codexImageUpstream) n() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.auths)
}

func (c *codexImageUpstream) auth(i int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.auths[i]
}

func (c *codexImageUpstream) path(i int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paths[i]
}

func (c *codexImageUpstream) body(i int) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bodies[i]
}

func (c *codexImageUpstream) authsSnapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.auths...)
}

// codexTestImageResponse 标准生图成功响应（2 张 + usage image_tokens）。
const codexTestImageResponse = `{"created":1720000000,"data":[{"b64_json":"QUJD"},{"b64_json":"REVG"}],"usage":{"input_tokens":2,"output_tokens":3,"input_tokens_details":{"image_tokens":1},"output_tokens_details":{"image_tokens":2}}}`

// codexOAuthExt 构造 codex-oauth 账号扩展（未过期 at + rt）。
func codexOAuthExt(accountID int64, at, rt string) *domain.AccountExt {
	exp := time.Now().Add(time.Hour)
	return &domain.AccountExt{
		AccountID: accountID, CredentialType: credential.TypeCodexOAuth,
		InstallationID: "inst-" + strings.Repeat("0", 32),
		OAuthToken:     &at, OAuthRefreshToken: &rt, OAuthExpiresAt: &exp,
	}
}

// newTestCodexProxy 构造 codex 类型 images 测试代理：模板（credType 模板级
// 类型）+ 携带 Ext 的账号（可多账号）+ 装配适配层（统一失效回调走真实 T1
// 处理链——fakeFailureStore 落库替身 + 真实调度器 FailAccount 摘除）。
// 模板 BaseURL = mock 上游根（路由 join images/generations 派生完整端点）。
func newTestCodexProxy(t *testing.T, credType credential.Type, accounts map[int64]*domain.AccountExt, upstream string, bill *BillingHooks, logs *captureLogStore) (*Proxy, *fakeFailureStore) {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credType,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIImages},
		Models:           []string{"gpt-image-2"},
	}
	accs := make(map[int64][]*domain.Account, 1)
	for id, ext := range accounts {
		accs[10] = append(accs[10], &domain.Account{
			ID: id, TemplateID: tpl.ID, Template: tpl, UpstreamKey: "",
			Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4, Ext: ext,
		})
	}
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, logs, nil)
	cfg := Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
		GroupKeyRPM:           0, UsageCapture: true,
	}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	sched := scheduler.New(scheduler.Config{DefaultMaxConcurrency: 4, SyncInterval: time.Hour}, noopLoader{accs: accs}, re, nil)
	require.NoError(t, sched.InvalidateAllSync())
	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{
		"gk-1": activeKey(1, 1, 10),
	}}, noopUserLoader{}, nil)
	require.NoError(t, auth.Reload(context.Background()))
	hc := &http.Client{Transport: http.DefaultTransport}
	clients := aiclient.NewFactory(hc, aiclient.Config{
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
	})
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, FlushInterval: time.Hour}, logs, nil)
	store := &fakeFailureStore{}
	// 统一失效回调（T1 装配形态）：落库替身 + 真实调度器摘除（FailAccount——
	// 失效标记断言依赖真实摘除，路由"不重试同账号"才成立）。
	failure := sdkbridge.NewFailureHandler(sdkbridge.FailureDeps{Store: store, Failer: sched, Log: nil})
	p := New(cfg, sched, credential.New(), rec, clients, auth, nil, bill, errlogW)
	p.SetCodex(sdkbridge.NewCodex(failure))
	return p, store
}

// TestImagesCodexGenerationsOK codex-oauth 非流式生图全链路（200 真实生成
// 形态）：适配层 SDK 直连 mock 上游（模板 base 派生 generations 端点）→ 客户
// 端 wire 转发（data 长 + 嵌套 usage）+ 计费口径统一（CallCount 张数 /
// ImageInput/OutputTokens image tokens / ImageCost per-image 分量）。
func TestImagesCodexGenerationsOK(t *testing.T) {
	up, c := newCodexImageUpstream(t, codexUpStep{status: 200, body: codexTestImageResponse})
	defer up.Close()
	codexRefreshMock(t, 200, `{"access_token":"at-new","refresh_token":"rt-new"}`)
	store := &captureLogStore{}
	p, recorder := newTestCodexProxy(t, credential.TypeCodexOAuth, map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")}, up.URL, &BillingHooks{
		Prices:      &fakePriceLookup{m: map[string]*domain.Pricing{}, im: map[string]*domain.ImagePrice{"gpt-image-2": perImagePriceRow("gpt-image-2")}},
		ImagePrices: &fakeImagePriceLookup{m: map[string]*domain.ImagePrice{"gpt-image-2": perImagePriceRow("gpt-image-2")}},
		Balances:    billingBalances(),
	}, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(
		`{"model":"gpt-image-2","prompt":"a cat","n":2}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleImagesGenerations(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	body := rec.Body.String()
	require.Equal(t, int64(2), gjson.Get(body, "data.#").Int(), "data 长 = 张数")
	require.Equal(t, "QUJD", gjson.Get(body, "data.0.b64_json").String(), "b64_json 直透")
	require.Equal(t, int64(1), gjson.Get(body, "usage.input_tokens_details.image_tokens").Int(), "wire 嵌套 usage（计费提取同源）")
	// cred 传递断言：账号 at 送达上游
	require.Equal(t, "Bearer at-10", c.auth(0), "oauth 账号 access token 送达实现侧")
	require.Equal(t, "/images/generations", c.path(0), "模板 base 派生 generations 端点")
	require.Equal(t, "gpt-image-2", gjson.GetBytes(c.body(0), "model").String(), "模型（sel.Model 映射）落位")
	require.Equal(t, "a cat", gjson.GetBytes(c.body(0), "prompt").String())
	require.Equal(t, int64(2), gjson.GetBytes(c.body(0), "n").Int(), "n 透传")
	// 计费：wire 与 C 提取纯函数同源（ImageUsageFromResponse）→ ImageCost
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	l := store.logs[0]
	require.Equal(t, domain.FormatOpenAIImages, l.Format)
	require.Equal(t, int64(2), l.CallCount, "张数 = data 长（入 call_count）")
	require.Equal(t, int64(1), l.InputTokens, "usage image_tokens 输入（并入 input_tokens）")
	require.Equal(t, int64(2), l.OutputTokens, "usage image_tokens 输出（并入 output_tokens）")
	require.Equal(t, int64(3), l.TotalTokens, "TotalTokens = image tokens 之和（张数不入）")
	require.Equal(t, int64(2*5400), l.Cost, "ImageCost per-image 分量（5400 毫分/张 × 2）")
	require.NotNil(t, l.PricePerCallMillis, "per-image 价格快照落列")
	require.Equal(t, int64(5400), *l.PricePerCallMillis)
	// 无失效上报
	require.Zero(t, recorderCalls(recorder), "成功路径零上报")
}

// TestImagesCodexPATDirect codex-pat 类型：PAT(key) 静态直连（无轮转回调面）。
func TestImagesCodexPATDirect(t *testing.T) {
	up, c := newCodexImageUpstream(t, codexUpStep{status: 200, body: codexTestImageResponse})
	defer up.Close()
	pat := "pat-key-1"
	ext := &domain.AccountExt{
		AccountID: 11, CredentialType: credential.TypeCodexPAT,
		InstallationID: "inst-" + strings.Repeat("1", 32),
		PATKey:         &pat,
	}
	store := &captureLogStore{}
	p, recorder := newTestCodexProxy(t, credential.TypeCodexPAT, map[int64]*domain.AccountExt{11: ext}, up.URL, nil, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(
		`{"model":"gpt-image-2","prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleImagesGenerations(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "Bearer pat-key-1", c.auth(0), "PAT 静态鉴权送达上游")
	require.Zero(t, recorderCalls(recorder))
	require.NoError(t, p.rec.Close(context.Background()))
}

// TestImagesCodexCredPassing 不同账号 cred 传递断言：同一适配层两账号（不同
// oauth at）连续请求 → 各自 at 送达上游（缓存按 accountID 隔离、互不串扰）。
// 加权序列每账号至少出现一次（游标顺序取用）——4 请求覆盖两账号，断言双 at
// 均送达。
func TestImagesCodexCredPassing(t *testing.T) {
	up, c := newCodexImageUpstream(t, codexUpStep{status: 200, body: codexTestImageResponse})
	defer up.Close()
	codexRefreshMock(t, 200, `{"access_token":"at-new","refresh_token":"rt-new"}`)
	store := &captureLogStore{}
	p, _ := newTestCodexProxy(t, credential.TypeCodexOAuth, map[int64]*domain.AccountExt{
		10: codexOAuthExt(10, "at-10", "rt-10"),
		11: codexOAuthExt(11, "at-11", "rt-11"),
	}, up.URL, nil, store)

	for i := 0; i < 4; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(
			`{"model":"gpt-image-2","prompt":"x"}`))
		req.Header.Set("Authorization", "Bearer gk-1")
		rec := httptest.NewRecorder()
		p.HandleImagesGenerations(rec, req)
		require.Equal(t, 200, rec.Code, "请求 %d body=%s", i, rec.Body.String())
	}
	got := map[string]bool{}
	for _, a := range c.authsSnapshot() {
		got[a] = true
	}
	require.True(t, got["Bearer at-10"], "账号 10 的 at 送达上游（auths=%v）", c.authsSnapshot())
	require.True(t, got["Bearer at-11"], "账号 11 的 at 送达上游（auths=%v）", c.authsSnapshot())
	require.NoError(t, p.rec.Close(context.Background()))
}

// TestImagesCodex403Passthrough 403 透传（账号无生图权限——HTTPError 信封）：
// 客户端收到 403 + 上游原始 body；信封不上报回调（透传协议）。
func TestImagesCodex403Passthrough(t *testing.T) {
	up, c := newCodexImageUpstream(t, codexUpStep{status: 403, body: `{"detail":"Forbidden"}`})
	defer up.Close()
	codexRefreshMock(t, 200, `{"access_token":"at-new","refresh_token":"rt-new"}`)
	store := &captureLogStore{}
	p, recorder := newTestCodexProxy(t, credential.TypeCodexOAuth, map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")}, up.URL, nil, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(
		`{"model":"gpt-image-2","prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleImagesGenerations(rec, req)

	require.Equal(t, 403, rec.Code, "403 透传（信封 StatusCode）")
	require.Contains(t, rec.Body.String(), "Forbidden", "上游原始 body 透传（信封 RawJSON）")
	require.Equal(t, 1, c.n(), "4xx 确定性错误不 failover")
	require.Zero(t, recorderCalls(recorder), "信封不上报回调")
	require.NoError(t, p.rec.Close(context.Background()))
}

// TestImagesCodexStreamEnvelope4xx 流式首事件前 4xx → 信封透传（T3 复审
// P1-1 修复回归——适配层 GenerateImageStream 缺 translateError 时 *HTTPError
// 裸抛：状态归 0 走连接级 → 客户端收占位文案、body 丢失；修复后 403 + 上游
// 原始 body 透传，与 T2 非流式同口径）。4xx 确定性错误不 failover、信封不
// 上报回调。
func TestImagesCodexStreamEnvelope4xx(t *testing.T) {
	up, c := newCodexImageUpstream(t, codexUpStep{status: 403, body: `{"error":{"message":"no image permission for account"}}`})
	defer up.Close()
	store := &captureLogStore{}
	p, recorder := newTestCodexProxy(t, credential.TypeCodexOAuth, map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")}, up.URL, nil, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(
		`{"model":"gpt-image-2","prompt":"x","stream":true}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleImagesGenerations(rec, req)

	require.Equal(t, 403, rec.Code, "流式首事件前 4xx 透传（信封 StatusCode）")
	require.Contains(t, rec.Body.String(), "no image permission for account", "上游原始 body 透传（信封 RawJSON——非 'codexsdk: upstream HTTP N' 占位）")
	require.Equal(t, 1, c.n(), "4xx 确定性错误不 failover")
	require.Zero(t, recorderCalls(recorder), "信封不上报回调")
	require.NoError(t, p.rec.Close(context.Background()))
}

// TestImagesCodexFatalFailAndNoRetry fatal（AT 401 判死——token_invalidated）：
// 统一回调单次上报（账号失效标记——快照 StatusDisabled）+ 不重试同账号（上游
// 只收一次）+ 客户端 5xx（failover 耗尽 502——账号已摘除不再被选）。
func TestImagesCodexFatalFailAndNoRetry(t *testing.T) {
	up, c := newCodexImageUpstream(t, codexUpStep{status: 401, body: `{"error":{"code":"token_invalidated"}}`})
	defer up.Close()
	codexRefreshMock(t, 200, `{"access_token":"at-new","refresh_token":"rt-new"}`)
	store := &captureLogStore{}
	p, recorder := newTestCodexProxy(t, credential.TypeCodexOAuth, map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")}, up.URL, nil, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(
		`{"model":"gpt-image-2","prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleImagesGenerations(rec, req)

	require.Equal(t, 502, rec.Code, "fatal → 账号失效 + 5xx 响应（failover 耗尽）")
	require.Equal(t, 1, c.n(), "判死不重试同账号（FailAccount 快照摘除）")
	calls, accountID, _ := recorder.snapshot()
	require.Equal(t, 1, calls, "fatal 统一回调单次上报（双源去重）")
	require.Equal(t, int64(10), accountID, "上报账号 = 选中账号")
	// 失效标记生效：快照 StatusDisabled（pickFrom 不再选）
	ri, ok := p.sched.Runtime(10)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status, "失效标记 = 调度摘除")
	// 第二次请求：账号已摘除 → 429 无可用账号（不重试；body 重新构造——首次
	// 请求已消费）
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(
		`{"model":"gpt-image-2","prompt":"x"}`))
	req2.Header.Set("Authorization", "Bearer gk-1")
	p.HandleImagesGenerations(rec2, req2)
	require.Equal(t, 429, rec2.Code, "账号摘除后选号失败（ErrNoAvailable → 429）")
	require.Equal(t, 1, c.n(), "摘除后不再触达上游")
	require.NoError(t, p.rec.Close(context.Background()))
}

// TestImagesCodex401Rotate 401 非判死（过期 AT）→ SDK 自动轮转（refresh 取新
// at）→ 重试一次成功：客户端 200，上游两次请求（旧 at → 新 at）——判死 vs
// 轮转的分界断言。
func TestImagesCodex401Rotate(t *testing.T) {
	var mu sync.Mutex
	var calls int
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		auths = append(auths, r.Header.Get("Authorization"))
		first := calls == 1
		mu.Unlock()
		if first {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"code":"token_expired"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(codexTestImageResponse))
	}))
	defer srv.Close()
	codexRefreshMock(t, 200, `{"access_token":"at-rotated","refresh_token":"rt-new"}`)
	store := &captureLogStore{}
	p, recorder := newTestCodexProxy(t, credential.TypeCodexOAuth, map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")}, srv.URL, nil, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(
		`{"model":"gpt-image-2","prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleImagesGenerations(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	mu.Lock()
	require.Equal(t, 2, calls, "401 非判死 → 轮转重试一次")
	require.Equal(t, "Bearer at-10", auths[0])
	require.Equal(t, "Bearer at-rotated", auths[1], "轮转后新 at 送达")
	mu.Unlock()
	require.Zero(t, recorderCalls(recorder), "轮转成功不上报")
	require.NoError(t, p.rec.Close(context.Background()))
}

// TestImagesCodexStreamSSE 流式（stream=true）生产接线全链路（T3——替换 501
// 骨架）：真实适配层 GenerateImageStream → 合成事件流（keepalive + 逐张
// completed，usage 仅末事件）→ 网关 SSE 透传（completed 帧 wire 形态 P2-1：
// b64_json + usage 四字段 JSON tag 直透）→ 流终计费（张数 = data 长 2、
// image tokens 平铺、ImageCost per-image 分量、倍率整单）。
func TestImagesCodexStreamSSE(t *testing.T) {
	up, c := newCodexImageUpstream(t, codexUpStep{status: 200, body: codexTestImageResponse})
	defer up.Close()
	store := &captureLogStore{}
	p, recorder := newTestCodexProxy(t, credential.TypeCodexOAuth, map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")}, up.URL, &BillingHooks{
		Prices:      &fakePriceLookup{m: map[string]*domain.Pricing{}, im: map[string]*domain.ImagePrice{"gpt-image-2": perImagePriceRow("gpt-image-2")}},
		ImagePrices: &fakeImagePriceLookup{m: map[string]*domain.ImagePrice{"gpt-image-2": perImagePriceRow("gpt-image-2")}},
		Balances:    billingBalances(),
	}, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(
		`{"model":"gpt-image-2","prompt":"a cat","stream":true}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleImagesGenerations(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "流式 200：body=%s", rec.Body.String())
	require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"), "SSE 内容类型")
	require.Equal(t, "no-cache", rec.Header().Get("Cache-Control"), "三件套 2/3")
	require.Equal(t, "no", rec.Header().Get("X-Accel-Buffering"), "三件套 3/3")
	// wire 形态：两帧 completed——首帧 b64_json 无 usage，末帧带 usage 平铺四字段
	// （codex-sdk ImageUsage JSON tag 直透——P3-2 等价）。
	want := "event: image_generation.completed\ndata: {\"b64_json\":\"QUJD\"}\n\n" +
		"event: image_generation.completed\ndata: {\"b64_json\":\"REVG\",\"usage\":{\"input_tokens\":2,\"input_image_tokens\":1,\"output_tokens\":3,\"output_image_tokens\":2}}\n\n"
	require.Equal(t, want, rec.Body.String(), "completed 帧 wire 形态（usage 仅末事件）")
	require.Equal(t, 1, c.n(), "流式合成 → 上游恰好一次（非流式调）")
	require.True(t, strings.HasSuffix(c.path(0), "/images/generations"), "SDK 派生 generations 端点")
	require.Zero(t, recorderCalls(recorder), "成功不上报失效")
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	l := store.logs[0]
	require.Equal(t, http.StatusOK, l.StatusCode)
	require.Equal(t, domain.ErrNone, l.ErrorType)
	require.Equal(t, int64(2), l.CallCount, "流终张数 = completed 数（入 call_count）")
	require.Equal(t, int64(1), l.InputTokens, "usage 平铺 image tokens 落账（并入 in）")
	require.Equal(t, int64(2), l.OutputTokens)
	require.Equal(t, int64(3), l.TotalTokens, "tt = image tokens 之和（张数不入）")
	require.Equal(t, int64(5400), *l.PricePerCallMillis, "per-image 快照列")
	require.Equal(t, int64(10800), l.Cost, "2 张 × 5400 ×1 倍率")
	require.Equal(t, "auto", l.BillingTier, "有价行 → 归一化照常")
}

// TestImagesCodexAdapterMissing501 适配层未装配（SetCodex 未调用）→ 501 显式
// 拒绝（防 nil 误走凭据缺失 502）——原 Task B 骨架语义保留。
func TestImagesCodexAdapterMissing501(t *testing.T) {
	up, c := newCodexImageUpstream(t, codexUpStep{status: 200, body: codexTestImageResponse})
	defer up.Close()
	store := &captureLogStore{}
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIImages},
		Models:           []string{"gpt-image-2"},
	}
	accs := map[int64][]*domain.Account{10: {{
		ID: 10, TemplateID: tpl.ID, Template: tpl, UpstreamKey: "",
		Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4, Ext: codexOAuthExt(10, "at-10", "rt-10"),
	}}}
	rec := usage.New(usage.UsageConfig{BatchSize: 100, FlushInterval: time.Hour, QuotaFlushInterval: time.Hour}, store, nil)
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	sched := scheduler.New(scheduler.Config{DefaultMaxConcurrency: 4, SyncInterval: time.Hour}, noopLoader{accs: accs}, re, nil)
	require.NoError(t, sched.InvalidateAllSync())
	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{
		"gk-1": activeKey(1, 1, 10),
	}}, noopUserLoader{}, nil)
	require.NoError(t, auth.Reload(context.Background()))
	hc := &http.Client{Transport: http.DefaultTransport}
	clients := aiclient.NewFactory(hc, aiclient.Config{UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second})
	p := New(Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second, GroupKeyRPM: 0, UsageCapture: true,
	}, sched, credential.New(), rec, clients, auth, nil, nil, usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, FlushInterval: time.Hour}, store, nil))
	// 不调 SetCodex —— 未装配形态

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(
		`{"model":"gpt-image-2","prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	recw := httptest.NewRecorder()
	p.HandleImagesGenerations(recw, req)

	require.Equal(t, http.StatusNotImplemented, recw.Code, "未装配 → 501（body=%s）", recw.Body.String())
	require.Contains(t, recw.Body.String(), "adapter not wired", "501 文案 = 装配缺失")
	require.Zero(t, c.n())
	require.NoError(t, p.rec.Close(context.Background()))
}

// TestImagesCodexMultipartEdits multipart edits 全链路：form 字段 + 图片文件
// part → ImageRef.Raw → SDK data URL 直嵌 → 上游收到 JSON（SDK 收结构体——
// 网关解析传，非 multipart 字节透传）。
func TestImagesCodexMultipartEdits(t *testing.T) {
	up, c := newCodexImageUpstream(t, codexUpStep{status: 200, body: codexTestImageResponse})
	defer up.Close()
	codexRefreshMock(t, 200, `{"access_token":"at-new","refresh_token":"rt-new"}`)
	store := &captureLogStore{}
	p, recorder := newTestCodexProxy(t, credential.TypeCodexOAuth, map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")}, up.URL, nil, store)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	require.NoError(t, mw.WriteField("model", "gpt-image-2"))
	require.NoError(t, mw.WriteField("prompt", "make it red"))
	fw, err := mw.CreateFormFile("image", "photo.png")
	require.NoError(t, err)
	_, err = fw.Write([]byte("PNG-binary"))
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Authorization", "Bearer gk-1")
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	p.HandleImagesEdits(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "/images/edits", c.path(0), "edits 由 SDK 尾段派生")
	b := c.body(0)
	require.Equal(t, "gpt-image-2", gjson.GetBytes(b, "model").String())
	require.Equal(t, "make it red", gjson.GetBytes(b, "prompt").String())
	require.Equal(t, "data:image/png;base64,UE5HLWJpbmFyeQ==", gjson.GetBytes(b, "images.0.image_url").String(), "Raw 字节 → data URL 直嵌")
	require.Zero(t, recorderCalls(recorder))
	require.NoError(t, p.rec.Close(context.Background()))
}

// TestImagesCodexJSONEdits JSON edits：images:[{image_url}] → 上游 JSON 直透。
func TestImagesCodexJSONEdits(t *testing.T) {
	up, c := newCodexImageUpstream(t, codexUpStep{status: 200, body: codexTestImageResponse})
	defer up.Close()
	codexRefreshMock(t, 200, `{"access_token":"at-new","refresh_token":"rt-new"}`)
	store := &captureLogStore{}
	p, _ := newTestCodexProxy(t, credential.TypeCodexOAuth, map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")}, up.URL, nil, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(
		`{"model":"gpt-image-2","prompt":"add hat","images":[{"image_url":"https://example.com/in.png"}]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleImagesEdits(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "/images/edits", c.path(0))
	require.Equal(t, "https://example.com/in.png", gjson.GetBytes(c.body(0), "images.0.image_url").String())
	require.NoError(t, p.rec.Close(context.Background()))
}

// TestImagesCodexEmptyRT 凭据不完整（oauth 缺 refresh_token）：P2-3 构造前
// 校验——上报失效（账号凭据不完整）不 panic；上游零请求；客户端 failover
// 耗尽 5xx。
func TestImagesCodexEmptyRT(t *testing.T) {
	up, c := newCodexImageUpstream(t, codexUpStep{status: 200, body: codexTestImageResponse})
	defer up.Close()
	at := "at-10"
	ext := &domain.AccountExt{
		AccountID: 10, CredentialType: credential.TypeCodexOAuth,
		InstallationID: "inst-" + strings.Repeat("2", 32),
		OAuthToken:     &at, // rt 缺失
	}
	store := &captureLogStore{}
	p, recorder := newTestCodexProxy(t, credential.TypeCodexOAuth, map[int64]*domain.AccountExt{10: ext}, up.URL, nil, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(
		`{"model":"gpt-image-2","prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleImagesGenerations(rec, req)

	require.Equal(t, 502, rec.Code, "凭据不完整 → 连接级 failover 耗尽（body=%s）", rec.Body.String())
	require.Zero(t, c.n(), "凭据不完整不触达上游")
	calls, accountID, _ := recorder.snapshot()
	require.Equal(t, 1, calls, "空 rt → 失效上报（账号凭据不完整）")
	require.Equal(t, int64(10), accountID)
	ri, ok := p.sched.Runtime(10)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status, "上报 → 调度摘除")
	require.NoError(t, p.rec.Close(context.Background()))
}

// TestImagesCodexParamsLocal400 本地参数拒绝（post-Select）：缺 prompt → 400 +
// err_logs 审计（P2-1 语义）；上游零请求。
func TestImagesCodexParamsLocal400(t *testing.T) {
	up, c := newCodexImageUpstream(t, codexUpStep{status: 200, body: codexTestImageResponse})
	defer up.Close()
	codexRefreshMock(t, 200, `{"access_token":"at-new","refresh_token":"rt-new"}`)
	store := &captureLogStore{}
	p, _ := newTestCodexProxy(t, credential.TypeCodexOAuth, map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")}, up.URL, nil, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(
		`{"model":"gpt-image-2"}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleImagesGenerations(rec, req)

	require.Equal(t, 400, rec.Code, "缺 prompt → 400（body=%s）", rec.Body.String())
	require.Contains(t, rec.Body.String(), "prompt required")
	require.Zero(t, c.n(), "本地拒绝不触达上游")
	require.NoError(t, p.errlog.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1, "本地拒绝记 err_logs")
	require.Equal(t, domain.ErrBilling, store.logs[0].ErrorType)
	require.Equal(t, http.StatusBadRequest, store.logs[0].StatusCode)
}

// TestImagesCodexMixedGroupFailoverReset P1-1 回归（评审实证）：混合类型组
// （同组 codex-oauth + api_key 模板均服务 images 格式、同模型）codex 尝试失败
// （5xx 可重试）→ failover 换 api_key 账号——调用器必须复位到直连 caller。
// 评审前泄漏：caller 单向赋值（codex 分支不复位），api_key 尝试被错误路由到
// codexImagesCaller → sel.Ext=nil → CredentialFromExt 空凭据 → 502 + 健康
// api_key 账号 MarkResult(ResultError) 错误率污染 + 无谓失效上报（account 0）。
//
// 确定性说明：weightedSeq 构造 shuffle 后 cursor 按序取 seq[1], seq[0],
// seq[1], seq[0]…——两请求内无论洗牌序，codex 先序至少一次触发 failover 路径
// （codex 在上游 500 → api_key 200），api_key 直连每请求恰一次触达（恒 2 次）；
// 修复前任一序必有一次 502，修复后恒 200。
func TestImagesCodexMixedGroupFailoverReset(t *testing.T) {
	codexUp, codexCap := newCodexImageUpstream(t, codexUpStep{status: 500, body: `{"error":"boom"}`})
	defer codexUp.Close()
	apiUp, apiCap := fakeImagesUpstream(t, "/v1/images/generations")
	defer apiUp.Close()
	codexRefreshMock(t, 200, `{"access_token":"at-new","refresh_token":"rt-new"}`)

	tplCodex := &domain.Template{
		ID: 1, Name: "codex", BaseURL: codexUp.URL,
		CredentialType:   credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIImages},
		Models:           []string{"gpt-image-2"},
	}
	tplAPI := &domain.Template{
		ID: 2, Name: "api", BaseURL: apiUp.URL,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIImages},
		Models:           []string{"gpt-image-2"},
	}
	accs := map[int64][]*domain.Account{10: {
		{
			ID: 10, TemplateID: tplCodex.ID, Template: tplCodex, UpstreamKey: "",
			Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4, Ext: codexOAuthExt(10, "at-10", "rt-10"),
		},
		{
			ID: 11, TemplateID: tplAPI.ID, Template: tplAPI, UpstreamKey: "sk-upstream",
			Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4,
		},
	}}
	rec := usage.New(usage.UsageConfig{BatchSize: 100, FlushInterval: time.Hour, QuotaFlushInterval: time.Hour}, &captureLogStore{}, nil)
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	sched := scheduler.New(scheduler.Config{DefaultMaxConcurrency: 4, SyncInterval: time.Hour}, noopLoader{accs: accs}, re, nil)
	require.NoError(t, sched.InvalidateAllSync())
	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{
		"gk-1": activeKey(1, 1, 10),
	}}, noopUserLoader{}, nil)
	require.NoError(t, auth.Reload(context.Background()))
	hc := &http.Client{Transport: http.DefaultTransport}
	clients := aiclient.NewFactory(hc, aiclient.Config{UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second})
	store := &captureLogStore{}
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, FlushInterval: time.Hour}, store, nil)
	fs := &fakeFailureStore{}
	failure := sdkbridge.NewFailureHandler(sdkbridge.FailureDeps{Store: fs, Failer: sched, Log: nil})
	p := New(Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second, GroupKeyRPM: 0, UsageCapture: true,
	}, sched, credential.New(), rec, clients, auth, nil, nil, errlogW)
	p.SetCodex(sdkbridge.NewCodex(failure))

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(
			`{"model":"gpt-image-2","prompt":"x"}`))
		req.Header.Set("Authorization", "Bearer gk-1")
		recw := httptest.NewRecorder()
		p.HandleImagesGenerations(recw, req)
		require.Equal(t, 200, recw.Code,
			"请求 %d：codex 失败后 api_key 尝试必须走直连 caller（修复前泄漏 → 502）；body=%s", i, recw.Body.String())
	}
	require.Zero(t, recorderCalls(fs), "跨类型 failover 不得无谓失效上报（修复前泄漏路径上报 account 0）")
	require.Equal(t, 2, apiCap.calls, "api_key 直连每请求恰一次触达（健康账号不被泄漏路径吞掉）")
	require.Equal(t, "Bearer sk-upstream", apiCap.auth, "api_key 直连凭据照常送达")
	require.GreaterOrEqual(t, codexCap.n(), 1, "codex 上游至少一次失败尝试（failover 路径确实被触发）")
	require.NoError(t, p.rec.Close(context.Background()))
}

// TestImageParamsJSONSinglePass A-P2-9 单遍解析等价回归：json.Unmarshal 单遍
// 替代 7×gjson.GetBytes 重扫（MB 级 base64 data URL body 每请求 ~8 遍全文档
// 扫描 → 1 遍）——同输入同输出：缺字段默认、类型不合忽略（gjson Type 判定
// 语义）、edits images 提取；MB 级 body 解析正确。
func TestImageParamsJSONSinglePass(t *testing.T) {
	// 全字段形态（与旧 gjson 路径逐字段等价）
	p, err := imageParamsJSON([]byte(`{"model":"gpt-image-2","prompt":"a cat","n":2,"size":"1024x1024","quality":"high","background":"transparent","images":[{"image_url":"https://example.com/in.png"},{"image_url":""}]}`))
	require.NoError(t, err)
	require.Equal(t, "gpt-image-2", p.Model)
	require.Equal(t, "a cat", p.Prompt)
	require.NotNil(t, p.N)
	require.Equal(t, 2, *p.N)
	require.NotNil(t, p.Size)
	require.Equal(t, "1024x1024", *p.Size)
	require.NotNil(t, p.Quality)
	require.Equal(t, "high", *p.Quality)
	require.NotNil(t, p.Background)
	require.Equal(t, "transparent", *p.Background)
	require.Len(t, p.Images, 1, "image_url 非空元素提取；空串元素跳过（gjson 同语义）")
	require.NotNil(t, p.Images[0].ImageURL)
	require.Equal(t, "https://example.com/in.png", *p.Images[0].ImageURL)

	// 缺字段 → 默认（gjson 缺字段默认一致：nil / 空切片）
	p2, err := imageParamsJSON([]byte(`{"model":"gpt-image-2","prompt":"x"}`))
	require.NoError(t, err)
	require.Nil(t, p2.N)
	require.Nil(t, p2.Size)
	require.Nil(t, p2.Quality)
	require.Nil(t, p2.Background)
	require.Empty(t, p2.Images)

	// 类型不合 → 忽略（gjson Type 判定同语义：字符串 n / 数字 size / null 不设）
	p3, err := imageParamsJSON([]byte(`{"model":"gpt-image-2","prompt":"x","n":"two","size":123,"background":null}`))
	require.NoError(t, err)
	require.Nil(t, p3.N, "字符串 n 忽略（gjson Number 判定）")
	require.Nil(t, p3.Size, "数字 size 忽略（gjson String 判定）")
	require.Nil(t, p3.Background, "null 忽略")

	// 必填缺失 → 错误；畸形 → 错误
	_, err = imageParamsJSON([]byte(`{"model":"gpt-image-2"}`))
	require.ErrorContains(t, err, "prompt required")
	_, err = imageParamsJSON([]byte(`{"prompt":"x"}`))
	require.ErrorContains(t, err, "model required")
	_, err = imageParamsJSON([]byte(`not-json`))
	require.ErrorContains(t, err, "invalid JSON")

	// MB 级 body（大 base64 data URL 的编辑请求）单遍解析正确
	b64 := strings.Repeat("A", 1<<20) // 1MB base64
	big := `{"model":"gpt-image-2","prompt":"edit","images":[{"image_url":"data:image/png;base64,` + b64 + `"}]}`
	p4, err := imageParamsJSON([]byte(big))
	require.NoError(t, err)
	require.Len(t, p4.Images, 1)
	require.True(t, strings.HasPrefix(*p4.Images[0].ImageURL, "data:image/png;base64,"), "MB 级 data URL 原样提取")
}
