// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestStreamGuardRejectsWithRealRequest spec §8(k)：未接线的方向收到
// `stream:true` 必须返回 400，且**在写出 SSE 头之前**——客户端绝不能拿到
// 「200 + 空流」。
//
// 这条**真的发一次 HTTP 请求**再断言 recorder：只检查注入函数返回 false、
// 或用源码下标比顺序，都证明不了"响应头没被提前写出去"（真实请求下
// `rec.Code` 与 `Content-Type` 是可观察的）。六个方向目前全部接线，故用
// `streamSupported` 注入缝造"未接线"这一态。
func TestStreamGuardRejectsWithRealRequest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pc      domain.ProtocolConvert
		handler func(*Proxy, http.ResponseWriter, *http.Request)
		path    string
		body    string
	}{
		{
			name:    "mess_client",
			pc:      domain.ProtocolConvertMessToChat,
			handler: (*Proxy).HandleAnthropic,
			path:    "/v1/messages",
			body:    `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
		},
		{
			name:    "resp_client",
			pc:      domain.ProtocolConvertRespToChat,
			handler: (*Proxy).HandleResponses,
			path:    "/v1/responses",
			body:    `{"model":"m","input":"hi","stream":true}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := streamSupported
			t.Cleanup(func() { streamSupported = orig })
			streamSupported = func(domain.ProtocolConvert) bool { return false }

			up := &capturedUpstream{}
			srv := up.srv(t)
			defer srv.Close()
			p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIChat},
				[]domain.ProtocolConvert{tc.pc})

			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer ck-1")
			rec := httptest.NewRecorder()
			tc.handler(p, rec, req)

			require.Equal(t, http.StatusBadRequest, rec.Code, "未接线的流式方向必须 400")
			require.NotContains(t, rec.Header().Get("Content-Type"), "text/event-stream",
				"不得写出 SSE 头（否则状态码已经锁成 200）")
			require.Contains(t, rec.Body.String(), "streaming is not supported",
				"错误正文必须说清原因")
			require.NotContains(t, rec.Body.String(), "data: ", "不得产出任何 SSE 帧")

			path, _, _ := up.last(t)
			require.Empty(t, path, "被守卫拦下的请求不该打到上游")
		})
	}
}

// TestStreamGuardAllowsWhenSupported 对照组：能力为 true 时同一请求必须走通
// （否则上面的拒绝可能只是"什么都坏了"）。
func TestStreamGuardAllowsWhenSupported(t *testing.T) {
	up := &capturedUpstream{}
	srv := up.srv(t)
	defer srv.Close()
	p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIChat},
		[]domain.ProtocolConvert{domain.ProtocolConvertMessToChat})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "text/event-stream")
	require.Contains(t, rec.Body.String(), "event: message_start")
}
