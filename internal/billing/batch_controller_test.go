// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

// batch_controller_test.go 自适应批控策略单测（纯内存零 DB）：safe_batch =
// 时间预算 / 实测每行成本 的轨迹锁定——倍增封顶 / 减半托底 / 超时立即减半 /
// 他错保持 / 震荡收敛。事故参照：固定大批次在千万行脏可见性地图上单语句超
// settleTimeout(10s) → 重试恒超时永久停摆（50000 批生产 stall）。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func Test_batchController_when_fresh_seedsFromSettleBatchLimit(t *testing.T) {
	require.Equal(t, settleBatchLimit, newBatchController().limit())
}

// ctrlStep 单次观测注入（时长 + 错误）。
type ctrlStep struct {
	d   time.Duration
	err error
}

func Test_batchController_observe_policy(t *testing.T) {
	fast := time.Millisecond // d < budget/3 ≈ 2.67s：健康 → 倍增
	slow := settleTimeBudget // d >= budget/3：慢 → 减半
	cases := []struct {
		name  string
		steps []ctrlStep
		want  int
	}{
		{
			name: "fast doubles until capped at maxBatchLimit",
			steps: []ctrlStep{
				{d: fast}, {d: fast}, {d: fast}, {d: fast},
				{d: fast}, {d: fast}, // 已到顶：继续快不再涨
			},
			want: maxBatchLimit,
		},
		{
			name: "slow halves until floored at minBatchLimit",
			steps: []ctrlStep{
				{d: slow}, {d: slow}, {d: slow}, {d: slow},
				{d: slow}, {d: slow}, // 已到底：继续慢不再降
			},
			want: minBatchLimit,
		},
		{
			name:  "deadline exceeded halves immediately regardless of duration",
			steps: []ctrlStep{{d: time.Nanosecond, err: context.DeadlineExceeded}},
			want:  settleBatchLimit / 2,
		},
		{
			name:  "exact budget/3 boundary counts as slow",
			steps: []ctrlStep{{d: settleTimeBudget / 3}},
			want:  settleBatchLimit / 2,
		},
		{
			name:  "other non-nil error holds current value",
			steps: []ctrlStep{{d: slow, err: errors.New("connection reset")}, {d: slow, err: errors.New("connection reset")}},
			want:  settleBatchLimit,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newBatchController()
			for _, s := range tc.steps {
				c.observe(s.d, s.err)
			}
			require.Equal(t, tc.want, c.limit())
		})
	}
}

// Test_batchController_oscillation_converges_near_budget_boundary 固定每行成本
// 模型下震荡收敛：均衡批 eq=12000 行（该规模实测恰为预算边界），交替快/慢观测
// 应稳定为环绕均衡的 2-周期，绝不单调漂向 cap/floor。
func Test_batchController_oscillation_converges_near_budget_boundary(t *testing.T) {
	const eq = 12000
	c := newBatchController()
	lo := max(minBatchLimit, eq/2)
	hi := min(maxBatchLimit, eq*2)
	var seen []int
	for range 64 {
		d := time.Duration(float64(settleTimeBudget) / 3 * float64(c.limit()) / eq)
		c.observe(d, nil)
		lim := c.limit()
		seen = append(seen, lim)
		require.GreaterOrEqual(t, lim, lo)
		require.LessOrEqual(t, lim, hi)
	}
	// 收敛判据：末段稳定 2-周期（无漂移）。
	n := len(seen)
	require.Equal(t, seen[n-3], seen[n-1])
	require.Equal(t, seen[n-4], seen[n-2])
}
