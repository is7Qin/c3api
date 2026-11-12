// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package probe 执行 account-model-test 的单账号单模型探测：用账号自身的凭据、
// 出口与模板规则向上游发一条最小请求，返回「模型是否可用 + 延迟 + 失败原因」。
//
// 只读诊断观测：不落库、不计费、不改账号状态、不进调度/规则/失败统计。请求体
// 与 query 不携带 URL/host/路径——出站目标只能是 service 组装进来的已存储配置；
// 本包负责 origin 锁定（scheme + hostname + 规范化端口）、不跟随重定向与响应体
// 硬上限（2 MiB）；结果只含固定分类文案与有限公开错误码，不含任何上游自由文本
// （安全由分类白名单 + 有限错误码结构性保证，不做凭据子串替换）。
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/pkg/aiclient"
	"github.com/is7qin/c3api/pkg/egress"
)

// codexOrigin 是 codex SDK 固定官方端点 origin（spec §5.1：codex 固定
// https://chatgpt.com；base_url 不参与）。
const (
	codexOriginScheme = "https"
	codexOriginHost   = "chatgpt.com"
	codexOriginPort   = "443"
)

// Prober 是探测执行器：aiclient Factory（静态族原始请求）+ 出口池（账号出口）。
// 准入（4 执行 + 16 等待）与 30s 总期限由 service 拥有；本包只执行调用方
// 已经准入、带期限的 ctx。日志由 service 统一记一条（本包不写日志）。
type Prober struct {
	factory *aiclient.Factory
	pool    *egress.Pool
}

// New 构造探测执行器。nil factory 或 nil pool 不 panic——Probe 返回失败结论
// （装配错误在 service/main 显形）。
func New(factory *aiclient.Factory, pool *egress.Pool) *Prober {
	return &Prober{factory: factory, pool: pool}
}

// Probe 执行一次探测，直接使用调用方 ctx 的期限（不再自带超时/信号量）。
// 延迟只统计上游调用本身（准入之后）。
func (p *Prober) Probe(ctx context.Context, spec domain.ProbeSpec) domain.ProbeResult {
	if p == nil {
		return finalizeResult(spec, domain.ProbeResult{Error: domain.ProbeErrorRequestFailed})
	}
	if err := ctx.Err(); err != nil {
		return finalizeResult(spec, domain.ProbeResult{Error: classifyTransportError(err)})
	}

	start := time.Now()
	var res domain.ProbeResult
	codex := spec.CredentialType.IsCodex()
	switch {
	case codex:
		switch spec.Format {
		case domain.FormatOpenAIImages:
			res = p.probeCodexImages(ctx, spec)
		case domain.FormatOpenAISearch:
			res = p.probeCodexSearch(ctx, spec)
		default:
			res = p.probeCodex(ctx, spec)
		}
	case spec.CredentialType == credential.TypeAPIKey ||
		spec.CredentialType == credential.TypeResponsesSpecial ||
		spec.CredentialType == credential.TypeCustom:
		switch spec.Format {
		case domain.FormatOpenAIImages:
			res = p.probeStaticImages(ctx, spec)
		case domain.FormatOpenAISearch:
			res = p.probeStaticSearch(ctx, spec)
		default:
			res = p.probeStatic(ctx, spec)
		}
	default:
		res = domain.ProbeResult{Error: domain.ProbeErrorRequestFailed}
	}
	res.LatencyMS = time.Since(start).Milliseconds()
	return finalizeResult(spec, res)
}

// finalizeResult 统一回显字段 + 截断（所有出口都必须经过这里）。安全由分类
// 白名单 + 有限错误码结构性保证：error/detail 不含动态上游文本，因此不做凭据
// 子串替换——短 key（如 `up`）会破坏 `upstream rejected credentials` 等固定
// 文案。唯一的窄例外是 output_preview：先在抽取侧完成格式投影，这里统一按
// 已知 URL/Bearer/key/ID/UUID 形态脱敏，再按 64 KiB UTF-8 边界截断；
// image_previews 是校验后的 b64 图片，不经过文本脱敏器。
func finalizeResult(spec domain.ProbeSpec, res domain.ProbeResult) domain.ProbeResult {
	res.Format = spec.Format
	res.RequestedModel = spec.RequestedModel
	res.UpstreamModel = spec.UpstreamModel
	res.Error = truncateBytes(res.Error, domain.ProbeErrorMaxLen)
	res.Detail = truncateBytes(res.Detail, domain.ProbeDetailMaxLen)
	if res.OutputPreview != "" {
		res.OutputPreview = truncateBytes(sanitizePreview(res.OutputPreview), domain.ProbePreviewMaxLen)
	}
	return res
}

// probeStatic 静态 key 族（api_key / responses-special / custom）：aiclient
// Factory 的原始方法 + 账号出口 client（克隆后替换 Transport = cap∘guard）。
func (p *Prober) probeStatic(ctx context.Context, spec domain.ProbeSpec) domain.ProbeResult {
	if p == nil || p.factory == nil {
		return domain.ProbeResult{Error: domain.ProbeErrorRequestFailed}
	}
	key, ok := domain.UpstreamPathKeyForFormat(spec.Format)
	if !ok {
		return domain.ProbeResult{Error: domain.ProbeErrorRequestFailed}
	}
	path := domain.ResolveUpstreamPath(spec.CredentialType, spec.UpstreamPaths, key, domain.UpstreamDefaultTail(key))
	hc, err := p.guardedHTTPClient(spec)
	if err != nil {
		return domain.ProbeResult{Error: domain.ProbeErrorRequestFailed}
	}
	payload := staticPayload(spec.Format, spec.UpstreamModel, spec.Input)

	var (
		resp *http.Response
		call error
	)
	switch spec.Format {
	case domain.FormatOpenAIChat:
		resp, call = p.factory.ChatCompletionRaw(ctx, spec.TemplateID, spec.BaseURL, path, spec.Credential, payload, nil, hc)
	case domain.FormatOpenAIResponses, domain.FormatOpenAIResponsesWS:
		resp, call = p.factory.ResponseRaw(ctx, spec.TemplateID, spec.BaseURL, path, spec.Credential, payload, nil, hc)
	case domain.FormatAnthropic:
		resp, call = p.factory.AnthMessageRaw(ctx, spec.TemplateID, spec.BaseURL, path, spec.Credential, payload, nil, hc)
	default:
		return domain.ProbeResult{Error: domain.ProbeErrorRequestFailed}
	}
	if call != nil {
		return domain.ProbeResult{Error: classifyTransportError(call)}
	}

	res := domain.ProbeResult{StatusCode: resp.StatusCode}
	body, overflow, readErr := readLimitedBody(resp.Body, domain.ProbeMaxBodyBytes)
	_ = resp.Body.Close() // 恰好一次（body 归本函数）
	if readErr != nil {
		// 响应头已到达但 body 读取失败（含 ctx 超时/取消）：传输失败 = status 0。
		res.StatusCode = 0
		res.Error = classifyTransportError(readErr)
		return res
	}
	if overflow {
		res.Error = domain.ProbeErrorBodyTooLarge
		return res
	}

	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		if validateStatic(spec.Format, body) {
			res.Success = true
			// 文本四格式保持既有 2xx 成功语义，但 output_preview 仅在 HTTP 200
			// 且成功判定通过后抽取（spec §4 预览抽取）。
			if resp.StatusCode == http.StatusOK {
				res.OutputPreview = extractStaticPreview(spec.Format, body)
			}
			return res
		}
		res.Error = domain.ProbeErrorNoUsableOutput
		return res
	}

	res.Error = classifyUpstreamStatus(resp.StatusCode)
	res.Detail = errorCodeDetail(body)
	return res
}

// probeCodex codex 族（codex-pat / codex-oauth）：SDK HTTP arm（StreamBody），
// origin 固定 https://chatgpt.com；200 体由本包扫描器读取到终态/上限。不经过
// sdkbridge.Codex 条目/回调/轮转。准入/期限已在 service 完成。
func (p *Prober) probeCodex(ctx context.Context, spec domain.ProbeSpec) domain.ProbeResult {
	transport, err := p.guardedCodexTransport(spec)
	if err != nil {
		return domain.ProbeResult{Error: domain.ProbeErrorRequestFailed}
	}
	var scan codexBodyScan
	status, body, callErr := sdkbridge.StreamBodyOnce(
		ctx, spec.Credential, spec.CodexAccountID, transport,
		codexProbePayload(spec.UpstreamModel, spec.Input),
		func(rc io.ReadCloser) ([]byte, error) {
			scan = scanCodexBody(rc)
			return scan.body, scan.err
		},
	)
	// SDK 对非 200 的 body 读错误可能被吞掉并只返回 HTTP 状态；只要调用方
	// ctx 已取消/过期，传输失败优先于真实状态码，不能被 401/201 掩盖。
	if ctx.Err() != nil {
		return domain.ProbeResult{Error: classifyTransportError(ctx.Err())}
	}
	if callErr != nil {
		if status != 0 {
			// 防御性保留：seam 目前把所有 HTTP 响应都以 err=nil 返回；若未来
			// 返回带状态码的错误，仍按 HTTP 结论分类，不丢真实状态。
			return domain.ProbeResult{
				StatusCode: status,
				Error:      classifyUpstreamStatus(status),
				Detail:     errorCodeDetail(body),
			}
		}
		return domain.ProbeResult{Error: classifyTransportError(callErr)}
	}
	// nil error ⇒ HTTP 响应；仅 200 走 2 MiB 溢出判定与 SSE 终态结果。
	res := domain.ProbeResult{StatusCode: status}
	if status != http.StatusOK {
		res.Error = classifyUpstreamStatus(status)
		res.Detail = errorCodeDetail(body)
		return res
	}
	if scan.overflow {
		res.Error = domain.ProbeErrorBodyTooLarge
		return res
	}
	if scan.success {
		res.Success = true
		res.OutputPreview = scan.preview
		return res
	}
	res.Error = scan.errMsg
	res.Detail = scan.detail
	return res
}

// codexProbePayload 是 codex 探测的生产请求体：`input` 必须实际存在且非空
// （SDK 白名单保留 input；只写 model/stream 会发出 `"input":null`）。无输出
// token 上限——SDK 白名单会剔除 max_output_tokens 等非白名单键（spec §5.1）。
type codexProbePayloadBody struct {
	Model  string            `json:"model"`
	Input  []codexProbeInput `json:"input"`
	Stream bool              `json:"stream"`
}

type codexProbeInput struct {
	Role    string                `json:"role"`
	Content []codexProbeInputText `json:"content"`
}

type codexProbeInputText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func codexProbePayload(model, input string) []byte {
	b, err := json.Marshal(codexProbePayloadBody{
		Model: model,
		Input: []codexProbeInput{{
			Role:    "user",
			Content: []codexProbeInputText{{Type: "input_text", Text: input}},
		}},
		Stream: true,
	})
	if err != nil {
		return nil
	}
	return b
}

// guardedHTTPClient 克隆账号出口 client 并替换 Transport：
// capTransport(originGuard(出口 Transport))；静态族不跟随重定向（文本四格式
// 保持既有 2xx 允许 2 MiB+1 的溢出探测语义）。
func (p *Prober) guardedHTTPClient(spec domain.ProbeSpec) (*http.Client, error) {
	return p.guardedHTTPClientWith(spec, false)
}

// guardedHTTPClient200Only 是 images/search 的静态族 client：只有 200 允许
// max+1，其余状态（含 201/204/206）恰好 max（不判溢出）；max 由
// probeResponseLimits 按格式选择（images 8 MiB/2 MiB，search 2 MiB/2 MiB）。
func (p *Prober) guardedHTTPClient200Only(spec domain.ProbeSpec) (*http.Client, error) {
	return p.guardedHTTPClientWith(spec, true)
}

func (p *Prober) guardedHTTPClientWith(spec domain.ProbeSpec, onlyOKOverflow bool) (*http.Client, error) {
	if p.pool == nil {
		return nil, errors.New("egress pool not configured")
	}
	eg := p.pool.For(spec.Egress)
	if eg == nil || eg.HTTP == nil {
		return nil, errors.New("egress client not configured")
	}
	u, err := url.Parse(spec.BaseURL)
	if err != nil || u.Scheme == "" || u.Hostname() == "" {
		return nil, fmt.Errorf("invalid base_url %q", spec.BaseURL)
	}
	hc := *eg.HTTP
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	guard := &originGuard{
		next:   transportOrDefault(eg.HTTP.Transport),
		scheme: strings.ToLower(u.Scheme),
		host:   strings.ToLower(u.Hostname()),
		port:   effectivePort(u),
	}
	if onlyOKOverflow {
		okMax, errorMax := probeResponseLimits(spec)
		hc.Transport = newCappedTransportLimit(guard, okMax, errorMax, true)
	} else {
		hc.Transport = newCappedTransport(guard)
	}
	return &hc, nil
}

// probeResponseLimits 按格式选择上游响应体上限：images 的成功体可到 8 MiB
// （b64_json），但非 200 错误体仍严格 2 MiB；文本/SSE/search 两者均 2 MiB。
func probeResponseLimits(spec domain.ProbeSpec) (okMax, errorMax int64) {
	if spec.Format == domain.FormatOpenAIImages {
		return domain.ProbeMaxImageResponseBytes, domain.ProbeMaxBodyBytes
	}
	return domain.ProbeMaxBodyBytes, domain.ProbeMaxBodyBytes
}

// guardedCodexTransport 构造 codex 的传输守卫（origin 固定 chatgpt.com）。
func (p *Prober) guardedCodexTransport(spec domain.ProbeSpec) (http.RoundTripper, error) {
	if p.pool == nil {
		return nil, errors.New("egress pool not configured")
	}
	eg := p.pool.For(spec.Egress)
	if eg == nil || eg.HTTP == nil {
		return nil, errors.New("egress client not configured")
	}
	okMax, errorMax := probeResponseLimits(spec)
	return newCappedTransportLimit(&originGuard{
		next:   transportOrDefault(eg.HTTP.Transport),
		scheme: codexOriginScheme,
		host:   codexOriginHost,
		port:   codexOriginPort,
	}, okMax, errorMax, true), nil
}

func transportOrDefault(rt http.RoundTripper) http.RoundTripper {
	if rt == nil {
		return http.DefaultTransport
	}
	return rt
}

// errBlockedOrigin 是 originGuard 的拒绝哨兵：classifyTransportError 用
// errors.Is 识别（而不是匹配错误文本），普通传输失败不会因 host/URL 文本
// 含 "redirect" 被误分类。
var errBlockedOrigin = errors.New("probe blocked request")

// originGuard 只放行与目标 origin（scheme + hostname + 规范化端口）完全一致的
// 请求；其余立即失败（凭据不会被重定向/换端口带到第二个 origin）。
type originGuard struct {
	next   http.RoundTripper
	scheme string
	host   string
	port   string
}

func (g *originGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	u := req.URL
	if u == nil || !strings.EqualFold(u.Scheme, g.scheme) || !strings.EqualFold(u.Hostname(), g.host) || effectivePort(u) != g.port {
		target := "unknown"
		if u != nil {
			target = u.Scheme + "://" + u.Host
		}
		return nil, fmt.Errorf("%w to %s", errBlockedOrigin, target)
	}
	return g.next.RoundTrip(req)
}

// effectivePort 规范化端口：显式端口原样，缺省按 scheme 补 80/443。换端口/降级
// 协议都因此被判成不同 origin。
func effectivePort(u *url.URL) string {
	if u == nil {
		return ""
	}
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// newCappedTransport 给响应体加硬上限：非 2xx 截断到 2 MiB（SDK 内部对非 200 的
// io.ReadAll 也只能拿到 ≤2 MiB）；静态族 2xx 允许读到 2 MiB+1（多一个字节用于
// 发现溢出——调用方读到第 2 MiB+1 字节即判失败）。
//
// 私有测试 seam：生产装配只经 guardedHTTPClient 构造本类型。
func newCappedTransport(next http.RoundTripper) http.RoundTripper {
	return newCappedTransportLimit(next, domain.ProbeMaxBodyBytes, domain.ProbeMaxBodyBytes, false)
}

// newCappedTransportLimit 是 capTransport 的唯一构造：okMax 是成功响应
// （200，或 onlyOKOverflow=false 时的全部 2xx）上限，errorMax 是非成功响应
// 上限。成功体允许读取 okMax+1 用于发现溢出；错误体恰好 errorMax。
func newCappedTransportLimit(next http.RoundTripper, okMax, errorMax int64, onlyOKOverflow bool) http.RoundTripper {
	if okMax <= 0 {
		okMax = domain.ProbeMaxBodyBytes
	}
	if errorMax <= 0 {
		errorMax = domain.ProbeMaxBodyBytes
	}
	return &capTransport{next: next, okMax: okMax, errorMax: errorMax, onlyOKOverflow: onlyOKOverflow}
}

type capTransport struct {
	next http.RoundTripper
	// okMax 是成功响应上限；成功体额外放行 1 字节用于发现溢出。
	okMax int64
	// errorMax 是非成功响应上限；错误体严格截断到 errorMax。
	errorMax int64
	// onlyOKOverflow 只对 200 放行 okMax+1（codex/images 的非 200 必须严格 errorMax）。
	onlyOKOverflow bool
}

func (t *capTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.Body == nil {
		return resp, nil
	}
	limit := t.errorMax
	if resp.StatusCode == http.StatusOK ||
		(!t.onlyOKOverflow && resp.StatusCode >= 200 && resp.StatusCode <= 299) {
		limit = t.okMax
		limit++ // 检测溢出需要第 okMax+1 字节
	}
	resp.Body = &cappedBody{rc: resp.Body, remaining: limit}
	return resp, nil
}

type cappedBody struct {
	rc        io.ReadCloser
	remaining int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.rc.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func (b *cappedBody) Close() error { return b.rc.Close() }

// readLimitedBody 读取至多 max+1 字节；len > max = 溢出（body 内容不再使用）。
func readLimitedBody(r io.Reader, max int) ([]byte, bool, error) {
	body, err := io.ReadAll(io.LimitReader(r, int64(max)+1))
	if err != nil {
		return nil, false, err
	}
	if len(body) > max {
		return nil, true, nil
	}
	return body, false, nil
}

// ---- 请求载荷（spec §5.1 最小形态；字段顺序稳定） ----

type probeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type probeInputText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type probeInputContent struct {
	Role    string           `json:"role"`
	Content []probeInputText `json:"content"`
}

type chatProbePayload struct {
	Model     string         `json:"model"`
	Messages  []probeMessage `json:"messages"`
	MaxTokens int            `json:"max_tokens"`
	Stream    bool           `json:"stream"`
}

type responsesProbePayload struct {
	Model           string              `json:"model"`
	Input           []probeInputContent `json:"input"`
	MaxOutputTokens int                 `json:"max_output_tokens"`
	Stream          bool                `json:"stream"`
}

type anthProbePayload struct {
	Model     string         `json:"model"`
	MaxTokens int            `json:"max_tokens"`
	Messages  []probeMessage `json:"messages"`
}

func staticPayload(format domain.RequestFormat, model, input string) []byte {
	switch format {
	case domain.FormatOpenAIChat:
		b, _ := json.Marshal(chatProbePayload{
			Model:     model,
			Messages:  []probeMessage{{Role: "user", Content: input}},
			MaxTokens: 16,
		})
		return b
	case domain.FormatOpenAIResponses, domain.FormatOpenAIResponsesWS:
		b, _ := json.Marshal(responsesProbePayload{
			Model: model,
			Input: []probeInputContent{{
				Role:    "user",
				Content: []probeInputText{{Type: "input_text", Text: input}},
			}},
			MaxOutputTokens: 16,
		})
		return b
	case domain.FormatAnthropic:
		b, _ := json.Marshal(anthProbePayload{
			Model:     model,
			MaxTokens: 16,
			Messages:  []probeMessage{{Role: "user", Content: input}},
		})
		return b
	default:
		return nil
	}
}

// ---- 成功判定（spec §5.2；只查首元素路径，不物化上游数组） ----

func validateStatic(format domain.RequestFormat, body []byte) bool {
	// 深度守卫必须先于递归的 gjson 解析：超深上游 body 直接判不可用。
	if len(body) == 0 || !domain.ShallowJSON(body, domain.ProbeMaxJSONDepth) || !gjson.ValidBytes(body) {
		return false
	}
	r := gjson.ParseBytes(body)
	switch format {
	case domain.FormatOpenAIChat:
		// 父级必须是数组：`{"choices":{"0":...}}` 不是数组。
		choices := r.Get("choices")
		if !choices.IsArray() {
			return false
		}
		first := choices.Get("0")
		if !first.IsObject() {
			return false
		}
		content := first.Get("message.content")
		if content.Type == gjson.String && content.String() != "" {
			return true
		}
		if content.IsArray() {
			// 数组首元素必须是对象，或 chat content 允许的非空字符串；null/
			// 标量不算可用输出（spec §5.2 / A13）。
			item := content.Get("0")
			if item.IsObject() || (item.Type == gjson.String && item.String() != "") {
				return true
			}
		}
		toolCalls := first.Get("message.tool_calls")
		if toolCalls.IsArray() && toolCalls.Get("0").IsObject() {
			return true
		}
		reasoning := first.Get("message.reasoning_content")
		if reasoning.Type == gjson.String && reasoning.String() != "" {
			return true
		}
		finish := first.Get("finish_reason")
		return finish.Type == gjson.String && finish.String() != ""
	case domain.FormatOpenAIResponses, domain.FormatOpenAIResponsesWS:
		output := r.Get("output")
		if output.IsArray() {
			typ := output.Get("0.type")
			if typ.Type == gjson.String && typ.String() != "" {
				return true
			}
		}
		outputText := r.Get("output_text")
		return outputText.Type == gjson.String && outputText.String() != ""
	case domain.FormatAnthropic:
		content := r.Get("content")
		if !content.IsArray() {
			return false
		}
		typ := content.Get("0.type")
		return typ.Type == gjson.String && typ.String() != ""
	}
	return false
}

// ---- SSE 事件块解析（spec §5.1/§5.2） ----

// sseDone 是 SSE 的 `[DONE]` 哨兵（线上以 data 字段给出）。
var sseDone = []byte("[DONE]")

// sseLineEmpty 只把真正的空行（`\n` 或 `\r\n`）视为事件分隔；仅含空白的行
// 属于事件内容（spec §5.1/§5.2，A20）。
func sseLineEmpty(line []byte) bool {
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	return len(line) == 0
}

// codexBodyScan 是一次 200 body 增量扫描的结果。body 供调用方返回/截断，
// err 是读取错误（传输失败），overflow 表示超过 2 MiB（含 +1 探测字节），
// success/errMsg/detail 是首个终态事件的分类。
type codexBodyScan struct {
	body     []byte
	overflow bool
	success  bool
	errMsg   string
	detail   string
	preview  string
	err      error
}

// scanCodexBody 增量读取 200 body，在**真正空行事件边界**（或 EOF）判定终态：
// response.completed / response.failed / error 时立即返回；总量上限 2 MiB+1
// （到限即停，由调用方判溢出）。扫描用索引循环（bytes.IndexByte）而不是
// bytes.Split/ReadBytes：2 MiB 纯换行 body 不能产生百万级行头/行缓冲分配。
// 事件在 buf 内以 [eventStart, lineEnd) 表示，只在边界解析一次——既不会把多行
// 事件的首行误当完整事件截断，也避免逐行重解析的二次放大。
//
// body 的 Close 由 sdkbridge.StreamBodyOnce 负责；本函数只读。
func scanCodexBody(rc io.Reader) codexBodyScan {
	return scanCodexBodyLimit(rc, int64(domain.ProbeMaxBodyBytes)+1, true)
}

func scanCodexBodyLimit(rc io.Reader, limit int64, enforceMax bool) codexBodyScan {
	scan := codexBodyScan{errMsg: domain.ProbeErrorStreamFailed}
	var buf bytes.Buffer
	chunk := make([]byte, 32*1024)
	eventStart, lineStart, scanFrom := 0, 0, 0

	evaluate := func(end int) bool {
		terminal, success, errMsg, detail, preview := evaluateSSEBlock(buf.Bytes()[eventStart:end])
		if !terminal {
			return false
		}
		scan.body = buf.Bytes()[:end]
		if enforceMax {
			scan.overflow = len(scan.body) > domain.ProbeMaxBodyBytes
		}
		scan.success = success
		scan.errMsg = errMsg
		scan.detail = detail
		scan.preview = preview
		return true
	}
	for {
		room := limit - int64(buf.Len())
		if room <= 0 {
			break
		}
		readBuf := chunk
		if int64(len(readBuf)) > room {
			readBuf = readBuf[:room]
		}
		n, readErr := rc.Read(readBuf)
		if n > 0 {
			_, _ = buf.Write(readBuf[:n])
			data := buf.Bytes()
			// scanFrom 只在换行边界推进；未完成的尾行这里只扫新增字节，
			// 不会每块从 lineStart 重扫（2 MiB 单行 + 1 KiB 块不再是 O(n²)）。
			// lineStart 保留未完成行的起点，空白行判定不看被块边界切开的尾巴。
			for scanFrom < len(data) {
				idx := bytes.IndexByte(data[scanFrom:], '\n')
				if idx < 0 {
					scanFrom = len(data)
					break
				}
				lineEnd := scanFrom + idx + 1
				if sseLineEmpty(data[lineStart:lineEnd]) {
					// 空行 = 事件块结束；此时事件已完整收齐，可判定终态。
					if evaluate(lineEnd) {
						// 只返回到终态空行为止的字节：同一次 read 的预读尾巴
						// 不得把有效终态误判成 2 MiB 溢出。
						return scan
					}
					eventStart = lineEnd
				}
				lineStart = lineEnd
				scanFrom = lineEnd
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				scan.body = buf.Bytes()
				scan.err = readErr
				return scan
			}
			// EOF 也是事件边界（无尾随空行）。
			if evaluate(len(buf.Bytes())) {
				return scan
			}
			scan.body = buf.Bytes()
			if enforceMax {
				scan.overflow = len(scan.body) > domain.ProbeMaxBodyBytes
			}
			return scan
		}
	}
	scan.body = buf.Bytes()
	if enforceMax {
		scan.overflow = len(scan.body) > domain.ProbeMaxBodyBytes
	}
	return scan
}

// evaluateSSEBlock 解析一个完整 SSE 事件块并返回 (terminal, success, error,
// detail, preview)。失败事件名优先于 data 内容：`event: error` /
// `event: response.failed` 即使 data 为空、[DONE]、非 JSON 或自相矛盾
// （type=completed）也是失败，不得因为没有可解析的 data 而静默放过、让后续
// completed 覆盖。preview 只从 response.completed 的 data.response 抽取。
func evaluateSSEBlock(block []byte) (bool, bool, string, string, string) {
	var (
		name    string
		single  []byte
		hasData bool
		multi   bool
		joined  bytes.Buffer
	)
	for start := 0; start < len(block); {
		lineEnd := bytes.IndexByte(block[start:], '\n')
		var line []byte
		if lineEnd < 0 {
			line = block[start:]
			start = len(block)
		} else {
			line = block[start : start+lineEnd+1]
			start += lineEnd + 1
		}
		if sseLineEmpty(line) {
			continue
		}
		// 行首空白与终态检测使用同一规则：` data:` 也是 data 字段，不能被一边
		// 忽略、另一边当内容（否则同一事件在两边语义不同）。
		field := bytes.TrimSpace(line)
		switch {
		case bytes.HasPrefix(field, []byte("event:")):
			name = string(bytes.TrimSpace(field[len("event:"):]))
		case bytes.HasPrefix(field, []byte("data:")):
			chunk := bytes.TrimSpace(field[len("data:"):])
			if !hasData {
				single, hasData = chunk, true
				continue
			}
			if !multi {
				multi = true
				joined.Reset()
				joined.Write(single)
			}
			// 与 SSE 规范一致：多行 data 以 '\n' 合并（空行也参与分隔）。
			if joined.Len() > 0 {
				joined.WriteByte('\n')
			}
			joined.Write(chunk)
		}
	}
	data := single
	if multi {
		data = joined.Bytes()
	}
	if name == "response.failed" || name == "error" {
		detail := ""
		if len(data) > 0 && !bytes.Equal(data, sseDone) &&
			domain.ShallowJSON(data, domain.ProbeMaxJSONDepth) && gjson.ValidBytes(data) {
			detail = errorCodeDetail(data)
		}
		return true, false, domain.ProbeErrorStreamFailed, detail, ""
	}
	if len(data) == 0 || bytes.Equal(data, sseDone) {
		return false, false, "", "", ""
	}
	// 深层嵌套事件在 gjson 之前丢弃（线性深度守卫）。
	if !domain.ShallowJSON(data, domain.ProbeMaxJSONDepth) || !gjson.ValidBytes(data) {
		return false, false, "", "", ""
	}
	typ := gjson.GetBytes(data, "type").String()
	if typ == "" {
		typ = name
	}
	if typ == "response.failed" || typ == "error" {
		return true, false, domain.ProbeErrorStreamFailed, errorCodeDetail(data), ""
	}
	if typ == "response.completed" {
		response := gjson.GetBytes(data, "response")
		output := response.Get("output")
		if output.IsArray() && output.Get("0").IsObject() {
			return true, true, "", "", codexResponsePreview(response)
		}
		return true, false, domain.ProbeErrorNoUsableOutput, "", ""
	}
	return false, false, "", "", ""
}

// validateCodexSSE 是完整 body 校验入口，复用同一增量扫描器（不保留第二份
// 行/字段/终态解析）。仅测试与对已读 body 的诊断使用。
func validateCodexSSE(body []byte) (bool, string, string) {
	// 多留 1 字节让 scanner 看到 EOF；否则完整 body 恰好等于 limit 时会在
	// room<=0 退出，EOF 边界事件（无尾随空行）永远不会被评估。
	scan := scanCodexBodyLimit(bytes.NewReader(body), int64(len(body))+1, false)
	if scan.err != nil {
		return false, domain.ProbeErrorRequestFailed, ""
	}
	if scan.overflow {
		return false, domain.ProbeErrorBodyTooLarge, ""
	}
	if scan.success {
		return true, "", ""
	}
	return false, scan.errMsg, scan.detail
}

// ---- 失败分类（根 AGENTS.md 反模式 7：结果/日志不回显上游自由文本） ----

func classifyUpstreamStatus(status int) string {
	switch {
	case status >= 300 && status <= 399:
		return domain.ProbeErrorUpstreamRedirect
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return domain.ProbeErrorUpstreamAuth
	case status == http.StatusNotFound:
		return domain.ProbeErrorUpstreamNotFound
	case status == http.StatusTooManyRequests:
		return domain.ProbeErrorUpstreamRateLimit
	case status >= 400 && status <= 499:
		return domain.ProbeErrorUpstreamRejected
	default:
		return domain.ProbeErrorUpstream
	}
}

// probeErrorCodes 是唯一允许回显的公开错误码集合（spec §5.2 的有限表，lowercase
// snake）。上游可把别家 key / 内部请求 id 塞进 error.code/error.type，因此只认
// 表内命中值；表外值（含任意形态）一律丢弃，绝不做形态正则放行。
var probeErrorCodes = map[string]struct{}{
	"invalid_request_error":      {},
	"invalid_api_key":            {},
	"model_not_found":            {},
	"context_length_exceeded":    {},
	"rate_limit_exceeded":        {},
	"rate_limit_error":           {},
	"insufficient_quota":         {},
	"server_error":               {},
	"api_error":                  {},
	"overloaded_error":           {},
	"authentication_error":       {},
	"permission_error":           {},
	"not_found_error":            {},
	"request_too_large":          {},
	"content_policy_violation":   {},
	"usage_not_included":         {},
	"invalid_prompt":             {},
	"model_not_supported":        {},
	"unsupported_model":          {},
	"billing_hard_limit_reached": {},
	"account_deactivated":        {},
}

// errorCodeDetail 只从上游错误对象提取**内置有限公开表**内的 error.code/
// error.type，以 code=<value> 返回；message/原始 JSON/其他文本/表外 code 一律
// 丢弃。
func errorCodeDetail(body []byte) string {
	if len(body) == 0 || !domain.ShallowJSON(body, domain.ProbeMaxJSONDepth) || !gjson.ValidBytes(body) {
		return ""
	}
	for _, path := range []string{"error.code", "error.type", "response.error.code", "response.error.type"} {
		v := gjson.GetBytes(body, path)
		if v.Type != gjson.String {
			continue
		}
		code := v.String()
		if _, ok := probeErrorCodes[code]; ok {
			return "code=" + code
		}
	}
	return ""
}

// isTimeoutError 覆盖 ctx deadline、客户端超时与 net.Error.Timeout。
func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// isMalformedLocationError 识别 Go http.Client 解析重定向 Location 失败时返回
// 的特定错误：*url.Error 包裹 "failed to parse Location header"。只有这一种
// redirect 解析错误归为 cross-origin 阻止；host/URL 文本里出现 "redirect" 的
// 普通传输失败不得误分类。
func isMalformedLocationError(err error) bool {
	var uerr *url.Error
	if !errors.As(err, &uerr) || uerr.Err == nil {
		return false
	}
	return strings.HasPrefix(uerr.Err.Error(), "failed to parse Location header")
}

// classifyTransportError 把传输层错误归一为固定文案；originGuard 拒绝与
// malformed Location 解析失败记 cross-origin 阻止，绝不回显 URL/Location/
// 底层错误文本。
func classifyTransportError(err error) string {
	if err == nil {
		return domain.ProbeErrorRequestFailed
	}
	if isTimeoutError(err) {
		return domain.ProbeErrorTimedOut
	}
	if errors.Is(err, context.Canceled) {
		return domain.ProbeErrorCanceled
	}
	if errors.Is(err, errBlockedOrigin) || isMalformedLocationError(err) {
		return domain.ProbeErrorBlockedRedirect
	}
	return domain.ProbeErrorRequestFailed
}

// ---- 截断（spec §5.2/§6） ----

func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
