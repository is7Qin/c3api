// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

// 错误文本落盘（部署故障修复 #20）：连接级失败 / 4xx / 5xx 的 usage log
// ErrorMessage 语义 + 连接级 Warn（err 全文）。成功路径 ErrorMessage 恒空
// （热路径零新增分配）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	"github.com/is7qin/c3api/pkg/logx"
)

// newTestProxyWarn 同 newTestProxyFormatLogs，但注入 zap 日志（Warn 断言用）；
// format 参数指定模板格式（chat/anth 等识别场景按需）。
func newTestProxyWarn(t *testing.T, upstream string, accountID int64, format domain.RequestFormat, logs usage.LogInserter, logOut string) *Proxy {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{format}, Models: []string{"gpt-4o"},
	}
	accs := map[int64][]*domain.Account{10: {{
		ID: accountID, TemplateID: tpl.ID, Template: tpl, UpstreamKey: "sk-upstream",
		Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4,
	}}}
	cfg := Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
		GroupKeyRPM:           0, UsageCapture: true,
	}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	sched := scheduler.New(scheduler.Config{
		DefaultMaxConcurrency: 4, SyncInterval: time.Hour,
	}, noopLoader{accs: accs}, re, nil)
	require.NoError(t, sched.InvalidateAllSync())
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, logs, nil)
	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{
		"ck-1": activeKey(1, 1, 10),
	}}, noopUserLoader{}, nil)
	require.NoError(t, auth.Reload(context.Background())) // 构造不再自载——测试显式首刷（快照注册表单一入口）
	hc := &http.Client{Transport: http.DefaultTransport}
	clients := aiclient.NewFactory(hc, aiclient.Config{
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
	})
	logger, err := logx.New("warn", logOut)
	require.NoError(t, err)
	// errlog worker（分表设计）：错误明细与 usage_logs 共用捕获 store——错误
	// 文本断言经 p.errlog.Close 显式排空后同 store 读取。
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, FlushInterval: time.Hour}, errLogStoreFrom(logs), nil)
	return New(cfg, sched, credential.New(), rec, clients, auth, logger, nil, errlogW)
}

// 连接级失败（fake 上游断连）：耗尽路径 usage log ErrorMessage = err.Error()
// （域内截断 500），Warn 含 err 全文（request_id/account/model）。
func TestProxyConnErrorLogsErrorMessage(t *testing.T) {
	up := fakeOpenAI(t, "")
	url := up.URL
	up.Close() // 断连：拨号必失败 → code 0（连接级）
	store := &captureLogStore{}
	// 日志文件：zap 持有句柄（Windows 上删除会失败），RemoveAll 忽略错误
	// （与 flusher_test/usage_test 同模式）
	dir, err := os.MkdirTemp("", "c3api-errlog-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	logOut := filepath.Join(dir, "warn.log")
	p := newTestProxyWarn(t, url, 1, domain.FormatOpenAIChat, store, logOut)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 502, rec.Code, "body=%s", rec.Body.String())

	// 耗尽记录（分表设计）：cost=0 错误行不入 usage_logs——err_logs 承载
	//（ErrorType=network + ErrorMessage=err.Error()（≤500））
	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1, "耗尽路径必须记一条 err_logs")
	l := store.logs[0]
	require.Equal(t, domain.ErrNetwork, l.ErrorType)
	require.NotNil(t, l.ErrorMessage, "连接级失败必须落 ErrorMessage")
	require.Contains(t, *l.ErrorMessage, "dial", "ErrorMessage = err.Error() 文本")
	require.LessOrEqual(t, len(*l.ErrorMessage), domain.ErrMsgMaxLen, "域内截断 500")

	// Warn 含 err 全文（request_id/account/model 字段 + 未截断的完整错误）
	data, err := os.ReadFile(logOut)
	require.NoError(t, err)
	logs := string(data)
	require.Contains(t, logs, "upstream connection failure")
	require.Contains(t, logs, "request_id")
	require.Contains(t, logs, "account_id")
	require.Contains(t, logs, "model")
	require.Contains(t, logs, url, "Warn 必须含 err 全文（含请求 URL）")
}

// 4xx 透传：usage log ErrorMessage = 上游 body 原文（域内截断 500）。
func TestProxy4xxLogsErrorMessage(t *testing.T) {
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

	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1, "4xx 透传必须记录 err_logs（cost=0 不入 usage_logs）")
	l := store.logs[0]
	require.Equal(t, domain.Err4xx, l.ErrorType)
	require.NotNil(t, l.ErrorMessage, "4xx 必须落 ErrorMessage")
	require.Contains(t, *l.ErrorMessage, "bad request", "ErrorMessage = 上游 body")
	require.LessOrEqual(t, len(*l.ErrorMessage), domain.ErrMsgMaxLen)
}

// 4xx 长 body：ErrorMessage 截断到 500 字符（不拆断、不越界）。
func TestProxy4xxLogsErrorMessageTruncated(t *testing.T) {
	long := strings.Repeat("x", 600)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"` + long + `"}}`))
	}))
	defer srv.Close()
	store := &captureLogStore{}
	p := newTestProxyTimeoutLogs(t, srv.URL, 1, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 400, rec.Code, "body=%s", rec.Body.String())

	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	l := store.logs[0]
	require.NotNil(t, l.ErrorMessage)
	// 原始上游 body = `{"error":{"message":"` + 600×x + `"}}`；openai-go SDK
	// 的 RawJSON 为解析后的错误对象 `{"message":"` + 600×x + `"}`（623→621
	// 字符）→ 截断 500 = 12 字符前缀 + 488×x
	require.Len(t, *l.ErrorMessage, domain.ErrMsgMaxLen, "长 body 截断 500 字符")
	require.Equal(t, strings.Repeat("x", 488), strings.TrimPrefix(*l.ErrorMessage, `{"message":"`))
}

// 5xx 耗尽：ErrorMessage = 上游 body 的 message（既有 upstreamErrMsg 语义）。
func TestProxy5xxLogsErrorMessage(t *testing.T) {
	up := fakeOpenAI(t, "500")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyTimeoutLogs(t, up.URL, 1, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 502, rec.Code, "body=%s", rec.Body.String())

	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	l := store.logs[0]
	require.Equal(t, domain.Err5xx, l.ErrorType)
	require.NotNil(t, l.ErrorMessage, "5xx 耗尽必须落 ErrorMessage")
	require.Equal(t, "boom", *l.ErrorMessage, "5xx ErrorMessage = 上游 body message")
}

// 成功路径（200）：ErrorMessage 恒空（热路径零新增分配）。分表路由（放行路径
// 语义）：成功行（none）入 usage_logs——cost=0 不限；err_logs 无错误行。
func TestProxySuccessLogsNoErrorMessage(t *testing.T) {
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
	require.NoError(t, p.errlog.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Nil(t, store.logs[0].ErrorMessage, "成功路径 ErrorMessage 恒空")
	require.Equal(t, domain.ErrNone, store.logs[0].ErrorType)
}

// 用户裁决修正（2026-08-11）：usage_logs 成员资格 = 放行路径语义（error_type
// ∈ {none, abort}），与 cost 无关——0 token 成功行（空响应）仍入 usage_logs
// （cost>0 判定会漏掉此类行）。
func TestProxyEmptyResponseSuccessStillLogsUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "c1", "object": "chat.completion", "model": "gpt-4o",
			"choices": []any{}, "usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
		})
	}))
	defer srv.Close()
	store := &captureLogStore{}
	p := newTestProxyTimeoutLogs(t, srv.URL, 1, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())

	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1, "0 token 成功行（cost=0）仍入 usage_logs（放行路径语义，非 cost 判定）")
	require.Equal(t, domain.ErrNone, store.logs[0].ErrorType)
	require.Zero(t, store.logs[0].TotalTokens)
}

// 用户裁决修正（2026-08-11）：失败行（4xx/5xx）不入 usage_logs——分表：失败
// 明细归 err_logs。4xx 透传与 5xx 耗尽双形态显式断言 usage_logs 零行 + err_logs
// 一行。
func TestProxyFailureRowsNeverInUsageLogs(t *testing.T) {
	for _, mode := range []struct{ mode, want string }{{"400", "400"}, {"500", "502"}} {
		t.Run(mode.mode, func(t *testing.T) {
			up := fakeOpenAI(t, mode.mode)
			defer up.Close()
			store := &captureLogStore{}
			p := newTestProxyTimeoutLogs(t, up.URL, 1, store)

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
				`{"model":"gpt-4o","messages":[]}`))
			req.Header.Set("Authorization", "Bearer ck-1")
			rec := httptest.NewRecorder()
			p.HandleChat(rec, req)
			require.Equal(t, mode.want, strconv.Itoa(rec.Code), "body=%s", rec.Body.String())

			require.NoError(t, p.rec.Close(context.Background()))
			store.mu.Lock()
			require.Empty(t, store.logs, "失败行不入 usage_logs（rec 明细零——分表）")
			store.mu.Unlock()
			require.NoError(t, p.errlog.Close(context.Background()))
			store.mu.Lock()
			defer store.mu.Unlock()
			require.Len(t, store.logs, 1, "失败行全部进 err_logs")
		})
	}
}

// 分类正确性（#20 E 项，用户实证）：客户端在上游首字节前断开（模型思考期
// 取消）→ r.Context() 已取消、SDK 返回 context.Canceled（statusOf=0）——
// 不得按连接级网络错误处理：不 failover、不 MarkResult/冷却；记 499
// （nginx client closed request 约定）+ ErrAbort + error_message，立即返回。
func TestProxyClientDisconnectBeforeFirstByte(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 模拟上游思考期：首字节前长时间不响应；客户端断开时服务器 ctx 也取消
		select {
		case <-time.After(time.Second):
			w.WriteHeader(200)
		case <-r.Context().Done():
		}
	}))
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyTimeoutLogs(t, up.URL, 1, store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[]}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	// 请求发出后 100ms 取消（SDK 请求阶段、首字节前）——模型思考期取消语义
	time.AfterFunc(100*time.Millisecond, cancel)
	p.HandleChat(rec, req)

	p.sched.FlushRules() // 若误 MarkResult 则冷却已生效——断言前排空队列
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusActive, ri.Status, "客户端断连不得冷却账号")
	require.Nil(t, ri.CooldownUntil, "客户端断连不得设冷却")
	require.Zero(t, ri.Concurrency, "断连路径必须释放并发槽")

	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 2, "499 abort 双轨：usage_logs（放行路径 abort）+ err_logs（豁免队列）各一行，request_id 关联")
	l := store.logs[0]
	require.Equal(t, statusClientClosedRequest, l.StatusCode, "首字节前断连记 499")
	require.Equal(t, domain.ErrAbort, l.ErrorType, "首字节前断连记 abort 而非 network")
	require.NotNil(t, l.ErrorMessage)
	require.Contains(t, *l.ErrorMessage, "client closed request", "断连错误文本落盘")
	require.Equal(t, l.RequestID, store.logs[1].RequestID, "双轨行 request_id 关联")
}

// SDK 本地校验错误归 4xx（spec 2026-08-16-anthropic-stream-accept-design A-1）：
// anthropic 非流式 + max_tokens 大（80000：expectedTime = 1h×80000/128000
// = 2250s > 10min）→ SDK CalculateNonStreamingTimeout（client.go:316 固定文本）
// 本地拒绝——无网络请求、无状态码（code=0），此前误归 network（err_logs 记
// network + 502 无原因文案）。识别归 400 + Err4xx：ErrorMessage = SDK 原文、
// Warn 留痕保留（不因识别而丢留痕）、不 failover 不 MarkResult（确定性错误
// §5.3，与 4xx 分支现状一致）、响应文案含原因。
func TestProxySDKValidationErrorClassified4xx(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(500)
	}))
	defer up.Close()
	store := &captureLogStore{}
	dir, err := os.MkdirTemp("", "c3api-errlog-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	logOut := filepath.Join(dir, "warn.log")
	p := newTestProxyWarn(t, up.URL, 1, domain.FormatAnthropic, store, logOut)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"gpt-4o","max_tokens":80000,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "ck-1")
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)

	require.Equal(t, 400, rec.Code, "body=%s", rec.Body.String())
	var resp struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, sdkStreamingRequiredText, resp.Error.Message, "message = SDK 原文不加前缀")
	require.Equal(t, "upstream_error", resp.Error.Type)
	require.Zero(t, hits.Load(), "SDK 本地拒绝：无网络请求（上游零命中）")

	// 确定性错误：不 failover、不 MarkResult（账号保持 active、不冷却）
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusActive, ri.Status)
	require.Nil(t, ri.CooldownUntil)
	require.Zero(t, ri.Concurrency, "识别路径必须释放并发槽")
	require.Zero(t, p.rec.Pending(), "失败行不入 usage_logs（分表：err_logs 承载）")

	// err_logs：400 + Err4xx + ErrorMessage = SDK 原文（<500 截断无损）
	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1, "识别路径记一条 err_logs")
	l := store.logs[0]
	require.Equal(t, 400, l.StatusCode, "err_logs status_code=400 而非 0")
	require.Equal(t, domain.Err4xx, l.ErrorType, "error_type=4xx 而非 network")
	require.NotNil(t, l.ErrorMessage)
	require.Equal(t, sdkStreamingRequiredText, *l.ErrorMessage, "ErrorMessage = SDK 原文")
	require.LessOrEqual(t, len(*l.ErrorMessage), domain.ErrMsgMaxLen)

	// Warn 留痕保留（与 code==0 网络路径同款）：request_id/account/model + 错误全文
	data, err := os.ReadFile(logOut)
	require.NoError(t, err)
	warns := string(data)
	require.Contains(t, warns, "upstream connection failure")
	require.Contains(t, warns, sdkStreamingRequiredText, "Warn 含 SDK 错误全文（不截断）")
	require.Contains(t, warns, "request_id")
	require.Contains(t, warns, "account_id")
	require.Contains(t, warns, "model")
}

// sdkStreamingRequiredText 为 anthropic-sdk-go v1.62.0 client.go:316
// （CalculateNonStreamingTimeout）硬编码错误文本——升级 SDK 若改文案须同步
// 本测试与 caller.go 识别点（A-1 版本依赖标注）。
const sdkStreamingRequiredText = "streaming is required for operations that may take longer than 10 minutes"
