// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
)

// --- codex /v1/alpha/search 端点测试（spec 2026-08-13 v2） ---
// 本地 mock 上游 + 真实调度器/适配层（真实凭据 e2e 在 pg_search_endpoint_test.go）。

// search 请求/响应 fixture（对齐 codex CLI 真实形态：请求体
// {id, model, input, commands, settings, max_output_tokens}；响应体 opaque
// results/encrypted_output——网关零解析断言面）。
const (
	searchReqBody = `{"id":"req_1","model":"gpt-4o","input":[{"type":"web_search_call","request":{"queries":["codex 最新版本"]}}],"commands":[],"settings":{},"max_output_tokens":256}`
	searchRespRaw = `{"id":"req_1","output":"search results text","results":[{"type":"web_search","web_search":{"title":"t","url":"u","content":"c"}}],"encrypted_output":"enc","status":"completed"}`
)

// codexSearchStep 单次 search 上游响应步骤（status 200 → body 原样返回；
// 非 200 → 同 body 原样返回——错误信封透传断言用）。
type codexSearchStep struct {
	status int
	body   string
}

// codexSearchUpstream codex search mock 上游（/v1/alpha/search 端点——SDK
// 派生与静态裸根派生同路径）：记录路径/鉴权头/请求体/x-codex-turn-metadata；
// 步骤按序弹出（耗尽重复最后一步）。非 /v1/alpha/search 路径 → 404（SDK 派生
// 断言：打 search 端点而非 /responses）。
type codexSearchUpstream struct {
	mu     sync.Mutex
	calls  int
	paths  []string
	auths  []string
	turns  []string // x-codex-turn-metadata 头值（统一不转发断言）
	bodies [][]byte
	steps  []codexSearchStep
	last   codexSearchStep
}

func newCodexSearchUpstream(t *testing.T, steps ...codexSearchStep) (*httptest.Server, *codexSearchUpstream) {
	t.Helper()
	c := &codexSearchUpstream{last: codexSearchStep{status: 500, body: `{}`}}
	if len(steps) > 0 {
		c.steps = steps
		c.last = steps[len(steps)-1]
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/alpha/search" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.calls++
		c.paths = append(c.paths, r.URL.Path)
		c.auths = append(c.auths, r.Header.Get("Authorization"))
		c.turns = append(c.turns, r.Header.Get("x-codex-turn-metadata"))
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

func (c *codexSearchUpstream) callsN() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *codexSearchUpstream) path(i int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paths[i]
}

func (c *codexSearchUpstream) auth(i int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.auths[i]
}

func (c *codexSearchUpstream) turn(i int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.turns[i]
}

func (c *codexSearchUpstream) body(i int) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bodies[i]
}

// searchTestAcct 单账号描述：模板级类型 + 账号级凭据（api_key/responses-
// special → UpstreamKey；codex 类型 → Ext）。同组多账号可混合类型（四类型分
// 派断言用）。
type searchTestAcct struct {
	id       int64
	tplID    int64
	credType credential.Type
	key      string
	ext      *domain.AccountExt
}

// fakeFunctionPriceLookup 内存按单元价快照（search 计费测试用）。镜像生产
// GetFunctionPrice 兜底语义（service.go——同一定义防测试与实现漂移）：命中
// 返回行；查无 + codex-search → 默认价行（1000 毫分 = $0.01/次）；查无其他
// → 错误。
type fakeFunctionPriceLookup struct {
	m map[string]*domain.FunctionPrice
}

func (f *fakeFunctionPriceLookup) GetFunctionPrice(model string) (*domain.FunctionPrice, error) {
	if p, ok := f.m[model]; ok {
		return p, nil
	}
	if model == domain.CodexSearchModel {
		v := domain.DefaultCodexSearchPricePerCall
		return &domain.FunctionPrice{Model: model, PricePerCall: &v, Source: domain.PricingSourceManual}, nil
	}
	return nil, errors.New("no function price")
}

// newTestSearchProxy 构造 search 测试代理：每账号独立模板（同组 10——
// openai-responses 格式 + gpt-4o；混合类型组 = 多模板多账号）+ 装配适配层
// （统一失效回调走真实 T1 处理链——fakeFailureStore 落库替身 + 真实调度器
// FailAccount 摘除）。模板 BaseURL = mock 上游根（SDK 路径 ResponsesWSURL
// 拼完整 /v1/responses 端点，Search 方法内派生 /alpha/search；静态路径裸根
// 派生同路径）。bill 为计费钩子（nil = 计费全关）。
func newTestSearchProxy(t *testing.T, accts []searchTestAcct, upstream string, bill *BillingHooks, logs *captureLogStore) (*Proxy, *fakeFailureStore) {
	t.Helper()
	accs := make(map[int64][]*domain.Account, 1)
	for _, a := range accts {
		tpl := &domain.Template{
			ID: a.tplID, Name: fmt.Sprintf("t%d", a.tplID), BaseURL: upstream,
			CredentialType:   a.credType,
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
			Models:           []string{"gpt-4o"},
		}
		accs[10] = append(accs[10], &domain.Account{
			ID: a.id, TemplateID: tpl.ID, Template: tpl, UpstreamKey: a.key,
			Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4, Ext: a.ext,
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
	store := &fakeFailureStore{}
	failure := sdkbridge.NewFailureHandler(sdkbridge.FailureDeps{Store: store, Failer: sched, Log: nil})
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{
		QueueSize: 4096, BatchSize: 100,
		FlushInterval: 20 * time.Millisecond,
	}, logs, nil)
	wctx, wcancel := context.WithCancel(context.Background())
	require.NoError(t, errlogW.Start(wctx))
	t.Cleanup(func() { wcancel(); _ = errlogW.Close(context.Background()) })
	p := New(cfg, sched, credential.New(), rec, clients, auth, nil, bill, errlogW)
	p.SetCodex(sdkbridge.NewCodex(failure))
	return p, store
}

// i64ptr 测试 int64 指针 helper（FunctionPrice.PricePerCall 构造用）。
func i64ptr(v int64) *int64 { return &v }

// postSearch 向网关发 /v1/alpha/search 请求（Bearer gk-1 + 可选
// x-codex-turn-metadata——统一不转发断言面）。
func postSearch(t *testing.T, srv *httptest.Server, body string, turnMetadata string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/alpha/search", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer gk-1")
	req.Header.Set("Content-Type", "application/json")
	if turnMetadata != "" {
		req.Header.Set("x-codex-turn-metadata", turnMetadata)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// searchBillingHooks 构造 search 计费钩子（价格快照 + 按单元价快照 + 余额快照
// + 倍率 ×1；flusher nil——captureLogStore 经 rec 捕获 applyBilling 后的行）。
// fn 为按单元价快照（nil = 空表——GetFunctionPrice 兜底路径）。
func searchBillingHooks(fn *fakeFunctionPriceLookup) *BillingHooks {
	if fn == nil {
		fn = &fakeFunctionPriceLookup{m: map[string]*domain.FunctionPrice{}}
	}
	return &BillingHooks{
		Prices:         &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}},
		FunctionPrices: fn,
		Balances:       billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{1: 1_000_000}}, nil),
	}
}

// --- 四类型分派 + 透传 + 计费 ---

// TestSearchStaticPassthroughBilling api_key 静态透传（分派断言 + 透传断言 +
// 计费断言）：Bearer upstream key 直连上游、URL 裸根派生 /v1/alpha/search、
// 请求体/响应体原样（opaque results 不解析）、x-codex-turn-metadata 不转发；
// 2xx → format=openai-search + call_count=1 + price_per_call_millis（表行价
// 2500）+ cost=2500（applyBilling search 分支非 0 计费断言）。
func TestSearchStaticPassthroughBilling(t *testing.T) {
	up, upc := newCodexSearchUpstream(t, codexSearchStep{status: 200, body: searchRespRaw})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestSearchProxy(t, []searchTestAcct{{id: 10, tplID: 1, credType: credential.TypeAPIKey, key: "sk-upstream"}},
		up.URL, searchBillingHooks(&fakeFunctionPriceLookup{m: map[string]*domain.FunctionPrice{
			"codex-search": {Model: "codex-search", PricePerCall: i64ptr(2500)},
		}}), store)

	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()
	resp := postSearch(t, srv, searchReqBody, "turn-123")
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body=%s", string(b))
	require.Equal(t, searchRespRaw, string(b), "响应原样透传（opaque results/encrypted_output 不解析）")

	// 静态路径 wire 断言：Bearer upstream key + 派生 URL + 请求体原样 + 头不转发
	require.Equal(t, 1, upc.callsN())
	require.Equal(t, "/v1/alpha/search", upc.path(0), "URL 裸根派生 base/v1/alpha/search")
	require.Equal(t, "Bearer sk-upstream", upc.auth(0), "api_key 静态透传——Bearer upstream key 直连")
	require.Equal(t, "", upc.turn(0), "x-codex-turn-metadata 不转发（静态路径）")
	require.Equal(t, searchReqBody, string(upc.body(0)), "请求体原样送达上游")

	// 2xx 计费落账断言（applyBilling search 分支——非 0 计费）
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	lg := store.logs[0]
	require.Equal(t, domain.FormatOpenAISearch, lg.Format, "format=openai-search")
	require.Equal(t, int64(1), lg.CallCount, "call_count=1（按次计费）")
	require.NotNil(t, lg.PricePerCallMillis, "price_per_call_millis 必须落账")
	require.Equal(t, int64(2500), *lg.PricePerCallMillis, "表行价快照（GetFunctionPrice 命中）")
	require.Equal(t, int64(2500), lg.Cost, "cost = 按次价 × call_count（非 0 计费断言）")
	require.Equal(t, domain.ErrNone, lg.ErrorType)
	require.Zero(t, lg.TotalTokens, "search 无 token 分量")
	require.Equal(t, "gpt-4o", lg.Model)
	ri, ok := p.sched.Runtime(10)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "成功路径必须释放并发槽")
}

// TestSearchFunctionPriceDefaultFallback 表行缺失 → GetFunctionPrice 默认兜底
// 行（1000 毫分 = $0.01/次）：cost=1000 非 0（静默 0 计费杜绝断言）。
func TestSearchFunctionPriceDefaultFallback(t *testing.T) {
	up, _ := newCodexSearchUpstream(t, codexSearchStep{status: 200, body: searchRespRaw})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestSearchProxy(t, []searchTestAcct{{id: 10, tplID: 1, credType: credential.TypeAPIKey, key: "sk-upstream"}},
		up.URL, searchBillingHooks(nil), store) // 空表 → 默认行兜底

	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()
	resp := postSearch(t, srv, searchReqBody, "")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, int64(1000), store.logs[0].Cost, "默认兜底 1000 毫分（$0.01/次）——非 0 计费")
}

// TestSearchCodexPathPassthrough codex-oauth/codex-pat → SDK Search 分派断言：
// Auth 注入（oauth 初始 at / PAT）+ 打 /v1/alpha/search 非 /responses + 请求
// 体/响应体原样 + x-codex-turn-metadata 不转发 + 计费落账。
func TestSearchCodexPathPassthrough(t *testing.T) {
	for _, tc := range []struct {
		name    string
		credTyp credential.Type
		ext     func(int64) *domain.AccountExt
		auth    string
	}{
		{"pat", credential.TypeCodexPAT, func(id int64) *domain.AccountExt { return codexPATExt(id, "pat-10") }, "Bearer pat-10"},
		{"oauth", credential.TypeCodexOAuth, func(id int64) *domain.AccountExt { return codexOAuthExt(id, "at-10", "rt-10") }, "Bearer at-10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, upc := newCodexSearchUpstream(t, codexSearchStep{status: 200, body: searchRespRaw})
			defer up.Close()
			store := &captureLogStore{}
			p, _ := newTestSearchProxy(t, []searchTestAcct{{id: 10, tplID: 1, credType: tc.credTyp, ext: tc.ext(10)}},
				up.URL, searchBillingHooks(nil), store)

			srv := httptest.NewServer(AIRouter(p))
			defer srv.Close()
			resp := postSearch(t, srv, searchReqBody, "turn-123")
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			require.Equal(t, http.StatusOK, resp.StatusCode, "body=%s", string(b))
			require.Equal(t, searchRespRaw, string(b), "响应原样透传（SDK Search 零解析）")

			// SDK 路径 wire 断言：Auth 注入 + search URL 派生（非 /responses）+ 请求体原样 + 头不转发
			require.Equal(t, 1, upc.callsN())
			require.Equal(t, "/v1/alpha/search", upc.path(0), "SDK Search 派生打 /alpha/search 非 /responses")
			require.Equal(t, tc.auth, upc.auth(0), "codex 类型凭据经适配层 Auth 注入")
			require.Equal(t, "", upc.turn(0), "x-codex-turn-metadata 不转发（SDK 路径）")
			require.Equal(t, searchReqBody, string(upc.body(0)), "请求体原样（零改写——含不做 ModelMapping）")

			require.NoError(t, p.rec.Close(context.Background()))
			store.mu.Lock()
			defer store.mu.Unlock()
			require.Len(t, store.logs, 1)
			require.Equal(t, domain.FormatOpenAISearch, store.logs[0].Format)
			require.Equal(t, int64(1), store.logs[0].CallCount)
			require.Equal(t, int64(1000), store.logs[0].Cost, "默认兜底按次价（SDK 路径同样落账）")
		})
	}
}

// TestSearchResponsesSpecialStatic responses-special 类型 → 静态透传（与 api_key
// 同通道——staticKeyProvider 直读 UpstreamKey）。
func TestSearchResponsesSpecialStatic(t *testing.T) {
	up, upc := newCodexSearchUpstream(t, codexSearchStep{status: 200, body: searchRespRaw})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestSearchProxy(t, []searchTestAcct{{id: 10, tplID: 1, credType: credential.TypeResponsesSpecial, key: "sk-special"}},
		up.URL, nil, store)

	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()
	resp := postSearch(t, srv, searchReqBody, "")
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body=%s", string(b))
	require.Equal(t, 1, upc.callsN())
	require.Equal(t, "/v1/alpha/search", upc.path(0))
	require.Equal(t, "Bearer sk-special", upc.auth(0), "responses-special 静态透传——Bearer upstream key")
}

// --- 4xx 透传不计费 / fatal 失效 / failover 跨类型分派 ---

// TestSearch4xxPassthroughNoBilling 4xx 确定性拒绝：透传上游状态码与原始 body、
// 不转移、不计费（cost=0——Err4xx 走 err_logs 面）；账号不冷却、并发槽释放。
func TestSearch4xxPassthroughNoBilling(t *testing.T) {
	up, upc := newCodexSearchUpstream(t, codexSearchStep{status: 403, body: `{"detail":"Forbidden"}`})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestSearchProxy(t, []searchTestAcct{{id: 10, tplID: 1, credType: credential.TypeAPIKey, key: "sk-upstream"}},
		up.URL, searchBillingHooks(nil), store)

	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()
	resp := postSearch(t, srv, searchReqBody, "")
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Equal(t, `{"detail":"Forbidden"}`, string(b), "4xx 原样透传（静态路径错误信封 = 原始状态 + body）")
	require.Equal(t, 1, upc.callsN(), "4xx 确定性拒绝不转移")
	ri, ok := p.sched.Runtime(10)
	require.True(t, ok)
	require.Equal(t, domain.StatusActive, ri.Status, "4xx 不 MarkResult（不冷却）")
	require.Zero(t, ri.Concurrency, "4xx 透传也必须释放并发槽")
	waitStoreLogs(t, store, 1)
	store.mu.Lock()
	defer store.mu.Unlock()
	lg := store.logs[0]
	require.Equal(t, domain.Err4xx, lg.ErrorType, "4xx 失败行走 err_logs 分表")
	require.Equal(t, http.StatusForbidden, lg.StatusCode)
	require.Zero(t, lg.CallCount, "4xx 不计费（call_count 0）")
	require.Zero(t, lg.Cost, "4xx 不计费（cost=0）")
}

// TestSearchCodexEnvelope4xxPassthrough SDK 路径 4xx 信封（translateError T2）：
// 同样透传不转移不计费（与静态路径同语义——错误信封分路径表述）。
func TestSearchCodexEnvelope4xxPassthrough(t *testing.T) {
	up, _ := newCodexSearchUpstream(t, codexSearchStep{status: 429, body: `{"error":{"message":"rate limited"}}`})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestSearchProxy(t, []searchTestAcct{{id: 10, tplID: 1, credType: credential.TypeCodexPAT, ext: codexPATExt(10, "pat-10")}},
		up.URL, searchBillingHooks(nil), store)

	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()
	resp := postSearch(t, srv, searchReqBody, "")
	defer resp.Body.Close()
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode, "信封 429 透传")
	waitStoreLogs(t, store, 1)
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Equal(t, domain.Err429, store.logs[0].ErrorType, "429 失败行走 err_logs")
	require.Zero(t, store.logs[0].Cost, "非 2xx 不计费")
}

// TestSearchFatalMarksFailed fatal（401 判死 token_invalidated）→ 统一回调上
// 报（账号失效标记 + 快照摘除）+ 不重试同账号：耗尽 502 + ErrNetwork；上游恰
// 一次接触；失效上报恰一次（account 10）。
func TestSearchFatalMarksFailed(t *testing.T) {
	up, upc := newCodexSearchUpstream(t, codexSearchStep{status: 401, body: `{"error":{"code":"token_invalidated"}}`})
	defer up.Close()
	store := &captureLogStore{}
	p, recorder := newTestSearchProxy(t, []searchTestAcct{{id: 10, tplID: 1, credType: credential.TypeCodexOAuth, ext: codexOAuthExt(10, "at-10", "rt-10")}},
		up.URL, nil, store)

	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()
	resp := postSearch(t, srv, searchReqBody, "")
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusBadGateway, resp.StatusCode, "fatal 耗尽 → 502")
	require.Contains(t, string(b), "all upstream attempts failed")
	require.Equal(t, 1, upc.callsN(), "判死不重试——上游恰一次接触")
	require.Equal(t, 1, recorderCalls(recorder), "统一回调恰一次")
	_, acc, _ := recorder.snapshot()
	require.Equal(t, int64(10), acc)
	ri, ok := p.sched.Runtime(10)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status, "fatal → 账号失效摘除")
	waitStoreLogs(t, store, 1)
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Equal(t, domain.ErrNetwork, store.logs[0].ErrorType, "fatal 收尾记 code 0 ErrNetwork")
	require.Zero(t, store.logs[0].Cost, "fatal 不计费")
}

// TestSearchFailoverCrossTypeDispatch failover 跨类型分派（P1-1 教训回归）：
// 组内 codex-pat 账号 5xx 故障 → 换 api_key 账号 → **按新类型重新分派**（第
// 二轮走静态透传路径——Bearer upstream key，而非复用旧 SDK 调用器把健康
// api_key 账号路由到 Ext 空凭据路径）。
func TestSearchFailoverCrossTypeDispatch(t *testing.T) {
	up, upc := newCodexSearchUpstream(t,
		codexSearchStep{status: 500, body: `{"error":{"message":"boom"}}`},
		codexSearchStep{status: 200, body: searchRespRaw})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestSearchProxy(t, []searchTestAcct{
		{id: 10, tplID: 1, credType: credential.TypeCodexPAT, ext: codexPATExt(10, "pat-10")},
		{id: 20, tplID: 2, credType: credential.TypeAPIKey, key: "sk-upstream"},
	}, up.URL, nil, store)

	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()
	resp := postSearch(t, srv, searchReqBody, "")
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body=%s", string(b))
	require.Equal(t, searchRespRaw, string(b))
	require.Equal(t, 2, upc.callsN(), "5xx → 转移其它账号（恰两次上游接触）")

	// 跨类型重新分派断言（P1-1 教训回归）：两轮尝试分别走两种凭据路径——SDK
	// Search（Bearer pat-10）与静态透传（Bearer sk-upstream），且**第二轮按当轮
	// sel.CredentialType 重新分派**（复用旧调用器会把健康账号路由到 Ext 空凭据
	// 路径——此处第二轮用新类型凭据成功，恰证明按新类型走了新路径）。轮次顺序
	// 由调度器加权序列游标决定（组内两账号同权），断言集合而非顺序。
	require.Equal(t, "/v1/alpha/search", upc.path(0))
	require.Equal(t, "/v1/alpha/search", upc.path(1), "两路径同端点（派生一致）")
	auths := []string{upc.auth(0), upc.auth(1)}
	require.ElementsMatch(t, []string{"Bearer pat-10", "Bearer sk-upstream"}, auths,
		"两次尝试 = codex SDK 路径 + api_key 静态路径各一次（跨类型换账号按新类型分派）")
	// 第二轮（成功）的账号 = 落账账号
	wantAcc := int64(20)
	if auths[1] == "Bearer pat-10" {
		wantAcc = 10
	}

	p.sched.FlushRules()
	for _, id := range []int64{10, 20} {
		ri, ok := p.sched.Runtime(id)
		require.True(t, ok)
		require.Zero(t, ri.Concurrency, "account %d 并发槽必须全部释放", id)
	}
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, domain.ErrNone, store.logs[0].ErrorType, "failover 后成功路径")
	require.Equal(t, wantAcc, store.logs[0].AccountID, "落账 = 最后一次实际尝试账号")
}

// TestSearchIndependentSelection 独立选号断言（P2——无会话绑定）：同组两账号
// 轮询，两次顺序请求各独立 Select、命中不同账号（无会话亲和机制——search
// 请求自包含）。
func TestSearchIndependentSelection(t *testing.T) {
	up, _ := newCodexSearchUpstream(t, codexSearchStep{status: 200, body: searchRespRaw})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestSearchProxy(t, []searchTestAcct{
		{id: 10, tplID: 1, credType: credential.TypeAPIKey, key: "sk-upstream"},
		{id: 20, tplID: 2, credType: credential.TypeAPIKey, key: "sk-upstream"},
	}, up.URL, nil, store)

	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()
	for i := 0; i < 2; i++ {
		resp := postSearch(t, srv, searchReqBody, "")
		b, _ := io.ReadAll(resp.Body)
		require.Equal(t, http.StatusOK, resp.StatusCode, "body=%s", string(b))
		resp.Body.Close()
	}
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 2)
	seen := map[int64]bool{}
	for _, lg := range store.logs {
		seen[lg.AccountID] = true
	}
	require.Len(t, seen, 2, "两次请求各独立选号、命中不同账号（无会话绑定）")
}

// TestSearchSelectFormatUnavailable404 选号失败映射（P3-4）：组内模板不支持
// openai-responses → ErrFormatUnavailable → 404（handleSelectError 既有语义）；
// 上游零接触。
func TestSearchSelectFormatUnavailable404(t *testing.T) {
	up, upc := newCodexSearchUpstream(t, codexSearchStep{status: 200, body: searchRespRaw})
	defer up.Close()
	store := &captureLogStore{}
	// 模板只支持 openai-chat：search Select（openai-responses）必然失败
	p := newTestProxyFormatLogs(t, up.URL, domain.FormatOpenAIChat, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(searchReqBody))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleSearch(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, 0, upc.callsN(), "选号失败不触达上游")
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "Select 前无并发槽占用")
}
