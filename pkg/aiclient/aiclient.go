// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package aiclient 负责上游 HTTP 出栈：URL 派生、懒缓存、凭据头注入与客户端头
// 中转。所有协议面（流式/非流式/图片/search/ws）统一走网关自己构造的原始请求，
// 不再经由 openai-go / anthropic-sdk-go 客户端——SDK 只作为测试依赖，用于验证
// 真实客户端能正确消费本网关产出的字节。
package aiclient

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

type Config struct {
	UpstreamTimeout       time.Duration // 非流式调用超时
	UpstreamStreamTimeout time.Duration // 流式 backstop（由调用方以 ctx 传入）
}

type Factory struct {
	hc  *http.Client
	cfg Config
	// 客户端与 URL 缓存：单原子快照 copy-modify-Store（同 scheduler 惯例），
	// 读路径零锁零共享计数——原全局互斥锁每请求一次，万级并发下
	// 单字缓存行弹跳。写路径（懒构建/失效）CAS 重试。
	cc atomic.Pointer[clientCache]
}

// clientCache 不可变快照。gen 为失效代号：构建者捕获 gen 后在锁外构建，CAS
// 存回时 gen 已变（InvalidateAll 发生）即弃件重建——等价于旧互斥锁的全序
// 语义。byT 的客户端把 base_url 烤进构造参数而键只有模板 ID，无 gen 校验会
// 产生"失效后陈旧客户端复活"；urls 是键的纯函数本无此害，统一用 gen 省一套
// 心智模型。
type clientCache struct {
	gen  int64
	urls map[urlKey]*url.URL
}

// urlKey 完整 URL 缓存键：模板 ID + base_url 快照 + 格式路径。
type urlKey struct {
	templateID int64
	baseURL    string
	path       string
}

// 流式原始请求的预解析完整 URL 懒缓存（GC 削减 rawPost 每请求免
// url.Parse+JoinPath）。键含 baseURL：绕过管理 API 的直接 DB
// 改 base_url 后，周期同步刷新 Selection.BaseURL 即命中新键收敛到新上游——
// 旧键残留由 InvalidateAll（管理 API 变更）整体清空，直接 DB 变更的旧键
// 至多每格式一条、体积可忽略。管理 API 变更链路（invalidate 成对失效）
// 不受影响。

func NewFactory(hc *http.Client, cfg Config) *Factory {
	f := &Factory{hc: hc, cfg: cfg}
	f.cc.Store(&clientCache{
		gen:  0,
		urls: make(map[urlKey]*url.URL),
	})
	return f
}

// InvalidateAll 模板变更后丢弃所有客户端与 URL 缓存（base_url 变化生效）。
// gen 自增使在途构建者 CAS 失败弃件重建，等价旧互斥锁全序语义。
func (f *Factory) InvalidateAll() {
	for {
		cur := f.cc.Load()
		if f.cc.CompareAndSwap(cur, &clientCache{
			gen:  cur.gen + 1,
			urls: make(map[urlKey]*url.URL),
		}) {
			return
		}
	}
}

// RawPost 通用原始 POST：非流式与流式共用同一出栈（路径、鉴权头名由调用方
// 按格式显式选定；status 检查与 body 关闭由调用方负责）。
func (f *Factory) RawPost(ctx context.Context, templateID int64, baseURL, path, authHeader, auth string, body []byte, in http.Header) (*http.Response, error) {
	return f.rawPost(ctx, templateID, baseURL, path, authHeader, auth, body, in)
}

// --- 流式原始请求（SSE relay 用） ---
// SDK 的请求层在 internal/requestconfig（不可 import），故基于共享 http.Client
// 构造原始请求：注入鉴权头、使用 SDK 客户端同款连接池与超时。
// 返回完整 *http.Response，status 检查与 body 关闭由调用方负责。
// 签名收 (templateID, baseURL) 而非 *domain.Template（GC 削减 调用方免
// tplOf 每请求模板对象分配；URL 在 Factory.urls 懒缓存，键含 base_url 快照）。

func (f *Factory) ChatCompletionRaw(ctx context.Context, templateID int64, baseURL, key string, body []byte, in http.Header) (*http.Response, error) {
	return f.rawPost(ctx, templateID, baseURL, "chat/completions", "Authorization", "Bearer "+key, body, in)
}

func (f *Factory) ResponseRaw(ctx context.Context, templateID int64, baseURL, key string, body []byte, in http.Header) (*http.Response, error) {
	return f.rawPost(ctx, templateID, baseURL, "responses", "Authorization", "Bearer "+key, body, in)
}

// SearchRaw codex search 端点直连透传（spec 2026-08-13：api_key/responses-
// special 静态路径——Bearer upstream key 直连上游，复用既有静态 key 通道零新
// 机制）。URL 派生 = 裸根 + /v1/alpha/search（openaiBaseURL 约定——与 responses
// 端点 base/v1/responses 尾段 → /alpha/search 派生同语义，见 parseFullURL）。
func (f *Factory) SearchRaw(ctx context.Context, templateID int64, baseURL, key string, body []byte, in http.Header) (*http.Response, error) {
	return f.rawPost(ctx, templateID, baseURL, "alpha/search", "Authorization", "Bearer "+key, body, in)
}

func (f *Factory) AnthMessageRaw(ctx context.Context, templateID int64, baseURL, key string, body []byte, in http.Header) (*http.Response, error) {
	return f.rawPost(ctx, templateID, baseURL, "v1/messages", "x-api-key", key, body, in)
}

// --- openai images（直连面） ---
// images 端点直连透传（JSON + multipart 双协议）：multipart 的 Content-Type
// 含 boundary（图片文件原样透传），JSON 传空串由 rawPostCT 补
// application/json。无 SDK 参数路径——直连语义 = 原始请求原样转发（响应
// 零改写零损失；上游路径 /v1/images/generations|edits 由调用方传 path）。

func (f *Factory) ImagesRaw(ctx context.Context, templateID int64, baseURL, path, key, contentType string, body []byte, in http.Header) (*http.Response, error) {
	return f.rawPostCT(ctx, templateID, baseURL, path, "Authorization", "Bearer "+key, contentType, body, in)
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
// （base 非法）返回错误。读路径零锁：命中直返；未命中锁外解析 + CAS-COW 回
// 写（urls 是键的纯函数，并发未命中重复解析最后写入者胜出，无害）。
func (f *Factory) fullURLOf(templateID int64, baseURL, path string) (*url.URL, error) {
	k := urlKey{templateID: templateID, baseURL: baseURL, path: path}
	for {
		cur := f.cc.Load()
		if u := cur.urls[k]; u != nil {
			return u, nil
		}
		u, err := parseFullURL(baseURL, path)
		if err != nil {
			return nil, err
		}
		if f.cc.CompareAndSwap(cur, &clientCache{gen: cur.gen, urls: cloneAdd(cur.urls, k, u)}) {
			return u, nil
		}
		// 并发插入/失效：重试，命中检查会吃掉他人已缓存的同键
	}
}

// cloneAdd 复制 map 快照并写入一个额外条目（URL 缓存 COW 回写；规模 = 模板×
// 格式，量级数十至百，未命中仅在预热期发生，O(n) 拷贝可忽略）。
func cloneAdd[K comparable, V any](m map[K]V, k K, v V) map[K]V {
	out := make(map[K]V, len(m)+1)
	for kk, vv := range m {
		out[kk] = vv
	}
	out[k] = v
	return out
}

// parseFullURL 由模板 base_url 与格式路径派生完整 URL：路径自带 v1/ 前缀
// （v1/messages、v1/responses）→ 不补 /v1；其余 openai 系路径裸根 + /v1。
func parseFullURL(base, path string) (*url.URL, error) {
	// query 必须先切分：url.JoinPath 会把 "?" 百分号转义成 %3F（实测），
	// 直接塞进 JoinPath 会产出错误 URL。
	pathPart, query := path, ""
	if i := strings.IndexByte(path, '?'); i >= 0 {
		pathPart, query = path[:i], path[i+1:]
	}
	// 路径自带 v1 前缀（anthropic v1/messages、resp-ws v1/responses——真实端点
	// 无 /ws 后缀）→ 不再补 /v1；其余 openai 系路径裸根 + /v1。
	if !strings.HasPrefix(pathPart, "v1/") {
		base = openaiBaseURL(base)
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	// 统一 JoinPath：openai → /v1/chat/completions；anthropic → /v1/messages。
	// 不做 Parse 尾段替换——那是对 base 约定含 /v1 时代的 hack，约定根除后不再需要。
	full := u.JoinPath(pathPart)
	// JoinPath 对空路径 base（anthropic 裸根）产出无前导斜杠的 Path
	// （"v1/messages"）——RequestURI 直接拼进请求行会变非法请求行。旧实现经
	// NewRequestWithContext 重解析字符串天然补回前导斜杠；此处显式归一。
	if full.Path != "" && full.Path[0] != '/' {
		full.Path = "/" + full.Path
	}
	if query != "" {
		full.RawQuery = query
	}
	return full, nil
}

// rawPost 构造并发出原始 POST（GC 削减 URL 预解析缓存 + 手工构造
// *http.Request，免 NewRequestWithContext 的内部分配；GetBody 保留重定向
// 语义，WithContext 保留 ctx 取消语义）。authHeader 为承载凭据的请求头名
// （openai 系 Authorization、anthropic x-api-key），auth 为其值——鉴权头名
// 由调用方按端点显式选定（此前的 path=="v1/messages" 内部分派已上移）。
func (f *Factory) rawPost(ctx context.Context, templateID int64, baseURL, path, authHeader, auth string, body []byte, in http.Header) (*http.Response, error) {
	return f.rawPostCT(ctx, templateID, baseURL, path, authHeader, auth, "", body, in)
}

// rawPostCT rawPost 的 Content-Type 定制变体（images multipart 需要
// 完整 multipart/form-data Content-Type——含 boundary；contentType 空 →
// application/json，与 rawPost 逐字节等价）。
// 零**映射**为契约：出栈头 = RelayHeaders(in) − relayDeny + 网关自身声明
// （Content-Type 按端点、账号鉴权头由 authHeader 指定）。网关不把客户端头
// 翻译成上游头，也不丢弃除 relayDeny 之外的客户端头；in 为 nil 时出栈只剩
// 网关声明（与 relay 前的现状逐字节等价）。清单与根规则见 relay.go（WS 静态
// 面直接调 RelayHeaders，共用同一份）。
func (f *Factory) rawPostCT(ctx context.Context, templateID int64, baseURL, path, authHeader, auth, contentType string, body []byte, in http.Header) (*http.Response, error) {
	full, err := f.fullURLOf(templateID, baseURL, path)
	if err != nil {
		return nil, err
	}
	req := &http.Request{
		Method:        http.MethodPost,
		URL:           full,
		Header:        RelayHeaders(in),
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		GetBody:       func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil },
	}
	if contentType == "" {
		contentType = "application/json"
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set(authHeader, auth)
	req = req.WithContext(ctx)
	return f.hc.Do(req)
}
