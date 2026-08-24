// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/logx"
)

// fakeStreamGen 模拟 T2 适配层 GenerateImageStream（同签名——mock 替身不落
// 生产代码）：按 events 依次回调 fn；fn 返回错误立即终止并透传（SDK 语义）；
// 全部回调完返回 genErr（nil = 成功）。afterEvent 在每个事件回调完成后触发
// （时序断言用）。
func fakeStreamGen(events []domain.ImageStreamEvent, genErr error, afterEvent func(domain.ImageStreamEvent)) imageStreamGenerator {
	return func(ctx context.Context, cred *domain.AccountCredential, p *domain.ImageGenParams, fn func(domain.ImageStreamEvent) error) error {
		for _, ev := range events {
			err := fn(ev)
			if afterEvent != nil {
				afterEvent(ev)
			}
			if err != nil {
				return err
			}
		}
		return genErr
	}
}

// failWriter 写失败包装（模拟客户端断开后的写错误——fn 回调错误路径）。
type failWriter struct {
	http.ResponseWriter
}

func (w *failWriter) Write([]byte) (int, error) { return 0, errors.New("client closed") }

// fakeEnvelope 信封错误替身（T2 信封协议：StatusCode() + RawJSON()）。
type fakeEnvelope struct {
	status int
	body   string
}

func (e *fakeEnvelope) Error() string { return "upstream error status=" + strconv.Itoa(e.status) }

func (e *fakeEnvelope) StatusCode() int { return e.status }
func (e *fakeEnvelope) RawJSON() string { return e.body }

// imageTestPrices 生图测试价格（litellm gpt-image-2 官方形态换算：
// input 8e-06 → 800,000 毫分/1M、output 3e-05 → 3,000,000；per-image
// 0.054 → 5,400 毫分/张——Task A ImageCost 实参断言同款）。
func imageTestPrices() map[string]*domain.PriceEntry {
	i64 := func(v int64) *int64 { return &v }
	return map[string]*domain.PriceEntry{"gpt-image-2": {
		Model: "gpt-image-2", Mode: domain.PriceModeImage,
		ImgInTokPerM:  i64(800000),
		ImgOutTokPerM: i64(3000000),
		PricePerImage: i64(5400),
	}}
}

// newImageStreamTestProxy 构造流式生图测试代理（账号 1 已注册；计费钩子注入
// 生图价格快照；hooks nil = 默认 ×1 倍率）。upstream 供既有 harness 需要
// （流式分支不触达）。
func newImageStreamTestProxy(t *testing.T, hooks *BillingHooks) (*Proxy, *captureLogStore) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(up.Close)
	store := &captureLogStore{}
	if hooks == nil {
		hooks = &BillingHooks{
			Resolver: &fakePriceLookup{entries: imageTestPrices()},
			Balances: billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{}}, nil),
		}
	}
	p := newTestProxyBillingLogs(t, up.URL, nil, nil, store)
	// 覆盖 hooks（newTestProxyBillingLogs 内置 price/bill 与本地构造等价——
	// 直接替换为测试专用 hooks）。
	p.bill = hooks
	return p, store
}

func streamImageSel() *scheduler.Selection {
	return &scheduler.Selection{
		AccountID: 1, TemplateID: 1, Format: domain.FormatOpenAIImages,
		Model: "gpt-image-2", CredentialType: credential.TypeCodexPAT,
	}
}

func streamImageCred() *domain.AccountCredential {
	return &domain.AccountCredential{AccountID: 1, PATKey: "pk-test"}
}

func streamImageParams() *domain.ImageGenParams {
	return &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "a cat"}
}

func streamImageReq(t *testing.T, ctx context.Context) (*http.Request, *httptest.ResponseRecorder) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	if ctx != nil {
		r = r.WithContext(ctx)
	}
	return r, httptest.NewRecorder()
}

// collectImageLogs 排空 recorder 取捕获的用量日志（最后一条 = 本请求）。
func collectImageLogs(t *testing.T, p *Proxy, store *captureLogStore) *domain.UsageLog {
	t.Helper()
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.NotEmpty(t, store.logs, "流式生图请求必须落账")
	return store.logs[len(store.logs)-1]
}

// TestStreamImagePassthrough 事件序列透传：keepalive → ": ping" 注释行；
// completed 每张图一个 SSE 帧（b64_json 各自）；usage 仅末事件携带且 JSON
// tag 直透；首事件即发响应头 + 每事件 Flush；流终计费落账（call_count 数
// completed、usage 取末事件、ImageCost + 价格快照）。
func TestStreamImagePassthrough(t *testing.T) {
	p, store := newImageStreamTestProxy(t, nil)
	r, rec := streamImageReq(t, nil)
	b64a, b64b := "aGVsbG8=", "d29ybGQ="
	events := []domain.ImageStreamEvent{
		{Type: domain.ImageStreamEventKeepalive},
		{Type: domain.ImageStreamEventCompleted, B64JSON: &b64a},
		{Type: domain.ImageStreamEventCompleted, B64JSON: &b64b, Usage: &domain.ImageUsage{
			InputTokens: 10, InputImageTokens: 100, OutputTokens: 5, OutputImageTokens: 50,
		}},
	}
	var (
		firstSeen bool
		headOK    bool // 首事件回调完成时响应头已发 + 已 Flush
	)
	gen := fakeStreamGen(events, nil, func(ev domain.ImageStreamEvent) {
		if !firstSeen {
			firstSeen = true
			// 首事件（keepalive）写入后：响应头已发 + Flush（CF 524 免疫时序）。
			headOK = rec.Code == http.StatusOK && rec.Header().Get("Content-Type") == "text/event-stream" && rec.Flushed
		}
	})
	code, body, handled, err := p.streamImageGeneration(context.Background(), rec, r, "req-1", 10, time.Now(), streamImageSel(), "gpt-image-2", streamImageCred(), streamImageParams(), gen)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, code)
	require.True(t, handled)
	require.Empty(t, body)
	require.True(t, headOK, "首事件即发响应头 + Flush（CF 524 免疫时序）")
	// wire 形态（P2-1）：注释行 + 两帧（usage 仅末帧，JSON tag 直透）。
	require.Equal(t, ": ping\n\n"+
		"event: image_generation.completed\ndata: {\"b64_json\":\"aGVsbG8=\"}\n\n"+
		"event: image_generation.completed\ndata: {\"b64_json\":\"d29ybGQ=\",\"usage\":{\"input_tokens\":10,\"input_image_tokens\":100,\"output_tokens\":5,\"output_image_tokens\":50}}\n\n",
		rec.Body.String())
	require.True(t, rec.Flushed, "每事件后 Flush")

	// 流终计费（与 T2 同口径）：call_count=2、image token 取末事件、价格快照、
	// ImageCost（100×800000/1e6 + 50×3000000/1e6 + 2×5400 = 11030）；text 分量
	// 恒 0；TotalTokens 含 image tokens 不含张数。统一计费模型（spec 2026-08-13）：
	// image token 并入 in/out、张数入 call_count、每张价入 price_per_call_millis。
	l := collectImageLogs(t, p, store)
	require.Equal(t, domain.FormatOpenAIImages, l.Format)
	require.Equal(t, domain.ErrNone, l.ErrorType)
	require.Equal(t, int64(2), l.CallCount)
	require.Equal(t, int64(100), l.InputTokens, "image input tokens 并入 input_tokens")
	require.Equal(t, int64(50), l.OutputTokens, "image output tokens 并入 output_tokens")
	require.Equal(t, int64(150), l.TotalTokens, "image tokens 入 TotalTokens（口径不变）")
	require.NotNil(t, l.PricePerCallMillis)
	require.Equal(t, int64(5400), *l.PricePerCallMillis)
	require.Equal(t, int64(11030), l.Cost, "100×800000/1e6 + 50×3000000/1e6 + 2×5400（ImageCost 口径不变）")
}

// TestBuildCompletedFrameNilB64JSON completed 帧 B64JSON=nil（*string——keepalive
// 恒 nil 的防御边界）→ b64_json 字段字节输出字面 null（与 json.Marshal(nil
// *string) 等价——手写引号改写不得改变字节）；非 nil 路径字节不变（回归锚）。
func TestBuildCompletedFrameNilB64JSON(t *testing.T) {
	out := buildCompletedFrame(&domain.ImageStreamEvent{Type: domain.ImageStreamEventCompleted})
	require.Equal(t, "event: image_generation.completed\ndata: {\"b64_json\":null}\n\n", string(out))
	b64 := "aGVsbG8="
	out = buildCompletedFrame(&domain.ImageStreamEvent{Type: domain.ImageStreamEventCompleted, B64JSON: &b64})
	require.Equal(t, "event: image_generation.completed\ndata: {\"b64_json\":\"aGVsbG8=\"}\n\n", string(out))
}

// TestStreamImageUsageOnlyLastCompleted usage 仅末事件携带：多张图时计费取
// 最后一个 completed 的 usage（中间的 usage 不覆盖）。
func TestStreamImageUsageOnlyLastCompleted(t *testing.T) {
	p, store := newImageStreamTestProxy(t, nil)
	r, rec := streamImageReq(t, nil)
	b64a, b64b := "AAA=", "BBB="
	events := []domain.ImageStreamEvent{
		{Type: domain.ImageStreamEventCompleted, B64JSON: &b64a, Usage: &domain.ImageUsage{InputImageTokens: 7, OutputImageTokens: 7}},
		{Type: domain.ImageStreamEventCompleted, B64JSON: &b64b, Usage: &domain.ImageUsage{InputImageTokens: 3, OutputImageTokens: 2}},
	}
	code, _, _, err := p.streamImageGeneration(context.Background(), rec, r, "req-1", 10, time.Now(), streamImageSel(), "gpt-image-2", streamImageCred(), streamImageParams(), fakeStreamGen(events, nil, nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, code)
	l := collectImageLogs(t, p, store)
	require.Equal(t, int64(2), l.CallCount)
	require.Equal(t, int64(3), l.InputTokens, "usage 取末事件（并入 input_tokens）")
	require.Equal(t, int64(2), l.OutputTokens)
}

// TestStreamImageZeroImagesSuccess 0 图成功边界（P3-3）：SDK Data 空 → 无任何
// 事件 → 网关自行收尾 200 + 记 0 张落账。
func TestStreamImageZeroImagesSuccess(t *testing.T) {
	p, store := newImageStreamTestProxy(t, nil)
	r, rec := streamImageReq(t, nil)
	code, _, handled, err := p.streamImageGeneration(context.Background(), rec, r, "req-1", 10, time.Now(), streamImageSel(), "gpt-image-2", streamImageCred(), streamImageParams(), fakeStreamGen(nil, nil, nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, code)
	require.True(t, handled)
	require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"), "0 图成功也发 SSE 响应头（200 收尾）")
	l := collectImageLogs(t, p, store)
	require.Equal(t, int64(0), l.CallCount)
	require.Zero(t, l.Cost)
	require.Equal(t, domain.ErrNone, l.ErrorType)
	require.Nil(t, l.PricePerCallMillis, "0 张无 per-image 价快照")
}

// TestStreamImagePreHeaderError 首事件前失败：响应头未发 → 错误原样透传
// （信封 StatusCode/RawJSON 可用——HTTP 状态可用路径）。
func TestStreamImagePreHeaderError(t *testing.T) {
	p, _ := newImageStreamTestProxy(t, nil)
	r, rec := streamImageReq(t, nil)
	envErr := &fakeEnvelope{status: http.StatusForbidden, body: `{"error":{"message":"account lacks image scope"}}`}
	code, body, handled, err := p.streamImageGeneration(context.Background(), rec, r, "req-1", 10, time.Now(), streamImageSel(), "gpt-image-2", streamImageCred(), streamImageParams(), fakeStreamGen(nil, envErr, nil))
	require.ErrorIs(t, err, envErr)
	require.Equal(t, http.StatusForbidden, code, "信封 StatusCode 透传")
	require.False(t, handled, "骨架接手写响应")
	require.Equal(t, envErr.body, string(body), "信封 RawJSON 透传")
	require.Empty(t, rec.Header().Get("Content-Type"), "首事件前不发 SSE 响应头")
	require.Zero(t, rec.Body.Len(), "首事件前失败不写任何帧")
}

// TestStreamImagePostHeaderError 响应头已发后失败（P2-2）：HTTP 状态不可用 →
// SSE error 帧（data 含 message——信封文案）+ EOF；计费走 recordStreamAbort
// （已收集张数落账）+ MarkResult(连接级/5xx 分流)。
func TestStreamImagePostHeaderError(t *testing.T) {
	p, store := newImageStreamTestProxy(t, nil)
	r, rec := streamImageReq(t, nil)
	b64a := "aGVsbG8="
	events := []domain.ImageStreamEvent{
		{Type: domain.ImageStreamEventKeepalive},
		{Type: domain.ImageStreamEventCompleted, B64JSON: &b64a},
	}
	envErr := &fakeEnvelope{status: http.StatusInternalServerError, body: `{"error":{"message":"upstream exploded"}}`}
	code, _, handled, err := p.streamImageGeneration(context.Background(), rec, r, "req-1", 10, time.Now(), streamImageSel(), "gpt-image-2", streamImageCred(), streamImageParams(), fakeStreamGen(events, envErr, nil))
	require.NoError(t, err, "响应头已发后失败不返回错误——帧内透传")
	require.Equal(t, 0, code)
	require.True(t, handled)
	require.Contains(t, rec.Body.String(), "event: error\ndata: {\"message\":\"upstream exploded\"}\n\n", "SSE error 帧 + EOF")
	// 计费走 recordStreamAbort：已收集 1 张照常落账（200 + abort 语义）。
	l := collectImageLogs(t, p, store)
	require.Equal(t, domain.ErrAbort, l.ErrorType)
	require.Equal(t, int64(1), l.CallCount)
	require.Equal(t, int64(5400), l.Cost, "1 张 per-image 计费")
	p.sched.FlushRules() // MarkResult 异步投递：断言前排空
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusUnhealthy, ri.Status, "上游错误 MarkResult(连接级/5xx 分流)")
}

// TestStreamImageAbortNoCompleted 响应头已发后失败且无 completed：已收集 0 张
// 落账（recordStreamAbort 语义边界）。
func TestStreamImageAbortNoCompleted(t *testing.T) {
	p, store := newImageStreamTestProxy(t, nil)
	r, rec := streamImageReq(t, nil)
	genErr := errors.New("connection reset")
	code, _, _, err := p.streamImageGeneration(context.Background(), rec, r, "req-1", 10, time.Now(), streamImageSel(), "gpt-image-2", streamImageCred(), streamImageParams(), fakeStreamGen([]domain.ImageStreamEvent{{Type: domain.ImageStreamEventKeepalive}}, genErr, nil))
	require.NoError(t, err)
	require.Equal(t, 0, code)
	require.Contains(t, rec.Body.String(), "event: error\ndata: {\"message\":\"upstream connection error\"}\n\n", "SSE error 帧固定文案（连接级内部文本不上用户帧）")
	l := collectImageLogs(t, p, store)
	require.Equal(t, domain.ErrAbort, l.ErrorType)
	require.Zero(t, l.CallCount, "无 completed → 0 张落账")
}

// TestStreamImageClientDisconnect 客户端断开（abort 双分支 1）：已收集张数照常
// 计费、不 MarkResult（镜像 caller_responses.go:92-101——无法转移）。
func TestStreamImageClientDisconnect(t *testing.T) {
	p, store := newImageStreamTestProxy(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	r, rec := streamImageReq(t, ctx)
	b64a := "aGVsbG8="
	events := []domain.ImageStreamEvent{{Type: domain.ImageStreamEventCompleted, B64JSON: &b64a}}
	gen := func(ctx context.Context, cred *domain.AccountCredential, prm *domain.ImageGenParams, fn func(domain.ImageStreamEvent) error) error {
		// 首个事件回调后模拟客户端断开（网关中止）：fn 错误返回后取消请求 ctx。
		if err := fn(events[0]); err != nil {
			return err
		}
		cancel()
		return errors.New("client closed")
	}
	code, _, handled, err := p.streamImageGeneration(context.Background(), rec, r, "req-1", 10, time.Now(), streamImageSel(), "gpt-image-2", streamImageCred(), streamImageParams(), gen)
	require.NoError(t, err)
	require.Equal(t, 0, code)
	require.True(t, handled)
	l := collectImageLogs(t, p, store)
	require.Equal(t, domain.ErrAbort, l.ErrorType)
	require.Equal(t, int64(1), l.CallCount, "客户端断开已收集张数照常落账")
	require.Equal(t, int64(5400), l.Cost)
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.ErrCount, "客户端断开不 MarkResult")
}

// TestStreamImageFnErrorTerminates fn 回调错误 → 立即终止（后续事件不再
// 回调——SDK 语义：fn 错误取消在途请求并优先返回）。
func TestStreamImageFnErrorTerminates(t *testing.T) {
	p, _ := newImageStreamTestProxy(t, nil)
	r, rec := streamImageReq(t, nil)
	b64a, b64b := "AAA=", "BBB="
	events := []domain.ImageStreamEvent{
		{Type: domain.ImageStreamEventCompleted, B64JSON: &b64a},
		{Type: domain.ImageStreamEventCompleted, B64JSON: &b64b},
	}
	var called int
	gen := fakeStreamGen(events, nil, func(ev domain.ImageStreamEvent) { called++ })
	// 首个事件写失败（客户端断开）→ fn 返回错误 → gen 立即终止（后续事件不再
	// 回调）。
	fw := &failWriter{ResponseWriter: rec}
	code, _, handled, err := p.streamImageGeneration(context.Background(), fw, r, "req-1", 10, time.Now(), streamImageSel(), "gpt-image-2", streamImageCred(), streamImageParams(), gen)
	require.NoError(t, err)
	require.Equal(t, 0, code)
	require.True(t, handled)
	require.Equal(t, 1, called, "fn 错误后立即终止——后续事件不再回调")
}

// TestStreamImageUnknownEventSkipped 未知事件类型跳过（SDK 合成流不产出，
// 防御——不写帧不计费不计数）。
func TestStreamImageUnknownEventSkipped(t *testing.T) {
	p, store := newImageStreamTestProxy(t, nil)
	r, rec := streamImageReq(t, nil)
	b64a := "aGVsbG8="
	events := []domain.ImageStreamEvent{
		{Type: domain.ImageStreamEventType("partial_image"), B64JSON: &b64a},
		{Type: domain.ImageStreamEventCompleted, B64JSON: &b64a},
	}
	code, _, _, err := p.streamImageGeneration(context.Background(), rec, r, "req-1", 10, time.Now(), streamImageSel(), "gpt-image-2", streamImageCred(), streamImageParams(), fakeStreamGen(events, nil, nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, code)
	require.NotContains(t, rec.Body.String(), "partial_image", "未知事件不写帧")
	require.Equal(t, 1, strings.Count(rec.Body.String(), "event: image_generation.completed"))
	l := collectImageLogs(t, p, store)
	require.Equal(t, int64(1), l.CallCount, "partial_image 不计费不计数")
}

// TestStreamImageUnknownEventWarns 未知事件类型 → Warn（A-P2-10 静默面收敛）：
// 不写帧不计费，且 p.log 装配时日志留痕（修复前零日志零告警——SDK 升级改事
// 件名则落账 0 张无从发现；适配层已显式映射过滤，此处分层防御）。
func TestStreamImageUnknownEventWarns(t *testing.T) {
	p, store := newImageStreamTestProxy(t, nil)
	dir, err := os.MkdirTemp("", "c3api-imgwarn-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	logOut := filepath.Join(dir, "warn.log")
	logger, err := logx.New("warn", logOut)
	require.NoError(t, err)
	p.log = logger // 测试代理默认 nil 日志——按需注入（与 p.bill 覆盖同模式）
	r, rec := streamImageReq(t, nil)
	b64a := "aGVsbG8="
	events := []domain.ImageStreamEvent{
		{Type: domain.ImageStreamEventType("partial_image"), B64JSON: &b64a},
		{Type: domain.ImageStreamEventCompleted, B64JSON: &b64a},
	}
	code, _, _, err := p.streamImageGeneration(context.Background(), rec, r, "req-1", 10, time.Now(), streamImageSel(), "gpt-image-2", streamImageCred(), streamImageParams(), fakeStreamGen(events, nil, nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, code)
	require.NotContains(t, rec.Body.String(), "partial_image", "未知事件不写帧")
	l := collectImageLogs(t, p, store)
	require.Equal(t, int64(1), l.CallCount, "未知事件不计费不计数")
	data, err := os.ReadFile(logOut)
	require.NoError(t, err)
	require.Contains(t, string(data), "image stream: unknown event type skipped", "未知事件必须 Warn（不静默吞）")
	require.Contains(t, string(data), "partial_image", "Warn 带事件名")
	require.Contains(t, string(data), "req-1", "Warn 带 request_id")
}

// TestStreamImageBillingNoPrice 价格快照缺失（预检后竞态）：BillingTier=
// no_price + cost 0（运行时防御，与 chat 同语义）；张数仍记（审计留痕）。
func TestStreamImageBillingNoPrice(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(up.Close)
	store := &captureLogStore{}
	hooks := &BillingHooks{
		Resolver: &fakePriceLookup{entries: map[string]*domain.PriceEntry{}},
		Balances: billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{}}, nil),
	}
	p := newTestProxyBillingLogs(t, up.URL, nil, nil, store)
	p.bill = hooks
	r, rec := streamImageReq(t, nil)
	b64a := "aGVsbG8="
	events := []domain.ImageStreamEvent{{Type: domain.ImageStreamEventCompleted, B64JSON: &b64a}}
	code, _, _, err := p.streamImageGeneration(context.Background(), rec, r, "req-1", 10, time.Now(), streamImageSel(), "gpt-image-2", streamImageCred(), streamImageParams(), fakeStreamGen(events, nil, nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, code)
	l := collectImageLogs(t, p, store)
	require.Equal(t, "no_price", l.BillingTier)
	require.Zero(t, l.Cost)
	require.Equal(t, int64(1), l.CallCount, "无价仍记张数（审计留痕）")
}

// TestStreamImageGroupMultiplier 组倍率作用于含 image 分量 cost（整单 ×
// 有效倍率——与 chat 同路径；×1.5 → 5400×1.5 = 8100）。
func TestStreamImageGroupMultiplier(t *testing.T) {
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
}
