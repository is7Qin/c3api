// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/redisx"
)

// --- WS 错误帧真实性（spec-ws-error-truth-2026-11-11 r2 §5.1） ---
//
// 判据面 = 客户端拿到的状态码 + 规则文案 + 账号惩罚投递 + 上游 create 条数 +
// 日志分表成员资格（err_logs 含上游 message）。这些都与"帧被当成流内容"直接对立：
//   - 帧被当内容 ⇒ 200 + SSE 事件字节（客户端无从得知本轮失败，账号无冷却）；
//   - 帧被翻译 ⇒ 未提交读失败 ⇒ pipeline 写真状态码 + seed-429 节流 + 失败行落 err_logs。
//
// 上游形态取"发一帧就 1000 关闭"（repro 报告 Case D）：错误帧之后上游不再说话，
// 所以没有"挂住"这个中间态，基线红也立刻可见。

// wsErrFrame429 生产"号额度耗尽"现场：带 status 的 error 事件帧。
const wsErrFrame429 = `{"type":"error","status":429,"error":{"type":"usage_limit_reached","message":"You've reached your usage limit."}}`

// newWSErrProxy 构造 codex resp 测试代理并把模板出站传输钉成 ws。
func newWSErrProxy(t *testing.T, upstream string, accounts map[int64]*domain.AccountExt, logs *captureLogStore) (*Proxy, *fakeFailureStore) {
	t.Helper()
	return newTestCodexRespProxy(t, credential.TypeCodexPAT, accounts, upstream, nil, nil, logs,
		withCodexResponsesTransport(domain.CodexResponsesTransportWS))
}

// pinWSRoute 钉住该 (组, 格式, 模型) 的候选序列（顺序即计划顺序）。
func pinWSRoute(p *Proxy, ids ...int64) {
	compiled := make([]scheduler.CompiledCandidate, 0, len(ids))
	for _, id := range ids {
		compiled = append(compiled, scheduler.CompiledCandidate{AccountID: id})
	}
	p.sched.PublishDecisionForTest(
		scheduler.RouteRefFor(10, string(domain.FormatOpenAIResponses), "gpt-4o"),
		&scheduler.RouteDecision{Primary: compiled})
}

// logTracks 按 routeLog 的成员资格分表计数（forward.go:441-467）：
//   - usage_logs（放行路径）成员 = error_type ∈ {none, abort}；
//   - err_logs（错误明细）成员 = error_type != none；
//   - abort 同属两表（半异常计费），单列以证明"无 recordStreamAbort 追加行"。
type logTracks struct {
	usage int
	errs  int
	abort int
}

func trackRows(rows []*domain.UsageLog) logTracks {
	var tr logTracks
	for _, l := range rows {
		if l.ErrorType == domain.ErrNone || l.ErrorType == domain.ErrAbort {
			tr.usage++
		}
		if l.ErrorType != domain.ErrNone {
			tr.errs++
		}
		if l.ErrorType == domain.ErrAbort {
			tr.abort++
		}
	}
	return tr
}

// storeRows 拷贝已落袋的日志行（异步双轨，断言前先 flush + waitStoreLogs）。
func storeRows(store *captureLogStore) []*domain.UsageLog {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]*domain.UsageLog(nil), store.logs...)
}

// dualHealthSink 把规则动作同时投给录制替身与生产同款 LatchSink：
// 前者保住既有 throttlesFor 断言，后者写入**真实** RuntimeHealth，使用例能读回
// StateRetryAfter（而不是只证明"投递过一次 Throttle"这个代理事实）。
type dualHealthSink struct {
	rec   *recordingHealthSink
	latch *scheduler.LatchSink
}

func (s dualHealthSink) Throttle(ev rule.Event, th domain.ThrottleAction) error {
	_ = s.rec.Throttle(ev, th)
	return s.latch.Throttle(ev, th)
}

func (s dualHealthSink) FailAccount(ev rule.Event) error {
	_ = s.rec.FailAccount(ev)
	return s.latch.FailAccount(ev)
}

// healthReadback 装配一个真实的 *scheduler.RuntimeHealth（miniredis 后端）并把它
// 接到规则 sink 与调度器上；cleanup 复位包级钩子。
type healthReadback struct {
	h *scheduler.RuntimeHealth
}

func newHealthReadback(t *testing.T) *healthReadback {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })

	h := scheduler.NewRuntimeHealth(c, "self-test", nil, nil)
	prevSink, prevHealth := testRuleSink, testRuntimeHealth
	testRuleSink = dualHealthSink{rec: testHealthSink, latch: scheduler.NewLatchSink(h, nil, nil)}
	testRuntimeHealth = h
	t.Cleanup(func() { testRuleSink, testRuntimeHealth = prevSink, prevHealth })
	return &healthReadback{h: h}
}

// statesFor 读回某账号在健康视图里的全部条目（视图按
// (account,quality,identity,rev) 建键；本用例单账号单路径，应恰好一条）。
func (hr *healthReadback) statesFor(t *testing.T, accountID int64) map[scheduler.HealthKey]scheduler.HealthState {
	t.Helper()
	require.NoError(t, hr.h.Sync(context.Background()), "health 视图刷新")
	out := make(map[scheduler.HealthKey]scheduler.HealthState)
	for k, e := range hr.h.View() {
		if k.AccountID == accountID {
			out[k] = e.State
		}
	}
	return out
}

// TestCodexWSErrorFrameStreamIs429（T2）：codex 流式车道，上游一到 create 就回
// 429 错误帧、计划只给 1 个候选。断言客户端 429 + 规则文案 + **账号进
// StateRetryAfter（真实 health 读回）** + create 恰 1 + err_logs 恰 1 行且含上游
// message + usage_logs 0 行 + 无 abort 行。
func TestCodexWSErrorFrameStreamIs429(t *testing.T) {
	testHealthSink.reset()
	hr := newHealthReadback(t)
	up, mock := newCodexWSScriptUpstream(t, wsErrFrame429)
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newWSErrProxy(t, up.URL, map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-10")}, store)
	pinWSRoute(p, 10)
	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	resp := postResponses(t, srv, `{"model":"gpt-4o","input":"hi","stream":true}`)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)

	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode,
		"上游 429 错误帧必须变成客户端 429；body=%q", body)
	require.NotContains(t, string(body), "data:", "错误帧不得再渲染成 SSE 字节")
	require.Equal(t, "rate limited", gjson.GetBytes(body, "error.message").String(),
		"客户端应拿到规则文案（seed-429）；body=%q", body)
	require.Equal(t, "upstream_error", gjson.GetBytes(body, "error.type").String(), "body=%q", body)

	require.Equal(t, 1, mock.upgradesN(), "只该拨一次号")
	require.Len(t, mock.framesSnapshot(), 1, "上游收到的 create 恰 1（不得重发）")

	// 账号进冷却：读回**真实** health 状态机（StateRetryAfter），而不是只看投递。
	p.sched.FlushRules() // MarkResult 异步投递 → 排空
	states := hr.statesFor(t, 10)
	require.Len(t, states, 1, "health 视图里必须有且仅有一条账号 10 的记录")
	for key, st := range states {
		require.Equal(t, scheduler.StateRetryAfter, st, "账号 10 必须进 StateRetryAfter（冷却）")
		require.NotEqual(t, scheduler.StateReady,
			hr.h.EffectiveState(10, key.Quality, key.Identity, key.IdentityRevision),
			"取用准入门必须判定为不可用（EffectiveState != StateReady）")
	}
	// 附加断言：typed throttle 对失败账号恰一次投递。
	require.Len(t, testHealthSink.throttlesFor(10), 1, "429 惩罚恰一次投递")

	// usage 批次是攒批的（BatchSize 100 / FlushInterval 1h），断言前必须显式 flush。
	require.NoError(t, p.rec.Close(context.Background()))
	waitStoreLogs(t, store, 1)
	rows := storeRows(store)
	require.Equal(t, logTracks{usage: 0, errs: 1, abort: 0}, trackRows(rows),
		"429 失败行只进 err_logs：usage_logs 0 行（无 abort 行=没走 recordStreamAbort）")
	require.NotNil(t, rows[0].ErrorMessage, "err_logs 行必须带错误文本")
	require.Contains(t, *rows[0].ErrorMessage, "You've reached your usage limit.",
		"err_logs 行必须含上游 message（否则'看不到为什么'原样残留）")
}

// TestCodexWSErrorFrameFailoverStaysOnePerAccount（T2b）：两个候选 ⇒ 429 按既有
// 矩阵语义允许 failover（与 HTTP 车道同义），但**同一账号不得被重发**：首选与
// 次选各 create 恰 1，两个账号各被节流一次。
func TestCodexWSErrorFrameFailoverStaysOnePerAccount(t *testing.T) {
	testHealthSink.reset()
	up, mock := newCodexWSScriptUpstream(t, wsErrFrame429)
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newWSErrProxy(t, up.URL, map[int64]*domain.AccountExt{
		10: codexPATExt(10, "pat-10"),
		20: codexPATExt(20, "pat-20"),
	}, store)
	pinWSRoute(p, 10, 20)
	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	resp := postResponses(t, srv, `{"model":"gpt-4o","input":"hi","stream":true}`)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode, "两个候选都 429 ⇒ 客户端 429；body=%q", body)

	require.Equal(t, 2, mock.upgradesN(), "两个候选各拨一次（同一账号不得重发）")
	frames := mock.framesSnapshot()
	require.Len(t, frames, 2, "每个账号 create 恰 1")
	require.Equal(t, "Bearer pat-10", mock.header(0).Get("Authorization"), "首选 = 账号 10")
	require.Equal(t, "Bearer pat-20", mock.header(1).Get("Authorization"), "次选 = 账号 20（不重发 10）")

	p.sched.FlushRules()
	require.Len(t, testHealthSink.throttlesFor(10), 1, "首选 429 恰一次惩罚")
	require.Len(t, testHealthSink.throttlesFor(20), 1, "次选 429 恰一次惩罚")

	require.NoError(t, p.rec.Close(context.Background()))
	waitStoreLogs(t, store, 1)
	require.Equal(t, 0, trackRows(storeRows(store)).abort, "无 abort 追加行")
}

// TestCodexWSErrorFrameNonStreamIs429（T3）：聚合臂（非流式）同一帧 ⇒ 429
// （基线是 502：聚合器 feed 无 error case ⇒ 截断错误 ⇒ statusOf=0）。
func TestCodexWSErrorFrameNonStreamIs429(t *testing.T) {
	testHealthSink.reset()
	up, mock := newCodexWSScriptUpstream(t, wsErrFrame429)
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newWSErrProxy(t, up.URL, map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-10")}, store)
	pinWSRoute(p, 10)
	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	resp := postResponses(t, srv, `{"model":"gpt-4o","input":"hi"}`)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)

	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode,
		"非流式同一帧必须是 429（不是 200 空体、也不是截断 502）；body=%q", body)
	require.Equal(t, "rate limited", gjson.GetBytes(body, "error.message").String(), "body=%q", body)
	require.Len(t, mock.framesSnapshot(), 1, "create 恰 1")

	// err_logs 同样带上游 message（L6 的收益在两臂都成立）。
	require.NoError(t, p.rec.Close(context.Background()))
	waitStoreLogs(t, store, 1)
	rows := storeRows(store)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].ErrorMessage)
	require.True(t, strings.Contains(*rows[0].ErrorMessage, "usage limit"), "ErrorMessage=%q", *rows[0].ErrorMessage)
}
