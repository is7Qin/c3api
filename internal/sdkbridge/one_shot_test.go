// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/sdkbridge"
)

type onceCapturedRequest struct {
	path string
	auth string
	body []byte
}

func imageSuccessBody(b64 string) string {
	raw, _ := json.Marshal(map[string]any{
		"created": 1,
		"data":    []map[string]any{{"b64_json": b64}},
	})
	return string(raw)
}

func generateImageParams() *domain.ImageGenParams {
	return &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "a red apple"}
}

// TestGenerateImageOnceSuccess 200 成功：typed success 的真实状态 + wire body、
// 官方 generations 路径、Bearer、恰好一次请求与 body close-once。
func TestGenerateImageOnceSuccess(t *testing.T) {
	var captured onceCapturedRequest
	var requests atomic.Int64
	body := &countingBody{Reader: strings.NewReader(imageSuccessBody("aGVsbG8="))}
	tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		raw, _ := io.ReadAll(req.Body)
		captured = onceCapturedRequest{path: req.URL.Path, auth: rawAuthorization(req.Header), body: raw}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
	})
	status, raw, err := sdkbridge.GenerateImageOnce(context.Background(), "pat-x", "acct", tr, generateImageParams())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, string(raw), `"b64_json":"aGVsbG8="`)
	require.Equal(t, "/backend-api/codex/images/generations", captured.path)
	require.Equal(t, "Bearer pat-x", captured.auth)
	require.Contains(t, string(captured.body), `"prompt":"a red apple"`)
	require.Contains(t, string(captured.body), `"model":"gpt-image-2"`)
	require.Equal(t, int64(1), requests.Load(), "必须恰好一次请求")
	require.Equal(t, int64(1), body.closes.Load(), "body 必须恰好关闭一次")
}

// TestGenerateImageOnceLargeBody 生图 b64_json 正常超过旧 2 MiB 文本上限，但
// 低于 images 8 MiB 上限时必须成功。
func TestGenerateImageOnceLargeBody(t *testing.T) {
	large := strings.Repeat("A", domain.ProbeMaxBodyBytes)
	tr := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(imageSuccessBody(large)))}, nil
	})
	status, raw, err := sdkbridge.GenerateImageOnce(context.Background(), "pat", "acct", tr, generateImageParams())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, string(raw), large)
}

// TestGenerateImageOnceEditsUsesEditsPath edits 走固定官方 edits 端点，ImageURL
// 原样进入请求体（codex 族不提前取图）。
func TestGenerateImageOnceEditsUsesEditsPath(t *testing.T) {
	var captured onceCapturedRequest
	const dataURL = "data:image/png;base64,iVBORw0KGgo="
	tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		captured = onceCapturedRequest{path: req.URL.Path, body: raw}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(imageSuccessBody("aGVsbG8=")))}, nil
	})
	params := generateImageParams()
	params.Images = []domain.ImageRef{{ImageURL: ptr(dataURL)}}
	status, _, err := sdkbridge.GenerateImageOnce(context.Background(), "pat", "acct", tr, params)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "/backend-api/codex/images/edits", captured.path)
	require.Contains(t, string(captured.body), dataURL)
}

func ptr[T any](v T) *T { return &v }

// TestGenerateImageOnceTypedSuccessKeepsRealStatus 201 也能被 SDK 解析成 typed
// success——seam 必须返回真实捕获状态，而不是默认 200。
func TestGenerateImageOnceTypedSuccessKeepsRealStatus(t *testing.T) {
	for _, status := range []int{http.StatusCreated, http.StatusPartialContent} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			tr := roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader(imageSuccessBody("aGVsbG8=")))}, nil
			})
			gotStatus, raw, err := sdkbridge.GenerateImageOnce(context.Background(), "pat", "acct", tr, generateImageParams())
			require.NoError(t, err)
			require.Equal(t, status, gotStatus)
			require.NotEmpty(t, raw)
		})
	}
}

// TestGenerateImageOnceNon200StatusAndRaw 非 2xx 返回真实状态 + raw。
func TestGenerateImageOnceNon200StatusAndRaw(t *testing.T) {
	tr := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"error":{"code":"bad"}}`))}, nil
	})
	status, raw, err := sdkbridge.GenerateImageOnce(context.Background(), "pat", "acct", tr, generateImageParams())
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, status)
	require.JSONEq(t, `{"error":{"code":"bad"}}`, string(raw))
}

// TestGenerateImageOnceRevoked401 401 判死码 → 401 + raw（一次请求，无轮转）。
func TestGenerateImageOnceRevoked401(t *testing.T) {
	var requests atomic.Int64
	tr := roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"error":{"code":"token_revoked"}}`))}, nil
	})
	status, raw, err := sdkbridge.GenerateImageOnce(context.Background(), "pat", "acct", tr, generateImageParams())
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, status)
	require.Contains(t, string(raw), "token_revoked")
	require.Equal(t, int64(1), requests.Load())
}

// TestGenerateImageOnceNonFatal401ExactlyOneRequest 非判死 401 不得重试/轮转。
func TestGenerateImageOnceNonFatal401ExactlyOneRequest(t *testing.T) {
	var requests atomic.Int64
	body := &countingBody{Reader: strings.NewReader(`{"error":{"message":"expired"}}`)}
	tr := roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: body}, nil
	})
	status, raw, err := sdkbridge.GenerateImageOnce(context.Background(), "pat", "acct", tr, generateImageParams())
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, status)
	require.Contains(t, string(raw), "expired")
	require.Equal(t, int64(1), requests.Load(), "PAT 无轮转：必须恰好一次请求")
	require.Equal(t, int64(1), body.closes.Load(), "401 body 也必须恰好关闭一次")
}

// TestGenerateImageOnceOverflow 200 解析失败且读出 >8 MiB → ErrBodyTooLarge。
func TestGenerateImageOnceOverflow(t *testing.T) {
	huge := strings.Repeat("x", domain.ProbeMaxImageResponseBytes+1)
	tr := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(huge))}, nil
	})
	status, body, err := sdkbridge.GenerateImageOnce(context.Background(), "pat", "acct", tr, generateImageParams())
	require.ErrorIs(t, err, sdkbridge.ErrBodyTooLarge)
	require.Equal(t, http.StatusOK, status)
	require.Nil(t, body)
}

// TestGenerateImageOnceCancel ctx 取消优先，不产生伪状态。
func TestGenerateImageOnceCancel(t *testing.T) {
	tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, req.Context().Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	status, body, err := sdkbridge.GenerateImageOnce(ctx, "pat", "acct", tr, generateImageParams())
	require.Zero(t, status)
	require.Nil(t, body)
	require.ErrorIs(t, err, context.Canceled)
}

// TestGenerateImageOnceNoAccountStateMutation 两次判死 401 各自发一次请求：
// 临时 PAT 不缓存 fatal 状态，第二次不被本地毒化拦下。
func TestGenerateImageOnceNoAccountStateMutation(t *testing.T) {
	var requests atomic.Int64
	tr := roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"error":{"code":"token_revoked"}}`))}, nil
	})
	for i := 0; i < 2; i++ {
		status, _, err := sdkbridge.GenerateImageOnce(context.Background(), "pat", "acct", tr, generateImageParams())
		require.NoError(t, err)
		require.Equal(t, http.StatusUnauthorized, status)
	}
	require.Equal(t, int64(2), requests.Load(), "两次探测必须各发一次请求（无共享 fatal/缓存状态）")
}

func testSearchPayload() []byte {
	return []byte(`{"id":"probe_0123456789abcdef","model":"m","input":[{"type":"web_search_call","request":{"queries":["q"]}}],"commands":[],"settings":{},"max_output_tokens":256}`)
}

// TestSearchOnceSuccess 200 成功：真实状态 + raw、官方 search 路径、Bearer、
// 请求体原样、恰好一次请求与 close-once。
func TestSearchOnceSuccess(t *testing.T) {
	var captured onceCapturedRequest
	var requests atomic.Int64
	body := &countingBody{Reader: strings.NewReader(`{"status":"completed","output":"answer"}`)}
	payload := testSearchPayload()
	tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		raw, _ := io.ReadAll(req.Body)
		captured = onceCapturedRequest{path: req.URL.Path, auth: rawAuthorization(req.Header), body: raw}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
	})
	status, raw, err := sdkbridge.SearchOnce(context.Background(), "pat", "acct", tr, payload)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.JSONEq(t, `{"status":"completed","output":"answer"}`, string(raw))
	require.Equal(t, "/backend-api/codex/alpha/search", captured.path)
	require.Equal(t, "Bearer pat", captured.auth)
	require.Equal(t, payload, captured.body)
	require.Equal(t, int64(1), requests.Load())
	require.Equal(t, int64(1), body.closes.Load())
}

// TestSearchOnceNon200StatusAndRaw 400 返回真实状态 + raw；201/206 也原样返回。
func TestSearchOnceNon200StatusAndRaw(t *testing.T) {
	for _, tc := range []struct {
		status int
		raw    string
	}{
		{http.StatusBadRequest, `{"error":{"code":"invalid_request_error"}}`},
		{http.StatusCreated, `{"ok":true}`},
		{http.StatusPartialContent, `{"ok":true}`},
		{http.StatusNoContent, ``},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			body := &countingBody{Reader: strings.NewReader(tc.raw)}
			tr := roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: body}, nil
			})
			status, raw, err := sdkbridge.SearchOnce(context.Background(), "pat", "acct", tr, testSearchPayload())
			require.NoError(t, err)
			require.Equal(t, tc.status, status)
			require.Equal(t, tc.raw, string(raw))
			require.Equal(t, int64(1), body.closes.Load())
		})
	}
}

// TestSearchOnceRevoked401NonFatalAndExactOneRequest 401 判死 → 401 + raw；
// 非判死 401 也不重试（PAT 无轮转）。
func TestSearchOnceRevoked401NonFatalAndExactOneRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"revoked", `{"error":{"code":"token_revoked"}}`},
		{"non-fatal", `{"error":{"message":"expired"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int64
			tr := roundTripFunc(func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			status, raw, err := sdkbridge.SearchOnce(context.Background(), "pat", "acct", tr, testSearchPayload())
			require.NoError(t, err)
			require.Equal(t, http.StatusUnauthorized, status)
			require.Equal(t, tc.body, string(raw))
			require.Equal(t, int64(1), requests.Load())
		})
	}
}

// TestSearchOnceOverflow 200 且 len >2 MiB → ErrBodyTooLarge；恰好 2 MiB 非 200
// 不判溢出（调用方 cap 的既成语义）。
func TestSearchOnceOverflow(t *testing.T) {
	t.Run("200-overflow", func(t *testing.T) {
		huge := strings.Repeat("x", domain.ProbeMaxBodyBytes+1)
		tr := roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(huge))}, nil
		})
		status, body, err := sdkbridge.SearchOnce(context.Background(), "pat", "acct", tr, testSearchPayload())
		require.ErrorIs(t, err, sdkbridge.ErrBodyTooLarge)
		require.Equal(t, http.StatusOK, status)
		require.Nil(t, body)
	})
	t.Run("non-200-exact-cap-not-overflow", func(t *testing.T) {
		capped := strings.Repeat("x", domain.ProbeMaxBodyBytes)
		tr := roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(capped))}, nil
		})
		status, body, err := sdkbridge.SearchOnce(context.Background(), "pat", "acct", tr, testSearchPayload())
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, status)
		require.Len(t, body, domain.ProbeMaxBodyBytes)
	})
}

// TestSearchOnceCancel ctx 取消优先。
func TestSearchOnceCancel(t *testing.T) {
	tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, req.Context().Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	status, body, err := sdkbridge.SearchOnce(ctx, "pat", "acct", tr, testSearchPayload())
	require.Zero(t, status)
	require.Nil(t, body)
	require.ErrorIs(t, err, context.Canceled)
}

// TestSearchOnceNoAccountStateMutation 两次调用互不影响。
func TestSearchOnceNoAccountStateMutation(t *testing.T) {
	var requests atomic.Int64
	tr := roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"error":{"code":"token_revoked"}}`))}, nil
	})
	for i := 0; i < 2; i++ {
		status, _, err := sdkbridge.SearchOnce(context.Background(), "pat", "acct", tr, testSearchPayload())
		require.NoError(t, err)
		require.Equal(t, http.StatusUnauthorized, status)
	}
	require.Equal(t, int64(2), requests.Load())
}

// TestOneShotNilTransport nil transport 必须显式报错而非 panic。
func TestOneShotNilTransport(t *testing.T) {
	_, _, err := sdkbridge.GenerateImageOnce(context.Background(), "pat", "acct", nil, generateImageParams())
	require.Error(t, err)
	_, _, err = sdkbridge.SearchOnce(context.Background(), "pat", "acct", nil, testSearchPayload())
	require.Error(t, err)
	require.False(t, errors.Is(err, sdkbridge.ErrBodyTooLarge))
}

// rawAuthorization 读出站请求头里 SDK 实际写入的鉴权值：SDK 用小写名直接赋值
// （`h["authorization"]`，不经 Header.Set 规范化），RoundTripper 这一侧看到的是原始
// map 键，`Header.Get` 读不到。只认小写键——断言的正是新形态。
func rawAuthorization(h http.Header) string {
	if v := h["authorization"]; len(v) > 0 {
		return v[0]
	}
	return ""
}
