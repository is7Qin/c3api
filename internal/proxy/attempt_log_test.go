// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

// attempt_logs 写点验收（spec 2026-11-18 retry-observability §6 的 S2 4/5/8）。
//
// 全部用例用**注入 sink** 断言（不经 DB、不起 worker、无 time.Sleep）：写点的
// 职责是「一次失败的 dispatch 恰好投递一行、内容来自既有投影」，落库/批量/保留
// 归 S2a 的 usage.AttemptLogWorker（另有 PG 用例走真实写路径）。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	codexsdk "github.com/is7Qin/codex-sdk"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/api"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/usage"
)

// captureAttemptSink 记录每一次投递（生产实现是 usage.AttemptLogWorker 的有界
// 队列；测试只需要「投了几行、每行是什么」）。
type captureAttemptSink struct {
	mu   sync.Mutex
	rows []*domain.AttemptLog
}

func (c *captureAttemptSink) Enqueue(row *domain.AttemptLog) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rows = append(c.rows, row)
}

func (c *captureAttemptSink) snapshot() []*domain.AttemptLog {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*domain.AttemptLog(nil), c.rows...)
}

// scriptedChatUpstream 按调用序返回脚本里的状态码（用尽后重复最后一个）：非流式
// chat 上游。200 回一份带 usage 的最小 chat.completion（计费/用量断言用）。
func scriptedChatUpstream(t *testing.T, steps ...int) *httptest.Server {
	t.Helper()
	return scriptedChatUpstreamDelayed(t, 0, steps...)
}

// scriptedChatUpstreamDelayed 同 scriptedChatUpstream，但每次响应前在 **handler 里**
// 停 delay（延时是上游行为，不是被测代码里的睡眠）——用来让 latency_ms 可断言。
func scriptedChatUpstreamDelayed(t *testing.T, delay time.Duration, steps ...int) *httptest.Server {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(hits.Add(1)) - 1
		step := steps[len(steps)-1]
		if n < len(steps) {
			step = steps[n]
		}
		if delay > 0 {
			time.Sleep(delay) // 允许：这是 httptest handler（模拟上游耗时），不是被测代码
		}
		if step != http.StatusOK {
			w.WriteHeader(step)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"gpt-4o",` +
			`"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// attemptLogProxy 造一个 n 账号（组 10）的 chat 代理，注入 attempt sink 与两个
// 独立捕获 store（usage_logs / err_logs 分开，才能逐表比对行序列）。
func attemptLogProxy(t *testing.T, upstream string, n int, sink AttemptLogSink) (*Proxy, *captureLogStore, *captureLogStore) {
	t.Helper()
	accs := make([]*domain.Account, 0, n)
	for id := int64(1); id <= int64(n); id++ {
		tpl := &domain.Template{
			ID: id, Name: "t", BaseURL: upstream, CredentialType: credential.TypeAPIKey,
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
		}
		accs = append(accs, &domain.Account{
			ID: id, TemplateID: id, Template: tpl, UpstreamKey: "sk-upstream",
			Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 4,
		})
	}
	usageStore, errStore := &captureLogStore{}, &captureLogStore{}
	rec := usage.New(usage.UsageConfig{BatchSize: 100, FlushInterval: time.Hour, QuotaFlushInterval: time.Hour}, usageStore, nil)
	p := newTestProxyTplAccountsRec(t, map[int64][]*domain.Account{10: accs}, true, 30*time.Second, rec, nil, errStore)
	p.SetAttemptLogs(sink)
	return p, usageStore, errStore
}

// drainSurface 排空两条落库通道并返回「usage_logs 行 + err_logs 行」（各自队列
// 内序 = 生产序；两条队列同步排空 ⇒ 无轮询、无 sleep）。
func drainSurface(t *testing.T, p *Proxy, usageStore, errStore *captureLogStore) (usageRows, errRows []*domain.UsageLog) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, p.errlog.Close(ctx))
	require.NoError(t, p.rec.Close(ctx))
	usageStore.mu.Lock()
	usageRows = append(usageRows, usageStore.logs...)
	usageStore.mu.Unlock()
	errStore.mu.Lock()
	errRows = append(errRows, errStore.logs...)
	errStore.mu.Unlock()
	return usageRows, errRows
}

// chatOnce 发一次 chat 请求（ck-1 = 活跃 key；其它 key 走本地 401 拒绝）。
func chatOnce(p *Proxy, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	return rec
}

// TestAttemptLogsOneRowPerFailedDispatch S2 验收 4：3 次尝试（2 失败 + 1 成功）
// ⇒ attempt_logs 恰好 2 行、ordinal 1/2（序号来自 scheduler.Attempt，不是循环里
// 自己数的第几次）。
func TestAttemptLogsOneRowPerFailedDispatch(t *testing.T) {
	// 上游每次停 30ms：latency_ms 若恒 0（没真量）也必须被发现。
	up := scriptedChatUpstreamDelayed(t, 30*time.Millisecond, 500, 500, 200)
	sink := &captureAttemptSink{}
	p, _, _ := attemptLogProxy(t, up.URL, 3, sink)
	p.cfg.FailoverAttempts = 3

	rec := chatOnce(p, "ck-1")
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	rows := sink.snapshot()
	require.Len(t, rows, 2, "一次失败 dispatch 一行；成功那跳不落行")
	for i, row := range rows {
		require.EqualValues(t, i+1, row.Ordinal, "ordinal = plan attempt 序号")
		require.NotZero(t, row.AccountID)
		require.Equal(t, "primary", row.Lane, "lane 复用 foldLaneTokens 词表")
		require.Equal(t, string(domain.FormatOpenAIChat), row.Format)
		require.Equal(t, http.StatusInternalServerError, row.StatusCode)
		require.Equal(t, "5xx", row.Outcome, "outcome 复用 flowOutcomeToken")
		require.Equal(t, string(domain.Err5xx), row.ErrorType)
		require.NotNil(t, row.CommitState, "handled=false 恒已知，不留 NULL")
		require.Equal(t, CommitUpstreamResponded.String(), *row.CommitState)
		require.NotNil(t, row.ErrorMessage)
		require.Contains(t, *row.ErrorMessage, "boom", "错误文本沿用 classify 那条（上游 body message）")
		require.GreaterOrEqual(t, row.LatencyMS, int64(25),
			"latency_ms 必须真的量到上游耗时（上游停 30ms；恒 0 说明计时没接）")
		require.Less(t, row.LatencyMS, int64(5000), "量的是这次 dispatch，不是整条请求")
		require.False(t, row.CreatedAt.IsZero(), "created_at = 写点时刻")
		require.NotEmpty(t, row.RequestID)
	}
	require.Equal(t, rows[0].RequestID, rows[1].RequestID, "同一次请求的两行共享 request_id")
	require.NotEqual(t, rows[0].AccountID, rows[1].AccountID, "failover 真的换了号（两次尝试两行）")
}

// TestAttemptLogsCoverageBoundaries S2 验收 5：覆盖边界诚实——本地拒绝与
// handled=true 的失败 ⇒ 0 行；循环可见 4xx 与 SDK fatal ⇒ 各 1 行（同时钉住
// err_logs.attempt_ordinal 的填值面）。
func TestAttemptLogsCoverageBoundaries(t *testing.T) {
	t.Run("本地拒绝不落尝试行", func(t *testing.T) {
		up := scriptedChatUpstream(t, 200)
		sink := &captureAttemptSink{}
		p, usageStore, errStore := attemptLogProxy(t, up.URL, 1, sink)
		rec := chatOnce(p, "ck-invalid")
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		require.Empty(t, sink.snapshot(), "401 鉴权失败是本地拒绝：没有 attempt 语义")

		usageRows, errRows := drainSurface(t, p, usageStore, errStore)
		require.Empty(t, usageRows)
		require.Len(t, errRows, 1, "拒绝行仍在 err_logs（真值不丢）")
		require.Nil(t, errRows[0].AttemptOrdinal, "拒绝行 attempt_ordinal 必须 NULL")
	})

	t.Run("handled=true 的失败不落尝试行", func(t *testing.T) {
		up := scriptedChatUpstream(t, 200)
		sink := &captureAttemptSink{}
		p, _, errStore := attemptLogProxy(t, up.URL, 1, sink)
		sel, plan, attempt, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o",
			scheduler.AttemptPlanIdentity{RequestID: "req-handled", UserID: 1})
		require.NoError(t, err)
		p.failoverLoopWithPlan(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
			domain.FormatOpenAIChat, "req-handled", 10, time.Now(), "gpt-4o", nil,
			sel, plan, attempt, attemptState{}, handledFailureAttempt{code: http.StatusInternalServerError}, &httpSink{}, false)
		require.Empty(t, sink.snapshot(), "handled=true ⇒ 已交付失败，循环不落尝试行")
		usageRows, errRows := drainSurface(t, p, &captureLogStore{}, errStore)
		require.Empty(t, usageRows)
		require.Empty(t, errRows)
	})

	t.Run("循环可见 4xx 落一行且 err_logs 带 ordinal", func(t *testing.T) {
		up := scriptedChatUpstream(t, 200)
		sink := &captureAttemptSink{}
		p, usageStore, errStore := attemptLogProxy(t, up.URL, 2, sink)
		sel, plan, attempt, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o",
			scheduler.AttemptPlanIdentity{RequestID: "req-4xx", UserID: 1})
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		p.failoverLoopWithPlan(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
			domain.FormatOpenAIChat, "req-4xx", 10, time.Now(), "gpt-4o", nil,
			sel, plan, attempt, attemptState{}, rejectedPipelineAttempt{code: http.StatusUnauthorized, body: []byte(`{"error":{"message":"nope"}}`)}, &httpSink{}, false)

		rows := sink.snapshot()
		require.Len(t, rows, 1, "循环可见的 4xx 是一次真实 dispatch")
		require.EqualValues(t, 1, rows[0].Ordinal)
		require.Equal(t, http.StatusUnauthorized, rows[0].StatusCode)
		require.Equal(t, "4xx", rows[0].Outcome)
		require.Equal(t, string(domain.Err4xx), rows[0].ErrorType)
		require.Equal(t, CommitUpstreamResponded.String(), *rows[0].CommitState)

		usageRows, errRows := drainSurface(t, p, usageStore, errStore)
		require.Empty(t, usageRows, "4xx 不可计费：不进 usage_logs")
		require.Len(t, errRows, 1)
		require.NotNil(t, errRows[0].AttemptOrdinal, "循环内 4xx 产出点必须填 ordinal")
		require.EqualValues(t, 1, *errRows[0].AttemptOrdinal)
	})

	t.Run("首字节前客户端取消落一行（client_cancel/abort）", func(t *testing.T) {
		up := scriptedChatUpstream(t, 200)
		sink := &captureAttemptSink{}
		p, usageStore, errStore := attemptLogProxy(t, up.URL, 2, sink)
		sel, plan, attempt, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o",
			scheduler.AttemptPlanIdentity{RequestID: "req-cancel", UserID: 1})
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // 客户端在首字节前就断了
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
		p.failoverLoopWithPlan(httptest.NewRecorder(), req,
			domain.FormatOpenAIChat, "req-cancel", 10, time.Now(), "gpt-4o", nil,
			sel, plan, attempt, attemptState{}, rejectedPipelineAttempt{code: 0}, &httpSink{}, false)

		rows := sink.snapshot()
		require.Len(t, rows, 1, "取消也是一次真实 dispatch（handled=false 走到循环）")
		require.EqualValues(t, 1, rows[0].Ordinal)
		require.Zero(t, rows[0].StatusCode)
		require.Equal(t, "client_cancel", rows[0].Outcome, "取消不得被记成上游错误（§3 的老坑）")
		require.Equal(t, string(domain.ErrAbort), rows[0].ErrorType)
		require.Equal(t, CommitNotSent.String(), *rows[0].CommitState)

		usageRows, errRows := drainSurface(t, p, usageStore, errStore)
		require.Empty(t, usageRows, "零业务帧不计费")
		require.Len(t, errRows, 1)
		require.Equal(t, statusClientClosedRequest, errRows[0].StatusCode)
		require.NotNil(t, errRows[0].AttemptOrdinal, "取消产出点必须填 ordinal")
		require.EqualValues(t, 1, *errRows[0].AttemptOrdinal)
	})

	t.Run("4xx 空响应体 ⇒ error_message NULL", func(t *testing.T) {
		up := scriptedChatUpstream(t, 200)
		sink := &captureAttemptSink{}
		p, usageStore, errStore := attemptLogProxy(t, up.URL, 2, sink)
		sel, plan, attempt, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o",
			scheduler.AttemptPlanIdentity{RequestID: "req-4xx-empty", UserID: 1})
		require.NoError(t, err)
		// 上游 401 且 **没有** body：既没有 message 也没有 callErr 文本。
		p.failoverLoopWithPlan(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
			domain.FormatOpenAIChat, "req-4xx-empty", 10, time.Now(), "gpt-4o", nil,
			sel, plan, attempt, attemptState{}, rejectedPipelineAttempt{code: http.StatusUnauthorized}, &httpSink{}, false)

		rows := sink.snapshot()
		require.Len(t, rows, 1)
		require.Nil(t, rows[0].ErrorMessage,
			"无错误文本 ⇒ NULL；写成空串会变成「有错误但说不出原因」的行")

		usageRows, errRows := drainSurface(t, p, usageStore, errStore)
		require.Empty(t, usageRows)
		require.Len(t, errRows, 1)
		require.Nil(t, errRows[0].ErrorMessage, "err_logs 同源：同样 NULL")
	})

	t.Run("SDK fatal 落一行且 err_logs 带 ordinal", func(t *testing.T) {
		up := scriptedChatUpstream(t, 200)
		sink := &captureAttemptSink{}
		p, usageStore, errStore := attemptLogProxy(t, up.URL, 2, sink)
		p.cfg.FailoverAttempts = 1
		sel, plan, attempt, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o",
			scheduler.AttemptPlanIdentity{RequestID: "req-fatal", UserID: 1})
		require.NoError(t, err)
		p.failoverLoopWithPlan(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
			domain.FormatOpenAIChat, "req-fatal", 10, time.Now(), "gpt-4o", nil,
			sel, plan, attempt, attemptState{}, fatalPipelineAttempt{err: &codexsdk.RefreshOAuthError{}}, &httpSink{}, false)

		rows := sink.snapshot()
		require.Len(t, rows, 1)
		require.EqualValues(t, 1, rows[0].Ordinal)
		require.Zero(t, rows[0].StatusCode, "fatal 判定不带上游状态码")
		require.Equal(t, "network", rows[0].Outcome, "零状态失败在 flow 词表里是 network")
		require.Equal(t, string(domain.ErrNetwork), rows[0].ErrorType)
		require.Equal(t, CommitNotSent.String(), *rows[0].CommitState)

		usageRows, errRows := drainSurface(t, p, usageStore, errStore)
		require.Empty(t, usageRows, "not_sent 的 fatal 行不可计费")
		// 两行 err_logs 是**既有形状**（不是本提交引入）：fatal 跳账一行
		// （recordAttemptLog）+ 循环随后 break 进耗尽收尾一行（writeFailoverExhausted）。
		// 两个产出点都必须带 ordinal——这正是 S3 能把它们归到同一次尝试的依据。
		require.Len(t, errRows, 2, "fatal 跳账 + 耗尽汇总")
		for _, row := range errRows {
			require.NotNil(t, row.AttemptOrdinal, "循环内产出点必须填 ordinal")
			require.EqualValues(t, 1, *row.AttemptOrdinal)
		}
	})
}

// handledFailureAttempt 返回 handled=true 的**失败**（已交付失败：caller 侧
// 自己写完了响应，循环不参与收尾）。
type handledFailureAttempt struct{ code int }

func (a handledFailureAttempt) call(context.Context, http.ResponseWriter, *http.Request, string, int64, time.Time, *api.Selection, string, []byte, attemptState) (int, []byte, http.Header, bool, error) {
	return a.code, nil, nil, true, nil
}

// fatalPipelineAttempt 返回 handled=false 的 SDK fatal 错误（账号级判死）。
// code 非 0 时模拟「fatal 判定 + 上游 4xx」这一形态（accounts/workspace 停用）。
type fatalPipelineAttempt struct {
	code int
	err  error
}

func (a fatalPipelineAttempt) call(context.Context, http.ResponseWriter, *http.Request, string, int64, time.Time, *api.Selection, string, []byte, attemptState) (int, []byte, http.Header, bool, error) {
	return a.code, nil, nil, false, a.err
}

// TestAttemptLogs429AttemptsCarryLimiterType 429 的**真实流程**（不是纯函数）：
// 上游限流的每一跳都必须是 outcome=429 / error_type=Err429 / commit 已知，且
// 耗尽行与 attempt 行指向同一 attempt（429 的中间跳仍不写 err_logs，既有语义）。
func TestAttemptLogs429AttemptsCarryLimiterType(t *testing.T) {
	up := scriptedChatUpstream(t, http.StatusTooManyRequests)
	sink := &captureAttemptSink{}
	p, usageStore, errStore := attemptLogProxy(t, up.URL, 2, sink)
	p.cfg.FailoverAttempts = 2

	require.NotEqual(t, http.StatusOK, chatOnce(p, "ck-1").Code)
	rows := sink.snapshot()
	require.Len(t, rows, 2, "两次 429 尝试 ⇒ 两行")
	for i, row := range rows {
		require.EqualValues(t, i+1, row.Ordinal)
		require.Equal(t, http.StatusTooManyRequests, row.StatusCode)
		require.Equal(t, "429", row.Outcome, "flow 词表里 429 是独立 token")
		require.Equal(t, string(domain.Err429), row.ErrorType)
		require.NotNil(t, row.CommitState)
		require.Equal(t, CommitUpstreamResponded.String(), *row.CommitState)
	}

	usageRows, errRows := drainSurface(t, p, usageStore, errStore)
	require.Empty(t, usageRows, "429 不可计费")
	require.Len(t, errRows, 1, "429 的中间跳只冷却不写 err_logs（既有语义），终态由耗尽收尾")
	require.Equal(t, domain.Err429, errRows[0].ErrorType)
	require.NotNil(t, errRows[0].AttemptOrdinal)
	require.EqualValues(t, rows[len(rows)-1].Ordinal, *errRows[0].AttemptOrdinal,
		"耗尽行指向最后一次尝试")
}

// TestAttemptLogLaneAndFormatProjection lane/format 不是硬编码常量：投影函数对
// 每一种 lane 与每一种协议都要给出精确 token（历史上这里是两条 `"primary"` /
// `"openai-chat"` 字面量，换个格式/换条 lane 就静默错标）。
func TestAttemptLogLaneAndFormatProjection(t *testing.T) {
	cases := []struct {
		name   string
		lane   scheduler.AttemptLane
		format domain.RequestFormat
		want   string
	}{
		{"primary", scheduler.AttemptLanePrimary, domain.FormatOpenAIChat, "primary"},
		{"explore", scheduler.AttemptLaneExplore, domain.FormatOpenAIResponses, "explore"},
		{"degraded", scheduler.AttemptLaneDegraded, domain.FormatAnthropic, "degraded"},
		{"codebook 外", scheduler.AttemptLane("mystery"), domain.FormatOpenAIImages, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &captureAttemptSink{}
			p := &Proxy{}
			p.SetAttemptLogs(sink)
			p.recordFailedDispatch(
				scheduler.Attempt{Ordinal: 3, AccountID: 7, Lane: tc.lane},
				"req-proj", tc.format, http.StatusBadGateway, false, nil, "boom", 12*time.Millisecond)

			rows := sink.snapshot()
			require.Len(t, rows, 1)
			require.Equal(t, tc.want, rows[0].Lane, "lane 复用 foldLaneTokens 词表（未知 = 空串，不发明第四个值）")
			require.Equal(t, string(tc.format), rows[0].Format, "format 与请求协议逐字一致")
			require.EqualValues(t, 3, rows[0].Ordinal)
			require.Equal(t, int64(7), rows[0].AccountID)
			require.Equal(t, int64(12), rows[0].LatencyMS)
			require.Equal(t, "5xx", rows[0].Outcome)
			require.Equal(t, string(domain.Err5xx), rows[0].ErrorType)
		})
	}
}

// TestSetAttemptLogsRejectsTypedNil typed nil 不再靠约定：nil 具体指针装进接口是
// 非 nil 接口，写点会调进 nil 接收者。setter 必须把它折成"未装配"，且写点不 panic。
func TestSetAttemptLogsRejectsTypedNil(t *testing.T) {
	p := &Proxy{}
	p.SetAttemptLogs((*usage.AttemptLogWorker)(nil))
	require.Nil(t, p.attemptLogs, "typed nil 必须折成未装配（nil 接口）")
	require.NotPanics(t, func() {
		p.recordFailedDispatch(scheduler.Attempt{Ordinal: 1, AccountID: 1},
			"req", domain.FormatOpenAIChat, http.StatusBadGateway, false, nil, "x", time.Millisecond)
	}, "未装配/typed nil ⇒ 写点 no-op，不得 panic")
}

// TestAttemptLogsFatal4xxExhaustedSameErrorType 一个 attempt 只有一种 error_type
// （oracle 双裁决必修 #1）：fatal 4xx 不重试直接走到耗尽时，耗尽行曾经沿用默认的
// Err5xx，而同一跳在 classify 里记的是 Err4xx —— attempt_logs 与 err_logs 会给
// 同一个 attempt 两种类型，S3 的链自相矛盾。
func TestAttemptLogsFatal4xxExhaustedSameErrorType(t *testing.T) {
	up := scriptedChatUpstream(t, 200)
	sink := &captureAttemptSink{}
	p, usageStore, errStore := attemptLogProxy(t, up.URL, 2, sink)
	p.cfg.FailoverAttempts = 1 // fatal 不重试 ⇒ 直接耗尽
	sel, plan, attempt, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o",
		scheduler.AttemptPlanIdentity{RequestID: "req-fatal-4xx", UserID: 1})
	require.NoError(t, err)
	p.failoverLoopWithPlan(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		domain.FormatOpenAIChat, "req-fatal-4xx", 10, time.Now(), "gpt-4o", nil,
		sel, plan, attempt, attemptState{},
		fatalPipelineAttempt{code: http.StatusUnauthorized, err: &codexsdk.RefreshOAuthError{}}, &httpSink{}, false)

	rows := sink.snapshot()
	require.Len(t, rows, 1)
	require.Equal(t, string(domain.Err4xx), rows[0].ErrorType, "attempt 行：401 ⇒ Err4xx")
	require.Equal(t, "4xx", rows[0].Outcome)
	require.EqualValues(t, 1, rows[0].Ordinal)

	usageRows, errRows := drainSurface(t, p, usageStore, errStore)
	require.Empty(t, usageRows, "upstream_responded 不可计费")
	require.Len(t, errRows, 2, "fatal 跳账 + 耗尽汇总（既有形状）")
	for _, row := range errRows {
		require.Equal(t, domain.Err4xx, row.ErrorType,
			"err_logs 的 fatal 行与耗尽行都必须是 Err4xx（不能一个 4xx 一个 5xx）")
		require.Equal(t, http.StatusUnauthorized, row.StatusCode)
		require.NotNil(t, row.AttemptOrdinal)
		require.EqualValues(t, rows[0].Ordinal, *row.AttemptOrdinal, "两表指向同一个 attempt")
	}
}

// TestAttemptLogsErrorTypeProjection error_type 投影词表（与 classify 同词表、
// 不共用实现）：429 / 4xx / 5xx / 零状态两分 / 客户端取消。
func TestAttemptLogsErrorTypeProjection(t *testing.T) {
	require.Equal(t, domain.Err429, attemptErrorType(http.StatusTooManyRequests, nil, false))
	require.Equal(t, domain.Err4xx, attemptErrorType(http.StatusUnauthorized, nil, false))
	require.Equal(t, domain.Err5xx, attemptErrorType(http.StatusBadGateway, nil, false))
	require.Equal(t, domain.ErrNetwork, attemptErrorType(0, fmt.Errorf("connection refused"), false))
	require.Equal(t, domain.ErrTimeout, attemptErrorType(0, errGatewayUpstreamTimeout, false),
		"零状态再分：网关预算到期 ⇒ timeout（与 zeroStatusFailureClass 同一判定）")
	require.Equal(t, domain.ErrAbort, attemptErrorType(0, nil, true), "客户端取消优先于零状态二分")
}

// TestAttemptLogOrdinalPtrThin 序号投影：0 = 没有 attempt（scheduler 要求
// Ordinal > 0）⇒ nil = ordinal_unknown；正数 ⇒ 独立指针。
func TestAttemptLogOrdinalPtrThin(t *testing.T) {
	require.Nil(t, attemptOrdinalPtr(0), "零值不是合法序号：必须留 NULL")
	got := attemptOrdinalPtr(7)
	require.NotNil(t, got)
	require.EqualValues(t, 7, *got)
	require.NotSame(t, attemptOrdinalPtr(7), got, "每次返回独立指针（queue 活得比栈帧久）")
}

// --- 写面行序列 golden（S2 验收 8 的主证） ---
//
// 固定场景跑完整 failover，断言 usage_logs / err_logs 的落库行**精确序列**逐条
// 等于基线——它抓的是「新写点顺手改了老写点」这类回归（读侧守卫永远看不见）。
//
// 基线取自**改 pipeline.go 之前**的实测输出，冻结成字面量；改统计/写面必须同时
// 解释清楚。request_id 是 per-request 随机值 ⇒ 归一成 <req>（同请求内必须一致，
// 单独断言）；latency/ttft/created_at 是环境量，不参与比对（latency 另测 ≥ 0）。

// formatUsageRow 把一行 usage_logs / err_logs 投影成稳定字符串。
func formatUsageRow(l *domain.UsageLog) string {
	msg, commit := "<nil>", "<nil>"
	if l.ErrorMessage != nil {
		msg = *l.ErrorMessage
	}
	if l.CommitState != nil {
		commit = *l.CommitState
	}
	return fmt.Sprintf("acct=%d tpl=%d user=%d key=%d model=%q fmt=%s status=%d et=%s commit=%s msg=%q in=%d out=%d total=%d cost=%d billed=%t",
		l.AccountID, l.TemplateID, l.UserID, l.KeyID, l.Model, string(l.Format), l.StatusCode,
		string(l.ErrorType), commit, msg, l.InputTokens, l.OutputTokens, l.TotalTokens, l.Cost, l.Billed)
}

// normalizeRows 归一 request_id（并断言同一序列里只有同一次请求的 id）。
func normalizeRows(t *testing.T, rows []*domain.UsageLog) []string {
	t.Helper()
	out := make([]string, 0, len(rows))
	var reqID string
	for _, l := range rows {
		if reqID == "" {
			reqID = l.RequestID
		}
		require.Equal(t, reqID, l.RequestID, "同一场景的行必须属于同一次请求")
		require.GreaterOrEqual(t, l.LatencyMS, int64(0))
		out = append(out, formatUsageRow(l))
	}
	return out
}

// TestWriteSurfaceRowsGolden2Fail1Success 2 失败（5xx）+ 1 成功：失败跳不落
// usage_logs（未交付不可计费）也不落 err_logs（5xx 只有冷却，终态由成功那跳
// 收尾）⇒ 一条成功行。
func TestWriteSurfaceRowsGolden2Fail1Success(t *testing.T) {
	up := scriptedChatUpstream(t, 500, 500, 200)
	sink := &captureAttemptSink{}
	p, usageStore, errStore := attemptLogProxy(t, up.URL, 3, sink)
	p.cfg.FailoverAttempts = 3

	require.Equal(t, http.StatusOK, chatOnce(p, "ck-1").Code)
	usageRows, errRows := drainSurface(t, p, usageStore, errStore)

	require.Equal(t, []string{
		`acct=3 tpl=0 user=1 key=1 model="gpt-4o" fmt=openai-chat status=200 et=none commit=response_started msg="<nil>" in=3 out=5 total=8 cost=0 billed=true`,
	}, normalizeRows(t, usageRows), "usage_logs 行序列 = 基线（成功那跳）")
	require.Empty(t, normalizeRows(t, errRows), "2 次 5xx 都不是终态：err_logs 零行")
	require.Len(t, sink.snapshot(), 2, "主证的另一半：两次失败 dispatch 各一行 attempt_logs")
}

// TestWriteSurfaceRowsGoldenExhausted 2 次 5xx 耗尽：终态行只落 err_logs
// （commit=upstream_responded 不可计费），attempt_logs 两行。
func TestWriteSurfaceRowsGoldenExhausted(t *testing.T) {
	up := scriptedChatUpstream(t, 500)
	sink := &captureAttemptSink{}
	p, usageStore, errStore := attemptLogProxy(t, up.URL, 2, sink)
	p.cfg.FailoverAttempts = 2

	require.Equal(t, http.StatusBadGateway, chatOnce(p, "ck-1").Code)
	usageRows, errRows := drainSurface(t, p, usageStore, errStore)

	require.Empty(t, normalizeRows(t, usageRows), "耗尽行 commit=upstream_responded ⇒ 不可计费，不进 usage_logs")
	// attempt_ordinal 不属于上面冻结的列投影（它是本提交新增的列，基线里恒
	// NULL），所以单独钉：耗尽行带**最后一次尝试**的序号。
	require.Len(t, errRows, 1)
	require.NotNil(t, errRows[0].AttemptOrdinal, "耗尽行必须带 ordinal")
	require.EqualValues(t, 2, *errRows[0].AttemptOrdinal)
	require.Equal(t, []string{
		`acct=2 tpl=0 user=1 key=1 model="gpt-4o" fmt=openai-chat status=500 et=5xx commit=upstream_responded msg="boom" in=0 out=0 total=0 cost=0 billed=false`,
	}, normalizeRows(t, errRows), "err_logs 行序列 = 基线（耗尽终态）")
	require.Len(t, sink.snapshot(), 2, "耗尽路径两次失败 dispatch 各一行 attempt_logs")
}

// TestWriteSurfaceRowsGolden4xx 循环可见的 4xx（terminate 分支的 finish 写点）：
// err_logs 恰一行、commit_state 恒 NULL（buildLog 不盖交付事实）、usage_logs
// 零行（不可计费）。删掉该写点或改它的投影，这条立刻红。
func TestWriteSurfaceRowsGolden4xx(t *testing.T) {
	up := scriptedChatUpstream(t, 200)
	sink := &captureAttemptSink{}
	p, usageStore, errStore := attemptLogProxy(t, up.URL, 2, sink)
	sel, plan, attempt, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o",
		scheduler.AttemptPlanIdentity{RequestID: "req-golden-4xx", UserID: 1})
	require.NoError(t, err)
	p.failoverLoopWithPlan(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		domain.FormatOpenAIChat, "req-golden-4xx", 10, time.Now(), "gpt-4o", nil,
		sel, plan, attempt, attemptState{}, rejectedPipelineAttempt{code: http.StatusUnauthorized, body: []byte(`{"error":{"message":"nope"}}`)}, &httpSink{}, false)

	usageRows, errRows := drainSurface(t, p, usageStore, errStore)
	require.Empty(t, normalizeRows(t, usageRows))
	// 4xx 分支的 finish 写点同样带当次 attempt 的序号（与 429/5xx/fatal 同一规则）。
	require.Len(t, errRows, 1)
	require.NotNil(t, errRows[0].AttemptOrdinal, "4xx 产出点必须带 ordinal")
	require.EqualValues(t, 1, *errRows[0].AttemptOrdinal)
	require.Equal(t, []string{
		`acct=1 tpl=0 user=0 key=0 model="gpt-4o" fmt=openai-chat status=401 et=4xx commit=<nil> msg="{\"error\":{\"message\":\"nope\"}}" in=0 out=0 total=0 cost=0 billed=false`,
	}, normalizeRows(t, errRows), "err_logs 行序列 = 基线（4xx 终态）")
}

// BenchmarkFailoverLoopHandledDispatch 成功路径（handled=true）的单跳开销：
// attempt_logs 通道**已装配**时也不得新增分配——这是 spec §5「成功路径零新增
// 分配、零新增写」的可执行证据（写点只在 handled=false 之后才建行）。
//
// 两个子基准唯一的差别是 sink 装没装：`allocs/op` 必须完全相同（差 0），因为成功
// 路径只多了一次指针比较。
//
//	go test ./internal/proxy/ -run XXX -bench FailoverLoopHandledDispatch -benchmem
func BenchmarkFailoverLoopHandledDispatch(b *testing.B) {
	for _, tc := range []struct {
		name string
		sink AttemptLogSink
	}{
		{"unwired", nil},
		{"attemptlog-wired", &captureAttemptSink{}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			p := benchProxy(b, "http://127.0.0.1:9")
			p.SetAttemptLogs(tc.sink)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				sel, plan, attempt, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o",
					scheduler.AttemptPlanIdentity{RequestID: "req-bench", UserID: 1})
				if err != nil {
					b.Fatal(err)
				}
				p.failoverLoopWithPlan(httptest.NewRecorder(), req, domain.FormatOpenAIChat, "req-bench", 10,
					time.Now(), "gpt-4o", nil, sel, plan, attempt, attemptState{}, handledPipelineAttempt{}, &httpSink{}, false)
				sel.HandleOrDetached().Release()
			}
			if sink, ok := tc.sink.(*captureAttemptSink); ok {
				if n := len(sink.snapshot()); n != 0 {
					b.Fatalf("success dispatch must write zero attempt rows, got %d", n)
				}
			}
		})
	}
}
