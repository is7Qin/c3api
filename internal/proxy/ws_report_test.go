// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/aiclient"
)

func selForWSReport() *scheduler.Selection {
	return &scheduler.Selection{AccountID: 7, TemplateID: 3, Model: "gpt-4o", CandidateFingerprint: "fp-ws-test-1234"}
}

func TestWSOutcome_successReportsResponseStartedWithUsageAndTTFT(t *testing.T) {
	sel := selForWSReport()
	start := time.Now().Add(-10 * time.Millisecond)
	ttft := int64(6)
	base := wsDispatchedBase(sel, "gpt-4o", start)
	out := wsOutcomeForSuccess(base, &ttft, usageTuple{it: 2, ot: 5, tt: 7, cr: 1, cc: 2})
	require.NoError(t, out.Validate())
	require.Equal(t, CommitResponseStarted, out.Commit)
	require.Equal(t, ResultSuccess, out.Result)
	require.Equal(t, AttemptStatus(200), out.HTTPStatus)
	require.True(t, out.BusinessFrameSent)
	require.True(t, out.Terminal)
	require.Equal(t, int64(2), out.Usage.InputTokens)
	require.NotNil(t, out.Timing.TTFTMS)
	require.Equal(t, CallerResponsesWS, out.CallerCategory)
}

func TestWSOutcome_clientAbortIsResponseStarted(t *testing.T) {
	sel := selForWSReport()
	base := wsDispatchedBase(sel, "gpt-4o", time.Now())
	ttft := int64(4)
	out := wsOutcomeForClientAbort(base, usageTuple{it: 1}, &ttft)
	require.NoError(t, out.Validate())
	require.Equal(t, ResultClientCancel, out.Result)
	require.Equal(t, CommitResponseStarted, out.Commit)
	require.True(t, out.BusinessFrameSent)
	require.Equal(t, AttemptStatus(0), out.HTTPStatus)
}

func TestWSOutcome_upstreamErrorIsSentAmbiguous(t *testing.T) {
	sel := selForWSReport()
	base := wsDispatchedBase(sel, "gpt-4o", time.Now())
	out := wsOutcomeForUpstreamError(base, usageTuple{it: 1}, nil)
	require.NoError(t, out.Validate())
	require.Equal(t, CommitSentAmbiguous, out.Commit)
	require.Equal(t, ResultFailed, out.Result)
	require.Equal(t, AttemptStatus(0), out.HTTPStatus)
	require.True(t, out.BusinessFrameSent)
	require.True(t, out.Terminal)
	require.False(t, CanRetry(CallerResponsesWS, out))
}

func TestWSOutcome_notSentNetworkIsRetryable(t *testing.T) {
	sel := selForWSReport()
	base := wsDispatchedBase(sel, "gpt-4o", time.Now())
	out := wsOutcomeForNotSentNetwork(base, usageTuple{})
	require.NoError(t, out.Validate())
	require.Equal(t, CommitNotSent, out.Commit)
	require.False(t, out.Terminal)
	require.True(t, CanRetry(CallerResponsesWS, out))
	require.True(t, CanRetry(CallerCodexWS, func() AttemptOutcome {
		b := wsDispatchedBase(&scheduler.Selection{AccountID: 7, TemplateID: 3, Model: "gpt-4o", CandidateFingerprint: "fp-ws-test-1234", CredentialType: "codex-oauth"}, "gpt-4o", time.Now())
		return wsOutcomeForNotSentNetwork(b, usageTuple{})
	}()))
}

func TestWSOutcome_codexCategoryFromSelection(t *testing.T) {
	sel := &scheduler.Selection{AccountID: 7, TemplateID: 3, Model: "gpt-4o", CandidateFingerprint: "fp", CredentialType: "codex-oauth"}
	base := wsDispatchedBase(sel, "gpt-4o", time.Now())
	require.Equal(t, CallerCodexWS, base.CallerCategory)
	sel2 := &scheduler.Selection{AccountID: 7, TemplateID: 3, Model: "gpt-4o", CandidateFingerprint: "fp", CredentialType: "api_key"}
	base2 := wsDispatchedBase(sel2, "gpt-4o", time.Now())
	require.Equal(t, CallerResponsesWS, base2.CallerCategory)
}

func TestWSOutcome_upstream429IsRetryable(t *testing.T) {
	sel := selForWSReport()
	base := wsDispatchedBase(sel, "gpt-4o", time.Now())
	out := wsOutcomeForUpstreamStatus(base, 429, usageTuple{}, nil)
	require.NoError(t, out.Validate())
	require.Equal(t, CommitUpstreamResponded, out.Commit)
	require.False(t, out.Terminal)
	require.True(t, CanRetry(CallerResponsesWS, out))
}

func TestWSOutcome_singleObserverExactlyOnceBarrier(t *testing.T) {
	store := &captureLogStore{}
	p := newTestProxyTimeoutLogs(t, "http://127.0.0.1:1", 1, store)
	sel, err := p.sched.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.NotNil(t, sel)
	var calls atomic.Int32
	obs := NewAttemptObserver(nil, func(AttemptOutcome, AttemptHealthEvent) { calls.Add(1) }, func(AttemptOutcome) { calls.Add(10) }, func() { calls.Add(100) })
	base := wsDispatchedBase(sel, "gpt-4o", time.Now())
	out := wsOutcomeForSuccess(base, nil, usageTuple{it: 1, ot: 1, tt: 2})
	require.NoError(t, out.Validate())
	startCh := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(8)
	for i := 0; i < 8; i++ {
		go func() {
			defer wg.Done()
			<-startCh
			_ = obs.Complete(out, wsHealthForOutcome(out))
		}()
	}
	close(startCh)
	wg.Wait()
	require.Equal(t, int32(111), calls.Load())
}

func TestWSReport_headersAndLimitsPreserved(t *testing.T) {
	h := wsPassthroughHeaders(map[string][]string{"Authorization": {"Bearer ck-1"}, "X-Api-Key": {"ck-1"}, "X-Client-Version": {"v1"}})
	require.Empty(t, h.Get("Authorization"))
	require.Empty(t, h.Get("X-Api-Key"))
	require.Equal(t, "v1", h.Get("X-Client-Version"))
	ch := codexWSPassthroughHeaders(map[string][]string{"Session-Id": {"s"}, "OpenAI-Beta": {"x"}, "User-Agent": {"ua"}})
	require.Empty(t, ch.Get("Session-Id"))
	require.Empty(t, ch.Get("OpenAI-Beta"))
	require.Equal(t, "ua", ch.Get("User-Agent"))
}

// TestWSPassthroughEqualsSharedRelayList R3b：把「WS 面剔除面 = 全仓唯一那份
// 清单」钉成等价断言，零字面量复制（清单内容自身的正确性由 pkg/aiclient 侧的
// R1/R3a 承担，两者不可互替）。
//
// 已知代价：C4 落地当下 wsPassthroughHeaders 就是 `return aiclient.RelayHeaders(h)`，
// 本式同义反复。它的价值是漂移防线——日后有人在 WS 面重新加回本地剔除逻辑
// （v6 之前两份真相漂移的成因）时立刻红。relayDeny 日后加项，此式自动覆盖。
//
// h 故意含非规范形键与脏值：canonicalize 由 RelayHeaders 内部统一做，手工构造的
// "authorization" 若绕过规范化就会把凭据写上线（实测：删掉 canonicalize 的那版
// Header.Write 输出 `authorization: Bearer gateway-key`）。
func TestWSPassthroughEqualsSharedRelayList(t *testing.T) {
	h := http.Header{
		"authorization":   {"Bearer gateway-key"},
		"Cookie":          {"tenant=leak"},
		"Accept-Encoding": {"gzip"},
		"Te":              {"trailers"},
		"X-Dirty-Value":   {"ok\r\ninjected"},
		"X-Mixed-Values":  {"good", "bad\x00here"},
		"X-Ok":            {"keep"},
	}
	require.Equal(t, aiclient.RelayHeaders(h), wsPassthroughHeaders(h),
		"WS 面必须与共享清单逐键逐值等价")

	// 等价式在 C4 当下是同义反复，故再钉三条**非自反**断言（§2b「另加一例非规范
	// 形键仍被剔」那条）：手工构造的小写 authorization 必须被规范化后命中清单；
	// 值卫生在 WS 面是新行为（此前无），整键值全脏则该键不出现、混合键只丢脏值。
	// 删掉 canonicalize 时第一条会红（实测：那版 Header.Write 会把凭据写上线）。
	out := wsPassthroughHeaders(h)
	_, ok := out["authorization"]
	require.False(t, ok, "非规范形凭据键不得原样残留")
	_, ok = out["Authorization"]
	require.False(t, ok, "非规范形凭据键必须被提升后命中清单剔除")
	_, ok = out["X-Dirty-Value"]
	require.False(t, ok, "值全脏的键必须整键不出现（WS 面此前无值卫生）")
	require.Equal(t, []string{"good"}, out["X-Mixed-Values"], "混合键只丢脏的那个值")
}
