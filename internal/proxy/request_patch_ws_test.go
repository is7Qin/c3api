// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

// request_patch WS 数据面（spec custom-request-patch §2.4/§7.2/§7.4/§9 WS 行/
// §10 P4/§11.5）：
//   - 握手：全局层 → 格式层 openai-responses-ws 的头 Set/Del/覆盖、Host 特例
//     （DialOptions.Host 按层三态）、{api_key} 用当次 attempt 原始 cred、无配置
//     与 P3 前等价；
//   - 静态车道首帧：wsFirstFrameForSel → stripTier → bodyPatch，只经 relayWS 写出
//     （dial 不收首帧——上游只看到恰好一帧、不被双写/双 patch）；
//   - 后续客户端帧：仅 response.create 帧 body patch；response.cancel 等控制帧与
//     非 JSON 帧逐字节原样；
//   - 零成本：无配置 hook/decorator 均 nil、零分配；非 create 帧扫描零分配；
//   - sjson 残余失败：裸透传 + 固定文案限频 WARN（不打印 path/value/body）。
// 真实 fake upstream（httptest + coder/websocket）逐字节断言上游握手头/帧。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/modelmap"
	"github.com/is7qin/c3api/pkg/aiclient"
)

// patchWSHandshake 一次上游 WS 握手观测快照（头 + Host + 路径）。
type patchWSHandshake struct {
	Header http.Header
	Host   string
	Path   string
}

// patchWSHooks 假上游观测面（握手头 + 收到的客户端帧；echo 为确定性屏障）。
type patchWSHooks struct {
	mu          sync.Mutex
	handshakes  []patchWSHandshake
	frames      []string
	rejectFirst int  // 前 N 次握手在 HTTP 层拒绝（failover / {api_key} 换号用例）
	rejectCode  int  // 拒绝状态码（0 ⇒ 429）
	frameLimit  int  // 读满 N 帧后上游 1000 关闭（0 = 不主动关闭）
	echo        bool // 每帧回显 {"type":"echo","payload":"<raw>"}（读回声 = 上游已收到）
}

func (h *patchWSHooks) snapshotHandshakes() []patchWSHandshake {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]patchWSHandshake(nil), h.handshakes...)
}

func (h *patchWSHooks) snapshotFrames() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.frames...)
}

// newPatchWSUpstream 假上游：/v1/responses 接受 WS 升级（真实上游无 /ws 后缀），
// 逐帧记录后回显；rejectFirst 次握手在 HTTP 层拒绝（状态码分类走既有 failover）。
func newPatchWSUpstream(t *testing.T, hooks *patchWSHooks) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		hooks.mu.Lock()
		idx := len(hooks.handshakes)
		hooks.handshakes = append(hooks.handshakes, patchWSHandshake{Header: r.Header.Clone(), Host: r.Host, Path: r.URL.Path})
		reject := idx < hooks.rejectFirst
		rejectCode := hooks.rejectCode
		frameLimit, echo := hooks.frameLimit, hooks.echo
		hooks.mu.Unlock()
		if reject {
			if rejectCode == 0 {
				rejectCode = http.StatusTooManyRequests
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rejectCode)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream rejected"}}`))
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			CompressionMode: websocket.CompressionContextTakeover,
		})
		if err != nil {
			return
		}
		defer c.CloseNow()
		n := 0
		for {
			typ, msg, err := c.Read(context.Background())
			if err != nil {
				return
			}
			hooks.mu.Lock()
			hooks.frames = append(hooks.frames, string(msg))
			hooks.mu.Unlock()
			n++
			if echo {
				payload, merr := json.Marshal(map[string]any{"type": "echo", "payload": string(msg)})
				if merr != nil {
					return
				}
				if werr := c.Write(context.Background(), typ, payload); werr != nil {
					return
				}
			}
			if frameLimit > 0 && n >= frameLimit {
				_ = c.Close(websocket.StatusNormalClosure, "")
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// patchWSSession 一段 WS 会话的三段式：假上游 + 网关代理 + 已拨号客户端。
type patchWSSession struct {
	p      *Proxy
	hooks  *patchWSHooks
	gw     *httptest.Server
	client *websocket.Conn
}

// newPatchWSSession 起会话（hooks nil ⇒ echo + 读满 1 帧关闭的默认观测面）。
func newPatchWSSession(t *testing.T, hooks *patchWSHooks, tplFn func(upstreamURL string) *domain.Template) *patchWSSession {
	t.Helper()
	if hooks == nil {
		hooks = &patchWSHooks{echo: true, frameLimit: 1}
	}
	up := newPatchWSUpstream(t, hooks)
	tpl := tplFn(up.URL)
	p := newPatchProxy(t, tpl, nil, nil, &captureLogStore{}, "", nil)
	gw := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	t.Cleanup(gw.Close)
	c := dialPatchWS(t, gw, nil)
	t.Cleanup(func() { c.CloseNow() })
	return &patchWSSession{p: p, hooks: hooks, gw: gw, client: c}
}

// dialPatchWS 测试客户端拨网关 /v1/responses（网关 key 双载体齐带；extra 为额外
// 客户端头——用于验证既有透传不被 patch 破坏）。
func dialPatchWS(t *testing.T, gw *httptest.Server, extra http.Header) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(gw.URL, "http") + "/v1/responses"
	h := http.Header{
		"Authorization": {"Bearer ck-1"},
		"X-Api-Key":     {"ck-1"},
	}
	for k, vs := range extra {
		h[k] = vs
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{
		HTTPHeader:      h,
		CompressionMode: websocket.CompressionContextTakeover,
	})
	require.NoError(t, err, "网关 WS 握手必须成功")
	return c
}

// relayedFrame 发送一帧并读上游回声（屏障：回声到 = 上游确实收到该帧；返回的
// 字节是上游实际收到的**转发后**字节——首帧 patch / 后续帧 hook 的直接证据）。
func relayedFrame(t *testing.T, c *websocket.Conn, frame []byte) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, c.Write(ctx, websocket.MessageText, frame))
	typ, b, err := c.Read(ctx)
	require.NoError(t, err, "等待上游回声（read frame）")
	require.Equal(t, websocket.MessageText, typ)
	var echo struct {
		Type    string `json:"type"`
		Payload string `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(b, &echo), "回声帧必须是合法 JSON")
	require.Equal(t, "echo", echo.Type)
	return []byte(echo.Payload)
}

// wsPatchTpl 用上游 URL 造 resp-ws custom 模板（id 1；patchWire 空 = 无配置）。
func wsPatchTpl(t *testing.T, upstreamURL, patchWire string) *domain.Template {
	t.Helper()
	return customPatchTpl(t, 1, upstreamURL, []domain.RequestFormat{domain.FormatOpenAIResponsesWS}, patchWire)
}

// TestRequestPatchWSHandshakeHeaders 握手装饰（spec §7.2/§11.5）：两层头
// （全局 → 格式层覆盖/Del）、网关默认头可覆盖/删除、客户端头照旧透传、Host 走
// DialOptions.Host、{api_key} 用当次 attempt 原始 cred 展开、脏 Accept-Encoding
// 不进握手（运行期跳过）。
func TestRequestPatchWSHandshakeHeaders(t *testing.T) {
	patch := `{
		"headers":{
			"User-Agent":"vibe-code/1.0",
			"X-Global":"global",
			"Authorization":"Bearer {api_key}",
			"Host":"virtual.example",
			"X-Drop":"",
			"X-Layer":"global"
		},
		"formats":{"openai-responses-ws":{"headers":{
			"X-Format":"format",
			"X-Layer":"format",
			"X-Drop":"kept-by-format",
			"Responses-Websockets":""
		}}}
	}`
	hooks := &patchWSHooks{echo: true, frameLimit: 1}
	up := newPatchWSUpstream(t, hooks)
	tpl := wsPatchTpl(t, up.URL, patch)
	p := newPatchProxy(t, tpl, nil, nil, &captureLogStore{}, "", nil)
	gw := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	t.Cleanup(gw.Close)
	c := dialPatchWS(t, gw, http.Header{"X-Client-Version": {"codex-1.2.3"}})
	t.Cleanup(func() { c.CloseNow() })

	_ = relayedFrame(t, c, []byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`))

	handshakes := hooks.snapshotHandshakes()
	require.Len(t, handshakes, 1)
	h := handshakes[0]
	require.Equal(t, "/v1/responses", h.Path, "既有上游路径派生不变")
	require.Equal(t, "Bearer sk-upstream", h.Header.Get("Authorization"),
		"{api_key} 用当次 attempt 原始 cred 展开，覆盖网关默认鉴权头")
	require.Equal(t, "vibe-code/1.0", h.Header.Get("User-Agent"), "配置面可覆盖 UA")
	require.Equal(t, "global", h.Header.Get("X-Global"), "全局层头生效")
	require.Equal(t, "format", h.Header.Get("X-Layer"), "格式层覆盖全局层")
	require.Equal(t, "format", h.Header.Get("X-Format"), "格式层独有头生效")
	require.Equal(t, "kept-by-format", h.Header.Get("X-Drop"),
		"全局空值 Del 客户端头后，格式层非空值可重新 Set（逐层三态）")
	require.Equal(t, "codex-1.2.3", h.Header.Get("X-Client-Version"), "既有客户端头透传不被破坏")
	_, hasBeta := h.Header["Responses-Websockets"]
	require.False(t, hasBeta, "格式层空值可删除网关默认 beta 头")
	require.Equal(t, "virtual.example", h.Host, "Host 特例走 DialOptions.Host")
	// 脏库 Accept-Encoding 的运行期跳过由 TestRequestPatchWSAcceptEncodingRuntimeSkip
	// 独立承重（保存期非空 AE 已 400，夹具无法经生产解析入口构造该值）。
}

// TestRequestPatchWSHandshakeNoConfigUnchanged 无配置握手与 P3 前等价：默认鉴权 +
// beta 头、客户端头照旧、Host = URL.Host、无任何 patch 头；首帧逐字节不变。
func TestRequestPatchWSHandshakeNoConfigUnchanged(t *testing.T) {
	hooks := &patchWSHooks{echo: true, frameLimit: 1}
	up := newPatchWSUpstream(t, hooks)
	tpl := wsPatchTpl(t, up.URL, "")
	p := newPatchProxy(t, tpl, nil, nil, &captureLogStore{}, "", nil)
	gw := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	t.Cleanup(gw.Close)
	c := dialPatchWS(t, gw, http.Header{"X-Client-Version": {"codex-1.2.3"}})
	t.Cleanup(func() { c.CloseNow() })

	first := []byte(`{"type":"response.create","model":"gpt-4o","input":"hi","client_only":1}`)
	echo := relayedFrame(t, c, first)
	require.Equal(t, first, echo, "无配置：首帧逐字节原样转发")

	handshakes := hooks.snapshotHandshakes()
	require.Len(t, handshakes, 1)
	h := handshakes[0]
	require.Equal(t, "Bearer sk-upstream", h.Header.Get("Authorization"), "无配置保持网关默认鉴权头")
	require.Equal(t, aiclient.ResponsesWSBetaHeader, h.Header.Get("Responses-Websockets"), "无配置保持网关默认 beta 头")
	upURL, err := url.Parse(up.URL)
	require.NoError(t, err)
	require.Equal(t, upURL.Host, h.Host, "无配置 Host = URL.Host（不设置 DialOptions.Host）")
	require.Equal(t, "codex-1.2.3", h.Header.Get("X-Client-Version"), "无配置客户端头照旧透传")
	for _, dropped := range []string{"X-Global", "X-Layer", "X-Drop"} {
		_, ok := h.Header[dropped]
		require.False(t, ok, "无配置不得出现 patch 头 %s", dropped)
	}
}

// TestRequestPatchWSHostTriState Host 特例的**按层三态**（spec §2.4/§2.6/§7.2；
// 与 P3 HTTP 修法同语义，落点是 DialOptions.Host）：
//   - 全局 Set（无格式层 Host）⇒ 全局生效；
//   - 全局 Set + 格式层空值 ⇒ 格式层覆盖 ⇒ 回落 URL.Host；
//   - 仅格式层 Set ⇒ 生效；
//   - 全局空值 + 格式层 Set ⇒ 格式层生效；
//   - 小写 host 经 ParseRequestPatch 归一为 Host 规范形后生效；
//   - Host 值里的 {api_key} 用当次 attempt 原始 cred 展开。
func TestRequestPatchWSHostTriState(t *testing.T) {
	cases := []struct {
		name      string
		patchWire string
		wantHost  string // "" = 期望回落 httptest URL.Host
	}{
		{"global_set", `{"headers":{"Host":"global.example"}}`, "global.example"},
		{
			"global_set_format_del_falls_back_to_url_host",
			`{"headers":{"Host":"global.example"},"formats":{"openai-responses-ws":{"headers":{"Host":""}}}}`,
			"",
		},
		{"format_set_only", `{"formats":{"openai-responses-ws":{"headers":{"Host":"format.example"}}}}`, "format.example"},
		{
			"global_del_format_set_wins",
			`{"headers":{"Host":""},"formats":{"openai-responses-ws":{"headers":{"Host":"format.example"}}}}`,
			"format.example",
		},
		{"lowercase_host_normalized", `{"headers":{"host":"lower.example"}}`, "lower.example"},
		{"host_value_expands_api_key", `{"headers":{"Host":"{api_key}.example"}}`, "sk-upstream.example"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hooks := &patchWSHooks{echo: true, frameLimit: 1}
			up := newPatchWSUpstream(t, hooks)
			upURL, err := url.Parse(up.URL)
			require.NoError(t, err)
			tpl := wsPatchTpl(t, up.URL, tc.patchWire)
			p := newPatchProxy(t, tpl, nil, nil, &captureLogStore{}, "", nil)
			gw := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
			t.Cleanup(gw.Close)
			c := dialPatchWS(t, gw, nil)
			t.Cleanup(func() { c.CloseNow() })

			_ = relayedFrame(t, c, []byte(`{"type":"response.create","model":"gpt-4o"}`))
			want := tc.wantHost
			if want == "" {
				want = upURL.Host
			}
			require.Equal(t, want, hooks.snapshotHandshakes()[0].Host, "Host 按层三态")
		})
	}
}

// TestRequestPatchWSAcceptEncodingRuntimeSkip 运行期跳过 Accept-Encoding 的独立
// 承重门（不经 transport 兜底）：wsHeaderDecorator 自身不得把脏库 AE 写进
// DialOptions.HTTPHeader；同一层的合法头照常应用。
func TestRequestPatchWSAcceptEncodingRuntimeSkip(t *testing.T) {
	sel := selectionForPatchTest()
	sel.Upstream = &domain.TemplateUpstream{Patch: &domain.RequestPatch{
		Headers: map[string]string{"Accept-Encoding": "gzip", "X-Global": "applied"},
		Formats: map[domain.UpstreamPathKey]domain.FormatPatch{
			domain.UpstreamPathResponsesWS: {Headers: map[string]string{"Accept-Encoding": "br", "X-Format": "applied"}},
		},
	}}
	dec := wsHeaderDecorator(sel, patchKeyResponsesWS, "sk-x")
	require.NotNil(t, dec, "有头配置必须返回装饰器（nil 则本门空转）")
	opts := &websocket.DialOptions{HTTPHeader: http.Header{}}
	dec(opts)
	_, hasAE := opts.HTTPHeader["Accept-Encoding"]
	require.False(t, hasAE, "装饰器自身不得写 Accept-Encoding（不能靠 transport 掩盖）")
	require.Equal(t, "applied", opts.HTTPHeader.Get("X-Global"), "全局层合法头正常应用")
	require.Equal(t, "applied", opts.HTTPHeader.Get("X-Format"), "格式层合法头正常应用")
}

// TestRequestPatchWSHelpersNoConfigZeroAllocs 无配置零成本 + 帧扫描零分配（spec
// A5/§7.2 性能要求，硬断言）：
//   - 无配置：wsHeaderDecorator / wsClientFrameHook 均 nil、0 allocs；
//   - 仅头配置：帧 hook nil（body 面空 ⇒ 快路径）；握手装饰器 ≤1 闭包；
//   - 有 body 配置：非 create 帧（response.cancel / 非 JSON）命中扫描后返回 nil，
//     0 allocs（不复制帧、不解析 JSON）；
//   - create 帧才真正执行 patch（返回 patched 字节）。
func TestRequestPatchWSHelpersNoConfigZeroAllocs(t *testing.T) {
	plain := selectionForPatchTest()
	p := &Proxy{}
	allocs := testing.AllocsPerRun(100, func() {
		if dec := wsHeaderDecorator(plain, patchKeyResponsesWS, "sk-x"); dec != nil {
			t.Fatalf("无配置 wsHeaderDecorator 必须返回 nil")
		}
		if hook := p.wsClientFrameHook(plain); hook != nil {
			t.Fatalf("无配置 wsClientFrameHook 必须返回 nil")
		}
	})
	require.Zero(t, allocs, "无配置 WS 热路径必须零分配（闭包/切片/解析都不许有）")

	headersOnly := selectionForPatchTest()
	headersOnly.Upstream = &domain.TemplateUpstream{Patch: &domain.RequestPatch{
		Formats: map[domain.UpstreamPathKey]domain.FormatPatch{
			domain.UpstreamPathResponsesWS: {Headers: map[string]string{"OpenAI-Beta": "responses=v2"}},
		},
	}}
	require.NotNil(t, wsHeaderDecorator(headersOnly, patchKeyResponsesWS, "sk-x"), "有格式层头必须返回装饰器")
	require.Nil(t, p.wsClientFrameHook(headersOnly), "仅头配置 ⇒ 帧 hook nil 快路径")

	bodyConfigured := selectionForPatchTest()
	bodyConfigured.Upstream = &domain.TemplateUpstream{Patch: &domain.RequestPatch{
		Formats: map[domain.UpstreamPathKey]domain.FormatPatch{
			domain.UpstreamPathResponsesWS: {Set: map[string]json.RawMessage{"patched": json.RawMessage(`1`)}},
		},
	}}
	hook := p.wsClientFrameHook(bodyConfigured)
	require.NotNil(t, hook, "有 body 配置必须返回帧 hook")
	cancelFrame := []byte(`{"type":"response.cancel","response_id":"rsp_1","patched":0}`)
	nonJSONFrame := []byte("not a json frame")
	allocs = testing.AllocsPerRun(100, func() {
		if got := hook(cancelFrame); got != nil {
			t.Fatalf("response.cancel 必须原样转发（返回 nil）")
		}
		if got := hook(nonJSONFrame); got != nil {
			t.Fatalf("非 JSON 帧必须原样转发（返回 nil）")
		}
	})
	require.Zero(t, allocs, "未命中 create 的帧：首键扫描零分配、不复制、不解析 JSON")

	createFrame := []byte(`{"type":"response.create","model":"gpt-4o"}`)
	patched := hook(createFrame)
	require.NotNil(t, patched, "response.create 命中才执行 body patch")
	require.EqualValues(t, 1, gjson.GetBytes(patched, "patched").Int(), "patch 生效")
	require.Equal(t, "gpt-4o", gjson.GetBytes(patched, "model").String(), "未配置字段保真")
}

// TestRequestPatchWSIsResponseCreateFrame 首键扫描判据（零分配字节扫描，与
// isResponsesCompleted 同款）：type 是首键/靠后键都命中；其他 type/非对象/非 JSON
// 不命中；不依赖字段顺序（scanKeyValue 精确顶层键）。
func TestRequestPatchWSIsResponseCreateFrame(t *testing.T) {
	require.True(t, isResponseCreateFrame([]byte(`{"type":"response.create"}`)))
	require.True(t, isResponseCreateFrame([]byte(`{"model":"gpt-4o","type":"response.create","input":"hi"}`)), "type 非首键也必须命中")
	require.True(t, isResponseCreateFrame([]byte("  "+`{"type":"response.create"}`)))
	require.False(t, isResponseCreateFrame([]byte(`{"type":"response.cancel"}`)))
	require.False(t, isResponseCreateFrame([]byte(`{"type":"response.created"}`)))
	require.False(t, isResponseCreateFrame([]byte("not-json")))
	require.False(t, isResponseCreateFrame(nil))
	require.False(t, isResponseCreateFrame([]byte(`["response.create"]`)), "顶层非对象不命中")
}

// TestRequestPatchWSFirstFramePatchedOnce 静态车道首帧（spec §7.2/§11.5）：
//   - 顺序契约由「patch 可覆盖映射后的 model」直接证明：wsFirstFrameForSel（模型
//     映射）→ stripTier → bodyPatch（最后一步，操作员权威）；
//   - 首帧只由 relayWS 写出一次（dial 不收首帧）：上游恰好收到 1 帧，且客户端看到
//     的上游回声 = relayWS 实际写出的 patched 字节。
//
// 「不得双写」门禁（P4 fix1 momus Minor-1）：frameLimit 必须**大于 1**——旧夹具
// frameLimit:1 时上游读满 1 帧就关，真写两次也只观察到 1 帧，断言恒真（momus M5
// 变异未红）。现在上游持续读 8 帧 + echo，随后再发一帧控制帧做**读取进度屏障**：
// 若首帧真被写两次，客户端读到的下一帧回声必然是重复首帧（TCP 有序——两次首帧写
// 都先于控制帧），从而确定性变红，不依赖 echo 之后到快照之间的调度时机。
func TestRequestPatchWSFirstFramePatchedOnce(t *testing.T) {
	hooks := &patchWSHooks{echo: true, frameLimit: 8}
	up := newPatchWSUpstream(t, hooks)
	patch := `{"formats":{"openai-responses-ws":{"set":{"model":"patch-wins","requested_via":"patched"},"delete":["client_only"]}}}`
	tpl := wsPatchTpl(t, up.URL, patch)
	tpl.ModelMapping = modelmap.ModelMapping{
		"gpt-4o": {MappedModel: "gpt-4o-mapped", Mode: modelmap.ModelMappingModeExplicit},
	}
	p := newPatchProxy(t, tpl, nil, nil, &captureLogStore{}, "", nil)
	gw := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	t.Cleanup(gw.Close)
	c := dialPatchWS(t, gw, nil)
	t.Cleanup(func() { c.CloseNow() })

	first := []byte(`{"type":"response.create","model":"gpt-4o","input":"hi","client_only":1}`)
	echo := relayedFrame(t, c, first)

	frames := hooks.snapshotFrames()
	require.Len(t, frames, 1, "首帧只经 relayWS 写出一次（dial 不收首帧，不得双写/双 patch）")
	require.Equal(t, frames[0], string(echo), "客户端看到的上游回声 = relayWS 实际写出的 patched 字节")
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(frames[0]), &got))
	require.Equal(t, "patch-wins", got["model"], "bodyPatch 在模型映射之后执行（patch 覆盖映射结果）")
	require.Equal(t, "patched", got["requested_via"], "首帧 body set 生效")
	_, hasClientOnly := got["client_only"]
	require.False(t, hasClientOnly, "首帧 body delete 生效")

	// 确定性双写门（momus Minor-1）：控制帧 = 上游读取进度屏障。单写时它逐字节
	// 原样到上游（下一步回声 = 控制帧、上游共 2 帧）；双写时下一帧回声 = 重复
	// 首帧（且上游 3 帧），两条断言都红。
	control := []byte(`{"type":"response.cancel","response_id":"rsp_1"}`)
	echo2 := relayedFrame(t, c, control)
	require.Equal(t, control, echo2, "首帧之后上游收到的下一帧必须是客户端控制帧（首帧不得有第二副本）")
	frames = hooks.snapshotFrames()
	require.Len(t, frames, 2, "首帧 1 次 + 控制帧 1 次：上游共 2 帧")
	require.Equal(t, frames[0], string(echo), "首帧字节在控制帧往返后不变")
	require.Equal(t, frames[1], string(control), "控制帧逐字节原样转发")
}

// TestRequestPatchWSFirstFrameTierMapPatchOrder 首帧三重变换的**同帧顺序证据**
// （spec §7.2/§11.5；P4 fix1 oracle Minor-3）：静态车道首帧在同一个字节序列上
// 依次经过 wsFirstFrameForSel（模型映射 gpt-4o → gpt-4o-mapped）→ stripTier
// （TierPolicy=strip 删 service_tier）→ bodyPatch（set model=patch-wins），最终
// 上游收到的**同一帧**必须同时满足：
//   - 不含 service_tier（stripTier 已执行）；
//   - model == patch-wins（bodyPatch 在映射之后执行；若顺序反了会看到映射值
//     gpt-4o-mapped，本断言即红）。
//
// 注：p.bill 只注入 TierPolicy（Resolver/Balances 不装配 ⇒ 计价/余额预检早退），
// 目的是让首帧 service_tier 进入 strip 策略分支；不扩 newPatchProxy 签名。
func TestRequestPatchWSFirstFrameTierMapPatchOrder(t *testing.T) {
	hooks := &patchWSHooks{echo: true, frameLimit: 8}
	up := newPatchWSUpstream(t, hooks)
	patch := `{"formats":{"openai-responses-ws":{"set":{"model":"patch-wins","requested_via":"patched"},"delete":["client_only"]}}}`
	tpl := wsPatchTpl(t, up.URL, patch)
	tpl.ModelMapping = modelmap.ModelMapping{
		"gpt-4o": {MappedModel: "gpt-4o-mapped", Mode: modelmap.ModelMappingModeExplicit},
	}
	p := newPatchProxy(t, tpl, nil, nil, &captureLogStore{}, "", nil)
	p.bill = &BillingHooks{TierPolicy: func(billing.Tier) billing.TierPolicyMode { return billing.TierPolicyStrip }}
	gw := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	t.Cleanup(gw.Close)
	c := dialPatchWS(t, gw, nil)
	t.Cleanup(func() { c.CloseNow() })

	first := []byte(`{"type":"response.create","model":"gpt-4o","service_tier":"fast","input":"hi","client_only":1}`)
	echo := relayedFrame(t, c, first)

	frames := hooks.snapshotFrames()
	require.Len(t, frames, 1, "首帧只写一次")
	got := frames[0]
	require.Equal(t, got, string(echo), "回声 = 上游实际收到的首帧字节")
	require.NotContains(t, got, "service_tier", "同一帧内 stripTier 生效：service_tier 字段被删（逐字节）")
	require.False(t, gjson.Get(got, "service_tier").Exists(), "service_tier 在 gjson 视角同样不存在")
	require.Equal(t, "patch-wins", gjson.Get(got, "model").String(),
		"bodyPatch 在 wsFirstFrameForSel 映射之后执行（最终 model 为 patch 值，不是映射值 gpt-4o-mapped）")
	require.Equal(t, "patched", gjson.Get(got, "requested_via").String(), "同帧 body set 生效")
	require.False(t, gjson.Get(got, "client_only").Exists(), "同帧 body delete 生效")
	require.Equal(t, "hi", gjson.Get(got, "input").String(), "既有字段保真（未被 patch 波及）")
}

// TestRequestPatchWSSubsequentFramesOnlyCreate 后续客户端帧（spec §3 矩阵/§7.2）：
// 仅 response.create 帧 body patch；response.cancel / 其他 type / 非 JSON 帧逐字节
// 原样（控制帧语义不被改写）；create 的 type 非首键也命中（scanKeyValue 顶层键）。
func TestRequestPatchWSSubsequentFramesOnlyCreate(t *testing.T) {
	hooks := &patchWSHooks{echo: true}
	up := newPatchWSUpstream(t, hooks)
	patch := `{"formats":{"openai-responses-ws":{"set":{"later":"patched"},"delete":["drop_me"]}}}`
	tpl := wsPatchTpl(t, up.URL, patch)
	p := newPatchProxy(t, tpl, nil, nil, &captureLogStore{}, "", nil)
	gw := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	t.Cleanup(gw.Close)
	c := dialPatchWS(t, gw, nil)
	t.Cleanup(func() { c.CloseNow() })

	sent := []string{
		`{"type":"response.create","model":"gpt-4o","input":"hi"}`,
		`{"type":"response.create","model":"gpt-4o","drop_me":1}`,
		`{"type":"response.cancel","response_id":"rsp_1","drop_me":1,"later":"keep-me"}`,
		`plain text client frame`,
		`{"model":"gpt-4o","type":"response.create","input":"hi","drop_me":1}`,
		`{"type":"response.output_text.delta","delta":"hi","later":"keep-me"}`,
	}
	echoes := make([]string, 0, len(sent))
	for _, frame := range sent {
		echoes = append(echoes, string(relayedFrame(t, c, []byte(frame))))
	}
	frames := hooks.snapshotFrames()
	require.Len(t, frames, len(sent), "每帧恰好一次上游写")

	// ① response.create（首帧）：patch 生效。
	require.Equal(t, "patched", gjson.Get(frames[0], "later").String(), "首帧 create patch 生效")
	// ② 后续 response.create：同样 patch。
	require.Equal(t, "patched", gjson.Get(frames[1], "later").String(), "后续 create 帧 patch 生效")
	require.False(t, gjson.Get(frames[1], "drop_me").Exists(), "后续 create 帧 delete 生效")
	// ③ response.cancel：patch 配置里同时有 later Set 与 drop_me Delete，必须逐字节不变。
	require.Equal(t, sent[2], frames[2], "response.cancel 帧逐字节不变")
	// ④ 非 JSON 帧：结构性跳过、逐字节不变。
	require.Equal(t, sent[3], frames[3], "非 JSON 帧逐字节不变")
	// ⑤ create 的 type 非首键：scanKeyValue 顶层键扫描仍命中。
	require.Equal(t, "patched", gjson.Get(frames[4], "later").String(), "type 非首键的 create 帧也命中")
	require.False(t, gjson.Get(frames[4], "drop_me").Exists())
	// ⑥ 其他事件 type：原样。
	require.Equal(t, sent[5], frames[5], "非 create 事件帧逐字节不变")
	// 回声 = 上游实际收到字节（同一事实的另一面）。
	require.Equal(t, frames[0], echoes[0], "首帧回声 = relayWS 写出的 patched 字节")
	require.Equal(t, frames[2], echoes[2], "cancel 帧回声 = 原样字节")
}

// TestRequestPatchWSFailoverRecomputesAPIKey failover 每 attempt 按当次 sel/cred
// 重算握手装饰器：{api_key} 换号换值（禁止从 Authorization 反解、禁止一次改写）；
// 首个 attempt 429 → 第二个账号握手成功并转发首帧。
func TestRequestPatchWSFailoverRecomputesAPIKey(t *testing.T) {
	hooks := &patchWSHooks{echo: true, frameLimit: 1, rejectFirst: 1, rejectCode: http.StatusTooManyRequests}
	up := newPatchWSUpstream(t, hooks)
	tpl := wsPatchTpl(t, up.URL, `{"headers":{"X-Vendor-Key":"{api_key}","Authorization":"Bearer {api_key}"}}`)
	accs := map[int64][]*domain.Account{10: {
		{ID: 1, TemplateID: tpl.ID, Template: tpl, UpstreamKey: "sk-first", Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 8},
		{ID: 2, TemplateID: tpl.ID, Template: tpl, UpstreamKey: "sk-second", Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 8},
	}}
	p := newPatchProxy(t, tpl, accs, nil, &captureLogStore{}, "", nil)
	gw := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	t.Cleanup(gw.Close)
	c := dialPatchWS(t, gw, nil)
	t.Cleanup(func() { c.CloseNow() })

	_ = relayedFrame(t, c, []byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`))

	handshakes := hooks.snapshotHandshakes()
	require.Len(t, handshakes, 2, "首个 attempt 握手 429 → 换号重试（两次握手）")
	require.Equal(t, "sk-first", handshakes[0].Header.Get("X-Vendor-Key"), "第一 attempt 用账号 1 原始 cred")
	require.Equal(t, "Bearer sk-first", handshakes[0].Header.Get("Authorization"))
	require.Equal(t, "sk-second", handshakes[1].Header.Get("X-Vendor-Key"), "第二 attempt 必须用账号 2 原始 cred（每 attempt 重算）")
	require.Equal(t, "Bearer sk-second", handshakes[1].Header.Get("Authorization"))
}

// TestRequestPatchWSResidualFailureWarnOnce sjson 残余失败（脏库路径，spec §7.4）：
//   - 会话不中止、帧裸透传（上游收到 = 既有变换后的原文）；
//   - 握手头装饰与 body 失败相互独立（头照常生效）；
//   - 固定文案限频 WARN 恰好一次（按 template_id+format），且不含 path/value/body。
func TestRequestPatchWSResidualFailureWarnOnce(t *testing.T) {
	dir, err := os.MkdirTemp("", "c3api-patch-ws-residual-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	logOut := filepath.Join(dir, "warn.log")

	hooks := &patchWSHooks{echo: true, frameLimit: 2}
	up := newPatchWSUpstream(t, hooks)
	// 手工构造脏 patch：x.999999999 超出 sjson 数组 padding 上限，运行期执行失败
	// （保存期本应 400——这里模拟手工改库绕过写面）。
	dirty := &domain.RequestPatch{
		Headers: map[string]string{"X-Patch-Header": "applied"},
		Formats: map[domain.UpstreamPathKey]domain.FormatPatch{
			domain.UpstreamPathResponsesWS: {
				Set: map[string]json.RawMessage{
					"x.999999999":   json.RawMessage(`1`),
					"secret_marker": json.RawMessage(`"must-not-leak"`),
				},
			},
		},
	}
	tpl := wsPatchTpl(t, up.URL, "")
	tpl.Upstream = domain.NewTemplateUpstream(nil, dirty)
	p := newPatchProxy(t, tpl, nil, nil, &captureLogStore{}, logOut, nil)
	gw := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	t.Cleanup(gw.Close)
	c := dialPatchWS(t, gw, nil)
	t.Cleanup(func() { c.CloseNow() })

	body := []byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)
	for i := 0; i < 2; i++ {
		echo := relayedFrame(t, c, body)
		require.Equal(t, body, echo, "残余失败不得中止会话/改写帧（第 %d 次）", i)
	}

	frames := hooks.snapshotFrames()
	require.Len(t, frames, 2)
	for i, f := range frames {
		require.Equal(t, body, []byte(f), "body 残余失败 ⇒ 帧裸透传（第 %d 次）", i)
	}
	require.Equal(t, "applied", hooks.snapshotHandshakes()[0].Header.Get("X-Patch-Header"),
		"头装饰与 body 失败相互独立（握手头照常生效）")

	data, err := os.ReadFile(logOut)
	require.NoError(t, err)
	logText := string(data)
	require.Equal(t, 1, strings.Count(logText, "request patch body apply failed"),
		"限频 WARN：同一 template+format 一分钟内只一条")
	for _, leak := range []string{"999999999", "must-not-leak", "secret_marker", string(body)} {
		require.NotContains(t, logText, leak, "WARN 不得打印 path/value/body")
	}
}

// TestRequestPatchWSSuccessPathNoPatchLogs 成功路径零 patch 日志（spec §8/§11.20）：
// 握手头 + 首帧 + 后续 create 帧 patch 全成功时不得有任何 patch 日志——只允许
// 失败路径的固定文案限频 WARN（由 TestRequestPatchWSResidualFailureWarnOnce 覆盖）。
func TestRequestPatchWSSuccessPathNoPatchLogs(t *testing.T) {
	dir, err := os.MkdirTemp("", "c3api-patch-ws-log-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	logOut := filepath.Join(dir, "warn.log")

	hooks := &patchWSHooks{echo: true, frameLimit: 2}
	up := newPatchWSUpstream(t, hooks)
	tpl := wsPatchTpl(t, up.URL, `{"headers":{"X-Patch":"1"},"formats":{"openai-responses-ws":{"set":{"patched":"v"}}}}`)
	p := newPatchProxy(t, tpl, nil, nil, &captureLogStore{}, logOut, nil)
	gw := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	t.Cleanup(gw.Close)
	c := dialPatchWS(t, gw, nil)
	t.Cleanup(func() { c.CloseNow() })

	_ = relayedFrame(t, c, []byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`))
	_ = relayedFrame(t, c, []byte(`{"type":"response.create","model":"gpt-4o","input":"again"}`))

	frames := hooks.snapshotFrames()
	require.Len(t, frames, 2)
	require.Equal(t, "v", gjson.Get(frames[1], "patched").String(), "patch 成功生效")

	data, err := os.ReadFile(logOut)
	require.NoError(t, err)
	require.NotContains(t, string(data), "request patch", "成功路径零 patch 日志")
	require.Empty(t, strings.TrimSpace(string(data)), "成功路径不得有 WARN")
}
