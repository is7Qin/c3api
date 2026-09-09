// SPDX-License-Identifier: AGPL-3.0-or-later
package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/billing"
)

func TestNextResponseID_isUnique(t *testing.T) {
	responsePrefix = "rsp"
	responseSequence.Store(0)

	first := nextResponseID()
	second := nextResponseID()

	require.NotEqual(t, first, second)
	require.Equal(t, "rsp_1", first)
	require.Equal(t, "rsp_2", second)
}

func TestUpstreamKey_acceptsAnthropicAPIKey(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("x-api-key", "sk-anthropic")

	require.Equal(t, "sk-anthropic", upstreamKey(req))
}

func TestModelsHandler_returnsHealthyResponse(t *testing.T) {
	response := httptest.NewRecorder()
	modelsHandler(response, httptest.NewRequest("GET", "/v1/models", nil))

	require.Equal(t, 200, response.Code)
	require.Contains(t, response.Body.String(), `"object":"list"`)
}

// sseDataLines 提取 SSE 响应里每个事件的 data 载荷（帧间以空行分隔）。
func sseDataLines(body string) []string {
	var out []string
	for _, frame := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(frame, "\n") {
			if v, ok := strings.CutPrefix(line, "data: "); ok {
				out = append(out, v)
			}
		}
	}
	return out
}

// TestImagesHandler_streamEmitsGatewayCompatibleSSE stream:true → 每张图一个
// image_generation.completed data 帧（帧首键恒为 type——网关 billing.ImageStreamEvent
// 锚定 `{"type":"` 判定，键序错 = 计 0 张）+ 末帧 usage image tokens + [DONE] 终帧。
// 兼容性用网关自己的提取函数逐帧证明（同模块 internal 导入，仅测试面）。
func TestImagesHandler_streamEmitsGatewayCompatibleSSE(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"gpt-image-1","prompt":"cat","n":2,"stream":true}`))
	rec := httptest.NewRecorder()
	imagesHandler(rec, req, imagesOpts{})

	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "text/event-stream")
	frames := sseDataLines(rec.Body.String())
	require.Len(t, frames, 3, "2 张 completed + [DONE]")

	completed, ii, io := billing.ImageStreamEvent([]byte(frames[0]))
	require.True(t, completed, "首帧必须被网关计费提取识别")
	require.Equal(t, int64(0), ii)
	require.Equal(t, int64(0), io)

	completed, ii, io = billing.ImageStreamEvent([]byte(frames[1]))
	require.True(t, completed)
	require.Equal(t, int64(10), ii, "末帧 usage.input_tokens_details.image_tokens")
	require.Equal(t, int64(20), io, "末帧 usage.output_tokens_details.image_tokens")

	require.Equal(t, "[DONE]", frames[2], "终帧与网关 SSE 透传口径一致")
}

// TestImagesHandler_nonStreamPreservesJSON 非流式契约不变：data 数组按 n 回显
// （网关按长度计张数），无 SSE 头。
func TestImagesHandler_nonStreamPreservesJSON(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"gpt-image-1","prompt":"cat","n":3}`))
	rec := httptest.NewRecorder()
	imagesHandler(rec, req, imagesOpts{})

	require.Equal(t, 200, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	ii, io, count := billing.ImageUsageFromResponse(rec.Body.Bytes())
	require.Equal(t, int64(3), count)
	require.Equal(t, int64(0), ii)
	require.Equal(t, int64(0), io)
}

// TestImagesHandler_injectionStillAppliesToStream 400/429/500 注入面在流式
// 请求上同样生效（注入判定先于 stream 分支）。
func TestImagesHandler_injectionStillAppliesToStream(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"gpt-image-1","prompt":"cat","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-bad")
	rec := httptest.NewRecorder()
	imagesHandler(rec, req, imagesOpts{f429: map[string]bool{"sk-bad": true}})

	require.Equal(t, 429, rec.Code)
	require.NotContains(t, rec.Body.String(), "image_generation.completed")
}

// TestImagesHandler_streamLatencyPaced latency>0 时逐帧限速（压测长流形态）；
// 0 = 不限速（单测不睡）。
func TestImagesHandler_streamLatencyPaced(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"gpt-image-1","prompt":"cat","n":2,"stream":true}`))
	rec := httptest.NewRecorder()
	start := time.Now()
	imagesHandler(rec, req, imagesOpts{latency: 20 * time.Millisecond})
	require.GreaterOrEqual(t, time.Since(start), 40*time.Millisecond)
}
