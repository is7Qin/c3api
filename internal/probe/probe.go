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
	"github.com/is7qin/c3api/pkg/logx"
)

// codexOrigin 是 codex SDK 固定官方端点 origin（spec §5.1：codex 固定
// https://chatgpt.com；base_url 不参与）。
const (
	codexOriginScheme = "https"
	codexOriginHost   = "chatgpt.com"
	codexOriginPort   = "443"
)

// DefaultTimeout / DefaultMaxConcurrency 是生产装配使用的硬上限（30s / 4）。
// 这里是构造默认值；spec 明确不开配置键。
const (
	DefaultTimeout        = domain.ProbeTimeout
	DefaultMaxConcurrency = domain.ProbeMaxConcurrency
)

// Prober 是探测执行器：aiclient Factory（静态族原始请求）+ 出口池（账号出口）
// + 进程内并发信号量。日志由 service 统一记一条（本包不写日志，保证 §7「一条」）。
type Prober struct {
	factory *aiclient.Factory
	pool    *egress.Pool
	log     *logx.Logger
	timeout time.Duration
	sem     chan struct{}
}

// New 构造探测执行器。timeout/concurrency <= 0 回落硬上限（30s/4）。nil factory
// 或 nil pool 不 panic——Probe 返回失败结论（装配错误在 service/main 显形）。
func New(factory *aiclient.Factory, pool *egress.Pool, log *logx.Logger, timeout time.Duration, concurrency int) *Prober {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if concurrency <= 0 {
		concurrency = DefaultMaxConcurrency
	}
	return &Prober{
		factory: factory,
		pool:    pool,
		log:     log,
		timeout: timeout,
		sem:     make(chan struct{}, concurrency),
	}
}

// Probe 执行一次探测（硬上限 = 构造时的 timeout，通常 30s）。并发信号量等待
// 受同一 ctx 约束；延迟只统计上游调用本身（拿到信号量之后）。
func (p *Prober) Probe(ctx context.Context, spec domain.ProbeSpec) domain.ProbeResult {
	if p == nil {
		return finalizeResult(spec, domain.ProbeResult{Error: "probe request failed"})
	}
	pctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-pctx.Done():
		return finalizeResult(spec, domain.ProbeResult{
			Error: classifyTransportError(pctx.Err()),
		})
	}

	start := time.Now()
	var res domain.ProbeResult
	switch {
	case spec.CredentialType.IsCodex():
		res = p.probeCodex(pctx, spec)
	case spec.CredentialType == credential.TypeAPIKey ||
		spec.CredentialType == credential.TypeResponsesSpecial ||
		spec.CredentialType == credential.TypeCustom:
		res = p.probeStatic(pctx, spec)
	default:
		res = domain.ProbeResult{Error: "probe request failed"}
	}
	res.LatencyMS = time.Since(start).Milliseconds()
	return finalizeResult(spec, res)
}

// finalizeResult 统一回显字段 + 截断（所有出口都必须经过这里）。安全由分类
// 白名单 + 有限错误码结构性保证：结果字段不含动态上游文本，因此不做凭据子串
// 替换——短 key（如 `up`）会破坏 `upstream rejected credentials` 等固定文案。
func finalizeResult(spec domain.ProbeSpec, res domain.ProbeResult) domain.ProbeResult {
	res.Format = spec.Format
	res.RequestedModel = spec.RequestedModel
	res.UpstreamModel = spec.UpstreamModel
	res.Error = truncateBytes(res.Error, domain.ProbeErrorMaxLen)
	res.Detail = truncateBytes(res.Detail, domain.ProbeDetailMaxLen)
	return res
}

// probeStatic 静态 key 族（api_key / responses-special / custom）：aiclient
// Factory 的原始方法 + 账号出口 client（克隆后替换 Transport = cap∘guard）。
func (p *Prober) probeStatic(ctx context.Context, spec domain.ProbeSpec) domain.ProbeResult {
	if p == nil || p.factory == nil {
		return domain.ProbeResult{Error: "probe request failed"}
	}
	key, ok := domain.UpstreamPathKeyForFormat(spec.Format)
	if !ok {
		return domain.ProbeResult{Error: "probe request failed"}
	}
	path := domain.ResolveUpstreamPath(spec.CredentialType, spec.UpstreamPaths, key, domain.UpstreamDefaultTail(key))
	hc, err := p.guardedHTTPClient(spec)
	if err != nil {
		return domain.ProbeResult{Error: "probe request failed"}
	}
	payload := staticPayload(spec.Format, spec.UpstreamModel)

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
		return domain.ProbeResult{Error: "probe request failed"}
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
		res.Error = "response exceeds 2 MiB"
		return res
	}

	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		if validateStatic(spec.Format, body) {
			res.Success = true
			return res
		}
		res.Error = "upstream response missing usable output"
		return res
	}

	res.Error = classifyUpstreamStatus(resp.StatusCode)
	res.Detail = errorCodeDetail(body)
	return res
}

// probeCodex codex 族（codex-pat / codex-oauth）：SDK HTTP arm（StreamBody），
// origin 固定 https://chatgpt.com，200 体由 sdkbridge 读取到 2 MiB+1 字节后由
// 本函数判溢出。不经过 sdkbridge.Codex 条目/回调/轮转。
func (p *Prober) probeCodex(ctx context.Context, spec domain.ProbeSpec) domain.ProbeResult {
	transport, err := p.guardedCodexTransport(spec)
	if err != nil {
		return domain.ProbeResult{Error: "probe request failed"}
	}
	payload := sdkbridge.ProbeResponsesPayload(spec.UpstreamModel)
	status, body, callErr := sdkbridge.ProbeResponses(ctx, spec.Credential, spec.CodexAccountID, transport, payload, p.timeout)
	if callErr != nil {
		// ctx 超时/取消优先于 SDK 吞掉的非 200 body 读错误：SDK 对非 200 的
		// io.ReadAll 错误与 401 预读错误都不透传，只留 HTTPError；响应头已到
		// 但 body 未读完时按传输失败处理（status 0）。
		if ctx.Err() != nil {
			return domain.ProbeResult{Error: classifyTransportError(ctx.Err())}
		}
		if status != 0 {
			// SDK 对任何非 200（含 201/204/3xx）都返回 HTTPError：这是 HTTP
			// 响应，原样保留真实状态码并按 HTTP 响应分类，不得当成 body 读失败
			// 或 200 溢出。
			return domain.ProbeResult{
				StatusCode: status,
				Error:      classifyUpstreamStatus(status),
				Detail:     errorCodeDetail(body),
			}
		}
		return domain.ProbeResult{Error: classifyTransportError(callErr)}
	}
	// nil error ⇒ SDK 只对 200 放行；仅 200 走 2 MiB 溢出判定与 SSE 终态解析。
	res := domain.ProbeResult{StatusCode: status}
	if status != http.StatusOK {
		res.Error = classifyUpstreamStatus(status)
		return res
	}
	if len(body) > domain.ProbeMaxBodyBytes {
		res.Error = "response exceeds 2 MiB"
		return res
	}
	success, errMsg, detail := validateCodexSSE(body)
	if success {
		res.Success = true
		return res
	}
	res.Error = errMsg
	res.Detail = detail
	return res
}

// guardedHTTPClient 克隆账号出口 client 并替换 Transport：
// capTransport(originGuard(出口 Transport))；静态族不跟随重定向。
func (p *Prober) guardedHTTPClient(spec domain.ProbeSpec) (*http.Client, error) {
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
	hc.Transport = NewCappedTransport(&originGuard{
		next:   transportOrDefault(eg.HTTP.Transport),
		scheme: strings.ToLower(u.Scheme),
		host:   strings.ToLower(u.Hostname()),
		port:   effectivePort(u),
	})
	return &hc, nil
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
	return NewCodexCappedTransport(&originGuard{
		next:   transportOrDefault(eg.HTTP.Transport),
		scheme: codexOriginScheme,
		host:   codexOriginHost,
		port:   codexOriginPort,
	}), nil
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

// capTransport 给响应体加硬上限：非 2xx 截断到 2 MiB（SDK 内部对非 200 的
// io.ReadAll 也只能拿到 ≤2 MiB）；静态族 2xx 允许读到 2 MiB+1（多一个字节用于
// 发现溢出——调用方读到第 2 MiB+1 字节即判失败）。
//
// NewCappedTransport 是测试 seam：导出使 sdkbridge 的外部测试能经
// sdkbridge.ProbeResponses 复用生产 capping（A5/A17 证据）；生产装配只经
// guardedHTTPClient 构造本类型。
func NewCappedTransport(next http.RoundTripper) http.RoundTripper {
	return &capTransport{next: next}
}

// NewCodexCappedTransport 是 codex 专用的测试 seam：只有 200 允许 2 MiB+1
// （200 溢出探测），其余状态（含 201/204/206/3xx/4xx/5xx）恰好 2 MiB，不享受
// +1 字节。生产装配只经 guardedCodexTransport 构造本类型。
func NewCodexCappedTransport(next http.RoundTripper) http.RoundTripper {
	return &capTransport{next: next, onlyOKOverflow: true}
}

type capTransport struct {
	next http.RoundTripper
	// onlyOKOverflow 只对 200 放行 2 MiB+1（codex 的非 200 必须严格 2 MiB）。
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
	limit := int64(domain.ProbeMaxBodyBytes)
	if resp.StatusCode == http.StatusOK ||
		(!t.onlyOKOverflow && resp.StatusCode >= 200 && resp.StatusCode <= 299) {
		limit++ // 检测溢出需要第 2 MiB+1 字节
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

func staticPayload(format domain.RequestFormat, model string) []byte {
	switch format {
	case domain.FormatOpenAIChat:
		b, _ := json.Marshal(chatProbePayload{
			Model:     model,
			Messages:  []probeMessage{{Role: "user", Content: "hi"}},
			MaxTokens: 16,
		})
		return b
	case domain.FormatOpenAIResponses, domain.FormatOpenAIResponsesWS:
		b, _ := json.Marshal(responsesProbePayload{
			Model: model,
			Input: []probeInputContent{{
				Role:    "user",
				Content: []probeInputText{{Type: "input_text", Text: "hi"}},
			}},
			MaxOutputTokens: 16,
		})
		return b
	case domain.FormatAnthropic:
		b, _ := json.Marshal(anthProbePayload{
			Model:     model,
			MaxTokens: 16,
			Messages:  []probeMessage{{Role: "user", Content: "hi"}},
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

// validateCodexSSE 单遍扫描 SSE 终态：response.completed 且 response.output
// 首元素为对象 = 成功；response.failed / error（事件名或 data.type，失败信号
// 优先于 data.type 的 response.completed）事件 = 失败（固定文案 + 白名单 code）。
//
// 扫描按索引推进，单行 data 直接引用 body 切片，只有多行 data 才复制到复用的
// 缓冲区：2 MiB 的 `data:0\n\n` 微事件洪泛分配 O(body)，不是 O(events)。字段
// 解析（含行首空白）与 sdkbridge.codexTerminalBlock 逐字一致。
func validateCodexSSE(body []byte) (bool, string, string) {
	var (
		name    string
		single  []byte
		hasData bool
		multi   bool
		joined  bytes.Buffer
	)
	reset := func() {
		name, single, hasData, multi = "", nil, false, false
		joined.Reset()
	}
	eventData := func() []byte {
		if !hasData {
			return nil
		}
		if multi {
			return joined.Bytes()
		}
		return single
	}
	addData := func(chunk []byte) {
		if !hasData {
			single, hasData = chunk, true
			return
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
	// evaluate 返回 (terminal, success, error, detail)；非终态事件返回 false。
	evaluate := func() (bool, bool, string, string) {
		data := eventData()
		// 失败事件名优先于 data 内容：`event: error` / `event: response.failed`
		// 即使 data 为空、[DONE]、非 JSON 或自相矛盾（type=completed）也是失败，
		// 不得因为没有可解析的 data 而静默放过、让后续 completed 覆盖。
		if name == "response.failed" || name == "error" {
			detail := ""
			if len(data) > 0 && !bytes.Equal(data, sseDone) &&
				domain.ShallowJSON(data, domain.ProbeMaxJSONDepth) && gjson.ValidBytes(data) {
				detail = errorCodeDetail(data)
			}
			return true, false, "upstream stream failed", detail
		}
		if len(data) == 0 || bytes.Equal(data, sseDone) {
			return false, false, "", ""
		}
		// 深层嵌套事件在 gjson 之前丢弃（线性深度守卫）。
		if !domain.ShallowJSON(data, domain.ProbeMaxJSONDepth) || !gjson.ValidBytes(data) {
			return false, false, "", ""
		}
		typ := gjson.GetBytes(data, "type").String()
		if typ == "" {
			typ = name
		}
		// data.type 失败同样是失败（事件名失败已在上面优先处理）。
		if typ == "response.failed" || typ == "error" {
			return true, false, "upstream stream failed", errorCodeDetail(data)
		}
		if typ == "response.completed" {
			output := gjson.GetBytes(data, "response.output")
			if output.IsArray() && output.Get("0").IsObject() {
				return true, true, "", ""
			}
			return true, false, "upstream response missing usable output", ""
		}
		return false, false, "", ""
	}
	for start := 0; start <= len(body); {
		if start == len(body) {
			// EOF 也是事件边界（无尾随空行），与 readProbeBody 一致。
			if terminal, success, errMsg, detail := evaluate(); terminal {
				return success, errMsg, detail
			}
			break
		}
		lineEnd := bytes.IndexByte(body[start:], '\n')
		var line []byte
		if lineEnd < 0 {
			line = body[start:]
			start = len(body)
		} else {
			line = body[start : start+lineEnd+1]
			start += lineEnd + 1
		}
		if sseLineEmpty(line) {
			if terminal, success, errMsg, detail := evaluate(); terminal {
				return success, errMsg, detail
			}
			reset()
			continue
		}
		// 行首空白与终态检测使用同一规则：` data:` 也是 data 字段，不能被一边
		// 忽略、另一边当内容（否则同一事件在两边语义不同）。
		field := bytes.TrimSpace(line)
		switch {
		case bytes.HasPrefix(field, []byte("event:")):
			name = string(bytes.TrimSpace(field[len("event:"):]))
		case bytes.HasPrefix(field, []byte("data:")):
			addData(bytes.TrimSpace(field[len("data:"):]))
		}
	}
	return false, "upstream stream failed", ""
}

// ---- 失败分类（根 AGENTS.md 反模式 7：结果/日志不回显上游自由文本） ----

func classifyUpstreamStatus(status int) string {
	switch {
	case status >= 300 && status <= 399:
		return "upstream redirect rejected"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "upstream rejected credentials"
	case status == http.StatusNotFound:
		return "upstream endpoint or model not found"
	case status == http.StatusTooManyRequests:
		return "upstream rate limited"
	case status >= 400 && status <= 499:
		return "upstream rejected request"
	default:
		return "upstream error"
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
		return "probe request failed"
	}
	if isTimeoutError(err) {
		return "probe timed out"
	}
	if errors.Is(err, context.Canceled) {
		return "probe canceled"
	}
	if errors.Is(err, errBlockedOrigin) || isMalformedLocationError(err) {
		return "probe blocked cross-origin redirect"
	}
	return "probe request failed"
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
