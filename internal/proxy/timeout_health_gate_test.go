// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
)

// --- timeout 的三条门禁（spec-commit-state-and-round-billing §3.1 / §5） ---
//
// ① 零帧网关预算到期（非流式，pipeline 归类）⇒ error_type=timeout + KindTimeout
// ② SSE 已发业务帧的 timeout ⇒ **不投递**健康事件（不冷却）
// ③ SSE 零帧 timeout ⇒ 投递健康事件，且 kind 必须是 timeout（不是 network）

// stallNoHeaders 接受连接后连响应头都不发：制造「上游一点都没产出」的非流式现场。
//
// 为什么不用 `<-r.Context().Done()` 等上游：本机 Go 版本在客户端 ctx 到期时**不**会
// 关闭「已发出请求、仍在等响应头」的连接（实测：http.Client.Do 已返回错误，服务端
// handler 的 r.Context() 仍活着）——只等 ctx 会让 httptest.Server.Close 永久阻塞。
// 因此由测试收尾关闭 release；t.Cleanup 是 LIFO，close(release) 后注册 ⇒ 先于
// srv.Close 执行，handler 先退出再关服务器。
func stallNoHeaders(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv
}

// stallSSE 先提交 SSE 响应头（200）再保持静默：SSE 侧的「零业务帧」现场。
// frames 非空时先下发给定帧（= 已交付过业务帧的现场）。release 语义同上。
func stallSSE(t *testing.T, frames ...string) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fl.Flush()
		for _, f := range frames {
			fmt.Fprint(w, f)
			fl.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv
}

func timeoutGateTemplate(t *testing.T, upstream string) *domain.Template {
	t.Helper()
	return &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
	}
}

// G-timeout-①：非流式零帧 + 网关预算（UpstreamTimeout）到期 ⇒ pipeline 的零状态
// 归类必须是 timeout（命中 seed-timeout 10m），不是 network（seed-network 5s）。
//
// 旧实现：`statusFor`/`writeFailoverExhausted` 只看 code==0 ⇒ 一律 network，本用例红。
func TestTimeoutZeroFrameGatewayBudgetIsTimeout(t *testing.T) {
	testHealthSink.reset()
	up := stallNoHeaders(t)
	store := &captureLogStore{}
	p := newTestProxyTplTimeoutLogs(t, timeoutGateTemplate(t, up.URL), 1, true, 30*time.Second, store, nil)
	p.cfg.UpstreamTimeout = 100 * time.Millisecond // 缩短非流式预算（等价直构）
	p.cfg.FailoverAttempts = 1                     // 单次尝试即耗尽，断言面唯一

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	rows := uniqueRows(t, drainLogs(t, p, store))
	require.Len(t, rows, 1, "零帧失败只进 err_logs 一行；rows=%+v", rows)
	row := rows[0]
	require.Equal(t, domain.ErrTimeout, row.ErrorType,
		"零帧网关预算到期 ⇒ timeout（旧实现记 network：把「零产出」罚成 5s）")
	require.Equal(t, 0, row.StatusCode, "上游没给过状态码 ⇒ 0（不合成）")
	requireCommitState(t, row, CommitNotSent)
	require.NotNil(t, row.ErrorMessage)
	require.Equal(t, msgUpstreamTimeoutNoOutput, *row.ErrorMessage, "§3.2：可读文案")
	require.Equal(t, http.StatusBadGateway, rec.Code, "seed-timeout 归一 502")

	p.sched.FlushRules()
	events := testHealthSink.eventsFor(1)
	require.Len(t, events, 1, "零帧失败必须投递一次健康事件；events=%+v", events)
	require.Equal(t, rule.KindTimeout, events[0].kind, "冷却类别必须是 timeout（seed-timeout 10m）")
}

// G-timeout-②：SSE 已写出业务帧后网关预算到期 ⇒ 只记日志、**不投递**健康事件。
//
// 旧实现：`streamFailureHealth(f)` 无条件投递（KindNetwork）⇒ 已交付的连接被罚
// 10 分钟；本用例在旧实现上红（events 非空）。
func TestTimeoutHealthSSEWithFramesIsSilent(t *testing.T) {
	testHealthSink.reset()
	up := stallSSE(t, `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{"content":"hi"}}],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`+"\n\n")
	store := &captureLogStore{}
	p := newTestProxyTplTimeoutLogs(t, timeoutGateTemplate(t, up.URL), 1, true, 100*time.Millisecond, store, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	rows := uniqueRows(t, drainLogs(t, p, store))
	require.Len(t, rows, 1, "rows=%+v", rows)
	require.Equal(t, domain.ErrTimeout, rows[0].ErrorType, "网关预算到期 ⇒ timeout")
	require.NotNil(t, rows[0].CommitState)
	require.Equal(t, "sent_ambiguous", *rows[0].CommitState, "已写业务帧 ⇒ 可计费（交付事实与冷却正交）")

	p.sched.FlushRules()
	require.Empty(t, testHealthSink.eventsFor(1),
		"已发业务帧的 timeout 不冷却账号（旧实现无条件投递 KindNetwork）")
}

// G-timeout-③：SSE 零业务帧 + 网关预算到期 ⇒ 必须投递健康事件，且 kind=timeout。
//
// 旧实现：事件 kind 是 network（seed-network 5s）⇒ 本用例红。
func TestTimeoutHealthSSEZeroFrameKeepsKind(t *testing.T) {
	testHealthSink.reset()
	up := stallSSE(t) // 已提交响应头、零业务帧
	store := &captureLogStore{}
	p := newTestProxyTplTimeoutLogs(t, timeoutGateTemplate(t, up.URL), 1, true, 100*time.Millisecond, store, nil)
	p.cfg.FailoverAttempts = 1

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	rows := uniqueRows(t, drainLogs(t, p, store))
	require.Len(t, rows, 1, "rows=%+v", rows)
	require.Equal(t, domain.ErrTimeout, rows[0].ErrorType,
		"零帧网关预算到期 ⇒ timeout（未提交路径同样靠 markGatewayBudget 带事实）")
	requireCommitState(t, rows[0], CommitNotSent)

	p.sched.FlushRules()
	events := testHealthSink.eventsFor(1)
	require.Len(t, events, 1, "零帧 timeout 必须投递健康事件；events=%+v", events)
	require.Equal(t, rule.KindTimeout, events[0].kind, "冷却类别必须是 timeout（旧实现 network）")
}
