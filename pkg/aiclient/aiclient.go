// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package aiclient 是 openai/anthropic 官方 SDK 的唯一引用点：客户端懒构建 +
// 鉴权头注入 + 非流式超时策略。协议类型（params/response/stream）直接透传为
// 调用签名——它们是协议本身，隐藏它们等于重写协议（违背"用现成库"）。
//
// 版本说明（openai-go v1.12.0 / anthropic-sdk-go v1.56.0）：客户端选项在
// option 子包（option.WithBaseURL/WithHTTPClient/WithHeader）；流式入口
// NewStreaming 在请求选项层注入 "stream": true，参数里不再有 Stream 字段。
// 调用方按原始请求体里的 stream 字段决定走 SDK 非流式（本文件）还是原始
// 流式请求（StreamRaw 系列，流式 SSE relay 用，见下方）。
package aiclient

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/openai/openai-go"
	openaioption "github.com/openai/openai-go/option"
	"github.com/openai/openai-go/responses"

	"github.com/is7qin/c3api/internal/domain"
)

type Config struct {
	UpstreamTimeout       time.Duration // 非流式调用超时
	UpstreamStreamTimeout time.Duration // 流式 backstop（由调用方以 ctx 传入）
}

type Factory struct {
	hc  *http.Client
	cfg Config
	mu  sync.Mutex
	byT map[int64]*TemplateClients
	// 流式原始请求的预解析完整 URL 懒缓存（GC 削减 P2：rawPost 每请求免
	// url.Parse+JoinPath）。键含 baseURL（评审 C1）：绕过管理 API 的直接 DB
	// 改 base_url 后，周期同步刷新 Selection.BaseURL 即命中新键收敛到新上游——
	// 旧键残留由 InvalidateAll（管理 API 变更）整体清空，直接 DB 变更的旧键
	// 至多每格式一条、体积可忽略。管理 API 变更链路（invalidate 成对失效）
	// 不受影响。
	urls map[urlKey]*url.URL
}

// urlKey 完整 URL 缓存键：模板 ID + base_url 快照 + 格式路径。
type urlKey struct {
	templateID int64
	baseURL    string
	path       string
}

type TemplateClients struct {
	chat      *openai.Client
	responses *openai.Client
	anthropic *anthropic.Client
}

func NewFactory(hc *http.Client, cfg Config) *Factory {
	return &Factory{hc: hc, cfg: cfg, byT: make(map[int64]*TemplateClients), urls: make(map[urlKey]*url.URL)}
}

// InvalidateAll 模板变更后丢弃所有客户端与 URL 缓存（base_url 变化生效）。
func (f *Factory) InvalidateAll() {
	f.mu.Lock()
	f.byT = make(map[int64]*TemplateClients)
	f.urls = make(map[urlKey]*url.URL)
	f.mu.Unlock()
}

// --- openai chat/completions ---

// ChatCompletion 非流式调用（内部注入鉴权头 + 超时）。
func (f *Factory) ChatCompletion(ctx context.Context, tpl *domain.Template, key string, params openai.ChatCompletionNewParams) (*openai.ChatCompletion, error) {
	ctx, cancel := context.WithTimeout(ctx, f.cfg.UpstreamTimeout)
	defer cancel()
	return f.chat(tpl).Chat.Completions.New(ctx, params, openaioption.WithHeader("Authorization", "Bearer "+key))
}

// --- openai responses ---

func (f *Factory) Response(ctx context.Context, tpl *domain.Template, key string, params responses.ResponseNewParams) (*responses.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, f.cfg.UpstreamTimeout)
	defer cancel()
	return f.responses(tpl).Responses.New(ctx, params, openaioption.WithHeader("Authorization", "Bearer "+key))
}

// --- anthropic messages ---

func (f *Factory) AnthMessage(ctx context.Context, tpl *domain.Template, key string, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	ctx, cancel := context.WithTimeout(ctx, f.cfg.UpstreamTimeout)
	defer cancel()
	return f.anthropic(tpl).Messages.New(ctx, params, anthropicoption.WithHeader("x-api-key", key))
}

// --- 流式原始请求（SSE relay 用） ---
// SDK 的请求层在 internal/requestconfig（不可 import），故基于共享 http.Client
// 构造原始请求：注入鉴权头、使用 SDK 客户端同款连接池与超时。
// 返回完整 *http.Response，status 检查与 body 关闭由调用方负责。
// 签名收 (templateID, baseURL) 而非 *domain.Template（GC 削减 P6：调用方免
// tplOf 每请求模板对象分配；URL 在 Factory.urls 懒缓存，键含 base_url 快照）。

func (f *Factory) ChatCompletionStreamRaw(ctx context.Context, templateID int64, baseURL, key string, body []byte) (*http.Response, error) {
	return f.rawPost(ctx, templateID, baseURL, "chat/completions", "Bearer "+key, body)
}

func (f *Factory) ResponseStreamRaw(ctx context.Context, templateID int64, baseURL, key string, body []byte) (*http.Response, error) {
	return f.rawPost(ctx, templateID, baseURL, "responses", "Bearer "+key, body)
}

// SearchRaw codex search 端点直连透传（spec 2026-08-13：api_key/responses-
// special 静态路径——Bearer upstream key 直连上游，复用既有静态 key 通道零新
// 机制）。URL 派生 = 裸根 + /v1/alpha/search（openaiBaseURL 约定——与 responses
// 端点 base/v1/responses 尾段 → /alpha/search 派生同语义，见 parseFullURL）。
func (f *Factory) SearchRaw(ctx context.Context, templateID int64, baseURL, key string, body []byte) (*http.Response, error) {
	return f.rawPost(ctx, templateID, baseURL, "alpha/search", "Bearer "+key, body)
}

func (f *Factory) AnthMessageStreamRaw(ctx context.Context, templateID int64, baseURL, key string, body []byte) (*http.Response, error) {
	return f.rawPost(ctx, templateID, baseURL, "v1/messages", key, body)
}

// --- openai images（Task B 直连面） ---
// images 端点直连透传（JSON + multipart 双协议）：multipart 的 Content-Type
// 含 boundary（图片文件原样透传），JSON 传空串由 rawPostCT 补
// application/json。无 SDK 参数路径——直连语义 = 原始请求原样转发（响应
// 零改写零损失；上游路径 /v1/images/generations|edits 由调用方传 path）。

func (f *Factory) ImagesRaw(ctx context.Context, templateID int64, baseURL, path, key, contentType string, body []byte) (*http.Response, error) {
	return f.rawPostCT(ctx, templateID, baseURL, path, "Bearer "+key, contentType, body)
}

// openaiBaseURL 规范化 openai 系 SDK 的 BaseURL：openai-go 约定 BaseURL 含 /v1
// （内部拼接 "chat/completions"）。模板 base_url 约定为**裸根**（不含 /v1——
// /v1 是协议细节，由本层按格式追加，见模板校验），故 openai 系在此补 /v1。
func openaiBaseURL(base string) string {
	return strings.TrimSuffix(base, "/") + "/v1"
}

// fullURLOf 取模板的完整请求 URL（懒缓存：键 = 模板 ID + base_url 快照 + 格式
// 路径，首访解析后复用——同一快照收敛后复用缓存，快照变化（DB 直改 base_url
// 周期同步）即新键解析新地址，逐请求等价于旧实现的按快照解析）。失败
// （base 非法）返回错误。
func (f *Factory) fullURLOf(templateID int64, baseURL, path string) (*url.URL, error) {
	k := urlKey{templateID: templateID, baseURL: baseURL, path: path}
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.urls[k]
	if u == nil {
		var err error
		u, err = parseFullURL(baseURL, path)
		if err != nil {
			return nil, err
		}
		f.urls[k] = u
	}
	return u, nil
}

func parseFullURL(base, path string) (*url.URL, error) {
	// 路径自带 v1 前缀（anthropic v1/messages、resp-ws v1/responses——真实端点
	// 无 /ws 后缀）→ 不再补 /v1；其余 openai 系路径裸根 + /v1。
	if !strings.HasPrefix(path, "v1/") {
		base = openaiBaseURL(base)
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	// 统一 JoinPath：openai → /v1/chat/completions；anthropic → /v1/messages。
	// 不做 Parse 尾段替换——那是对 base 约定含 /v1 时代的 hack，约定根除后不再需要。
	full := u.JoinPath(path)
	// JoinPath 对空路径 base（anthropic 裸根）产出无前导斜杠的 Path
	// （"v1/messages"）——RequestURI 直接拼进请求行会变非法请求行。旧实现经
	// NewRequestWithContext 重解析字符串天然补回前导斜杠；此处显式归一。
	if full.Path != "" && full.Path[0] != '/' {
		full.Path = "/" + full.Path
	}
	return full, nil
}

// rawPost 构造并发出原始 POST（GC 削减 P2：URL 预解析缓存 + 手工构造
// *http.Request，免 NewRequestWithContext 的内部分配；GetBody 保留重定向
// 语义，WithContext 保留 ctx 取消语义）。auth 为 Authorization 值
// （anthropic 用 x-api-key，传 key 本身）。
func (f *Factory) rawPost(ctx context.Context, templateID int64, baseURL, path, auth string, body []byte) (*http.Response, error) {
	return f.rawPostCT(ctx, templateID, baseURL, path, auth, "", body)
}

// rawPostCT rawPost 的 Content-Type 定制变体（Task B images multipart 需要
// 完整 multipart/form-data Content-Type——含 boundary；contentType 空 →
// application/json，与 rawPost 逐字节等价）。
// 零透传为契约：HTTP 面只设 Content-Type 与账号鉴权头（Authorization /
// anthropic 用 x-api-key 传 key 本身），客户端原始头一律不达上游——与 WS 面
// 透传的差异是有意边界非遗漏（WS 面契约见 internal/proxy/caller_responses_ws.go
// wsPassthroughHeaders），SDK 伪装研究完成前此面不动。
func (f *Factory) rawPostCT(ctx context.Context, templateID int64, baseURL, path, auth, contentType string, body []byte) (*http.Response, error) {
	full, err := f.fullURLOf(templateID, baseURL, path)
	if err != nil {
		return nil, err
	}
	req := &http.Request{
		Method:        http.MethodPost,
		URL:           full,
		Header:        make(http.Header),
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		GetBody:       func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil },
	}
	if contentType == "" {
		contentType = "application/json"
	}
	req.Header.Set("Content-Type", contentType)
	if path == "v1/messages" {
		req.Header.Set("x-api-key", auth)
	} else {
		req.Header.Set("Authorization", auth)
	}
	req = req.WithContext(ctx)
	return f.hc.Do(req)
}

// --- 客户端懒构建（每模板最多 3 个，共享 http.Client，规格 §6.1） ---

func (f *Factory) chat(tpl *domain.Template) *openai.Client {
	f.mu.Lock()
	defer f.mu.Unlock()
	tc := f.ensure(tpl.ID)
	if tc.chat == nil {
		c := openai.NewClient(
			openaioption.WithBaseURL(openaiBaseURL(tpl.BaseURL)),
			openaioption.WithHTTPClient(f.hc),
			// 关闭 SDK 内置重试：转移/退避由调度器统一控制（规格 §5），
			// SDK 在单次调用内静默重试会让 429 背压放大并阻塞热路径。
			openaioption.WithMaxRetries(0),
		)
		tc.chat = &c
	}
	return tc.chat
}

func (f *Factory) responses(tpl *domain.Template) *openai.Client {
	f.mu.Lock()
	defer f.mu.Unlock()
	tc := f.ensure(tpl.ID)
	if tc.responses == nil {
		c := openai.NewClient(
			openaioption.WithBaseURL(openaiBaseURL(tpl.BaseURL)),
			openaioption.WithHTTPClient(f.hc),
			openaioption.WithMaxRetries(0),
		)
		tc.responses = &c
	}
	return tc.responses
}

func (f *Factory) anthropic(tpl *domain.Template) *anthropic.Client {
	f.mu.Lock()
	defer f.mu.Unlock()
	tc := f.ensure(tpl.ID)
	if tc.anthropic == nil {
		c := anthropic.NewClient(
			anthropicoption.WithBaseURL(tpl.BaseURL),
			anthropicoption.WithHTTPClient(f.hc),
			anthropicoption.WithMaxRetries(0),
		)
		tc.anthropic = &c
	}
	return tc.anthropic
}

func (f *Factory) ensure(id int64) *TemplateClients {
	tc, ok := f.byT[id]
	if !ok {
		tc = &TemplateClients{}
		f.byT[id] = tc
	}
	return tc
}
