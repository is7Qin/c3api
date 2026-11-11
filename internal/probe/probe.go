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
	"net"
	"net/http"
	"net/url"
	"regexp"
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
	res := domain.ProbeResult{StatusCode: status}
	if len(body) > domain.ProbeMaxBodyBytes {
		res.Error = "response exceeds 2 MiB"
		return res
	}
	if callErr != nil {
		if isContextError(callErr) {
			// 响应头可能已到达，但 body 读取被超时/取消截断：传输失败 = status 0。
			res.StatusCode = 0
			res.Error = classifyTransportError(callErr)
			return res
		}
		if status == 0 {
			res.Error = classifyTransportError(callErr)
			return res
		}
		if status < 200 || status > 299 {
			res.Error = classifyUpstreamStatus(status)
			res.Detail = errorCodeDetail(body)
			return res
		}
		// 2xx 但 body 读取失败：传输失败。
		res.StatusCode = 0
		res.Error = "probe request failed"
		return res
	}
	if status < 200 || status > 299 {
		res.Error = classifyUpstreamStatus(status)
		res.Detail = errorCodeDetail(body)
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
	return NewCappedTransport(&originGuard{
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
//
// NewCappedTransport 导出生产实现，供 sdkbridge 的 cap 证据测试（A5/A17）经
// sdkbridge.ProbeResponses 复用同一层，避免测试自带一份 capping transport。
func NewCappedTransport(next http.RoundTripper) http.RoundTripper {
	return &capTransport{next: next}
}

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
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return false
	}
	r := gjson.ParseBytes(body)
	switch format {
	case domain.FormatOpenAIChat:
		first := r.Get("choices.0")
		if !first.IsObject() {
			return false
		}
		content := first.Get("message.content")
		if content.Type == gjson.String && content.String() != "" {
			return true
		}
		if content.IsArray() && content.Get("0").Exists() {
			return true
		}
		if first.Get("message.tool_calls.0").Exists() {
			return true
		}
		if first.Get("message.reasoning_content").String() != "" {
			return true
		}
		return first.Get("finish_reason").String() != ""
	case domain.FormatOpenAIResponses, domain.FormatOpenAIResponsesWS:
		if r.Get("output.0.type").String() != "" {
			return true
		}
		return r.Get("output_text").String() != ""
	case domain.FormatAnthropic:
		return r.Get("content.0.type").String() != ""
	}
	return false
}

// ---- SSE 事件块解析（spec §5.1/§5.2） ----

type sseEvent struct {
	name string
	data []byte
}

// parseSSEEvents 按空行分块；同一事件的多行 data 用 "\n" 合并（SSE 规范），
// 因此被拆成多行的 response.completed 仍能还原成完整 JSON。
func parseSSEEvents(body []byte) []sseEvent {
	events := make([]sseEvent, 0, 4)
	var (
		name string
		data []byte
	)
	flush := func() {
		if len(data) > 0 {
			events = append(events, sseEvent{name: name, data: data})
		}
		name, data = "", nil
	}
	for _, rawLine := range bytes.Split(body, []byte("\n")) {
		line := bytes.TrimSuffix(rawLine, []byte("\r"))
		if len(bytes.TrimSpace(line)) == 0 {
			flush()
			continue
		}
		switch {
		case bytes.HasPrefix(line, []byte("event:")):
			name = string(bytes.TrimSpace(line[len("event:"):]))
		case bytes.HasPrefix(line, []byte("data:")):
			chunk := bytes.TrimSpace(line[len("data:"):])
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, chunk...)
		}
	}
	flush()
	return events
}

// validateCodexSSE 扫描 SSE 终态：response.completed 且 response.output 长度
// ≥1 = 成功；response.failed / error 事件 = 失败（固定文案 + 白名单 code）。
func validateCodexSSE(body []byte) (bool, string, string) {
	for _, ev := range parseSSEEvents(body) {
		if bytes.Equal(ev.data, []byte("[DONE]")) || !gjson.ValidBytes(ev.data) {
			continue
		}
		typ := gjson.GetBytes(ev.data, "type").String()
		if typ == "" {
			typ = ev.name
		}
		switch typ {
		case "response.completed":
			if gjson.GetBytes(ev.data, "response.output").IsArray() && gjson.GetBytes(ev.data, "response.output.0").Exists() {
				return true, "", ""
			}
			return false, "upstream response missing usable output", ""
		case "response.failed", "error":
			return false, "upstream stream failed", errorCodeDetail(ev.data)
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

var probeErrorCodePattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

// errorCodeDetail 只从上游错误对象提取白名单形态的 error.code/error.type，
// 以 code=<value> 返回；message/原始 JSON/其他文本一律丢弃。
func errorCodeDetail(body []byte) string {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ""
	}
	for _, path := range []string{"error.code", "error.type", "response.error.code", "response.error.type"} {
		v := gjson.GetBytes(body, path)
		if v.Type == gjson.String && probeErrorCodePattern.MatchString(v.String()) {
			return "code=" + v.String()
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

func isContextError(err error) bool {
	return isTimeoutError(err) || errors.Is(err, context.Canceled)
}

// classifyTransportError 把传输层错误归一为固定文案；redirect/Location 解析
// 失败一律记 cross-origin 阻止，绝不回显 URL/Location/底层错误文本。
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
	msg := err.Error()
	if strings.Contains(msg, "probe blocked request to") ||
		strings.Contains(msg, "failed to parse Location header") ||
		strings.Contains(msg, "redirect") {
		return "probe blocked cross-origin redirect"
	}
	return "probe request failed"
}

// ---- 脱敏 / 截断（spec §5.2/§6） ----

// redactText 把每个 secret 的所有可回显形态替换为 ***：原文、JSON 转义
// （含 `\/`）、QueryEscape、PathEscape、URL userinfo（url.UserPassword 形态）
// 以及它们的小写百分号十六进制变体。secret 为空跳过。
func redactText(s string, secrets []string) string {
	if s == "" {
		return s
	}
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		for _, form := range redactionForms(secret) {
			if form == "" {
				continue
			}
			s = strings.ReplaceAll(s, form, "***")
		}
	}
	return s
}

func redactionForms(secret string) []string {
	jsonForm := jsonEscape(secret)
	queryForm := url.QueryEscape(secret)
	pathForm := url.PathEscape(secret)
	userinfoForm := url.UserPassword("u", secret).String()
	userinfoForm = strings.TrimPrefix(userinfoForm, "u:")
	forms := []string{
		secret,
		jsonForm,
		strings.ReplaceAll(jsonForm, "/", `\/`),
		queryForm,
		lowerPercentHex(queryForm),
		pathForm,
		lowerPercentHex(pathForm),
		userinfoForm,
		lowerPercentHex(userinfoForm),
	}
	// 去空并保持顺序去重（原文优先，避免短形态先替换破坏长形态）。
	seen := make(map[string]struct{}, len(forms))
	out := forms[:0]
	for _, form := range forms {
		if form == "" {
			continue
		}
		if _, ok := seen[form]; ok {
			continue
		}
		seen[form] = struct{}{}
		out = append(out, form)
	}
	return out
}

func jsonEscape(s string) string {
	b, err := json.Marshal(s)
	if err != nil || len(b) < 2 {
		return s
	}
	return string(b[1 : len(b)-1])
}

// lowerPercentHex 只把 %XX 的十六进制字母转小写；其他字符保持原样。
func lowerPercentHex(s string) string {
	b := []byte(s)
	for i := 0; i+2 < len(b); i++ {
		if b[i] != '%' || !isHexDigit(b[i+1]) || !isHexDigit(b[i+2]) {
			continue
		}
		b[i+1] = lowerHex(b[i+1])
		b[i+2] = lowerHex(b[i+2])
	}
	return string(b)
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func lowerHex(c byte) byte {
	if c >= 'A' && c <= 'F' {
		return c + ('a' - 'A')
	}
	return c
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
