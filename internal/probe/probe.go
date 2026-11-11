// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package probe 执行 account-model-test 的单账号单模型探测：用账号自身的凭据、
// 出口与模板规则向上游发一条最小请求，返回「模型是否可用 + 延迟 + 失败原因」。
//
// 只读诊断观测：不落库、不计费、不改账号状态、不进调度/规则/失败统计。请求体
// 与 query 不携带 URL/host/路径——出站目标只能是 service 组装进来的已存储配置；
// 本包负责 origin 锁定（scheme + hostname + 规范化端口）、不跟随重定向与响应体
// 硬上限（2 MiB），并把凭据/绑定代理密码从结果文本中脱敏。
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
		return finalizeResult(spec, domain.ProbeResult{Error: "probe not configured"})
	}
	pctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-pctx.Done():
		return finalizeResult(spec, domain.ProbeResult{
			Error: "probe canceled before start: " + pctx.Err().Error(),
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
		res = domain.ProbeResult{Error: "unsupported credential type for probe"}
	}
	res.LatencyMS = time.Since(start).Milliseconds()
	return finalizeResult(spec, res)
}

// finalizeResult 统一回显字段 + 脱敏 + 截断（所有出口都必须经过这里）。
func finalizeResult(spec domain.ProbeSpec, res domain.ProbeResult) domain.ProbeResult {
	res.Format = spec.Format
	res.RequestedModel = spec.RequestedModel
	res.UpstreamModel = spec.UpstreamModel
	secrets := []string{spec.Credential, spec.ProxyPassword}
	res.Error = truncateBytes(redactText(res.Error, secrets), domain.ProbeErrorMaxLen)
	res.Detail = truncateBytes(redactText(res.Detail, secrets), domain.ProbeDetailMaxLen)
	return res
}

// probeStatic 静态 key 族（api_key / responses-special / custom）：aiclient
// Factory 的原始方法 + 账号出口 client（克隆后替换 Transport = cap∘guard）。
func (p *Prober) probeStatic(ctx context.Context, spec domain.ProbeSpec) domain.ProbeResult {
	if p == nil || p.factory == nil {
		return domain.ProbeResult{Error: "probe factory not configured"}
	}
	key, ok := domain.UpstreamPathKeyForFormat(spec.Format)
	if !ok {
		return domain.ProbeResult{Error: "unsupported probe format"}
	}
	path := domain.ResolveUpstreamPath(spec.CredentialType, spec.UpstreamPaths, key, domain.UpstreamDefaultTail(key))
	hc, err := p.guardedHTTPClient(spec)
	if err != nil {
		return domain.ProbeResult{Error: "probe client: " + err.Error()}
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
		return domain.ProbeResult{Error: "unsupported probe format"}
	}
	if call != nil {
		return domain.ProbeResult{Error: "probe request failed: " + call.Error()}
	}

	res := domain.ProbeResult{StatusCode: resp.StatusCode}
	body, overflow, readErr := readLimitedBody(resp.Body, domain.ProbeMaxBodyBytes)
	_ = resp.Body.Close() // 恰好一次（body 归本函数）
	if readErr != nil {
		res.Error = "probe response read failed: " + readErr.Error()
		return res
	}
	if overflow {
		res.Error = "response exceeds 2 MiB"
		res.Detail = "response body exceeds 2 MiB; connection closed"
		return res
	}

	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		if validateStatic(spec.Format, body) {
			res.Success = true
			return res
		}
		errMsg, detail := extractErrorText(body)
		if errMsg == "" {
			errMsg = "upstream returned no usable output"
		}
		res.Error = errMsg
		res.Detail = detail
		return res
	}

	errMsg, detail := extractErrorText(body)
	if errMsg == "" {
		errMsg = upstreamStatusText(resp.StatusCode)
	}
	res.Error = errMsg
	res.Detail = detail
	return res
}

// probeCodex codex 族（codex-pat / codex-oauth）：SDK HTTP arm（StreamBody），
// origin 固定 https://chatgpt.com，200 体由 sdkbridge 读取到 2 MiB+1 字节后由
// 本函数判溢出。不经过 sdkbridge.Codex 条目/回调/轮转。
func (p *Prober) probeCodex(ctx context.Context, spec domain.ProbeSpec) domain.ProbeResult {
	transport, err := p.guardedCodexTransport(spec)
	if err != nil {
		return domain.ProbeResult{Error: "probe client: " + err.Error()}
	}
	payload, err := json.Marshal(codexProbePayload{Model: spec.UpstreamModel, Stream: true})
	if err != nil {
		return domain.ProbeResult{Error: "probe payload: " + err.Error()}
	}
	status, body, callErr := sdkbridge.ProbeResponses(ctx, spec.Credential, spec.CodexAccountID, transport, payload, p.timeout)
	res := domain.ProbeResult{StatusCode: status}
	if len(body) > domain.ProbeMaxBodyBytes {
		res.Error = "response exceeds 2 MiB"
		res.Detail = "response body exceeds 2 MiB; connection closed"
		return res
	}
	if callErr != nil && status == 0 {
		res.Error = "probe request failed: " + callErr.Error()
		return res
	}
	if status < 200 || status > 299 {
		errMsg, detail := extractErrorText(body)
		if errMsg == "" && callErr != nil {
			errMsg = callErr.Error()
		}
		if errMsg == "" {
			errMsg = upstreamStatusText(status)
		}
		res.Error = errMsg
		res.Detail = detail
		return res
	}
	if callErr != nil {
		res.Error = "probe response read failed: " + callErr.Error()
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
	hc.Transport = &capTransport{next: &originGuard{
		next:   transportOrDefault(eg.HTTP.Transport),
		scheme: strings.ToLower(u.Scheme),
		host:   strings.ToLower(u.Hostname()),
		port:   effectivePort(u),
	}}
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
	return &capTransport{next: &originGuard{
		next:   transportOrDefault(eg.HTTP.Transport),
		scheme: codexOriginScheme,
		host:   codexOriginHost,
		port:   codexOriginPort,
	}}, nil
}

func transportOrDefault(rt http.RoundTripper) http.RoundTripper {
	if rt == nil {
		return http.DefaultTransport
	}
	return rt
}

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
		return nil, fmt.Errorf("probe blocked request to %s", target)
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
// io.ReadAll 也只能拿到 ≤2 MiB）；2xx 允许读到 2 MiB+1（多一个字节用于发现
// 溢出——调用方读到第 2 MiB+1 字节即判失败）。
type capTransport struct {
	next http.RoundTripper
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
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
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

// codexProbePayload 无输出 token 上限：SDK 白名单会剔除 max_output_tokens 等
// 非白名单键，codex 探测不假定输出上限（spec §5.1）。
type codexProbePayload struct {
	Model  string              `json:"model"`
	Input  []probeInputContent `json:"input"`
	Stream bool                `json:"stream"`
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

// ---- 成功判定（spec §5.2） ----

func validateStatic(format domain.RequestFormat, body []byte) bool {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return false
	}
	r := gjson.ParseBytes(body)
	switch format {
	case domain.FormatOpenAIChat:
		choices := r.Get("choices")
		if !choices.IsArray() || len(choices.Array()) == 0 {
			return false
		}
		first := choices.Array()[0]
		if !first.IsObject() {
			return false
		}
		if first.Get("message.content").String() != "" {
			return true
		}
		if tc := first.Get("message.tool_calls"); tc.IsArray() && len(tc.Array()) > 0 {
			return true
		}
		if first.Get("message.reasoning_content").String() != "" {
			return true
		}
		return first.Get("finish_reason").String() != ""
	case domain.FormatOpenAIResponses, domain.FormatOpenAIResponsesWS:
		if out := r.Get("output"); out.IsArray() && len(out.Array()) > 0 && out.Array()[0].Get("type").String() != "" {
			return true
		}
		return r.Get("output_text").String() != ""
	case domain.FormatAnthropic:
		content := r.Get("content")
		return content.IsArray() && len(content.Array()) > 0 && content.Array()[0].Get("type").String() != ""
	}
	return false
}

// validateCodexSSE 扫描 SSE 终态：response.completed 且 response.output 数组
// 长度 ≥1 = 成功；response.failed / error 事件 = 失败。
func validateCodexSSE(body []byte) (bool, string, string) {
	lastType := ""
	eventName := ""
	for _, rawLine := range bytes.Split(body, []byte("\n")) {
		line := bytes.TrimSpace(rawLine)
		if bytes.HasPrefix(line, []byte("event:")) {
			eventName = string(bytes.TrimSpace(line[len("event:"):]))
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		name := eventName
		eventName = ""
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) || !gjson.ValidBytes(data) {
			continue
		}
		typ := gjson.GetBytes(data, "type").String()
		if typ == "" {
			typ = name
		}
		if typ != "" {
			lastType = typ
		}
		switch typ {
		case "response.completed":
			out := gjson.GetBytes(data, "response.output")
			if out.IsArray() && len(out.Array()) > 0 {
				return true, "", ""
			}
			return false, "upstream response completed without output", "response.completed output is empty"
		case "response.failed":
			errMsg, detail := codexEventError(data)
			if errMsg == "" {
				errMsg = "upstream response failed"
			}
			if detail == "" {
				detail = "response.failed"
			}
			return false, errMsg, detail
		case "error":
			errMsg, detail := codexEventError(data)
			if errMsg == "" {
				errMsg = "upstream returned an error event"
			}
			if detail == "" {
				detail = "error"
			}
			return false, errMsg, detail
		}
	}
	if lastType != "" {
		return false, "upstream stream ended before response.completed", "last event: " + lastType
	}
	return false, "upstream stream ended without a terminal event", ""
}

// ---- 错误提取（解码后的文本，不放未解码的原始 JSON 字节） ----

func extractErrorText(body []byte) (string, string) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return "", ""
	}
	r := gjson.ParseBytes(body)
	if !r.IsObject() {
		return "", ""
	}
	if m := r.Get("error.message"); m.Exists() && m.String() != "" {
		return m.String(), m.String()
	}
	e := r.Get("error")
	if !e.Exists() || e.Type == gjson.Null {
		return "", ""
	}
	if e.Type == gjson.String {
		s := e.String()
		return s, s
	}
	raw := compactJSON(e.Raw)
	if m := e.Get("message"); m.Exists() && m.String() != "" {
		return m.String(), raw
	}
	if raw != "" {
		return raw, raw
	}
	return "", ""
}

func codexEventError(data []byte) (string, string) {
	for _, path := range []string{"response.error.message", "error.message", "message"} {
		if m := gjson.GetBytes(data, path); m.Exists() && m.String() != "" {
			return m.String(), m.String()
		}
	}
	for _, path := range []string{"response.error", "error"} {
		e := gjson.GetBytes(data, path)
		if !e.Exists() || e.Type == gjson.Null {
			continue
		}
		if e.Type == gjson.String {
			return e.String(), e.String()
		}
		raw := compactJSON(e.Raw)
		if raw != "" {
			return "upstream response failed", raw
		}
	}
	return "", ""
}

func compactJSON(raw string) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(raw)); err != nil {
		return ""
	}
	return buf.String()
}

func upstreamStatusText(status int) string {
	if t := http.StatusText(status); t != "" {
		return t
	}
	return fmt.Sprintf("HTTP %d", status)
}

// ---- 脱敏 / 截断（spec §5.2/§6） ----

// redactText 把每个 secret 的原文、JSON 转义形式、URL 编码形式（QueryEscape 与
// PathEscape）一律替换为 ***。secret 为空跳过。
func redactText(s string, secrets []string) string {
	if s == "" {
		return s
	}
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		for _, form := range []string{secret, jsonEscape(secret), url.QueryEscape(secret), url.PathEscape(secret)} {
			if form == "" {
				continue
			}
			s = strings.ReplaceAll(s, form, "***")
		}
	}
	return s
}

func jsonEscape(s string) string {
	b, err := json.Marshal(s)
	if err != nil || len(b) < 2 {
		return s
	}
	return string(b[1 : len(b)-1])
}

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
