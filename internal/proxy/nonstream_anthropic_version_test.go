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

	"github.com/is7qin/c3api/internal/domain"
)

// anthropicVersionSpy 模拟真实 Anthropic：缺 Anthropic-Version 直接 400。
// 去 SDK 前该头由 anthropic-sdk-go 的默认值补上（DefaultVersion
// "2023-06-01"），客户端从不带它——转换方向（openai 客户端 → anthropic 上游）
// 更是如此。原始出栈后必须由网关显式补回，否则整条 anthropic 非流链路 400。
type anthropicVersionSpy struct {
	mu      sync.Mutex
	version []string
	hits    int
}

func newAnthropicVersionSpy(t *testing.T, body string) (*httptest.Server, *anthropicVersionSpy) {
	t.Helper()
	spy := &anthropicVersionSpy{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := r.Header.Values("Anthropic-Version")
		spy.mu.Lock()
		spy.hits++
		spy.version = append(spy.version, v...)
		spy.mu.Unlock()
		if len(v) == 0 || v[0] == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"missing anthropic-version header"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, spy
}

func (s *anthropicVersionSpy) snapshot() (int, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits, append([]string(nil), s.version...)
}

// TestNonStreamAnthropicVersionHeader anthropic 上游必须收到 anthropic-version：
// 客户端带了 → 原值透传（客户端覆盖默认，SDK 同款语义）；客户端没带 → 网关补
// 默认值。直连与转换两条非流路径都要成立。
func TestNonStreamAnthropicVersionHeader(t *testing.T) {
	// anthropicDefaultVersionForTest 是 SDK DefaultVersion 的独立字面量（不引用生产
	// 常量）：默认值若被改动，这里必须一起改，否则测试变红——这是有意的冗余。
	const anthropicDefaultVersionForTest = "2023-06-01"

	const messBody = `{"id":"msg_1","type":"message","role":"assistant","model":"gpt-4o","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":5}}`

	cases := []struct {
		name        string
		clientPath  string
		body        string
		clientHdr   string                 // 客户端自带的 anthropic-version（空 = 不带）
		dir         domain.ProtocolConvert // 非空 = 走「openai 客户端 → anthropic 上游」的该转换方向
		wantVersion string
	}{
		{
			name: "直连 anthropic，客户端未带头 → 网关补默认", clientPath: "/v1/messages",
			body:        `{"model":"gpt-4o","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			wantVersion: anthropicDefaultVersionForTest,
		},
		{
			name: "直连 anthropic，客户端带头 → 原值透传", clientPath: "/v1/messages",
			body:        `{"model":"gpt-4o","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			clientHdr:   "2024-01-01",
			wantVersion: "2024-01-01",
		},
		{
			name: "转换 chat→mess（客户端从不带头）→ 网关补默认", clientPath: "/v1/chat/completions",
			body:        `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`,
			dir:         domain.ProtocolConvertChatToMess,
			wantVersion: anthropicDefaultVersionForTest,
		},
		{
			name: "转换 resp→mess（客户端从不带头）→ 网关补默认", clientPath: "/v1/responses",
			body:        `{"model":"gpt-4o","input":"hi"}`,
			dir:         domain.ProtocolConvertRespToMess,
			wantVersion: anthropicDefaultVersionForTest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, spy := newAnthropicVersionSpy(t, messBody)
			var p *Proxy
			if tc.dir != "" {
				p = newConvertedTestProxyLogs(t, up.URL, []domain.RequestFormat{domain.FormatAnthropic},
					[]domain.ProtocolConvert{tc.dir}, &captureLogStore{}, 30*time.Second)
			} else {
				tpl := mappingTpl(up.URL, nil)
				tpl.SupportedFormats = []domain.RequestFormat{domain.FormatAnthropic}
				p = newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, &captureLogStore{}, nil)
			}

			req := httptest.NewRequest(http.MethodPost, tc.clientPath, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer ck-1")
			req.Header.Set("x-api-key", "ck-1")
			if tc.clientHdr != "" {
				req.Header.Set("Anthropic-Version", tc.clientHdr)
			}
			rec := httptest.NewRecorder()
			switch tc.clientPath {
			case "/v1/chat/completions":
				p.HandleChat(rec, req)
			case "/v1/responses":
				p.HandleResponses(rec, req)
			default:
				p.HandleAnthropic(rec, req)
			}

			hits, versions := spy.snapshot()
			require.Equal(t, 1, hits, "应恰好一次上游调用；客户端响应=%s", rec.Body.String())
			require.Equal(t, []string{tc.wantVersion}, versions, "上游收到的 anthropic-version")
			require.Equal(t, http.StatusOK, rec.Code,
				"缺 anthropic-version 的上游会 400，客户端必须拿到成功；body=%s", rec.Body.String())
		})
	}
}
