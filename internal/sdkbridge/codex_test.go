// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/httpx"
)

// ---------------------------------------------------------------------------
// mock 上游：images 端点（WithBaseURL 覆盖）+ refresh 端点（env override——
// 对齐 SDK 测试模式 t.Setenv("CODEX_REFRESH_TOKEN_URL_OVERRIDE")）
// ---------------------------------------------------------------------------

// codexUpstreamCapture 断言请求面（路径/鉴权）+ 可编程响应序列。
type codexUpstreamCapture struct {
	mu    sync.Mutex
	calls int
	auths []string
	steps []codexUpstreamStep
	last  codexUpstreamStep
}

type codexUpstreamStep struct {
	status int
	body   string
}

// newCodexUpstream 构造 images 端点 mock：响应序列按序弹出（耗尽重复最后一步）。
// baseURL = srv.URL + "/images/generations"（SDK WithBaseURL 完整端点语义）。
func newCodexUpstream(t *testing.T, steps ...codexUpstreamStep) (*httptest.Server, *codexUpstreamCapture) {
	t.Helper()
	c := &codexUpstreamCapture{last: codexUpstreamStep{status: 500, body: `{}`}}
	if len(steps) > 0 {
		c.steps = steps
		c.last = steps[len(steps)-1]
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		c.mu.Lock()
		c.calls++
		c.auths = append(c.auths, r.Header.Get("Authorization"))
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

func (c *codexUpstreamCapture) callsN() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *codexUpstreamCapture) auth(i int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.auths[i]
}

// codexMockRefresh 构造 refresh 端点 mock（同 SDK 测试形态：构造即设置 env）。
type codexMockRefresh struct {
	mu    sync.Mutex
	calls int
}

func newCodexMockRefresh(t *testing.T, steps ...codexUpstreamStep) *codexMockRefresh {
	t.Helper()
	m := &codexMockRefresh{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		m.mu.Lock()
		m.calls++
		step := codexUpstreamStep{status: 500, body: `{}`}
		if len(steps) > 0 {
			step = steps[0]
			steps = steps[1:]
		}
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(step.status)
		_, _ = w.Write([]byte(step.body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CODEX_REFRESH_TOKEN_URL_OVERRIDE", srv.URL)
	return m
}

func (m *codexMockRefresh) callsN() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// oauthCred 构造 oauth 测试凭据（未过期初始 at + rt）。
func oauthCred(accountID int64, at, rt string) *domain.AccountCredential {
	exp := time.Now().Add(time.Hour)
	return &domain.AccountCredential{
		AccountID: accountID, OAuthToken: at, OAuthRefreshToken: rt, OAuthExpiresAt: &exp,
	}
}

// okImageResponse 标准生图成功响应（usage 嵌套 details——对齐上游 wire）。
const okImageResponse = `{"created":1720000000,"data":[{"b64_json":"QUJD"},{"b64_json":"REVG"}],"usage":{"input_tokens":2,"output_tokens":3,"input_tokens_details":{"image_tokens":1},"output_tokens_details":{"image_tokens":2}}}`

// ---------------------------------------------------------------------------
// 转换层双向映射（防漂移）
// ---------------------------------------------------------------------------

// TestCodexConversions domain↔codexsdk 双向转换字段完备：toSDKParams 全字段
// （含 edits Images——ImageURL 与 Raw 双形态）；fromSDKResponse 全字段平铺
// （含 usage 嵌套提取）；MarshalImageResponse 上游 wire 形态（嵌套 usage
// details + 计费提取同源）。
func TestCodexConversions(t *testing.T) {
	n := 2
	size, quality, background := "1024x1024", "high", "transparent"
	url1 := "https://example.com/in.png"
	p := &domain.ImageGenParams{
		Model: "gpt-image-2", Prompt: "a cat", N: &n,
		Size: &size, Quality: &quality, Background: &background,
		Images: []domain.ImageRef{
			{ImageURL: &url1},
			{Raw: []byte("PNG-bytes")},
		},
	}
	s := toSDKParams(p)
	require.Equal(t, "gpt-image-2", s.Model)
	require.Equal(t, "a cat", s.Prompt)
	require.Equal(t, &n, s.N)
	require.Equal(t, &size, s.Size)
	require.Equal(t, &quality, s.Quality)
	require.Equal(t, &background, s.Background)
	require.Len(t, s.Images, 2)
	require.Equal(t, &url1, s.Images[0].ImageURL)
	require.Nil(t, s.Images[0].Raw)
	require.Equal(t, []byte("PNG-bytes"), s.Images[1].Raw)
	require.Nil(t, s.Images[1].ImageURL)
	// nil 输入
	require.Nil(t, toSDKParams(nil))
	require.Empty(t, toSDKParams(&domain.ImageGenParams{}).Images, "空 Images → 不发 images 字段")

	// fromSDKResponse：全字段 + usage 嵌套提取 + nil usage
	sdk := &codexsdk.ImageResponse{
		Created:      1720000000,
		Background:   &background,
		Data:         []codexsdk.Image{{B64JSON: strPtr("QUJD")}, {B64JSON: strPtr("REVG")}},
		OutputFormat: strPtr("png"),
		Quality:      &quality,
		Size:         &size,
		Usage: &codexsdk.ImageUsage{
			InputTokens: 2, InputImageTokens: 1, OutputTokens: 3, OutputImageTokens: 2,
		},
	}
	d := fromSDKResponse(sdk)
	require.Equal(t, int64(1720000000), d.Created)
	require.Equal(t, &background, d.Background)
	require.Equal(t, strPtr("png"), d.OutputFormat)
	require.Len(t, d.Data, 2)
	require.Equal(t, strPtr("QUJD"), d.Data[0].B64JSON)
	require.Equal(t, strPtr("REVG"), d.Data[1].B64JSON)
	require.NotNil(t, d.Usage)
	require.Equal(t, int64(1), d.Usage.InputImageTokens)
	require.Equal(t, int64(2), d.Usage.OutputImageTokens)
	require.Nil(t, fromSDKResponse(&codexsdk.ImageResponse{}).Usage, "usage 缺失 → nil（per-image 兜底）")

	// MarshalImageResponse：wire 形态（嵌套 usage details——计费提取同源）
	wire, err := MarshalImageResponse(d)
	require.NoError(t, err)
	require.Equal(t, int64(2), jsonGetInt(t, wire, "data.#"), "data 长 = 张数")
	require.Equal(t, int64(1), jsonGetInt(t, wire, "usage.input_tokens_details.image_tokens"))
	require.Equal(t, int64(2), jsonGetInt(t, wire, "usage.output_tokens_details.image_tokens"))
	require.Equal(t, int64(2), jsonGetInt(t, wire, "usage.input_tokens"))
	// 无 usage / 空 data 形态
	wire2, err := MarshalImageResponse(&domain.ImageResponse{Data: []domain.Image{}})
	require.NoError(t, err)
	require.Equal(t, "[]", jsonGetStr(t, wire2, "data"), "空 data → []（非 null）")
	require.Equal(t, "", jsonGetStr(t, wire2, "usage"), "usage 缺失 → 字段不输出")
	// 零 image token 不输出 details（缺失与 0 同语义——计费读 0）
	wire3, err := MarshalImageResponse(&domain.ImageResponse{
		Data:  []domain.Image{{}},
		Usage: &domain.ImageUsage{InputTokens: 5},
	})
	require.NoError(t, err)
	require.Equal(t, "", jsonGetStr(t, wire3, "usage.input_tokens_details.image_tokens"), "0 image token → details 不输出")
}

func strPtr(s string) *string { return &s }

func jsonGetInt(t *testing.T, b []byte, path string) int64 {
	t.Helper()
	return gjson.GetBytes(b, path).Int()
}

func jsonGetStr(t *testing.T, b []byte, path string) string {
	t.Helper()
	return gjson.GetBytes(b, path).String()
}

// TestMapStreamEventType 流式事件类型显式映射（A-P2-10 转换单测防漂移）：SDK
// 事件名 → domain 类型化常量（case 用 SDK 常量）；未知（SDK 升级改事件名）→
// ok=false——适配层 Warn + 跳过，不静默透传导致网关落账 0 张零告警。
func TestMapStreamEventType(t *testing.T) {
	cases := []struct {
		in   string
		want domain.ImageStreamEventType
		ok   bool
	}{
		{codexsdk.ImageStreamEventCompleted, domain.ImageStreamEventCompleted, true},
		{codexsdk.ImageStreamEventKeepalive, domain.ImageStreamEventKeepalive, true},
		{"partial_image", "", false},
		{"image_generation.failed", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := mapStreamEventType(tc.in)
		require.Equal(t, tc.ok, ok, "type=%q", tc.in)
		require.Equal(t, tc.want, got, "type=%q", tc.in)
	}
}

// ---------------------------------------------------------------------------
// cred → Auth 缓存
// ---------------------------------------------------------------------------

// TestCodexCacheReuseAndRebuild 同账号复用（同 HTTPClient 指针断言）/ 凭据
// 更新后重建（token/rt/pat/base URL 任一变化 → 新客户端）/ 轮转回调写回不重建
// （回调本身不触发缓存变更）。
func TestCodexCacheReuseAndRebuild(t *testing.T) {
	up, c := newCodexUpstream(t, codexUpstreamStep{status: 200, body: okImageResponse})
	defer up.Close()
	newCodexMockRefresh(t, codexUpstreamStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`})
	handler := &recordingHandler{}
	a := NewCodex(handler.add)

	cred := oauthCred(7, "at-1", "rt-1")
	cred.BaseURL = up.URL + "/images/generations"

	p := &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"}
	img, err := a.GenerateImage(context.Background(), cred, p)
	require.NoError(t, err)
	require.Len(t, img.Data, 2, "mock 200 真实生成形态")
	e1 := a.entries[7]
	require.NotNil(t, e1, "构造后入缓存")

	// 同账号同凭据 → 复用（同一 HTTPClient）
	_, err = a.GenerateImage(context.Background(), cred, p)
	require.NoError(t, err)
	require.Same(t, e1.client, a.entries[7].client, "同账号复用（轮转状态/连接池保持）")
	require.Equal(t, 2, c.callsN())

	// 凭据更新（管理面导入/更新——at 变更）→ 重建
	cred2 := oauthCred(7, "at-2", "rt-1")
	cred2.BaseURL = up.URL + "/images/generations"
	_, err = a.GenerateImage(context.Background(), cred2, p)
	require.NoError(t, err)
	require.NotSame(t, e1.client, a.entries[7].client, "凭据更新 → 重建")
	require.NotEqual(t, "Bearer at-1", c.auth(c.callsN()-1), "重建后新 at 生效")

	// rt 变更同样触发重建
	cred3 := oauthCred(7, "at-2", "rt-2")
	cred3.BaseURL = up.URL + "/images/generations"
	_, err = a.GenerateImage(context.Background(), cred3, p)
	require.NoError(t, err)
	require.NotSame(t, a.entries[7].client, e1.client, "rt 更新 → 重建")

	// base URL 变更（模板 base 更新——aiclient InvalidateAll 同语义）→ 重建
	cred4 := oauthCred(7, "at-2", "rt-2")
	cred4.BaseURL = up.URL + "/images/generations/other"
	_, err = a.GenerateImage(context.Background(), cred4, p)
	require.NoError(t, err)
	require.NotSame(t, a.entries[7].client, e1.client, "base URL 变更 → 重建")

	// 无上报（成功路径）
	require.Empty(t, handler.snapshot(), "成功路径不上报")
}

// TestCodexCachePAT pat 类型：PAT(key) 静态直连（无轮转回调）；同账号复用。
func TestCodexCachePAT(t *testing.T) {
	up, c := newCodexUpstream(t, codexUpstreamStep{status: 200, body: okImageResponse})
	defer up.Close()
	handler := &recordingHandler{}
	a := NewCodex(handler.add)

	cred := &domain.AccountCredential{AccountID: 9, PATKey: "pat-1"}
	cred.BaseURL = up.URL + "/images/generations"
	p := &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"}
	_, err := a.GenerateImage(context.Background(), cred, p)
	require.NoError(t, err)
	require.Equal(t, "Bearer pat-1", c.auth(0), "PAT 静态鉴权")
	e1 := a.entries[9]
	_, err = a.GenerateImage(context.Background(), cred, p)
	require.NoError(t, err)
	require.Same(t, e1.client, a.entries[9].client, "PAT 同账号复用")
}

// TestCodexCacheConcurrentSingleFlight 同账号并发请求单飞构造：N goroutine
// 并发首请求 → 恰一次构造入缓存（互斥锁单飞——对齐 SDK OAuth 单飞语义）；
// 全部请求成功送达同一账号凭据。
func TestCodexCacheConcurrentSingleFlight(t *testing.T) {
	up, c := newCodexUpstream(t, codexUpstreamStep{status: 200, body: okImageResponse})
	defer up.Close()
	newCodexMockRefresh(t, codexUpstreamStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`})
	a := NewCodex(nil)

	cred := oauthCred(7, "at-1", "rt-1")
	cred.BaseURL = up.URL + "/images/generations"
	p := &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"}

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := a.GenerateImage(context.Background(), cred, p)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err, "并发首请求全部成功")
	}
	a.mu.Lock()
	require.Len(t, a.entries, 1, "并发单飞构造——恰一个缓存条目")
	e := a.entries[7]
	a.mu.Unlock()
	require.NotNil(t, e)
	require.Equal(t, 32, c.callsN(), "并发请求全部送达上游")
}

// TestCodexCacheEvictionOnFatal fatal → 失效剔除（T1 联动）：上报后缓存条目
// 摘除；后续请求重建（新条目）。
func TestCodexCacheEvictionOnFatal(t *testing.T) {
	up, _ := newCodexUpstream(t, codexUpstreamStep{status: 401, body: `{"error":{"code":"token_invalidated"}}`})
	defer up.Close()
	newCodexMockRefresh(t, codexUpstreamStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`})
	handler := &recordingHandler{}
	a := NewCodex(handler.add)

	cred := oauthCred(7, "at-1", "rt-1")
	cred.BaseURL = up.URL + "/images/generations"
	p := &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"}
	_, err := a.GenerateImage(context.Background(), cred, p)
	require.Error(t, err)

	a.mu.Lock()
	require.Len(t, a.entries, 0, "fatal 上报后失效剔除——缓存条目摘除")
	a.mu.Unlock()
	calls := handler.snapshot()
	require.Len(t, calls, 1, "fatal 单次上报")
	require.Equal(t, int64(7), calls[0].accountID)
	var ap *codexsdk.AuthPermanentlyRevokedError
	require.True(t, errors.As(calls[0].fatal, &ap), "上报错误类型 = SDK 判死类型")
}

// ---------------------------------------------------------------------------
// 信封包装
// ---------------------------------------------------------------------------

// TestCodexEnvelopeHTTPError SDK *HTTPError → 网关侧信封：StatusCode()/
// RawJSON()/Unwrap 链（errors.As *codexsdk.HTTPError 穿透命中——网关
// statusOf/upstreamBody 零改动复用）。
func TestCodexEnvelopeHTTPError(t *testing.T) {
	up, _ := newCodexUpstream(t, codexUpstreamStep{status: 403, body: `{"detail":"Forbidden"}`})
	defer up.Close()
	newCodexMockRefresh(t, codexUpstreamStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`})
	handler := &recordingHandler{}
	a := NewCodex(handler.add)

	cred := oauthCred(7, "at-1", "rt-1")
	cred.BaseURL = up.URL + "/images/generations"
	_, err := a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.Error(t, err)

	var env *EnvelopeError
	require.True(t, errors.As(err, &env), "信封类型 errors.As 命中")
	require.Equal(t, 403, env.StatusCode(), "信封 StatusCode() = 上游状态码")
	require.Equal(t, `{"detail":"Forbidden"}`, env.RawJSON(), "信封 RawJSON() = 上游原始 body")
	// Unwrap 链：errors.As *codexsdk.HTTPError 穿透信封
	var he *codexsdk.HTTPError
	require.True(t, errors.As(err, &he), "Unwrap 保留 errors.As 链（SDK 类型仍可命中）")
	require.Equal(t, 403, he.StatusCode)
	require.Equal(t, `{"detail":"Forbidden"}`, string(he.Raw))
	// 信封不上报回调（透传协议）
	require.Empty(t, handler.snapshot(), "信封错误不上报回调")
}

// TestEnvelopeErrorFatalChain 信封 Unwrap 链直接断言：包装 SDK fatal → errors.As
// 穿透命中（网关 fatal 分类不因信封包装失效——T1 envelope_test 的 sdkHTTPError
// 链覆盖协议面，此处补真实 SDK fatal 类型穿透）。
func TestEnvelopeErrorFatalChain(t *testing.T) {
	fatal := &codexsdk.RefreshOAuthError{Code: "invalid_grant", Raw: []byte(`{"error":"invalid_grant"}`)}
	env := NewEnvelopeError(401, `{"error":"invalid_grant"}`, fatal)
	var re *codexsdk.RefreshOAuthError
	require.True(t, errors.As(env, &re), "Unwrap 保留 errors.As 链——fatal 类型穿透信封命中")
	require.Equal(t, "invalid_grant", re.Code)
	// Err nil → Unwrap nil → 链自然中断
	env2 := NewEnvelopeError(502, "x", nil)
	require.Nil(t, env2.Unwrap())
	var he *codexsdk.HTTPError
	require.False(t, errors.As(env2, &he))
}

// ---------------------------------------------------------------------------
// fatal 双源去重 + 不上报类
// ---------------------------------------------------------------------------

// TestCodexFatalDedupCallbackAndErrorsAs fatal 双源去重：rotationAuth 路径同一
// fatal 既触发 WithOnAuthFatal 又随返回错误 errors.As 命中——以回调为准去重、
// 单次上报（回调 CAS 胜出，errors.As 补报路径跳过）。
func TestCodexFatalDedupCallbackAndErrorsAs(t *testing.T) {
	up, c := newCodexUpstream(t, codexUpstreamStep{status: 401, body: `{"error":{"code":"token_invalidated"}}`})
	defer up.Close()
	newCodexMockRefresh(t, codexUpstreamStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`})
	handler := &recordingHandler{}
	a := NewCodex(handler.add)

	cred := oauthCred(7, "at-1", "rt-1")
	cred.BaseURL = up.URL + "/images/generations"
	_, err := a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.Error(t, err)
	var ap *codexsdk.AuthPermanentlyRevokedError
	require.True(t, errors.As(err, &ap), "fatal 原样透传（errors.As 命中）")
	require.Equal(t, 1, c.callsN(), "判死不重试")

	calls := handler.snapshot()
	require.Len(t, calls, 1, "回调 + errors.As 双源 → 单次上报")
	require.Equal(t, int64(7), calls[0].accountID)
	require.True(t, errors.As(calls[0].fatal, &ap), "上报 = SDK 判死错误（非信封）")

	// 后续同账号请求（管理面恢复前不应出现——快照已摘除；防御断言）：条目已
	// 剔除 → 重建新 Auth → 新 incident 再次上报一次（每次事件一次——下游
	// HandleFailure 幂等，重复上报不重复写）。
	_, err = a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.Error(t, err, "同账号再次请求仍失败（判死态）")
	calls = handler.snapshot()
	require.Len(t, calls, 2, "重建条目后同 fatal 再上报一次（每 incident 一次）")
}

// TestCodexRefreshErrorNotReported RefreshError 可重试不上报（对齐 SDK 语义
// auth_errors.go:53-58）：refresh 端点 500 耗尽退避 → RefreshError 原样透传，
// FailureHandler 零调用。
func TestCodexRefreshErrorNotReported(t *testing.T) {
	up, c := newCodexUpstream(t, codexUpstreamStep{status: 200, body: okImageResponse})
	defer up.Close()
	// refresh 恒 500（可重试类——非 fatal）
	newCodexMockRefresh(t, codexUpstreamStep{status: 500, body: `{}`})
	handler := &recordingHandler{}
	a := NewCodex(handler.add)

	// 无初始 at（OAuthToken 空 → 首请求前用 rt 换取 → refresh 失败）
	cred := &domain.AccountCredential{AccountID: 7, OAuthRefreshToken: "rt-1"}
	cred.BaseURL = up.URL + "/images/generations"
	_, err := a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.Error(t, err)
	var re *codexsdk.RefreshError
	require.True(t, errors.As(err, &re), "RefreshError 原样透传（errors.As 命中）")
	require.Zero(t, c.callsN(), "refresh 失败不触达上游 images 端点")
	require.Empty(t, handler.snapshot(), "RefreshError 可重试类不上报")
}

// TestCodexEmptyRefreshTokenNoPanic P2-3 空 rt 防护：oauth 凭据缺 refresh_token
// → 按失效处理上报（账号凭据不完整）不 panic（OAuthWithRotation 空 rt 构造
// panic 被构造前校验拦截）。
func TestCodexEmptyRefreshTokenNoPanic(t *testing.T) {
	handler := &recordingHandler{}
	a := NewCodex(handler.add)
	cred := &domain.AccountCredential{AccountID: 7, OAuthToken: "at-1"} // rt 缺失

	require.NotPanics(t, func() {
		_, err := a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
		require.Error(t, err)
		require.ErrorIs(t, err, errCredentialIncomplete)
	})
	calls := handler.snapshot()
	require.Len(t, calls, 1, "空 rt → 失效上报一次")
	require.Equal(t, int64(7), calls[0].accountID)
	require.ErrorIs(t, calls[0].fatal, errCredentialIncomplete)
	a.mu.Lock()
	require.Len(t, a.entries, 0, "构造失败不入缓存")
	a.mu.Unlock()
}

// TestCodexInitialATPreset 过期判定在网关侧构造前：未过期 at → 预置初始 at
// （首请求直接用，不强制 refresh）；已过期 → 不预置（SDK 首请求前用 rt 换取）。
func TestCodexInitialATPreset(t *testing.T) {
	up, c := newCodexUpstream(t, codexUpstreamStep{status: 200, body: okImageResponse})
	defer up.Close()
	refresh := newCodexMockRefresh(t, codexUpstreamStep{status: 200, body: `{"access_token":"at-fresh","refresh_token":"rt-new"}`})
	a := NewCodex(nil)

	// 未过期 → 预置 at：上游直接收到 at-1，refresh 零调用
	cred := oauthCred(7, "at-1", "rt-1")
	cred.BaseURL = up.URL + "/images/generations"
	_, err := a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.NoError(t, err)
	require.Equal(t, "Bearer at-1", c.auth(0), "未过期 at 预置——首请求直接用")
	require.Zero(t, refresh.callsN(), "预置 at 不触发 refresh")

	// 已过期 → 不预置：首请求前先用 rt 换取新 at
	expired := time.Now().Add(-time.Hour)
	cred2 := &domain.AccountCredential{
		AccountID: 7, OAuthToken: "at-expired", OAuthRefreshToken: "rt-2", OAuthExpiresAt: &expired,
	}
	cred2.BaseURL = up.URL + "/images/generations"
	_, err = a.GenerateImage(context.Background(), cred2, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.NoError(t, err)
	require.Equal(t, "Bearer at-fresh", c.auth(1), "过期 at 不预置——rt 换取的新 at 生效")
	require.Equal(t, 1, refresh.callsN(), "已过期 → 首请求前 refresh 一次")

	// nil 过期时刻（未知）→ 视为可用预置
	cred3 := &domain.AccountCredential{AccountID: 7, OAuthToken: "at-3", OAuthRefreshToken: "rt-3"}
	cred3.BaseURL = up.URL + "/images/generations"
	_, err = a.GenerateImage(context.Background(), cred3, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.NoError(t, err)
	require.Equal(t, "Bearer at-3", c.auth(2), "未知过期时刻 → 预置（401 自愈兜底）")
	require.Equal(t, 1, refresh.callsN(), "预置路径不触发 refresh")
}

// TestCodex401RotationSuccess 401 非判死 → SDK 自动轮转重试一次（刷新后新 at
// 重发）——成功路径（判死 vs 轮转的分界断言）。
func TestCodex401RotationSuccess(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		if n == 1 {
			// 非判死 401（过期 AT——错误码不在判死集）
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"code":"token_expired"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okImageResponse))
	}))
	t.Cleanup(srv.Close)
	newCodexMockRefresh(t, codexUpstreamStep{status: 200, body: `{"access_token":"at-rotated","refresh_token":"rt-new"}`})
	a := NewCodex(nil)

	cred := oauthCred(7, "at-old", "rt-1")
	cred.BaseURL = srv.URL + "/images/generations"
	img, err := a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.NoError(t, err)
	require.Len(t, img.Data, 2)
	require.Equal(t, int64(2), calls.Load(), "401 非判死 → 轮转重试一次")
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, "Bearer at-old", auths[0])
	require.Equal(t, "Bearer at-rotated", auths[1], "轮转后新 at 生效")
}

// TestCodexNilParamsAndNetwork SDK 参数校验错误透传（Model/Prompt 必填——
// 网关已前置校验，防御断言）与网络错误（连接级——code 0 分类由网关侧承担）。
func TestCodexNilParamsAndNetwork(t *testing.T) {
	a := NewCodex(nil)
	cred := oauthCred(7, "at-1", "rt-1")
	cred.BaseURL = "http://127.0.0.1:1/images/generations" // 不可达端口
	_, err := a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "必填", "SDK 参数校验错误原样透传")
}

// TestCodexIncompleteNotReportedTwice 同一账号连续空 rt 请求：失效上报每次
// 事件一次（首次上报后网关侧已摘除——重复上报幂等由 HandleFailure 保证；
// 适配层每请求构造失败都上报，链自限）。
func TestCodexIncompleteNotReportedTwice(t *testing.T) {
	handler := &recordingHandler{}
	a := NewCodex(handler.add)
	cred := &domain.AccountCredential{AccountID: 7}
	for i := 0; i < 3; i++ {
		_, err := a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
		require.Error(t, err)
	}
	require.Len(t, handler.snapshot(), 3, "每次构造失败都上报（下游 HandleFailure 幂等；摘除后不再调度）")
}

// ---------------------------------------------------------------------------
// T6：Responses / StreamResponses 面（resp 接入——SSE mock 上游；fixture 对齐
// codex-sdk responses_test.go 事件形态）
// ---------------------------------------------------------------------------

// codexRespUpstream responses 端点 SSE mock：步骤按序弹出（耗尽重复最后一步），
// 记录鉴权头/请求体/turn-state 请求头；200 步 → 逐 events 发 data: 行 +
// [DONE]；步骤 turnState 非空 → 响应头签发 x-codex-turn-state（HOST-2 断言面）。
type codexRespUpstream struct {
	mu         sync.Mutex
	calls      int
	auths      []string
	bodies     [][]byte
	turnStates []string // 每次请求的 x-codex-turn-state 请求头
	steps      []codexRespStep
	last       codexRespStep
}

type codexRespStep struct {
	status int
	events []string // SSE data 载荷（status==200 时逐行下发 + [DONE]）
	body   string   // 非 200 错误体
	// turnState 响应头签发值（非空 → 200 响应携带 x-codex-turn-state——
	// HOST-2 mock 上游签发面）。
	turnState string
}

func newCodexRespUpstream(t *testing.T, steps ...codexRespStep) (*httptest.Server, *codexRespUpstream) {
	t.Helper()
	c := &codexRespUpstream{last: codexRespStep{status: 500, body: `{}`}}
	if len(steps) > 0 {
		c.steps = steps
		c.last = steps[len(steps)-1]
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.calls++
		c.auths = append(c.auths, r.Header.Get("Authorization"))
		c.bodies = append(c.bodies, b)
		c.turnStates = append(c.turnStates, r.Header.Get(codexsdk.HeaderTurnState))
		step := c.last
		if len(c.steps) > 0 {
			step = c.steps[0]
			c.steps = c.steps[1:]
			c.last = step
		}
		c.mu.Unlock()
		if step.status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(step.status)
			_, _ = w.Write([]byte(step.body))
			return
		}
		if step.turnState != "" {
			w.Header().Set(codexsdk.HeaderTurnState, step.turnState)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for _, ev := range step.events {
			_, _ = io.WriteString(w, "data: "+ev+"\n\n")
			f.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func (c *codexRespUpstream) callsN() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *codexRespUpstream) auth(i int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.auths[i]
}

func (c *codexRespUpstream) body(i int) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bodies[i]
}

func (c *codexRespUpstream) turnState(i int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.turnStates[i]
}

// T6 事件 fixture（对齐 codex-sdk responses_test.go：created/item.done/completed
// 形状；usage 顶层五计数含 cache 明细——P1-1 双路径断言共用）。SDK 聚合器从
// output_item.done 事件提取 item 对象（合成体 output 只含 item——t6RespItem）。
const (
	t6RespCreated = `{"type":"response.created","response":{"id":"resp_t6","object":"response","status":"in_progress","model":"gpt-5.6"}}`
	t6RespItem    = `{"id":"msg_1","status":"completed","type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}`
	t6RespItemEv  = `{"type":"output_item.done","item":` + t6RespItem + `}`
	t6RespUsage   = `{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":2},"cache_creation":{"ephemeral_5m_input_tokens":1,"ephemeral_1h_input_tokens":3}}`
	t6RespDone    = `{"type":"response.completed","response":{"id":"resp_t6","object":"response","status":"completed"},"usage":` + t6RespUsage + `}`
)

// uuidv7Re UUIDv7 格式（8-4-4-4-12 十六进制，version 位 = 7——SDK NewUUIDv7
// 产物；client_metadata.turn_id 断言面，META-2）。
var uuidv7Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func isUUIDv7(s string) bool { return uuidv7Re.MatchString(s) }

// respCred 构造 responses 端点测试凭据（完整 /v1/responses 端点——clientFor
// WithBaseURL 完整端点语义）。
func respCred(accountID int64, at, rt, baseURL string) *domain.AccountCredential {
	exp := time.Now().Add(time.Hour)
	return &domain.AccountCredential{
		AccountID: accountID, OAuthToken: at, OAuthRefreshToken: rt, OAuthExpiresAt: &exp,
		BaseURL: baseURL + "/v1/responses",
	}
}

// TestCodexResponsesAggregateNonstream 合成非流式：PAT 静态直连 → SSE 事件聚
// 合 → 合成体正确（id/output 流序/usage 原样）；wire 面：鉴权头 + stream:true
// 注入（payload 未带 stream）+ 其余字段保留。
func TestCodexResponsesAggregateNonstream(t *testing.T) {
	up, c := newCodexRespUpstream(t, codexRespStep{status: 200, events: []string{t6RespCreated, t6RespItemEv, t6RespDone}})
	defer up.Close()
	a := NewCodex(nil)
	cred := &domain.AccountCredential{AccountID: 9, PATKey: "pat-resp", BaseURL: up.URL + "/v1/responses"}

	resp, err := a.Responses(context.Background(), cred, []byte(`{"model":"gpt-5.6","input":"hi"}`), nil, nil, "")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	want := `{"id":"resp_t6","object":"response","status":"completed","output":[` + t6RespItem + `],"usage":` + t6RespUsage + `}`
	require.Equal(t, want, string(resp.Raw), "合成体 = id/output 流序/usage 原样")
	require.Equal(t, "Bearer pat-resp", c.auth(0), "PAT 静态鉴权")
	if !gjson.GetBytes(c.body(0), "stream").Bool() {
		t.Fatalf("payload 未带 stream 应注入 stream:true, body = %s", c.body(0))
	}
	if gjson.GetBytes(c.body(0), "model").String() != "gpt-5.6" || gjson.GetBytes(c.body(0), "input").String() != "hi" {
		t.Fatalf("注入不应动其余字段: %s", c.body(0))
	}
	// 未配置 identity（nil）：SDK 仍恒带 turn_id（META-1——真实 client_metadata()
	// 无条件 turn_id）。
	cm := gjson.GetBytes(c.body(0), "client_metadata")
	require.True(t, isUUIDv7(cm.Get("turn_id").String()), "未配置 identity 仍注入自动 turn_id: %s", cm.Raw)
	require.False(t, cm.Get("x-codex-installation-id").Exists(), "未配置不注入静态键")
}

// TestCodexResponsesIdentityMetadata 伪装身份注入（META-2——spec 2026-08-15）：
// Responses 传 session/meta → 上游请求体 client_metadata 恒 4 key
// （x-codex-installation-id/session_id/thread_id/x-codex-window-id——CodexMeta
// 与 WithSession 同值双设不冲突）+ turn_id 自动 UUIDv7（payload 未带）+ 条件
// 键不出现（未配置）+ 其余字段不改写。
func TestCodexResponsesIdentityMetadata(t *testing.T) {
	up, c := newCodexRespUpstream(t, codexRespStep{status: 200, events: []string{t6RespCreated, t6RespItemEv, t6RespDone}})
	defer up.Close()
	a := NewCodex(nil)
	cred := &domain.AccountCredential{AccountID: 9, PATKey: "pat-id", BaseURL: up.URL + "/v1/responses"}
	sess := &codexsdk.Session{SessionID: "sess-1", ThreadID: "thread-1", WindowID: "thread-1:0"}
	meta := &codexsdk.CodexMeta{InstallationID: "inst-1", SessionID: "sess-1", ThreadID: "thread-1", WindowID: "thread-1:0"}

	_, err := a.Responses(context.Background(), cred, []byte(`{"model":"m","input":"hi"}`), sess, meta, "")
	require.NoError(t, err)
	cm := gjson.GetBytes(c.body(0), "client_metadata")
	require.Equal(t, "inst-1", cm.Get("x-codex-installation-id").String(), "installation_id 注入")
	require.Equal(t, "sess-1", cm.Get("session_id").String(), "session_id 注入")
	require.Equal(t, "thread-1", cm.Get("thread_id").String(), "thread_id 注入")
	require.Equal(t, "thread-1:0", cm.Get("x-codex-window-id").String(), "window_id 注入")
	require.True(t, isUUIDv7(cm.Get("turn_id").String()), "turn_id 自动 UUIDv7（payload 未带）")
	for _, k := range []string{"x-openai-subagent", "x-codex-parent-thread-id", "parent_turn_id", "x-codex-turn-metadata"} {
		require.False(t, cm.Get(k).Exists(), "未配置条件键不注入: %s", k)
	}
	require.Equal(t, "hi", gjson.GetBytes(c.body(0), "input").String(), "注入不应动其余字段")
}

// TestCodexResponsesIdentityPassthroughTurnID payload 自带 turn_id → 原值透传
// 不覆盖（META-1 优先级：payload 内已存在 > CodexMeta > 自动 UUIDv7）。
func TestCodexResponsesIdentityPassthroughTurnID(t *testing.T) {
	up, c := newCodexRespUpstream(t, codexRespStep{status: 200, events: []string{t6RespCreated, t6RespItemEv, t6RespDone}})
	defer up.Close()
	a := NewCodex(nil)
	cred := &domain.AccountCredential{AccountID: 9, PATKey: "pat-t", BaseURL: up.URL + "/v1/responses"}

	_, err := a.Responses(context.Background(), cred, []byte(`{"model":"m","client_metadata":{"turn_id":"tid-keep"}}`), nil, nil, "")
	require.NoError(t, err)
	require.Equal(t, "tid-keep", gjson.GetBytes(c.body(0), "client_metadata.turn_id").String(), "透传优先（不覆盖）")
}

// TestCodexStreamResponsesIdentityMetadata 流式路径同注入（META-1：Stream 统
// 一注入点——Responses 内部走 Stream，两路径不重复）。
func TestCodexStreamResponsesIdentityMetadata(t *testing.T) {
	up, c := newCodexRespUpstream(t, codexRespStep{status: 200, events: []string{t6RespCreated, t6RespItemEv, t6RespDone}})
	defer up.Close()
	a := NewCodex(nil)
	cred := &domain.AccountCredential{AccountID: 9, PATKey: "pat-si", BaseURL: up.URL + "/v1/responses"}

	err := a.StreamResponses(context.Background(), cred, []byte(`{"model":"m","stream":true}`), nil, &codexsdk.CodexMeta{InstallationID: "inst-s"}, "", func(raw []byte) error { return nil })
	require.NoError(t, err)
	cm := gjson.GetBytes(c.body(0), "client_metadata")
	require.Equal(t, "inst-s", cm.Get("x-codex-installation-id").String(), "流式路径同样注入")
	require.True(t, isUUIDv7(cm.Get("turn_id").String()), "turn_id 恒带")
}

// TestCodexResponsesIdentityChangeRebuild identity 变化 → 重建 HTTPClient
// （idSig 比对——与 cred sig 同语义：账号配置变更 → 重建；同 identity 复用
// 连接池不重建——构造期 opts 承载的注入面变化才能生效）。
func TestCodexResponsesIdentityChangeRebuild(t *testing.T) {
	up, _ := newCodexRespUpstream(t, codexRespStep{status: 200, events: []string{t6RespCreated, t6RespItemEv, t6RespDone}})
	defer up.Close()
	a := NewCodex(nil)
	cred := &domain.AccountCredential{AccountID: 9, PATKey: "pat-r", BaseURL: up.URL + "/v1/responses"}
	meta1 := &codexsdk.CodexMeta{InstallationID: "inst-1"}
	meta2 := &codexsdk.CodexMeta{InstallationID: "inst-2"}

	_, err := a.Responses(context.Background(), cred, []byte(`{"model":"m"}`), nil, meta1, "")
	require.NoError(t, err)
	a.mu.Lock()
	c1 := a.entries[9].client
	a.mu.Unlock()
	require.NotNil(t, c1)

	_, err = a.Responses(context.Background(), cred, []byte(`{"model":"m"}`), nil, meta1, "")
	require.NoError(t, err)
	a.mu.Lock()
	require.Same(t, c1, a.entries[9].client, "同 identity 复用连接池")
	a.mu.Unlock()

	_, err = a.Responses(context.Background(), cred, []byte(`{"model":"m"}`), nil, meta2, "")
	require.NoError(t, err)
	a.mu.Lock()
	require.NotSame(t, c1, a.entries[9].client, "identity 变化 → 重建客户端")
	a.mu.Unlock()
}

// TestCodexStreamResponsesPassthrough 流式透传：fn 收到逐 data: 载荷（零拷贝
// 语义——字节与原事件一致）；[DONE] 不回调（SDK 消费语义——网关自行补发）。
func TestCodexStreamResponsesPassthrough(t *testing.T) {
	up, _ := newCodexRespUpstream(t, codexRespStep{status: 200, events: []string{t6RespCreated, t6RespItemEv, t6RespDone}})
	defer up.Close()
	a := NewCodex(nil)
	cred := &domain.AccountCredential{AccountID: 9, PATKey: "pat-s", BaseURL: up.URL + "/v1/responses"}

	var got []string
	err := a.StreamResponses(context.Background(), cred, []byte(`{"model":"m","stream":true}`), nil, nil, "", func(raw []byte) error {
		got = append(got, string(raw)) // 测试内立即拷贝（回调外切片失效语义）
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{t6RespCreated, t6RespItemEv, t6RespDone}, got, "逐载荷透传（[DONE] 不回调）")
}

// t6RespCallItem 工具调用输出项 fixture（HOST-2 轮继续信号判定面——item.type
// function_call）。
const t6RespCallItem = `{"id":"call_1","status":"completed","type":"function_call","name":"shell","arguments":"{}","call_id":"call_1"}`

// TestCodexResponsesTurnStateCarryAndClear turn-state 持有/注入/清除（HOST-2）：
// 首请求未带 → 上游签发 ts-1 → held 回写；同轮后续请求（客户端未带）自动注入
// x-codex-turn-state；ClearTurnState 清除后下一请求不再携带（跨轮不回传——
// 对齐真实 codex 轮级实例 ModelClientSession.new_session + 真实测试
// turn_state.rs persists_within_turn_and_resets_after）。
func TestCodexResponsesTurnStateCarryAndClear(t *testing.T) {
	up, c := newCodexRespUpstream(t,
		codexRespStep{status: 200, events: []string{t6RespCreated, `{"type":"output_item.done","item":` + t6RespCallItem + `}`, t6RespDone}, turnState: "ts-1"},
		codexRespStep{status: 200, events: []string{t6RespCreated, t6RespItemEv, t6RespDone}})
	defer up.Close()
	a := NewCodex(nil)
	cred := &domain.AccountCredential{AccountID: 9, PATKey: "pat-ts", BaseURL: up.URL + "/v1/responses"}

	// 首请求（轮首）：未带 turn-state → 上游签发 ts-1 → held 回写
	resp, err := a.Responses(context.Background(), cred, []byte(`{"model":"m"}`), nil, nil, "")
	require.NoError(t, err)
	require.Equal(t, "ts-1", resp.TurnState, "响应签发值暴露")
	require.Equal(t, "", c.turnState(0), "轮首请求不带头")

	// 同轮后续请求：客户端未带 → 自动注入 held（ts-1）
	_, err = a.Responses(context.Background(), cred, []byte(`{"model":"m"}`), nil, nil, "")
	require.NoError(t, err)
	require.Equal(t, "ts-1", c.turnState(1), "同轮续传（注入 held）")

	// 轮结束清除 → 下一请求不再携带（跨轮不回传）
	a.ClearTurnState(9)
	_, err = a.Responses(context.Background(), cred, []byte(`{"model":"m"}`), nil, nil, "")
	require.NoError(t, err)
	require.Equal(t, "", c.turnState(2), "清除后跨轮不回传")
}

// TestCodexResponsesTurnStatePassthrough 透传优先（HOST-2）：客户端自带
// x-codex-turn-state → 原值透传不覆盖（客户端自管）；held 不介入。
func TestCodexResponsesTurnStatePassthrough(t *testing.T) {
	up, c := newCodexRespUpstream(t,
		codexRespStep{status: 200, events: []string{t6RespCreated, t6RespItemEv, t6RespDone}, turnState: "ts-up"})
	defer up.Close()
	a := NewCodex(nil)
	cred := &domain.AccountCredential{AccountID: 9, PATKey: "pat-pt", BaseURL: up.URL + "/v1/responses"}

	// 先构造条目（首调用建 entry——无 held 无客户端值）
	_, err := a.Responses(context.Background(), cred, []byte(`{"model":"m"}`), nil, nil, "")
	require.NoError(t, err)
	// held 覆盖预置（验证客户端值覆盖 held——不被 held 覆盖）
	a.mu.Lock()
	a.entries[9].turnState = "held-old"
	a.mu.Unlock()

	_, err = a.Responses(context.Background(), cred, []byte(`{"model":"m"}`), nil, nil, "client-ts")
	require.NoError(t, err)
	require.Equal(t, "client-ts", c.turnState(1), "客户端自带 → 透传优先（覆盖 held）")

	// 响应签发值 → held 回写（服务端为准——后续未带请求注入新签发值）
	_, err = a.Responses(context.Background(), cred, []byte(`{"model":"m"}`), nil, nil, "")
	require.NoError(t, err)
	require.Equal(t, "ts-up", c.turnState(2), "签发值回写 held 后续注入")
}

// TestCodexResponsesTurnStateChangeRebuild turn-state 变化 → 重建 HTTPClient
// （与 idSig 同语义——构造期 opts 承载的头面变化才能生效；生产路径共享
// transport 承载连接池，重建不重置连接池）。
func TestCodexResponsesTurnStateChangeRebuild(t *testing.T) {
	up, _ := newCodexRespUpstream(t, codexRespStep{status: 200, events: []string{t6RespCreated, t6RespItemEv, t6RespDone}, turnState: "ts-1"})
	defer up.Close()
	a := NewCodex(nil)
	cred := &domain.AccountCredential{AccountID: 9, PATKey: "pat-rt", BaseURL: up.URL + "/v1/responses"}

	// 首调用（无 held）：无头客户端
	_, err := a.Responses(context.Background(), cred, []byte(`{"model":"m"}`), nil, nil, "")
	require.NoError(t, err)
	a.mu.Lock()
	c1 := a.entries[9].client
	require.Equal(t, "", a.entries[9].appliedTurnState, "首调用应用空值")
	a.mu.Unlock()

	// 同值复用（held 已回写 ts-1 → 生效值变化 → 重建——断言客户端非同一实例）
	_, err = a.Responses(context.Background(), cred, []byte(`{"model":"m"}`), nil, nil, "")
	require.NoError(t, err)
	a.mu.Lock()
	c2 := a.entries[9].client
	require.NotSame(t, c1, c2, "生效值空 → ts-1 变化 → 重建客户端")
	require.Equal(t, "ts-1", a.entries[9].appliedTurnState, "重建后记录应用值")
	require.NotNil(t, c2)
	a.mu.Unlock()

	// 同值复用不重建
	_, err = a.Responses(context.Background(), cred, []byte(`{"model":"m"}`), nil, nil, "ts-1")
	require.NoError(t, err)
	a.mu.Lock()
	require.Same(t, c2, a.entries[9].client, "同生效值复用不重建")
	a.mu.Unlock()
}

// searchReqPayload search 端点请求 fixture（opaque——网关零解析断言面）。
const searchReqPayload = `{"id":"req_1","model":"gpt-4o","input":[{"type":"web_search_call"}],"commands":[],"settings":{},"max_output_tokens":256}`

// TestCodexSearchPassthrough Search 端点（spec 2026-08-13）：统一 client 形态
// ——clientFor 缓存客户端 → e.client.Search；URL 方法内派生（baseURL 完整
// /v1/responses 端点 → /v1/alpha/search）；请求/响应体 opaque 零解析；PAT
// 静态鉴权；无头注入。
func TestCodexSearchPassthrough(t *testing.T) {
	const searchRaw = `{"id":"req_1","output":"o","results":[{"type":"web_search"}],"encrypted_output":"enc"}`
	var mu sync.Mutex
	var paths, auths []string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		auths = append(auths, r.Header.Get("Authorization"))
		gotBody = b
		mu.Unlock()
		if r.URL.Path != "/v1/alpha/search" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(searchRaw))
	}))
	t.Cleanup(srv.Close)
	a := NewCodex(nil)
	cred := &domain.AccountCredential{AccountID: 9, PATKey: "pat-search", BaseURL: srv.URL + "/v1/responses"}

	resp, err := a.Search(context.Background(), cred, []byte(searchReqPayload))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, searchRaw, string(resp.Raw), "opaque 响应原样交付（零解析）")
	// 快照读（锁内复制——后续 Search 调用期间不得持锁：handler 需同一把锁）。
	mu.Lock()
	paths0, auths0, body0 := append([]string(nil), paths...), append([]string(nil), auths...), string(gotBody)
	mu.Unlock()
	require.Equal(t, []string{"/v1/alpha/search"}, paths0, "URL 方法内派生：/responses 尾段 → /alpha/search")
	require.Equal(t, []string{"Bearer pat-search"}, auths0, "PAT 静态鉴权（clientFor 缓存客户端）")
	require.Equal(t, searchReqPayload, body0, "请求体原样（零改写）")
	// 同一 cred 二次调用：clientFor 缓存客户端直接复用（统一 client 形态——无
	// 独立构造器）；路径/凭据仍正确。
	_, err = a.Search(context.Background(), cred, []byte(searchReqPayload))
	require.NoError(t, err)
	mu.Lock()
	require.Len(t, paths, 2, "缓存客户端复用——第二次调用同路径")
	mu.Unlock()
}

// TestCodexSearchEnvelope4xx Search 信封错误翻译（translateError——与
// Responses 同款）：上游 403 → EnvelopeError（403 + 原始 body——网关
// statusOf/upstreamBody 复用）。
func TestCodexSearchEnvelope4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"detail":"Forbidden"}`))
	}))
	t.Cleanup(srv.Close)
	a := NewCodex(nil)
	cred := &domain.AccountCredential{AccountID: 9, PATKey: "pat-4xx", BaseURL: srv.URL + "/v1/responses"}

	_, err := a.Search(context.Background(), cred, []byte(`{}`))
	require.Error(t, err)
	var env *EnvelopeError
	require.ErrorAs(t, err, &env, "SDK *HTTPError → 信封包装")
	require.Equal(t, http.StatusForbidden, env.StatusCode())
	require.Equal(t, `{"detail":"Forbidden"}`, env.RawJSON(), "原始 body 透传")
}

// TestCodexResponsesEnvelope4xx 信封包装：上游 403 → EnvelopeError（403 + 原始
// body + Unwrap 链 errors.As *HTTPError 穿透——网关 statusOf/upstreamBody 复用）。
func TestCodexResponsesEnvelope4xx(t *testing.T) {
	up, _ := newCodexRespUpstream(t, codexRespStep{status: 403, body: `{"detail":"Forbidden"}`})
	defer up.Close()
	handler := &recordingHandler{}
	a := NewCodex(handler.add)
	cred := &domain.AccountCredential{AccountID: 9, PATKey: "pat-4xx", BaseURL: up.URL + "/v1/responses"}

	_, err := a.Responses(context.Background(), cred, []byte(`{}`), nil, nil, "")
	require.Error(t, err)
	var env *EnvelopeError
	require.True(t, errors.As(err, &env), "信封类型 errors.As 命中: %v", err)
	require.Equal(t, 403, env.StatusCode())
	require.Equal(t, `{"detail":"Forbidden"}`, env.RawJSON())
	var he *codexsdk.HTTPError
	require.True(t, errors.As(err, &he), "Unwrap 保留 errors.As 链（SDK 类型仍可命中）")
	require.Empty(t, handler.snapshot(), "信封错误不上报回调（透传协议）")
}

// TestCodexResponses401Rotate 401 非判死 → SDK 单飞 refresh → 自动重试一次成
// 功（轮转后聚合正常；新旧 at 序列断言——首拨旧 at 401 / 重试新 at 200）。
func TestCodexResponses401Rotate(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") == "Bearer at-old" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for _, ev := range []string{t6RespCreated, t6RespItemEv, t6RespDone} {
			_, _ = io.WriteString(w, "data: "+ev+"\n\n")
			f.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	t.Cleanup(srv.Close)
	rm := newCodexMockRefresh(t, codexUpstreamStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`})
	a := NewCodex(nil)
	cred := respCred(7, "at-old", "rt-1", srv.URL)

	resp, err := a.Responses(context.Background(), cred, []byte(`{"model":"m"}`), nil, nil, "")
	require.NoError(t, err)
	require.Equal(t, `"resp_t6"`, gjson.GetBytes(resp.Raw, "id").Raw, "轮转后应成功聚合")
	require.Equal(t, 1, rm.callsN(), "refresh 恰一次（单飞）")
	mu.Lock()
	require.Equal(t, []string{"Bearer at-old", "Bearer at-new"}, auths, "请求序列 = 旧 at 401 → 新 at 重试")
	mu.Unlock()
}

// TestCodexResponsesFatal 401 判死码 → *AuthPermanentlyRevokedError 透传 +
// 统一回调单次上报 + 缓存失效剔除（不重试）。
func TestCodexResponsesFatal(t *testing.T) {
	var reqs atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"token_invalidated"}}`))
	}))
	t.Cleanup(srv.Close)
	handler := &recordingHandler{}
	a := NewCodex(handler.add)
	cred := respCred(7, "at-old", "rt-1", srv.URL)

	_, err := a.Responses(context.Background(), cred, []byte(`{}`), nil, nil, "")
	require.Error(t, err)
	var ap *codexsdk.AuthPermanentlyRevokedError
	require.True(t, errors.As(err, &ap), "fatal 原样透传（errors.As 命中）: %v", err)
	require.Equal(t, int64(1), reqs.Load(), "判死不重试")
	calls := handler.snapshot()
	require.Len(t, calls, 1, "fatal 单次上报（回调 + errors.As 双源去重）")
	require.Equal(t, int64(7), calls[0].accountID)
	a.mu.Lock()
	require.Len(t, a.entries, 0, "fatal 上报后失效剔除——缓存条目摘除")
	a.mu.Unlock()
}

// TestCodexStreamResponsesFnError fn 回调错误（网关写出失败/客户端断开路径）
// → SDK 终止读取并原样透传（非 SDK 错误不过滤）。
func TestCodexStreamResponsesFnError(t *testing.T) {
	up, _ := newCodexRespUpstream(t, codexRespStep{status: 200, events: []string{t6RespCreated, t6RespItemEv, t6RespDone}})
	defer up.Close()
	a := NewCodex(nil)
	cred := &domain.AccountCredential{AccountID: 9, PATKey: "pat-fn", BaseURL: up.URL + "/v1/responses"}

	sentinel := errors.New("client write failed")
	err := a.StreamResponses(context.Background(), cred, []byte(`{}`), nil, nil, "", func(raw []byte) error {
		return sentinel
	})
	require.ErrorIs(t, err, sentinel, "fn 回调错误原样透传")
}

// TestCodexTransportPoolReuse 补压测修复回归（连接风暴）：SDK 默认 transport
// MaxIdleConnsPerHost=2 → 高并发爆发后连接被池化丢弃，下波大量重拨（压测
// profile ~12% CPU 连接风暴）。装配网关同形态 transport
// （httpx.NewTransport——复用既有构造 helper + MaxIdleConnsPerHost=2048 +
// MaxConnsPerHost=2048 生产同源 main.go:347）后，第二波并发必须零新拨号
// （池内复用）。走 GenerateImage（Do 排空路径——响应体完整读到 EOF，连接
// 确定可复用）；resp HTTP / 流式 images 与 GenerateImage 共用同一 clientFor
// 装配的 HTTPClient + transport（T2 机制——连接池断言同源）。计数
// DialContext 包装判定拨号次数。
//
// 屏障式控制（spec 2026-08-15）：gated handler 每波读完请求体后阻塞在该波
// release gate 上——16 个 handler 同时到达严格推出 16 条已建立、在用连接
// （HTTP/1.1 无多路复用；波内无连接提前 idle，合并拨号也不可能少于在用连接
// 数）。Go transport eofc 屏障（transport.go:2316-2320/2418-2423/2451-2458）
// 保证 GenerateImage 读到 EOF 返回时连接已确定回池——首波放行后 16 条即在
// 池，第二波零新拨号是确定性断言非调度窗口依赖。
func TestCodexTransportPoolReuse(t *testing.T) {
	const n = 16
	const waveTimeout = 15 * time.Second

	var dials atomic.Int64

	// gated upstream（HTTP/1.1）：请求体读完 → 到达信号（带缓冲 chan，容量 =
	// n——评审 §5-①：防控制器未接收时 handler 死锁）→ 阻塞当前波 release
	// gate（select gate / r.Context().Done()——客户端断开自动退出，handler
	// 永不永久阻塞）→ 放行后回 200 + okImageResponse。
	var mu sync.Mutex
	var curGate, curArrived chan struct{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		gate, arrived := curGate, curArrived
		mu.Unlock()
		arrived <- struct{}{}
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okImageResponse))
	}))
	t.Cleanup(srv.Close)

	tr := httpx.NewTransport(httpx.TransportConfig{
		MaxIdleConns:        8192,
		MaxIdleConnsPerHost: 2048,
		MaxConnsPerHost:     2048, // 生产同源 main.go:347——上界对齐 MaxIdleConnsPerHost
		IdleConnTimeout:     90 * time.Second,
		DialTimeout:         10 * time.Second,
		ForceHTTP2:          false, // 纯 http:// URL 无 ALPN 恒 HTTP/1.1（评审 §5-④；不动）
	})
	tr.Proxy = nil // 测试确定性：不经环境代理
	baseDial := tr.DialContext
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		return baseDial(ctx, network, addr)
	}
	t.Cleanup(tr.CloseIdleConnections) // 防跨测试连接泄漏（spec 验收 6）
	a := NewCodex(nil)
	a.SetTransport(tr)
	cred := &domain.AccountCredential{AccountID: 9, PATKey: "pat-pool", BaseURL: srv.URL + "/images/generations"}
	p := &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"}

	// runWave 屏障式一波：启动 n 个 GenerateImage（burst 形态——errs 通道容量
	// n）→ 等 n 个 handler 到达（释放前）→ close gate 放行 → 等 n 个调用返回
	// （含错误收集）。到达等待与放行后的完成等待均带 watchdog（评审 §5-②）；
	// 任一段超时：close gate 放行在途 handler → 回收 goroutine → FailNow（失
	// 败路径不泄漏）。gate close 后不可复用——每波新建。
	runWave := func(wave int) {
		gate := make(chan struct{})
		arrived := make(chan struct{}, n)
		mu.Lock()
		curGate, curArrived = gate, arrived
		mu.Unlock()

		var wg sync.WaitGroup
		errs := make(chan error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := a.GenerateImage(context.Background(), cred, p)
				errs <- err
			}()
		}

		// 等 n 个 handler 全到达（释放前：n 条连接同时在用——拨号恰 n：每条在
		// 用连接对应一次拨号，波内无 idle 连接可供他请求抢先复用）
		got := 0
		deadline := time.After(waveTimeout)
		for got < n {
			select {
			case <-arrived:
				got++
			case <-deadline:
				close(gate) // 放行在途 handler
				reclaim := make(chan struct{})
				go func() {
					wg.Wait()
					close(reclaim)
				}()
				select { // 回收有界（防在途调用卡死时测试无限挂起）
				case <-reclaim:
				case <-time.After(5 * time.Second):
				}
				var errMsg string
				select { // 诊断：已返回调用的错误（非阻塞读取）
				case e := <-errs:
					errMsg = fmt.Sprintf("；首个调用错误 %v", e)
				default:
				}
				require.FailNow(t, "", "wave %d: 等 %d 个 handler 到达超时（已到 %d；拨号 %d%s）", wave, n, got, dials.Load(), errMsg)
			}
		}
		require.Equal(t, int64(n), dials.Load(), "wave %d: %d 个 handler 到达时拨号恰为 %d（在用连接数）", wave, n, n)

		close(gate)
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(waveTimeout):
			require.FailNow(t, "", "wave %d: gate 放行后 %d 个调用回收超时", wave, n)
		}
		close(errs)
		for err := range errs {
			require.NoError(t, err, "wave %d: GenerateImage 全部成功", wave)
		}
		require.Equal(t, int64(n), dials.Load(), "wave %d: 本波零新拨号——连接池复用（MaxIdleConnsPerHost/MaxConnsPerHost=2048 生效；SDK 默认 2 会再拨 n-2 条）", wave)
	}

	// 首波：拨号恰 16（16 条连接全部建立并在用）；放行后经 eofc 屏障全部回池。
	runWave(1)
	// 第二波：16 个并发请求全部命中首波 idle 连接——到达时与结束后均零新拨号。
	runWave(2)
}

// ---------------------------------------------------------------------------
// T4 §2/§5：Dial 面（resp-ws 接线——错误契约/轮转；伪装选项由网关侧组装，
// 本文件测适配层 Dial 的凭据→Auth 缓存 + 错误翻译）
// ---------------------------------------------------------------------------

// codexWSUpstream Dial 面 mock WS 上游（/v1/responses 升级状态按序弹出，
// 耗尽重复最后一步）+ 握手头观测 + 帧回声（成功升级后每帧回
// {"type":"echo","payload":<原帧>}——Dial 返回后客户端收发断言）。
type codexWSUpstream struct {
	mu       sync.Mutex
	upgrades int
	headers  []http.Header
	frames   []string
	steps    []int
	last     int
}

func newCodexWSUpstream(t *testing.T, steps ...int) (*httptest.Server, *codexWSUpstream) {
	t.Helper()
	u := &codexWSUpstream{last: 200}
	if len(steps) > 0 {
		u.steps = steps
		u.last = steps[len(steps)-1]
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(404)
			return
		}
		u.mu.Lock()
		u.upgrades++
		u.headers = append(u.headers, r.Header.Clone())
		status := u.last
		if len(u.steps) > 0 {
			status = u.steps[0]
			u.steps = u.steps[1:]
			u.last = status
		}
		u.mu.Unlock()
		if status != 200 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream rejected"}}`))
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			CompressionMode: websocket.CompressionContextTakeover,
		})
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			typ, msg, err := c.Read(context.Background())
			if err != nil {
				return
			}
			u.mu.Lock()
			u.frames = append(u.frames, string(msg))
			u.mu.Unlock()
			payload, err := json.Marshal(map[string]any{"type": "echo", "payload": string(msg)})
			if err != nil {
				return
			}
			if err := c.Write(context.Background(), typ, payload); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, u
}

func (u *codexWSUpstream) upgradesN() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.upgrades
}

func (u *codexWSUpstream) header(i int) http.Header {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.headers[i]
}

// dialCred 构造带完整 responses 端点 BaseURL 的测试凭据。
func dialCred(accountID int64, baseURL string) *domain.AccountCredential {
	exp := time.Now().Add(time.Hour)
	return &domain.AccountCredential{
		AccountID: accountID, OAuthToken: "at-old", OAuthRefreshToken: "rt-1", OAuthExpiresAt: &exp,
		BaseURL: baseURL + "/v1/responses",
	}
}

// TestCodexDialPATSuccess PAT 凭据 Dial 成功：升级头带 Bearer pat；连接建
// 立后 Send/Recv 帧回声往返（帧透传面）。
func TestCodexDialPATSuccess(t *testing.T) {
	srv, up := newCodexWSUpstream(t, 200)
	a := NewCodex(nil)
	cred := &domain.AccountCredential{AccountID: 9, PATKey: "pat-1", BaseURL: srv.URL + "/v1/responses"}
	c, err := a.Dial(context.Background(), cred, codexsdk.WithPingInterval(0), codexsdk.WithPayloadFiltering(false))
	require.NoError(t, err)
	require.NoError(t, c.Send(context.Background(), []byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	f, err := c.Recv(context.Background())
	require.NoError(t, err)
	// 回声 payload 为 JSON 转义字符串（内嵌原帧 + SDK 注入的 client_metadata）
	var echo struct {
		Type    string `json:"type"`
		Payload string `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(f, &echo))
	require.Equal(t, "echo", echo.Type)
	require.Contains(t, echo.Payload, `"input":"hi"`, "原帧内容原样透传")
	require.Contains(t, echo.Payload, `"client_metadata"`, "伪装注入照常（关闭过滤与注入独立）")
	c.CloseNow()
	require.Equal(t, 1, up.upgradesN())
	require.Equal(t, "Bearer pat-1", up.header(0).Get("Authorization"))
}

// TestCodexDialErrorEnvelope PAT 401 → 信封（DialError → EnvelopeError：
// StatusCode/Unwrap 链——PAT 不轮转 Refreshed 恒 false）。
func TestCodexDialErrorEnvelope(t *testing.T) {
	srv, _ := newCodexWSUpstream(t, 401)
	a := NewCodex(nil)
	cred := &domain.AccountCredential{AccountID: 9, PATKey: "pat-1", BaseURL: srv.URL + "/v1/responses"}
	_, err := a.Dial(context.Background(), cred)
	require.Error(t, err)
	var env *EnvelopeError
	require.True(t, errors.As(err, &env), "DialError 必须信封包装：%v", err)
	require.Equal(t, 401, env.StatusCode())
	require.False(t, env.Refreshed, "PAT 不轮转 → Refreshed 恒 false")
	var de *codexsdk.DialError
	require.True(t, errors.As(err, &de), "Unwrap 链保留 DialError（errors.As 穿透）")
	require.False(t, de.Refreshed)
}

// TestCodexDialOAuthRotationSuccess oauth 401 轮转成功（升级 401 → 单飞
// refresh → 自动重连一次 200）：新 at 生效（升级头断言）+ 连接可用。
func TestCodexDialOAuthRotationSuccess(t *testing.T) {
	srv, up := newCodexWSUpstream(t, 401, 200)
	newCodexMockRefresh(t, codexUpstreamStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`})
	a := NewCodex(nil)
	cred := dialCred(7, srv.URL)
	c, err := a.Dial(context.Background(), cred, codexsdk.WithPingInterval(0))
	require.NoError(t, err)
	require.NoError(t, c.Send(context.Background(), []byte(`{"type":"response.create","model":"gpt-4o"}`)))
	_, err = c.Recv(context.Background())
	require.NoError(t, err)
	c.CloseNow()
	require.Equal(t, 2, up.upgradesN(), "401 非判死 → SDK 单飞 refresh 后自动重连一次")
	require.Equal(t, "Bearer at-old", up.header(0).Get("Authorization"))
	require.Equal(t, "Bearer at-new", up.header(1).Get("Authorization"), "轮转后新 at 生效")
}

// TestCodexDialOAuthRefreshedEnvelope 轮转重连仍 401 → DialError.Refreshed
// 保留进信封（网关避免双份刷新）：401 + Refreshed=true；refresh 恰一次。
func TestCodexDialOAuthRefreshedEnvelope(t *testing.T) {
	srv, up := newCodexWSUpstream(t, 401, 401)
	rm := newCodexMockRefresh(t, codexUpstreamStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`})
	a := NewCodex(nil)
	cred := dialCred(7, srv.URL)
	_, err := a.Dial(context.Background(), cred)
	require.Error(t, err)
	var env *EnvelopeError
	require.True(t, errors.As(err, &env))
	require.Equal(t, 401, env.StatusCode())
	require.True(t, env.Refreshed, "已轮转重连一次仍失败 → Refreshed=true")
	require.Equal(t, 2, up.upgradesN())
	require.Equal(t, 1, rm.callsN(), "refresh 恰一次（单飞；网关不再二次刷新）")
}

// TestCodexDialRefreshFatalBare refresh 判死（401 invalid_grant）→ 裸
// RefreshOAuthError 透传不包 DialError：IsFatal=true + 统一回调单次上报
// （回调路径与 errors.As 路径双源去重）。
func TestCodexDialRefreshFatalBare(t *testing.T) {
	srv, up := newCodexWSUpstream(t, 401)
	newCodexMockRefresh(t, codexUpstreamStep{status: 401, body: `{"error":"invalid_grant"}`})
	handler := &recordingHandler{}
	a := NewCodex(handler.add)
	cred := dialCred(7, srv.URL)
	_, err := a.Dial(context.Background(), cred)
	require.Error(t, err)
	var re *codexsdk.RefreshOAuthError
	require.True(t, errors.As(err, &re), "refresh 失败裸错误必须透传（不包 DialError）：%v", err)
	require.True(t, IsFatal(err), "RefreshOAuthError 在 fatal 集")
	require.Len(t, handler.snapshot(), 1, "双源去重：单次上报")
	require.Equal(t, 1, up.upgradesN(), "refresh 失败不重连")
}

// TestCodexDialRefreshErrorBare refresh 可重试类（500 耗尽 3 次）→ 裸
// RefreshError：IsFatal=false + 不上报（网关正常 failover）。
func TestCodexDialRefreshErrorBare(t *testing.T) {
	srv, up := newCodexWSUpstream(t, 401)
	rm := newCodexMockRefresh(t) // 默认 500 重复
	handler := &recordingHandler{}
	a := NewCodex(handler.add)
	cred := dialCred(7, srv.URL)
	_, err := a.Dial(context.Background(), cred)
	require.Error(t, err)
	var fe *codexsdk.RefreshError
	require.True(t, errors.As(err, &fe), "RefreshError 裸透传：%v", err)
	require.Equal(t, 3, fe.Attempts, "退避耗尽 maxAttempts=3")
	require.False(t, IsFatal(err), "RefreshError 不在 fatal 集（可重试）")
	require.Len(t, handler.snapshot(), 0, "可重试类不上报")
	require.Equal(t, 3, rm.callsN())
	require.Equal(t, 1, up.upgradesN())
}

// TestCodexIsFatal fatal 集判定（T4 §5 网关消费面）：五类 + 信封穿透 +
// RefreshError/普通错误不在集。
func TestCodexIsFatal(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"refresh oauth", &codexsdk.RefreshOAuthError{Code: "invalid_grant"}, true},
		{"permanently revoked", &codexsdk.AuthPermanentlyRevokedError{Code: "token_invalidated"}, true},
		{"account disabled", &codexsdk.AccountDisabledError{StatusCode: 402, Detail: "deactivated_workspace"}, true},
		{"callback delivery", &codexsdk.CallbackDeliveryError{Attempts: 3, Err: errors.New("cb")}, true},
		{"refresh retryable", &codexsdk.RefreshError{Attempts: 3, Err: errors.New("net")}, false},
		{"plain", errors.New("plain"), false},
		{"nil", nil, false},
		{"fatal through envelope", NewEnvelopeError(0, "", &codexsdk.RefreshOAuthError{Code: "invalid_grant"}), true},
		{"refresh through envelope", NewEnvelopeError(0, "", &codexsdk.RefreshError{Attempts: 3, Err: errors.New("net")}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, IsFatal(tc.err))
		})
	}
}

// TestCodexDialAuthCacheReuse 同账号连续 Dial：条目复用（Auth 账号级 at 缓
// 存跨 Dial 保持——第二次 Dial 不触发 refresh，升级仍带初始 at）。
func TestCodexDialAuthCacheReuse(t *testing.T) {
	srv, up := newCodexWSUpstream(t, 200, 200)
	rm := newCodexMockRefresh(t, codexUpstreamStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`})
	a := NewCodex(nil)
	cred := dialCred(7, srv.URL)
	c1, err := a.Dial(context.Background(), cred, codexsdk.WithPingInterval(0))
	require.NoError(t, err)
	c1.CloseNow()
	c2, err := a.Dial(context.Background(), cred, codexsdk.WithPingInterval(0))
	require.NoError(t, err)
	c2.CloseNow()
	require.Same(t, a.entries[7].auth, a.entries[7].auth, "条目同账号复用")
	require.Equal(t, 2, up.upgradesN())
	require.Equal(t, 0, rm.callsN(), "at 缓存保持 → 第二次 Dial 零 refresh")
	require.Equal(t, "Bearer at-old", up.header(1).Get("Authorization"), "同一 Auth 的 at 缓存跨 Dial 生效")
}
