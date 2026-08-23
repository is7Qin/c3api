// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

// raw_cost 列行为契约测试（spec 2026-08-18，gate 修订版）：倍率 3 路径
// （chat/image/search）各一例 raw=倍率前、cost=倍率后 + 行为契约四态（billed
// 双值 / 免费组 cost=0 raw>0 / 非 billed 行照填 / bill 未装配恒 0）。search
// 红绿断言：raw 必须 = 显式表达式（按次价 × call_count）——该时刻 l.Cost 恒
// 0，误读字段作 raw 则 search 全量 raw=0 即红。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/usage"
)

// rawCostChatProxy 构造 chat 计费测试代理（billed 路由 flusher + 捕获 store；
// gm 为组 10 倍率——ck-1 → (user 1, group 10)）。
func rawCostChatProxy(t *testing.T, gm int) (*Proxy, *billing.Flusher, *fakeDeductWriter, *captureLogStore) {
	t.Helper()
	up := fakeOpenAI(t, "")
	t.Cleanup(up.Close)
	store := &captureLogStore{}
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, store, nil)
	writer := &fakeDeductWriter{}
	bal := billing.NewBalances(fakeBalanceLoader{
		m: map[int64]int64{1: 50000}, gm: map[int64]int{10: gm},
	}, nil)
	require.NoError(t, bal.Reload(context.Background()))
	f := billing.NewFlusher(billing.FlushConfig{
		FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour,
	}, writer, rec, bal, nil)
	p := newTestProxyBillingT3Logs(t, up.URL, &fakePriceLookup{entries: map[string]*domain.PriceEntry{"gpt-4o": proxyPricingEntry()}, variants: map[string][]*domain.PriceVariant{"gpt-4o": proxyPricingVariants()}}, bal, f, rec)
	return p, f, writer, store
}

// chatCostReq 发一次 gpt-4o chat 请求（3/5 tokens → 130 毫分倍率前）。
func chatCostReq(t *testing.T, p *Proxy) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	recw := httptest.NewRecorder()
	p.HandleChat(recw, req)
	require.Equal(t, 200, recw.Code, "body=%s", recw.Body.String())
}

// TestProxyRawCostChatMultiplier billed chat ×1.5（组倍率）：cost=倍率后 195、
// raw=倍率前 130（billed 双值——raw 原文、cost 乘后）。
func TestProxyRawCostChatMultiplier(t *testing.T) {
	p, f, writer, _ := rawCostChatProxy(t, 15000)
	chatCostReq(t, p)
	require.NoError(t, f.Close(context.Background()))
	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Len(t, writer.calls, 1)
	require.Equal(t, int64(195), writer.calls[0].cost, "组倍率 ×1.5：130×15000/10000 = 195")
	require.Equal(t, int64(195), writer.calls[0].logs[0].Cost)
	require.Equal(t, int64(130), writer.calls[0].logs[0].RawCost, "billed 行 raw = 倍率前 cost 原文")
}

// TestProxyRawCostChatFreeGroup 免费组（m=0）：cost=0、raw=130 照填（"实际
// 消耗"只有 raw 能看）。
func TestProxyRawCostChatFreeGroup(t *testing.T) {
	p, f, writer, _ := rawCostChatProxy(t, 0)
	chatCostReq(t, p)
	require.NoError(t, f.Close(context.Background()))
	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Len(t, writer.calls, 1)
	require.Zero(t, writer.calls[0].cost, "免费组 cost 0 不扣费")
	require.Zero(t, writer.calls[0].logs[0].Cost)
	require.Equal(t, int64(130), writer.calls[0].logs[0].RawCost, "免费组 cost=0 但 raw 有值")
}

// TestProxyRawCostImageMultiplier image 路径 ×1.5（用户-组专属倍率）：cost=
// 8100（1 张 × 5400 ×1.5）、raw=5400（ImageCost 原文——:291 赋值后累计）。
func TestProxyRawCostImageMultiplier(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(up.Close)
	store := &captureLogStore{}
	bal := billing.NewBalances(fakeBalanceLoader{
		am: map[billing.AssignmentKey]int{{UserID: 1, GroupID: 10}: 15000},
	}, nil)
	require.NoError(t, bal.Reload(context.Background()))
	hooks := &BillingHooks{
		Resolver: &fakePriceLookup{entries: imageTestPrices()},
		Balances: bal,
	}
	p := newTestProxyBillingLogs(t, up.URL, nil, nil, store)
	p.bill = hooks
	// 鉴权元数据入 ctx（logWithCtx 填 UserID——EffectiveMultiplier 用户覆盖面）。
	rm := &reqMeta{meta: domain.KeyMeta{UserID: 1, KeyID: 1}}
	ctx := context.WithValue(context.Background(), ctxKeyReqMeta{}, rm)
	r, rec := streamImageReq(t, nil)
	b64a := "aGVsbG8="
	events := []domain.ImageStreamEvent{{Type: domain.ImageStreamEventCompleted, B64JSON: &b64a}}
	code, _, _, err := p.streamImageGeneration(ctx, rec, r, "req-1", 10, time.Now(), streamImageSel(), "gpt-image-2", streamImageCred(), streamImageParams(), fakeStreamGen(events, nil, nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, code)
	l := collectImageLogs(t, p, store)
	require.Equal(t, int64(8100), l.Cost, "×1.5 组倍率作用于 image 分量 cost")
	require.Equal(t, int64(5400), l.RawCost, "raw = 倍率前 ImageCost 原文（1 张 × 5400）")
}

// TestProxyRawCostSearchMultiplier search 路径 ×1.5（组倍率）：cost=3750
// （2500×1.5）、raw=2500——红绿断言：raw 必须 = 显式表达式（按次价 ×
// call_count）；误读 l.Cost（该时刻恒 0）→ raw=0 即红。
func TestProxyRawCostSearchMultiplier(t *testing.T) {
	up, _ := newCodexSearchUpstream(t, codexSearchStep{status: 200, body: searchRespRaw})
	defer up.Close()
	store := &captureLogStore{}
	bal := billing.NewBalances(fakeBalanceLoader{
		m: map[int64]int64{1: 1_000_000}, gm: map[int64]int{10: 15000},
	}, nil)
	require.NoError(t, bal.Reload(context.Background()))
	bill := &BillingHooks{
		Resolver: &fakeFunctionPriceLookup{entries: map[string]*domain.PriceEntry{"codex-search": {Model: "codex-search", Mode: domain.PriceModeCall, PricePerCall: i64ptr(2500), Source: domain.PricingSourceManual}}},
		Balances: bal,
	}
	p, _ := newTestSearchProxy(t, []searchTestAcct{{id: 10, tplID: 1, credType: credential.TypeAPIKey, key: "sk-upstream"}},
		up.URL, bill, store)

	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()
	resp := postSearch(t, srv, searchReqBody, "")
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body=%s", string(b))

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	lg := store.logs[0]
	require.Equal(t, int64(3750), lg.Cost, "×1.5 组倍率：2500×15000/10000 = 3750")
	require.Equal(t, int64(2500), lg.RawCost, "search raw = 显式表达式（按次价 × call_count）；l.Cost 该时刻恒 0，误读即红")
}

// TestProxyRawCostNonBilledFilled 非 billed 行（UserID==0 但 bill 装配——
// gate 修订："非 billed 恒 0" 不实）：applyBilling 照算 → raw 照填（倍率前
// 原文），cost 乘后——helper 在 applyBilling 内天然覆盖，无额外条件。
func TestProxyRawCostNonBilledFilled(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	bal := billing.NewBalances(fakeBalanceLoader{
		m: map[int64]int64{1: 50000}, gm: map[int64]int{10: 15000},
	}, nil)
	require.NoError(t, bal.Reload(context.Background()))
	acc := &domain.Account{ID: 1, TemplateID: 1, Template: &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
	}, UpstreamKey: "sk-upstream", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4}
	p := newTestProxyBillingKeys(t, map[string]domain.KeyMeta{
		"ck-1": activeKey(1, 0, 10), // UserID 0 → shouldBill false（非 billed）
	}, map[int64][]*domain.Account{10: {acc}}, bal, nil, store)
	p.cfg.BillingCapture = false // 计费捕获关但钩子装配——applyBilling 照算、routeLog 走 rec

	chatCostReq(t, p)
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, int64(195), store.logs[0].Cost, "bill 装配 → 倍率 ×1.5 照算")
	require.Equal(t, int64(130), store.logs[0].RawCost, "非 billed 行 raw 照填（倍率前原文）")
}

// TestProxyRawCostBillUnassembledZero bill 未装配（计费全关）：cost/raw 恒 0。
func TestProxyRawCostBillUnassembledZero(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyTimeoutLogs(t, up.URL, 1, store) // bill nil

	chatCostReq(t, p)
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Zero(t, store.logs[0].Cost)
	require.Zero(t, store.logs[0].RawCost, "bill 未装配：cost/raw 恒 0")
}
