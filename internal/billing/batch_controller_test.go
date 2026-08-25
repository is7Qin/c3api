// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

// batch_controller_test.go 自适应批控策略单测（纯内存零 DB）：safe_batch =
// 时间预算 / 实测每行成本 的轨迹锁定——满批门控倍增 / 超时立即减半（唯一收缩
// 触发器）/ 他错保持 / 空批棘轮回归钉（v2）/ 锯齿轨迹（v2.1）。事故参照：固定
// 大批次在千万行脏可见性地图上单语句超 settleTimeout(10s) → 重试恒超时永久停摆
// （50000 批生产 stall）；v2 动机：空批/薄积压下「快」被误判健康 → 棘轮到 64k
// 陈旧最大批；v2.1 修订（spec-adaptive-batch-v2 §八，压测机 150-270 行/s 实测）：
// d(L)=F+c·L 固定成本主导时 d(500)≈d(64000)，慢→减半是误归因、吞吐 ∝ L 崩溃
// → 慢成功保持（完成了真实工作），仅超时收缩。

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

// ctrlStep 单次观测注入（时长 + 错误 + 是否满批 subscribed）。
type ctrlStep struct {
	d          time.Duration
	err        error
	subscribed bool
}

func Test_batchController_observe_policy(t *testing.T) {
	fast := time.Millisecond // d < budget/3 ≈ 2.67s：快
	slow := settleTimeBudget // d >= budget/3：慢 → 保持（v2.1：慢成功=真实工作）
	cases := []struct {
		name  string
		steps []ctrlStep
		want  int
	}{
		{
			name: "fast and full doubles until capped at maxBatchLimit",
			steps: []ctrlStep{
				{d: fast, subscribed: true}, {d: fast, subscribed: true},
				{d: fast, subscribed: true}, {d: fast, subscribed: true},
				{d: fast, subscribed: true}, {d: fast, subscribed: true}, // 已到顶：继续快+满批不再涨
			},
			want: maxBatchLimit,
		},
		{
			name: "slow holds regardless of subscription",
			steps: []ctrlStep{
				{d: slow, subscribed: true}, {d: slow, subscribed: true},
				{d: slow, subscribed: true}, {d: slow, subscribed: true},
				{d: slow, subscribed: true}, {d: slow, subscribed: true}, // 慢成功=完成了真实工作，恒保持（SC7）
			},
			want: settleBatchLimit,
		},
		{
			name:  "slow holds even when not subscribed", // 慢保持不门控——固定成本主导时 d 与批规模无关
			steps: []ctrlStep{{d: slow}},
			want:  settleBatchLimit,
		},
		{
			name:  "deadline exceeded halves immediately regardless of duration and subscription",
			steps: []ctrlStep{{d: time.Nanosecond, err: context.DeadlineExceeded, subscribed: true}},
			want:  settleBatchLimit / 2,
		},
		{
			name:  "exact budget/3 boundary counts as slow and holds",
			steps: []ctrlStep{{d: settleTimeBudget / 3, subscribed: true}},
			want:  settleBatchLimit,
		},
		{
			name: "other non-nil error holds current value even when full",
			steps: []ctrlStep{
				{d: slow, err: errors.New("connection reset")},
				{d: slow, err: errors.New("connection reset"), subscribed: true},
			},
			want: settleBatchLimit,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newBatchController()
			for _, s := range tc.steps {
				c.observe(s.d, s.err, s.subscribed)
			}
			require.Equal(t, tc.want, c.limit())
		})
	}
}

// Test_batchController_fast_partial_never_ratchets v2 空批棘轮回归钉（SC2）：
// 快但未满批 = 需求不足的伪健康信号，绝不倍增——64 轮 fast+partial 观测后 cur
// 恒为种子值（v1 缺陷：排空尾段空批 ~ms 完成 → 判健康 → 棘轮到 64k 陈旧最大批）。
func Test_batchController_fast_partial_never_ratchets(t *testing.T) {
	c := newBatchController()
	for range 64 {
		c.observe(time.Millisecond, nil, false)
		require.Equal(t, settleBatchLimit, c.limit())
	}
}

// Test_batchController_slow_holds_timeout_shrinks_sawtooth v2.1 锯齿轨迹钉（SC8，
// 替代 v2 震荡收敛钉——慢不再收缩，无 2-周期收敛语义）：快满批倍增至 cap →
// 超时一次减半（唯一收缩触发器）→ 快满批复倍增回 cap（第二次撞钳制）。确定性
// 轨迹断言，锁死「慢保持 + 超时收缩」的锯齿稳态。
func Test_batchController_slow_holds_timeout_shrinks_sawtooth(t *testing.T) {
	c := newBatchController()
	fast := time.Millisecond
	// 快满批 ×3 → cap
	for range 3 {
		c.observe(fast, nil, true)
	}
	require.Equal(t, maxBatchLimit, c.limit())
	// 超时一次 → 减半
	c.observe(time.Nanosecond, context.DeadlineExceeded, false)
	require.Equal(t, maxBatchLimit/2, c.limit())
	// 快满批 ×2 → 复倍增（第二次撞钳制）
	c.observe(fast, nil, true)
	require.Equal(t, maxBatchLimit, c.limit())
	c.observe(fast, nil, true)
	require.Equal(t, maxBatchLimit, c.limit())
}
