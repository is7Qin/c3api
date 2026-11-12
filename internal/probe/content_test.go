// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package probe

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

type contentRT func(*http.Request) (*http.Response, error)

func (f contentRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func contentResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(body))}
}

func contentResponseBytes(status int, body []byte, header http.Header) *http.Response {
	if header == nil {
		header = make(http.Header)
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(bytes.NewReader(body))}
}

func contentSpec(format domain.RequestFormat) domain.ProbeSpec {
	spec := staticSpec("https://upstream.example")
	spec.Format = format
	spec.UpstreamModel = "up-model"
	spec.RequestedModel = "client-model"
	return spec
}

func contentImageSpec(mode, imageURL string) domain.ProbeSpec {
	spec := contentSpec(domain.FormatOpenAIImages)
	spec.ImageMode = mode
	spec.ImageURL = imageURL
	if mode == domain.ProbeImageModeEdits {
		spec.Input = domain.ProbeDefaultInputEdits
	} else {
		spec.Input = domain.ProbeDefaultInputImages
	}
	return spec
}

func contentSearchSpec() domain.ProbeSpec {
	spec := contentSpec(domain.FormatOpenAISearch)
	spec.Input = domain.ProbeDefaultInputSearch
	return spec
}

func b64(raw []byte) string { return base64.StdEncoding.EncodeToString(raw) }

func pngBytes(n int) []byte {
	raw := make([]byte, n)
	copy(raw, pngMagic)
	for i := len(pngMagic); i < n; i++ {
		raw[i] = byte(i)
	}
	return raw
}

func imagesBody(b64Values ...string) string {
	items := make([]string, 0, len(b64Values))
	for _, value := range b64Values {
		items = append(items, `{"b64_json":"`+value+`"}`)
	}
	return `{"data":[` + strings.Join(items, ",") + `]}`
}

func codexCompletedSSE(response string) string {
	return "event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
}

// TestProbeCustomInput 文本四格式的请求体必须携带自定 input（service 已填好
// 默认值；prober 不再有第二份默认表）。
func TestProbeCustomInput(t *testing.T) {
	const input = "custom-input-42"
	cases := []struct {
		format domain.RequestFormat
		body   string
		path   string
	}{
		{domain.FormatOpenAIChat, `{"choices":[{"message":{"content":"ok"}}]}`, "messages.0.content"},
		{domain.FormatOpenAIResponses, `{"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`, "input.0.content.0.text"},
		{domain.FormatOpenAIResponsesWS, `{"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`, "input.0.content.0.text"},
		{domain.FormatAnthropic, `{"content":[{"type":"text","text":"ok"}]}`, "messages.0.content"},
	}
	for _, tc := range cases {
		t.Run(string(tc.format), func(t *testing.T) {
			var captured []byte
			rt := contentRT(func(req *http.Request) (*http.Response, error) {
				captured, _ = io.ReadAll(req.Body)
				return contentResponse(http.StatusOK, tc.body), nil
			})
			spec := contentSpec(tc.format)
			spec.Input = input
			res := newTestProber(t, rt).Probe(context.Background(), spec)
			require.True(t, res.Success)
			require.Equal(t, input, gjson.GetBytes(captured, tc.path).String())
			require.Equal(t, "ok", res.OutputPreview)
		})
	}

	t.Run("codex", func(t *testing.T) {
		var captured []byte
		rt := contentRT(func(req *http.Request) (*http.Response, error) {
			captured, _ = io.ReadAll(req.Body)
			return contentResponse(http.StatusOK, codexCompletedSSE(
				`{"output":[{"type":"message","content":[{"type":"output_text","text":"codex-ok"}]}]}`)), nil
		})
		spec := contentSpec(domain.FormatOpenAIResponses)
		spec.CredentialType = credential.TypeCodexPAT
		spec.Credential = "pat-test"
		spec.Input = input
		res := newTestProber(t, rt).Probe(context.Background(), spec)
		require.True(t, res.Success)
		require.Equal(t, input, gjson.GetBytes(captured, "input.0.content.0.text").String())
		require.Equal(t, "codex-ok", res.OutputPreview)
	})
}

// TestProbeOutputPreviewExtraction 各格式的确定性抽取规则与「仅 200」边界。
func TestProbeOutputPreviewExtraction(t *testing.T) {
	cases := []struct {
		name   string
		format domain.RequestFormat
		status int
		body   string
		want   string
		wantOK bool
	}{
		{
			name: "chat-multi-choices-and-array", format: domain.FormatOpenAIChat, status: 200,
			body: `{"choices":[{"message":{"content":"one"}},{"message":{"content":[{"type":"text","text":"two"},{"type":"image_url","text":"ignored"}]}}]}`,
			want: "one\ntwo", wantOK: true,
		},
		{
			name: "responses-output-text-wins", format: domain.FormatOpenAIResponses, status: 200,
			body: `{"output_text":"only","output":[{"content":[{"type":"output_text","text":"ignored"}]}]}`,
			want: "only", wantOK: true,
		},
		{
			name: "responses-output-fallback", format: domain.FormatOpenAIResponses, status: 200,
			body: `{"output":[{"type":"message","content":[{"type":"output_text","text":"a"}]},{"type":"message","content":[{"type":"output_text","text":"b"}]}]}`,
			want: "a\nb", wantOK: true,
		},
		{
			name: "anthropic-text-blocks", format: domain.FormatAnthropic, status: 200,
			body: `{"content":[{"type":"text","text":"a"},{"type":"tool_use","text":"skip"},{"type":"text","text":"b"}]}`,
			want: "a\nb", wantOK: true,
		},
		{
			name: "chat-empty-content-but-finish-reason", format: domain.FormatOpenAIChat, status: 200,
			body: `{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`,
			want: "", wantOK: true,
		},
		{
			name: "static-201-no-preview", format: domain.FormatOpenAIChat, status: 201,
			body: `{"choices":[{"message":{"content":"ok"}}]}`,
			want: "", wantOK: true,
		},
		{
			name: "static-206-no-preview", format: domain.FormatOpenAIChat, status: 206,
			body: `{"choices":[{"message":{"content":"ok"}}]}`,
			want: "", wantOK: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := contentRT(func(*http.Request) (*http.Response, error) {
				return contentResponse(tc.status, tc.body), nil
			})
			res := newTestProber(t, rt).Probe(context.Background(), contentSpec(tc.format))
			require.Equal(t, tc.wantOK, res.Success)
			require.Equal(t, tc.want, res.OutputPreview)
		})
	}

	t.Run("codex-terminal-response", func(t *testing.T) {
		rt := contentRT(func(*http.Request) (*http.Response, error) {
			return contentResponse(http.StatusOK, codexCompletedSSE(
				`{"output":[{"type":"message","content":[{"type":"output_text","text":"codex-a"},{"type":"output_text","text":"codex-b"}]}]}`)), nil
		})
		spec := contentSpec(domain.FormatOpenAIResponses)
		spec.CredentialType = credential.TypeCodexPAT
		spec.Credential = "pat-test"
		res := newTestProber(t, rt).Probe(context.Background(), spec)
		require.True(t, res.Success)
		require.Equal(t, "codex-a\ncodex-b", res.OutputPreview)
	})
}

// TestProbeImagesGenerationsStatic 静态族 generations：JSON prompt +
// response_format=b64_json；200 成功与安全图片预览。
func TestProbeImagesGenerationsStatic(t *testing.T) {
	png := pngBytes(64)
	var capturedPath string
	var capturedBody []byte
	rt := contentRT(func(req *http.Request) (*http.Response, error) {
		capturedPath = req.URL.Path
		capturedBody, _ = io.ReadAll(req.Body)
		return contentResponse(http.StatusOK, imagesBody(b64(png))), nil
	})
	res := newTestProber(t, rt).Probe(context.Background(), contentImageSpec(domain.ProbeImageModeGenerations, ""))
	require.True(t, res.Success, res.Error)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, "/v1/images/generations", capturedPath)
	require.Equal(t, "up-model", gjson.GetBytes(capturedBody, "model").String())
	require.Equal(t, domain.ProbeDefaultInputImages, gjson.GetBytes(capturedBody, "prompt").String())
	require.Equal(t, "b64_json", gjson.GetBytes(capturedBody, "response_format").String())
	require.Empty(t, res.OutputPreview, "images 格式 output_preview 恒为空")
	require.Len(t, res.ImagePreviews, 1)
	require.True(t, strings.HasPrefix(res.ImagePreviews[0], "data:image/png;base64,"))
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(res.ImagePreviews[0], "data:image/png;base64,"))
	require.NoError(t, err)
	require.Equal(t, png, decoded)
}

// TestProbeImagesStatusMatrix 新 images 格式只接受 HTTP 200；201/204/206/错误
// 状态保留真实 status_code 且不投影任何 body。
// TestProbeImagesResponseCaps 生图请求固定 b64_json，正常响应会超过旧的 2 MiB
// 文本/SSE 上限；images 必须使用独立的 8 MiB 上限，且超过后仍返回固定文案。
func TestProbeImagesResponseCaps(t *testing.T) {
	t.Run("large-b64-json-passes-text-cap", func(t *testing.T) {
		raw := pngBytes(domain.ProbeMaxBodyBytes) // base64 后约 2.8 MiB，超过文本上限
		body := imagesBody(b64(raw))
		require.Greater(t, len(body), domain.ProbeMaxBodyBytes)
		require.LessOrEqual(t, len(body), domain.ProbeMaxImageResponseBytes)
		rt := contentRT(func(*http.Request) (*http.Response, error) {
			return contentResponse(http.StatusOK, body), nil
		})
		res := newTestProber(t, rt).Probe(context.Background(), contentImageSpec(domain.ProbeImageModeGenerations, ""))
		require.True(t, res.Success, res.Error)
		require.Equal(t, http.StatusOK, res.StatusCode)
	})

	t.Run("over-image-cap", func(t *testing.T) {
		huge := strings.Repeat("x", domain.ProbeMaxImageResponseBytes+16)
		rt := contentRT(func(*http.Request) (*http.Response, error) {
			return contentResponse(http.StatusOK, huge), nil
		})
		res := newTestProber(t, rt).Probe(context.Background(), contentImageSpec(domain.ProbeImageModeGenerations, ""))
		require.False(t, res.Success)
		require.Equal(t, http.StatusOK, res.StatusCode)
		require.Equal(t, domain.ProbeErrorImageBodyTooLarge, res.Error)
	})
}

// TestImageCapTransportStatusLimits images 只在 200 放开 8 MiB；非 200 错误体
// 仍严格截断到 2 MiB，不能被成功的 b64_json 上限放大。
func TestImageCapTransportStatusLimits(t *testing.T) {
	tr := newCappedTransportLimit(contentRT(func(*http.Request) (*http.Response, error) {
		return contentResponseBytes(http.StatusOK, bytes.Repeat([]byte("x"), domain.ProbeMaxImageResponseBytes+1), nil), nil
	}), domain.ProbeMaxImageResponseBytes, domain.ProbeMaxBodyBytes, true)
	req, err := http.NewRequest(http.MethodGet, "https://upstream.example/x", nil)
	require.NoError(t, err)
	resp, err := tr.RoundTrip(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Len(t, body, domain.ProbeMaxImageResponseBytes+1, "200 允许 max+1 发现溢出")

	tr = newCappedTransportLimit(contentRT(func(*http.Request) (*http.Response, error) {
		return contentResponseBytes(http.StatusCreated, bytes.Repeat([]byte("x"), domain.ProbeMaxImageResponseBytes), nil), nil
	}), domain.ProbeMaxImageResponseBytes, domain.ProbeMaxBodyBytes, true)
	resp, err = tr.RoundTrip(req)
	require.NoError(t, err)
	body, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Len(t, body, domain.ProbeMaxBodyBytes, "非 200 必须严格 2 MiB")
}

func TestProbeImagesStatusMatrix(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantError  string
		wantDetail string
	}{
		{"created-201", http.StatusCreated, imagesBody(b64(pngBytes(16))), domain.ProbeErrorUpstream, ""},
		{"no-content-204", http.StatusNoContent, "", domain.ProbeErrorUpstream, ""},
		{"partial-206", http.StatusPartialContent, imagesBody(b64(pngBytes(16))), domain.ProbeErrorUpstream, ""},
		{"bad-request-400", http.StatusBadRequest, `{"error":{"code":"invalid_api_key","message":"secret-tenant"}}`, domain.ProbeErrorUpstreamRejected, "code=invalid_api_key"},
		{"rate-limited-429", http.StatusTooManyRequests, `{"error":{"code":"rate_limit_exceeded"}}`, domain.ProbeErrorUpstreamRateLimit, "code=rate_limit_exceeded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := contentRT(func(*http.Request) (*http.Response, error) {
				return contentResponse(tc.status, tc.body), nil
			})
			res := newTestProber(t, rt).Probe(context.Background(), contentImageSpec(domain.ProbeImageModeGenerations, ""))
			require.False(t, res.Success)
			require.Equal(t, tc.status, res.StatusCode, "status_code 必须保留真实值")
			require.Equal(t, tc.wantError, res.Error)
			require.Equal(t, tc.wantDetail, res.Detail)
			require.Empty(t, res.OutputPreview)
			require.Empty(t, res.ImagePreviews)
			require.NotContains(t, res.Error+res.Detail, "secret-tenant")
		})
	}

	for _, body := range []string{"", "{}", `{"data":[]}`, `{"data":["x"]}`, `{"data":null}`} {
		rt := contentRT(func(*http.Request) (*http.Response, error) {
			return contentResponse(http.StatusOK, body), nil
		})
		res := newTestProber(t, rt).Probe(context.Background(), contentImageSpec(domain.ProbeImageModeGenerations, ""))
		require.False(t, res.Success, body)
		require.Equal(t, domain.ProbeErrorNoUsableOutput, res.Error)
		require.Empty(t, res.ImagePreviews)
	}
}

// TestProbeImageMIMERejectsHeaderInjection data URL 的 MIME 会被写进 multipart
// part 头；CR/LF 必须在校验层拒绝，不能交给 mime/multipart 原样输出。
func TestProbeImageMIMERejectsHeaderInjection(t *testing.T) {
	png := pngBytes(16)
	_, _, errMsg := decodeProbeDataImage("data:image/png\r\nX-Injected: 1;base64," + b64(png))
	require.Equal(t, domain.ProbeErrorImageInvalid, errMsg)

	_, _, err := imagesEditsMultipart("m", "p", "image/png\r\nX-Injected: 1", png)
	require.Error(t, err, "multipart 层必须拒绝带控制字符的 MIME")
}

// TestProbeImagesEditsStaticDataURL 静态族 edits：data URL 本地解码 → multipart
// （model/prompt/response_format/image + 图片 Content-Type），命中 images/edits。
func TestProbeImagesEditsStaticDataURL(t *testing.T) {
	image := append([]byte{}, pngBytes(32)...)
	image = append(image, []byte("payload")...)
	const dataURL = "data:image/png;base64,"
	var capturedPath, capturedCT string
	var capturedBody []byte
	rt := contentRT(func(req *http.Request) (*http.Response, error) {
		capturedPath = req.URL.Path
		capturedCT = req.Header.Get("Content-Type")
		capturedBody, _ = io.ReadAll(req.Body)
		return contentResponse(http.StatusOK, imagesBody(b64(pngBytes(16)))), nil
	})
	spec := contentImageSpec(domain.ProbeImageModeEdits, dataURL+b64(image))
	res := newTestProber(t, rt).Probe(context.Background(), spec)
	require.True(t, res.Success, res.Error)
	require.Equal(t, "/v1/images/edits", capturedPath)

	mediaType, params, err := mime.ParseMediaType(capturedCT)
	require.NoError(t, err)
	require.Equal(t, "multipart/form-data", mediaType)
	reader := multipart.NewReader(bytes.NewReader(capturedBody), params["boundary"])
	form, err := reader.ReadForm(1 << 20)
	require.NoError(t, err)
	defer form.RemoveAll()
	require.Equal(t, "up-model", form.Value["model"][0])
	require.Equal(t, domain.ProbeDefaultInputEdits, form.Value["prompt"][0])
	require.Equal(t, "b64_json", form.Value["response_format"][0])
	require.Len(t, form.File["image"], 1)
	file, err := form.File["image"][0].Open()
	require.NoError(t, err)
	defer file.Close()
	gotImage, err := io.ReadAll(file)
	require.NoError(t, err)
	require.Equal(t, image, gotImage)
	require.Equal(t, "image/png", form.File["image"][0].Header.Get("Content-Type"))
}

// TestProbeImagesEditsHTTPFetch 静态族 edits 的 http(s) 取图：全新空头、不跟随
// 重定向、Content-Type/大小校验；失败时 status_code=0 且上游图片端点 0 命中。
func TestProbeImagesEditsHTTPFetch(t *testing.T) {
	png := pngBytes(48)
	const imageURL = "https://images.example/private/secret.png?sig=abc"

	run := func(t *testing.T, imageResp func() (*http.Response, error)) (domain.ProbeResult, *atomic.Int64, *http.Request) {
		t.Helper()
		var upstreamCalls atomic.Int64
		var capturedImageReq *http.Request
		rt := contentRT(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Host {
			case "images.example":
				capturedImageReq = req.Clone(context.Background())
				return imageResp()
			case "upstream.example":
				upstreamCalls.Add(1)
				body, _ := io.ReadAll(req.Body)
				require.Contains(t, string(body), "name=\"image\"")
				return contentResponse(http.StatusOK, imagesBody(b64(png))), nil
			default:
				t.Fatalf("unexpected host %q", req.URL.Host)
				return nil, nil
			}
		})
		return newTestProber(t, rt).Probe(context.Background(), contentImageSpec(domain.ProbeImageModeEdits, imageURL)), &upstreamCalls, capturedImageReq
	}

	t.Run("success-empty-headers", func(t *testing.T) {
		res, upstream, _ := run(t, func() (*http.Response, error) {
			return contentResponseBytes(http.StatusOK, png, http.Header{"Content-Type": {"image/png; charset=binary"}}), nil
		})
		require.True(t, res.Success, res.Error)
		require.Equal(t, int64(1), upstream.Load())

		// 重新执行一次以捕获请求（run 内 capture 在返回后才可读，故用独立断言）。
		var got *http.Request
		rt := contentRT(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host == "images.example" {
				got = req.Clone(context.Background())
				return contentResponseBytes(http.StatusOK, png, http.Header{"Content-Type": {"image/png"}}), nil
			}
			return contentResponse(http.StatusOK, imagesBody(b64(png))), nil
		})
		res = newTestProber(t, rt).Probe(context.Background(), contentImageSpec(domain.ProbeImageModeEdits, imageURL))
		require.True(t, res.Success, res.Error)
		require.NotNil(t, got)
		require.Equal(t, "image/*", got.Header.Get("Accept"))
		require.NotEmpty(t, got.Header.Get("User-Agent"))
		for _, header := range []string{"Authorization", "Cookie", "X-Api-Key", "ChatGPT-Account-ID", "X-Forwarded-For"} {
			require.Empty(t, got.Header.Get(header), "取图请求不得携带 %s", header)
		}
	})

	t.Run("url-userinfo-not-forwarded", func(t *testing.T) {
		var got *http.Request
		rt := contentRT(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host == "images.example" {
				got = req.Clone(context.Background())
				return contentResponseBytes(http.StatusOK, png, http.Header{"Content-Type": {"image/png"}}), nil
			}
			return contentResponse(http.StatusOK, imagesBody(b64(png))), nil
		})
		res := newTestProber(t, rt).Probe(context.Background(),
			contentImageSpec(domain.ProbeImageModeEdits, "https://user:pass@images.example/private/secret.png"))
		require.True(t, res.Success, res.Error)
		require.NotNil(t, got)
		require.Nil(t, got.URL.User, "取图请求不得携带 URL userinfo")
		require.Empty(t, got.Header.Get("Authorization"))
	})

	t.Run("missing-content-type-detected", func(t *testing.T) {
		res, upstream, _ := run(t, func() (*http.Response, error) {
			return contentResponseBytes(http.StatusOK, png, make(http.Header)), nil
		})
		require.True(t, res.Success, res.Error)
		require.Equal(t, int64(1), upstream.Load())
	})

	t.Run("non-200", func(t *testing.T) {
		res, upstream, _ := run(t, func() (*http.Response, error) {
			return contentResponseBytes(http.StatusFound, []byte("redirect"), http.Header{"Location": {"https://evil.example"}}), nil
		})
		require.False(t, res.Success)
		require.Zero(t, res.StatusCode)
		require.Equal(t, domain.ProbeErrorImageFetchFailed, res.Error)
		require.Zero(t, upstream.Load(), "取图失败不得命中上游图片端点")
	})

	t.Run("non-image-content-type", func(t *testing.T) {
		res, upstream, _ := run(t, func() (*http.Response, error) {
			return contentResponseBytes(http.StatusOK, []byte("plain text"), http.Header{"Content-Type": {"text/plain"}}), nil
		})
		require.Equal(t, domain.ProbeErrorImageInvalid, res.Error)
		require.Zero(t, res.StatusCode)
		require.Zero(t, upstream.Load())
	})

	t.Run("exceeds-4MiB", func(t *testing.T) {
		huge := pngBytes(domain.ProbeMaxImageBytes + 1)
		res, upstream, _ := run(t, func() (*http.Response, error) {
			return contentResponseBytes(http.StatusOK, huge, http.Header{"Content-Type": {"image/png"}}), nil
		})
		require.Equal(t, domain.ProbeErrorImageTooLarge, res.Error)
		require.Zero(t, res.StatusCode)
		require.Zero(t, upstream.Load())
	})
}

// TestProbeImagesEditsFetchFailures data URL 与连接/取消失败的固定映射；失败时
// 上游图片端点 0 命中。
func TestProbeImagesEditsFetchFailures(t *testing.T) {
	t.Run("bad-base64", func(t *testing.T) {
		var upstreamCalls atomic.Int64
		rt := contentRT(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host == "upstream.example" {
				upstreamCalls.Add(1)
			}
			return contentResponse(http.StatusOK, imagesBody()), nil
		})
		res := newTestProber(t, rt).Probe(context.Background(),
			contentImageSpec(domain.ProbeImageModeEdits, "data:image/png;base64,!!!not-base64!!!"))
		require.Equal(t, domain.ProbeErrorImageInvalid, res.Error)
		require.Zero(t, res.StatusCode)
		require.Zero(t, upstreamCalls.Load())
	})

	t.Run("dial-failure", func(t *testing.T) {
		var upstreamCalls atomic.Int64
		rt := contentRT(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host == "upstream.example" {
				upstreamCalls.Add(1)
				return contentResponse(http.StatusOK, imagesBody()), nil
			}
			return nil, io.ErrUnexpectedEOF
		})
		res := newTestProber(t, rt).Probe(context.Background(),
			contentImageSpec(domain.ProbeImageModeEdits, "https://images.example/x.png"))
		require.Equal(t, domain.ProbeErrorImageFetchFailed, res.Error)
		require.Zero(t, res.StatusCode)
		require.Zero(t, upstreamCalls.Load())
	})

	t.Run("cancel-during-fetch", func(t *testing.T) {
		started := make(chan struct{})
		var once sync.Once
		var upstreamCalls atomic.Int64
		rt := contentRT(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host == "upstream.example" {
				upstreamCalls.Add(1)
				return contentResponse(http.StatusOK, imagesBody()), nil
			}
			once.Do(func() { close(started) })
			<-req.Context().Done()
			return nil, req.Context().Err()
		})
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan domain.ProbeResult, 1)
		go func() {
			result <- newTestProber(t, rt).Probe(ctx, contentImageSpec(domain.ProbeImageModeEdits, "https://images.example/x.png"))
		}()
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("image fetch never started")
		}
		cancel()
		select {
		case res := <-result:
			require.Zero(t, res.StatusCode)
			require.Equal(t, domain.ProbeErrorCanceled, res.Error)
		case <-time.After(2 * time.Second):
			t.Fatal("probe did not return")
		}
		require.Zero(t, upstreamCalls.Load())
	})

	t.Run("deadline-during-fetch", func(t *testing.T) {
		var upstreamCalls atomic.Int64
		rt := contentRT(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host == "upstream.example" {
				upstreamCalls.Add(1)
				return contentResponse(http.StatusOK, imagesBody()), nil
			}
			<-req.Context().Done()
			return nil, req.Context().Err()
		})
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		res := newTestProber(t, rt).Probe(ctx, contentImageSpec(domain.ProbeImageModeEdits, "https://images.example/x.png"))
		require.Zero(t, res.StatusCode)
		require.Equal(t, domain.ProbeErrorTimedOut, res.Error)
		require.Zero(t, upstreamCalls.Load())
	})
}

// TestProbeCodexImages codex 族生图：SDK 固定端点、200-only、错误状态映射、
// edits 直传 ImageURL（不提前取图）、2 MiB 溢出与取消。
func TestProbeCodexImages(t *testing.T) {
	png := pngBytes(32)
	codexImageSpec := func(mode, imageURL string) domain.ProbeSpec {
		spec := contentImageSpec(mode, imageURL)
		spec.CredentialType = credential.TypeCodexPAT
		spec.Credential = "pat-test"
		return spec
	}

	t.Run("generations-success", func(t *testing.T) {
		var capturedPath, capturedBody string
		rt := contentRT(func(req *http.Request) (*http.Response, error) {
			capturedPath = req.URL.Path
			raw, _ := io.ReadAll(req.Body)
			capturedBody = string(raw)
			return contentResponse(http.StatusOK, imagesBody(b64(png))), nil
		})
		res := newTestProber(t, rt).Probe(context.Background(), codexImageSpec(domain.ProbeImageModeGenerations, ""))
		require.True(t, res.Success, res.Error)
		require.Equal(t, "/backend-api/codex/images/generations", capturedPath)
		require.Equal(t, "up-model", gjson.Get(capturedBody, "model").String())
		require.Equal(t, domain.ProbeDefaultInputImages, gjson.Get(capturedBody, "prompt").String())
		require.Len(t, res.ImagePreviews, 1)
	})

	t.Run("edits-image-url-passthrough", func(t *testing.T) {
		const dataURL = "data:image/png;base64,iVBORw0KGgo="
		var capturedPath, capturedBody string
		rt := contentRT(func(req *http.Request) (*http.Response, error) {
			capturedPath = req.URL.Path
			raw, _ := io.ReadAll(req.Body)
			capturedBody = string(raw)
			return contentResponse(http.StatusOK, imagesBody(b64(png))), nil
		})
		res := newTestProber(t, rt).Probe(context.Background(), codexImageSpec(domain.ProbeImageModeEdits, dataURL))
		require.True(t, res.Success, res.Error)
		require.Equal(t, "/backend-api/codex/images/edits", capturedPath)
		require.Contains(t, capturedBody, dataURL, "codex edits 直接把 image_url 交给 SDK")
	})

	matrix := []struct {
		name      string
		status    int
		body      string
		wantError string
	}{
		{"201", http.StatusCreated, imagesBody(b64(png)), domain.ProbeErrorUpstream},
		{"204", http.StatusNoContent, "", domain.ProbeErrorUpstream},
		{"206", http.StatusPartialContent, imagesBody(b64(png)), domain.ProbeErrorUpstream},
		{"401", http.StatusUnauthorized, `{"error":{"code":"token_revoked"}}`, domain.ProbeErrorUpstreamAuth},
		{"200-unusable", http.StatusOK, `{}`, domain.ProbeErrorNoUsableOutput},
		{"200-unparseable", http.StatusOK, `not-json`, domain.ProbeErrorNoUsableOutput},
	}
	for _, tc := range matrix {
		t.Run(tc.name, func(t *testing.T) {
			rt := contentRT(func(*http.Request) (*http.Response, error) {
				return contentResponse(tc.status, tc.body), nil
			})
			res := newTestProber(t, rt).Probe(context.Background(), codexImageSpec(domain.ProbeImageModeGenerations, ""))
			require.False(t, res.Success)
			require.Equal(t, tc.status, res.StatusCode)
			require.Equal(t, tc.wantError, res.Error)
			require.Empty(t, res.ImagePreviews)
		})
	}

	t.Run("overflow", func(t *testing.T) {
		huge := strings.Repeat("x", domain.ProbeMaxImageResponseBytes+16)
		rt := contentRT(func(*http.Request) (*http.Response, error) {
			return contentResponse(http.StatusOK, huge), nil
		})
		res := newTestProber(t, rt).Probe(context.Background(), codexImageSpec(domain.ProbeImageModeGenerations, ""))
		require.False(t, res.Success)
		require.Equal(t, http.StatusOK, res.StatusCode)
		require.Equal(t, domain.ProbeErrorImageBodyTooLarge, res.Error)
	})

	t.Run("cancel", func(t *testing.T) {
		rt := contentRT(func(*http.Request) (*http.Response, error) {
			t.Fatal("canceled probe must not hit upstream")
			return nil, nil
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		res := newTestProber(t, rt).Probe(ctx, codexImageSpec(domain.ProbeImageModeGenerations, ""))
		require.Equal(t, domain.ProbeErrorCanceled, res.Error)
		require.Zero(t, res.StatusCode)
	})
}

// TestProbeSearchStatic 静态族 search：固定 payload 形状、唯一 correlation id、
// output/web_search 预览与 URL 永不投影、状态矩阵。
func TestProbeSearchStatic(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var bodies [][]byte
	responses := []string{
		`{"status":"completed","output":"answer text","results":[{"web_search":{"title":"T","url":"https://secret.example/x","content":"C"}}]}`,
		`{"results":[{"web_search":{"title":"T1","url":"https://secret.example/one","content":"C1"}},{"web_search":{"title":"T2","content":"C2"}}]}`,
	}
	rt := contentRT(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		paths = append(paths, req.URL.Path)
		raw, _ := io.ReadAll(req.Body)
		bodies = append(bodies, raw)
		return contentResponse(http.StatusOK, responses[len(bodies)-1]), nil
	})
	prober := newTestProber(t, rt)

	first := prober.Probe(context.Background(), contentSearchSpec())
	require.True(t, first.Success, first.Error)
	require.Equal(t, "answer text", first.OutputPreview, "output 非空时只用它")
	require.NotContains(t, first.OutputPreview, "secret.example")

	second := prober.Probe(context.Background(), contentSearchSpec())
	require.True(t, second.Success, second.Error)
	require.Equal(t, "T1\nC1\nT2\nC2", second.OutputPreview, "output 为空时按 title/content 顺序 fallback")
	require.NotContains(t, second.OutputPreview, "secret.example", "results[].web_search.url 永不投影")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, bodies, 2)
	ids := make([]string, 0, 2)
	for i, raw := range bodies {
		require.Equal(t, "/v1/alpha/search", paths[i])
		require.Equal(t, "up-model", gjson.GetBytes(raw, "model").String())
		require.Equal(t, "OpenAI", gjson.GetBytes(raw, "input.0.request.queries.0").String())
		require.Equal(t, "web_search_call", gjson.GetBytes(raw, "input.0.type").String())
		require.True(t, gjson.GetBytes(raw, "commands").IsArray())
		require.True(t, gjson.GetBytes(raw, "settings").IsObject())
		require.Equal(t, int64(256), gjson.GetBytes(raw, "max_output_tokens").Int())
		id := gjson.GetBytes(raw, "id").String()
		require.Regexp(t, `^probe_[0-9a-f]{16}$`, id)
		ids = append(ids, id)
	}
	require.NotEqual(t, ids[0], ids[1], "每次探测必须使用唯一 id")

	matrix := []struct {
		status    int
		body      string
		wantError string
		detail    string
	}{
		{http.StatusCreated, `{"status":"completed","output":"x"}`, domain.ProbeErrorUpstream, ""},
		{http.StatusNoContent, "", domain.ProbeErrorUpstream, ""},
		{http.StatusPartialContent, `{"status":"completed"}`, domain.ProbeErrorUpstream, ""},
		{http.StatusBadRequest, `{"error":{"code":"invalid_api_key"}}`, domain.ProbeErrorUpstreamRejected, "code=invalid_api_key"},
		{http.StatusOK, "", domain.ProbeErrorNoUsableOutput, ""},
		{http.StatusOK, `{}`, domain.ProbeErrorNoUsableOutput, ""},
		{http.StatusOK, `{"results":[]}`, domain.ProbeErrorNoUsableOutput, ""},
		{http.StatusOK, `{"status":"incomplete"}`, domain.ProbeErrorNoUsableOutput, ""},
	}
	for _, tc := range matrix {
		t.Run(tc.body+"-"+http.StatusText(tc.status), func(t *testing.T) {
			rt := contentRT(func(*http.Request) (*http.Response, error) {
				return contentResponse(tc.status, tc.body), nil
			})
			res := newTestProber(t, rt).Probe(context.Background(), contentSearchSpec())
			require.False(t, res.Success)
			require.Equal(t, tc.status, res.StatusCode)
			require.Equal(t, tc.wantError, res.Error)
			require.Equal(t, tc.detail, res.Detail)
			require.Empty(t, res.OutputPreview)
		})
	}

	t.Run("overflow", func(t *testing.T) {
		huge := strings.Repeat("x", domain.ProbeMaxBodyBytes+16)
		rt := contentRT(func(*http.Request) (*http.Response, error) {
			return contentResponse(http.StatusOK, huge), nil
		})
		res := newTestProber(t, rt).Probe(context.Background(), contentSearchSpec())
		require.False(t, res.Success)
		require.Equal(t, http.StatusOK, res.StatusCode)
		require.Equal(t, domain.ProbeErrorBodyTooLarge, res.Error)
		require.Empty(t, res.OutputPreview)
	})
}

// TestProbeCodexSearch codex 族 search：SDK 固定端点、唯一 id、200-only、状态
// 矩阵与预览。
func TestProbeCodexSearch(t *testing.T) {
	codexSearchSpec := func() domain.ProbeSpec {
		spec := contentSearchSpec()
		spec.CredentialType = credential.TypeCodexPAT
		spec.Credential = "pat-test"
		return spec
	}
	var capturedPath string
	var capturedBody []byte
	rt := contentRT(func(req *http.Request) (*http.Response, error) {
		capturedPath = req.URL.Path
		capturedBody, _ = io.ReadAll(req.Body)
		return contentResponse(http.StatusOK, `{"status":"completed","output":"codex answer"}`), nil
	})
	res := newTestProber(t, rt).Probe(context.Background(), codexSearchSpec())
	require.True(t, res.Success, res.Error)
	require.Equal(t, "/backend-api/codex/alpha/search", capturedPath)
	require.Regexp(t, `^probe_[0-9a-f]{16}$`, gjson.GetBytes(capturedBody, "id").String())
	require.Equal(t, "up-model", gjson.GetBytes(capturedBody, "model").String())
	require.Equal(t, "OpenAI", gjson.GetBytes(capturedBody, "input.0.request.queries.0").String())
	require.Equal(t, "codex answer", res.OutputPreview)

	matrix := []struct {
		status    int
		body      string
		wantError string
	}{
		{http.StatusCreated, `{"status":"completed","output":"x"}`, domain.ProbeErrorUpstream},
		{http.StatusNoContent, "", domain.ProbeErrorUpstream},
		{http.StatusPartialContent, `{"status":"completed"}`, domain.ProbeErrorUpstream},
		{http.StatusUnauthorized, `{"error":{"code":"token_revoked"}}`, domain.ProbeErrorUpstreamAuth},
		{http.StatusOK, `{}`, domain.ProbeErrorNoUsableOutput},
	}
	for _, tc := range matrix {
		t.Run(http.StatusText(tc.status)+tc.body, func(t *testing.T) {
			rt := contentRT(func(*http.Request) (*http.Response, error) {
				return contentResponse(tc.status, tc.body), nil
			})
			res := newTestProber(t, rt).Probe(context.Background(), codexSearchSpec())
			require.False(t, res.Success)
			require.Equal(t, tc.status, res.StatusCode)
			require.Equal(t, tc.wantError, res.Error)
			require.Empty(t, res.OutputPreview)
		})
	}

	t.Run("overflow", func(t *testing.T) {
		huge := strings.Repeat("x", domain.ProbeMaxBodyBytes+16)
		rt := contentRT(func(*http.Request) (*http.Response, error) {
			return contentResponse(http.StatusOK, huge), nil
		})
		res := newTestProber(t, rt).Probe(context.Background(), codexSearchSpec())
		require.Equal(t, http.StatusOK, res.StatusCode)
		require.Equal(t, domain.ProbeErrorBodyTooLarge, res.Error)
	})

	t.Run("cancel", func(t *testing.T) {
		rt := contentRT(func(*http.Request) (*http.Response, error) {
			t.Fatal("canceled probe must not hit upstream")
			return nil, nil
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		res := newTestProber(t, rt).Probe(ctx, codexSearchSpec())
		require.Equal(t, domain.ProbeErrorCanceled, res.Error)
		require.Zero(t, res.StatusCode)
	})
}

// TestProbeDenseArraysDoNotMaterialize 2 MiB 内的密集数组不得调用 gjson.Array()：
// 每个元素 80 字节，60 万空对象会瞬态放大 ~48 MiB；预览/校验都必须流式 ForEach
// 或用 Get("#")/Get("0")。阈值 8 MiB 留足对象头/字符串开销，远低于旧实现。
func TestProbeDenseArraysDoNotMaterialize(t *testing.T) {
	const count = 600000
	entries := strings.Repeat(`{},`, count)
	cases := []struct {
		name string
		run  func()
	}{
		{"chat-preview", func() {
			_ = extractChatPreview(gjson.ParseBytes([]byte(`{"choices":[` + entries + `{"message":{"content":"ok"}}]}`)))
		}},
		{"validate-images", func() { _ = validateImages([]byte(`{"data":[` + entries + `{}]}`)) }},
		{"image-previews", func() { _ = imagePreviews([]byte(`{"data":[` + entries + `{}]}`)) }},
		{"validate-search", func() { _ = validateSearch([]byte(`{"results":[` + entries + `{}]}`)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			tc.run()
			runtime.ReadMemStats(&after)
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("%s dense array allocated %d bytes", tc.name, allocated)
			require.Less(t, allocated, uint64(8<<20), "密集数组不得物化为 []gjson.Result")
		})
	}
}

// TestProbeNewFormatErrorsCarryNoPreview 错误路径绝不回显上游 body（即使 body
// 里塞了看似 output 的字段）。
func TestProbeNewFormatErrorsCarryNoPreview(t *testing.T) {
	cases := []struct {
		format domain.RequestFormat
		body   string
	}{
		{domain.FormatOpenAIChat, `{"choices":[{"message":{"content":"leak"}}],"error":{"message":"boom"}}`},
		{domain.FormatOpenAIImages, `{"data":[{"b64_json":"aGVsbG8="}],"error":{"message":"boom"}}`},
		{domain.FormatOpenAISearch, `{"output":"leak","status":"completed","error":{"message":"boom"}}`},
	}
	for _, tc := range cases {
		t.Run(string(tc.format), func(t *testing.T) {
			rt := contentRT(func(*http.Request) (*http.Response, error) {
				return contentResponse(http.StatusBadRequest, tc.body), nil
			})
			spec := contentSpec(tc.format)
			if tc.format == domain.FormatOpenAIImages {
				spec = contentImageSpec(domain.ProbeImageModeGenerations, "")
			}
			if tc.format == domain.FormatOpenAISearch {
				spec = contentSearchSpec()
			}
			res := newTestProber(t, rt).Probe(context.Background(), spec)
			require.False(t, res.Success)
			require.Empty(t, res.OutputPreview)
			require.Empty(t, res.ImagePreviews)
		})
	}
}
