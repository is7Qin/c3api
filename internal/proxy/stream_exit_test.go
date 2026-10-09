// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/sserelay"
)

// planBase 取一个合法的 plan-canonical base（供 dispatchFailureOutcome/观测器
// 校验通过）。
func planBase(t *testing.T, p *Proxy) AttemptOutcome {
	t.Helper()
	_, _, attempt, err := p.selectWithPlan(10, domain.FormatOpenAIChat, "gpt-4o",
		scheduler.AttemptPlanIdentity{RequestID: "req-carry", UserID: 1})
	require.NoError(t, err)
	return pipelineBase(attempt)
}

// TestObserveDispatchFailureCarriesCallerUsage 未提交交 pipeline 时携带已采
// usage/TTFT（阶段② §3.7 clause 3）：carryStreamUsage 写入 owner 观测 →
// observeDispatchFailure 合并进 handled=false 结束观测（usage/timing 不再恒空）。
func TestObserveDispatchFailureCarriesCallerUsage(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	base := planBase(t, p)

	var got AttemptOutcome
	obs := NewAttemptObserver(nil, nil, func(o AttemptOutcome) { got = o }, nil)
	d := &dispatchObservation{observer: obs, base: base}
	ctx := context.WithValue(context.Background(), ctxKeyDispatch{}, d)

	carryStreamUsage(ctx,
		AttemptUsage{InputTokens: 5, OutputTokens: 7, CacheReadTokens: 3, CacheCreationTokens: 4, CallCount: 2},
		ptr(int64(321)))
	p.observeDispatchFailure(ctx, d, 502)

	require.Equal(t, ResultFailed, got.Result)
	require.Equal(t, int64(5), got.Usage.InputTokens)
	require.Equal(t, int64(7), got.Usage.OutputTokens)
	require.Equal(t, int64(3), got.Usage.CacheReadTokens)
	require.Equal(t, int64(4), got.Usage.CacheCreationTokens)
	require.Equal(t, int64(2), got.Usage.CallCount)
	require.NotNil(t, got.Timing.TTFTMS)
	require.Equal(t, int64(321), *got.Timing.TTFTMS)
}

// TestObserveDispatchFailureNoCarryStaysEmpty 对照：无携带（如上游取回失败、
// 尚无 usage）→ handled=false 观测 usage/TTFT 恒空。
func TestObserveDispatchFailureNoCarryStaysEmpty(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)
	base := planBase(t, p)

	var got AttemptOutcome
	obs := NewAttemptObserver(nil, nil, func(o AttemptOutcome) { got = o }, nil)
	d := &dispatchObservation{observer: obs, base: base}
	ctx := context.WithValue(context.Background(), ctxKeyDispatch{}, d)

	p.observeDispatchFailure(ctx, d, 502)

	require.Zero(t, got.Usage.InputTokens)
	require.Zero(t, got.Usage.OutputTokens)
	require.Nil(t, got.Timing.TTFTMS)
}

// TestClassifyStreamExitCarriesOnlyWhenUncommitted 统一出口 helper：仅「未提交
// → pipeline」分支写入携带 usage/TTFT；已提交分支不携带。
func TestClassifyStreamExitCarriesOnlyWhenUncommitted(t *testing.T) {
	u := AttemptUsage{InputTokens: 9, OutputTokens: 8}
	ttft := int64(123)

	t.Run("uncommitted carries", func(t *testing.T) {
		o := sserelay.NewOutput(httptest.NewRecorder(), 0, sserelay.OutputOptions{})
		defer o.Release()
		d := &dispatchObservation{}
		ctx := context.WithValue(context.Background(), ctxKeyDispatch{}, d)
		kind := classifyStreamExit(ctx, o, errors.New("read boom"), u, &ttft)
		require.Equal(t, streamExitUncommitted, kind)
		require.Equal(t, u, d.carriedUsage)
		require.Same(t, &ttft, d.carriedTTFT)
	})

	t.Run("committed does not carry", func(t *testing.T) {
		o := sserelay.NewOutput(httptest.NewRecorder(), 0, sserelay.OutputOptions{})
		defer o.Release()
		require.NoError(t, o.Commit())
		d := &dispatchObservation{}
		ctx := context.WithValue(context.Background(), ctxKeyDispatch{}, d)
		kind := classifyStreamExit(ctx, o, errors.New("read boom"), u, &ttft)
		require.Equal(t, streamExitCommitted, kind)
		require.Zero(t, d.carriedUsage.InputTokens)
		require.Nil(t, d.carriedTTFT)
	})
}
