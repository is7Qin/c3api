// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestChatDispatchedBaseIncludesRequiredDispatchMetadata(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 42)
	s, err := p.sched.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	defer s.Release()
	base := chatDispatchedBase(s, "req-1", "gpt-4o", time.Now())
	o := chatOutcomeForSuccess(base, nil, usageTuple{it: 1})
	require.NoError(t, o.Validate())
	require.Equal(t, CallerChat, o.CallerCategory)
	require.Equal(t, "chat_completions", string(o.OperationTag))
	require.NotEmpty(t, o.RouteClassID)
	require.NotEmpty(t, o.QualityClassID)
	require.Equal(t, s.TemplateID, o.TemplateID)
	require.Equal(t, s.AccountID, o.AccountID)
}

func TestChatOutcomeSuccessIsTerminalResponseStarted(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	sel, err := p.sched.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	defer sel.Release()
	base := chatDispatchedBase(sel, "req-1", "gpt-4o", time.Now())
	ttft := int64(12)
	o := chatOutcomeForSuccess(base, &ttft, usageTuple{it: 5, ot: 7, tt: 12, cr: 1, cc: 2})
	require.NoError(t, o.Validate())
	require.Equal(t, ResultSuccess, o.Result)
	require.Equal(t, CommitResponseStarted, o.Commit)
	require.True(t, o.Terminal)
	require.True(t, o.BusinessFrameSent)
	require.Equal(t, AttemptStatus(200), o.HTTPStatus)
	require.NotNil(t, o.Timing.TTFTMS)
	require.Equal(t, int64(12), *o.Timing.TTFTMS)
}

func TestChatOutcomeClientCancelHasExactlyOneCommitState(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	sel, err := p.sched.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	defer sel.Release()
	base := chatDispatchedBase(sel, "req-1", "gpt-4o", time.Now())
	o1 := chatOutcomeForClientCancel(base, false, usageTuple{}, nil)
	require.NoError(t, o1.Validate())
	require.Equal(t, CommitNotSent, o1.Commit)
	require.False(t, o1.BusinessFrameSent)
	o2 := chatOutcomeForClientCancel(base, true, usageTuple{it: 1}, func() *int64 { v := int64(5); return &v }())
	require.NoError(t, o2.Validate())
	require.Equal(t, CommitResponseStarted, o2.Commit)
	require.True(t, o2.BusinessFrameSent)
}

func TestChatOutcomeNetworkPreservesCommitStates(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	sel, err := p.sched.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	defer sel.Release()
	base := chatDispatchedBase(sel, "req-1", "gpt-4o", time.Now())
	oNotSent := chatOutcomeForNetwork(base, false, usageTuple{}, nil)
	require.NoError(t, oNotSent.Validate())
	require.Equal(t, CommitNotSent, oNotSent.Commit)
	require.False(t, oNotSent.Terminal)
	require.True(t, CanRetry(CallerChat, oNotSent))
	oSent := chatOutcomeForNetwork(base, true, usageTuple{it: 2}, func() *int64 { v := int64(3); return &v }())
	require.NoError(t, oSent.Validate())
	require.Equal(t, CommitSentAmbiguous, oSent.Commit)
	require.True(t, oSent.Terminal)
	require.False(t, CanRetry(CallerChat, oSent))
}

func TestChatStreamingSuccessPreservesProtocolBytesAndUsage(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyTimeoutLogs(t, up.URL, 1, store)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, "data: [DONE]")
	require.Contains(t, body, `"prompt_tokens":5`)
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency)
	require.NoError(t, p.rec.Close(t.Context()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, int64(5), store.logs[0].InputTokens)
	require.Equal(t, int64(7), store.logs[0].OutputTokens)
}

func TestChatNonStreamingSuccessPreservesUsageAndTTFT(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyTimeoutLogs(t, up.URL, 1, store)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code)
	require.NoError(t, p.rec.Close(t.Context()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, domain.ErrNone, store.logs[0].ErrorType)
	require.Nil(t, store.logs[0].TTFTMS)
}

func TestChatLocalRejectReleasesSlotWithoutDispatchObservation(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{`)) // malformed json triggers local reject after select in non-stream path
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 400, rec.Code)
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "local reject must release slot exactly once")
	require.Zero(t, p.rec.Pending())
}

func TestChatAttemptObserverExactlyOnceUnderRace(t *testing.T) {
	var releases atomic.Int32
	var healths atomic.Int32
	// Use proxy helper to get a real selection then replace release with counting
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	sel, err := p.sched.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	base := chatDispatchedBase(sel, "req-1", "gpt-4o", time.Now())
	o := chatOutcomeForSuccess(base, nil, usageTuple{it: 1, ot: 1})
	observer := NewAttemptObserver(nil,
		func(AttemptOutcome, AttemptHealthEvent) { healths.Add(1) },
		func(AttemptOutcome) {},
		func() { releases.Add(1); sel.Release() },
	)
	// concurrent completes should only count once
	done := make(chan struct{})
	for range 8 {
		go func() { _ = observer.Complete(o, &AttemptHealthEvent{Kind: 1}); done <- struct{}{} }()
	}
	for range 8 {
		<-done
	}
	require.Equal(t, int32(1), healths.Load())
	require.Equal(t, int32(1), releases.Load())
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency)
}
