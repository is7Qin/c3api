// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package probe

// 本文件承载 account-probe-content 新增的两类探测格式（openai-images 的
// generations/edits、openai-search）与安全投影（output_preview/image_previews）。
// 安全边界（spec §4/§6）：只有 HTTP 200 且成功判定通过的上游响应才进入抽取；
// 错误路径的原始 JSON/SSE/envelope、URL、ID、凭据绝不进入结果。成功文本是
// best-effort 脱敏的模型可见输出，**不是 formal secret boundary**，绝不写日志。

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/sdkbridge"
)

// ---- 请求载荷（固定字段形状；字段顺序稳定） ----

type imagesProbePayload struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	ResponseFormat string `json:"response_format"`
}

func imagesGenerationsPayload(model, prompt string) []byte {
	b, _ := json.Marshal(imagesProbePayload{
		Model:          model,
		Prompt:         prompt,
		ResponseFormat: "b64_json",
	})
	return b
}

// imagesEditsMultipart 组静态族 edits 的 multipart：model/prompt/
// response_format=b64_json + 单张 image（原始字节 + 图片 Content-Type）。
// 不内置任何测试图片；图片只来自调用方 image_url。
func imagesEditsMultipart(model, prompt, imageMIME string, image []byte) ([]byte, string, error) {
	if !domain.ValidProbeImageMIME(imageMIME) {
		return nil, "", errors.New("probe: invalid image MIME")
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("model", model); err != nil {
		return nil, "", err
	}
	if err := mw.WriteField("prompt", prompt); err != nil {
		return nil, "", err
	}
	if err := mw.WriteField("response_format", "b64_json"); err != nil {
		return nil, "", err
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="image"; filename="image"`)
	header.Set("Content-Type", imageMIME)
	part, err := mw.CreatePart(header)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(image); err != nil {
		return nil, "", err
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

// searchProbePayloadBody 是 codex CLI 形态的 search 请求体（字段形状固定：
// id/model/input/commands/settings/max_output_tokens）。
type searchProbePayloadBody struct {
	ID              string             `json:"id"`
	Model           string             `json:"model"`
	Input           []searchProbeInput `json:"input"`
	Commands        []struct{}         `json:"commands"`
	Settings        struct{}           `json:"settings"`
	MaxOutputTokens int                `json:"max_output_tokens"`
}

type searchProbeInput struct {
	Type    string             `json:"type"`
	Request searchProbeRequest `json:"request"`
}

type searchProbeRequest struct {
	Queries []string `json:"queries"`
}

func searchProbePayload(model, query string) []byte {
	b, _ := json.Marshal(searchProbePayloadBody{
		ID:    searchProbeID(),
		Model: model,
		Input: []searchProbeInput{{
			Type:    "web_search_call",
			Request: searchProbeRequest{Queries: []string{query}},
		}},
		Commands:        []struct{}{},
		MaxOutputTokens: 256,
	})
	return b
}

// ---- search correlation id（启动 seed XOR 单调 counter） ----

// probeIDSeed 在进程启动时生成一次（8 字节 crypto/rand；失败回退纳秒时间
// 戳）。probeIDCounter 单调递增；两者 XOR 后固定输出 16 hex，保证同进程内
// 并发/重复探测不会共用 correlation id。
var (
	probeIDSeed    = newProbeIDSeed()
	probeIDCounter atomic.Uint64
)

func newProbeIDSeed() uint64 {
	return newProbeIDSeedFrom(rand.Reader)
}

// newProbeIDSeedFrom 是 seed 的测试 seam：读取失败回退纳秒时间戳。
func newProbeIDSeedFrom(r io.Reader) uint64 {
	var b [8]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return uint64(time.Now().UnixNano())
	}
	return binary.LittleEndian.Uint64(b[:])
}

func searchProbeID() string {
	return fmt.Sprintf("probe_%016x", probeIDSeed^probeIDCounter.Add(1))
}

// ---- 静态 key 族：images generations/edits ----

func (p *Prober) probeStaticImages(ctx context.Context, spec domain.ProbeSpec) domain.ProbeResult {
	if p == nil || p.factory == nil {
		return domain.ProbeResult{Error: domain.ProbeErrorRequestFailed}
	}
	key := domain.UpstreamPathImagesGen
	if spec.ImageMode == domain.ProbeImageModeEdits {
		key = domain.UpstreamPathImagesEdits
	}
	path := domain.ResolveUpstreamPath(spec.CredentialType, spec.UpstreamPaths, key, domain.UpstreamDefaultTail(key))
	// 新 images 格式：只有 200 允许 2 MiB+1（溢出探测），其他 2xx 恰好 2 MiB。
	hc, err := p.guardedHTTPClient200Only(spec)
	if err != nil {
		return domain.ProbeResult{Error: domain.ProbeErrorRequestFailed}
	}

	payload := imagesGenerationsPayload(spec.UpstreamModel, spec.Input)
	contentType := ""
	if spec.ImageMode == domain.ProbeImageModeEdits {
		image, imageMIME, fetchErr := p.fetchProbeImage(ctx, spec)
		if fetchErr != "" {
			return domain.ProbeResult{Error: fetchErr}
		}
		var buildErr error
		payload, contentType, buildErr = imagesEditsMultipart(spec.UpstreamModel, spec.Input, imageMIME, image)
		if buildErr != nil {
			return domain.ProbeResult{Error: domain.ProbeErrorRequestFailed}
		}
	}

	resp, callErr := p.factory.ImagesRaw(ctx, spec.TemplateID, spec.BaseURL, path, spec.Credential, contentType, payload, nil, hc)
	return imagesRawResult(ctx, resp, callErr)
}

// imagesRawResult 统一 images 静态族响应处理：200 成功 = JSON 深度守卫 +
// data 为非空数组且首元素是对象；预览只取 b64_json。
func imagesRawResult(ctx context.Context, resp *http.Response, callErr error) domain.ProbeResult {
	if callErr != nil {
		return domain.ProbeResult{Error: classifyTransportError(callErr)}
	}
	res := domain.ProbeResult{StatusCode: resp.StatusCode}
	bodyLimit := domain.ProbeMaxImageResponseBytes
	if resp.StatusCode != http.StatusOK {
		bodyLimit = domain.ProbeMaxBodyBytes // 非 200 错误体仍严格 2 MiB
	}
	body, overflow, readErr := readLimitedBody(resp.Body, bodyLimit)
	_ = resp.Body.Close() // 恰好一次（body 归本函数）
	if readErr != nil {
		if ctx.Err() != nil {
			return domain.ProbeResult{Error: classifyTransportError(ctx.Err())}
		}
		return domain.ProbeResult{Error: classifyTransportError(readErr)}
	}
	if overflow {
		res.Error = domain.ProbeErrorImageBodyTooLarge
		return res
	}
	if resp.StatusCode != http.StatusOK {
		res.Error = classifyUpstreamStatus(resp.StatusCode)
		res.Detail = errorCodeDetail(body)
		return res
	}
	if !validateImages(body) {
		res.Error = domain.ProbeErrorNoUsableOutput
		res.Detail = errorCodeDetail(body)
		return res
	}
	res.Success = true
	res.ImagePreviews = imagePreviews(body)
	return res
}

// ---- 静态 key 族：search ----

func (p *Prober) probeStaticSearch(ctx context.Context, spec domain.ProbeSpec) domain.ProbeResult {
	if p == nil || p.factory == nil {
		return domain.ProbeResult{Error: domain.ProbeErrorRequestFailed}
	}
	key := domain.UpstreamPathSearch
	path := domain.ResolveUpstreamPath(spec.CredentialType, spec.UpstreamPaths, key, domain.UpstreamDefaultTail(key))
	hc, err := p.guardedHTTPClient200Only(spec)
	if err != nil {
		return domain.ProbeResult{Error: domain.ProbeErrorRequestFailed}
	}
	resp, callErr := p.factory.SearchRaw(ctx, spec.TemplateID, spec.BaseURL, path, spec.Credential,
		searchProbePayload(spec.UpstreamModel, spec.Input), nil, hc)
	return searchRawResult(ctx, resp, callErr)
}

// searchRawResult 统一 search 静态族响应处理：200 成功 = JSON 深度守卫 +
// （status=="completed" 或 output 非空字符串 或 results 非空数组）。
func searchRawResult(ctx context.Context, resp *http.Response, callErr error) domain.ProbeResult {
	if callErr != nil {
		return domain.ProbeResult{Error: classifyTransportError(callErr)}
	}
	res := domain.ProbeResult{StatusCode: resp.StatusCode}
	body, overflow, readErr := readLimitedBody(resp.Body, domain.ProbeMaxBodyBytes)
	_ = resp.Body.Close()
	if readErr != nil {
		if ctx.Err() != nil {
			return domain.ProbeResult{Error: classifyTransportError(ctx.Err())}
		}
		return domain.ProbeResult{Error: classifyTransportError(readErr)}
	}
	if overflow {
		res.Error = domain.ProbeErrorBodyTooLarge
		return res
	}
	if resp.StatusCode != http.StatusOK {
		res.Error = classifyUpstreamStatus(resp.StatusCode)
		res.Detail = errorCodeDetail(body)
		return res
	}
	if !validateSearch(body) {
		res.Error = domain.ProbeErrorNoUsableOutput
		res.Detail = errorCodeDetail(body)
		return res
	}
	res.Success = true
	res.OutputPreview = extractSearchPreview(body)
	return res
}

// ---- codex 族：images / search（one-shot seam） ----

func (p *Prober) probeCodexImages(ctx context.Context, spec domain.ProbeSpec) domain.ProbeResult {
	transport, err := p.guardedCodexTransport(spec)
	if err != nil {
		return domain.ProbeResult{Error: domain.ProbeErrorRequestFailed}
	}
	params := &domain.ImageGenParams{Model: spec.UpstreamModel, Prompt: spec.Input}
	if spec.ImageMode == domain.ProbeImageModeEdits {
		imageURL := spec.ImageURL
		params.Images = []domain.ImageRef{{ImageURL: &imageURL}}
	}
	status, body, callErr := sdkbridge.GenerateImageOnce(ctx, spec.Credential, spec.CodexAccountID, transport, params)
	return codexFormatResult(ctx, status, body, callErr, true)
}

func (p *Prober) probeCodexSearch(ctx context.Context, spec domain.ProbeSpec) domain.ProbeResult {
	transport, err := p.guardedCodexTransport(spec)
	if err != nil {
		return domain.ProbeResult{Error: domain.ProbeErrorRequestFailed}
	}
	status, body, callErr := sdkbridge.SearchOnce(ctx, spec.Credential, spec.CodexAccountID, transport,
		searchProbePayload(spec.UpstreamModel, spec.Input))
	return codexFormatResult(ctx, status, body, callErr, false)
}

// codexFormatResult 统一 codex images/search 结论：ctx 取消/到期优先于 body
// 超限与状态分类；ErrBodyTooLarge 按 images 选择 8 MiB/2 MiB 固定文案；其余 err 在 status != 0 时按
// 真实 HTTP 状态分类（201/204/206 → upstream error），status=0 才走传输分类。
func codexFormatResult(ctx context.Context, status int, body []byte, callErr error, images bool) domain.ProbeResult {
	if ctx.Err() != nil {
		return domain.ProbeResult{Error: classifyTransportError(ctx.Err())}
	}
	if errors.Is(callErr, sdkbridge.ErrBodyTooLarge) {
		errMsg := domain.ProbeErrorBodyTooLarge
		if images {
			errMsg = domain.ProbeErrorImageBodyTooLarge
		}
		return domain.ProbeResult{StatusCode: status, Error: errMsg}
	}
	if callErr != nil {
		// status 200 + err 只可能来自“200 body 不可用”（SDK 解析失败/读取失败）：
		// 固定判 missing usable output，而不是把它当成 upstream error。
		if status == http.StatusOK {
			return domain.ProbeResult{StatusCode: status, Error: domain.ProbeErrorNoUsableOutput}
		}
		if status != 0 {
			return domain.ProbeResult{
				StatusCode: status,
				Error:      classifyUpstreamStatus(status),
				Detail:     errorCodeDetail(body),
			}
		}
		return domain.ProbeResult{Error: classifyTransportError(callErr)}
	}
	res := domain.ProbeResult{StatusCode: status}
	if status != http.StatusOK {
		res.Error = classifyUpstreamStatus(status)
		res.Detail = errorCodeDetail(body)
		return res
	}
	if images {
		if !validateImages(body) {
			res.Error = domain.ProbeErrorNoUsableOutput
			res.Detail = errorCodeDetail(body)
			return res
		}
		res.Success = true
		res.ImagePreviews = imagePreviews(body)
		return res
	}
	if !validateSearch(body) {
		res.Error = domain.ProbeErrorNoUsableOutput
		res.Detail = errorCodeDetail(body)
		return res
	}
	res.Success = true
	res.OutputPreview = extractSearchPreview(body)
	return res
}

// ---- 成功判定（200-only；JSON 深度守卫先于 gjson） ----

func validateImages(body []byte) bool {
	if len(body) == 0 || !domain.ShallowJSON(body, domain.ProbeMaxJSONDepth) || !gjson.ValidBytes(body) {
		return false
	}
	data := gjson.GetBytes(body, "data")
	if !data.IsArray() {
		return false
	}
	// Get("#") / Get("0") 不物化整个数组；Array() 会为每个元素分配
	// gjson.Result（2 MiB 的密集数组可放大出数十 MB）。
	return data.Get("#").Int() > 0 && data.Get("0").IsObject()
}

func validateSearch(body []byte) bool {
	if len(body) == 0 || !domain.ShallowJSON(body, domain.ProbeMaxJSONDepth) || !gjson.ValidBytes(body) {
		return false
	}
	r := gjson.ParseBytes(body)
	if status := r.Get("status"); status.Type == gjson.String && status.String() == "completed" {
		return true
	}
	if out := r.Get("output"); out.Type == gjson.String && out.String() != "" {
		return true
	}
	results := r.Get("results")
	return results.IsArray() && results.Get("#").Int() > 0
}

// ---- 安全预览抽取（仅 200 成功体；确定性字段路径） ----

func extractStaticPreview(format domain.RequestFormat, body []byte) string {
	r := gjson.ParseBytes(body)
	switch format {
	case domain.FormatOpenAIChat:
		return extractChatPreview(r)
	case domain.FormatOpenAIResponses, domain.FormatOpenAIResponsesWS:
		return extractResponsesPreview(r)
	case domain.FormatAnthropic:
		return extractAnthropicPreview(r)
	default:
		return ""
	}
}

// extractChatPreview：按 choices 顺序取 message.content（字符串，或数组内
// type=="text" 的 text），多个值用 "\n" 连接。
func extractChatPreview(r gjson.Result) string {
	choices := r.Get("choices")
	if !choices.IsArray() {
		return ""
	}
	var parts []string
	choices.ForEach(func(_, choice gjson.Result) bool {
		content := choice.Get("message.content")
		switch {
		case content.Type == gjson.String:
			parts = append(parts, content.String())
		case content.IsArray():
			content.ForEach(func(_, item gjson.Result) bool {
				if item.Get("type").String() != "text" {
					return true
				}
				if text := item.Get("text"); text.Type == gjson.String {
					parts = append(parts, text.String())
				}
				return true
			})
		}
		return true
	})
	return strings.Join(parts, "\n")
}

// extractResponsesPreview：顶层 output_text 非空时只用它；否则按 output 顺序
// 取 content[].type=="output_text" 的 text。
func extractResponsesPreview(r gjson.Result) string {
	if out := r.Get("output_text"); out.Type == gjson.String && out.String() != "" {
		return out.String()
	}
	return responsesOutputText(r)
}

func responsesOutputText(r gjson.Result) string {
	output := r.Get("output")
	if !output.IsArray() {
		return ""
	}
	var parts []string
	output.ForEach(func(_, item gjson.Result) bool {
		content := item.Get("content")
		if !content.IsArray() {
			return true
		}
		content.ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() != "output_text" {
				return true
			}
			if text := block.Get("text"); text.Type == gjson.String {
				parts = append(parts, text.String())
			}
			return true
		})
		return true
	})
	return strings.Join(parts, "\n")
}

// extractAnthropicPreview：按 content 顺序取 type=="text" 的 text。
func extractAnthropicPreview(r gjson.Result) string {
	content := r.Get("content")
	if !content.IsArray() {
		return ""
	}
	var parts []string
	content.ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").String() != "text" {
			return true
		}
		if text := block.Get("text"); text.Type == gjson.String {
			parts = append(parts, text.String())
		}
		return true
	})
	return strings.Join(parts, "\n")
}

// extractSearchPreview：只取 output 字符串；为空时按顺序取
// results[].web_search.title 与 content；url 永不投影。
func extractSearchPreview(body []byte) string {
	return searchPreviewFromResult(gjson.ParseBytes(body))
}

func searchPreviewFromResult(r gjson.Result) string {
	if out := r.Get("output"); out.Type == gjson.String && out.String() != "" {
		return out.String()
	}
	results := r.Get("results")
	if !results.IsArray() {
		return ""
	}
	var parts []string
	results.ForEach(func(_, item gjson.Result) bool {
		web := item.Get("web_search")
		for _, field := range []string{"title", "content"} {
			if value := web.Get(field); value.Type == gjson.String && value.String() != "" {
				parts = append(parts, value.String())
			}
		}
		return true
	})
	return strings.Join(parts, "\n")
}

// codexResponsePreview 从终态 response.completed 事件的 data.response 复用
// responses 抽取（原始 SSE 不进入结果）。
func codexResponsePreview(response gjson.Result) string {
	return extractResponsesPreview(response)
}

// ---- 图片预览（base64 + 魔数校验；单张/合计/张数上限） ----

var pngMagic = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}

// imageMagicMIME 只认 PNG/JPEG/WebP 魔数；顶层 output_format 不参与判定。
func imageMagicMIME(raw []byte) (string, bool) {
	switch {
	case bytes.HasPrefix(raw, pngMagic):
		return "image/png", true
	case len(raw) >= 3 && raw[0] == 0xFF && raw[1] == 0xD8 && raw[2] == 0xFF:
		return "image/jpeg", true
	case len(raw) >= 12 && bytes.Equal(raw[:4], []byte("RIFF")) && bytes.Equal(raw[8:12], []byte("WEBP")):
		return "image/webp", true
	default:
		return "", false
	}
}

// imagePreviews 只取 data[].b64_json：标准 base64 解码、魔数校验、单张解码
// ≤2 MiB、合计 ≤4 MiB、最多 4 张；不合格的图片丢弃。返回恒非 nil。
func imagePreviews(body []byte) []string {
	out := make([]string, 0, domain.ProbeMaxImagePreviews)
	data := gjson.GetBytes(body, "data")
	if !data.IsArray() {
		return out
	}
	total := 0
	data.ForEach(func(_, item gjson.Result) bool {
		if len(out) >= domain.ProbeMaxImagePreviews {
			return false
		}
		encoded := item.Get("b64_json")
		if encoded.Type != gjson.String || encoded.String() == "" {
			return true
		}
		raw, err := base64.StdEncoding.DecodeString(encoded.String())
		if err != nil || len(raw) == 0 || len(raw) > domain.ProbeMaxBodyBytes {
			return true
		}
		if total+len(raw) > domain.ProbeMaxImageBytes {
			return true
		}
		mime, ok := imageMagicMIME(raw)
		if !ok {
			return true
		}
		total += len(raw)
		out = append(out, "data:"+mime+";base64,"+base64.StdEncoding.EncodeToString(raw))
		return len(out) < domain.ProbeMaxImagePreviews
	})
	return out
}

// ---- 图片源取图（静态族 edits）：data URL 本地解码 / http(s) 账号出口下载 ----

// fetchProbeImage 返回 (原始字节, 图片 Content-Type, 固定错误文案)。失败文案
// 只来自 domain.ProbeError*，绝不包含 image_url/host/query。
func (p *Prober) fetchProbeImage(ctx context.Context, spec domain.ProbeSpec) ([]byte, string, string) {
	if strings.HasPrefix(strings.ToLower(spec.ImageURL), "data:") {
		return decodeProbeDataImage(spec.ImageURL)
	}
	return p.downloadProbeImage(ctx, spec)
}

func decodeProbeDataImage(raw string) ([]byte, string, string) {
	meta, payload, ok := strings.Cut(raw, ",")
	if !ok {
		return nil, "", domain.ProbeErrorImageInvalid
	}
	lower := strings.ToLower(meta)
	if !strings.HasPrefix(lower, "data:image/") || !strings.HasSuffix(lower, ";base64") {
		return nil, "", domain.ProbeErrorImageInvalid
	}
	mime := mediaType(meta[len("data:") : len(meta)-len(";base64")])
	if !domain.ValidProbeImageMIME(mime) {
		return nil, "", domain.ProbeErrorImageInvalid
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || len(decoded) == 0 {
		return nil, "", domain.ProbeErrorImageInvalid
	}
	if len(decoded) > domain.ProbeMaxImageBytes {
		return nil, "", domain.ProbeErrorImageTooLarge
	}
	return decoded, mime, ""
}

// downloadProbeImage 用账号出口下载图片：不跟随重定向、全新空请求头（仅
// Accept image/* 与 User-Agent，绝不携带入站头/账号 key/Authorization/
// ChatGPT-Account-ID/Cookie）、Jar 清空、HTTP 200、Content-Type image/*
// （缺失时用 http.DetectContentType）、原始字节 ≤4 MiB。
func (p *Prober) downloadProbeImage(ctx context.Context, spec domain.ProbeSpec) ([]byte, string, string) {
	if p == nil || p.pool == nil {
		return nil, "", domain.ProbeErrorImageFetchFailed
	}
	eg := p.pool.For(spec.Egress)
	if eg == nil || eg.HTTP == nil {
		return nil, "", domain.ProbeErrorImageFetchFailed
	}
	target, err := url.Parse(spec.ImageURL)
	if err != nil {
		return nil, "", domain.ProbeErrorImageFetchFailed
	}
	// URL userinfo 会被 net/http 自动转成 Basic Authorization 头；探测取图必须
	// 保持全新空头，不能把 URL 里的凭据带到图片源。
	target.User = nil
	hc := *eg.HTTP
	hc.Jar = nil // 克隆 client 必须清空 Jar
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	hc.Transport = transportOrDefault(eg.HTTP.Transport)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, "", domain.ProbeErrorImageFetchFailed
	}
	req.Header = make(http.Header) // 全新空头
	req.Header.Set("Accept", "image/*")
	req.Header.Set("User-Agent", "c3api-probe/1")

	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", classifyTransportError(ctx.Err())
		}
		return nil, "", domain.ProbeErrorImageFetchFailed
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", domain.ProbeErrorImageFetchFailed
	}
	data, overflow, readErr := readLimitedBody(resp.Body, domain.ProbeMaxImageBytes)
	if readErr != nil {
		if ctx.Err() != nil {
			return nil, "", classifyTransportError(ctx.Err())
		}
		return nil, "", domain.ProbeErrorImageFetchFailed
	}
	if overflow {
		return nil, "", domain.ProbeErrorImageTooLarge
	}
	if len(data) == 0 {
		return nil, "", domain.ProbeErrorImageInvalid
	}
	imageMIME := mediaType(resp.Header.Get("Content-Type"))
	if imageMIME == "" {
		imageMIME = mediaType(http.DetectContentType(data))
	}
	if !domain.ValidProbeImageMIME(imageMIME) {
		return nil, "", domain.ProbeErrorImageInvalid
	}
	return data, imageMIME, ""
}

// mediaType 归一 Content-Type/DetectContentType：去掉参数并小写。
func mediaType(value string) string {
	if i := strings.IndexByte(value, ';'); i >= 0 {
		value = value[:i]
	}
	return strings.ToLower(strings.TrimSpace(value))
}

// ---- 脱敏（唯一实现；只作用于 output_preview） ----

var (
	previewSchemeURL  = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s"'<>]+`)
	previewMailtoURL  = regexp.MustCompile(`(?i)\bmailto:[^\s"'<>]+`)
	previewDataURL    = regexp.MustCompile(`(?i)\bdata:[^\s"'<>]+`)
	previewWWWURL     = regexp.MustCompile(`(?i)\bwww\.[^\s"'<>]+`)
	previewBearer     = regexp.MustCompile(`(?i)\bBearer\s+[^\s"'<>]+`)
	previewKeyToken   = regexp.MustCompile(`(?i)\b(?:sk|pk|mk|pat|rk|ak)-[a-z0-9_-]{6,}`)
	previewPrefixedID = regexp.MustCompile(`(?i)\b(?:req|resp|msg|chatcmpl|call|fc|toolu|run|thread|asst|file|img|igc)_[a-z0-9]{4,}`)
	previewUUID       = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
)

// sanitizePreview 替换任意 URL-like 文本（任意 scheme://、mailto:、data:、
// www.）、Bearer token、常见 key 形态、内部 ID 前缀与 UUID。不删除普通文本；
// 不因脱敏改变成功判定。
func sanitizePreview(s string) string {
	if s == "" {
		return ""
	}
	s = previewSchemeURL.ReplaceAllString(s, "[url]")
	s = previewMailtoURL.ReplaceAllString(s, "[url]")
	s = previewDataURL.ReplaceAllString(s, "[url]")
	s = previewWWWURL.ReplaceAllString(s, "[url]")
	s = previewBearer.ReplaceAllString(s, "Bearer [redacted]")
	s = previewKeyToken.ReplaceAllString(s, "[redacted]")
	s = previewPrefixedID.ReplaceAllString(s, "[redacted-id]")
	s = previewUUID.ReplaceAllString(s, "[redacted-id]")
	return s
}
