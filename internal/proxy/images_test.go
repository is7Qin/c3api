// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	enterrlog "github.com/is7qin/c3api/internal/ent/errlog"
	entusagelog "github.com/is7qin/c3api/internal/ent/usagelog"
)

// --- 假上游：images 端点（/v1/images/generations|edits） ---
// 断言请求面（路径/鉴权/Content-Type/body），返回标准 ImageResponse（非流式）
// 或 SSE（stream=true）。

type imagesUpstreamCapture struct {
	mu          sync.Mutex
	calls       int
	path        string
	auth        string
	contentType string
	body        []byte
}

func fakeImagesUpstream(t *testing.T, wantPath string) (*httptest.Server, *imagesUpstreamCapture) {
	t.Helper()
	c := &imagesUpstreamCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.calls++
		c.path = r.URL.Path
		c.auth = r.Header.Get("Authorization")
		c.contentType = r.Header.Get("Content-Type")
		c.body = b
		c.mu.Unlock()
		if r.URL.Path != wantPath {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-upstream" {
			w.WriteHeader(401)
			return
		}
		if gjson.GetBytes(b, "stream").Type == gjson.True {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fl := w.(http.Flusher)
			fmt.Fprintf(w, "data: %s\n\n", `{"type":"image_generation.completed","data":[{"b64_json":"QUJD"}]}`)
			fl.Flush()
			fmt.Fprint(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"created": 1720000000,
			"data": []any{
				map[string]any{"b64_json": "QUJD"},
				map[string]any{"url": "https://upstream.example/1.png"},
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

// newTestImagesProxy 构造 images 格式模板测试代理（api_key 类型 + images
// 格式；bill 可注入计费钩子）。capture 为捕获落库（用量断言）。
func newTestImagesProxy(t *testing.T, upstream string, bill *BillingHooks) (*Proxy, *captureLogStore) {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIImages},
		Models:           []string{"gpt-image-1"},
	}
	store := &captureLogStore{}
	return newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, bill), store
}

// fakeImagePriceLookup 内存 image 价快照（PriceResolver 实现；缺失 → false）。
type fakeImagePriceLookup struct {
	mu       sync.Mutex
	entries  map[string]*domain.PriceEntry
	variants map[string][]*domain.PriceVariant
}

func (f *fakeImagePriceLookup) ResolvePrices(model string, promptTokens int64, tier string, at time.Time) (domain.ResolvedPrices, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[model]
	if !ok {
		return domain.ResolvedPrices{}, false
	}
	return domain.ResolveEntryPrices(e, f.variants[model], tier, promptTokens, at)
}

func perImagePriceRow(model string) *domain.PriceEntry {
	return &domain.PriceEntry{
		Model: model, Mode: domain.PriceModeImage,
		PricePerImage: ptr(int64(5400)),
		Source:        domain.PricingSourceManual,
	}
}

func ptr[T any](v T) *T { return &v }

// TestImagesGenerationsJSONDirect generations JSON 非流式直连：api_key 类型
// 模板 → 转发模板 base_url + /v1/images/generations；JSON 形态 setModel 模型
// 映射改写（客户端 img-1 → 上游 gpt-image-1）；响应原样透传。
func TestImagesGenerationsJSONDirect(t *testing.T) {
	up, c := fakeImagesUpstream(t, "/v1/images/generations")
	defer up.Close()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIImages},
		Models:           []string{"gpt-image-1"},
		ModelMapping:     map[string]string{"img-1": "gpt-image-1"},
	}
	store := &captureLogStore{}
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(
		`{"model":"img-1","prompt":"a cat","n":2}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleImagesGenerations(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"b64_json":"QUJD"`, "上游响应原样透传")
	require.Contains(t, rec.Body.String(), `"url":"https://upstream.example/1.png"`)
	c.mu.Lock()
	require.Equal(t, "/v1/images/generations", c.path)
	require.Equal(t, "Bearer sk-upstream", c.auth)
	require.Equal(t, "gpt-image-1", gjson.GetBytes(c.body, "model").String(), "JSON 形态 setModel 映射改写")
	require.Equal(t, "a cat", gjson.GetBytes(c.body, "prompt").String(), "其余字段原样")
	c.mu.Unlock()

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, domain.FormatOpenAIImages, store.logs[0].Format, "images 请求落 usage_logs 格式 openai-images")
	require.Equal(t, "img-1", store.logs[0].Model, "日志 Model = 客户端请求模型")
	require.Equal(t, "gpt-image-1", store.logs[0].MappedModel, "日志 MappedModel = 映射后模型")
}

// TestImagesEditsJSONDirect edits JSON 非流式直连（generations 之外的第二个
// 端点：上游子路径 /v1/images/edits）。
func TestImagesEditsJSONDirect(t *testing.T) {
	up, c := fakeImagesUpstream(t, "/v1/images/edits")
	defer up.Close()
	p, _ := newTestImagesProxy(t, up.URL, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(
		`{"model":"gpt-image-1","image":"https://example.com/in.png"}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleImagesEdits(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	c.mu.Lock()
	require.Equal(t, "/v1/images/edits", c.path)
	require.Equal(t, "Bearer sk-upstream", c.auth)
	require.Equal(t, "gpt-image-1", gjson.GetBytes(c.body, "model").String())
	c.mu.Unlock()
	require.NoError(t, p.rec.Close(context.Background()))
}

// TestImagesMultipartHardGateSkippedAndPassthrough multipart 专用 body 分支
// （P1-2）：body 为非 JSON（含图片文件字节）→ 必须 200（json.Valid 硬门对
// multipart 跳过——不跳过则 400 误杀）；body 字节与 Content-Type（含
// boundary）原样透传上游；model 从 form 字段取（映射不回写——form model
// 原样透传）。
func TestImagesMultipartHardGateSkippedAndPassthrough(t *testing.T) {
	up, c := fakeImagesUpstream(t, "/v1/images/edits")
	defer up.Close()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIImages},
		Models:           []string{"gpt-image-1"},
		ModelMapping:     map[string]string{"img-1": "gpt-image-1"},
	}
	p := newTestProxyTplCapture(t, tpl, 1, true)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	require.NoError(t, mw.WriteField("model", "img-1"))
	require.NoError(t, mw.WriteField("prompt", "make it red"))
	fw, err := mw.CreateFormFile("image", "photo.png")
	require.NoError(t, err)
	_, err = fw.Write([]byte("PNG-binary-junk-that-is-not-json"))
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	body := buf.Bytes()
	ct := mw.FormDataContentType()

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer ck-1")
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	p.HandleImagesEdits(rec, req)

	require.Equal(t, 200, rec.Code, "multipart 不得撞 json.Valid 硬门：body=%s", rec.Body.String())
	c.mu.Lock()
	require.Equal(t, 1, c.calls, "multipart 请求必须转发上游")
	require.Equal(t, ct, c.contentType, "multipart Content-Type（含 boundary）原样透传")
	require.Equal(t, body, c.body, "multipart body 字节原样透传（图片文件不解析不重写）")
	// 上游侧解析 form：model 为客户端原值（img-1，映射不回写——spec §5.1 声明）
	mr := multipart.NewReader(bytes.NewReader(c.body), boundaryOf(ct))
	formModel := ""
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		if part.FormName() == "model" {
			b, _ := io.ReadAll(part)
			formModel = string(b)
		}
	}
	require.Equal(t, "img-1", formModel, "multipart 形态不做 setModel 改写（form model 原样透传）")
	c.mu.Unlock()
	require.NoError(t, p.rec.Close(context.Background()))
}

func boundaryOf(ct string) string {
	_, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return ""
	}
	return params["boundary"]
}

// TestImagesNoImagePrice402 生死判定（空行语义）：计费启用且 image_price 快照
// 无该模型行 → 402，上游一个请求都不许收到（对齐 chat 缺价预检语义，不按
// 0 计价）。
func TestImagesNoImagePrice402(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(500)
	}))
	defer up.Close()
	p := newTestImagesProxyWithBill(t, up.URL, &BillingHooks{
		Resolver: &fakeImagePriceLookup{entries: map[string]*domain.PriceEntry{}},
	}, &captureLogStore{})

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-1","prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleImagesGenerations(rec, req)

	require.Equal(t, http.StatusPaymentRequired, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "no price", "402 文案说明缺价")
	require.Zero(t, hits.Load(), "缺价不得转发上游")
	require.NoError(t, p.rec.Close(context.Background()))
}

// TestImagesPureImageModelNotKilledByChatPrecheck P1-1 核心断言：纯 image 价
// 模型（aiml 形态——仅 per-image 分量，无文本价 → 无 pricings 行）在 images
// 端点不被 chat 价预检（GetPrice）误杀——预检按格式切换：images 查
// GetImagePrice（有行 → 放行），跳过 GetPrice（空 chat 价表 → 修复前 402）。
func TestImagesPureImageModelNotKilledByChatPrecheck(t *testing.T) {
	up, c := fakeImagesUpstream(t, "/v1/images/generations")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestImagesProxyWithBill(t, up.URL, &BillingHooks{
		Resolver: &fakeImagePriceLookup{entries: map[string]*domain.PriceEntry{"gpt-image-1": perImagePriceRow("gpt-image-1")}},
		Balances: billingBalances(),
	}, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-1","prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleImagesGenerations(rec, req)

	require.Equal(t, 200, rec.Code, "纯 image 价模型不得被 chat 价预检误杀：body=%s", rec.Body.String())
	require.Equal(t, 1, c.calls, "预检通过 → 正常转发")
	require.NoError(t, p.rec.Close(context.Background()))
	// T2 起 images 日志走 applyImageBilling（价格快照有行 → 不 no_price 标记、
	// 不落 chat 价快照列）。T2 P3-4 起直连路径接入 usage 提取（data 长 =
	// 张数——fake 上游返回 2 元素 data、无 usage → 无 token 分量，per-image
	// 分量照算）——不按 0 计价的断言面 = no_price 标记不出现（有价行）+ Cost
	// 含 image 分量。
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, int64(2), store.logs[0].CallCount, "直连路径张数 = data 长（入 call_count）")
	require.Equal(t, int64(2*5400), store.logs[0].Cost, "直连路径 ImageCost per-image 分量（5400 毫分/张 × 2）")
	require.Equal(t, "auto", store.logs[0].BillingTier, "有价行 → service_tier 归一化照常（不 no_price）")
	require.Nil(t, store.logs[0].PriceInputMillis, "images 日志不落 chat 价快照列（nil）")
	require.NotNil(t, store.logs[0].PricePerCallMillis, "有张数 → per-image 价格快照落列")
	require.Equal(t, int64(5400), *store.logs[0].PricePerCallMillis)
}

// TestImagesDirectUsageExtractionBilling T2 P3-4 直连路径 usage 提取断言：api_key
// 直连 /v1/images/generations（Task B 路径）计费含 image 分量——上游响应带
// 嵌套 usage image_tokens（与 codexTestImageResponse 同 wire 形态）→
// CallCount = data 长 + ImageInput/OutputTokens = image_tokens（并入 in/out）+ ImageCost
// 落账（与 codexImagesCaller 同口径——同一 ImageUsageFromResponse 纯函数，
// 断言字段与 TestImagesCodexGenerationsOK 逐项对齐）。直连路径零改写不在此
// 测（TestImagesGenerationsJSONDirect 已钉原样透传）。
func TestImagesDirectUsageExtractionBilling(t *testing.T) {
	// 与 codexTestImageResponse 同形态（2 张 + image_tokens 1/2——同口径比对基准）
	const body = `{"created":1720000000,"data":[{"b64_json":"QUJD"},{"b64_json":"REVG"}],"usage":{"input_tokens":2,"output_tokens":3,"input_tokens_details":{"image_tokens":1},"output_tokens_details":{"image_tokens":2}}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer up.Close()
	store := &captureLogStore{}
	p := newTestImagesProxyWithBill(t, up.URL, &BillingHooks{
		Resolver: &fakeImagePriceLookup{entries: map[string]*domain.PriceEntry{"gpt-image-1": perImagePriceRow("gpt-image-1")}},
		Balances: billingBalances(),
	}, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-1","prompt":"a cat","n":2}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleImagesGenerations(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, body, rec.Body.String(), "直连响应原样透传（提取与转发共用同一字节）")
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	l := store.logs[0]
	require.Equal(t, domain.FormatOpenAIImages, l.Format)
	require.Equal(t, int64(2), l.CallCount, "张数 = data 长（与 codex 路径同口径）")
	require.Equal(t, int64(1), l.InputTokens, "usage image_tokens 输入（与 codex 路径同口径）")
	require.Equal(t, int64(2), l.OutputTokens, "usage image_tokens 输出（与 codex 路径同口径）")
	require.Equal(t, int64(3), l.TotalTokens, "TotalTokens = image tokens 之和（张数不入）")
	require.Equal(t, int64(2*5400), l.Cost, "ImageCost per-image 分量（5400 毫分/张 × 2）")
	require.NotNil(t, l.PricePerCallMillis, "per-image 价格快照落列")
	require.Equal(t, int64(5400), *l.PricePerCallMillis)
}

// TestImagesNoImagePriceWhenImagePricesNil bill 装配但 ImagePrices 未注入（未
// 装配形态）→ 不预检（等价计费全关），请求放行。
func TestImagesNoImagePriceWhenImagePricesNil(t *testing.T) {
	up, c := fakeImagesUpstream(t, "/v1/images/generations")
	defer up.Close()
	p := newTestImagesProxyWithBill(t, up.URL, &BillingHooks{
		Balances: billingBalances(),
	}, &captureLogStore{})

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-1","prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleImagesGenerations(rec, req)
	require.Equal(t, 200, rec.Code, "ImagePrices 未装配不预检：body=%s", rec.Body.String())
	require.Equal(t, 1, c.calls)
	require.NoError(t, p.rec.Close(context.Background()))
}

// TestImagesStreamingSSE 流式直连透传：JSON stream=true → 上游 SSE 原样透传
// （首事件 + [DONE]），模型改写照常（JSON 形态）。
func TestImagesStreamingSSE(t *testing.T) {
	up, c := fakeImagesUpstream(t, "/v1/images/generations")
	defer up.Close()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIImages},
		Models:           []string{"gpt-image-1"},
		ModelMapping:     map[string]string{"img-1": "gpt-image-1"},
	}
	p := newTestProxyTplCapture(t, tpl, 1, true)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(
		`{"model":"img-1","prompt":"cat","stream":true}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleImagesGenerations(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	body := rec.Body.String()
	require.Contains(t, body, "data: [DONE]")
	require.Contains(t, body, `"type":"image_generation.completed"`, "上游 SSE 事件原样透传")
	c.mu.Lock()
	require.Equal(t, "gpt-image-1", gjson.GetBytes(c.body, "model").String(), "流式 JSON 同样模型改写")
	c.mu.Unlock()
	require.NoError(t, p.rec.Close(context.Background()))
}

// TestImagesStreamDirectBilling A-P1-2 直连流式 images 计费接线：api_key 模板 +
// stream:true 上游 SSE completed 帧（含 usage image_tokens）→ Observer 逐帧提取
// → 流终落账非零——count = completed 帧数累积、ii/io 取最后一个 completed 帧的
// usage（覆盖语义，对齐 codex 流式路径——累积求和多图差 N 倍）、ImageCost
// per-image 分量照算（修复前整单 usageTuple{} 免费）。
func TestImagesStreamDirectBilling(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", `{"type":"image_generation.completed","data":[{"b64_json":"QUJD"}],"usage":{"input_tokens_details":{"image_tokens":1000},"output_tokens_details":{"image_tokens":4000}}}`)
		fl.Flush()
		fmt.Fprintf(w, "data: %s\n\n", `{"type":"image_generation.completed","data":[{"b64_json":"REVG"}],"usage":{"input_tokens_details":{"image_tokens":2000},"output_tokens_details":{"image_tokens":8000}}}`)
		fl.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer up.Close()
	store := &captureLogStore{}
	p := newTestImagesProxyWithBill(t, up.URL, &BillingHooks{
		Resolver: &fakeImagePriceLookup{entries: map[string]*domain.PriceEntry{"gpt-image-1": perImagePriceRow("gpt-image-1")}},
		Balances: billingBalances(),
	}, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(
		`{"model":"gpt-image-1","prompt":"a cat","stream":true}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleImagesGenerations(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "data: [DONE]", "SSE 原样透传")
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	l := store.logs[0]
	require.Equal(t, domain.FormatOpenAIImages, l.Format)
	require.Equal(t, domain.ErrNone, l.ErrorType)
	require.Equal(t, int64(2), l.CallCount, "count = completed 帧数累积（partial/DONE 不计）")
	require.Equal(t, int64(2000), l.InputTokens, "usage 取末个 completed 帧（覆盖语义）")
	require.Equal(t, int64(8000), l.OutputTokens)
	require.Equal(t, int64(10000), l.TotalTokens, "tt = image tokens 之和（张数不入）")
	require.Equal(t, int64(2*5400), l.Cost, "per-image 分量照算（修复前恒 0——整单免费）")
	require.NotNil(t, l.PricePerCallMillis)
	require.Equal(t, int64(5400), *l.PricePerCallMillis)
}

// TestImagesCodexNotIntegrated501 codex 分流骨架：codex-oauth 模板在 images
// 端点选号命中 → 501 明确"未接入"（SDK 调用 T2/T3 接；未接入前不得误报
// 502/network），上游不收请求；评审 P2-1：post-Select 拒绝必须 recordRejected
// 留 err_logs 审计（error_type=billing、StatusCode=501、文案落 ErrorMessage）。
func TestImagesCodexNotIntegrated501(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
	}))
	defer up.Close()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIImages},
		Models:           []string{"gpt-image-1"},
	}
	store := &captureLogStore{}
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-1","prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleImagesGenerations(rec, req)

	require.Equal(t, http.StatusNotImplemented, rec.Code, "codex 未接入必须显式 501：body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "adapter not wired", "501 文案 = 装配缺失")
	require.Zero(t, hits.Load(), "未接入不得转发上游")
	require.NoError(t, p.rec.Close(context.Background()))
	// P2-1：err_logs 审计断言（拒绝行走 errlog worker，Close 显式排空）
	require.NoError(t, p.errlog.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1, "501 拒绝必须记一条 err_logs")
	l := store.logs[0]
	require.Equal(t, domain.ErrBilling, l.ErrorType, "post-Select 拒绝 error_type=billing（与 402 缺价预检同型）")
	require.Equal(t, http.StatusNotImplemented, l.StatusCode)
	require.Equal(t, domain.FormatOpenAIImages, l.Format)
	require.Equal(t, "gpt-image-1", l.Model)
	require.NotNil(t, l.ErrorMessage)
	require.Contains(t, *l.ErrorMessage, "adapter not wired", "文案落 ErrorMessage（域内截断 500）")
}

// TestImagesCodexPATNotIntegrated501 codex-pat 同 codex-oauth（分流骨架两类型
// 一并覆盖；审计断言同上）。
func TestImagesCodexPATNotIntegrated501(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer up.Close()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeCodexPAT,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIImages},
		Models:           []string{"gpt-image-1"},
	}
	store := &captureLogStore{}
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(`{"model":"gpt-image-1"}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleImagesEdits(rec, req)
	require.Equal(t, http.StatusNotImplemented, rec.Code, "body=%s", rec.Body.String())
	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1, "codex-pat 501 同样记 err_logs")
	require.Equal(t, domain.ErrBilling, store.logs[0].ErrorType)
	require.Equal(t, http.StatusNotImplemented, store.logs[0].StatusCode)
}

// TestImagesResponsesSpecialDirect responses-special 类型直连（用户裁决：两
// 类型都支持两个端点）：凭据取用成功（P4 502 消灭同款——注册表必须含
// responses-special provider）+ 上游收到账号 key。
func TestImagesResponsesSpecialDirect(t *testing.T) {
	up, c := fakeImagesUpstream(t, "/v1/images/generations")
	defer up.Close()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeResponsesSpecial,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIImages},
		Models:           []string{"gpt-image-1"},
	}
	p := newTestProxyTplCapture(t, tpl, 1, true)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-1","prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleImagesGenerations(rec, req)

	require.Equal(t, 200, rec.Code, "responses-special 直连必须可用：body=%s", rec.Body.String())
	c.mu.Lock()
	require.Equal(t, "Bearer sk-upstream", c.auth, "responses-special 凭据 = 账号 upstream_key")
	c.mu.Unlock()
	require.NoError(t, p.rec.Close(context.Background()))
}

// TestImagesFormatValidatorOpenaiImages 枚举扩展（spec §4.3）：usage_logs /
// err_logs 的 format 枚举必须接受 openai-images（否则 images 请求落账 COPY
// 恒失败——评审 D4）。
func TestImagesFormatValidatorOpenaiImages(t *testing.T) {
	require.NoError(t, entusagelog.FormatValidator(entusagelog.Format(domain.FormatOpenAIImages)))
	require.NoError(t, enterrlog.FormatValidator(enterrlog.Format(domain.FormatOpenAIImages)))
	require.Equal(t, entusagelog.FormatOpenaiImages, entusagelog.Format(domain.FormatOpenAIImages))
}

// TestImagesMultipartModelField extraction 单测：model 从 form 字段取（图片
// 文件 part 跳过）；缺失 → ""；无 boundary → ""。
func TestImagesMultipartModelField(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("prompt", "x")
	fw, _ := mw.CreateFormFile("image", "p.png")
	_, _ = fw.Write([]byte("file-bytes"))
	_ = mw.WriteField("model", "  gpt-image-1  ")
	_ = mw.Close()
	body := buf.Bytes()
	require.Equal(t, "gpt-image-1", imagesMultipartModel(body, mw.FormDataContentType()), "form model 字段提取（去空白）")

	// 缺失 model
	var buf2 bytes.Buffer
	mw2 := multipart.NewWriter(&buf2)
	_ = mw2.WriteField("prompt", "x")
	_ = mw2.Close()
	require.Equal(t, "", imagesMultipartModel(buf2.Bytes(), mw2.FormDataContentType()), "无 model 字段 → 空")

	// 无 boundary（非法 Content-Type）
	require.Equal(t, "", imagesMultipartModel(body, "multipart/form-data"), "boundary 缺失 → 空")
	require.False(t, isMultipartForm("application/json"), "JSON 不是 multipart")
	require.True(t, isMultipartForm(mw.FormDataContentType()), "multipart/form-data 判定")
}

// newTestImagesProxyWithBill 构造 images 模板 + 注入计费钩子的测试代理。
func newTestImagesProxyWithBill(t *testing.T, upstream string, bill *BillingHooks, store *captureLogStore) *Proxy {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIImages},
		Models:           []string{"gpt-image-1"},
	}
	return newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, bill)
}

func billingBalances() *billing.Balances {
	return billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{}}, nil)
}
