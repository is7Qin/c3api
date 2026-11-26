// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

// request_patch 运行期语义（spec custom-request-patch §7.4/§8/§9 P3）：
//   - failover 每 attempt 用当次 cred 重算闭包（{api_key} 换号换值，禁止一次改写）；
//   - 流式分支同样在既有变换之后、出网之前接装饰器；
//   - sjson 残余失败：裸透传 + 固定文案限频 WARN（不含 path/value/body）；
//   - err_logs / attempt_logs 的上游回显 key 脱敏（客户端的 4xx 原文面不脱敏）；
//   - 无配置 helper 零分配。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/api"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/sserelay"
)

// sequenceUpstream 按顺序回放 status/body 的上游（failover 用例）。
type sequenceUpstream struct {
	mu       sync.Mutex
	hit      int
	statuses []int
	bodies   []string
	headers  []http.Header
}

func newSequenceUpstream(t *testing.T, statuses []int, bodies []string) (*httptest.Server, *sequenceUpstream) {
	t.Helper()
	seq := &sequenceUpstream{statuses: statuses, bodies: bodies}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seq.mu.Lock()
		i := seq.hit
		seq.hit++
		seq.headers = append(seq.headers, r.Header.Clone())
		seq.mu.Unlock()
		status := http.StatusOK
		body := "{}"
		if i < len(seq.statuses) {
			status = seq.statuses[i]
		}
		if i < len(seq.bodies) {
			body = seq.bodies[i]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, seq
}

func (s *sequenceUpstream) snapshot() (int, []http.Header) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]http.Header, len(s.headers))
	copy(out, s.headers)
	return s.hit, out
}

// TestRequestPatchStreamingChat 流式分支接线：既有 setStreamAndModel 之后、出网
// 之前应用 patch；上游收到 patched 头/体，客户端拿到 SSE 200。
func TestRequestPatchStreamingChat(t *testing.T) {
	sse := "data: " + `{"id":"c1","choices":[{"delta":{"content":"hi"}}],"usage":null}` + "\n\ndata: [DONE]\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Stream-Patched") != "1" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if gjson.GetBytes(raw, "streamed_patch").String() != "yes" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sse))
	}))
	t.Cleanup(up.Close)

	tpl := customPatchTpl(t, 1, up.URL, []domain.RequestFormat{domain.FormatOpenAIChat},
		`{"formats":{"openai-chat":{"headers":{"X-Stream-Patched":"1"},"set":{"streamed_patch":"yes"}}}}`)
	p := newPatchProxy(t, tpl, nil, nil, &captureLogStore{}, "", nil)
	rec := postViaRouter(p, "/v1/chat/completions", map[string]string{"Authorization": "Bearer ck-1"},
		[]byte(`{"model":"gpt-4o","stream":true,"messages":[]}`))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "[DONE]")
}

// TestRequestPatchFailoverRecomputesAPIKey failover 每 attempt 按当次 cred 重算
// 闭包：两次上游调用看到不同 key（禁止从 auth 反解、禁止 pipeline 一次改写）。
func TestRequestPatchFailoverRecomputesAPIKey(t *testing.T) {
	up, seq := newSequenceUpstream(t,
		[]int{http.StatusTooManyRequests, http.StatusOK},
		[]string{`{"error":{"message":"rate limited"}}`, `{"id":"c1"}`})
	tpl := customPatchTpl(t, 1, up.URL, []domain.RequestFormat{domain.FormatOpenAIChat},
		`{"headers":{"Authorization":"Bearer {api_key}","X-Vendor-Key":"{api_key}"}}`)
	accs := map[int64][]*domain.Account{10: {
		{ID: 1, TemplateID: tpl.ID, Template: tpl, UpstreamKey: "sk-first", Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 8},
		{ID: 2, TemplateID: tpl.ID, Template: tpl, UpstreamKey: "sk-second", Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 8},
	}}
	p := newPatchProxy(t, tpl, accs, nil, &captureLogStore{}, "", nil)
	rec := postViaRouter(p, "/v1/chat/completions", map[string]string{"Authorization": "Bearer ck-1"},
		[]byte(`{"model":"gpt-4o","messages":[]}`))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	hits, headers := seq.snapshot()
	require.Equal(t, 2, hits, "429 → 换号重试（两次上游调用）")
	require.Equal(t, "Bearer sk-first", headers[0].Get("Authorization"), "第一 attempt 用账号1 原始 cred")
	require.Equal(t, "sk-first", headers[0].Get("X-Vendor-Key"))
	require.Equal(t, "Bearer sk-second", headers[1].Get("Authorization"), "第二 attempt 必须用账号2 原始 cred（每 attempt 重算）")
	require.Equal(t, "sk-second", headers[1].Get("X-Vendor-Key"))
}

// TestRequestPatchResidualFailureWarnOnce sjson 残余失败（脏库路径）：
//   - 请求不中止、body 裸透传（上游收到 = 既有变换后的原文）；
//   - 固定文案限频 WARN 恰好一次（按 template_id+format），且不含 path/value/body。
func TestRequestPatchResidualFailureWarnOnce(t *testing.T) {
	dir, err := os.MkdirTemp("", "c3api-patch-residual-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	logOut := filepath.Join(dir, "warn.log")

	up, capture := newPatchUpstream(t, http.StatusOK, `{"id":"c1"}`)
	// 手工构造脏 patch：x.999999999 超出 sjson 数组 padding 上限，运行期执行失败
	// （保存期本应 400——这里模拟手工改库绕过写面）。
	dirty := &domain.RequestPatch{
		Headers: map[string]string{"X-Patch-Header": "applied"},
		Formats: map[domain.UpstreamPathKey]domain.FormatPatch{
			domain.UpstreamPathChat: {
				Set: map[string]json.RawMessage{"x.999999999": json.RawMessage(`1`), "secret_marker": json.RawMessage(`"must-not-leak"`)},
			},
		},
	}
	tpl := customPatchTpl(t, 990001, up.URL, []domain.RequestFormat{domain.FormatOpenAIChat}, "")
	tpl.Upstream = domain.NewTemplateUpstream(nil, dirty)
	p := newPatchProxy(t, tpl, nil, nil, &captureLogStore{}, logOut, nil)

	body := []byte(`{"model":"gpt-4o","messages":[],"temperature":0.2}`)
	for i := 0; i < 2; i++ {
		rec := postViaRouter(p, "/v1/chat/completions", map[string]string{"Authorization": "Bearer ck-1"}, body)
		require.Equal(t, http.StatusOK, rec.Code, "残余失败不得中止请求（第 %d 次）：%s", i, rec.Body.String())
	}
	reqs := capture.snapshot()
	require.Len(t, reqs, 2)
	for _, req := range reqs {
		require.Equal(t, "applied", req.Header.Get("X-Patch-Header"), "头装饰与 body 失败相互独立")
		require.JSONEq(t, string(body), string(req.Body), "body 残余失败 ⇒ 裸透传（不改写、不中止）")
	}

	data, err := os.ReadFile(logOut)
	require.NoError(t, err)
	logText := string(data)
	require.Equal(t, 1, strings.Count(logText, "request patch body apply failed"),
		"限频 WARN：同一 template+format 一分钟内只一条")
	require.Contains(t, logText, "request patch body apply failed")
	for _, leak := range []string{"999999999", "must-not-leak", "secret_marker", string(body)} {
		require.NotContains(t, logText, leak, "WARN 不得打印 path/value/body")
	}
}

// TestRequestPatchErrLogsRedactKey 上游回显防泄漏（只保日志面）：
//   - 上游在 4xx body 里回显收到的账号 key；
//   - err_logs / attempt_logs 落库前精确替换为 [redacted]；
//   - 返回客户端的 4xx 原文面保持既有契约（不脱敏，spec §8 明示裁决）。
func TestRequestPatchErrLogsRedactKey(t *testing.T) {
	const key = "sk-upstream"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 上游把收到的自定义头原样回显（典型「key invalid」错误体）。
		echo := r.Header.Get("X-Vendor-Key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"error":{"message":"invalid key %s"}}`, echo)
	}))
	t.Cleanup(up.Close)

	tpl := customPatchTpl(t, 1, up.URL, []domain.RequestFormat{domain.FormatOpenAIChat},
		`{"headers":{"Authorization":"Bearer {api_key}","X-Vendor-Key":"{api_key}"}}`)
	store := &captureLogStore{}
	attemptSink := &captureAttemptSink{}
	p := newPatchProxy(t, tpl, nil, nil, store, "", nil)
	p.SetAttemptLogs(attemptSink)

	rec := postViaRouter(p, "/v1/chat/completions", map[string]string{"Authorization": "Bearer ck-1"},
		[]byte(`{"model":"gpt-4o","messages":[]}`))
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), key, "客户端 4xx 原文面不脱敏（既有响应契约）")

	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))

	store.mu.Lock()
	rows := append([]*domain.UsageLog(nil), store.logs...)
	store.mu.Unlock()
	var errRow *domain.UsageLog
	for _, row := range rows {
		if row.ErrorMessage != nil && row.ErrorType == domain.Err4xx {
			errRow = row
		}
	}
	require.NotNil(t, errRow, "4xx 必须落一条 err_logs")
	require.NotContains(t, *errRow.ErrorMessage, key, "err_logs.error_message 不得出现账号 key")
	require.Contains(t, *errRow.ErrorMessage, "[redacted]", "脱敏替换为固定文本")

	attemptRows := attemptSink.snapshot()
	require.NotEmpty(t, attemptRows, "4xx dispatch 应落 attempt_logs 行")
	require.NotNil(t, attemptRows[len(attemptRows)-1].ErrorMessage)
	require.NotContains(t, *attemptRows[len(attemptRows)-1].ErrorMessage, key, "attempt_logs.error_message 同步脱敏")
}

// TestRequestPatchHelpersNoConfigZeroAllocs 无配置零成本（spec A5/§7.1）：
// headerDecorator 返回 nil、bodyPatch 返回原切片，全部 0 分配；有配置时每 attempt
// 至多 1 枚闭包（bodyPatch 无 patch 键时也不分配）。
func TestRequestPatchHelpersNoConfigZeroAllocs(t *testing.T) {
	plain := selectionForPatchTest()
	body := []byte(`{"model":"gpt-4o","messages":[]}`)
	allocs := testing.AllocsPerRun(100, func() {
		if dec := headerDecorator(plain, patchKeyChat, "sk-x"); dec != nil {
			t.Fatalf("无配置 headerDecorator 必须返回 nil")
		}
		got := (&Proxy{}).bodyPatch(plain, patchKeyChat, body)
		if &got[0] != &body[0] {
			t.Fatalf("无配置 bodyPatch 必须返回原 slice")
		}
	})
	require.Zero(t, allocs, "无配置热路径必须零分配（闭包/切片/解析都不许有）")

	configured := selectionForPatchTest()
	configured.Upstream = &domain.TemplateUpstream{Patch: &domain.RequestPatch{
		Headers: map[string]string{"X-A": "1"},
		Formats: map[domain.UpstreamPathKey]domain.FormatPatch{
			domain.UpstreamPathChat: {Headers: map[string]string{"X-B": "2"}},
		},
	}}
	// 只量**我们自己的**构造开销（闭包）：apply 阶段的 Header.Set 分配属
	// net/http/textproto 既有语义（任何写头都会分配新值 slice），不计入本预算。
	var sinkDec func(*http.Request)
	allocs = testing.AllocsPerRun(100, func() {
		sinkDec = headerDecorator(configured, patchKeyChat, "sk-x")
		if sinkDec == nil {
			t.Fatalf("有配置必须返回非 nil 闭包")
		}
	})
	require.LessOrEqual(t, allocs, 1.0, "有配置但无占位符：每 attempt ≤1 个闭包分配")
	require.NotNil(t, sinkDec)

	// bodyPatch 在配置存在但该格式层为空时同样零分配（只做一次 map 查）。
	configured.Upstream.Patch.Formats = nil
	allocs = testing.AllocsPerRun(100, func() {
		got := (&Proxy{}).bodyPatch(configured, patchKeyChat, body)
		if &got[0] != &body[0] {
			t.Fatalf("该格式无 patch 键 ⇒ 原 slice")
		}
	})
	require.Zero(t, allocs, "配置存在但本格式无 body patch ⇒ 零分配")
}

// selectionForPatchTest 无上游配置的 Selection（零值 + 类型，供 helper 判定用）。
func selectionForPatchTest() *api.Selection {
	return &api.Selection{CredentialType: credential.TypeCustom, UpstreamKey: "sk-x"}
}

// TestRequestPatchStreamAbortWarnRedacted 上游在流内错误载荷里回显账号 key 时，
// 进程 WARN 行（recordStreamAbort）同样在落日志前精确替换（spec §8「打日志前」；
// err_logs 面由 TestRequestPatchErrLogsRedactKey 覆盖）。
func TestRequestPatchStreamAbortWarnRedacted(t *testing.T) {
	dir, err := os.MkdirTemp("", "c3api-patch-warn-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	logOut := filepath.Join(dir, "warn.log")

	up, _ := newPatchUpstream(t, http.StatusOK, `{}`)
	tpl := customPatchTpl(t, 1, up.URL, []domain.RequestFormat{domain.FormatOpenAIChat}, "")
	p := newPatchProxy(t, tpl, nil, nil, &captureLogStore{}, logOut, nil)

	sel := &api.Selection{
		TemplateID: 1, AccountID: 1, UpstreamKey: "sk-upstream",
		Format: domain.FormatOpenAIChat, Model: "gpt-4o",
	}
	p.recordStreamAbort(context.Background(), "req-warn-1", 10, time.Now(), sel, "gpt-4o", usageTuple{}, CommitNotSent,
		&sserelay.PayloadError{Type: "server_error", Message: "invalid key sk-upstream"})

	data, err := os.ReadFile(logOut)
	require.NoError(t, err)
	logText := string(data)
	require.Contains(t, logText, "upstream stream aborted")
	require.NotContains(t, logText, "sk-upstream", "进程 WARN 行不得出现账号 key")
	require.Contains(t, logText, "[redacted]")
}
