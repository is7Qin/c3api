// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

// pipeline_retry_5xx_test.go 是「5xx 换号重试」的端到端钉：
//   - 502 零业务字节 ⇒ 换号一次（第二次成功），只扣一次费、usage_logs 只 1 行；
//   - 已写业务字节的失败 ⇒ 不换号（客户端收到已写内容，无第二次上游调用）。
//
// 诚实边界：HTTP 语义下「带 5xx 状态且已写业务字节」不可能同时成立（写出字节
// 即已定态 200）。因此「已写字节」的真实形态是**已提交后失败**（response_started
// / sent_ambiguous 收尾），本文件按该形态做端到端；而「502 + 已写字节」这种
// 组合形状由 retry_matrix_5xx_test.go 的 #12/#27/#28 在类型层钉死（形状非法，
// 矩阵前置字节闸门也不放行）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// chatAccount 构造组 10 的 chat 账号（tpl 决定 BaseURL/模板 id）。
func chatAccount(id int64, tpl *domain.Template) *domain.Account {
	return &domain.Account{
		ID: id, TemplateID: tpl.ID, Template: tpl, UpstreamKey: "sk-upstream",
		Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 4,
	}
}

// chatUpstreamTemplate 构造 chat 模板（组内每个账号一份，BaseURL 各异）。
func chatUpstreamTemplate(id int64, baseURL string) *domain.Template {
	return &domain.Template{
		ID: id, Name: fmt.Sprintf("t%d", id), BaseURL: baseURL,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
	}
}

// fakeUpstreamStatus 恒返回指定状态码的上游（计次；5xx 换号/不换号用例共用）。
func fakeStatusUpstream(counter *atomic.Int64, status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream status"}}`))
	}))
}

// fakeBadGateway 恒 502 的上游（5xx 白名单内的「网关坏响应」）。
func fakeBadGateway(counter *atomic.Int64) *httptest.Server {
	return fakeStatusUpstream(counter, http.StatusBadGateway)
}

// fakeChatOK 正常 chat 上游（非流式 3/5 token ⇒ 单次成本 130 毫分）。
func fakeChatOK(counter *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "c1", "object": "chat.completion", "model": body["model"],
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 5, "total_tokens": 8},
		})
	}))
}

func chatRequest(auth string, stream bool) *http.Request {
	body := `{"model":"gpt-4o","messages":[]}`
	if stream {
		body = `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+auth)
	return req
}

// TestPipelineRetry5xxZeroByteFailsOverOnce 502 零字节 ⇒ 换号一次、第二次成功。
// 「只扣一次费」用两条独立证据：
//  1. usage_logs 只有 1 行，且该行 Cost 恰等于**一次**请求的价格（130 毫分，
//     不是失败尝试 + 成功尝试的 260）；
//  2. 配额护栏：额度 260 的 key 连续两次「502 → 200」都成功（若失败尝试也计费，
//     第二次就会 429），第三次才 429——同一额度恰被扣两次（每请求一次）。
func TestPipelineRetry5xxZeroByteFailsOverOnce(t *testing.T) {
	t.Run("usage_logs 只 1 行且只扣一次", func(t *testing.T) {
		var hitsA, hitsB atomic.Int64
		upA := fakeBadGateway(&hitsA)
		defer upA.Close()
		upB := fakeChatOK(&hitsB)
		defer upB.Close()

		store := &captureLogStore{}
		bal := billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{1: 50000}}, nil)
		require.NoError(t, bal.Reload(context.Background()))
		p := newTestProxyBillingKeys(t, map[string]domain.KeyMeta{"ck-1": activeKey(1, 1, 10)},
			map[int64][]*domain.Account{10: {
				chatAccount(1, chatUpstreamTemplate(1, upA.URL)),
				chatAccount(2, chatUpstreamTemplate(2, upB.URL)),
			}}, bal, store)

		rec := httptest.NewRecorder()
		p.HandleChat(rec, chatRequest("ck-1", false))

		require.Equal(t, http.StatusOK, rec.Code, "502 零字节换号后第二次成功。body=%s", rec.Body.String())
		require.Equal(t, int64(1), hitsA.Load(), "首个账号恰派发一次")
		require.Equal(t, int64(1), hitsB.Load(), "换号恰发生一次（第二次上游调用）")

		require.NoError(t, p.rec.Close(context.Background()))
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 1, "usage_logs 只 1 行（失败尝试不产生计费行）")
		row := store.logs[0]
		require.Equal(t, int64(2), row.AccountID, "日志/计费归属取获胜尝试")
		require.Equal(t, int64(130), row.Cost, "一次费用（3×1e7+5×2e7=130 毫分），不是两次")
		require.Equal(t, http.StatusOK, row.StatusCode)
	})

	t.Run("配额只被扣一次（第二次仍放行）", func(t *testing.T) {
		testHealthSink.reset()
		var hitsA, hitsB atomic.Int64
		upA := fakeBadGateway(&hitsA)
		defer upA.Close()
		upB := fakeChatOK(&hitsB)
		defer upB.Close()

		meta := activeKey(1, 1, 10)
		meta.HasQuota = true
		meta.Quota = 260 // 恰好两次 130
		bal := billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{1: 50000}}, nil)
		require.NoError(t, bal.Reload(context.Background()))
		store := &captureLogStore{}
		p := newTestProxyBillingKeys(t, map[string]domain.KeyMeta{"ck-1": meta},
			map[int64][]*domain.Account{10: {
				chatAccount(1, chatUpstreamTemplate(1, upA.URL)),
				chatAccount(2, chatUpstreamTemplate(2, upB.URL)),
			}}, bal, store)
		// 首账号的 502 换号后仍可由第二账号服务；两个请求各扣 130。
		p.cfg.FailoverAttempts = 2

		for i, want := range []int{http.StatusOK, http.StatusOK} {
			rec := httptest.NewRecorder()
			p.HandleChat(rec, chatRequest("ck-1", false))
			require.Equal(t, want, rec.Code, "第 %d 次请求（每请求只扣一次费；双扣会让第二次 429）。body=%s", i+1, rec.Body.String())
		}
		// 额度护栏真实性：第三次必须 429（额度已归零，证明前面确实各扣了一次）。
		rec := httptest.NewRecorder()
		p.HandleChat(rec, chatRequest("ck-1", false))
		require.Equal(t, http.StatusTooManyRequests, rec.Code, "额度 260 已用尽。body=%s", rec.Body.String())
	})
}

// TestPipelineRetry5xxWrittenBytesNoFailover 已写业务字节的失败 ⇒ 不换号：
// 客户端拿到已写内容，第二个账号一次都没被调用。
func TestPipelineRetry5xxWrittenBytesNoFailover(t *testing.T) {
	testHealthSink.reset()
	up, rec := fakeAcceptedAbortOpenAI(t) // 下游确认首帧后上游截断响应体
	defer up.Close()
	var hitsB atomic.Int64
	upB := fakeChatOK(&hitsB)
	defer upB.Close()

	p := newTestProxy(t, up.URL, 1)
	loader := p.sched.Loader().(noopLoader)
	loader.accs[10] = append(loader.accs[10], chatAccount(2, chatUpstreamTemplate(2, upB.URL)))
	require.NoError(t, p.sched.InvalidateAllSync())
	publishTestRoutes(t, p.sched)

	p.HandleChat(rec, chatRequest("ck-1", true))

	body := rec.Body.String()
	require.Contains(t, body, `"content":"hi"`, "客户端必须收到已写出的业务内容")
	require.Zero(t, hitsB.Load(), "已写字节的失败不得换号：第二个账号零次上游调用")
	require.Equal(t, 1, p.rec.Pending(), "已写字节的失败按 abort 记一条用量（不重复计费）")
	for _, id := range []int64{1, 2} {
		ri, ok := p.sched.Runtime(id)
		require.True(t, ok)
		require.Zero(t, ri.Concurrency, "账号 %d 并发槽必须释放", id)
	}
	require.NoError(t, p.errlog.Close(context.Background()))
}

// TestPipelineRetry5xxNonRetryableStatusNoFailover 非白名单 5xx（501/505）不换号：
// 端到端确认「放开 5xx」没有把确定性不兼容也一起放开。
func TestPipelineRetry5xxNonRetryableStatusNoFailover(t *testing.T) {
	for _, status := range []int{http.StatusNotImplemented, http.StatusHTTPVersionNotSupported} {
		t.Run(fmt.Sprintf("%d", status), func(t *testing.T) {
			testHealthSink.reset()
			var hitsA, hitsB atomic.Int64
			upA := fakeStatusUpstream(&hitsA, status)
			defer upA.Close()
			upB := fakeChatOK(&hitsB)
			defer upB.Close()

			p := newTestProxy(t, upA.URL, 1)
			loader := p.sched.Loader().(noopLoader)
			loader.accs[10] = append(loader.accs[10], chatAccount(2, chatUpstreamTemplate(2, upB.URL)))
			require.NoError(t, p.sched.InvalidateAllSync())
			publishTestRoutes(t, p.sched)
			p.cfg.FailoverAttempts = 3

			rec := httptest.NewRecorder()
			p.HandleChat(rec, chatRequest("ck-1", false))

			require.Equal(t, int64(1), hitsA.Load(), "首账号恰一次")
			require.Zero(t, hitsB.Load(), "%d 不在可重试集合：不得换号", status)
			require.NotEqual(t, http.StatusOK, rec.Code, "客户端拿到失败态")
		})
	}
}
