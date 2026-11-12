// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package probe

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
)

// TestSanitizePreview 对抗样本：任意 scheme/mailto/data/www、Bearer、常见
// key/ID/UUID 全大小写不敏感替换；普通文本不动。
func TestSanitizePreview(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "schemes",
			in:   "a https://evil.example/x?y=1 b FTP://files.example/z CUSTOM+v1.2://host/path",
			want: "a [url] b [url] [url]",
		},
		{
			name: "mailto-data-www",
			in:   "MailTo:user@example.com then DATA:text/plain;base64,AAAA and WWW.evil.example/p",
			want: "[url] then [url] and [url]",
		},
		{
			name: "bearer",
			in:   "header bEaReR abcDEF123 end",
			want: "header Bearer [redacted] end",
		},
		{
			name: "keys",
			in:   "sk-abcdef123456 pk-AAAAAA mk-123456 pat-zzzzzz rk-qqqqqq ak-999999",
			want: "[redacted] [redacted] [redacted] [redacted] [redacted] [redacted]",
		},
		{
			name: "ids",
			in:   "req_abcd resp_1234 msg_zzzz chatcmpl_abcd call_1234 fc_abcd toolu_abcd run_abcd thread_abcd asst_abcd file_abcd img_abcd igc_abcd",
			want: "[redacted-id] [redacted-id] [redacted-id] [redacted-id] [redacted-id] [redacted-id] [redacted-id] [redacted-id] [redacted-id] [redacted-id] [redacted-id] [redacted-id] [redacted-id]",
		},
		{
			name: "uuid-uppercase",
			in:   "id 550E8400-E29B-41D4-A716-446655440000 done",
			want: "id [redacted-id] done",
		},
		{
			name: "ordinary-text-untouched",
			in:   "The quick brown fox jumps over 13 lazy dogs.",
			want: "The quick brown fox jumps over 13 lazy dogs.",
		},
		{
			name: "too-short-patterns-untouched",
			in:   "req_abc sk-12345",
			want: "req_abc sk-12345",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, sanitizePreview(tc.in))
		})
	}
}

// TestProbePreviewSanitizedAndNoEnvelopeLeak 成功文本经已知形态脱敏；未抽取的
// 上游字段（id/URL/原始 JSON）不进入结果。
func TestProbePreviewSanitizedAndNoEnvelopeLeak(t *testing.T) {
	const secretURL = "https://tenant.example/private?token=abc"
	body := `{"id":"req_leak1234","choices":[{"message":{"content":"visit ` + secretURL +
		` with Bearer abcDEF123 and sk-abcdef123456 id 550e8400-e29b-41d4-a716-446655440000"}}]}`
	rt := contentRT(func(*http.Request) (*http.Response, error) {
		return contentResponse(http.StatusOK, body), nil
	})
	res := newTestProber(t, rt).Probe(context.Background(), contentSpec(domain.FormatOpenAIChat))
	require.True(t, res.Success)
	require.NotContains(t, res.OutputPreview, "tenant.example")
	require.NotContains(t, res.OutputPreview, "abcDEF123")
	require.NotContains(t, res.OutputPreview, "sk-abcdef123456")
	require.NotContains(t, res.OutputPreview, "550e8400-e29b-41d4-a716-446655440000")
	require.NotContains(t, res.OutputPreview, "req_leak1234")
	require.Contains(t, res.OutputPreview, "[url]")
	require.Contains(t, res.OutputPreview, "Bearer [redacted]")
	require.Contains(t, res.OutputPreview, "[redacted]")
	require.Contains(t, res.OutputPreview, "[redacted-id]")
	require.NotContains(t, res.OutputPreview, `"choices"`, "原始 JSON 不得回显")
}

// TestProbePreviewTruncation 64 KiB UTF-8 边界截断（先脱敏后截断）。
func TestProbePreviewTruncation(t *testing.T) {
	t.Run("ascii", func(t *testing.T) {
		content := strings.Repeat("a", domain.ProbePreviewMaxLen+1024)
		rt := contentRT(func(*http.Request) (*http.Response, error) {
			return contentResponse(http.StatusOK, `{"choices":[{"message":{"content":"`+content+`"}}]}`), nil
		})
		res := newTestProber(t, rt).Probe(context.Background(), contentSpec(domain.FormatOpenAIChat))
		require.True(t, res.Success)
		require.Len(t, res.OutputPreview, domain.ProbePreviewMaxLen)
	})

	t.Run("multibyte-boundary", func(t *testing.T) {
		content := strings.Repeat("界", domain.ProbePreviewMaxLen/3+2) // 3 字节 rune，越过边界
		rt := contentRT(func(*http.Request) (*http.Response, error) {
			return contentResponse(http.StatusOK, `{"choices":[{"message":{"content":"`+content+`"}}]}`), nil
		})
		res := newTestProber(t, rt).Probe(context.Background(), contentSpec(domain.FormatOpenAIChat))
		require.True(t, res.Success)
		require.LessOrEqual(t, len(res.OutputPreview), domain.ProbePreviewMaxLen)
		require.True(t, utf8.ValidString(res.OutputPreview), "截断必须落在 UTF-8 边界")
		require.Equal(t, domain.ProbePreviewMaxLen/3*3, len(res.OutputPreview))
	})
}

// imagePreviewBody 是 imagePreviews 的直测 payload（不经过 2 MiB 响应体上限，
// 用于验证单张/合计/张数上限）。
func imagePreviewBody(values ...string) []byte {
	items := make([]string, 0, len(values))
	for _, value := range values {
		items = append(items, `{"b64_json":"`+value+`"}`)
	}
	return []byte(`{"data":[` + strings.Join(items, ",") + `]}`)
}

// TestImagePreviewsCaps base64+魔数校验与单张/合计/张数上限；只取 b64_json。
func TestImagePreviewsCaps(t *testing.T) {
	png := pngBytes(64)
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0x00, 0x01}
	webp := append([]byte("RIFF\x00\x00\x00\x00WEBP"), 0x01, 0x02)
	t.Run("magic-and-base64", func(t *testing.T) {
		previews := imagePreviews(imagePreviewBody(
			b64(png), b64(jpeg), b64(webp),
			"!!!not-base64!!!", b64([]byte("plain-text")), "",
		))
		require.Len(t, previews, 3)
		require.True(t, strings.HasPrefix(previews[0], "data:image/png;base64,"))
		require.True(t, strings.HasPrefix(previews[1], "data:image/jpeg;base64,"))
		require.True(t, strings.HasPrefix(previews[2], "data:image/webp;base64,"))
	})

	t.Run("max-four-previews", func(t *testing.T) {
		values := make([]string, 5)
		for i := range values {
			values[i] = b64(png)
		}
		require.Len(t, imagePreviews(imagePreviewBody(values...)), domain.ProbeMaxImagePreviews)
	})

	t.Run("single-decoded-cap", func(t *testing.T) {
		oversized := pngBytes(domain.ProbeMaxBodyBytes + 1)
		require.Empty(t, imagePreviews(imagePreviewBody(b64(oversized))))
	})

	t.Run("aggregate-cap", func(t *testing.T) {
		chunk := pngBytes(domain.ProbeMaxImageBytes/3 + 1) // 3 张合计 >4 MiB
		previews := imagePreviews(imagePreviewBody(b64(chunk), b64(chunk), b64(chunk)))
		require.Len(t, previews, 2, "第三张会越过 4 MiB 合计上限，必须丢弃")
	})

	t.Run("non-b64-fields-ignored", func(t *testing.T) {
		body := []byte(`{"data":[{"url":"https://x"},{"b64_json":null}],"output_format":"image/png"}`)
		require.Empty(t, imagePreviews(body))
	})
}

// TestSearchProbeIDSeed 确定性 seed 与 rand 失败回退。
func TestSearchProbeIDSeed(t *testing.T) {
	raw := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	require.Equal(t, binary.LittleEndian.Uint64(raw), newProbeIDSeedFrom(bytes.NewReader(raw)))

	seed := newProbeIDSeedFrom(errReader{})
	require.NotZero(t, seed, "rand 失败必须回退到纳秒时间戳（非零）")
	require.Greater(t, seed, uint64(0))
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("rand failed") }

// TestSearchProbeIDConcurrentUnique 并发生成 id 固定 probe_<16 hex> 且互不重复。
func TestSearchProbeIDConcurrentUnique(t *testing.T) {
	const n = 128
	ids := make([]string, n)
	payloads := make([][]byte, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload := searchProbePayload("m", "q")
			payloads[i] = payload
			ids[i] = gjson.GetBytes(payload, "id").String()
		}(i)
	}
	wg.Wait()
	seen := make(map[string]struct{}, n)
	for _, id := range ids {
		require.Regexp(t, `^probe_[0-9a-f]{16}$`, id)
		_, dup := seen[id]
		require.False(t, dup, "重复 id: %s", id)
		seen[id] = struct{}{}
	}
}

// TestProbePanicFreeOnEmptyInput 防御性用例：spec.Input 为空时 prober 不 panic
// （service 恒填默认值；这里只锁定执行面契约）。
func TestProbePanicFreeOnEmptyInput(t *testing.T) {
	rt := contentRT(func(*http.Request) (*http.Response, error) {
		return contentResponse(http.StatusOK, `{"choices":[{"message":{"content":"ok"}}]}`), nil
	})
	spec := contentSpec(domain.FormatOpenAIChat)
	spec.Input = ""
	res := newTestProber(t, rt).Probe(context.Background(), spec)
	require.True(t, res.Success)
}

// TestNewProbeIDSeedFromShortReader 短读（n<8）也走回退路径。
func TestNewProbeIDSeedFromShortReader(t *testing.T) {
	seed := newProbeIDSeedFrom(strings.NewReader("abc"))
	require.NotZero(t, seed)
}

// TestProbeSearchPayloadShapeFixed 固定字段形状（含 id 16 hex）不随外部变化。
func TestProbeSearchPayloadShapeFixed(t *testing.T) {
	payload := searchProbePayload("up-model", "query text")
	require.True(t, gjson.ValidBytes(payload))
	require.Equal(t, "up-model", gjson.GetBytes(payload, "model").String())
	require.Equal(t, "query text", gjson.GetBytes(payload, "input.0.request.queries.0").String())
	require.Equal(t, "web_search_call", gjson.GetBytes(payload, "input.0.type").String())
	require.Equal(t, "[]", gjson.GetBytes(payload, "commands").Raw)
	require.Equal(t, "{}", gjson.GetBytes(payload, "settings").Raw)
	require.Equal(t, int64(256), gjson.GetBytes(payload, "max_output_tokens").Int())
	require.Regexp(t, `^probe_[0-9a-f]{16}$`, gjson.GetBytes(payload, "id").String())
}

// TestProbeSearchIDVariesPerProbe 同一 prober 的连续探测 id 不同（counter 单调）。
func TestProbeSearchIDVariesPerProbe(t *testing.T) {
	var mu sync.Mutex
	var ids []string
	rt := contentRT(func(req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		mu.Lock()
		ids = append(ids, gjson.GetBytes(raw, "id").String())
		mu.Unlock()
		return contentResponse(http.StatusOK, `{"status":"completed","output":"x"}`), nil
	})
	prober := newTestProber(t, rt)
	for i := 0; i < 3; i++ {
		res := prober.Probe(context.Background(), contentSearchSpec())
		require.True(t, res.Success)
	}
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, ids, 3)
	seen := map[string]struct{}{}
	for _, id := range ids {
		_, dup := seen[id]
		require.False(t, dup)
		seen[id] = struct{}{}
	}
}

// TestProbeImagePreviewTimeoutsDoNotUseDefaultClient 确保取图 client 从出口克隆，
// 不在请求上叠加 SDK 默认头。此用例以 header 断言为主（见 content_test）。
func TestProbeImageFetchHeaderAndLatency(t *testing.T) {
	png := pngBytes(16)
	var imageSeen bool
	rt := contentRT(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "images.example" {
			imageSeen = true
			time.Sleep(5 * time.Millisecond) // latency 含取图
			return contentResponseBytes(http.StatusOK, png, http.Header{"Content-Type": {"image/png"}}), nil
		}
		return contentResponse(http.StatusOK, imagesBody(b64(png))), nil
	})
	res := newTestProber(t, rt).Probe(context.Background(),
		contentImageSpec(domain.ProbeImageModeEdits, "https://images.example/x.png"))
	require.True(t, imageSeen)
	require.True(t, res.Success)
	require.GreaterOrEqual(t, res.LatencyMS, int64(0))
}

// TestProbeImagesNoUpstreamWhenFetchInvalid 取图失败时不得调用上游 images 端点。
func TestProbeImagesNoUpstreamWhenFetchInvalid(t *testing.T) {
	var upstream atomic.Int64
	rt := contentRT(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "upstream.example" {
			upstream.Add(1)
		}
		return contentResponseBytes(http.StatusOK, []byte("not image"), http.Header{"Content-Type": {"text/plain"}}), nil
	})
	res := newTestProber(t, rt).Probe(context.Background(),
		contentImageSpec(domain.ProbeImageModeEdits, "https://images.example/x.png"))
	require.Equal(t, domain.ProbeErrorImageInvalid, res.Error)
	require.Zero(t, upstream.Load())
}

// TestProbeSearchPreviewSanitized search 的 output / web_search content 同样
// 走唯一脱敏器；results[].web_search.url 不参与投影。
func TestProbeSearchPreviewSanitized(t *testing.T) {
	body := `{"status":"completed","output":"see https://tenant.example/x and Bearer abcDEF123",` +
		`"results":[{"web_search":{"title":"t","url":"https://tenant.example/u","content":"sk-abcdef123456"}}]}`
	rt := contentRT(func(*http.Request) (*http.Response, error) {
		return contentResponse(http.StatusOK, body), nil
	})
	res := newTestProber(t, rt).Probe(context.Background(), contentSearchSpec())
	require.True(t, res.Success)
	require.NotContains(t, res.OutputPreview, "tenant.example")
	require.NotContains(t, res.OutputPreview, "abcDEF123")
	require.Contains(t, res.OutputPreview, "[url]")
	require.Contains(t, res.OutputPreview, "Bearer [redacted]")

	fallback := `{"results":[{"web_search":{"title":"T","url":"https://tenant.example/u","content":"mailto:a@b.example"}}]}`
	rt = contentRT(func(*http.Request) (*http.Response, error) {
		return contentResponse(http.StatusOK, fallback), nil
	})
	res = newTestProber(t, rt).Probe(context.Background(), contentSearchSpec())
	require.True(t, res.Success)
	require.NotContains(t, res.OutputPreview, "tenant.example")
	require.NotContains(t, res.OutputPreview, "a@b.example")
	require.Contains(t, res.OutputPreview, "[url]")
}
