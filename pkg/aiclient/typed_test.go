// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package aiclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/responses"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// typedUpstream 三格式 typed 入口共用的 mock 上游：按 openai 系补 /v1、anthropic
// 自带 v1/ 前缀的裸根约定分发路径，每种返回该格式的最小合法 JSON。
func typedUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
		case "/v1/responses":
			_, _ = w.Write([]byte(`{"id":"r1","object":"response","created_at":1,"model":"gpt-4o","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
		case "/v1/messages":
			_, _ = w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","model":"claude-3-5-sonnet","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// typedIn 入站头构造纪律（§1「假绿」那条）：一律规范形键。
func typedIn() http.Header {
	return http.Header{
		"User-Agent":         {"curl/8.7.1"},
		"X-Opencode-Session": {"oc-1"},
		"Accept-Encoding":    {"gzip"},
		"Cookie":             {"tenant=leak"},
		"Authorization":      {"Bearer client-side-gateway-key"},
	}
}

// TestTypedRelayClientHeadersArrive R6（typed 面）：非空客户端 UA 经 WithHeader
// 压过 SDK 默认（openai `OpenAI/Go …` / anthropic `Anthropic/Go …`），自定义头
// 如实达上游，deny 头不出栈，账号鉴权头在 relay 之后写（不变量 #5 后写覆盖赢）。
//
// 断言打在网关自建的出栈头（headerCapture RoundTripper）上：SDK 的 New 最终走的
// 是注入的 http.Client，但 net/http 的 Request.write 会在 RoundTripper 之后再补
// User-Agent/Accept-Encoding，httptest 服务端视图永远看得到它们（C2 实测教训）。
func TestTypedRelayClientHeadersArrive(t *testing.T) {
	srv := typedUpstream(t)
	defer srv.Close()
	capRt := &headerCapture{base: srv.Client().Transport}
	// UpstreamTimeout 必须非零：typed 入口用它包 ctx，零值 = 立即过期。
	f := NewFactory(&http.Client{Transport: capRt}, Config{UpstreamTimeout: 5 * time.Second})
	tpl := &domain.Template{ID: 1, BaseURL: srv.URL}

	t.Run("chat", func(t *testing.T) {
		_, err := f.ChatCompletion(context.Background(), tpl, "sk-chat", openai.ChatCompletionNewParams{
			Model:    "gpt-4o",
			Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("x")},
		}, typedIn())
		require.NoError(t, err)
		requireTypedRelay(t, capRt.headers(t), "Bearer sk-chat")
	})

	t.Run("responses", func(t *testing.T) {
		_, err := f.Response(context.Background(), tpl, "sk-resp", responses.ResponseNewParams{
			Model: "gpt-4o",
			Input: responses.ResponseNewParamsInputUnion{OfString: openai.String("x")},
		}, typedIn())
		require.NoError(t, err)
		requireTypedRelay(t, capRt.headers(t), "Bearer sk-resp")
	})

	t.Run("anthropic", func(t *testing.T) {
		_, err := f.AnthMessage(context.Background(), tpl, "sk-anth", anthropic.MessageNewParams{
			Model:     "claude-3-5-sonnet",
			MaxTokens: 16,
			Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("x"))},
		}, typedIn())
		require.NoError(t, err)
		built := capRt.headers(t)
		require.Equal(t, "curl/8.7.1", built.Get("User-Agent"), "客户端 UA 压过 anthropic SDK 默认")
		require.Equal(t, []string{"oc-1"}, built["X-Opencode-Session"], "自定义 session 头如实达上游")
		_, ok := built["Accept-Encoding"]
		require.False(t, ok, "Accept-Encoding 不得出栈（deny＝计费回归门）")
		_, ok = built["Cookie"]
		require.False(t, ok, "Cookie（跨租户态）不得出栈")
		require.Equal(t, "sk-anth", built.Get("x-api-key"), "anthropic 面账号鉴权头 = x-api-key（后写赢）")
		_, ok = built["Authorization"]
		require.False(t, ok, "客户端 Authorization 被剔后 anthropic 面不得凭空出现该头")
	})
}

// requireTypedRelay openai 系两入口的共用断言。
func requireTypedRelay(t *testing.T, built http.Header, wantAuth string) {
	t.Helper()
	require.Equal(t, "curl/8.7.1", built.Get("User-Agent"), "客户端 UA 压过 SDK 默认（WithHeader 在 cfg.Apply 之后执行）")
	require.Equal(t, []string{"oc-1"}, built["X-Opencode-Session"], "自定义 session 头如实达上游")
	_, ok := built["Accept-Encoding"]
	require.False(t, ok, "Accept-Encoding 不得出栈（deny＝计费回归门）")
	_, ok = built["Cookie"]
	require.False(t, ok, "Cookie（跨租户态）不得出栈")
	require.Equal(t, wantAuth, built.Get("Authorization"), "入站凭据被剔后网关写账号 key（不变量 #5）")
}

// TestTypedRelayMultiValueFidelity 多值保真：`WithHeader` 内部是 `Header.Set`
// ⇒ 若一律 WithHeader，同键两个值只剩末值 = 静默丢客户端事实。这条断言把
// 「首个 WithHeader + 其余 WithHeaderAdd」钉住（实现写错时第二个值会消失）。
func TestTypedRelayMultiValueFidelity(t *testing.T) {
	srv := typedUpstream(t)
	defer srv.Close()
	capRt := &headerCapture{base: srv.Client().Transport}
	f := NewFactory(&http.Client{Transport: capRt}, Config{UpstreamTimeout: 5 * time.Second})
	tpl := &domain.Template{ID: 2, BaseURL: srv.URL}

	in := http.Header{
		"Openai-Beta": {"advanced-tool-use", "citations-2025-01-01"},
		"X-Tag":       {"a", "b", "c"},
	}

	_, err := f.Response(context.Background(), tpl, "sk-resp", responses.ResponseNewParams{
		Model: "gpt-4o",
		Input: responses.ResponseNewParamsInputUnion{OfString: openai.String("x")},
	}, in)
	require.NoError(t, err)
	built := capRt.headers(t)
	require.Equal(t, []string{"advanced-tool-use", "citations-2025-01-01"}, built["Openai-Beta"],
		"beta 多值必须全在线上（顺序也要保住：首值 Set、其余 Add）")
	require.Equal(t, []string{"a", "b", "c"}, built["X-Tag"], "三值键必须全在")

	_, err = f.AnthMessage(context.Background(), tpl, "sk-anth", anthropic.MessageNewParams{
		Model:     "claude-3-5-sonnet",
		MaxTokens: 16,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("x"))},
	}, http.Header{"Anthropic-Beta": {"v1", "v2"}})
	require.NoError(t, err)
	built = capRt.headers(t)
	require.Equal(t, []string{"v1", "v2"}, built["Anthropic-Beta"], "anthropic option 包同样不得压掉第二个值")
}

// TestTypedRelayNilInKeepsSDKDefaults typed 面 in=nil 时不得凭空造出客户端头：
// 断言只覆盖「网关这一层没造东西」——自定义头键不存在 + 鉴权声明仍是账号 key。
//
// 注意它**不是** raw 面护栏 G1 的对应项：G1 断言出栈不含 User-Agent 键，typed 面
// 结构上做不到（SDK 在 cfg.Apply 之前就写了自己的默认 UA，RoundTripper 视图必然
// 看到 `OpenAI/Go …`，其字面值随 SDK 版本漂）。"网关不凭空造 UA" 由 raw 面 G1
// 守住；typed 面若将来出现网关自造 UA，这条测试拦不住，是已知覆盖边界。
func TestTypedRelayNilInKeepsSDKDefaults(t *testing.T) {
	srv := typedUpstream(t)
	defer srv.Close()
	capRt := &headerCapture{base: srv.Client().Transport}
	f := NewFactory(&http.Client{Transport: capRt}, Config{UpstreamTimeout: 5 * time.Second})
	tpl := &domain.Template{ID: 3, BaseURL: srv.URL}

	_, err := f.ChatCompletion(context.Background(), tpl, "sk-nil", openai.ChatCompletionNewParams{
		Model:    "gpt-4o",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("x")},
	}, nil)
	require.NoError(t, err)
	built := capRt.headers(t)
	_, ok := built["X-Opencode-Session"]
	require.False(t, ok, "in=nil 时不得凭空造出客户端头")
	require.Equal(t, "Bearer sk-nil", built.Get("Authorization"))
}

// TestTypedRelayStripsInboundFootprint R7 typed 面（spec §10）：客户端 IP / 转发
// 路径类头不得达上游。断言打在网关自建出栈头（headerCapture）——SDK 走的是同一个
// 注入的 http.Client，但 net/http 的 Request.write 会在 RoundTripper 之后补
// User-Agent/Accept-Encoding，服务端视图不足以证明「网关没发」（见 C2 实测教训）。
func TestTypedRelayStripsInboundFootprint(t *testing.T) {
	srv := typedUpstream(t)
	defer srv.Close()
	capRt := &headerCapture{base: srv.Client().Transport}
	f := NewFactory(&http.Client{Transport: capRt}, Config{UpstreamTimeout: 5 * time.Second})
	tpl := &domain.Template{ID: 4, BaseURL: srv.URL}

	in := http.Header{"X-Opencode-Session": {"oc-1"}} // 哨兵：允许面不受影响
	for _, k := range inboundFootprintCanonical {
		in[k] = []string{"203.0.113.9"}
	}
	_, err := f.ChatCompletion(context.Background(), tpl, "sk-foot", openai.ChatCompletionNewParams{
		Model:    "gpt-4o",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("x")},
	}, in)
	require.NoError(t, err)

	built := capRt.headers(t)
	for _, k := range inboundFootprintCanonical {
		_, ok := built[k]
		require.False(t, ok, "入站足迹头 %s 不得进入 typed 面出栈头（map 槽位级断言）", k)
	}
	require.Equal(t, []string{"oc-1"}, built["X-Opencode-Session"], "哨兵键仍须透传")
	require.Equal(t, "Bearer sk-foot", built.Get("Authorization"), "账号凭据仍在 relay 之后写（不变量 #5）")
}
