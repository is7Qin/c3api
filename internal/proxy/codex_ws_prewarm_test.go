// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
)

// --- codex-sdk HTTP→WS 预热（新建连接且一轮首请求先发 generate:false）在网关侧的
// 失败语义：预热阶段真实 response.create 尚未发出，一律是提交前失败。---

// TestCodexWSPrewarmFailureIsPreCommit 预热阶段失败：
//   - 预热收到 429 error 帧：SDK 以 *HTTPError 在提交前返回 ⇒ 网关按上游 429 记录、
//     规则事件 kind=429、客户端看到 429（单候选），上游零真实 create；
//   - 预热收到 429 且有第二候选：提交前失败可换号 ⇒ 第二账号成功、客户端 200；
//   - 预热阶段连接断开（无应答直接关闭）：非 *HTTPError 的连接错误（SDK 文案前缀
//     “websocket 预热未完成（真实请求未发出）”，错误链保留）⇒ 提交前 5xx，上游零
//     真实 create。
func TestCodexWSPrewarmFailureIsPreCommit(t *testing.T) {
	t.Run("prewarm_429_single_candidate", func(t *testing.T) {
		testHealthSink.reset()
		up, mock := newCodexWSUpstream(t, nil, 0)
		mock.prewarmScript = []string{wsErrFrame429}
		store := &captureLogStore{}
		p, _ := newWSErrProxy(t, up.URL, map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-10")}, store)
		pinWSRoute(p, 10)
		srv := httptest.NewServer(AIRouter(p))
		defer srv.Close()

		resp := postResponses(t, srv, `{"model":"gpt-4o","input":"hi","stream":true}`)
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.NoError(t, err)
		require.Equal(t, http.StatusTooManyRequests, resp.StatusCode, "预热 429 在提交前返回 ⇒ 客户端看到 429；body=%q", body)
		require.Len(t, mock.prewarmsSnapshot(), 1)
		require.Empty(t, mock.framesSnapshot(), "预热被拒 ⇒ 真实 create 未发出")

		require.NoError(t, p.rec.Close(context.Background()))
		waitStoreLogs(t, store, 1)
		rows := uniqueRows(t, storeRows(store))
		require.Len(t, rows, 1, "rows=%+v", rows)
		require.Equal(t, domain.Err429, rows[0].ErrorType)
		require.Equal(t, http.StatusTooManyRequests, rows[0].StatusCode, "归因状态 = 上游真值")

		p.sched.FlushRules()
		events := testHealthSink.eventsFor(10)
		require.Len(t, events, 1, "events=%+v", events)
		require.Equal(t, rule.Kind429, events[0].kind)
	})

	t.Run("prewarm_429_fails_over", func(t *testing.T) {
		testHealthSink.reset()
		up, mock := newCodexWSUpstream(t, nil, 0)
		mock.prewarmScript = []string{wsErrFrame429}
		mock.prewarmScriptLimit = 1
		store := &captureLogStore{}
		p, _ := newWSErrProxy(t, up.URL, map[int64]*domain.AccountExt{
			10: codexPATExt(10, "pat-10"),
			11: codexPATExt(11, "pat-11"),
		}, store)
		pinWSRoute(p, 10, 11)
		srv := httptest.NewServer(AIRouter(p))
		defer srv.Close()

		resp := postResponses(t, srv, `{"model":"gpt-4o","input":"hi","stream":true}`)
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, "提交前 429 ⇒ 换号成功；body=%q", body)
		require.Len(t, mock.prewarmsSnapshot(), 2, "两个账号各自新连接预热一次")
		require.Len(t, mock.framesSnapshot(), 1, "只有第二账号发出真实 create")
		require.Equal(t, 2, mock.upgradesN())
		require.Equal(t, "Bearer pat-10", mock.header(0).Get("Authorization"))
		require.Equal(t, "Bearer pat-11", mock.header(1).Get("Authorization"))

		p.sched.FlushRules()
		events := testHealthSink.eventsFor(10)
		require.Len(t, events, 1, "events=%+v", events)
		require.Equal(t, rule.Kind429, events[0].kind, "第一账号按 429 计健康")
	})

	t.Run("prewarm_conn_failure", func(t *testing.T) {
		testHealthSink.reset()
		up, mock := newCodexWSUpstream(t, nil, 0)
		mock.prewarmScript = []string{}
		store := &captureLogStore{}
		p, _ := newWSErrProxy(t, up.URL, map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-10")}, store)
		pinWSRoute(p, 10)
		srv := httptest.NewServer(AIRouter(p))
		defer srv.Close()

		resp := postResponses(t, srv, `{"model":"gpt-4o","input":"hi","stream":true}`)
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.NoError(t, err)
		require.GreaterOrEqual(t, resp.StatusCode, 500, "预热阶段连接错误 ⇒ 提交前 5xx；body=%q", body)
		require.NotEmpty(t, mock.prewarmsSnapshot())
		require.Empty(t, mock.framesSnapshot(), "预热未完成 ⇒ 真实 create 未发出")
	})
}
