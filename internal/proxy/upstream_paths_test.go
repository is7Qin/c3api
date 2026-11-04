// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/scheduler"
)

// TestCustomUpstreamPathReachesUpstream custom 模板配置的路径必须真正决定上游
// 请求行（含 query），未配置时必须逐字节回落到默认端点。
func TestCustomUpstreamPathReachesUpstream(t *testing.T) {
	var mu sync.Mutex
	var gotPath, gotQuery string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer up.Close()

	cases := []struct {
		name      string
		paths     map[string]string
		wantPath  string
		wantQuery string
	}{
		{
			name:     "未配置回落默认",
			wantPath: "/v1/chat/completions",
		},
		{
			name:      "配置生效含 query",
			paths:     map[string]string{pathKeyChat: "v1/custom/chat?api-version=2024-10-21"},
			wantPath:  "/v1/custom/chat",
			wantQuery: "api-version=2024-10-21",
		},
		{
			name:     "配置生效无 query 且带 base 前缀",
			paths:    map[string]string{pathKeyChat: "v1/acme/chat"},
			wantPath: "/v1/acme/chat",
		},
		{
			// 自定义 = 原样：不以 v1/ 开头也不补 /v1（补全只属于内置端点）。
			name:     "配置不带 v1/ 前缀原样直连",
			paths:    map[string]string{pathKeyChat: "acme/chat"},
			wantPath: "/acme/chat",
		},
		{
			name:      "配置不带 v1/ 前缀且含 query",
			paths:     map[string]string{pathKeyChat: "acme/chat?api-version=2024-10-21"},
			wantPath:  "/acme/chat",
			wantQuery: "api-version=2024-10-21",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			gotPath, gotQuery = "", ""
			mu.Unlock()

			tpl := mappingTpl(up.URL, nil)
			tpl.CredentialType = credential.TypeCustom
			tpl.UpstreamPaths = tc.paths
			store := &captureLogStore{}
			p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, nil)

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
			req.Header.Set("Authorization", "Bearer ck-1")
			rec := httptest.NewRecorder()
			p.HandleChat(rec, req)
			require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
			require.NoError(t, p.rec.Close(t.Context()))

			mu.Lock()
			defer mu.Unlock()
			require.Equal(t, tc.wantPath, gotPath, "上游请求行必须等于配置路径")
			require.Equal(t, tc.wantQuery, gotQuery, "query 必须原样到达（不得被转义进 path）")
		})
	}
}

// TestDefaultTailsAreProtocolFree 门禁：7 个 defaultPath* 只是端点尾段，不得
// 含 /v1 协议段、也不带前导斜杠。协议段只在 targetPath 的「内置端点」分支拼
// 一次（"/v1/" + tail），一旦写回默认值，配置面就会重新背上「补不补 v1」的
// 隐式规则——那正是自定义路径不该关心的东西。
func TestDefaultTailsAreProtocolFree(t *testing.T) {
	cases := []struct {
		key  string
		tail string
	}{
		{pathKeyChat, defaultPathChat},
		{pathKeyResponses, defaultPathResponses},
		{pathKeyResponsesWS, defaultPathResponsesWS},
		{pathKeyAnthropic, defaultPathAnthropic},
		{pathKeySearch, defaultPathSearch},
		{pathKeyImagesGen, defaultPathImagesGen},
		{pathKeyImagesEdits, defaultPathImagesEdits},
	}
	require.Len(t, cases, 7, "7 个格式键必须全部覆盖，新增键时同步此表")
	for _, tc := range cases {
		require.False(t, strings.HasPrefix(tc.tail, "v1/"),
			"%s 的默认尾段 %q 不得含 v1 协议段", tc.key, tc.tail)
		require.False(t, strings.HasPrefix(tc.tail, "/"),
			"%s 的默认尾段 %q 不得带前导斜杠（/v1 由 targetPath 拼）", tc.key, tc.tail)
	}
}

// TestTargetPath 路径解析的全部语义：
//   - 内置端点（非 custom / custom 未配置 / 空值）→ "/v1/" + 尾段
//   - custom 配置了该键 → **原样返回**，不做任何补全（写 v1/x 就是 v1/x，
//     写 x 就是 x：/v1 的补全属于内置端点，与自定义路径无关）
func TestTargetPath(t *testing.T) {
	require.Equal(t, "/v1/"+defaultPathChat, targetPath(nil, pathKeyChat, defaultPathChat))
	require.Equal(t, "/v1/"+defaultPathChat, targetPath(&scheduler.Selection{}, pathKeyChat, defaultPathChat), "nil map 回落默认端点")
	require.Equal(t, "/v1/"+defaultPathChat,
		targetPath(&scheduler.Selection{UpstreamPaths: map[string]string{pathKeyChat: ""}}, pathKeyChat, defaultPathChat), "空值等同未配置")

	custom := &scheduler.Selection{CredentialType: credential.TypeCustom, UpstreamPaths: map[string]string{pathKeyChat: "v1/x"}}
	require.Equal(t, "v1/x", targetPath(custom, pathKeyChat, defaultPathChat), "custom 路径原样透传")

	customNoV1 := &scheduler.Selection{CredentialType: credential.TypeCustom, UpstreamPaths: map[string]string{pathKeyChat: "acme/chat"}}
	require.Equal(t, "acme/chat", targetPath(customNoV1, pathKeyChat, defaultPathChat),
		"不以 v1/ 开头的自定义路径同样原样透传：补 /v1 只属于内置端点")
	require.NotEqual(t, "/v1/acme/chat", targetPath(customNoV1, pathKeyChat, defaultPathChat))

	// 非 custom 类型即使快照里残留配置也必须回落默认（防类型切换后打到旧端点）
	apiKey := &scheduler.Selection{CredentialType: credential.TypeAPIKey, UpstreamPaths: map[string]string{pathKeyChat: "v1/x"}}
	require.Equal(t, "/v1/"+defaultPathChat, targetPath(apiKey, pathKeyChat, defaultPathChat), "非 custom 忽略路径")
}

// TestAnthropicRequiresStreaming 复刻 SDK 判据：时间规则 + 模型级上限表。
// 缺表会让 claude-opus-4* 的 8193..21333 请求从「本地 400」变成「打到上游」，
// 上游可能挂满超时并把客户端参数问题记成账号网络惩罚。
func TestAnthropicRequiresStreaming(t *testing.T) {
	cases := []struct {
		model     string
		maxTokens int64
		want      bool
	}{
		{"claude-sonnet-4-20250514", 21333, false},
		{"claude-sonnet-4-20250514", 21334, true}, // 时间规则
		{"claude-opus-4-20250514", 8192, false},   // 表内上限边界
		{"claude-opus-4-20250514", 8193, true},    // 表内超限
		{"claude-opus-4-0", 9000, true},
		{"anthropic.claude-opus-4-1-20250805-v1:0", 9000, true},
		{"claude-opus-4-1@20250805", 8193, true},
		{"claude-3-5-sonnet", 8000, false},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, anthropicRequiresStreaming(tc.model, tc.maxTokens), "%s/%d", tc.model, tc.maxTokens)
	}
}
