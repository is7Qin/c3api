// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/aiclient"
)

// --- resp/resp-ws 响应检测旁路（spec §6）：计数提取 + 开关矩阵 + 计费落账 ---

// respImagesOutput 假上游 resp 响应 output（V1-V3 wire 形态）：2 个
// image_generation_call item（终态 status="generating" 非 "completed"——
// status 不参与判定；result 为 base64 字符串）+ 1 个带 result 字段的
// function_call item（type 过滤后不误计断言载体）。
// 注意：单行紧凑 JSON——SSE data: 每物理行一条 payload（sserelay 只合并多
// data: 行），换行会截断 Observer 的 data 视图（真实上游恒单行紧凑）。
const respImagesOutput = `[{"type":"image_generation_call","id":"igc_1","status":"generating","result":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="},{"type":"image_generation_call","id":"igc_2","status":"generating","result":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="},{"type":"function_call","id":"fc_1","call_id":"call_1","name":"web_search","arguments":"{}","result":"{\"query\":\"x\"}"}]`

// respImagePriceRow 构造 ImagePrice 测试行（三分量指针）。
func respImagePriceRow(in, out, perImage *int64) *domain.ImagePrice {
	return &domain.ImagePrice{
		Model:                           "gpt-4o",
		InputImageTokenPricePerMillion:  in,
		OutputImageTokenPricePerMillion: out,
		OutputCostPerImageMilli:         perImage,
	}
}

// --- unit：计数提取 ---

func TestRespImageCount(t *testing.T) {
	completed := func(output string) []byte {
		return []byte(`{"type":"response.completed","response":{"id":"rsp_1","status":"completed","model":"m","output":` + output + `}}`)
	}

	t.Run("type+result 非空计数（status 不参与）", func(t *testing.T) {
		// 2 个 image_generation_call（status=generating 终态照常计数）+ function_call
		// 的 result 不误计 → 2
		require.Equal(t, int64(2), respImageCountCompleted(completed(respImagesOutput)))
	})

	t.Run("id 去重：同 id 重复只计 1", func(t *testing.T) {
		out := `[{"type":"image_generation_call","id":"igc_1","status":"completed","result":"aaa"},
		         {"type":"image_generation_call","id":"igc_1","status":"completed","result":"bbb"}]`
		require.Equal(t, int64(1), respImageCountCompleted(completed(out)))
	})

	t.Run("id 缺失：按出现顺序全数计入", func(t *testing.T) {
		out := `[{"type":"image_generation_call","status":"generating","result":"aaa"},
		         {"type":"image_generation_call","status":"generating","result":"bbb"}]`
		require.Equal(t, int64(2), respImageCountCompleted(completed(out)))
	})

	t.Run("result 缺失/null/空串/空数组 → 不计", func(t *testing.T) {
		cases := map[string]struct {
			out  string
			want int64
		}{
			"缺失":      {`[{"type":"image_generation_call","id":"a","status":"completed"}]`, 0},
			"null":    {`[{"type":"image_generation_call","id":"a","status":"completed","result":null}]`, 0},
			"空串":      {`[{"type":"image_generation_call","id":"a","status":"completed","result":""}]`, 0},
			"空数组":     {`[{"type":"image_generation_call","id":"a","status":"completed","result":[]}]`, 0},
			"混合空+非空":  {`[{"type":"image_generation_call","id":"a","result":null},{"type":"image_generation_call","id":"b","result":"x"}]`, 1},
			"其他工具不误计": {`[{"type":"web_search_call","id":"w","status":"completed","result":"x"}]`, 0},
		}
		for name, tc := range cases {
			t.Run(name, func(t *testing.T) {
				require.Equal(t, tc.want, respImageCountCompleted(completed(tc.out)))
			})
		}
	})

	t.Run("result 数组形态（image_url 对象数组）→ 计数", func(t *testing.T) {
		out := `[{"type":"image_generation_call","id":"a","status":"completed","result":[{"type":"image","image_url":"https://x/y.png"}]}]`
		require.Equal(t, int64(1), respImageCountCompleted(completed(out)))
	})

	t.Run("无 image item / 无 output → 0", func(t *testing.T) {
		require.Zero(t, respImageCountCompleted(completed(`[]`)))
		require.Zero(t, respImageCountCompleted(completed(`[{"type":"function_call","id":"f","name":"web_search","arguments":"{}","result":"x"}]`)))
		require.Zero(t, respImageCountCompleted([]byte(`{"type":"response.completed","response":{"id":"r"}}`)))
		require.Zero(t, respImageCountCompleted([]byte(`{"type":"response.output_text.delta","delta":"hi"}`)))
		// 形态错配（信封路径缺失）：completed 入口对裸响应体 → 0（调用方按形态直选）
		require.Zero(t, respImageCountCompleted([]byte(`{"id":"rsp_1","output":[]}`)))
	})

	t.Run("非流式响应体（无 response 信封，output 顶层）", func(t *testing.T) {
		raw := `{"id":"rsp_1","object":"response","status":"completed","model":"m","output":` + respImagesOutput + `}`
		require.Equal(t, int64(2), respImageCountBody([]byte(raw)))
		// 形态错配：body 入口对 completed 帧 → 0
		require.Zero(t, respImageCountBody(completed(respImagesOutput)))
	})
}

// --- unit：开关矩阵（spec §6） ---

func TestRespImageDetectOn(t *testing.T) {
	sel := func(typ credential.Type, strip bool) *scheduler.Selection {
		return &scheduler.Selection{CredentialType: typ, StripImageTools: strip}
	}
	// api_key 永不检测（strip 开/关均关）
	require.False(t, respImageDetectOn(sel(credential.TypeAPIKey, false)))
	require.False(t, respImageDetectOn(sel(credential.TypeAPIKey, true)))
	// responses-special / codex-oauth / codex-pat：strip 关 = 检测；开 = 不检测
	require.True(t, respImageDetectOn(sel(credential.TypeResponsesSpecial, false)))
	require.False(t, respImageDetectOn(sel(credential.TypeResponsesSpecial, true)))
	require.True(t, respImageDetectOn(sel(credential.TypeCodexOAuth, false)))
	require.False(t, respImageDetectOn(sel(credential.TypeCodexOAuth, true)))
	require.True(t, respImageDetectOn(sel(credential.TypeCodexPAT, false)))
	require.False(t, respImageDetectOn(sel(credential.TypeCodexPAT, true)))
}

// TestRespImageCountZeroAlloc 热路径零分配断言（评审 P2-1 修复钉住）：
// 检测开启时每 completed 帧 AllocsPerRun==0——含图帧、无图帧、空数组帧、
// output 缺失帧四形态全覆盖（gjson GetBytes 数组值物化 Raw 的 1 alloc 已随
// 字节扫描重写消除；断言回归即失败）。
func TestRespImageCountZeroAlloc(t *testing.T) {
	completed := func(output string) []byte {
		return []byte(`{"type":"response.completed","response":{"id":"rsp_1","status":"completed","model":"m","output":` + output + `}}`)
	}
	withImg := completed(respImagesOutput)
	noImg := completed(`[{"type":"function_call","id":"fc_1","name":"web_search","arguments":"{}","result":"x"},{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]`)
	empty := completed(`[]`)
	miss := []byte(`{"type":"response.completed","response":{"id":"r"}}`)
	raw := []byte(`{"id":"rsp_1","object":"response","status":"completed","model":"m","output":` + respImagesOutput + `}`)

	checks := []struct {
		name string
		f    func() int64
	}{
		{"含图帧（completed 信封）", func() int64 { return respImageCountCompleted(withImg) }},
		{"无图帧", func() int64 { return respImageCountCompleted(noImg) }},
		{"空数组帧", func() int64 { return respImageCountCompleted(empty) }},
		{"output 缺失帧", func() int64 { return respImageCountCompleted(miss) }},
		{"非流式响应体", func() int64 { return respImageCountBody(raw) }},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			if allocs := testing.AllocsPerRun(200, func() { tc.f() }); allocs != 0 {
				t.Errorf("AllocsPerRun = %v，要求 0（热路径零分配红线）", allocs)
			}
		})
	}
}

// --- e2e：resp 非流式（SDK 路径）检测 + 计费 ---

// fakeResponsesImages 假上游（resp 非流式 + 流式）：响应 output 可注入（图片
// item + 其他工具 item 混排——计数/不误计断言载体），usage 3/5/8 固定
// （chat 分量 130 毫分）。
func fakeResponsesImages(t *testing.T, output string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-upstream" {
			w.WriteHeader(401)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		stream, _ := body["stream"].(bool)
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fl := w.(http.Flusher)
			fmt.Fprintf(w, "event: response.completed\n"+`data: {"type":"response.completed","response":{"id":"rsp_img","status":"completed","model":%q,"output":%s,"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`+"\n\n", body["model"], output)
			fl.Flush()
			fmt.Fprint(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "rsp_img", "object": "response", "created_at": 1750000000,
			"status": "completed", "model": body["model"],
			"output": json.RawMessage(output),
			"usage":  map[string]any{"input_tokens": 3, "output_tokens": 5, "total_tokens": 8},
		})
	}))
	return srv
}

// runRespDetectRequest 跑一次非流式 resp 请求（200 + 图片 item 原样透传断言）。
func runRespDetectRequest(t *testing.T, srv *httptest.Server) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4o","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	srv.Config.Handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"type":"image_generation_call"`, "图片 item 原样透传（检测为旁路）")
	return rec
}

// TestProxyResponsesImageDetectNonStream 非流式 resp 检测 + ImageCost 计费落账
// 全矩阵：有价聚合 / 缺图价 no_price / P3-9 per-image nil → 0 / api_key 永不
// 检测 / strip 开不检测。断言以 usage_logs（captureLogStore）为准——检测计数
// 经 buildLog → applyBilling 落既有 Cost 通道。
func TestProxyResponsesImageDetectNonStream(t *testing.T) {
	i64p := func(v int64) *int64 { return &v }
	up := fakeResponsesImages(t, respImagesOutput)
	defer up.Close()
	// 有价行：per-image 5400 毫分/张（aiml 0.054 ×1e5 形态）
	withPerImage := map[string]*domain.ImagePrice{"gpt-4o": respImagePriceRow(nil, nil, i64p(5400))}
	// P3-9：行存在但 per-image nil（仅 token 价有）→ 图分量 0
	tokenOnly := map[string]*domain.ImagePrice{"gpt-4o": respImagePriceRow(i64p(800000), i64p(3000000), nil)}

	tests := []struct {
		name        string
		typ         credential.Type
		strip       bool
		im          map[string]*domain.ImagePrice
		wantCount   int64
		wantCost    int64
		wantPerCall *int64 // 统一计费模型：有按单元价分量时落 price_per_call_millis 快照
		wantTier    string // 空 = 不断言
	}{
		{"有价：ImageCost 聚合（2 张 × 5400 + chat 130）", credential.TypeResponsesSpecial, false, withPerImage, 2, 10930, i64p(5400), ""},
		{"缺图价：no_price 整单不计费", credential.TypeResponsesSpecial, false, nil, 2, 0, nil, "no_price"},
		{"P3-9：per-image nil → 图分量 0（chat 照常）", credential.TypeResponsesSpecial, false, tokenOnly, 2, 130, nil, ""},
		{"api_key 永不检测", credential.TypeAPIKey, false, withPerImage, 0, 130, nil, ""},
		{"strip 开 → 不检测", credential.TypeResponsesSpecial, true, withPerImage, 0, 130, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &captureLogStore{}
			prices := &fakePriceLookup{
				m:  map[string]*domain.Pricing{"gpt-4o": proxyPricing()},
				im: tt.im,
			}
			tpl := &domain.Template{
				ID: 1, Name: "t", BaseURL: up.URL,
				CredentialType:   tt.typ,
				SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
				Models:           []string{"gpt-4o"},
				StripImageTools:  tt.strip,
			}
			p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, &BillingHooks{
				Prices:   prices,
				Balances: billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{}}, nil),
			})
			srv := httptest.NewServer(http.HandlerFunc(p.HandleResponses))
			defer srv.Close()
			runRespDetectRequest(t, srv)

			require.NoError(t, p.rec.Close(context.Background()))
			store.mu.Lock()
			defer store.mu.Unlock()
			require.Len(t, store.logs, 1)
			lg := store.logs[0]
			require.Equal(t, tt.wantCount, lg.CallCount, "检测计数（入 call_count）")
			require.Equal(t, tt.wantCost, lg.Cost, "cost（chat 分量 + image 分量聚合）")
			if tt.wantPerCall != nil {
				require.NotNil(t, lg.PricePerCallMillis, "有按单元价 → price_per_call_millis 快照落列")
				require.Equal(t, *tt.wantPerCall, *lg.PricePerCallMillis)
			} else {
				require.Nil(t, lg.PricePerCallMillis, "无按单元价分量 → nil")
			}
			if tt.wantTier != "" {
				require.Equal(t, tt.wantTier, lg.BillingTier, "no_price 标记")
			} else {
				require.Equal(t, "auto", lg.BillingTier)
			}
		})
	}
}

// TestProxyResponsesImageDetectMultiplier 组倍率整单作用（fast/组倍率与 chat
// 同路径声明）：含 image 分量的 cost × 1.5——(130 + 2×5400) × 1.5 = 16395。
func TestProxyResponsesImageDetectMultiplier(t *testing.T) {
	i64p := func(v int64) *int64 { return &v }
	up := fakeResponsesImages(t, respImagesOutput)
	defer up.Close()
	store := &captureLogStore{}
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeResponsesSpecial,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
		Models:           []string{"gpt-4o"},
	}
	prices := &fakePriceLookup{
		m:  map[string]*domain.Pricing{"gpt-4o": proxyPricing()},
		im: map[string]*domain.ImagePrice{"gpt-4o": respImagePriceRow(nil, nil, i64p(5400))},
	}
	bal := billing.NewBalances(fakeBalanceLoader{
		m: map[int64]int64{}, gm: map[int64]int{10: 15000}, // 组 10 ×1.5（ck-1 归属组）
	}, nil)
	require.NoError(t, bal.Reload(context.Background()), "倍率快照加载（EffectiveMultiplier 读 mult 快照）")
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, &BillingHooks{
		Prices:   prices,
		Balances: bal,
	})
	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponses))
	defer srv.Close()
	runRespDetectRequest(t, srv)

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, int64(2), store.logs[0].CallCount)
	require.Equal(t, int64(16395), store.logs[0].Cost, "(130 + 2×5400) × 1.5 = 16395——倍率整单作用于含 image 分量的 cost")
}

// TestProxyResponsesImageDetectStream 流式 resp 检测：response.completed 帧
// 计数（Observer 旁路）→ 计费落账；api_key 不检测对照。
func TestProxyResponsesImageDetectStream(t *testing.T) {
	i64p := func(v int64) *int64 { return &v }
	up := fakeResponsesImages(t, respImagesOutput)
	defer up.Close()

	for _, tt := range []struct {
		name      string
		typ       credential.Type
		wantCount int64
		wantCost  int64
	}{
		{"responses-special 检测计费", credential.TypeResponsesSpecial, 2, 10930},
		{"api_key 永不检测", credential.TypeAPIKey, 0, 130},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &captureLogStore{}
			tpl := &domain.Template{
				ID: 1, Name: "t", BaseURL: up.URL,
				CredentialType:   tt.typ,
				SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
				Models:           []string{"gpt-4o"},
			}
			prices := &fakePriceLookup{
				m:  map[string]*domain.Pricing{"gpt-4o": proxyPricing()},
				im: map[string]*domain.ImagePrice{"gpt-4o": respImagePriceRow(nil, nil, i64p(5400))},
			}
			p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, &BillingHooks{
				Prices:   prices,
				Balances: billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{}}, nil),
			})
			srv := httptest.NewServer(http.HandlerFunc(p.HandleResponses))
			defer srv.Close()

			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4o","input":"hi","stream":true}`))
			req.Header.Set("Authorization", "Bearer ck-1")
			rec := httptest.NewRecorder()
			srv.Config.Handler.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code)
			require.Contains(t, rec.Body.String(), `"type":"image_generation_call"`, "completed 帧原样透传")

			require.NoError(t, p.rec.Close(context.Background()))
			store.mu.Lock()
			defer store.mu.Unlock()
			require.Len(t, store.logs, 1)
			require.Equal(t, tt.wantCount, store.logs[0].CallCount)
			require.Equal(t, tt.wantCost, store.logs[0].Cost)
		})
	}
}

// --- e2e：resp-ws 检测（relay 帧级旁路） ---

// fakeResponsesWSImages 假上游（resp-ws）：completed 帧 output 可注入（图片
// item + 其他工具 item 混排），usage 3/5/8；首帧后立即正常关闭。
func fakeResponsesWSImages(t *testing.T, output string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-upstream" {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("Responses-Websockets") != aiclient.ResponsesWSBetaHeader {
			w.WriteHeader(400)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
		if err != nil {
			return
		}
		defer c.CloseNow()
		typ, _, err := c.Read(context.Background())
		if err != nil {
			return
		}
		frame := `{"type":"response.completed","response":{"id":"rsp_ws_img","status":"completed","model":"gpt-4o","output":` + output + `,"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`
		if err := c.Write(context.Background(), typ, []byte(frame)); err != nil {
			return
		}
		_ = c.Close(websocket.StatusNormalClosure, "")
	}))
	return srv
}

// TestResponsesWSImageDetect resp-ws 检测 + 计费：completed 帧图片 item 计数
// （relay sniff 旁路）→ ImageCost 落账；api_key 永不检测对照。
func TestResponsesWSImageDetect(t *testing.T) {
	i64p := func(v int64) *int64 { return &v }
	up := fakeResponsesWSImages(t, respImagesOutput)
	defer up.Close()

	for _, tt := range []struct {
		name      string
		typ       credential.Type
		wantCount int64
		wantCost  int64
	}{
		{"responses-special 检测计费", credential.TypeResponsesSpecial, 2, 10930},
		{"api_key 永不检测", credential.TypeAPIKey, 0, 130},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &captureLogStore{}
			tpl := &domain.Template{
				ID: 1, Name: "t", BaseURL: up.URL,
				CredentialType:   tt.typ,
				SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS},
				Models:           []string{"gpt-4o"},
			}
			prices := &fakePriceLookup{
				m:  map[string]*domain.Pricing{"gpt-4o": proxyPricing()},
				im: map[string]*domain.ImagePrice{"gpt-4o": respImagePriceRow(nil, nil, i64p(5400))},
			}
			p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, &BillingHooks{
				Prices:   prices,
				Balances: billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{}}, nil),
			})
			srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
			defer srv.Close()

			c := dialResponsesWS(t, srv)
			require.NoError(t, c.Write(context.Background(), websocket.MessageText,
				[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
			f := readResponsesWSFrame(t, c)
			require.Contains(t, string(f), `"type":"response.completed"`, "completed 帧原样透传（检测为旁路）")
			readResponsesWSClose(t, c, websocket.StatusNormalClosure)

			require.NoError(t, p.rec.Close(context.Background()))
			store.mu.Lock()
			defer store.mu.Unlock()
			require.Len(t, store.logs, 1)
			require.Equal(t, tt.wantCount, store.logs[0].CallCount)
			require.Equal(t, tt.wantCost, store.logs[0].Cost)
		})
	}
}

// codex 类型恒 0 计数分层标注（V1-V3 实证：chatgpt.com 上游图片生成 = 客户端
// 本地执行——响应无图片 item → 检测计数 0；旁路无效但无害）。T4 起 codex 类
// 型走 SDK Dial 路径（T2 的 credentialFor + 静态 provider 方法已随
// codexKeyProvider 移除——快照派生 cred 直供适配层）。
func TestResponsesWSCodexTypeZeroCount(t *testing.T) {
	i64p := func(v int64) *int64 { return &v }
	// SDK 路径 mock 上游（Accept-only）：fakeResponsesWSImages 校验
	// "Bearer sk-upstream"（aiclient 形态）——SDK Dial 注入的是账号 PAT，需
	// 独立上游；completed 帧 output 含 function_call 工具 item（无图片 item——
	// V1-V3 实证 chatgpt.com 上游图片生成 = 客户端本地执行）。
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(404)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
		if err != nil {
			return
		}
		defer c.CloseNow()
		typ, _, err := c.Read(context.Background())
		if err != nil {
			return
		}
		frame := `{"type":"response.completed","response":{"id":"rsp_ws_img","status":"completed","model":"gpt-4o","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"web_search","arguments":"{}","result":"x"}],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`
		if err := c.Write(context.Background(), typ, []byte(frame)); err != nil {
			return
		}
		_ = c.Close(websocket.StatusNormalClosure, "")
	}))
	defer up.Close()
	store := &captureLogStore{}
	pat := "pat-1"
	prices := &fakePriceLookup{
		m:  map[string]*domain.Pricing{"gpt-4o": proxyPricing()},
		im: map[string]*domain.ImagePrice{"gpt-4o": respImagePriceRow(nil, nil, i64p(5400))},
	}
	// SDK 路径（newTestCodexWSProxy）：模板 BaseURL = mock 上游根，账号 ext 快
	// 照承载 PAT（relay 线凭据——不经 credential 注册表）。
	p, _ := newTestCodexWSProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: {AccountID: 10, CredentialType: credential.TypeCodexPAT, CodexIdentity: &domain.CodexIdentity{InstallationID: "i"}, CodexPATKey: &pat}},
		up.URL, &BillingHooks{
			Prices:   prices,
			Balances: billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{}}, nil),
		}, store)
	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer srv.Close()

	c := dialResponsesWS(t, srv)
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	_ = readResponsesWSFrame(t, c)
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	require.NoError(t, p.rec.Close(context.Background()))
	waitStoreLogs(t, store, 1)
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, int64(0), store.logs[0].CallCount, "codex 类型响应无图片 item → 恒 0 计数（客户端本地执行）")
	require.Equal(t, int64(130), store.logs[0].Cost, "无图 → 只计 chat 分量")
}
