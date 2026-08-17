// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
)

// --- 假上游：SSE 流式 chat/completions ---
// failMode: "" = 正常；"429" = 每个非流式请求都返回 429（测 failover）；
// "500" = 每个非流式请求都返回 500（测 连接级/5xx 分流→502）；
// "400" = 每个非流式请求都返回 400（测 4xx 透传、不转移）；
// "400-stream" = 每个流式请求都返回 400（测流式 4xx 透传）；
// "abort-stream" = 流式响应发完一部分后 panic 断开连接（chunked 帧未终结 →
// relay 读到读错误，测中止路径；旧实现靠非法 JSON 事件触发 SDK 解码失败）；
// "stall-stream" = 流式首帧后不再发送任何字节（测 UpstreamStreamTimeout 超时）。
func fakeOpenAI(t *testing.T, failMode string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-upstream" {
			w.WriteHeader(401)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		stream, _ := body["stream"].(bool)
		if failMode == "429" && !stream {
			w.Header().Set("x-ratelimit-reset-requests", "5s")
			w.WriteHeader(429)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "rate limited"}})
			return
		}
		if failMode == "500" && !stream {
			w.WriteHeader(500)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "boom"}})
			return
		}
		if failMode == "400" && !stream {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "bad request"}})
			return
		}
		if failMode == "400-stream" && stream {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "bad request"}})
			return
		}
		if failMode == "stall-stream" && stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fl := w.(http.Flusher)
			fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{"content":"hi"}}],"usage":null}`+"\n\n")
			fl.Flush()
			// 首帧后不再发送任何字节：relay 阻塞在读上直到 ctx 超时
			// （UpstreamStreamTimeout），模拟上游停滞。代理端 ctx 超时后
			// 传输层取消连接 → 本处理器 r.Context() 结束，随之返回。
			<-r.Context().Done()
			return
		}
		if failMode == "abort-stream" && stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fl := w.(http.Flusher)
			fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{"content":"hi"}}],"usage":null}`+"\n\n")
			fl.Flush()
			// 已发出部分流后 panic：连接被服务器强制关闭，relay 读到读错误
			// （chunked 传输未写终结帧 → io.ErrUnexpectedEOF）。relay 原样
			// 转发字节不解析事件，必须用真实读错误触发中止路径。
			panic("abort-stream: connection cut mid-stream")
		}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fl := w.(http.Flusher)
			chunks := [2]string{
				`{"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{"content":"hi"}}],"usage":null}`,
				`{"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`,
			}
			for _, c := range chunks {
				fmt.Fprintf(w, "data: %s\n\n", c)
				fl.Flush()
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "c1", "object": "chat.completion",
			"model": body["model"],
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 5, "total_tokens": 8},
		})
	}))
	return srv
}

// noopKeyLoader 测试 helper（Phase 3a：LoadGroupKeys → LoadKeys 形态升级）。
type noopKeyLoader struct{ keys map[string]domain.KeyMeta }

func (n noopKeyLoader) LoadKeys(ctx context.Context) (map[string]domain.KeyMeta, error) {
	return n.keys, nil
}

// noopUserLoader 用户快照 helper（NewAuth 签名扩展；status+role 单次查找）。
type noopUserLoader struct{ users map[int64]domain.UserSnapshot }

func (n noopUserLoader) LoadUsers(ctx context.Context) (map[int64]domain.UserSnapshot, error) {
	return n.users, nil
}

// activeKey 构造启用态 KeyMeta（测试默认；门禁上限 0 = 不限，行为与旧
// LoadGroupKeys 等价）。
func activeKey(keyID, userID, groupID int64) domain.KeyMeta {
	return domain.KeyMeta{
		KeyID: keyID, UserID: userID, GroupID: groupID,
		KeyStatus: domain.KeyStatusActive, UserStatus: domain.UserStatusActive,
	}
}

type noopLogStore struct{}

func (noopLogStore) InsertBatch(ctx context.Context, l []*domain.UsageLog) error { return nil }

type noopLoader struct{ accs map[int64][]*domain.Account }

func (n noopLoader) LoadGroupsAccounts(ctx context.Context) (map[int64][]*domain.Account, error) {
	return n.accs, nil
}
func (n noopLoader) LoadGroupAccounts(ctx context.Context, id int64) ([]*domain.Account, error) {
	return n.accs[id], nil
}
func (n noopLoader) UpdateAccountStatus(ctx context.Context, id int64, s domain.AccountStatus, c *time.Time, e *string, w *int) error {
	return nil
}

// fakeRuleStore 内存 RuleStore：种子写入（值语义副本）。
type fakeRuleStore struct {
	mu    sync.Mutex
	rules map[int64]domain.Rule
	next  int64
}

func (f *fakeRuleStore) ListRules(ctx context.Context, enabled *bool) ([]domain.Rule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.Rule, 0, len(f.rules))
	for _, r := range f.rules {
		if enabled != nil && r.Enabled != *enabled {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeRuleStore) CreateRule(ctx context.Context, r domain.Rule) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r.ID = f.next
	f.next++
	f.rules[r.ID] = r
	return r.ID, nil
}

func (f *fakeRuleStore) UpdateRule(ctx context.Context, r domain.Rule) error { return nil }
func (f *fakeRuleStore) DeleteRule(ctx context.Context, id int64) error      { return nil }
func (f *fakeRuleStore) DeleteRulesBatch(ctx context.Context, ids []int64) error {
	return nil
}
func (f *fakeRuleStore) CountRules(ctx context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.rules)), nil
}

func newTestProxy(t *testing.T, upstream string, accountID int64) *Proxy {
	t.Helper()
	return newTestProxyCapture(t, upstream, accountID, true)
}

func newTestProxyCapture(t *testing.T, upstream string, accountID int64, usageCapture bool) *Proxy {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
	}
	return newTestProxyTplCapture(t, tpl, accountID, usageCapture)
}

// newTestProxyTimeoutLogs 同 newTestProxy，但注入 LogInserter（模型语义断言用捕获实现）。
func newTestProxyTimeoutLogs(t *testing.T, upstream string, accountID int64, logs usage.LogInserter) *Proxy {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
	}
	return newTestProxyTplTimeoutLogs(t, tpl, accountID, true, 30*time.Second, logs, nil)
}

// newTestProxyTplCapture 用自定义模板构造测试代理（ModelMapping 等定制场景用）。
func newTestProxyTplCapture(t *testing.T, tpl *domain.Template, accountID int64, usageCapture bool) *Proxy {
	t.Helper()
	return newTestProxyTplTimeoutLogs(t, tpl, accountID, usageCapture, 30*time.Second, noopLogStore{}, nil)
}

// noopErrLogStore 空 errlog 写者（无错误明细断言需求的测试代理用）。
type noopErrLogStore struct{}

func (noopErrLogStore) InsertBatch(ctx context.Context, l []*domain.UsageLog) error { return nil }

// errLogStoreFrom 从 LogInserter 提取 errlog 写者（分表设计后错误明细断言与
// usage_logs 明细断言共用同一 captureLogStore——该类型同时实现 LogInserter 与
// ErrLogInserter；其他写者 → no-op）。错误路径行（4xx/5xx/network/abort）经
// 测试代理内 errlog worker 落同一 store。
func errLogStoreFrom(logs usage.LogInserter) usage.ErrLogInserter {
	if cs, ok := logs.(*captureLogStore); ok {
		return cs
	}
	return noopErrLogStore{}
}

// newTestProxyTplTimeoutLogs 用自定义模板、流式超时与日志存储构造测试代理
// （流式超时回归、用量值断言场景用；默认 30s 超时走 newTestProxyTplCapture）。
// bill 为计费钩子（nil = 计费全关，默认测试路径）。
func newTestProxyTplTimeoutLogs(t *testing.T, tpl *domain.Template, accountID int64, usageCapture bool, streamTimeout time.Duration, logs usage.LogInserter, bill *BillingHooks) *Proxy {
	t.Helper()
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, logs, nil)
	return newTestProxyTplTimeoutRec(t, tpl, accountID, usageCapture, streamTimeout, rec, bill, errLogStoreFrom(logs))
}

// newTestProxyTplTimeoutRec 同 newTestProxyTplTimeoutLogs，但注入完整 Recorder
// （P2a 拒绝路径统计聚合断言用：调用方构造 rec——含捕获 stat store——后同一
// 实例挂 Flusher 与 proxy.rec，聚合单面可观测）。errWriter 为 errlog worker
// 写者（captureLogStore 时与 logs 同一 store；nil = no-op——错误明细不捕获）。
func newTestProxyTplTimeoutRec(t *testing.T, tpl *domain.Template, accountID int64, usageCapture bool, streamTimeout time.Duration, rec *usage.Recorder, bill *BillingHooks, errWriter usage.ErrLogInserter) *Proxy {
	t.Helper()
	accs := map[int64][]*domain.Account{10: {{
		ID: accountID, TemplateID: tpl.ID, Template: tpl, UpstreamKey: "sk-upstream",
		Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4,
	}}}
	cfg := Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: streamTimeout,
		GroupKeyRPM:           0, UsageCapture: usageCapture,
	}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background())) // 空表写种子（429/30s、error/5s、ok/active）
	sched := scheduler.New(scheduler.Config{
		DefaultMaxConcurrency: 4, SyncInterval: time.Hour,
	}, noopLoader{accs: accs}, re, nil)
	require.NoError(t, sched.InvalidateAllSync())
	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{
		"ck-1": activeKey(1, 1, 10),
	}}, noopUserLoader{}, nil)
	require.NoError(t, auth.Reload(context.Background())) // 构造不再自载——测试显式首刷（快照注册表单一入口）
	hc := &http.Client{Transport: http.DefaultTransport}
	clients := aiclient.NewFactory(hc, aiclient.Config{
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: streamTimeout,
	})
	// errlog worker（分表设计）：错误明细落盘通道——与 usage_logs 共用捕获
	// store；FlushInterval 小时级（不自动落盘，测试经 p.errlog.Close 显式排空）。
	if errWriter == nil {
		errWriter = noopErrLogStore{}
	}
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, FlushInterval: time.Hour}, errWriter, nil)
	return New(cfg, sched, credential.New(), rec, clients, auth, nil, bill, errlogW)
}

func TestProxyStreamingChat(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyTimeoutLogs(t, up.URL, 1, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	body := rec.Body.String()
	require.Contains(t, body, "data: [DONE]")
	require.Contains(t, body, `"content":"hi"`)
	require.Contains(t, body, `"prompt_tokens":5`, "usage captured from final chunk")
	// 成功路径必须释放并发槽并记录用量
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "成功路径必须释放并发槽")
	require.Equal(t, 1, p.rec.Pending(), "成功路径必须记录一条用量")
	// 评审 I-1：日志 Model=客户端请求模型（无映射 → MappedModel 空）
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1, "must capture exactly one usage log")
	require.Equal(t, "gpt-4o", store.logs[0].Model, "成功流式：Model = 客户端请求模型")
	require.Equal(t, "", store.logs[0].MappedModel, "无映射 → MappedModel 空")
}

// 兼容性钉（Task 3）：流式转发必须原样保留上游原始字节——chat 是 data-only
// SSE（无 event: 行），[DONE] 与 usage 字段完整透传，成功路径释放并发槽并记用量。
func TestProxyStreamingChatPreservesRawBytes(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, 200, rec.Code)
	body := rec.Body.String()
	// 原始字节必须完整保留：上游 chunk 与网关转发字节一致
	require.Contains(t, body, "data: ")
	require.Contains(t, body, "data: [DONE]")
	require.NotContains(t, body, "event:") // openai chat 是 data-only SSE
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "成功路径必须释放并发槽")
	require.Equal(t, 1, p.rec.Pending(), "成功路径必须记录一条用量")
}

// 回归（Task 3 迁移发现）：流式原始转发必须沿用 SDK 路径的模型改写语义——
// 调度器选号时已应用 ModelMapping（sel.Model 为上游模型名），原始请求体里的
// 客户端模型名若不改写，映射用户流式请求会打到上游不存在的模型。
func TestProxyStreamingChatAppliesModelMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		stream, _ := body["stream"].(bool)
		require.True(t, stream, "原始流式请求体必须带 stream:true")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"id\":\"c1\",\"model\":%q,\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n", body["model"])
		fl.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: srv.URL,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
		ModelMapping: map[string]string{"gpt-4o": "gpt-4o-upstream"},
	}
	store := &captureLogStore{}
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"model":"gpt-4o-upstream"`, "流式原始请求必须携带映射后的上游模型名")
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency)
	require.Equal(t, 1, p.rec.Pending())
	// 评审 I-1：映射请求的日志必须保留请求模型与映射后模型
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1, "must capture exactly one usage log")
	require.Equal(t, "gpt-4o", store.logs[0].Model, "Model = 客户端请求模型")
	require.Equal(t, "gpt-4o-upstream", store.logs[0].MappedModel, "MappedModel = 映射后实际模型")
}

// SSE 事件级冲刷回归（Task 9 压测发现）：sseWriter 必须每事件调用 http.Flusher.Flush()。
// 只刷 bufio 不刷 Flusher 时，http.Server 内部 4KB 缓冲攒批放出，流式首字节
// 延迟实测 145ms（修复后 ~1ms，见 docs/superpowers/plans/loadtest-results.md）。
// ResponseRecorder 实现 Flusher：首个事件写出后 Flushed 必须为真。
func TestProxyStreamingSSEFlushesPerEvent(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	require.True(t, rec.Flushed, "SSE 每事件必须冲刷（http.Flusher），首个事件后 Flushed 为真")
	require.Contains(t, rec.Body.String(), "data: [DONE]")
}

func TestProxyAuthRejected(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyTimeoutLogs(t, up.URL, 1, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 401, rec.Code)
	// 401 鉴权失败（无请求模型可记，Model/MappedModel 均空）：recordRejected →
	// err_logs 拒绝行（不入 usage_logs 明细——P2a 拒绝风暴不产生 pending）
	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1, "401 必须记一条 err_logs")
	require.Equal(t, "", store.logs[0].Model, "401 无请求模型")
	require.Equal(t, "", store.logs[0].MappedModel, "401 MappedModel 空")
	require.Equal(t, domain.ErrAuth, store.logs[0].ErrorType, "401 记 ErrAuth")
}

// 同 key 双头兼容：Anthropic 官方 SDK / Claude Code 用 x-api-key 头（而非
// Authorization: Bearer）→ /v1/messages 必须能通过网关认证；错误 key 同样 401。
func TestProxyAuthXAPIKey(t *testing.T) {
	up := fakeAnthropic(t, "")
	defer up.Close()
	p := newTestProxyFormat(t, up.URL, domain.FormatAnthropic)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"gpt-4o","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "ck-1")
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)
	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())

	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"gpt-4o","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	req2.Header.Set("x-api-key", "wrong")
	rec2 := httptest.NewRecorder()
	p.HandleAnthropic(rec2, req2)
	require.Equal(t, 401, rec2.Code, "body=%s", rec2.Body.String())
}

func TestProxyFailoverOn429(t *testing.T) {
	// 两个账号指向同一个会 429 的上游：第一个失败后转移第二个（同样失败则最终 429）
	up := fakeOpenAI(t, "429")
	defer up.Close()
	mapping := map[string]string{"gpt-4o": "gpt-4o-upstream"}
	store := &captureLogStore{}
	tpl1 := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
		ModelMapping: mapping,
	}
	p := newTestProxyTplTimeoutLogs(t, tpl1, 1, true, 30*time.Second, store, nil)
	// 第二个账号（同样带映射，耗尽路径才能断言最后一次实际尝试的映射模型）
	tpl2 := &domain.Template{ID: 2, Name: "t2", BaseURL: up.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"}, ModelMapping: mapping}
	sched := p.sched
	acc2 := &domain.Account{ID: 2, TemplateID: 2, Template: tpl2, UpstreamKey: "sk-upstream", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4}
	loader := p.sched.Loader().(noopLoader)
	loader.accs[10] = append(loader.accs[10], acc2)
	require.NoError(t, sched.InvalidateAllSync())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 429, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "1", rec.Header().Get("Retry-After"), "429 最终失败回 Retry-After")
	// MarkResult 为异步投递：断言前排空规则队列（测试与优雅关闭用钩子）
	sched.FlushRules()
	// 两个账号都进入 429 冷却：Runtime 视图可查
	ri, ok := sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.Status429, ri.Status)
	require.Zero(t, ri.Concurrency, "failover 后并发槽必须全部释放")
	ri, ok = sched.Runtime(2)
	require.True(t, ok)
	require.Equal(t, domain.Status429, ri.Status)
	require.Zero(t, ri.Concurrency, "failover 后并发槽必须全部释放")
	// 耗尽路径（请求已完成）：429 失败行（Err429）不入 usage_logs——err_logs
	// 承载（分表：失败明细归 err_logs；pending 恒 0）
	require.Zero(t, p.rec.Pending(), "failover 耗尽失败行不产生明细 pending")
	// 评审 I-1：耗尽路径 Model=请求模型、MappedModel=最后一次实际尝试的映射模型
	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1, "must capture exactly one err_logs row")
	require.Equal(t, "gpt-4o", store.logs[0].Model, "耗尽路径：Model = 客户端请求模型")
	require.Equal(t, "gpt-4o-upstream", store.logs[0].MappedModel, "耗尽路径：MappedModel = 最后一次实际尝试的映射模型")
	require.Equal(t, domain.Err429, store.logs[0].ErrorType)
}

// 5xx：触发 failover 与 MarkResult(连接级/5xx 分流)；全部尝试失败最终回 502（非 429 不设 Retry-After）。
func TestProxyFailoverOn5xx(t *testing.T) {
	up := fakeOpenAI(t, "500")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	tpl2 := &domain.Template{ID: 2, Name: "t2", BaseURL: up.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"}}
	sched := p.sched
	acc2 := &domain.Account{ID: 2, TemplateID: 2, Template: tpl2, UpstreamKey: "sk-upstream", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4}
	loader := p.sched.Loader().(noopLoader)
	loader.accs[10] = append(loader.accs[10], acc2)
	require.NoError(t, sched.InvalidateAllSync())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 502, rec.Code, "body=%s", rec.Body.String())
	require.Empty(t, rec.Header().Get("Retry-After"))
	sched.FlushRules() // MarkResult 异步投递：断言前排空
	ri, ok := sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusUnhealthy, ri.Status)
	require.Zero(t, ri.Concurrency, "failover 后并发槽必须全部释放")
	ri, ok = sched.Runtime(2)
	require.True(t, ok)
	require.Equal(t, domain.StatusUnhealthy, ri.Status)
	require.Zero(t, ri.Concurrency, "failover 后并发槽必须全部释放")
	// 耗尽路径（请求已完成）：5xx 失败行（Err5xx）不入 usage_logs——err_logs
	// 承载（分表：失败明细归 err_logs；pending 恒 0）
	require.Zero(t, p.rec.Pending(), "failover 耗尽失败行不产生明细 pending")
}

// 回归（评审 Critical）：failover 耗尽时尾部 Select 为"不存在的下一次尝试"预取
// 并发槽——若选中第三个健康账号则槽位永不释放（CAS 抢占、仅 Release 递减、无回收）。
// 3 账号全健康 + FailoverAttempts=2：前两轮 429 后，泄漏版本会在循环退出前
// 抢走剩余健康账号的槽（Concurrency==1），修复版本不预选（全部为 0）。
func TestProxyFailoverExhaustedNoLeak(t *testing.T) {
	up := fakeOpenAI(t, "429")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	sched := p.sched
	loader := p.sched.Loader().(noopLoader)
	for i := int64(2); i <= 3; i++ {
		tpl := &domain.Template{ID: i, Name: fmt.Sprintf("t%d", i), BaseURL: up.URL,
			CredentialType:   credential.TypeAPIKey,
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"}}
		loader.accs[10] = append(loader.accs[10], &domain.Account{
			ID: i, TemplateID: i, Template: tpl, UpstreamKey: "sk-upstream",
			Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4,
		})
	}
	require.NoError(t, sched.InvalidateAllSync())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 429, rec.Code, "body=%s", rec.Body.String())
	for id := int64(1); id <= 3; id++ {
		ri, ok := sched.Runtime(id)
		require.True(t, ok)
		require.Zero(t, ri.Concurrency, "account %d 并发槽必须全部释放", id)
	}
	require.Zero(t, p.rec.Pending(), "耗尽路径失败行不产生明细 pending（err_logs 承载）")
}

// 防呆（spec 纵深）：failover_attempts=0 直构（绕过 validate 的 >=1 下限——测试
// 侧 p.cfg 改写等价直构）时 failover 循环零次执行，首次 Select 已占并发槽——
// 修复前槽永不释放（组内账号耗尽后全组 429 死锁，重启才能恢复）；耗尽路径必须
// 补 Release。N=0 时 lastCode=0 → ErrNetwork → 502 "all upstream attempts failed"。
func TestProxyFailoverZeroReleasesSlot(t *testing.T) {
	up := fakeOpenAI(t, "500") // 循环体不执行，上游不会被调用——返回码任意
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	sched := p.sched
	p.cfg.FailoverAttempts = 0

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 502, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "all upstream attempts failed")
	ri, ok := sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "failover_attempts=0 首次选号占槽必须释放（防呆 Release）")
	require.Zero(t, p.rec.Pending(), "N=0 耗尽路径失败行不产生明细 pending（err_logs 承载）")
}

// 4xx：确定性错误，透传上游状态码与原始 body、不转移（规格 §5.3），账号不进入冷却。
func TestProxyPassthrough4xx(t *testing.T) {
	up := fakeOpenAI(t, "400")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyTimeoutLogs(t, up.URL, 1, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 400, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"bad request"`, "4xx 必须透传上游原始 body")
	require.NotContains(t, rec.Body.String(), "upstream rejected request", "透传 body 时不得回退网关文案")
	// 未 MarkResult：状态保持 active，不冷却
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusActive, ri.Status)
	require.Nil(t, ri.CooldownUntil)
	require.Zero(t, ri.Concurrency, "4xx 透传也必须释放并发槽")
	// 请求已完成（上游消费了请求）：4xx 失败行不入 usage_logs——err_logs 承载
	//（分表：失败明细归 err_logs；pending 恒 0）
	require.Zero(t, p.rec.Pending(), "4xx 透传不产生明细 pending")
	// 评审 I-1：4xx 透传 Model=客户端请求模型（无映射 → MappedModel 空）
	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1, "must capture exactly one err_logs row")
	require.Equal(t, "gpt-4o", store.logs[0].Model, "4xx：Model = 客户端请求模型")
	require.Equal(t, "", store.logs[0].MappedModel, "无映射 → MappedModel 空")
	require.Equal(t, domain.Err4xx, store.logs[0].ErrorType)
}

// 流式中止：上游在流中途发非法事件（解码失败）→ 连接级/5xx 分流 + 释放并发槽 + ErrAbort 记录。
func TestProxyStreamAbortFreesSlot(t *testing.T) {
	up := fakeOpenAI(t, "abort-stream")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	p.sched.FlushRules() // MarkResult 异步投递：断言前排空
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusUnhealthy, ri.Status, "中止记 连接级/5xx 分流")
	require.Zero(t, ri.Concurrency, "中止路径必须释放并发槽")
	require.Equal(t, 1, p.rec.Pending(), "中止路径记 ErrAbort 用量")
}

// 回归（评审 Critical）：流式上游停滞超过 UpstreamStreamTimeout 必须按上游错误
// 处理——记 ErrAbort + MarkResult(连接级/5xx 分流) → 账号不健康。此前 sserelay.
// normalize 把 ctx 超时折叠为 context.Canceled，tryChat 按 errors.Is(err,
// context.Canceled) 走了"客户端断开"分支：释放槽位但不 MarkResult、不记用量
// （账号保持 active、Pending 0），与迁移前 SDK 路径（记 ErrAbort + 不健康）相悖。
func TestProxyStreamTimeoutMarksUnhealthy(t *testing.T) {
	up := fakeOpenAI(t, "stall-stream")
	defer up.Close()
	store := &captureLogStore{}
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
	}
	// 小超时保证断言在测试生命周期内触发（父 ctx 不取消，仅子 ctx 超时）
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 100*time.Millisecond, store, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	p.sched.FlushRules() // MarkResult 异步投递：断言前排空
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusUnhealthy, ri.Status, "停滞超时记 连接级/5xx 分流 → 不健康")
	require.Zero(t, ri.Concurrency, "超时中止路径必须释放并发槽")
	require.Equal(t, 1, p.rec.Pending(), "超时中止必须记一条 ErrAbort 用量")
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1, "must capture exactly one usage log")
	require.Equal(t, domain.ErrAbort, store.logs[0].ErrorType, "超时中止按上游读失败记 ErrAbort")
	require.Equal(t, "gpt-4o", store.logs[0].Model, "recordStreamAbort：Model = 客户端请求模型")
	require.Equal(t, "", store.logs[0].MappedModel, "无映射 → MappedModel 空")
}

// 回归（评审 Minor）：流式 4xx 透传必须与非流式同语义——上游非 200 响应在
// relay 之前就被检出，状态码 + 原始 body 原样写出、不 MarkResult（账号保持
// active、不冷却）、并发槽释放、记一条用量。此前 fakes 的 "400" 模式只在
// 非流式请求上触发，流式 4xx 路径没有测试覆盖。
func TestProxyChatStreamingPassthrough4xx(t *testing.T) {
	up := fakeOpenAI(t, "400-stream")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, 400, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"bad request"`, "4xx 必须透传上游原始 body")
	require.NotContains(t, rec.Body.String(), "upstream rejected request", "透传 body 时不得回退网关文案")
	// 未 MarkResult：状态保持 active，不冷却
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusActive, ri.Status)
	require.Nil(t, ri.CooldownUntil)
	require.Zero(t, ri.Concurrency, "4xx 透传也必须释放并发槽")
	require.Zero(t, p.rec.Pending(), "4xx 透传不产生明细 pending（err_logs 承载）")
}

// failingResponseWriter 模拟客户端断开：所有写出都失败。
type failingResponseWriter struct{}

func (failingResponseWriter) Header() http.Header         { return http.Header{} }
func (failingResponseWriter) Write(p []byte) (int, error) { return 0, errors.New("client gone") }
func (failingResponseWriter) WriteHeader(int)             {}

// 客户端断开：SSE 写出失败 → 连接级/5xx 分流 + 释放并发槽。旧 SDK 路径不记用量；
// relay 无法区分"写出失败"与"上游读失败"（两者都是非 ctx 取消的错误），
// 按控制器语义一律走 recordStreamAbort → 记一条 ErrAbort 用量。
func TestProxyClientDisconnectFreesSlot(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyTimeoutLogs(t, up.URL, 1, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	p.HandleChat(failingResponseWriter{}, req)

	p.sched.FlushRules() // MarkResult 异步投递：断言前排空
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusUnhealthy, ri.Status, "客户端断开记 连接级/5xx 分流")
	require.Zero(t, ri.Concurrency, "客户端断开必须释放并发槽")
	require.Equal(t, 1, p.rec.Pending(), "写出失败按上游读失败处理，记 ErrAbort 用量")
	// 评审 I-1：客户端断开（recordStreamAbort）Model=客户端请求模型
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1, "must capture exactly one usage log")
	require.Equal(t, domain.ErrAbort, store.logs[0].ErrorType)
	require.Equal(t, "gpt-4o", store.logs[0].Model, "recordStreamAbort：Model = 客户端请求模型")
}

// UsageCapture=false：Record 不得被调用（channel 零填充，否则饱和后阻塞热路径）。
func TestProxyUsageCaptureDisabled(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxyCapture(t, up.URL, 1, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code)
	require.Zero(t, p.rec.Pending(), "UsageCapture=false 时不得入队")
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "并发槽仍须释放")
}

// 回归：单账号 429 冷却后，失败转移中途 Select 失败（nil, ErrNoAvailable）时
// 耗尽路径不得解引用 nil Selection（此前 panic → 500；应 429）。
func TestProxyChatFailoverSingleAccountNoPanic(t *testing.T) {
	up := fakeOpenAI(t, "429")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	require.NotPanics(t, func() { p.HandleChat(rec, req) })
	require.Equal(t, 429, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "1", rec.Header().Get("Retry-After"))
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "并发槽必须释放")
	require.Zero(t, p.rec.Pending(), "耗尽路径失败行不产生明细 pending（err_logs 承载）")
}

// 评审 I-1：成功非流式路径日志 Model=客户端请求模型（无映射 → MappedModel 空）。
func TestProxyChatNonStreamingLogsModel(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyTimeoutLogs(t, up.URL, 1, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1, "must capture exactly one usage log")
	require.Equal(t, "gpt-4o", store.logs[0].Model, "成功非流式：Model = 客户端请求模型")
	require.Equal(t, "", store.logs[0].MappedModel, "无映射 → MappedModel 空")
	require.Equal(t, int64(3), store.logs[0].InputTokens, "非流式 usage 直读")
	require.Equal(t, int64(5), store.logs[0].OutputTokens)
}

// 评审 I-1 + P2a：Select 失败（组内无账号支持请求格式）→ 404；本地预用量
// 拒绝不产生 usage_logs 明细（无 tokens 无 cost，P2a 源头修复——拒绝风暴
// 不进入 pending）。
func TestProxySelectFailLogsModel(t *testing.T) {
	up := fakeAnthropic(t, "")
	defer up.Close()
	store := &captureLogStore{}
	// 模板只支持 openai-chat：anthropic 请求必然 Select 失败（ErrFormatUnavailable → 404）
	p := newTestProxyTimeoutLogs(t, up.URL, 1, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"gpt-4o","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
	require.Zero(t, p.rec.Pending(), "Select 失败不产生明细 pending（P2a）")
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Empty(t, store.logs, "Select 失败不产生 usage_logs 明细（P2a）")
}

// 分发实证（评审 M2）：注册自定义 provider（覆盖 api_key 类型的默认实现）→
// 上游收到的鉴权值必须来自 provider 返回的凭据值，而非 Selection.UpstreamKey
// 直读——证明注册表分发路径真实生效，接线非空转。
func TestProxyCredentialDispatchUsesRegistry(t *testing.T) {
	gotAuth := make(chan string, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "c1", "object": "chat.completion", "model": "gpt-4o",
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	p.creds.Register(customAPIKeyProvider{val: "sk-via-registry"})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "Bearer sk-via-registry", <-gotAuth, "上游收到的鉴权值必须来自注册表 provider")
}

type customAPIKeyProvider struct{ val string }

func (c customAPIKeyProvider) Type() credential.Type { return credential.TypeAPIKey }
func (c customAPIKeyProvider) Credential(_ context.Context, _ credential.CredentialInput) (string, error) {
	return c.val, nil
}

// 评审 M1：未知凭据类型（号池生态类型未注册）→ credentialFor 显式错误 →
// 网络错误路径（耗尽 502，Retry-After 不设），上游不得收到任何请求——
// 不得静默 fallback 到 api_key（fallback 是号池类型安全隐患）。
func TestProxyCredentialUnknownTypeRejectsNoUpstreamCall(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(500)
	}))
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	loader := p.sched.Loader().(noopLoader)
	loader.accs[10][0].Template.CredentialType = credential.Type("codex_oauth")
	require.NoError(t, p.sched.InvalidateAllSync())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, http.StatusBadGateway, rec.Code, "未知凭据类型按网络错误处理 → 耗尽 502")
	require.Empty(t, rec.Header().Get("Retry-After"), "非 429 不设 Retry-After")
	require.Zero(t, hits.Load(), "未知类型不得 fallback：上游一个请求都不许收到")
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "凭据错误路径也必须释放并发槽")
	require.Zero(t, p.rec.Pending(), "网络失败行不产生明细 pending（err_logs 承载）")
}
