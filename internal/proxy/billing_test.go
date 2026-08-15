// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
	"github.com/is7qin/c3api/pkg/cryptox"
)

// proxyPricing 测试价格行：gpt-4o 基础价 $100/$200 每 1M（1e7/2e7 毫分），
// priority 档 $200/$300，fast ×2.0。非流式上游返回 usage 3/5 tokens →
// auto 130 毫分 / priority 210 / fast 260。
func proxyPricing() *domain.Pricing {
	i64 := func(v int64) *int64 { return &v }
	return &domain.Pricing{
		Model:                             "gpt-4o",
		PromptPricePerMillion:             1e7,
		CompletionPricePerMillion:         2e7,
		PriorityPromptPricePerMillion:     i64(2e7),
		PriorityCompletionPricePerMillion: i64(3e7),
		FastMultiplier:                    i64(20000), // ×2.0
	}
}

// fakePriceLookup 内存价格快照（proxy 计费测试用）。failFrom > 0 = 第 N 次
// GetPrice 调用起恒失败（模拟预检后快照被删竞态）。im 为图片生成价快照
// （Task D 响应检测计费旁路用；nil = 无图价——GetImagePrice 恒 ErrNotFound）。
type fakePriceLookup struct {
	mu       sync.Mutex
	m        map[string]*domain.Pricing
	im       map[string]*domain.ImagePrice
	call     int
	failFrom int
}

func (f *fakePriceLookup) GetPrice(model string) (*domain.Pricing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call++
	if f.failFrom > 0 && f.call >= f.failFrom {
		return nil, errors.New("no price")
	}
	if p, ok := f.m[model]; ok {
		return p, nil
	}
	return nil, errors.New("no price")
}

// GetImagePrice 图片生成价快照读（spec §6 检测旁路查价；缺失 → 错误）。
func (f *fakePriceLookup) GetImagePrice(model string) (*domain.ImagePrice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.im[model]; ok {
		return p, nil
	}
	return nil, errors.New("no image price")
}

// newTestProxyBillingLogs 构造注入计费钩子的测试代理（默认 gpt-4o 模板 + 捕获
// 日志；policy nil = 恒透传）。Balances 空快照 → 倍率默认 ×1（T2 断言恒等，
// T3.5 无 nil 容忍：hooks 四字段齐备）。
func newTestProxyBillingLogs(t *testing.T, upstream string, prices *fakePriceLookup, policy func(billing.Tier) billing.TierPolicyMode, logs usage.LogInserter) *Proxy {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
	}
	return newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, logs, &BillingHooks{
		Prices:     prices,
		Balances:   billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{}}, nil),
		TierPolicy: policy,
	})
}

// TestProxyBillingNoPrice402 缺价预检：计费启用且模型无价格 → 402 + 释放并发槽
// + 无明细（P2a 源头修复：本地预用量拒绝不产生 usage_logs/pending——无 tokens
// 无 cost 的拒绝每请求一条明细即拒绝风暴无界积压源），上游一个请求都不许收到
// （评审 I-1：先 Release 再记录）。
func TestProxyBillingNoPrice402(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(500)
	}))
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyBillingLogs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{}}, nil, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, http.StatusPaymentRequired, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "no price", "402 文案说明缺价")
	require.Zero(t, hits.Load(), "缺价不得转发上游")
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "402 路径必须释放并发槽")
	require.Zero(t, p.rec.Pending(), "预用量拒绝不产生明细 pending")
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Empty(t, store.logs, "预用量拒绝不产生 usage_logs 明细（P2a）")
}

// TestProxyBillingAppliesCost finish applyBilling：成功请求按 tokens 计算毫分
// 成本（BillingTier=auto，无 service_tier）。
func TestProxyBillingAppliesCost(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyBillingLogs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}}, nil, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, "auto", store.logs[0].BillingTier, "无 service_tier → auto")
	require.Equal(t, int64(130), store.logs[0].Cost, "3×1e7+5×2e7 → 130 毫分")
	require.False(t, store.logs[0].AboveHit)
	// 价格快照（每 M token 毫分）：基础单价直读 + 无缓存分量 → 缓存价 nil；
	// 非流式无首 token → TTFT nil
	require.Equal(t, int64(1e7), *store.logs[0].PriceInputMillis, "输入单价快照（基础价）")
	require.Equal(t, int64(2e7), *store.logs[0].PriceOutputMillis, "输出单价快照（基础价）")
	require.Nil(t, store.logs[0].PriceCacheReadMillis, "无缓存读 → nil")
	require.Nil(t, store.logs[0].PriceCacheCreationMillis, "无缓存写 → nil")
	require.Nil(t, store.logs[0].TTFTMS, "非流式 → TTFT nil")
}

// TestProxyBillingTierPriority service_tier=priority：BillingTier 归一化落日志，
// 成本按 priority 单价档计算（210 ≠ auto 130）。
func TestProxyBillingTierPriority(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyBillingLogs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}}, nil, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","service_tier":"priority","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, "priority", store.logs[0].BillingTier)
	require.Equal(t, int64(210), store.logs[0].Cost, "priority 单价档：3×2e7+5×3e7 → 210 毫分")
}

// TestProxyBillingTierFast service_tier=fast：独立档位（Anthropic Fast Mode）→
// 整单 × fast_multiplier（130×2.0 = 260）。
func TestProxyBillingTierFast(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyBillingLogs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}}, nil, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","service_tier":"fast","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, "fast", store.logs[0].BillingTier)
	require.Equal(t, int64(260), store.logs[0].Cost, "fast ×2.0：130×2 = 260 毫分")
}

// TestProxyBillingTierPolicyStrip strip 策略：转发体删除 service_tier 字段（流式
// 原始 body 直发，可观测）；剥离路径计费照常（tier 已提取 → priority 单价）。
func TestProxyBillingTierPolicyStrip(t *testing.T) {
	gotTier := make(chan bool, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		_, hasTier := body["service_tier"]
		gotTier <- hasTier
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`+"\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyBillingLogs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}},
		func(billing.Tier) billing.TierPolicyMode { return billing.TierPolicyStrip }, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","service_tier":"priority","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	require.False(t, <-gotTier, "strip 策略：上游不得收到 service_tier 字段")

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, "priority", store.logs[0].BillingTier, "剥离路径计费照常（tier 已提取）")
	require.Equal(t, int64(210), store.logs[0].Cost, "剥离路径按 priority 单价计费")
	require.NotNil(t, store.logs[0].TTFTMS, "流式首 chunk 到达 → TTFT 采集")
	require.GreaterOrEqual(t, *store.logs[0].TTFTMS, int64(0), "TTFT 毫秒非负")
}

// TestProxyBillingTierPolicyReject reject 策略：直接 400，不转发上游；无明细
// （P2a 源头修复：本地预用量拒绝不产生 usage_logs/pending）。
func TestProxyBillingTierPolicyReject(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(500)
	}))
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyBillingLogs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}},
		func(billing.Tier) billing.TierPolicyMode { return billing.TierPolicyReject }, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","service_tier":"priority","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	require.Zero(t, hits.Load(), "reject 不得转发上游")
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "reject 路径并发槽必须释放（acquire defer）")
	require.Zero(t, p.rec.Pending(), "预用量拒绝不产生明细 pending")

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Empty(t, store.logs, "预用量拒绝不产生 usage_logs 明细（P2a）")
}

// TestProxyBillingTierFastPolicyStrip fast 档 strip 策略（M-1 回归：此前 caller
// 门控不含 TierFast → fast 恒透传，策略零效果）：转发体删 service_tier；剥离
// 路径计费照常（tier 已提取 → fast 档 ×2.0 → 260）。
func TestProxyBillingTierFastPolicyStrip(t *testing.T) {
	gotTier := make(chan bool, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		_, hasTier := body["service_tier"]
		gotTier <- hasTier
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`+"\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyBillingLogs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}},
		func(billing.Tier) billing.TierPolicyMode { return billing.TierPolicyStrip }, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","service_tier":"fast","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	require.False(t, <-gotTier, "fast strip 策略：上游不得收到 service_tier 字段")

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, "fast", store.logs[0].BillingTier, "剥离路径计费照常（tier 已提取）")
	require.Equal(t, int64(260), store.logs[0].Cost, "剥离路径按 fast 档计费：130×2.0 = 260")
}

// TestProxyBillingTierFastPolicyReject fast 档 reject 策略：直接 400，不转发
// 上游；无明细（P2a 源头修复）。
func TestProxyBillingTierFastPolicyReject(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(500)
	}))
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyBillingLogs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}},
		func(billing.Tier) billing.TierPolicyMode { return billing.TierPolicyReject }, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","service_tier":"fast","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	require.Zero(t, hits.Load(), "fast reject 不得转发上游")
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "reject 路径并发槽必须释放（acquire defer）")
	require.Zero(t, p.rec.Pending(), "预用量拒绝不产生明细 pending")

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Empty(t, store.logs, "预用量拒绝不产生 usage_logs 明细（P2a）")
}

// TestProxyBillingTierFastPolicyPassthrough fast 档 passthrough（默认）：原样
// 转发（service_tier 保留在转发体）；计费照常 fast 档。
func TestProxyBillingTierFastPolicyPassthrough(t *testing.T) {
	gotTier := make(chan bool, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		_, hasTier := body["service_tier"]
		gotTier <- hasTier
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`+"\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyBillingLogs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}},
		func(billing.Tier) billing.TierPolicyMode { return billing.TierPolicyPassthrough }, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","service_tier":"fast","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	require.True(t, <-gotTier, "passthrough 策略：service_tier 原样保留在转发体")

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, "fast", store.logs[0].BillingTier)
	require.Equal(t, int64(260), store.logs[0].Cost, "passthrough 按 fast 档计费：130×2.0 = 260")
}

// TestProxyBillingNoPriceDefenseAtFinish 运行时防御：预检通过后快照被删（竞态）→
// applyBilling Warn + BillingTier="no_price" + cost 0（不按 0 计价也不炸）。
func TestProxyBillingNoPriceDefenseAtFinish(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	// failFrom=2：预检（第 1 次）成功，finish applyBilling（第 2 次）失败。
	p := newTestProxyBillingLogs(t, up.URL, &fakePriceLookup{
		m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}, failFrom: 2,
	}, nil, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String(), "预检通过 → 正常转发")

	// 分表路由（放行路径语义）：防御行 ErrorType=none（客户端拿到 200）→ 入
	// usage_logs（cost=0 不限——err_logs 仅失败行）；errlog 不投递。
	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, "no_price", store.logs[0].BillingTier, "竞态防御：no_price 审计")
	require.Zero(t, store.logs[0].Cost, "缺价防御 cost 0")
	require.Equal(t, domain.ErrNone, store.logs[0].ErrorType, "放行路径（none）入 usage_logs")
	require.Nil(t, store.logs[0].PriceInputMillis, "no_price 防御：输入单价快照保持 nil（NULL 落库）")
	require.Nil(t, store.logs[0].PriceOutputMillis, "no_price 防御：输出单价快照保持 nil")
	require.Nil(t, store.logs[0].PriceCacheReadMillis, "no_price 防御：缓存读单价快照保持 nil")
	require.Nil(t, store.logs[0].PriceCacheCreationMillis, "no_price 防御：缓存写单价快照保持 nil")
}

// TestProxyBillingPriceSnapshotCache 缓存价快照：请求有缓存读/写分量且模型有
// 缓存价 → price_cache_*_millis 落快照（基础 input/output 快照照常）；无缓存
// 分量的请求 → 缓存价保持 nil（见 TestProxyBillingAppliesCost）。
func TestProxyBillingPriceSnapshotCache(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":4},"cache_creation":{"ephemeral_5m_input_tokens":2}}}`+"\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer up.Close()
	i64 := func(v int64) *int64 { return &v }
	pr := proxyPricing()
	pr.CacheReadPricePerMillion = i64(5e6)     // $50 / 1M
	pr.CacheCreationPricePerMillion = i64(1e7) // $100 / 1M
	store := &captureLogStore{}
	p := newTestProxyBillingLogs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": pr}}, nil, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, int64(4), store.logs[0].CacheReadTokens, "缓存读 token 采集")
	require.Equal(t, int64(2), store.logs[0].CacheCreationTokens, "缓存写 token 采集")
	require.Equal(t, int64(5e6), *store.logs[0].PriceCacheReadMillis, "缓存读单价快照")
	require.Equal(t, int64(1e7), *store.logs[0].PriceCacheCreationMillis, "缓存写单价快照")
	require.Equal(t, int64(1e7), *store.logs[0].PriceInputMillis, "基础输入单价快照照常")
	require.Equal(t, int64(2e7), *store.logs[0].PriceOutputMillis, "基础输出单价快照照常")
	require.NotNil(t, store.logs[0].TTFTMS, "流式 → TTFT 采集")
}

// TestProxyBillingStreamAbortCostsTokens recordStreamAbort 修复（评审 M-2）：
// 上游停滞前已收到的 usage 帧必须参与计费（此前传 nil → tokens 全 0 → 消费不扣费）。
func TestProxyBillingStreamAbortCostsTokens(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`+"\n\n")
		fl.Flush()
		<-r.Context().Done() // 首帧后停滞 → UpstreamStreamTimeout 触发中止
	}))
	defer up.Close()
	store := &captureLogStore{}
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
	}
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 100*time.Millisecond, store, &BillingHooks{
		Prices:   &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}},
		Balances: billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{}}, nil),
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	p.sched.FlushRules() // MarkResult 异步投递：断言前排空
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusUnhealthy, ri.Status, "停滞超时记 ResultError")
	require.Zero(t, ri.Concurrency)
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, domain.ErrAbort, store.logs[0].ErrorType)
	require.Equal(t, int64(5), store.logs[0].InputTokens, "中止前已累积的 usage 帧不丢")
	require.Equal(t, int64(7), store.logs[0].OutputTokens)
	require.Equal(t, int64(190), store.logs[0].Cost, "5×1e7+7×2e7 → 190 毫分（计费不丢）")
}

// TestProxyBillingStreamAbortGroupMultiplier 评审 M-1：recordStreamAbort 传
// groupID → 中止路径组倍率生效（此前硬编码 0 → 组查找恒 miss → 按 ×1 计费，
// 上浮倍率少收/折扣倍率多收）。组倍率 15000（gk-1 → groupID 10）：
// 190×15000/10000 = 285 毫分，与正常路径一致。
func TestProxyBillingStreamAbortGroupMultiplier(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`+"\n\n")
		fl.Flush()
		<-r.Context().Done() // 首帧后停滞 → UpstreamStreamTimeout 触发中止
	}))
	defer up.Close()
	store := &captureLogStore{}
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
	}
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 100*time.Millisecond, store, &BillingHooks{
		Prices: &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}},
		Balances: func() *billing.Balances {
			bal := billing.NewBalances(fakeBalanceLoader{
				m: map[int64]int64{}, gm: map[int64]int{10: 15000}, // 组倍率 ×1.5（用户未设置）
			}, nil)
			require.NoError(t, bal.Reload(context.Background()), "倍率快照加载（组倍率进快照）")
			return bal
		}(),
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	p.sched.FlushRules()
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, domain.ErrAbort, store.logs[0].ErrorType)
	require.Equal(t, int64(285), store.logs[0].Cost, "中止路径组倍率生效：190×15000/10000 = 285 毫分")
}

// TestProxyBillingDisabledPassthrough 计费全关（bill nil）：service_tier 恒透传
// （不 402、不 reject、不剥离），BillingTier 不落日志（空 = 未计费路径）。
// 分表路由（放行路径语义）：成功行（none）入 usage_logs——cost=0 不限。
func TestProxyBillingDisabledPassthrough(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyTimeoutLogs(t, up.URL, 1, store) // bill nil

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","service_tier":"priority","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String(), "计费全关：无价格表也不 402")

	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, "", store.logs[0].BillingTier, "计费全关：BillingTier 空")
	require.Zero(t, store.logs[0].Cost)
}

// ---------------------------------------------------------------------------
// T3：余额预检 402 + shouldBill 路由切换
// ---------------------------------------------------------------------------

// fakeBalanceLoader 余额 + 倍率快照测试 loader（am/gm 缺省 = 空倍率表）。
type fakeBalanceLoader struct {
	m  map[int64]int64               // 余额
	am map[billing.AssignmentKey]int // 用户-组专属倍率（仅已设置行）
	gm map[int64]int                 // 组倍率
}

func (f fakeBalanceLoader) LoadBalances(ctx context.Context) (map[int64]int64, error) {
	return f.m, nil
}

func (f fakeBalanceLoader) LoadGroupMultipliers(ctx context.Context) (map[int64]int, error) {
	return f.gm, nil
}

func (f fakeBalanceLoader) LoadAssignmentMultipliers(ctx context.Context) (map[billing.AssignmentKey]int, error) {
	return f.am, nil
}

// fakeDeductWriter 记录 DeductAndLog 调用（T3 billed 路由断言）。
type fakeDeductWriter struct {
	mu    sync.Mutex
	calls []deductCall
}

type deductCall struct {
	userID, cost int64
	logs         []*domain.UsageLog
}

func (f *fakeDeductWriter) DeductAndLog(ctx context.Context, userID, cost int64, logs []*domain.UsageLog) (bool, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, deductCall{userID: userID, cost: cost, logs: logs})
	return false, 900000, nil
}

// newTestProxyBillingT3Logs 构造注入完整计费钩子（Prices+Balances+Flusher）的
// 测试代理：BillingCapture 开（shouldBill 路由 + 余额预检生效）。rec 为调用方
// 构造的 Recorder（同一实例既挂 Flusher 又作 proxy.rec——统计聚合单面可观测，
// P2a 拒绝路径断言用）。
func newTestProxyBillingT3Logs(t *testing.T, upstream string, prices *fakePriceLookup, bal *billing.Balances, f *billing.Flusher, rec *usage.Recorder) *Proxy {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
	}
	p := newTestProxyTplTimeoutRec(t, tpl, 1, true, 30*time.Second, rec, &BillingHooks{
		Prices: prices, Balances: bal, Flusher: f,
	}, nil)
	p.cfg.BillingCapture = true
	return p
}

// TestProxyBillingInsufficientBalance402 余额预检（评审 I-1 无槽位问题）：
// 快照 <0 或缺失 → 402 + 上游零命中，预检在 Acquire 前不占用并发槽；余额 0
// 放行（spec 2026-08-15 语义边界表：临时额度由 FEFO 扣费消化，预检不读临时
// 额度）。P2a 源头修复：本地预用量拒绝不产生 usage_logs 明细/pending
// （balance 烧穿后的 402 风暴与 429 同路径，明细即无界积压源）；billed
// flusher 零调用（spec 2026-08-14：请求路径零统计——拒绝路径统计计数交由
// 离线聚合 worker 兜底，不再请求路径即时聚合）。
func TestProxyBillingInsufficientBalance402(t *testing.T) {
	cases := []struct {
		name     string
		bal      *billing.Balances
		pass     bool   // true = 放行（上游命中）；false = 402 + 上游零命中
		upStatus int    // 放行用例 200（单次命中完成流，failover 不重试）；拒绝用例 500（不可达）
	}{
		{"余额 0 放行", billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{1: 0}}, nil), true, http.StatusOK},
		{"余额负", billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{1: -1}}, nil), false, http.StatusInternalServerError},
		{"快照缺失", billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{}}, nil), false, http.StatusInternalServerError},
		// 评审 I-1：快照缺失 + 组倍率显式 ×1（非免费）→ 仍 402（免费放行只对
		// 有效倍率 0 生效；缺失且非免费 = 无余额记录，语义不变）。
		{"快照缺失 + 组倍率 10000", billing.NewBalances(fakeBalanceLoader{gm: map[int64]int{10: 10000}}, nil), false, http.StatusInternalServerError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var hits atomic.Int64
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if c.upStatus != http.StatusOK {
					w.WriteHeader(c.upStatus)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","created":0,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`))
			}))
			defer up.Close()
			store := &captureLogStore{}
			rec := usage.New(usage.UsageConfig{
				BatchSize: 100, FlushInterval: time.Hour,
				QuotaFlushInterval: time.Hour,
			}, store, nil)
			require.NoError(t, c.bal.Reload(context.Background()), "快照加载（余额 0 / 负 / 空表）")
			writer := &fakeDeductWriter{}
			f := billing.NewFlusher(billing.FlushConfig{
				FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour,
			}, writer, rec, c.bal, nil)
			p := newTestProxyBillingT3Logs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}}, c.bal, f, rec)

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
			req.Header.Set("Authorization", "Bearer gk-1")
			recw := httptest.NewRecorder()
			p.HandleChat(recw, req)

			if c.pass {
				require.Equal(t, http.StatusOK, recw.Code, "余额 0 放行：不得 402，须转发上游成功响应")
				require.Equal(t, int64(1), hits.Load(), "余额 0 放行：必须命中上游且单次完成")
				require.NoError(t, f.Close(context.Background()))
				require.NoError(t, rec.Close(context.Background()), "Recorder 手动 flush")
				return
			}
			require.Equal(t, http.StatusPaymentRequired, recw.Code, "body=%s", recw.Body.String())
			require.Contains(t, recw.Body.String(), "insufficient balance", "402 文案说明余额不足")
			require.Zero(t, hits.Load(), "预检拒绝不得转发上游")
			ri, ok := p.sched.Runtime(1)
			require.True(t, ok)
			require.Zero(t, ri.Concurrency, "预检在 Acquire 前：不占用并发槽")
			require.Zero(t, p.rec.Pending(), "预用量拒绝不产生明细 pending")

			require.NoError(t, f.Close(context.Background()))
			writer.mu.Lock()
			require.Empty(t, writer.calls, "预用量拒绝不进 billed flusher（无扣费无计费日志）")
			writer.mu.Unlock()
			require.NoError(t, rec.Close(context.Background()), "Recorder 手动 flush")
		})
	}
}

// TestProxyBillingRoutesToFlusher shouldBill 路由切换（评审 C-4）：billed 日志
// 只进 flusher（Recorder 明细管道零残留——每日志恰好一个写者），扣费按聚合 cost 落库
// + 余额快照定向刷新。
func TestProxyBillingRoutesToFlusher(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, store, nil)
	writer := &fakeDeductWriter{}
	bal := billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{1: 50000}}, nil)
	require.NoError(t, bal.Reload(context.Background()), "快照加载（余额 50000）")
	f := billing.NewFlusher(billing.FlushConfig{
		FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour,
	}, writer, rec, bal, nil)
	p := newTestProxyBillingT3Logs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}}, bal, f, rec)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	recw := httptest.NewRecorder()
	p.HandleChat(recw, req)
	require.Equal(t, 200, recw.Code, "body=%s", recw.Body.String())
	require.Zero(t, p.rec.Pending(), "billed 日志不进 Recorder 明细管道（每日志恰好一个写者）")

	require.NoError(t, f.Close(context.Background())) // 未 Start：排空 + 全量 flush
	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Len(t, writer.calls, 1)
	require.Equal(t, int64(1), writer.calls[0].userID)
	require.Equal(t, int64(130), writer.calls[0].cost, "3×1e7+5×2e7 → 130 毫分")
	require.Len(t, writer.calls[0].logs, 1)
	require.Equal(t, int64(130), writer.calls[0].logs[0].Cost)
	require.Equal(t, int64(1), writer.calls[0].logs[0].UserID)
	got, ok := bal.BalanceOf(1)
	require.True(t, ok)
	require.Equal(t, int64(900000), got, "扣费成功 → 余额快照定向刷新")
}

// ---------------------------------------------------------------------------
// T3.5：价格倍率（applyMultiplier 纯函数 + 按组倍率应用 + 免费放行）
// ---------------------------------------------------------------------------

// TestApplyMultiplier 倍率纯函数表驱动：×2 上浮 / ×0.5 折扣 round（奇数 cost
// 验证四舍五入）/ 0 免费 / ×10 上限 / m==10000 恒等短路。
func TestApplyMultiplier(t *testing.T) {
	cases := []struct {
		name string
		cost int64
		m    int
		want int64
	}{
		{"×2 上浮", 130, 20000, 260},
		{"×1.5 精确", 130, 15000, 195},
		{"×0.5 折扣 half-up（131×0.5=65.5 → 66）", 131, 5000, 66},
		{"×0.5 折扣 half-up（129×0.5=64.5 → 65）", 129, 5000, 65},
		{"0 免费", 130, 0, 0},
		{"×10 上限", 130, 100000, 1300},
		{"m==10000 恒等", 130, 10000, 130},
		{"0 成本 × 倍率恒 0", 0, 20000, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, applyMultiplier(c.cost, c.m))
		})
	}
}

// TestProxyBillingMultiplierAssignment 用户-组专属倍率（T3.5 修正：按组挂载，
// 用户覆盖组）：(1,10) ×2 → 扣费 cost 翻倍（130×2 = 260），billed 路由 +
// 快照刷新照常。
func TestProxyBillingMultiplierAssignment(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, store, nil)
	writer := &fakeDeductWriter{}
	bal := billing.NewBalances(fakeBalanceLoader{
		m:  map[int64]int64{1: 50000},
		am: map[billing.AssignmentKey]int{{UserID: 1, GroupID: 10}: 20000},
	}, nil)
	require.NoError(t, bal.Reload(context.Background()), "快照加载（余额 50000 + 用户-组倍率 ×2）")
	f := billing.NewFlusher(billing.FlushConfig{
		FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour,
	}, writer, rec, bal, nil)
	p := newTestProxyBillingT3Logs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}}, bal, f, rec)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	recw := httptest.NewRecorder()
	p.HandleChat(recw, req)
	require.Equal(t, 200, recw.Code, "body=%s", recw.Body.String())

	require.NoError(t, f.Close(context.Background()))
	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Len(t, writer.calls, 1)
	require.Equal(t, int64(260), writer.calls[0].cost, "用户-组专属倍率 ×2：130×2 = 260 毫分")
	require.Equal(t, int64(260), writer.calls[0].logs[0].Cost)
}

// newTestProxyBillingKeys 构造注入完整计费钩子 + 自定义 key→(user,group) 映射
// 的测试代理（多组场景：同用户不同组不同倍率；accs 必须含所有组的账号）。
func newTestProxyBillingKeys(t *testing.T, keys map[string]domain.KeyMeta, accs map[int64][]*domain.Account, bal *billing.Balances, f *billing.Flusher, logs usage.LogInserter) *Proxy {
	t.Helper()
	cfg := Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
		GroupKeyRPM:           0, UsageCapture: true,
	}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	sched := scheduler.New(scheduler.Config{
		DefaultMaxConcurrency: 4, SyncInterval: time.Hour,
	}, noopLoader{accs: accs}, re, nil)
	require.NoError(t, sched.InvalidateAllSync())
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, logs, nil)
	auth := NewAuth(noopKeyLoader{keys: keys}, noopUserLoader{}, nil)
	require.NoError(t, auth.Reload(context.Background())) // 构造不再自载——测试显式首刷（快照注册表单一入口）
	hc := &http.Client{Transport: http.DefaultTransport}
	clients := aiclient.NewFactory(hc, aiclient.Config{
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
	})
	p := New(cfg, sched, credential.New(), rec, clients, auth, nil, &BillingHooks{
		Prices:   &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}},
		Balances: bal, Flusher: f,
	}, nil)
	p.cfg.BillingCapture = true
	return p
}

// TestProxyBillingMultiplierPerGroup 同用户不同组不同倍率（T3.5 修正核心：
// 专属倍率按组挂载——assignment (1,10)=×2 与 (1,11)=×0.5 互不覆盖）。每组
// 独立 proxy+flusher（flusher 按用户聚合扣费，同用户两请求须分 flusher 落库
// 才能分开断言；倍率快照同一份，含两组 assignment）。
func TestProxyBillingMultiplierPerGroup(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	bal := billing.NewBalances(fakeBalanceLoader{
		m: map[int64]int64{1: 50000},
		am: map[billing.AssignmentKey]int{
			{UserID: 1, GroupID: 10}: 20000, // gk-1 → 组 10：×2
			{UserID: 1, GroupID: 11}: 5000,  // gk-2 → 组 11：×0.5
		},
	}, nil)
	require.NoError(t, bal.Reload(context.Background()))
	acc := &domain.Account{ID: 1, TemplateID: 1, Template: &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
	}, UpstreamKey: "sk-upstream", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4}

	// 组 10：gk-1 → assignment ×2 → 130×2 = 260
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, store, nil)
	w1 := &fakeDeductWriter{}
	f1 := billing.NewFlusher(billing.FlushConfig{
		FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour,
	}, w1, rec, bal, nil)
	p1 := newTestProxyBillingKeys(t, map[string]domain.KeyMeta{
		cryptox.HashKey("gk-1"): activeKey(1, 1, 10),
	}, map[int64][]*domain.Account{10: {acc}}, bal, f1, store)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	r.Header.Set("Authorization", "Bearer gk-1")
	rr := httptest.NewRecorder()
	p1.HandleChat(rr, r)
	require.Equal(t, 200, rr.Code, "body=%s", rr.Body.String())
	require.NoError(t, f1.Close(context.Background()))
	w1.mu.Lock()
	require.Len(t, w1.calls, 1)
	require.Equal(t, int64(260), w1.calls[0].cost, "组 10 专属倍率 ×2：130×2 = 260")
	w1.mu.Unlock()

	// 组 11：gk-2 → assignment ×0.5 → 130×0.5 = 65（同用户不同组互不覆盖）
	w2 := &fakeDeductWriter{}
	f2 := billing.NewFlusher(billing.FlushConfig{
		FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour,
	}, w2, rec, bal, nil)
	p2 := newTestProxyBillingKeys(t, map[string]domain.KeyMeta{
		cryptox.HashKey("gk-2"): activeKey(2, 1, 11),
	}, map[int64][]*domain.Account{11: {acc}}, bal, f2, store)
	r2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	r2.Header.Set("Authorization", "Bearer gk-2")
	rr2 := httptest.NewRecorder()
	p2.HandleChat(rr2, r2)
	require.Equal(t, 200, rr2.Code, "body=%s", rr2.Body.String())
	require.NoError(t, f2.Close(context.Background()))
	w2.mu.Lock()
	defer w2.mu.Unlock()
	require.Len(t, w2.calls, 1)
	require.Equal(t, int64(65), w2.calls[0].cost, "组 11 专属倍率 ×0.5：130×0.5 = 65（同用户不同组）")
}

// TestProxyBillingMultiplierGroup 组倍率（用户未设置 → 用组倍率）：×1.5 →
// 130×15000/10000 = 195；用户未设置不落入用户覆盖分支。
func TestProxyBillingMultiplierGroup(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, store, nil)
	writer := &fakeDeductWriter{}
	bal := billing.NewBalances(fakeBalanceLoader{
		m: map[int64]int64{1: 50000}, gm: map[int64]int{10: 15000}, // gk-1 → groupID 10
	}, nil)
	require.NoError(t, bal.Reload(context.Background()), "快照加载（余额 50000 + 组倍率 ×1.5）")
	f := billing.NewFlusher(billing.FlushConfig{
		FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour,
	}, writer, rec, bal, nil)
	p := newTestProxyBillingT3Logs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}}, bal, f, rec)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	recw := httptest.NewRecorder()
	p.HandleChat(recw, req)
	require.Equal(t, 200, recw.Code, "body=%s", recw.Body.String())

	require.NoError(t, f.Close(context.Background()))
	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Len(t, writer.calls, 1)
	require.Equal(t, int64(195), writer.calls[0].cost, "组倍率 ×1.5：130×15000/10000 = 195 毫分")
}

// TestProxyBillingFreeUserPasses 免费用户放行（T3.5）：有效倍率 0 → 余额 0
// 不 402——正常转发，cost 0（放行路径语义：billed + none → 仍走 Flusher 计费
// 明细——usage_logs 落 cost=0 行，扣费 0；不因免费丢计费明细）。
func TestProxyBillingFreeUserPasses(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, store, nil)
	writer := &fakeDeductWriter{}
	bal := billing.NewBalances(fakeBalanceLoader{
		m:  map[int64]int64{1: 0},
		am: map[billing.AssignmentKey]int{{UserID: 1, GroupID: 10}: 0}, // 余额 0 + 专属倍率 0（免费）
	}, nil)
	require.NoError(t, bal.Reload(context.Background()), "快照加载（余额 0 + 免费倍率）")
	f := billing.NewFlusher(billing.FlushConfig{
		FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour,
	}, writer, rec, bal, nil)
	p := newTestProxyBillingT3Logs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}}, bal, f, rec)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	recw := httptest.NewRecorder()
	p.HandleChat(recw, req)
	require.Equal(t, 200, recw.Code, "body=%s", recw.Body.String(), "免费用户余额 0 不 402")
	require.Zero(t, p.rec.Pending(), "billed 日志不进 Recorder 明细管道")

	require.NoError(t, f.Close(context.Background()))
	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Len(t, writer.calls, 1)
	require.Zero(t, writer.calls[0].cost, "免费：cost 0 不扣费")
	require.Zero(t, writer.calls[0].logs[0].Cost)
	require.Equal(t, int64(1), writer.calls[0].logs[0].UserID)
}

// TestProxyBillingFreeGroupPasses 免费组放行（T3.5）：组倍率 0（用户未设置）
// → 余额 0 放行；与用户免费同判定（EffectiveMultiplier 共用）。放行路径语义：
// billed + none（cost=0）→ 仍走 Flusher 计费明细。
func TestProxyBillingFreeGroupPasses(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, store, nil)
	writer := &fakeDeductWriter{}
	bal := billing.NewBalances(fakeBalanceLoader{
		m: map[int64]int64{1: 0}, gm: map[int64]int{10: 0}, // gk-1 → groupID 10；组免费
	}, nil)
	require.NoError(t, bal.Reload(context.Background()))
	f := billing.NewFlusher(billing.FlushConfig{
		FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour,
	}, writer, rec, bal, nil)
	p := newTestProxyBillingT3Logs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}}, bal, f, rec)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	recw := httptest.NewRecorder()
	p.HandleChat(recw, req)
	require.Equal(t, 200, recw.Code, "body=%s", recw.Body.String(), "免费组余额 0 不 402")

	require.NoError(t, f.Close(context.Background()))
	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Len(t, writer.calls, 1)
	require.Zero(t, writer.calls[0].cost, "免费组：cost 0 不扣费")
	require.Zero(t, writer.calls[0].logs[0].Cost)
}

// TestProxyBillingFreeGroupSnapshotMissing 评审 I-1：快照缺失（Reload 滞后
// 窗口内用户无余额记录）但组免费（倍率 0）→ 放行不 402（此前只在 BalanceOf
// 命中时查倍率 → 免费组误 402）。缺失且非免费仍 402（见
// TestProxyBillingInsufficientBalance402）。
func TestProxyBillingFreeGroupSnapshotMissing(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, store, nil)
	writer := &fakeDeductWriter{}
	// 余额快照为空（用户 1 不在快照）+ 组免费（gk-1 → groupID 10）。
	bal := billing.NewBalances(fakeBalanceLoader{
		m: map[int64]int64{}, gm: map[int64]int{10: 0},
	}, nil)
	require.NoError(t, bal.Reload(context.Background()))
	f := billing.NewFlusher(billing.FlushConfig{
		FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour,
	}, writer, rec, bal, nil)
	p := newTestProxyBillingT3Logs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}}, bal, f, rec)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	recw := httptest.NewRecorder()
	p.HandleChat(recw, req)
	require.Equal(t, 200, recw.Code, "body=%s", recw.Body.String(), "快照缺失窗口内免费组不 402")

	require.NoError(t, f.Close(context.Background()))
	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Len(t, writer.calls, 1)
	require.Zero(t, writer.calls[0].cost, "免费组：cost 0 不扣费")
	require.Zero(t, writer.calls[0].logs[0].Cost)
}

// TestProxyBillingNewUserImmediatelyUsable 评审 M-2 回归：新建用户（store 插入）
// → 全量 Reload → 立即请求 → 200（不得 402）。O1 前 Set 兜底补入新用户掩盖了
// 该窗口；O1 后 Set 仅限已存在条目（缺失忽略）——新用户必须经 Reload 进快照
// （创建路径不走 Set）。窗口显式暴露：创建前快照缺失 → 402（不用 sleep 掩盖）。
func TestProxyBillingNewUserImmediatelyUsable(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, store, nil)
	writer := &fakeDeductWriter{}
	loader := &fakeBalanceLoader{m: map[int64]int64{}} // 用户 1 尚未创建
	bal := billing.NewBalances(loader, nil)
	require.NoError(t, bal.Reload(context.Background()))
	f := billing.NewFlusher(billing.FlushConfig{
		FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour,
	}, writer, rec, bal, nil)
	p := newTestProxyBillingT3Logs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}}, bal, f, rec)

	req := func() int {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
		r.Header.Set("Authorization", "Bearer gk-1")
		rr := httptest.NewRecorder()
		p.HandleChat(rr, r)
		return rr.Code
	}
	// 窗口显式暴露：新用户未入快照 → 402（不得 sleep 掩盖）
	require.Equal(t, http.StatusPaymentRequired, req(), "创建前快照缺失 → 402 窗口如实暴露")

	// 用户创建 → invalidate → 全量 Reload（创建路径不走 Set）
	loader.m[1] = 50000
	require.NoError(t, bal.Reload(context.Background()))

	require.Equal(t, 200, req(), "新建用户 Reload 后立即请求不得 402（评审 M-2）")
	require.NoError(t, f.Close(context.Background()))
}
