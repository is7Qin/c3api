// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/logx"
)

type handledPipelineAttempt struct{}

func (handledPipelineAttempt) call(context.Context, http.ResponseWriter, *http.Request, string, int64, time.Time, *scheduler.Selection, string, []byte, attemptState) (int, []byte, http.Header, bool, error) {
	return http.StatusOK, nil, nil, true, nil
}

type rejectedPipelineAttempt struct {
	code int
	body []byte
}

func (a rejectedPipelineAttempt) call(context.Context, http.ResponseWriter, *http.Request, string, int64, time.Time, *scheduler.Selection, string, []byte, attemptState) (int, []byte, http.Header, bool, error) {
	return a.code, a.body, nil, false, nil
}

func TestPipelineObserver_observedPreResponseLeavesCleanupToFailover(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	sel, plan, attempt, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o",
		scheduler.AttemptPlanIdentity{RequestID: "req-1", UserID: 1})
	require.NoError(t, err)
	// the threaded settle value and the session derivation coincide.
	current, ok := plan.CurrentAttempt()
	require.True(t, ok)
	require.Equal(t, attempt, current)
	flowCalls := 0
	p.pipelineFlowAppend = func(outcome AttemptOutcome) {
		require.Equal(t, AttemptID(attempt.AttemptID), outcome.ID)
		require.EqualValues(t, attempt.Ordinal, outcome.Ordinal)
		flowCalls++
	}
	observer, owns := p.pipelineObserver(sel, attempt, p.pipelineFlowAppend)
	require.True(t, owns)
	require.Nil(t, observer.markHealth, "failover classification remains the single MarkResult owner")
	require.Nil(t, observer.release, "failover retry/finish remains the single lease release owner")
	outcome := dispatchFailureOutcome(pipelineBase(attempt), 429, false, false)
	require.NoError(t, observer.Complete(outcome, nil))
	require.Equal(t, 1, flowCalls)
	runtime, ok := p.sched.Runtime(sel.AccountID)
	require.True(t, ok)
	require.Equal(t, int64(1), runtime.Concurrency, "observer must leave the lease to failover cleanup")
	p.cfg.FailoverAttempts = 1
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	p.failoverLoopWithPlan(httptest.NewRecorder(), req, domain.FormatOpenAIChat, "req-1", 10, time.Now(), "gpt-4o", nil, sel, plan, attempt, attemptState{}, rejectedPipelineAttempt{code: 429}, &httpSink{}, false)
	runtime, ok = p.sched.Runtime(sel.AccountID)
	require.True(t, ok)
	require.Zero(t, runtime.Concurrency, "observed exhaustion has one pipeline release owner")
}

func TestFailoverPipeline_observed4xxPreservesFinishAndMarkResult(t *testing.T) {
	testHealthSink.reset()
	up := fakeUpstreamStatus(t, 401, `{"error":{"message":"balance"}}`)
	defer up.Close()
	p := newTestProxyRules(t, up.URL, domain.FormatOpenAIChat,
		domain.Rule{Name: "balance-401", Enabled: true, Priority: 10,
			When: domain.RuleWhen{Kind: new("4xx"), HTTPStatus: new(401)},
			Then: domain.RuleThen{Throttle: &domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: new(int64(60 * 1000))}, ResponseCode: new(502), CustomMessage: new("upstream rejected request")}},
	)
	sel, plan, attempt, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o",
		scheduler.AttemptPlanIdentity{RequestID: "req-4xx", UserID: 1})
	require.NoError(t, err)
	// the threaded settle value and the session derivation coincide.
	current, ok := plan.CurrentAttempt()
	require.True(t, ok)
	require.Equal(t, attempt, current)
	flowCalls := 0
	p.pipelineFlowAppend = func(AttemptOutcome) { flowCalls++ }
	observer, owns := p.pipelineObserver(sel, attempt, p.pipelineFlowAppend)
	require.True(t, owns)
	require.Nil(t, observer.markHealth)
	require.Nil(t, observer.release)
	// The observer is never completed by hand: the failover loop owns the
	// exactly-one completion for every plan-backed dispatch.

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	p.failoverLoopWithPlan(rec, req, domain.FormatOpenAIChat, "req-4xx", 10, time.Now(), "gpt-4o", nil, sel, plan, attempt, attemptState{}, rejectedPipelineAttempt{code: 401, body: []byte(`{"error":{"message":"balance"}}`)}, &httpSink{}, false)

	require.Equal(t, 1, flowCalls, "the loop observes the failed dispatch exactly once")
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), "upstream rejected request")
	p.sched.FlushRules()
	require.Len(t, testHealthSink.throttlesFor(sel.AccountID), 1, "observed 4xx has one MarkResult owner")
	runtime, ok := p.sched.Runtime(sel.AccountID)
	require.True(t, ok)
	require.Zero(t, runtime.Concurrency, "observed 4xx finish releases once")
}

func TestFailoverPipeline_handledTruePerformsNoSharedCleanup(t *testing.T) {
	p := &Proxy{}
	p.pipelineFlowAppend = func(AttemptOutcome) { t.Fatal("handled attempt must not append flow") }
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	// the session and attempt thread by value — zero values here prove
	// a handled terminal performs no shared cleanup without any identity.
	p.failoverLoopWithPlan(httptest.NewRecorder(), req, "chat", "req-handled", 1, time.Now(), "gpt-4o", nil, &scheduler.Selection{}, scheduler.AttemptPlan{}, scheduler.Attempt{}, attemptState{}, handledPipelineAttempt{}, &httpSink{}, false)
}

// TestPipelineObserverIllegalOutcomeIsLoggedNotDropped 非法 outcome 的丢弃必须可
// 观测，但控制流不变：AttemptObserver.Complete 先用 Validate 拒收，旧实现
// `_ = ...` 把这条事实静默吞掉——「类型层占位编码」一度正是这样制造非法 outcome
// 并被无声丢弃的（H7 现场），事后只剩「数字对不上」。
//
// 用例构造零业务帧 + failed + status 0 + sent_ambiguous（BusinessFrameSent=false
// ⇒ Validate 拒收），断言：记录一次 Warn（account_id/result/commit/错误文本）、
// 不 panic、不阻断（非法丢弃不毒化观测器，随后的合法 outcome 仍正常完成）。
func TestPipelineObserverIllegalOutcomeIsLoggedNotDropped(t *testing.T) {
	dir, err := os.MkdirTemp("", "c3api-obswarn-*")
	require.NoError(t, err)
	// zap 持有文件句柄（Windows 上删除会失败）：清理忽略错误（与 errlog_test 同模式）。
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	logOut := filepath.Join(dir, "warn.log")
	logger, err := logx.New("warn", logOut)
	require.NoError(t, err)
	p := &Proxy{log: logger}

	base := AttemptOutcome{
		ID: AttemptID("attempt-1"), RouteClassID: RouteClassID("rc-1"), QualityClassID: QualityClassID("qc-1"),
		Fingerprint: CandidateFingerprint("fp-1"), TemplateID: 3, AccountID: 42,
		RequestedModel: "gpt-4o", MappedModel: "gpt-4o",
		CallerCategory: CallerChat, OperationTag: OperationTag(domain.OpChatCompletions),
		Ordinal: 1, IdentityRevision: 1, Lane: LanePrimary, Generation: 1,
	}
	illegal := base
	illegal.Result = ResultFailed
	illegal.HTTPStatus = 0
	illegal.Commit = CommitSentAmbiguous
	illegal.BusinessFrameSent = false // 非法：sent_ambiguous 要求业务帧已下行
	illegal.Terminal = true
	require.Error(t, illegal.Validate(), "前提：该 outcome 必须非法（零业务帧 + sent_ambiguous）")

	var flow []AttemptOutcome
	obs := NewAttemptObserver(nil, nil, func(o AttemptOutcome) { flow = append(flow, o) }, nil)
	ctx := context.WithValue(context.Background(), ctxKeyDispatch{}, &dispatchObservation{observer: obs, base: base})

	p.observeDispatchOutcome(ctx, illegal, nil) // 不 panic、不阻断
	require.Empty(t, flow, "非法 outcome 仍被丢弃（控制流不变：不向上传播、不落 flow）")

	p.observeDispatchOutcome(ctx, wsOutcomeForSuccess(base, nil, usageTuple{}), nil)
	require.Len(t, flow, 1, "非法丢弃不得毒化观测器：随后的合法 outcome 仍正常完成")

	data, err := os.ReadFile(logOut)
	require.NoError(t, err)
	logged := string(data)
	require.Contains(t, logged, "dispatch observation rejected outcome", "非法 outcome 必须留痕（不静默）")
	require.Contains(t, logged, `"account_id":42`, "告警必须可定位到账号")
	require.Contains(t, logged, `"result":"failed"`)
	require.Contains(t, logged, `"commit":"sent_ambiguous"`)
	require.Contains(t, logged, "sent_ambiguous requires BusinessFrameSent", "告警必须带 Validate 的错误文本")
}
