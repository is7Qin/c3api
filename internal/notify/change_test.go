// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package notify

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestChangeRoundtrip 全字段载荷 marshal/unmarshal roundtrip（各 bool + Groups
// + Src + V）。
func TestChangeRoundtrip(t *testing.T) {
	c := Change{
		V:           1,
		Users:       true,
		Templates:   true,
		Clients:     true,
		Multipliers: true,
		Keys:        true,
		Settings:    true,
		Rules:       true,
		Groups:      []int64{12, 34, 56},
		Src:         "i-1",
	}
	got, err := Unmarshal(Marshal(c))
	require.NoError(t, err)
	require.Equal(t, c, got)
}

// TestChangeOmitEmpty 零值字段不占载荷（紧凑 JSON：仅 v 恒在）。
func TestChangeOmitEmpty(t *testing.T) {
	payload := string(Marshal(Change{}))
	require.Equal(t, `{"v":0}`, payload, "零值字段 omitempty 应全部省略")

	c, err := Unmarshal([]byte(`{"v":1,"users":true}`))
	require.NoError(t, err)
	require.True(t, c.Users)
	require.False(t, c.Templates, "未出现字段 = false")
	require.Nil(t, c.Groups, "未出现字段 = nil")
}

// TestMarshalPayloadGuard 载荷守卫：Groups 超 6KB → 丢弃 Groups 置
// Templates=true（降级 sched 全量重载，语义仍正确）；未超限 → 原样保留。
func TestMarshalPayloadGuard(t *testing.T) {
	t.Run("超限降级 full", func(t *testing.T) {
		groups := make([]int64, 1200) // 每个 ~9-10B，总计 ~11KB > 6KB
		for i := range groups {
			groups[i] = int64(10000000 + i)
		}
		c := Change{V: 1, Groups: groups, Src: "i-1"}
		payload := Marshal(c)
		require.Greater(t, len(payload), 0)
		require.LessOrEqual(t, len(payload), maxPayloadBytes, "降级后必须落在守卫阈值内")

		got, err := Unmarshal(payload)
		require.NoError(t, err)
		require.True(t, got.Templates, "超限 → Templates=true（sched 全量包含组级重载）")
		require.Nil(t, got.Groups, "超限 → Groups 丢弃")
		require.Equal(t, "i-1", got.Src, "Src 保留")
	})
	t.Run("未超限原样保留", func(t *testing.T) {
		c := Change{V: 1, Groups: []int64{12, 34}}
		payload := Marshal(c)
		require.LessOrEqual(t, len(payload), maxPayloadBytes)
		got, err := Unmarshal(payload)
		require.NoError(t, err)
		require.Equal(t, []int64{12, 34}, got.Groups, "未超限 → Groups 保留")
		require.False(t, got.Templates)
	})
	t.Run("无 Groups 永不打守卫", func(t *testing.T) {
		c := Change{V: 1, Users: true}
		require.Equal(t, `{"v":1,"users":true}`, string(Marshal(c)))
	})
	t.Run("恰好 6KB 边界不降级", func(t *testing.T) {
		// 未守卫载荷长度恰好 = maxPayloadBytes：守卫是严格 >，边界内必须原样保留。
		groups := groupsForTarget(t, maxPayloadBytes)
		c := Change{V: 1, Groups: groups}
		require.Len(t, marshalRaw(c), maxPayloadBytes, "前置：未守卫载荷必须恰好落在边界上")
		payload := Marshal(c)
		require.Len(t, payload, maxPayloadBytes, "恰好边界 → 不降级")
		got, err := Unmarshal(payload)
		require.NoError(t, err)
		require.Equal(t, groups, got.Groups, "恰好边界 → Groups 保留")
		require.False(t, got.Templates, "恰好边界 → 不降级")
	})
	t.Run("6KB+1 降级 full", func(t *testing.T) {
		// 未守卫载荷长度恰好 = maxPayloadBytes+1：超 1 字节必须触发降级。
		groups := groupsForTarget(t, maxPayloadBytes+1)
		c := Change{V: 1, Groups: groups}
		require.Len(t, marshalRaw(c), maxPayloadBytes+1, "前置：未守卫载荷必须恰好超出边界 1 字节")
		payload := Marshal(c)
		require.Less(t, len(payload), len(marshalRaw(c)), "降级后载荷必须显著变小")
		got, err := Unmarshal(payload)
		require.NoError(t, err)
		require.Nil(t, got.Groups, "超 1 字节 → Groups 丢弃")
		require.True(t, got.Templates, "超 1 字节 → Templates=true（降级 full）")
	})
}

// TestNotifyRoutingDirtySurvivesPayloadGuard intelligent routing 契约：账号/
// 静态变更是 routing dirty 的载体（Groups → sched 组级定向重载 → RequestCompile
// 重编译决策视图）。载荷守卫 >6KB 降级丢 Groups 时，Templates=true 必须置位
// ——sched 全量重载包含组级（去抖器 merge 语义），routing dirty 不随载荷丢失。
func TestNotifyRoutingDirtySurvivesPayloadGuard(t *testing.T) {
	groups := make([]int64, 2000) // ~20KB > 6KB 守卫阈值
	for i := range groups {
		groups[i] = int64(10000000 + i)
	}
	got, err := Unmarshal(Marshal(Change{V: 1, Groups: groups}))
	require.NoError(t, err)
	require.Nil(t, got.Groups, "超限 → 定向组列表丢弃")
	require.True(t, got.Templates, "routing dirty 必须经全量重载触发器保留（重编译入口）")
}

// marshalRaw 不经守卫的原始序列化（守卫边界构造/断言用——守卫内部测量的就是
// 这个长度）。
func marshalRaw(c Change) []byte {
	payload, _ := json.Marshal(c) // Change 仅基本类型 + []int64，marshal 不可能失败
	return payload
}

// groupsForTarget 构造 Groups 使未守卫载荷长度恰好为 target：
// 每组基础 8 位数字（+9B 含分隔逗号）；逼近后用末元素位数补差 1..8 位，
// 差 9 位补一组（末元素前一组补逗号 +1B + 新末元素 8B = +9B）。构造失败
// （布局假设变化）直接 Fatal——守卫边界测试依赖精确长度。
func groupsForTarget(t *testing.T, target int) []int64 {
	t.Helper()
	groups := []int64{12345678}
	for len(marshalRaw(Change{V: 1, Groups: groups})) < target-9 {
		groups = append(groups, 12345678)
	}
	rem := target - len(marshalRaw(Change{V: 1, Groups: groups})) // ∈ [1,9]
	switch {
	case rem == 9:
		groups = append(groups, 12345678)
	case rem > 0:
		last := groups[len(groups)-1]
		for i := 0; i < rem; i++ {
			last *= 10 // 8 位 → 8+rem 位（rem ≤ 8 → ≤ 16 位，int64 内）
		}
		groups[len(groups)-1] = last
	}
	if got := len(marshalRaw(Change{V: 1, Groups: groups})); got != target {
		t.Fatalf("groupsForTarget(%d) = %d 字节，构造失败", target, got)
	}
	return groups
}

// TestUnmarshalInvalid 非法 JSON → 错误。
func TestUnmarshalInvalid(t *testing.T) {
	_, err := Unmarshal([]byte(`{not json`))
	require.Error(t, err)
}
