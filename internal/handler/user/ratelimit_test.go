// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// F3：per-IP token 桶单元测试（Router 级 429 见 handler 包 TestUserPublicRateLimit）。

// 突发上限：burst 内全放行，超发拒绝；不同 IP 独立计数。
func TestIPRateLimiterBurstPerIP(t *testing.T) {
	l := NewIPRateLimiter(1000, 3, time.Minute, 1000) // 速率高 → 突发窗口内无补充
	for i := 0; i < 3; i++ {
		require.True(t, l.Allow("203.0.113.1"), "burst 内放行 #%d", i+1)
	}
	require.False(t, l.Allow("203.0.113.1"), "burst 超发拒绝")
	require.True(t, l.Allow("203.0.113.2"), "不同 IP 独立计数")
	require.True(t, l.Allow("203.0.113.2"))
	require.False(t, l.Allow("203.0.113.1"), "原 IP 桶不受他 IP 影响")
}

// 速率补充：burst 耗尽后按 rate 补充 token（补充受 burst 封顶）。
func TestIPRateLimiterRefill(t *testing.T) {
	l := NewIPRateLimiter(100, 5, time.Minute, 1000) // 100/s → 每 10ms 一个 token
	for i := 0; i < 5; i++ {
		require.True(t, l.Allow("203.0.113.9"))
	}
	require.False(t, l.Allow("203.0.113.9"), "burst 耗尽")
	time.Sleep(150 * time.Millisecond) // 补充 ~15 token（封顶 5）
	for i := 0; i < 5; i++ {
		require.True(t, l.Allow("203.0.113.9"), "补充后放行 #%d", i+1)
	}
	require.False(t, l.Allow("203.0.113.9"), "补充量封顶于 burst")
}

// 键空间容量上限：满容拒绝新 IP；过期清理（ttl 无请求）后腾出空间。
func TestIPRateLimiterCapacityAndExpiry(t *testing.T) {
	l := NewIPRateLimiter(1000, 3, 30*time.Millisecond, 2)
	require.True(t, l.Allow("203.0.113.1"))
	require.True(t, l.Allow("203.0.113.2"))
	require.False(t, l.Allow("203.0.113.3"), "键空间满 → 新 IP 拒绝（内存有界）")
	require.True(t, l.Allow("203.0.113.1"), "已有键不受影响")

	time.Sleep(80 * time.Millisecond) // 两键均空闲超 ttl → sweep 清理
	require.True(t, l.Allow("203.0.113.3"), "过期清理后新 IP 可入（容量腾出）")
}

// 并发安全：多 goroutine 同 IP 竞争（-race 兜底；放行数 = burst——桶语义
// 即互斥有序扣减。速率取极小值：竞争窗口内的补充量 ≈ 0，放行数不漂移）。
func TestIPRateLimiterConcurrent(t *testing.T) {
	l := NewIPRateLimiter(0.001, 10, time.Minute, 100) // 1 token/1000s：窗口内零补充
	done := make(chan bool, 100)
	for i := 0; i < 100; i++ {
		go func() { done <- l.Allow("203.0.113.7") }()
	}
	allowed := 0
	for i := 0; i < 100; i++ {
		if <-done {
			allowed++
		}
	}
	require.Equal(t, 10, allowed, "并发下放行数恒等于 burst（互斥扣减）")
}
