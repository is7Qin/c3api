// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/usage"
)

// 低余额用户级并发钳制的阈值（$10 → 毫分）。
const lowBalThresholdMilli = 1_000_000

// TestCapLowBalanceUserConc capLowBalanceUserConc 全域表驱动：
// cur==0（不限）与大原上限 → cap；0<cur<=cap → 取 min（维持原值）。
func TestCapLowBalanceUserConc(t *testing.T) {
	for _, c := range []struct {
		cur, cap, want int
	}{
		{0, 5, 5},  // 不限 → 钳为 cap
		{3, 5, 3},  // 原上限小于 cap → 取 min（不放大）
		{5, 5, 5},  // 等于 cap → 不变
		{10, 5, 5}, // 大于 cap → 钳为 cap
	} {
		require.Equal(t, c.want, capLowBalanceUserConc(c.cur, c.cap), "cur=%d cap=%d", c.cur, c.cap)
	}
}

// TestCapLowBalanceUserConcZeroAlloc 热路径红线：纯函数与融合判定表达式零分配。
func TestCapLowBalanceUserConcZeroAlloc(t *testing.T) {
	require.Zero(t, testing.AllocsPerRun(1000, func() { _ = capLowBalanceUserConc(10, 5) }))
	require.Zero(t, testing.AllocsPerRun(1000, func() {
		bal, ok := int64(500_000), true
		cur := 10
		if ok && bal < lowBalThresholdMilli {
			cur = capLowBalanceUserConc(cur, 5)
		}
		_ = cur
	}))
}

// lowBalGateProxy 构造「真实门禁 + 假余额」的最小代理（guardPipeline/基准共用）：
// 计费开（余额预检 + 钳制生效）、无额度、单用户单 key（user 级门禁）。
// UsageCapture 关 → 拒绝路径 recordRejected 短路（不落明细，无需 errlog）。
func lowBalGateProxy(tb testing.TB, capConc int, thresholdMilli, balanceMilli int64, userMaxConc int) *Proxy {
	tb.Helper()
	bal := billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{1: balanceMilli}}, nil)
	require.NoError(tb, bal.Reload(context.Background()))
	meta := activeKey(1, 1, 10)
	meta.UserMaxConc = userMaxConc
	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{"ck-1": meta}}, noopUserLoader{}, nil, true)
	require.NoError(tb, auth.Reload(context.Background()))
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour, QuotaFlushInterval: time.Hour,
	}, noopLogStore{}, nil)
	cfg := Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second,
		BillingCapture:    true,
		LowBalanceConcCap: capConc, LowBalanceConcThresholdMilli: thresholdMilli,
	}
	return New(cfg, nil, credential.New(), rec, nil, auth, nil, &BillingHooks{Balances: bal}, nil, Deps{})
}

// guardOnce 单次 guardPipeline（真实门禁入口）：返回写出状态码、响应体、
// 已 acquire 层级、是否放行。放行者占用门禁槽，调用方须随后 Release。
func guardOnce(t *testing.T, p *Proxy) (int, string, int, bool) {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	_, _, level, ok := p.guardPipeline(w, req, domain.FormatOpenAIChat, "r", time.Now(), true)
	return w.Code, w.Body.String(), level, ok
}

// acquireN 连续 hold 次 guardPipeline 全部须放行（占住门禁槽），返回已 acquire 层级。
func acquireN(t *testing.T, p *Proxy, hold int) []int {
	t.Helper()
	levels := make([]int, 0, hold)
	for i := 0; i < hold; i++ {
		code, body, level, ok := guardOnce(t, p)
		require.True(t, ok, "第 %d 个并发须放行（code=%d body=%s）", i+1, code, body)
		levels = append(levels, level)
	}
	return levels
}

func releaseAll(p *Proxy, levels []int) {
	for _, lv := range levels {
		p.auth.Release(activeKey(1, 1, 10), lv)
	}
}

// TestGuardPipelineLowBalanceCapsUserConcurrency 低余额钳制端到端（A3，真实 gate
// + 假 balances）：余额 $5（<$10）且 UserMaxConc ∈ {0,10} → 有效上限 5，第 5 个
// 放行、第 6 个 429；余额 $20（≥$10）且 UserMaxConc=2 → 原上限生效，第 3 个 429；
// 余额 $20 且 UserMaxConc=0（不限）→ 不受钳（第 4 个仍放行）。
func TestGuardPipelineLowBalanceCapsUserConcurrency(t *testing.T) {
	const low = 500_000    // $5（毫分）
	const high = 2_000_000 // $20
	cases := []struct {
		name   string
		bal    int64
		umc    int  // 原 UserMaxConc
		cap    int  // LowBalanceConcCap
		hold   int  // 先占满的并发数（须全部放行）
		reject bool // hold+1 个是否须 429
	}{
		{"低余额 $5 原上限 10 → 钳为 5", low, 10, 5, 5, true},
		{"低余额 $5 原上限不限(0) → 钳为 5", low, 0, 5, 5, true},
		{"余额 $20 原上限 2 → 第 3 个 429", high, 2, 5, 2, true},
		{"余额 $20 原上限不限(0) → 不受钳", high, 0, 5, 3, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := lowBalGateProxy(t, c.cap, lowBalThresholdMilli, c.bal, c.umc)
			levels := acquireN(t, p, c.hold)
			code, body, level, ok := guardOnce(t, p)
			if c.reject {
				require.False(t, ok, "第 %d 个并发必须拒绝", c.hold+1)
				require.Equal(t, http.StatusTooManyRequests, code, "body=%s", body)
				require.Contains(t, body, "concurrency limit exceeded")
			} else {
				require.True(t, ok, "不限并发不得拒绝（code=%d body=%s）", code, body)
				levels = append(levels, level)
			}
			releaseAll(p, levels)
		})
	}
}

// TestGuardPipelineLowBalanceDisabledIdentical cap=0（关闭）逐位等价现状（A4）：
// 低余额不钳制——原上限（2）照常生效，第 3 个 429；原上限不限（0）照常不限。
func TestGuardPipelineLowBalanceDisabledIdentical(t *testing.T) {
	const low = 500_000 // $5（即便低余额也不钳，因 cap=0）
	t.Run("cap=0 低余额 原上限 2 → 第 3 个 429", func(t *testing.T) {
		p := lowBalGateProxy(t, 0, lowBalThresholdMilli, low, 2)
		levels := acquireN(t, p, 2)
		code, body, _, ok := guardOnce(t, p)
		require.False(t, ok, "第 3 个须 429")
		require.Equal(t, http.StatusTooManyRequests, code, "body=%s", body)
		releaseAll(p, levels)
	})
	t.Run("cap=0 低余额 原上限不限 → 不受钳", func(t *testing.T) {
		p := lowBalGateProxy(t, 0, lowBalThresholdMilli, low, 0)
		levels := acquireN(t, p, 3) // 不限并发：3 个全放行
		releaseAll(p, levels)
	})
}

// TestGuardPipelineLowBalanceAllocsParity cap_on 与 cap_off 每轮分配必须相等
// （delta==0）：cap>0 只新增 1 次整数比较，热路径零新增分配。
func TestGuardPipelineLowBalanceAllocsParity(t *testing.T) {
	measure := func(p *Proxy) float64 {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
		req.Header.Set("Authorization", "Bearer ck-1")
		var failed bool
		allocs := testing.AllocsPerRun(200, func() {
			_, rm, level, ok := p.guardPipeline(w, req, domain.FormatOpenAIChat, "r", time.Now(), true)
			if !ok {
				failed = true
				return
			}
			p.auth.Release(rm.meta, level) // 复位门禁槽（每轮成功）
		})
		require.False(t, failed, "guardPipeline 必须保持可准入（夹具/上限配置错误）")
		return allocs
	}
	off := measure(lowBalGateProxy(t, 0, lowBalThresholdMilli, 500_000, 10)) // cap_off（低余额但不钳）
	on := measure(lowBalGateProxy(t, 5, lowBalThresholdMilli, 500_000, 10))  // cap_on（低余额钳为 5）
	require.Equal(t, off, on, "cap_on 与 cap_off AllocsPerRun 必须相等（delta==0）")
}
