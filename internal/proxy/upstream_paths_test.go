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

// TestPathOfFallsBackWhenUnset 未配置/空值时回落默认常量（热路径回落语义单测）。
func TestPathOfFallsBackWhenUnset(t *testing.T) {
	require.Equal(t, defaultPathChat, pathOf(nil, pathKeyChat, defaultPathChat))
	require.Equal(t, defaultPathChat, pathOf(&scheduler.Selection{}, pathKeyChat, defaultPathChat), "nil map 回落默认")
	require.Equal(t, defaultPathChat, pathOf(&scheduler.Selection{UpstreamPaths: map[string]string{pathKeyChat: ""}}, pathKeyChat, defaultPathChat), "空值等同未配置")
	custom := &scheduler.Selection{CredentialType: credential.TypeCustom, UpstreamPaths: map[string]string{pathKeyChat: "v1/x"}}
	require.Equal(t, "v1/x", pathOf(custom, pathKeyChat, defaultPathChat))
	// 非 custom 类型即使快照里残留配置也必须回落默认（防类型切换后打到旧端点）
	apiKey := &scheduler.Selection{CredentialType: credential.TypeAPIKey, UpstreamPaths: map[string]string{pathKeyChat: "v1/x"}}
	require.Equal(t, defaultPathChat, pathOf(apiKey, pathKeyChat, defaultPathChat), "非 custom 忽略路径")
}
