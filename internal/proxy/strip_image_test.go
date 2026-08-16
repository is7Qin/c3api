// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// ---------------------------------------------------------------------------
// W4 图像 tool 剥离单元测试：纯函数 stripImageTools（response.create 帧 →
// 剥离后帧）。
// 热路径纪律：关闭 = 调用方开关分支（不调用，见集成测试）；开启 = 预筛无
// 命中零解析零分配直转（切片恒等断言 + AllocsPerRun 零分配断言）。
// ---------------------------------------------------------------------------

// TestStripImageToolsStripsImageTools 剥离正确性：tools 数组按真实形态剥离
// （image_generation_tool hosted 工具 / codex namespace 工具 / image_edits），
// 保留工具字节原样（描述/参数不丢），非图像字段原样保留。
func TestStripImageToolsStripsImageTools(t *testing.T) {
	body := []byte(`{
		"model": "gpt-5-codex",
		"input": [{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hi"}]}],
		"tools": [
			{"type": "function", "name": "shell", "description": "run a command", "parameters": {"type": "object", "additionalProperties": false}},
			{"type": "image_generation_tool", "namespace": "image_gen", "name": "image_gen", "description": "Generate an image"},
			{"type": "namespace", "name": "image_gen", "description": "Tools in the image_gen namespace.", "tools": [{"type": "function", "name": "imagegen", "description": "Generate an image", "strict": false, "parameters": {"type": "object", "additionalProperties": false}}]},
			{"type": "web_search_preview", "search_context_size": "medium"}
		]
	}`)
	out := stripImageTools(body)
	require.True(t, json.Valid(out), "拼接输出必须合法 JSON（splice 逗号管理兜底）")
	require.NotSame(t, &body[0], &out[0], "命中且剥离 → 必须产出新帧")

	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &m))
	var tools []json.RawMessage
	require.NoError(t, json.Unmarshal(m["tools"], &tools))
	require.Len(t, tools, 2, "image_generation_tool + image_gen namespace 剥离后只剩 function + web_search")

	var first, last map[string]any
	require.NoError(t, json.Unmarshal(tools[0], &first))
	require.NoError(t, json.Unmarshal(tools[1], &last))
	require.Equal(t, "shell", first["name"], "保留工具字节原样（函数工具不丢）")
	require.Equal(t, "medium", last["search_context_size"], "保留工具字节原样（web_search 参数不丢）")

	var in []map[string]any
	require.NoError(t, json.Unmarshal(m["input"], &in))
	require.Equal(t, "message", in[0]["type"], "非图像字段（input）原样保留")
	require.JSONEq(t, `"gpt-5-codex"`, string(m["model"]), "非图像字段（model）原样保留")
}

// TestStripImageToolsImageEdits image_edits 工具形态剥离。
func TestStripImageToolsImageEdits(t *testing.T) {
	body := []byte(`{"model":"m","tools":[{"type":"image_edits","input_image_mask":{"image_url":"data:image/png;base64,x"}},{"type":"code_interpreter"}]}`)
	out := stripImageTools(body)
	require.True(t, json.Valid(out), "拼接输出必须合法 JSON（splice 逗号管理兜底）")
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &m))
	var tools []map[string]any
	require.NoError(t, json.Unmarshal(m["tools"], &tools))
	require.Len(t, tools, 1)
	require.Equal(t, "code_interpreter", tools[0]["type"])
}

// TestStripImageToolsCodexNamespace codex standalone namespace 工具
// （image_gen.imagegen）：整体剥离 namespace 工具对象（含嵌套 tools）。
func TestStripImageToolsCodexNamespace(t *testing.T) {
	body := []byte(`{"model":"m","tools":[
		{"type":"namespace","name":"image_gen","description":"Tools in the image_gen namespace.","tools":[{"type":"function","name":"imagegen","parameters":{"type":"object"}}]},
		{"type":"function","name":"shell","parameters":{"type":"object"}}
	],"tool_choice":{"type":"namespace","name":"image_gen"}}`)
	out := stripImageTools(body)
	require.True(t, json.Valid(out), "拼接输出必须合法 JSON（splice 逗号管理兜底）")
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &m))
	var tools []map[string]any
	require.NoError(t, json.Unmarshal(m["tools"], &tools))
	require.Len(t, tools, 1)
	require.Equal(t, "shell", tools[0]["name"])
	_, ok := m["tool_choice"]
	require.False(t, ok, "指向已剥 namespace 的 tool_choice 必须移除")
}

// TestStripImageToolsToolChoiceDangling tool_choice 悬挂处理：对象形指向已剥
// 工具 → 移除（缺省 = "auto"）；指向保留工具 / 字符串形 → 原样保留。
func TestStripImageToolsToolChoiceDangling(t *testing.T) {
	cases := []struct {
		name   string
		choice string
		want   string // 期望 tool_choice JSON；"" = 已移除
	}{
		{name: "hosted image tool dangling", choice: `{"type":"image_generation_tool"}`, want: ""},
		{name: "namespace image dangling", choice: `{"type":"namespace","name":"image_gen"}`, want: ""},
		{name: "image_edits dangling", choice: `{"type":"image_edits"}`, want: ""},
		{name: "function choice kept", choice: `{"type":"function","name":"shell"}`, want: `{"type":"function","name":"shell"}`},
		{name: "auto string kept", choice: `"auto"`, want: `"auto"`},
		{name: "required string kept", choice: `"required"`, want: `"required"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 图像工具形态矩阵（评审 I-3 补 image_edits）：悬挂判定按被剥工具
			// 形态匹配——hosted image_generation_tool / namespace image_gen /
			// image_edits 三形态各验证移除或保留。
			tool := `{"type":"image_generation_tool","namespace":"image_gen"}`
			if tc.name == "image_edits dangling" {
				tool = `{"type":"image_edits"}`
			}
			body := []byte(`{"model":"m","tools":[` +
				`{"type":"function","name":"shell","parameters":{"type":"object"}},` +
				tool +
				`],"tool_choice":` + tc.choice + `}`)
			out := stripImageTools(body)
			var m map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(out, &m))
			if tc.want == "" {
				_, ok := m["tool_choice"]
				require.False(t, ok, "悬挂 tool_choice 必须移除（Responses API 缺省 = auto）")
			} else {
				require.JSONEq(t, tc.want, string(m["tool_choice"]), "非悬挂 tool_choice 必须原样保留")
			}
		})
	}
}

// TestStripImageToolsAllStripped 全剥语义（评审 I-1 实证钉住）：tools 全部
// 被剥离 → 删除 tools 字段（缺省 = 无工具，最稳语义——不保留空数组
// "tools":[]），悬挂 tool_choice 同步移除，非图像字段原样保留。
func TestStripImageToolsAllStripped(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","tools":[{"type":"image_generation_tool","namespace":"image_gen"},{"type":"image_edits"}],"tool_choice":{"type":"image_generation_tool"}}`)
	out := stripImageTools(body)
	require.True(t, json.Valid(out), "拼接输出必须合法 JSON（splice 逗号管理兜底）")
	require.NotSame(t, &body[0], &out[0], "命中且剥离 → 必须产出新帧")

	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &m))
	_, ok := m["tools"]
	require.False(t, ok, "全剥 → tools 字段删除（缺省 = 无工具，不保留空数组）")
	_, ok = m["tool_choice"]
	require.False(t, ok, "全剥 + 悬挂 tool_choice → 同步移除（缺省 = auto）")
	require.JSONEq(t, `"m"`, string(m["model"]), "非图像字段原样保留")
	require.JSONEq(t, `"hi"`, string(m["input"]), "非图像字段原样保留")
}

// TestStripImageToolsNoImageToolZeroChange 无图像工具：即使帧内存在 "image"
// 字样（input 内嵌图像内容/工具描述——边界：input 不做），必须返回原切片
// （零解析改写，底层数组同一）。
func TestStripImageToolsNoImageToolZeroChange(t *testing.T) {
	body := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://example.com/a.png"}]}],"tools":[{"type":"function","name":"shell","description":"Generate an image file"}]}`)
	out := stripImageTools(body)
	require.True(t, json.Valid(out), "拼接输出必须合法 JSON（splice 逗号管理兜底）")
	require.Same(t, &body[0], &out[0], "无图像工具 → 原切片直转（零解析改写）")
}

// TestStripImageToolsNoImageSubstring 帧内无 "image" 子串：预筛直转。
func TestStripImageToolsNoImageSubstring(t *testing.T) {
	body := []byte(`{"model":"m","tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}]}`)
	out := stripImageTools(body)
	require.True(t, json.Valid(out), "拼接输出必须合法 JSON（splice 逗号管理兜底）")
	require.Same(t, &body[0], &out[0], "预筛无命中 → 原切片直转")
}

// TestStripImageToolsPreFilterZeroAlloc 预筛路径零分配断言（热路径纪律：开启
// 开关后无命中零解析零分配直转）。
func TestStripImageToolsPreFilterZeroAlloc(t *testing.T) {
	body := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}]}`)
	allocs := testing.AllocsPerRun(200, func() {
		out := stripImageTools(body)
		_ = out
	})
	require.Zero(t, allocs, "预筛无命中必须零分配")
	require.True(t, json.Valid(stripImageTools(body)), "输出必须合法 JSON")
}

// TestStripImageToolsMalformedToolsPassthrough 异常帧（tools 非数组）：
// best-effort 原样转发（上游按现状拒绝，行为不变）。
func TestStripImageToolsMalformedToolsPassthrough(t *testing.T) {
	body := []byte(`{"model":"m","tools":"image_generation_tool","tool_choice":{"type":"image_generation_tool"}}`)
	out := stripImageTools(body)
	require.True(t, json.Valid(out), "拼接输出必须合法 JSON（splice 逗号管理兜底）")
	require.Same(t, &body[0], &out[0], "tools 非数组 → 原样转发")
}

// ---------------------------------------------------------------------------
// 集成：开关开/关两路径（快照布尔读 + 分支）——上游收到的帧断言。
// ---------------------------------------------------------------------------

// fakeResponsesCaptured 记录最近一次收到的 /v1/responses 请求体（剥离断言用）。
func fakeResponsesCaptured(t *testing.T) (*httptest.Server, func() string) {
	t.Helper()
	var (
		mu   sync.Mutex
		body string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "rsp_1", "object": "response", "status": "completed", "model": "m",
			"output": []any{}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
		})
	}))
	return srv, func() string {
		mu.Lock()
		defer mu.Unlock()
		return body
	}
}

// newTestProxyStripTpl 构造指定模板的 responses 测试代理（开关开/关两路径）。
func newTestProxyStripTpl(t *testing.T, tpl *domain.Template) *Proxy {
	t.Helper()
	return newTestProxyTplCapture(t, tpl, 1, true)
}

func stripTpl(up string, strip bool) *domain.Template {
	t := &domain.Template{
		ID: 1, Name: "t", BaseURL: up,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
		Models:           []string{"m"},
	}
	if strip {
		t.CredentialType = credential.TypeResponsesSpecial
		t.StripImageTools = true
	}
	return t
}

// specialKeyProvider responses-special 类型的静态 Key provider（生产接线 =
// W6 codex 凭据族；测试模拟：special 模板凭据经注册表分发返回 UpstreamKey）。
type specialKeyProvider struct{}

func (specialKeyProvider) Type() credential.Type { return credential.TypeResponsesSpecial }
func (specialKeyProvider) Credential(_ context.Context, in credential.CredentialInput) (string, error) {
	return in.APIKey, nil
}

// TestProxyResponsesStripImageToolsOnStream 开关开启 + 流式：原始请求路径
// （ResponseStreamRaw）——上游收到剥离后帧（图像工具删、tool_choice 删、其余
// 字段保留）。
func TestProxyResponsesStripImageToolsOnStream(t *testing.T) {
	up, got := fakeResponsesCaptured(t)
	defer up.Close()
	p := newTestProxyStripTpl(t, stripTpl(up.URL, true))
	p.creds.Register(specialKeyProvider{}) // responses-special 凭据分发（W6 接线模拟）

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"m","input":"hi","stream":true,"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}},{"type":"image_generation_tool","namespace":"image_gen"}],"tool_choice":{"type":"image_generation_tool"}}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleResponses(rec, req)
	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())

	var upBody map[string]any
	require.NoError(t, json.Unmarshal([]byte(got()), &upBody))
	tools, _ := upBody["tools"].([]any)
	require.Len(t, tools, 1, "上游收到的 tools 只剩非图像工具")
	require.Equal(t, "shell", tools[0].(map[string]any)["name"])
	_, ok := upBody["tool_choice"]
	require.False(t, ok, "悬挂 tool_choice 不得转发上游")
	require.Equal(t, "m", upBody["model"], "非图像字段必须原样保留")
	require.Equal(t, true, upBody["stream"], "stream 标志必须保留")
}

// TestProxyResponsesStripImageToolsAllStrippedStream 全剥集成（评审 I-1）：
// 流式原始请求路径——上游收到的帧不得含 tools 字段（删除而非空数组），
// 悬挂 tool_choice 同步移除，其余字段原样。
func TestProxyResponsesStripImageToolsAllStrippedStream(t *testing.T) {
	up, got := fakeResponsesCaptured(t)
	defer up.Close()
	p := newTestProxyStripTpl(t, stripTpl(up.URL, true))
	p.creds.Register(specialKeyProvider{}) // responses-special 凭据分发（W6 接线模拟）

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"m","input":"hi","stream":true,"tools":[{"type":"image_generation_tool","namespace":"image_gen"}],"tool_choice":{"type":"image_generation_tool"}}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleResponses(rec, req)
	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())

	var upBody map[string]any
	require.NoError(t, json.Unmarshal([]byte(got()), &upBody))
	_, ok := upBody["tools"]
	require.False(t, ok, "全剥 → 上游不得收到 tools 字段（缺省 = 无工具，非空数组）")
	_, ok = upBody["tool_choice"]
	require.False(t, ok, "悬挂 tool_choice 不得转发上游")
	require.Equal(t, "m", upBody["model"], "非图像字段原样保留")
	require.Equal(t, true, upBody["stream"], "stream 标志必须保留")
}

// TestProxyResponsesStripImageToolsOffStream 开关关闭 + 流式：原始请求路径——
// 上游必须收到与原帧逐字节相同的字节（快照布尔读 + 分支零开销路径）。
func TestProxyResponsesStripImageToolsOffStream(t *testing.T) {
	up, got := fakeResponsesCaptured(t)
	defer up.Close()
	p := newTestProxyStripTpl(t, stripTpl(up.URL, false))

	orig := `{"model":"m","input":"hi","stream":true,"tools":[{"type":"image_generation_tool","namespace":"image_gen"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(orig))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleResponses(rec, req)
	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, orig, got(), "开关关闭：上游必须收到原始帧（逐字节，零改动）")
}

// TestProxyResponsesStripImageToolsOnNonStream 开关开启 + 非流式：SDK 路径
// （剥离先于 params 解析）——图像工具删、tool_choice 删；其余字段随 SDK
// 重序列化（模型映射后）。回归保障：剥离后仅剩已知工具类型，SDK 可正常
// 序列化 tools（未剥离时 SDK v1.12 对含未知工具的数组整体丢弃——上游收到
// 悬挂 tool_choice 而 400 的缺陷根因，本功能根治）。
func TestProxyResponsesStripImageToolsOnNonStream(t *testing.T) {
	up, got := fakeResponsesCaptured(t)
	defer up.Close()
	p := newTestProxyStripTpl(t, stripTpl(up.URL, true))
	p.creds.Register(specialKeyProvider{}) // responses-special 凭据分发（W6 接线模拟）

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"m","input":"hi","tools":[{"type":"function","name":"shell","parameters":{"type":"object"}},{"type":"image_generation_tool","namespace":"image_gen"}],"tool_choice":{"type":"image_generation_tool"}}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleResponses(rec, req)
	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())

	var upBody map[string]any
	require.NoError(t, json.Unmarshal([]byte(got()), &upBody))
	tools, _ := upBody["tools"].([]any)
	require.Len(t, tools, 1, "剥离后 tools 只剩 function 工具且被 SDK 正常序列化")
	require.Equal(t, "shell", tools[0].(map[string]any)["name"])
	_, ok := upBody["tool_choice"]
	require.False(t, ok, "悬挂 tool_choice 不得转发上游")
}

// ---------------------------------------------------------------------------
// 2026-08-12-strip-optimize-spec v5 新增用例：非数组守卫三形态 / \u 转义必剥 /
// \u 字面不解码 / 双删组合（相邻共享逗号）/ 50-tool 等价性 / 无剥除零分配 /
// 剥除路径分配断言。
// ---------------------------------------------------------------------------

func TestStripImageToolsNonArrayPassthrough(t *testing.T) {
	// 非数组守卫补用例（评审缺陷 6）：tools=number/object/null 原样转发。
	cases := []struct {
		name  string
		tools string
	}{
		{name: "number", tools: `123`},
		{name: "object", tools: `{"type":"image_generation_tool"}`},
		{name: "null", tools: `null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"m","tools":` + tc.tools + `,"tool_choice":{"type":"image_generation_tool"}}`)
			out := stripImageTools(body)
			require.Same(t, &body[0], &out[0], "tools 非数组 → 原样转发（数组守卫）")
		})
	}
}

func TestStripImageToolsUnicodeEscapedToolType(t *testing.T) {
	// \u 转义工具标识必剥（与现状 gjson String() 等价）：type 字节无 "image"
	// 子串 → 预筛 \u 路径 → 解码判定 → 剥除。
	body := []byte(`{"model":"m","tools":[{"type":"\u0069mage_generation_tool","namespace":"image_gen"},{"type":"function","name":"shell","parameters":{"type":"object"}}],"tool_choice":{"type":"image_generation_tool"}}`)
	out := stripImageTools(body)
	require.True(t, json.Valid(out), "输出必须合法 JSON")
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &m))
	var tools []map[string]any
	require.NoError(t, json.Unmarshal(m["tools"], &tools))
	require.Len(t, tools, 1, "\\u 转义图像工具必须剥除")
	require.Equal(t, "shell", tools[0]["name"])
	_, ok := m["tool_choice"]
	require.False(t, ok, "悬挂 tool_choice 同步移除")
}

func TestStripImageToolsBackslashUPrefixLiteral(t *testing.T) {
	// \u 字面（双反斜杠前缀）不解码：type 为字面 "\u0069mage_generation_tool"
	// ≠ 图像工具标识 → 保留（解码器 \\ 前缀感知防漏剥/错剥）。
	body := []byte(`{"model":"m","tools":[{"type":"\\u0069mage_generation_tool"},{"type":"function","name":"shell","parameters":{"type":"object"}}]}`)
	out := stripImageTools(body)
	require.True(t, json.Valid(out), "输出必须合法 JSON")
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &m))
	var tools []map[string]any
	require.NoError(t, json.Unmarshal(m["tools"], &tools))
	require.Len(t, tools, 2, "字面 \\u 形态非图像工具 → 保留")
}

func TestStripImageToolsAllStrippedKeyCombos(t *testing.T) {
	// 双删组合（三位置逗号 + run 合并/分删）：tools 与 tool_choice 双双全剥。
	// 相邻（共享逗号区间重叠 1 字节）→ run 合并重算边界；非相邻/带前导键 →
	// 分删各自三位置规则（一次性计算全部区间后统一拼接）。
	cases := []struct {
		name  string
		body  string
		model bool // 期望保留 model 键（false = 全剥后空对象）
	}{
		{name: "tools first tc second", body: `{"tools":[{"type":"image_generation_tool"}],"tool_choice":{"type":"image_generation_tool"},"model":"m"}`, model: true},
		{name: "tc first tools second", body: `{"tool_choice":{"type":"image_generation_tool"},"tools":[{"type":"image_generation_tool"}],"model":"m"}`, model: true},
		{name: "only keys", body: `{"tools":[{"type":"image_generation_tool"}],"tool_choice":{"type":"image_generation_tool"}}`},
		{name: "model first", body: `{"model":"m","tools":[{"type":"image_generation_tool"}],"tool_choice":{"type":"image_generation_tool"}}`, model: true},
		{name: "non adjacent", body: `{"tools":[{"type":"image_generation_tool"}],"model":"m","tool_choice":{"type":"image_generation_tool"}}`, model: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := stripImageTools([]byte(tc.body))
			require.True(t, json.Valid(out), "拼接输出必须合法 JSON")
			var m map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(out, &m))
			_, ok := m["tools"]
			require.False(t, ok, "全剥 → tools 字段删除")
			_, ok = m["tool_choice"]
			require.False(t, ok, "悬挂 tool_choice 同步删除")
			if tc.model {
				require.JSONEq(t, `"m"`, string(m["model"]), "非图像字段原样保留")
			} else {
				require.Len(t, m, 0, "仅双键全剥 → 空对象")
			}
		})
	}
}

func TestStripImageTools50ToolsEquivalence(t *testing.T) {
	// 50-tool 边界体等价性（压测同形态：1 image_generation_tool + 49 function）。
	body := build50ToolBody()
	require.InDelta(t, 3400, len(body), 600, "压测同形态 ~3.4KB")
	out := stripImageTools(body)
	require.True(t, json.Valid(out), "拼接输出必须合法 JSON（splice 逗号管理兜底）")
	require.NotSame(t, &body[0], &out[0], "命中且剥离 → 必须产出新帧")
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &m))
	var tools []map[string]any
	require.NoError(t, json.Unmarshal(m["tools"], &tools))
	require.Len(t, tools, 49, "1 image_generation_tool 剥离后只剩 49 function")
	require.Equal(t, "f_49", tools[48]["name"], "保留工具顺序与原样")
	_, ok := m["tool_choice"]
	require.False(t, ok, "指向已剥工具的 tool_choice 必须移除")
	require.JSONEq(t, `"m"`, string(m["model"]))
}

func TestStripImageToolsHitNoStripZeroAlloc(t *testing.T) {
	// 预筛命中但无剥除：一次扫描先计数后分配——原切片直转 + 零分配
	// （require.Same 钉住；range-over-func 闭包分配为编译器行为，实测记录）。
	body := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://example.com/a.png"}]}],"tools":[{"type":"function","name":"shell","description":"Generate an image file"}]}`)
	out := stripImageTools(body)
	require.True(t, json.Valid(out), "拼接输出必须合法 JSON（splice 逗号管理兜底）")
	require.Same(t, &body[0], &out[0], "无图像工具 → 原切片直转（零解析改写）")
	allocs := testing.AllocsPerRun(200, func() {
		out := stripImageTools(body)
		_ = out
	})
	require.Zero(t, allocs, "预筛命中但无剥除 → 零分配")
}

func TestStripImageToolsStripAllocs(t *testing.T) {
	// 命中且剥除路径 allocs ≤4（out 预分配 1 + idsBuf/keptBuf 栈数组 0-2
	// （50-tool 溢栈转堆 2，实测 3 allocs/op）+ iter 闭包 0——闭包分配为
	// 编译器行为非语言保证，实测记录）。
	body := []byte(`{"model":"m","tools":[{"type":"function","name":"shell","parameters":{"type":"object"}},{"type":"image_generation_tool","namespace":"image_gen"}]}`)
	allocs := testing.AllocsPerRun(200, func() {
		out := stripImageTools(body)
		_ = out
	})
	require.LessOrEqual(t, allocs, 4.0, "命中且剥除路径 allocs ≤4，实测 %.2f", allocs)
}
